package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Upstream OAuth: logging Bangle in to an MCP server, in my browser.
//
//  1. Discovery. An unauthenticated MCP request gets a 401 whose
//     WWW-Authenticate names the RFC 9728 protected-resource metadata
//     (resource_metadata=...); without one, the well-known URL
//     /.well-known/oauth-protected-resource[/path]. That names the
//     authorization server, whose RFC 8414 (else OIDC) metadata gives the
//     endpoints.
//  2. Client: RFC 7591 dynamic client registration as a public client,
//     fresh per login.
//  3. POST /oauth/upstream/start (the Connect form) answers 303 to the
//     authorization endpoint, with PKCE S256 and state.
//  4. GET /oauth/upstream/callback, the one fixed redirect URI for every
//     server, exchanges the code and stores the sealed tokens, then 303s
//     back to the server's page.
//
// Any failure in 1–4 is recorded as the server's auth_error, which the
// server's page shows as its status. Refresh is upstream_token.go.

const (
	upstreamStartPath    = "/oauth/upstream/start"
	upstreamCallbackPath = "/oauth/upstream/callback"

	// flowTTL is how long a login may sit on the authorization server's page.
	flowTTL = 15 * time.Minute
	// discoveryTimeout bounds steps 1 and 2 together, and each token request.
	discoveryTimeout = 30 * time.Second
	// maxMetadataBody caps every metadata, registration and token response.
	maxMetadataBody = 256 << 10
)

type protectedResourceMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type authServerMeta struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	ScopesSupported               []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// discovered is everything step 1 learns about a server.
type discovered struct {
	Resource protectedResourceMeta
	AS       authServerMeta
}

// discover runs step 1 against an MCP server URL.
func discover(ctx context.Context, client *http.Client, serverURL string) (discovered, error) {
	metadataURL, err := probeResourceMetadata(ctx, client, serverURL)
	if err != nil {
		return discovered{}, err
	}
	var prmURLs []string
	if metadataURL != "" {
		if err := checkUpstreamURL(metadataURL); err != nil {
			return discovered{}, fmt.Errorf("resource_metadata in WWW-Authenticate: %w", err)
		}
		prmURLs = []string{metadataURL}
	} else if prmURLs, err = wellKnownURLs(serverURL, "oauth-protected-resource"); err != nil {
		return discovered{}, err
	}
	var prm protectedResourceMeta
	if err := fetchFirstJSON(ctx, client, prmURLs, &prm); err != nil {
		return discovered{}, fmt.Errorf("protected resource metadata: %w", err)
	}
	if prm.Resource == "" {
		return discovered{}, errors.New("protected resource metadata has no resource")
	}
	if len(prm.AuthorizationServers) == 0 {
		return discovered{}, errors.New("protected resource metadata lists no authorization_servers")
	}

	issuer := prm.AuthorizationServers[0]
	if err := checkUpstreamURL(issuer); err != nil {
		return discovered{}, fmt.Errorf("authorization server: %w", err)
	}
	as, err := fetchAuthServerMeta(ctx, client, issuer)
	if err != nil {
		return discovered{}, err
	}
	return discovered{Resource: prm, AS: as}, nil
}

// probeResourceMetadata sends an unauthenticated initialize and returns the
// resource_metadata URL from the 401's WWW-Authenticate, or "" when the 401
// names none. Anything but a 401 means the server is not asking for OAuth,
// which Bangle does not support yet.
func probeResourceMetadata(ctx context.Context, client *http.Client, serverURL string) (string, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"bangle","version":"0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching %s: %w", serverURL, err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetadataBody)); err != nil {
		return "", fmt.Errorf("reading %s: %w", serverURL, err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("%s answered an unauthenticated request with HTTP %d, not 401: it does not ask for OAuth, and Bangle only supports OAuth servers so far", serverURL, resp.StatusCode)
	}
	for _, h := range resp.Header.Values("WWW-Authenticate") {
		if v := authParam(h, "resource_metadata"); v != "" {
			return v, nil
		}
	}
	return "", nil
}

// authParam returns the value of one auth-param (RFC 9110 §11.2) in a
// WWW-Authenticate value, quoted or not, or "" when it is absent.
func authParam(header, name string) string {
	s := header
	for s != "" {
		i := strings.Index(s, "=")
		if i < 0 {
			return ""
		}
		// The key is the last token before "=".
		key := strings.TrimSpace(s[:i])
		if j := strings.LastIndexAny(key, " ,"); j >= 0 {
			key = key[j+1:]
		}
		s = strings.TrimLeft(s[i+1:], " ")
		var value string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			k := 1
			for ; k < len(s) && s[k] != '"'; k++ {
				if s[k] == '\\' && k+1 < len(s) {
					k++
				}
				b.WriteByte(s[k])
			}
			value = b.String()
			s = s[min(k+1, len(s)):]
		} else {
			end := strings.IndexAny(s, ", ")
			if end < 0 {
				end = len(s)
			}
			value, s = s[:end], s[end:]
		}
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

// wellKnownURLs is RFC 9728 §3.1 / RFC 8414 §3.1's path insertion: the
// well-known suffix goes between the origin and the path. For a URL with a
// path, the root form follows as the second candidate.
func wellKnownURLs(rawURL, suffix string) ([]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%q is not an absolute URL", rawURL)
	}
	root := u.Scheme + "://" + u.Host + "/.well-known/" + suffix
	path := strings.TrimSuffix(u.Path, "/")
	if path == "" {
		return []string{root}, nil
	}
	return []string{root + path, root}, nil
}

// fetchAuthServerMeta tries the authorization server's metadata in the MCP
// spec's order: RFC 8414 then OIDC, path-inserted, then OIDC path-appended.
// The issuer it reports must be the one asked for (RFC 8414 §3.3).
func fetchAuthServerMeta(ctx context.Context, client *http.Client, issuer string) (authServerMeta, error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return authServerMeta{}, fmt.Errorf("authorization server %q is not a URL", issuer)
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")
	candidates := []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + "/.well-known/openid-configuration" + path,
	}
	if path != "" {
		candidates = append(candidates, origin+path+"/.well-known/openid-configuration")
	}
	var as authServerMeta
	if err := fetchFirstJSON(ctx, client, candidates, &as); err != nil {
		return authServerMeta{}, fmt.Errorf("authorization server metadata: %w", err)
	}
	if strings.TrimSuffix(as.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return authServerMeta{}, fmt.Errorf("authorization server metadata names issuer %q, not %q", as.Issuer, issuer)
	}
	if as.AuthorizationEndpoint == "" || as.TokenEndpoint == "" {
		return authServerMeta{}, errors.New("authorization server metadata has no authorization_endpoint or token_endpoint")
	}
	for _, e := range []string{as.AuthorizationEndpoint, as.TokenEndpoint} {
		if err := checkUpstreamURL(e); err != nil {
			return authServerMeta{}, fmt.Errorf("authorization server endpoint: %w", err)
		}
	}
	if as.RegistrationEndpoint != "" {
		if err := checkUpstreamURL(as.RegistrationEndpoint); err != nil {
			return authServerMeta{}, fmt.Errorf("registration endpoint: %w", err)
		}
	}
	if len(as.CodeChallengeMethodsSupported) > 0 && !slices.Contains(as.CodeChallengeMethodsSupported, "S256") {
		return authServerMeta{}, errors.New("authorization server does not support PKCE S256")
	}
	return as, nil
}

func fetchFirstJSON(ctx context.Context, client *http.Client, urls []string, dst any) error {
	var errs []error
	for _, u := range urls {
		err := fetchJSON(ctx, client, u, dst)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func fetchJSON(ctx context.Context, client *http.Client, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBody))
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("GET %s: invalid JSON: %w", u, err)
	}
	return nil
}

// chooseScopes is the configured scopes, else the resource's
// scopes_supported, plus offline_access whenever the authorization server
// lists it: Railway issues a refresh token only for offline_access.
func chooseScopes(configured string, d discovered) []string {
	scopes := strings.Fields(configured)
	if len(scopes) == 0 {
		scopes = slices.Clone(d.Resource.ScopesSupported)
	}
	if slices.Contains(d.AS.ScopesSupported, "offline_access") && !slices.Contains(scopes, "offline_access") {
		scopes = append(scopes, "offline_access")
	}
	return scopes
}

// oauthClient is who Bangle authenticates to the token endpoint as.
type oauthClient struct {
	ID     string
	Secret string // plaintext in memory; sealed in the DB
	// AuthMethod is "none", "client_secret_post" or "client_secret_basic".
	AuthMethod string
}

// secretAuthMethod picks how to present a client secret: what registration
// said if it said something usable, else client_secret_basic, RFC 8414's
// default.
func secretAuthMethod(registered string) string {
	if registered == "client_secret_post" || registered == "client_secret_basic" {
		return registered
	}
	return "client_secret_basic"
}

// registerClient is RFC 7591 dynamic client registration, as a public client
// (PKCE, no secret) with the one fixed redirect URI.
func registerClient(ctx context.Context, client *http.Client, endpoint, redirectURI string, scopes []string) (oauthClient, error) {
	if endpoint == "" {
		return oauthClient{}, errors.New("the authorization server has no dynamic client registration, which Bangle needs")
	}
	payload := map[string]any{
		"client_name":                "Bangle",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if len(scopes) > 0 {
		payload["scope"] = strings.Join(scopes, " ")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return oauthClient{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return oauthClient{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return oauthClient{}, fmt.Errorf("client registration: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBody))
	if err != nil {
		return oauthClient{}, fmt.Errorf("client registration: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return oauthClient{}, fmt.Errorf("client registration: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var reg struct {
		ClientID                string `json:"client_id"`
		ClientSecret            string `json:"client_secret"`
		TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(respBody, &reg); err != nil {
		return oauthClient{}, fmt.Errorf("client registration: invalid JSON: %w", err)
	}
	if reg.ClientID == "" {
		return oauthClient{}, errors.New("client registration returned no client_id")
	}
	c := oauthClient{ID: reg.ClientID, AuthMethod: "none"}
	// Some servers issue a secret even to a client that asked to be public.
	if reg.ClientSecret != "" {
		c.Secret = reg.ClientSecret
		c.AuthMethod = secretAuthMethod(reg.TokenEndpointAuthMethod)
	}
	return c, nil
}

// pendingFlow is a login waiting for its callback. Flows live in memory on
// purpose: nothing secret reaches the DB before the exchange succeeds, and a
// restart mid-login only means clicking Connect again.
type pendingFlow struct {
	ServerID      int64
	Verifier      string
	Issuer        string
	TokenEndpoint string
	Client        oauthClient
	Resource      string
	Scope         string
	Expires       time.Time
}

type pendingFlows struct {
	mu    sync.Mutex
	flows map[string]pendingFlow // by state
}

func (p *pendingFlows) put(state string, f pendingFlow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, old := range p.flows {
		if now.After(old.Expires) {
			delete(p.flows, k)
		}
	}
	p.flows[state] = f
}

// take removes and returns the flow for state. A state is good for one
// callback only.
func (p *pendingFlows) take(state string) (pendingFlow, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.flows[state]
	delete(p.flows, state)
	if !ok || time.Now().After(f.Expires) {
		return pendingFlow{}, false
	}
	return f, true
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkceChallenge is RFC 7636's S256: base64url(sha256(verifier)), unpadded.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// handleUpstreamStart is the Connect form's POST: discovery and registration,
// then 303 to the authorization server. A failure is recorded on the server
// and the browser goes back to its page, which shows it.
func (s *Server) handleUpstreamStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTMLPage(w, http.StatusMethodNotAllowed, "Method not allowed", "Connect is a POST from the MCP server's page.")
		return
	}
	name := r.PostFormValue("server")
	row, ok, err := s.serverByName(r.Context(), name)
	if err != nil {
		writeHTMLPage(w, http.StatusInternalServerError, "Connect failed", err.Error())
		return
	}
	if !ok {
		writeHTMLPage(w, http.StatusNotFound, "Connect failed", fmt.Sprintf("There is no MCP server named %q.", name))
		return
	}
	authURL, err := s.startLogin(r.Context(), row)
	if err != nil {
		log.Printf("server %q: connect: %v", row.Name, err)
		if err := s.setAuthError(r.Context(), row.ID, "connect: "+err.Error()); err != nil {
			writeHTMLPage(w, http.StatusInternalServerError, "Connect failed", err.Error())
			return
		}
		http.Redirect(w, r, serverPagePath(row.Name), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// startLogin runs steps 1–2, records the pending flow and returns the
// authorization URL to send the browser to.
func (s *Server) startLogin(ctx context.Context, row serverRow) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	d, err := discover(ctx, s.upstream, row.URL)
	if err != nil {
		return "", err
	}
	scopes := chooseScopes(row.Scopes, d)
	client, err := registerClient(ctx, s.upstream, d.AS.RegistrationEndpoint, s.baseURL+upstreamCallbackPath, scopes)
	if err != nil {
		return "", err
	}
	state, err := randomToken(24)
	if err != nil {
		return "", err
	}
	verifier, err := randomToken(32)
	if err != nil {
		return "", err
	}
	s.flows.put(state, pendingFlow{
		ServerID:      row.ID,
		Verifier:      verifier,
		Issuer:        d.AS.Issuer,
		TokenEndpoint: d.AS.TokenEndpoint,
		Client:        client,
		Resource:      d.Resource.Resource,
		Scope:         strings.Join(scopes, " "),
		Expires:       time.Now().Add(flowTTL),
	})

	u, err := url.Parse(d.AS.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", client.ID)
	q.Set("redirect_uri", s.baseURL+upstreamCallbackPath)
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("resource", d.Resource.Resource)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	// OIDC Core §11: an authorization server ignores offline_access (and so
	// issues no refresh token) unless the request also says prompt=consent.
	// Railway's is one of those, and without a refresh token its 1-hour
	// access tokens mean logging in every hour.
	if slices.Contains(scopes, "offline_access") {
		q.Set("prompt", "consent")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// handleUpstreamCallback is step 4, the redirect URI registered for every
// server.
func (s *Server) handleUpstreamCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	flow, ok := s.flows.take(q.Get("state"))
	if !ok {
		writeHTMLPage(w, http.StatusBadRequest, "Login failed",
			"This login is unknown or expired (logins last 15 minutes, and a restart forgets them). Start again from the server's page.")
		return
	}
	row, ok, err := s.serverByID(r.Context(), flow.ServerID)
	if err != nil {
		writeHTMLPage(w, http.StatusInternalServerError, "Login failed", err.Error())
		return
	}
	if !ok {
		writeHTMLPage(w, http.StatusBadRequest, "Login failed", "The MCP server was deleted while its login was in progress.")
		return
	}
	if err := s.completeLogin(r.Context(), flow, q); err != nil {
		log.Printf("server %q: login: %v", row.Name, err)
		if err := s.setAuthError(r.Context(), row.ID, "login: "+err.Error()); err != nil {
			writeHTMLPage(w, http.StatusInternalServerError, "Login failed", err.Error())
			return
		}
	} else {
		log.Printf("server %q: logged in", row.Name)
	}
	http.Redirect(w, r, serverPagePath(row.Name), http.StatusSeeOther)
}

// completeLogin checks the callback, exchanges the code and stores the login.
func (s *Server) completeLogin(ctx context.Context, flow pendingFlow, q url.Values) error {
	if e := q.Get("error"); e != "" {
		if d := q.Get("error_description"); d != "" {
			e += ": " + d
		}
		return fmt.Errorf("the authorization server refused: %s", e)
	}
	code := q.Get("code")
	if code == "" {
		return errors.New("the authorization server redirected back without a code (for Railway, set scopes explicitly)")
	}
	// RFC 9207: an iss on the response must be the issuer we sent the browser
	// to, or another server's code is being replayed at us.
	if iss := q.Get("iss"); iss != "" && strings.TrimSuffix(iss, "/") != strings.TrimSuffix(flow.Issuer, "/") {
		return fmt.Errorf("the response came from issuer %q, not %q", iss, flow.Issuer)
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.baseURL+upstreamCallbackPath)
	form.Set("code_verifier", flow.Verifier)
	form.Set("resource", flow.Resource)
	tok, err := postTokenForm(ctx, s.upstream, flow.TokenEndpoint, flow.Client, form)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	scope := tok.Scope
	if scope == "" {
		scope = flow.Scope // RFC 6749 §5.1: omitted means as requested
	}
	return s.saveLogin(ctx, flow.ServerID, storedToken{
		TokenEndpoint: flow.TokenEndpoint,
		Client:        flow.Client,
		Resource:      flow.Resource,
		Scope:         scope,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		ExpiresAt:     expiryMS(tok.ExpiresIn),
	})
}

func (s *Server) setAuthError(ctx context.Context, serverID int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE servers SET auth_error = ? WHERE id = ?`, msg, serverID)
	return err
}

func serverPagePath(name string) string {
	return "/servers/" + url.PathEscape(name)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// tokenRejectedError is a token endpoint's non-200 answer.
type tokenRejectedError struct {
	Status int
	Body   string
}

func (e *tokenRejectedError) Error() string {
	return fmt.Sprintf("token endpoint answered HTTP %d: %s", e.Status, e.Body)
}

// postTokenForm POSTs a grant (authorization_code or refresh_token) to a
// token endpoint, authenticating as c.
func postTokenForm(ctx context.Context, client *http.Client, endpoint string, c oauthClient, form url.Values) (tokenResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	switch c.AuthMethod {
	case "none":
		form.Set("client_id", c.ID)
	case "client_secret_post":
		form.Set("client_id", c.ID)
		form.Set("client_secret", c.Secret)
	case "client_secret_basic":
	default:
		return tokenResponse{}, fmt.Errorf("unknown token auth method %q", c.AuthMethod)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.AuthMethod == "client_secret_basic" {
		// RFC 6749 §2.3.1: both halves are form-encoded first.
		req.SetBasicAuth(url.QueryEscape(c.ID), url.QueryEscape(c.Secret))
	}
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBody))
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, &tokenRejectedError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("invalid token response: %w", err)
	}
	if tok.AccessToken == "" {
		return tokenResponse{}, errors.New("token response has no access_token")
	}
	if tok.TokenType != "" && !strings.EqualFold(tok.TokenType, "bearer") {
		return tokenResponse{}, fmt.Errorf("token response has token_type %q, not Bearer", tok.TokenType)
	}
	return tok, nil
}

// expiryMS turns expires_in into the stored unix-millisecond expiry, 0 when
// the endpoint gave none.
func expiryMS(expiresIn int64) int64 {
	if expiresIn <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
}

// writeHTMLPage is the plain page a browser lands on when a login cannot get
// back to a server's page.
func writeHTMLPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s</title><style>:root{color-scheme:light dark}body{font-family:system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem}</style></head>
<body><h1>%s</h1><p>%s</p><p><a href="/">Back to Bangle</a></p></body></html>
`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}
