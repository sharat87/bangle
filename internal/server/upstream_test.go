package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"bangle/internal/db"
	"bangle/migrations"
)

const testSecretsKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// fakeUpstream is an MCP server and its authorization server in one
// httptest server: the MCP endpoint at /mcp (the go-sdk's own server, behind
// a bearer check), RFC 9728 and RFC 8414 / OIDC metadata, DCR, an authorize
// endpoint that approves at once, and a token endpoint that checks PKCE.
type fakeUpstream struct {
	t   *testing.T
	srv *httptest.Server

	// Knobs, set before use.
	header       bool  // name resource_metadata in the 401's WWW-Authenticate
	oidcOnly     bool  // serve only /.well-known/openid-configuration
	listOffline  bool  // list offline_access in the AS's scopes_supported
	issueRefresh bool  // issue refresh tokens
	rotate       bool  // rotate the refresh token on every refresh
	expiresIn    int64 // expires_in for every access token

	mu            sync.Mutex
	codes         map[string]fakeGrant
	access        map[string]bool
	refresh       map[string]bool
	n             int
	registered    int
	refreshCalls  int
	listCalls     int // tools/list requests answered
	lastAuthorize url.Values
}

type fakeGrant struct {
	challenge, redirectURI, clientID, resource string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{
		t: t, header: true, listOffline: true, issueRefresh: true, rotate: true, expiresIn: 3600,
		codes: map[string]fakeGrant{}, access: map[string]bool{}, refresh: map[string]bool{},
	}
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "echo", Description: "Echoes text."},
		func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
		})
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "shout", Description: "Upper-cases text."},
		func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.ToUpper(in.Text)}}}, nil, nil
		})
	mcpServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				f.mu.Lock()
				f.listCalls++
				f.mu.Unlock()
			}
			return next(ctx, method, req)
		}
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok := f.access[token]
		f.mu.Unlock()
		if !ok {
			if f.header {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="OAuth", resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="read"`, f.srv.URL))
			} else {
				w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth"`)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"resource":              f.srv.URL + "/mcp",
			"authorization_servers": []string{f.srv.URL},
			"scopes_supported":      []string{"read"},
		})
	})
	asMeta := func(w http.ResponseWriter, r *http.Request) {
		scopes := []string{"read", "write"}
		if f.listOffline {
			scopes = append(scopes, "offline_access")
		}
		writeTestJSON(w, map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"registration_endpoint":                 f.srv.URL + "/register",
			"scopes_supported":                      scopes,
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},
		})
	}
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		if f.oidcOnly {
			http.NotFound(w, r)
			return
		}
		asMeta(w, r)
	})
	mux.HandleFunc("/.well-known/openid-configuration", asMeta)
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.registered++
		id := fmt.Sprintf("dcr-%d", f.registered)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeTestJSON(w, map[string]any{"client_id": id, "redirect_uris": req["redirect_uris"]})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.lastAuthorize = q
		code := f.next("code")
		f.codes[code] = fakeGrant{q.Get("code_challenge"), q.Get("redirect_uri"), q.Get("client_id"), q.Get("resource")}
		f.mu.Unlock()
		if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {f.srv.URL}}
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) next(kind string) string {
	f.n++
	return fmt.Sprintf("%s-%d", kind, f.n)
}

func (f *fakeUpstream) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	invalidGrant := func() {
		w.WriteHeader(http.StatusBadRequest)
		writeTestJSON(w, map[string]string{"error": "invalid_grant"})
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		if !ok || g.redirectURI != r.PostForm.Get("redirect_uri") || g.clientID != r.PostForm.Get("client_id") ||
			g.resource != r.PostForm.Get("resource") || pkceChallenge(r.PostForm.Get("code_verifier")) != g.challenge {
			invalidGrant()
			return
		}
	case "refresh_token":
		f.refreshCalls++
		rt := r.PostForm.Get("refresh_token")
		if !f.refresh[rt] {
			invalidGrant()
			return
		}
		if !f.rotate {
			access := f.next("access")
			f.access[access] = true
			writeTestJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": f.expiresIn})
			return
		}
		delete(f.refresh, rt)
	default:
		invalidGrant()
		return
	}
	resp := map[string]any{"token_type": "bearer", "expires_in": f.expiresIn}
	access := f.next("access")
	f.access[access] = true
	resp["access_token"] = access
	if f.issueRefresh {
		refresh := f.next("refresh")
		f.refresh[refresh] = true
		resp["refresh_token"] = refresh
	}
	writeTestJSON(w, resp)
}

// revokeAccess makes upstream reject every access token issued so far.
func (f *fakeUpstream) revokeAccess() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.access = map[string]bool{}
}

func (f *fakeUpstream) revokeRefresh() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh = map[string]bool{}
}

func (f *fakeUpstream) validRefresh(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refresh[token]
}

func (f *fakeUpstream) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

func writeTestJSON(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	json.NewEncoder(w).Encode(v)
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "bangle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := migrations.Run(database); err != nil {
		t.Fatal(err)
	}
	box, err := NewSecretBox(testSecretsKey)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("http://bangle.test")
	return New(database, "127.0.0.1:7060", base, http.Dir(t.TempDir()), box, Build{Commit: "0123456789abcdef"})
}

// call sends one request to the server as a same-origin browser would.
func call(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	switch b := body.(type) {
	case nil:
		r = httptest.NewRequest(method, path, nil)
	case url.Values:
		r = httptest.NewRequest(method, path, strings.NewReader(b.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = s.baseHost
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	return v
}

func addServer(t *testing.T, s *Server, body map[string]string) {
	t.Helper()
	if w := call(t, s, "POST", "/api/servers", body); w.Code != http.StatusCreated {
		t.Fatalf("adding server: %d %s", w.Code, w.Body.String())
	}
}

// login clicks Connect for a server and follows the browser through the fake
// authorization server and back, returning where the browser ends up.
func login(t *testing.T, s *Server, name string) string {
	t.Helper()
	w := call(t, s, "POST", upstreamStartPath, url.Values{"server": {name}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	authURL := w.Header().Get("Location")
	if strings.HasPrefix(authURL, "/") {
		return authURL // discovery failed; back to the server page
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if want := s.baseURL + upstreamCallbackPath; cb.Scheme+"://"+cb.Host+cb.Path != want {
		t.Fatalf("redirected to %s, want %s", cb, want)
	}
	w = call(t, s, "GET", cb.RequestURI(), nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	return w.Header().Get("Location")
}

func serverStatusOf(t *testing.T, s *Server, name string) serverJSON {
	t.Helper()
	w := call(t, s, "GET", "/api/servers/"+name, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get server: %d %s", w.Code, w.Body.String())
	}
	return decode[serverJSON](t, w)
}

func toolsOf(t *testing.T, s *Server, name string) ([]toolJSON, *httptest.ResponseRecorder) {
	t.Helper()
	w := call(t, s, "GET", "/api/servers/"+name+"/tools", nil)
	if w.Code != http.StatusOK {
		return nil, w
	}
	return decode[[]toolJSON](t, w), w
}

func mustListEcho(t *testing.T, s *Server, name string) {
	t.Helper()
	tools, w := toolsOf(t, s, name)
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "shout" {
		t.Fatalf("tools = %v (%d %s)", tools, w.Code, w.Body.String())
	}
}

// RFC 7636 Appendix B.
func TestPKCEChallenge(t *testing.T) {
	if got := pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge = %s", got)
	}
}

func TestSecretBox(t *testing.T) {
	box, err := NewSecretBox(testSecretsKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"", "hunter2", "pässwörd with spaces"} {
		sealed, err := box.seal(plain)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(sealed, sealedPrefix) || (plain != "" && strings.Contains(sealed, plain)) {
			t.Fatalf("sealed %q as %q", plain, sealed)
		}
		if again, _ := box.seal(plain); again == sealed {
			t.Fatalf("two seals of %q are identical: the nonce is not fresh", plain)
		}
		if got, err := box.open(sealed); err != nil || got != plain {
			t.Fatalf("open(seal(%q)) = %q, %v", plain, got, err)
		}
	}

	sealed, _ := box.seal("hunter2")
	if _, err := box.open("hunter2"); err == nil {
		t.Fatal("a plaintext value was accepted")
	}
	if _, err := box.open(sealed[:len(sealed)-2] + "AA"); err == nil {
		t.Fatal("a tampered value was accepted")
	}
	other, _ := NewSecretBox("Hx4dHBsaGRgXFhUUExIREA8ODQwLCgkIBwYFBAMCAQA=")
	if _, err := other.open(sealed); err == nil {
		t.Fatal("a value sealed under another key was accepted")
	}
	for _, bad := range []string{"", "not base64!", "c2hvcnQ="} {
		if _, err := NewSecretBox(bad); err == nil {
			t.Fatalf("key %q was accepted", bad)
		}
	}
}

func TestAuthParam(t *testing.T) {
	cases := []struct{ header, want string }{
		{`Bearer realm="OAuth", resource_metadata="https://mcp.linear.app/.well-known/oauth-protected-resource/mcp", scope="read write"`,
			"https://mcp.linear.app/.well-known/oauth-protected-resource/mcp"},
		{`Bearer resource_metadata=https://x.example/m, scope=a`, "https://x.example/m"},
		{`Bearer realm="a, resource_metadata=\"fake\"", resource_metadata="real"`, "real"},
		{`Bearer realm="OAuth"`, ""},
		{`Bearer error="invalid_token"`, ""},
	}
	for _, c := range cases {
		if got := authParam(c.header, "resource_metadata"); got != c.want {
			t.Errorf("authParam(%s) = %q, want %q", c.header, got, c.want)
		}
	}
}

func TestDiscovery(t *testing.T) {
	cases := []struct {
		name                      string
		header, oidcOnly, offline bool
		configured                string
		wantScopes                string
	}{
		{"WWW-Authenticate, RFC 8414", true, false, true, "", "read offline_access"},
		{"well-known fallback, OIDC only", false, true, true, "", "read offline_access"},
		{"configured scopes win", true, false, true, "openid write", "openid write offline_access"},
		{"no offline_access when unlisted", true, false, false, "", "read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.header, f.oidcOnly, f.listOffline = c.header, c.oidcOnly, c.offline
			d, err := discover(context.Background(), http.DefaultClient, f.srv.URL+"/mcp")
			if err != nil {
				t.Fatal(err)
			}
			if d.Resource.Resource != f.srv.URL+"/mcp" || d.AS.TokenEndpoint != f.srv.URL+"/token" || d.AS.RegistrationEndpoint != f.srv.URL+"/register" {
				t.Fatalf("discovered %+v", d)
			}
			if got := strings.Join(chooseScopes(c.configured, d), " "); got != c.wantScopes {
				t.Fatalf("scopes = %q, want %q", got, c.wantScopes)
			}
		})
	}

	t.Run("a server that does not ask for OAuth", func(t *testing.T) {
		open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer open.Close()
		if _, err := discover(context.Background(), http.DefaultClient, open.URL); err == nil || !strings.Contains(err.Error(), "not 401") {
			t.Fatalf("err = %v", err)
		}
	})
}

// The whole browser flow, then a tool list through the go-sdk client; the
// tokens are stored sealed.
func TestLoginAndListTools(t *testing.T) {
	f := newFakeUpstream(t)
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	if st := serverStatusOf(t, s, "fake"); st.Status != "needs_login" || st.LoggedIn {
		t.Fatalf("status before login = %+v", st)
	}
	if _, w := toolsOf(t, s, "fake"); w.Code != http.StatusConflict {
		t.Fatalf("tools before login: %d %s", w.Code, w.Body.String())
	}

	if back := login(t, s, "fake"); back != "/servers/fake" {
		t.Fatalf("callback went to %s", back)
	}
	q := f.lastAuthorize
	if q.Get("scope") != "read offline_access" || q.Get("prompt") != "consent" || q.Get("resource") != f.srv.URL+"/mcp" ||
		q.Get("redirect_uri") != "http://bangle.test/oauth/upstream/callback" || q.Get("client_id") != "dcr-1" {
		t.Fatalf("authorize request = %v", q)
	}
	st := serverStatusOf(t, s, "fake")
	if st.Status != "connected" || !st.LoggedIn || !st.Refreshable || st.TokenExpiresAt == nil {
		t.Fatalf("status after login = %+v", st)
	}
	mustListEcho(t, s, "fake")

	var access, refresh string
	if err := s.db.QueryRow(`SELECT access_token, refresh_token FROM server_tokens`).Scan(&access, &refresh); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(access, sealedPrefix) || !strings.HasPrefix(refresh, sealedPrefix) || strings.Contains(access+refresh, "access-") {
		t.Fatalf("tokens not sealed: %q %q", access, refresh)
	}

	// The state is single use.
	if w := call(t, s, "GET", upstreamCallbackPath+"?state=nope&code=x", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown state: %d", w.Code)
	}
}

// An expired access token is refreshed before use, the rotated refresh token
// is stored, and the next refresh succeeds with it.
func TestRefreshOnExpiryRotates(t *testing.T) {
	f := newFakeUpstream(t)
	f.expiresIn = 30 // inside tokenEarlyExpiry, so every token is already due
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	first, _, err := s.loadToken(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	mustListEcho(t, s, "fake")
	if n := f.refreshes(); n == 0 {
		t.Fatal("an expired token was not refreshed")
	}
	second, _, err := s.loadToken(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken || f.validRefresh(first.RefreshToken) || !f.validRefresh(second.RefreshToken) {
		t.Fatalf("refresh token not rotated and stored: %q then %q", first.RefreshToken, second.RefreshToken)
	}
	mustListEcho(t, s, "fake") // refreshes again, with the rotated token
	if st := serverStatusOf(t, s, "fake"); st.Status != "connected" {
		t.Fatalf("status = %+v", st)
	}
}

// Concurrent callers with an expired token refresh once.
func TestRefreshIsSerialized(t *testing.T) {
	f := newFakeUpstream(t)
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	if _, err := s.db.Exec(`UPDATE server_tokens SET expires_at = 1`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	tokens := make([]string, 8)
	errs := make([]error, 8)
	for i := range tokens {
		wg.Go(func() { tokens[i], errs[i] = s.upstreamToken(context.Background(), 1, "") })
	}
	wg.Wait()
	for i := range tokens {
		if errs[i] != nil || tokens[i] != tokens[0] {
			t.Fatalf("caller %d got %q, %v; caller 0 got %q", i, tokens[i], errs[i], tokens[0])
		}
	}
	if n := f.refreshes(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
}

// A token upstream rejects before its stated expiry is refreshed on the 401
// and the request retried.
func TestRefreshOn401(t *testing.T) {
	f := newFakeUpstream(t)
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	f.revokeAccess()
	mustListEcho(t, s, "fake")
	if n := f.refreshes(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	if st := serverStatusOf(t, s, "fake"); st.Status != "connected" {
		t.Fatalf("status = %+v", st)
	}
}

// When the AS refuses the refresh, the login is dropped and the server shows
// the error until it is connected again.
func TestRefreshRejected(t *testing.T) {
	f := newFakeUpstream(t)
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	f.revokeAccess()
	f.revokeRefresh()
	if _, w := toolsOf(t, s, "fake"); w.Code == http.StatusOK {
		t.Fatal("tools listed with a dead login")
	}
	st := serverStatusOf(t, s, "fake")
	if st.Status != "error" || st.LoggedIn || !strings.Contains(st.Error, "invalid_grant") {
		t.Fatalf("status = %+v", st)
	}
	if _, ok, _ := s.loadToken(context.Background(), 1); ok {
		t.Fatal("dead login kept")
	}
	login(t, s, "fake")
	if st := serverStatusOf(t, s, "fake"); st.Status != "connected" || st.Error != "" {
		t.Fatalf("status after reconnect = %+v", st)
	}
	mustListEcho(t, s, "fake")
}

// Without a refresh token, an expired login is dropped with a reason.
func TestExpiredWithoutRefreshToken(t *testing.T) {
	f := newFakeUpstream(t)
	f.issueRefresh = false
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	if st := serverStatusOf(t, s, "fake"); st.Refreshable {
		t.Fatalf("status = %+v", st)
	}
	f.revokeAccess()
	toolsOf(t, s, "fake")
	if st := serverStatusOf(t, s, "fake"); st.Status != "error" || !strings.Contains(st.Error, "no refresh token") {
		t.Fatalf("status = %+v", st)
	}
}

// A failed connect is recorded and shown as the server's status.
func TestConnectFailureIsShown(t *testing.T) {
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer open.Close()
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "open", "url": open.URL})
	if back := login(t, s, "open"); back != "/servers/open" {
		t.Fatalf("went to %s", back)
	}
	if st := serverStatusOf(t, s, "open"); st.Status != "error" || !strings.Contains(st.Error, "not 401") {
		t.Fatalf("status = %+v", st)
	}
}

func TestRenameDisconnectDelete(t *testing.T) {
	f := newFakeUpstream(t)
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	if w := call(t, s, "PATCH", "/api/servers/fake", map[string]string{"name": "renamed"}); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "GET", "/api/servers/fake", nil); w.Code != http.StatusNotFound {
		t.Fatalf("old name still answers: %d", w.Code)
	}
	if st := serverStatusOf(t, s, "renamed"); st.Status != "connected" {
		t.Fatalf("status = %+v", st)
	}
	mustListEcho(t, s, "renamed")

	if w := call(t, s, "DELETE", "/api/servers/renamed/login", nil); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}
	if st := serverStatusOf(t, s, "renamed"); st.Status != "needs_login" {
		t.Fatalf("status after disconnect = %+v", st)
	}

	if w := call(t, s, "DELETE", "/api/servers/renamed", nil); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM server_tokens`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tokens left after delete: %d, %v", n, err)
	}
}

func TestServerValidation(t *testing.T) {
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "linear", "url": "https://mcp.linear.app/mcp"})
	cases := []struct {
		name string
		body map[string]string
		want int
	}{
		{"underscore", map[string]string{"name": "my_server", "url": "https://x.example"}, http.StatusBadRequest},
		{"upper case", map[string]string{"name": "Linear", "url": "https://x.example"}, http.StatusBadRequest},
		{"duplicate", map[string]string{"name": "linear", "url": "https://x.example"}, http.StatusConflict},
		{"plain http", map[string]string{"name": "x", "url": "http://x.example/mcp"}, http.StatusBadRequest},
		{"not a URL", map[string]string{"name": "x", "url": "mcp.linear.app"}, http.StatusBadRequest},
		{"unknown field", map[string]string{"name": "x", "url": "https://x.example", "scope": "a"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if w := call(t, s, "POST", "/api/servers", c.body); w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body.String(), c.want)
		}
	}
	addServer(t, s, map[string]string{"name": "other", "url": "https://x.example"})
	if w := call(t, s, "PATCH", "/api/servers/other", map[string]string{"name": "linear"}); w.Code != http.StatusConflict {
		t.Errorf("rename onto an existing name: %d", w.Code)
	}
}

// A server's tool list is kept, tied to its session: listing again is
// answered from it, and a new session (here, closing the old one) lists
// afresh.
func TestToolListCache(t *testing.T) {
	f := newFakeUpstream(t)
	s, _ := newLiveServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	lists := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.listCalls
	}
	for range 2 {
		if w := call(t, s, "GET", "/api/servers/fake/tools", nil); w.Code != http.StatusOK {
			t.Fatal(w.Body.String())
		}
	}
	if n := lists(); n != 1 {
		t.Fatalf("listed %d times, want 1", n)
	}
	row, _, err := s.serverByName(context.Background(), "fake")
	if err != nil {
		t.Fatal(err)
	}
	s.pool.close(row.ID)
	if w := call(t, s, "GET", "/api/servers/fake/tools", nil); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if n := lists(); n != 2 {
		t.Fatalf("after a new session, listed %d times, want 2", n)
	}
}
