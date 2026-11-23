package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Servers are upstream MCP servers, remote ones only: streamable HTTP with
// an OAuth login. Local (stdio) servers were supported until 2026-11-22 and
// removed for want of any in use. The API is
// resource-shaped by name, which is also the tool prefix and the UI's URL:
//
//	GET    /api/servers               list, with each one's login status
//	POST   /api/servers               add: {name, url, scopes}
//	GET    /api/servers/{name}        one
//	PATCH  /api/servers/{name}        rename {name}; the login is kept
//	DELETE /api/servers/{name}        delete, with its login
//	GET    /api/servers/{name}/tools  the server's live tools/list
//	DELETE /api/servers/{name}/login  disconnect: forget the login, keep the server
//	POST   /api/servers/{name}/call     call a tool, audited: {tool, arguments} (audit.go)
//	PUT    /api/servers/{name}/groups   set the groups it's in: {groups: [group names]} (groups.go)
//
// Logging in is not under /api/: it is a browser navigation, see
// upstream_oauth.go.

// validServerName mirrors the CHECK in migrations/001_init.sql. No
// underscores, so "<server>__<tool>" always splits at the first "__".
var validServerName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

type serverRow struct {
	ID        int64
	Name      string
	URL       string
	Scopes    string // space-separated; "" means the resource's scopes_supported
	AuthError string
}

const serverColumns = `id, name, url, scopes, auth_error`

func scanServer(row interface{ Scan(...any) error }) (serverRow, error) {
	var s serverRow
	err := row.Scan(&s.ID, &s.Name, &s.URL, &s.Scopes, &s.AuthError)
	return s, err
}

// serverByName returns the server, or ok=false when there is none.
func (s *Server) serverByName(ctx context.Context, name string) (serverRow, bool, error) {
	row, err := scanServer(s.db.QueryRowContext(ctx, `SELECT `+serverColumns+` FROM servers WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return serverRow{}, false, nil
	}
	return row, err == nil, err
}

func (s *Server) serverByID(ctx context.Context, id int64) (serverRow, bool, error) {
	row, err := scanServer(s.db.QueryRowContext(ctx, `SELECT `+serverColumns+` FROM servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return serverRow{}, false, nil
	}
	return row, err == nil, err
}

// serverJSON is a server as the API shows it. Tokens never appear.
type serverJSON struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Scopes string `json:"scopes"`
	// Status is "connected", "needs_login" or "error" (see serverStatus).
	Status string `json:"status"`
	Error  string `json:"error"`
	// LoggedIn is whether a login is stored. It is not the same as status
	// "connected": a failed reconnect leaves an old login in place under
	// status "error". The UI offers Reconnect and Disconnect only when true.
	LoggedIn bool `json:"logged_in"`
	// The login, when there is one: when the access token expires (null when
	// the AS gave no expiry) and whether it can be refreshed.
	TokenExpiresAt *time.Time `json:"token_expires_at"`
	Refreshable    bool       `json:"refreshable"`
}

// serverStatus is the one rule for a server's status, so the list and the
// detail page cannot disagree:
//   - error: the last login or refresh failed (auth_error), whatever else
//     is stored. Connecting again clears it.
//   - connected: there is a stored login.
//   - needs_login: neither.
func serverStatus(authError string, hasToken bool) string {
	switch {
	case authError != "":
		return "error"
	case hasToken:
		return "connected"
	default:
		return "needs_login"
	}
}

func (s *Server) serverJSON(ctx context.Context, row serverRow) (serverJSON, error) {
	out := serverJSON{
		Name:   row.Name,
		URL:    row.URL,
		Scopes: row.Scopes,
		Error:  row.AuthError,
	}
	tok, ok, err := s.loadToken(ctx, row.ID)
	if err != nil {
		return serverJSON{}, fmt.Errorf("server %q: %w", row.Name, err)
	}
	out.LoggedIn = ok
	if ok {
		if tok.ExpiresAt != 0 {
			t := time.UnixMilli(tok.ExpiresAt).UTC()
			out.TokenExpiresAt = &t
		}
		out.Refreshable = tok.RefreshToken != ""
	}
	out.Status = serverStatus(row.AuthError, ok)
	return out, nil
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listServers(w, r)
	case http.MethodPost:
		s.createServer(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleServer serves /api/servers/{name} and its /tools and /login.
func (s *Server) handleServer(w http.ResponseWriter, r *http.Request, rest string) {
	name, sub, _ := strings.Cut(rest, "/")
	row, ok, err := s.serverByName(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no server named %q", name))
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		out, err := s.serverJSON(r.Context(), row)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case sub == "" && r.Method == http.MethodPatch:
		s.renameServer(w, r, row)
	case sub == "" && r.Method == http.MethodDelete:
		if _, err := s.db.ExecContext(r.Context(), `DELETE FROM servers WHERE id = ?`, row.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.pool.close(row.ID)
		writeJSON(w, http.StatusOK, map[string]string{})
	case sub == "tools" && r.Method == http.MethodGet:
		s.serverTools(w, r, row)
	case sub == "login" && r.Method == http.MethodDelete:
		s.disconnectServer(w, r, row)
	case sub == "call" && r.Method == http.MethodPost:
		s.callFromUI(w, r, row)
	case sub == "groups" && r.Method == http.MethodPut:
		s.setServerGroups(w, r, row)
	case sub == "" || sub == "tools" || sub == "call" || sub == "login" || sub == "groups":
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+serverColumns+` FROM servers ORDER BY name`)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var servers []serverRow
	for rows.Next() {
		row, err := scanServer(rows)
		if err != nil {
			rows.Close()
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		servers = append(servers, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []serverJSON{}
	for _, row := range servers {
		j, err := s.serverJSON(r.Context(), row)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

type createServerRequest struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Scopes string `json:"scopes"`
}

func (s *Server) createServer(w http.ResponseWriter, r *http.Request) {
	var req createServerRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validServerName.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "name must be 1–64 of a-z, 0-9 and -")
		return
	}
	serverURL := strings.TrimSpace(req.URL)
	if err := checkUpstreamURL(serverURL); err != nil {
		writeJSONError(w, http.StatusBadRequest, "url: "+err.Error())
		return
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO servers (name, url, scopes) VALUES (?, ?, ?)`,
		name, serverURL, strings.Join(strings.Fields(req.Scopes), " "))
	if isUniqueViolation(err) {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("a server named %q already exists", name))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeCreatedServer(w, r, name)
}

func (s *Server) writeCreatedServer(w http.ResponseWriter, r *http.Request, name string) {
	row, _, err := s.serverByName(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := s.serverJSON(r.Context(), row)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// renameServer changes the name, and so every tool name, but not the row:
// the stored login is keyed by id and survives, so no new approval.
func (s *Server) renameServer(w http.ResponseWriter, r *http.Request, row serverRow) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validServerName.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "name must be 1–64 of a-z, 0-9 and -")
		return
	}
	_, err := s.db.ExecContext(r.Context(), `UPDATE servers SET name = ? WHERE id = ?`, name, row.ID)
	if isUniqueViolation(err) {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("a server named %q already exists", name))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	row.Name = name
	out, err := s.serverJSON(r.Context(), row)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// disconnectServer forgets the stored login and any auth_error, so the
// server is back to needs_login. The upstream grant is not revoked; the
// provider's own settings do that.
func (s *Server) disconnectServer(w http.ResponseWriter, r *http.Request, row serverRow) {
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM server_tokens WHERE server_id = ?`, row.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE servers SET auth_error = '' WHERE id = ?`, row.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.pool.close(row.ID)
	row.AuthError = ""
	out, err := s.serverJSON(r.Context(), row)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// checkUpstreamURL accepts https URLs, and http only to this machine (a
// local MCP server, or a test's httptest server). It guards every URL Bangle
// sends a token or the browser to: the configured server URL and every
// endpoint discovery returns.
func checkUpstreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL", raw)
	}
	if u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%q must be an absolute http(s) URL without credentials or fragment", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
		return fmt.Errorf("%q must be https (plain http only to localhost)", raw)
	default:
		return fmt.Errorf("%q must be an http(s) URL", raw)
	}
}
