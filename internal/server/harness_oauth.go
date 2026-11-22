package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bangle as the OAuth authorization server for harnesses, so a harness
// (Claude Code, Codex, ...) logs in to a group the same way it logs in to
// any remote MCP server, and no token is copied around by hand.
//
//  1. An unauthenticated /g/<group>/mcp request gets 401 with
//     WWW-Authenticate naming /.well-known/oauth-protected-resource/g/<group>/mcp
//     (RFC 9728), whose authorization server is Bangle itself
//     (/.well-known/oauth-authorization-server, RFC 8414).
//  2. The harness registers at /oauth/register (RFC 7591): public clients
//     only, PKCE S256 required.
//  3. /oauth/authorize checks the request and sends the browser to the
//     consent page, /consent/<id>, which shows the client's name and the
//     group. Approve or Deny posts /oauth/consent, which redirects back to
//     the harness with a code (or access_denied).
//  4. /oauth/token exchanges the code, and later refresh tokens. Every
//     refresh replaces both tokens.
//
// Two origins: a harness may come in through BASE_URL (Caddy's https) or
// straight to the loopback listener (http://127.0.0.1:7060). Node- and
// rustls-based harnesses don't trust Caddy's local CA, so the loopback
// origin is the one to give them. Everything a harness discovers (the
// challenge, resource metadata, issuer, endpoints) is built from the origin
// it came in on, which originFor reads from the request. That is not taking
// BASE_URL from the Host header: the request guard admits only BASE_URL's
// host or loopback on the listen port, so the origin is one of those two.
//
// A grant is bound to one group: the authorize request's resource (RFC
// 8707) must be that group's endpoint, and its tokens work there only.
// Tokens are stored as sha256, never as themselves. Pending consents and
// codes live in memory: a restart mid-login means logging in again.
// Revoking a grant (the Clients page) deletes it.

const (
	harnessAccessTTL  = time.Hour
	harnessConsentTTL = 10 * time.Minute
	harnessCodeTTL    = 5 * time.Minute

	harnessRegisterPath  = "/oauth/register"
	harnessAuthorizePath = "/oauth/authorize"
	harnessConsentPath   = "/oauth/consent"
	harnessTokenPath     = "/oauth/token"

	protectedResourcePrefix = "/.well-known/oauth-protected-resource"
	authServerMetadataPath  = "/.well-known/oauth-authorization-server"
)

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---- discovery

// originFor is the origin a request came in on: BASE_URL, or the loopback
// listener (see above).
func (s *Server) originFor(r *http.Request) string {
	if r.Host == s.baseHost {
		return s.baseURL
	}
	return "http://" + r.Host
}

// localOrigin is the loopback origin the UI tells harnesses to use. When
// Bangle listens on every interface (in a container, -listen 0.0.0.0:7060),
// that's 127.0.0.1 on the same port, which is where the host publishes it.
func (s *Server) localOrigin() string {
	host, port, err := net.SplitHostPort(s.listenAddr)
	if err == nil && (host == "" || net.ParseIP(host).IsUnspecified()) {
		return "http://" + net.JoinHostPort("127.0.0.1", port)
	}
	return "http://" + s.listenAddr
}

func endpointAt(origin, group string) string {
	return origin + "/g/" + group + "/mcp"
}

func resourceMetadataURL(origin, group string) string {
	return origin + protectedResourcePrefix + "/g/" + group + "/mcp"
}

// serveProtectedResource serves RFC 9728 metadata for a group endpoint.
func (s *Server) serveProtectedResource(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, protectedResourcePrefix)
	name, ok := strings.CutPrefix(rest, "/g/")
	name, ok2 := strings.CutSuffix(name, "/mcp")
	if !ok || !ok2 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if _, found, err := s.groupByName(r.Context(), name); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	} else if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no group named %q", name))
		return
	}
	origin := s.originFor(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 endpointAt(origin, name),
		"authorization_servers":    []string{origin},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Bangle group " + name,
	})
}

func (s *Server) serveAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	origin := s.originFor(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         origin,
		"authorization_endpoint":                         origin + harnessAuthorizePath,
		"token_endpoint":                                 origin + harnessTokenPath,
		"registration_endpoint":                          origin + harnessRegisterPath,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// ---- the gateway's check

// authorizeGateway lets a request through only with a live access token
// granted for this group. Otherwise it answers 401 with the challenge that
// starts a harness's login (RFC 9728 §5.1).
// gatewayGrant is who a gateway request comes from: the grant its token
// belongs to. The audit records it on every call (audit.go).
type gatewayGrant struct {
	ID         int64
	ClientName string
	GroupName  string
	Expires    time.Time
}

func (s *Server) authorizeGateway(w http.ResponseWriter, r *http.Request, g groupRow) (gatewayGrant, bool) {
	challenge := fmt.Sprintf(`Bearer resource_metadata="%s"`, resourceMetadataURL(s.originFor(r), g.Name))
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", challenge)
		writeJSONError(w, http.StatusUnauthorized, "log in: this endpoint needs a token for group "+g.Name)
		return gatewayGrant{}, false
	}
	var grantID, groupID, expires int64
	var clientName string
	err := s.db.QueryRowContext(r.Context(), `
		SELECT g.id, g.group_id, g.access_expires_at, c.name
		FROM harness_grants g JOIN harness_clients c ON c.id = g.client_id
		WHERE g.access_hash = ?`,
		hashToken(token)).Scan(&grantID, &groupID, &expires, &clientName)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (groupID != g.ID || time.Now().UnixMilli() >= expires)) {
		w.Header().Set("WWW-Authenticate", challenge+`, error="invalid_token"`)
		writeJSONError(w, http.StatusUnauthorized, "the token is unknown, expired, revoked, or for another group")
		return gatewayGrant{}, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return gatewayGrant{}, false
	}
	// last_used_at is shown on the Clients page; once a minute is enough.
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE harness_grants SET last_used_at = CURRENT_TIMESTAMP
		WHERE id = ? AND (last_used_at IS NULL OR last_used_at < datetime('now', '-1 minute'))`, grantID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return gatewayGrant{}, false
	}
	return gatewayGrant{ID: grantID, ClientName: clientName, GroupName: g.Name, Expires: time.UnixMilli(expires)}, true
}

// ---- registration

// validRedirectURI accepts https, http to loopback (RFC 8252 §7.3), and an
// app's own scheme (RFC 8252 §7.1, e.g. cursor://), but never a scheme a
// browser would run or read locally.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	case "javascript", "data", "file", "vbscript", "blob", "about":
		return false
	default:
		return true
	}
}

func (s *Server) handleHarnessRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	// Registration metadata has many optional fields; only these matter, so
	// unknown ones are ignored rather than refused (RFC 7591 §2).
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON: "+err.Error())
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirectURI(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("%q is not an allowed redirect URI", u))
			return
		}
	}
	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		name = "Unnamed client"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	clientID, err := randomToken(18)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	uris, err := json.Marshal(req.RedirectURIs)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO harness_clients (client_id, name, redirect_uris) VALUES (?, ?, ?)`, clientID, name, string(uris)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Registration is open and harnesses register on every login, so a
	// registration nobody approved within a day is dropped.
	if _, err := s.db.ExecContext(r.Context(), `
		DELETE FROM harness_clients WHERE created_at < datetime('now', '-1 day')
		AND id NOT IN (SELECT client_id FROM harness_grants)`); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

type harnessClient struct {
	ID           int64
	ClientID     string
	Name         string
	RedirectURIs []string
}

func (s *Server) harnessClientByClientID(ctx context.Context, clientID string) (harnessClient, bool, error) {
	var c harnessClient
	var uris string
	err := s.db.QueryRowContext(ctx, `SELECT id, client_id, name, redirect_uris FROM harness_clients WHERE client_id = ?`, clientID).
		Scan(&c.ID, &c.ClientID, &c.Name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return harnessClient{}, false, nil
	}
	if err != nil {
		return harnessClient{}, false, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return harnessClient{}, false, err
	}
	return c, true, nil
}

// redirectMatches is exact match, except that a loopback redirect may use
// any port (RFC 8252 §7.3): a CLI picks a free one per login.
func (c harnessClient) redirectMatches(uri string) bool {
	if slices.Contains(c.RedirectURIs, uri) {
		return true
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" {
		return false
	}
	for _, registered := range c.RedirectURIs {
		ru, err := url.Parse(registered)
		if err == nil && ru.Scheme == "http" && ru.Hostname() == u.Hostname() && ru.Path == u.Path &&
			ru.RawQuery == u.RawQuery && validRedirectURI(uri) {
			return true
		}
	}
	return false
}

// ---- authorize and consent

type pendingConsent struct {
	Issuer      string // the origin the authorize request came in on
	Client      harnessClient
	RedirectURI string
	State       string
	Challenge   string
	Group       groupRow
	Expires     time.Time
}

type issuedCode struct {
	ClientID    int64
	RedirectURI string
	Challenge   string
	GroupID     int64
	Expires     time.Time
}

// harnessFlows holds consents waiting for a click and codes waiting for
// their exchange, each single use.
type harnessFlows struct {
	mu       sync.Mutex
	consents map[string]pendingConsent
	codes    map[string]issuedCode
}

func newHarnessFlows() *harnessFlows {
	return &harnessFlows{consents: map[string]pendingConsent{}, codes: map[string]issuedCode{}}
}

func (f *harnessFlows) putConsent(id string, c pendingConsent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for k, v := range f.consents {
		if now.After(v.Expires) {
			delete(f.consents, k)
		}
	}
	f.consents[id] = c
}

func (f *harnessFlows) consent(id string, take bool) (pendingConsent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.consents[id]
	if take {
		delete(f.consents, id)
	}
	if !ok || time.Now().After(c.Expires) {
		return pendingConsent{}, false
	}
	return c, true
}

func (f *harnessFlows) putCode(code string, c issuedCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for k, v := range f.codes {
		if now.After(v.Expires) {
			delete(f.codes, k)
		}
	}
	f.codes[code] = c
}

func (f *harnessFlows) takeCode(code string) (issuedCode, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[code]
	delete(f.codes, code)
	if !ok || time.Now().After(c.Expires) {
		return issuedCode{}, false
	}
	return c, true
}

// handleHarnessAuthorize checks an authorization request and sends the
// browser to its consent page. Until client_id and redirect_uri check out,
// errors are a page (redirecting to an unchecked URI would make Bangle an
// open redirector); after, they go back to the client (RFC 6749 §4.1.2.1).
func (s *Server) handleHarnessAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTMLPage(w, http.StatusMethodNotAllowed, "Method not allowed", "Authorization requests are GETs.")
		return
	}
	q := r.URL.Query()
	client, ok, err := s.harnessClientByClientID(r.Context(), q.Get("client_id"))
	if err != nil {
		writeHTMLPage(w, http.StatusInternalServerError, "Authorization failed", err.Error())
		return
	}
	if !ok {
		writeHTMLPage(w, http.StatusBadRequest, "Authorization failed", "The client is not registered with Bangle. Ask it to log in again.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if !client.redirectMatches(redirectURI) {
		writeHTMLPage(w, http.StatusBadRequest, "Authorization failed", "The redirect URI is not one the client registered.")
		return
	}
	origin := s.originFor(r)
	fail := func(code, desc string) {
		http.Redirect(w, r, withQuery(redirectURI, url.Values{
			"error": {code}, "error_description": {desc}, "state": {q.Get("state")}, "iss": {origin},
		}), http.StatusSeeOther)
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with S256 is required")
		return
	}
	group, ok, err := s.groupForResource(r.Context(), origin, q.Get("resource"))
	if err != nil {
		writeHTMLPage(w, http.StatusInternalServerError, "Authorization failed", err.Error())
		return
	}
	if !ok {
		fail("invalid_target", "resource must be a Bangle group endpoint, "+endpointAt(origin, "<group>"))
		return
	}
	id, err := randomToken(18)
	if err != nil {
		writeHTMLPage(w, http.StatusInternalServerError, "Authorization failed", err.Error())
		return
	}
	s.harness.putConsent(id, pendingConsent{
		Issuer:      origin,
		Client:      client,
		RedirectURI: redirectURI,
		State:       q.Get("state"),
		Challenge:   q.Get("code_challenge"),
		Group:       group,
		Expires:     time.Now().Add(harnessConsentTTL),
	})
	http.Redirect(w, r, "/consent/"+id, http.StatusSeeOther)
}

// groupForResource maps an RFC 8707 resource, at origin, to its group.
func (s *Server) groupForResource(ctx context.Context, origin, resource string) (groupRow, bool, error) {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(resource, "/"), origin+"/g/")
	if !ok {
		return groupRow{}, false, nil
	}
	name, ok := strings.CutSuffix(rest, "/mcp")
	if !ok {
		return groupRow{}, false, nil
	}
	return s.groupByName(ctx, name)
}

func withQuery(base string, q url.Values) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	merged := u.Query()
	for k, v := range q {
		merged[k] = v
	}
	u.RawQuery = merged.Encode()
	return u.String()
}

// consentJSON answers GET /api/consent/{id}: what the consent page shows.
func (s *Server) consentJSON(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	c, ok := s.harness.consent(id, false)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "this login is unknown or expired: start it again from the harness")
		return
	}
	redirect, err := url.Parse(c.RedirectURI)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"client_name":   c.Client.Name,
		"redirect_host": redirect.Scheme + "://" + redirect.Host,
		"group":         c.Group.Name,
		"endpoint":      endpointAt(c.Issuer, c.Group.Name),
	})
}

// handleHarnessConsent is the consent page's Approve or Deny form post.
func (s *Server) handleHarnessConsent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTMLPage(w, http.StatusMethodNotAllowed, "Method not allowed", "Consent is a POST from the consent page.")
		return
	}
	c, ok := s.harness.consent(r.PostFormValue("id"), true)
	if !ok {
		writeHTMLPage(w, http.StatusBadRequest, "Authorization failed", "This login is unknown or expired. Start it again from the harness.")
		return
	}
	if r.PostFormValue("decision") != "approve" {
		http.Redirect(w, r, withQuery(c.RedirectURI, url.Values{
			"error": {"access_denied"}, "state": {c.State}, "iss": {c.Issuer},
		}), http.StatusSeeOther)
		return
	}
	code, err := randomToken(32)
	if err != nil {
		writeHTMLPage(w, http.StatusInternalServerError, "Authorization failed", err.Error())
		return
	}
	s.harness.putCode(code, issuedCode{
		ClientID:    c.Client.ID,
		RedirectURI: c.RedirectURI,
		Challenge:   c.Challenge,
		GroupID:     c.Group.ID,
		Expires:     time.Now().Add(harnessCodeTTL),
	})
	log.Printf("harness %q approved for group %q", c.Client.Name, c.Group.Name)
	http.Redirect(w, r, withQuery(c.RedirectURI, url.Values{
		"code": {code}, "state": {c.State}, "iss": {c.Issuer},
	}), http.StatusSeeOther)
}

// ---- tokens

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (s *Server) handleHarnessToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	f := r.PostForm
	client, ok, err := s.harnessClientByClientID(r.Context(), f.Get("client_id"))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if !ok {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		code, ok := s.harness.takeCode(f.Get("code"))
		if !ok || code.ClientID != client.ID || code.RedirectURI != f.Get("redirect_uri") ||
			subtle.ConstantTimeCompare([]byte(pkceChallenge(f.Get("code_verifier"))), []byte(code.Challenge)) != 1 {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, expired, used, or does not match this request")
			return
		}
		if res := f.Get("resource"); res != "" {
			g, ok, err := s.groupForResource(r.Context(), s.originFor(r), res)
			if err != nil {
				oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
				return
			}
			if !ok || g.ID != code.GroupID {
				oauthError(w, http.StatusBadRequest, "invalid_target", "resource is not the group this code was granted for")
				return
			}
		}
		s.issueTokens(w, r, 0, client.ID, code.GroupID)
	case "refresh_token":
		var grantID, clientID, groupID int64
		err := s.db.QueryRowContext(r.Context(),
			`SELECT id, client_id, group_id FROM harness_grants WHERE refresh_hash = ?`, hashToken(f.Get("refresh_token"))).
			Scan(&grantID, &clientID, &groupID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && clientID != client.ID) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown, rotated or revoked")
			return
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		s.issueTokens(w, r, grantID, client.ID, groupID)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code and refresh_token are supported")
	}
}

// issueTokens writes fresh tokens: a new grant (grantID 0), or a refresh
// that replaces both of a grant's tokens.
func (s *Server) issueTokens(w http.ResponseWriter, r *http.Request, grantID, clientID, groupID int64) {
	access, err := randomToken(32)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	refresh, err := randomToken(32)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	access, refresh = "bangle_at_"+access, "bangle_rt_"+refresh
	expires := time.Now().Add(harnessAccessTTL).UnixMilli()
	if grantID == 0 {
		_, err = s.db.ExecContext(r.Context(), `
			INSERT INTO harness_grants (client_id, group_id, access_hash, access_expires_at, refresh_hash)
			VALUES (?, ?, ?, ?, ?)`, clientID, groupID, hashToken(access), expires, hashToken(refresh))
	} else {
		_, err = s.db.ExecContext(r.Context(), `
			UPDATE harness_grants SET access_hash = ?, access_expires_at = ?, refresh_hash = ? WHERE id = ?`,
			hashToken(access), expires, hashToken(refresh), grantID)
	}
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(harnessAccessTTL.Seconds()),
		"refresh_token": refresh,
	})
}

// ---- the Clients page's API
//
//	GET    /api/clients       every grant: client, group, created, last used
//	DELETE /api/clients/{id}  revoke a grant

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" && r.Method == http.MethodGet {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT g.id, c.name, gr.name, g.created_at, g.last_used_at
			FROM harness_grants g JOIN harness_clients c ON c.id = g.client_id JOIN groups gr ON gr.id = g.group_id
			ORDER BY g.created_at DESC`)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		type clientJSON struct {
			ID         int64   `json:"id"`
			ClientName string  `json:"client_name"`
			Group      string  `json:"group"`
			CreatedAt  string  `json:"created_at"`
			LastUsedAt *string `json:"last_used_at"`
		}
		out := []clientJSON{}
		for rows.Next() {
			var c clientJSON
			var last sql.NullString
			if err := rows.Scan(&c.ID, &c.ClientName, &c.Group, &c.CreatedAt, &last); err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if last.Valid {
				c.LastUsedAt = &last.String
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM harness_grants WHERE id = ?`, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		writeJSONError(w, http.StatusNotFound, "no such client grant")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}
