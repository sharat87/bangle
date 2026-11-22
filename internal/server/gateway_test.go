package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// newLiveServer is Bangle behind a real listener. BASE_URL stays
// http://bangle.test and the listener is the loopback origin, so harness
// tests exercise what harnesses are told to use: the loopback origin, where
// discovery, issuer and resource are all http://127.0.0.1:<port>.
func newLiveServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := newTestServer(t)
	live := httptest.NewUnstartedServer(nil)
	live.Start()
	t.Cleanup(live.Close)
	base, _ := url.Parse(live.URL)
	s.listenAddr = base.Host
	live.Config.Handler = s
	t.Cleanup(s.Close)
	return s, live
}

func addGroup(t *testing.T, s *Server, name string, servers ...string) {
	t.Helper()
	if servers == nil {
		servers = []string{}
	}
	w := call(t, s, "POST", "/api/groups", map[string]any{"name": name, "servers": servers})
	if w.Code != http.StatusCreated {
		t.Fatalf("adding group: %d %s", w.Code, w.Body.String())
	}
}

// harnessLogin logs a client in to a group the way a harness does, with the
// go-sdk's own OAuth client: discovery from the 401, DCR, then the browser
// part (here: follow /oauth/authorize to the consent page and approve).
// It returns a session on the group's endpoint.
func harnessLogin(t *testing.T, s *Server, live *httptest.Server, group, decision string) (*mcp.ClientSession, error) {
	t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "Test Harness", RedirectURIs: []string{"http://127.0.0.1:9/callback"}},
		},
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			resp, err := noFollow.Get(args.URL)
			if err != nil {
				return nil, err
			}
			resp.Body.Close()
			consent := resp.Header.Get("Location")
			id, ok := strings.CutPrefix(consent, "/consent/")
			if resp.StatusCode != http.StatusSeeOther || !ok {
				return nil, fmt.Errorf("authorize answered %d, Location %q", resp.StatusCode, consent)
			}
			// What the consent page shows.
			info, err := http.Get(live.URL + "/api/consent/" + id)
			if err != nil {
				return nil, err
			}
			info.Body.Close()
			if info.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("consent info: %d", info.StatusCode)
			}
			resp, err = noFollow.PostForm(live.URL+harnessConsentPath, url.Values{"id": {id}, "decision": {decision}})
			if err != nil {
				return nil, err
			}
			resp.Body.Close()
			back, err := url.Parse(resp.Header.Get("Location"))
			if err != nil {
				return nil, err
			}
			q := back.Query()
			if e := q.Get("error"); e != "" {
				return nil, errors.New(e)
			}
			return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-harness", Version: "1"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:     live.URL + "/g/" + group + "/mcp",
		OAuthHandler: handler,
	}, nil)
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	return names
}

func callText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError
}

// The whole of milestones 2–4 in one pass: a harness logs in to a group
// through consent, sees the live tools of two servers
// as <server>__<tool>, and calls both, on one session.
func TestGatewayEndToEnd(t *testing.T) {
	f := newFakeUpstream(t)
	s, live := newLiveServer(t)
	addServer(t, s, map[string]string{"name": "fake", "url": f.srv.URL + "/mcp"})
	login(t, s, "fake")
	addServer(t, s, map[string]string{"name": "local", "url": f.srv.URL + "/mcp"})
	login(t, s, "local")
	addServer(t, s, map[string]string{"name": "outsider", "url": f.srv.URL + "/mcp"})
	login(t, s, "outsider")
	addGroup(t, s, "myproject", "fake", "local")

	session, err := harnessLogin(t, s, live, "myproject", "approve")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	names := toolNames(t, session)
	if !slices.Contains(names, "fake__echo") || !slices.Contains(names, "local__shout") || slices.Contains(names, "outsider__echo") {
		t.Fatalf("tools = %q", names)
	}
	if text, isErr := callText(t, session, "fake__echo", map[string]any{"text": "hi"}); text != "hi" || isErr {
		t.Fatalf("fake__echo = %q, %v", text, isErr)
	}
	if text, isErr := callText(t, session, "local__shout", map[string]any{"text": "hi"}); text != "HI" || isErr {
		t.Fatalf("local__shout = %q, %v", text, isErr)
	}
	checkAudit(t, s)
	// Refusals are tool errors the model can read, and are not audited.
	for _, bad := range []string{"outsider__echo", "echo", "fake__"} {
		if text, isErr := callText(t, session, bad, nil); !isErr {
			t.Errorf("calling %q succeeded: %s", bad, text)
		}
	}

	// Live, not a snapshot: a member added later is in the same session's
	// next tools/list.
	if w := call(t, s, "PATCH", "/api/groups/myproject", map[string]any{"servers": []string{"fake", "local", "outsider"}}); w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if names := toolNames(t, session); !slices.Contains(names, "outsider__echo") {
		t.Fatalf("tools after adding a member = %q", names)
	}

	// A member that can't be reached is left out, and the group's page
	// names it.
	if w := call(t, s, "DELETE", "/api/servers/outsider/login", nil); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if names := toolNames(t, session); slices.Contains(names, "outsider__echo") || !slices.Contains(names, "fake__echo") {
		t.Fatalf("tools with a disconnected member = %q", names)
	}
	w := call(t, s, "GET", "/api/groups/myproject/tools", nil)
	if !strings.Contains(w.Body.String(), `"server":"outsider"`) {
		t.Fatalf("group tools did not report the failure: %s", w.Body.String())
	}

	// The grant shows on the Clients page; revoking it ends access.
	w = call(t, s, "GET", "/api/clients", nil)
	clients := decode[[]map[string]any](t, w)
	if len(clients) != 1 || clients[0]["client_name"] != "Test Harness" || clients[0]["group"] != "myproject" || clients[0]["last_used_at"] == nil {
		t.Fatalf("clients = %v", clients)
	}
	token := issueTestToken(t, s, "myproject")
	if code := rawGatewayStatus(t, live, "myproject", token); code != http.StatusOK {
		t.Fatalf("live token: %d", code)
	}
	if w := call(t, s, "DELETE", fmt.Sprintf("/api/clients/%v", clients[0]["id"]), nil); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	// (The go-sdk client would answer the 401 by logging in again, so the
	// revoked token is checked with a plain request.)
	if code := rawGatewayStatus(t, live, "myproject", token); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", code)
	}
}

// rawGatewayStatus sends an initialize with token and returns the status.
func rawGatewayStatus(t *testing.T, live *httptest.Server, group, token string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	req, _ := http.NewRequest("POST", live.URL+"/g/"+group+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestGatewayNeedsTokenForItsGroup(t *testing.T) {
	s, live := newLiveServer(t)
	addGroup(t, s, "myproject")
	addGroup(t, s, "other")

	resp, err := http.Post(live.URL+"/g/myproject/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/g/myproject/mcp"`, live.URL)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != want {
		t.Fatalf("unauthenticated: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// A token for one group does not open another (RFC 8707 binding).
	session, err := harnessLogin(t, s, live, "other", "approve")
	if err != nil {
		t.Fatal(err)
	}
	session.Close()
	var grantGroup string
	if err := s.db.QueryRow(`SELECT g.name FROM harness_grants h JOIN groups g ON g.id = h.group_id`).Scan(&grantGroup); err != nil || grantGroup != "other" {
		t.Fatalf("grant group = %q, %v", grantGroup, err)
	}
	token := issueTestToken(t, s, "other")
	req, _ := http.NewRequest("POST", live.URL+"/g/myproject/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("other group's token on myproject: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// issueTestToken runs the token endpoint's refresh on the group's grant and
// returns the new access token, proving rotation on the way: the old refresh
// token stops working.
func issueTestToken(t *testing.T, s *Server, group string) string {
	t.Helper()
	// Swap in a refresh token we know, as the token endpoint would store it.
	known := "bangle_rt_test-" + group
	if _, err := s.db.Exec(`UPDATE harness_grants SET refresh_hash = ? WHERE group_id = (SELECT id FROM groups WHERE name = ?)`,
		hashToken(known), group); err != nil {
		t.Fatal(err)
	}
	var clientID string
	if err := s.db.QueryRow(`SELECT c.client_id FROM harness_grants h JOIN harness_clients c ON c.id = h.client_id
		JOIN groups g ON g.id = h.group_id WHERE g.name = ?`, group).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	refresh := func(rt string) (map[string]any, int) {
		w := call(t, s, "POST", harnessTokenPath, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID}})
		return decode[map[string]any](t, w), w.Code
	}
	tok, code := refresh(known)
	if code != http.StatusOK || tok["access_token"] == nil {
		t.Fatalf("refresh: %d %v", code, tok)
	}
	if _, code := refresh(known); code != http.StatusBadRequest {
		t.Fatalf("a rotated refresh token was accepted again: %d", code)
	}
	// Each refresh replaces the access token too, so the latest is returned.
	latest, code := refresh(tok["refresh_token"].(string))
	if code != http.StatusOK {
		t.Fatalf("the new refresh token was refused: %d", code)
	}
	return latest["access_token"].(string)
}

func TestHarnessDeny(t *testing.T) {
	s, live := newLiveServer(t)
	addGroup(t, s, "myproject")
	if _, err := harnessLogin(t, s, live, "myproject", "deny"); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("deny: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM harness_grants`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("grants after deny: %d, %v", n, err)
	}
}

func TestHarnessAuthorizeRefusals(t *testing.T) {
	s := newTestServer(t)
	addGroup(t, s, "myproject")
	w := call(t, s, "POST", harnessRegisterPath, map[string]any{"client_name": "x", "redirect_uris": []string{"javascript:alert(1)"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("javascript: redirect registered: %d", w.Code)
	}
	w = call(t, s, "POST", harnessRegisterPath, map[string]any{"client_name": "x", "redirect_uris": []string{"http://127.0.0.1:5000/cb"}})
	clientID := decode[map[string]any](t, w)["client_id"].(string)

	base := url.Values{
		"client_id": {clientID}, "redirect_uri": {"http://127.0.0.1:6000/cb"}, "response_type": {"code"},
		"code_challenge": {"x"}, "code_challenge_method": {"S256"}, "resource": {s.groupEndpoint("myproject")},
	}
	with := func(k, v string) url.Values {
		q := url.Values{}
		for kk, vv := range base {
			q[kk] = vv
		}
		q.Set(k, v)
		return q
	}
	cases := []struct {
		name  string
		q     url.Values
		check func(*httptest.ResponseRecorder) bool
	}{
		{"loopback on another port is fine", base, func(w *httptest.ResponseRecorder) bool {
			return w.Code == http.StatusSeeOther && strings.HasPrefix(w.Header().Get("Location"), "/consent/")
		}},
		{"unregistered redirect is a page, not a redirect", with("redirect_uri", "https://evil.example/cb"), func(w *httptest.ResponseRecorder) bool {
			return w.Code == http.StatusBadRequest && w.Header().Get("Location") == ""
		}},
		{"no PKCE", with("code_challenge_method", "plain"), func(w *httptest.ResponseRecorder) bool {
			return strings.Contains(w.Header().Get("Location"), "error=invalid_request")
		}},
		{"unknown group", with("resource", s.groupEndpoint("nope")), func(w *httptest.ResponseRecorder) bool {
			return strings.Contains(w.Header().Get("Location"), "error=invalid_target")
		}},
	}
	for _, c := range cases {
		w := call(t, s, "GET", harnessAuthorizePath+"?"+c.q.Encode(), nil)
		if !c.check(w) {
			t.Errorf("%s: %d Location=%q", c.name, w.Code, w.Header().Get("Location"))
		}
	}
}

// checkAudit runs right after TestGatewayEndToEnd's two calls: both are
// recorded, newest first, with who made them, sealed arguments and the
// result.
func checkAudit(t *testing.T, s *Server) {
	t.Helper()
	w := call(t, s, "GET", "/api/calls", nil)
	list := decode[struct {
		Calls []callJSON `json:"calls"`
		More  bool       `json:"more"`
	}](t, w)
	if len(list.Calls) != 2 || list.More {
		t.Fatalf("calls = %+v", list)
	}
	shout, echo := list.Calls[0], list.Calls[1]
	if shout.ServerName != "local" || shout.Tool != "shout" || echo.ServerName != "fake" || echo.Tool != "echo" {
		t.Fatalf("calls = %+v", list.Calls)
	}
	for _, c := range list.Calls {
		if c.ClientName != "Test Harness" || c.GroupName != "myproject" || c.Outcome != "ok" || c.DurationMS == nil || c.Arguments != nil {
			t.Fatalf("call = %+v", c)
		}
	}
	var stored string
	if err := s.db.QueryRow(`SELECT arguments FROM tool_calls WHERE id = ?`, echo.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// Look for the plaintext's quotes, which base64 can't contain: "hi" alone
	// turns up in random ciphertext now and then (it failed CI once).
	if !strings.HasPrefix(stored, sealedPrefix) || strings.Contains(stored, `"text"`) {
		t.Fatalf("arguments stored as %q", stored)
	}
	detail := decode[callJSON](t, call(t, s, "GET", fmt.Sprintf("/api/calls/%d", echo.ID), nil))
	if string(detail.Arguments) != `{"text":"hi"}` || !strings.Contains(string(detail.Result), `"text":"hi"`) {
		t.Fatalf("detail arguments %s, result %s", detail.Arguments, detail.Result)
	}
	filtered := decode[struct {
		Calls []callJSON `json:"calls"`
	}](t, call(t, s, "GET", "/api/calls?server=fake", nil))
	if len(filtered.Calls) != 1 || filtered.Calls[0].ID != echo.ID {
		t.Fatalf("filtered = %+v", filtered.Calls)
	}
}

// Calls past retention go; newer ones stay.
func TestAuditPrunes(t *testing.T) {
	s := newTestServer(t)
	for _, created := range []string{"2000-01-01 00:00:00", time.Now().UTC().Format("2006-01-02 15:04:05")} {
		if _, err := s.db.Exec(`INSERT INTO tool_calls (grant_id, client_name, group_name, server_name, tool, arguments, created_at)
			VALUES (1, 'c', 'g', 's', 't', '', ?)`, created); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.auditStart(context.Background(), gatewayGrant{ID: 1, ClientName: "c", GroupName: "g"}, "s", "t", nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE created_at < '2001-01-01'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old calls left: %d, %v", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tool_calls`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("calls left: %d, %v", n, err)
	}
}

// A tool's page calls it through /api/servers/{name}/call, audited as the
// Bangle UI with no group.
func TestCallFromUI(t *testing.T) {
	f := newFakeUpstream(t)
	s := newTestServer(t)
	t.Cleanup(s.Close)
	addServer(t, s, map[string]string{"name": "local", "url": f.srv.URL + "/mcp"})
	login(t, s, "local")
	w := call(t, s, "POST", "/api/servers/local/call", map[string]any{"tool": "shout", "arguments": map[string]string{"text": "hi"}})
	if w.Code != http.StatusOK {
		t.Fatalf("call: %d %s", w.Code, w.Body.String())
	}
	out := decode[struct {
		ID      int64           `json:"id"`
		Outcome string          `json:"outcome"`
		Result  json.RawMessage `json:"result"`
	}](t, w)
	if out.Outcome != "ok" || !strings.Contains(string(out.Result), `"text":"HI"`) {
		t.Fatalf("call = %+v %s", out, out.Result)
	}
	detail := decode[callJSON](t, call(t, s, "GET", fmt.Sprintf("/api/calls/%d", out.ID), nil))
	if detail.ClientName != "Bangle UI" || detail.GroupName != "" || detail.Tool != "shout" || string(detail.Arguments) != `{"text":"hi"}` {
		t.Fatalf("recorded = %+v", detail)
	}
	if w := call(t, s, "POST", "/api/servers/local/call", map[string]any{"arguments": map[string]string{}}); w.Code != http.StatusBadRequest {
		t.Fatalf("no tool: %d", w.Code)
	}
}

// A server's page sets which groups it's in, leaving the groups' other
// members alone, and refuses an unknown group without changing anything.
func TestSetServerGroups(t *testing.T) {
	s := newTestServer(t)
	addServer(t, s, map[string]string{"name": "local", "url": "https://local.example/mcp"})
	addServer(t, s, map[string]string{"name": "other", "url": "https://other.example/mcp"})
	for _, g := range []string{"a", "b"} {
		if w := call(t, s, "POST", "/api/groups", map[string]any{"name": g, "servers": []string{"other"}}); w.Code != http.StatusCreated {
			t.Fatalf("adding %s: %d %s", g, w.Code, w.Body.String())
		}
	}
	members := func(g string) string {
		return strings.Join(decode[groupJSON](t, call(t, s, "GET", "/api/groups/"+g, nil)).Servers, " ")
	}
	if w := call(t, s, "PUT", "/api/servers/local/groups", map[string]any{"groups": []string{"b", "a", "a"}}); w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if members("a") != "local other" || members("b") != "local other" {
		t.Fatalf("after joining: a %q, b %q", members("a"), members("b"))
	}
	if w := call(t, s, "PUT", "/api/servers/local/groups", map[string]any{"groups": []string{"b", "nope"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown group: %d %s", w.Code, w.Body.String())
	}
	if members("a") != "local other" {
		t.Fatalf("a refused put changed a: %q", members("a"))
	}
	if w := call(t, s, "PUT", "/api/servers/local/groups", map[string]any{"groups": []string{"b"}}); w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if members("a") != "other" || members("b") != "local other" {
		t.Fatalf("after leaving a: a %q, b %q", members("a"), members("b"))
	}
}
