package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// The stored upstream login, and the one refresh path.
//
// upstreamToken is the only way anything gets an access token, and the only
// place a token is refreshed. Per server it is serialized by a mutex, so
// concurrent callers refresh once: a waiter re-reads the row and finds the
// fresh token. It refreshes when the token is about to expire, and when
// upstream answered 401 to it (upstreamAuth), and it persists the new tokens,
// rotated refresh token included, before handing the access token out: a
// refresh token the AS rotated but Bangle failed to store would be gone for
// good, and the next refresh would fail with invalid_grant.

// tokenEarlyExpiry refreshes this long before the stated expiry, so a
// request in flight does not race it.
const tokenEarlyExpiry = time.Minute

// errNotLoggedIn is upstreamToken's answer for a server with no stored login.
var errNotLoggedIn = errors.New("not logged in: click Connect on the server's page")

type storedToken struct {
	TokenEndpoint string
	Client        oauthClient
	Resource      string
	Scope         string
	AccessToken   string
	RefreshToken  string // "" when the AS issued none
	ExpiresAt     int64  // unix ms; 0 = no stated expiry
}

func (t storedToken) expired(now time.Time) bool {
	return t.ExpiresAt != 0 && now.Add(tokenEarlyExpiry).UnixMilli() >= t.ExpiresAt
}

// loadToken reads and opens a server's login, ok=false when there is none.
func (s *Server) loadToken(ctx context.Context, serverID int64) (storedToken, bool, error) {
	var t storedToken
	var secret, access, refresh string
	var expires sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT token_endpoint, client_id, client_secret, token_auth_method, resource, scope,
		       access_token, refresh_token, expires_at
		FROM server_tokens WHERE server_id = ?`, serverID).
		Scan(&t.TokenEndpoint, &t.Client.ID, &secret, &t.Client.AuthMethod, &t.Resource, &t.Scope,
			&access, &refresh, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return storedToken{}, false, nil
	}
	if err != nil {
		return storedToken{}, false, err
	}
	for _, f := range []struct {
		sealed string
		dst    *string
	}{{secret, &t.Client.Secret}, {access, &t.AccessToken}, {refresh, &t.RefreshToken}} {
		if *f.dst, err = s.box.open(f.sealed); err != nil {
			return storedToken{}, false, err
		}
	}
	t.ExpiresAt = expires.Int64
	return t, true, nil
}

// saveLogin stores a fresh login and clears the server's auth_error, in one
// transaction.
func (s *Server) saveLogin(ctx context.Context, serverID int64, t storedToken) error {
	sealed, err := s.sealAll(t.Client.Secret, t.AccessToken, t.RefreshToken)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO server_tokens (server_id, token_endpoint, client_id, client_secret, token_auth_method,
		                           resource, scope, access_token, refresh_token, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (server_id) DO UPDATE SET
		    token_endpoint = excluded.token_endpoint, client_id = excluded.client_id,
		    client_secret = excluded.client_secret, token_auth_method = excluded.token_auth_method,
		    resource = excluded.resource, scope = excluded.scope, access_token = excluded.access_token,
		    refresh_token = excluded.refresh_token, expires_at = excluded.expires_at`,
		serverID, t.TokenEndpoint, t.Client.ID, sealed[0], t.Client.AuthMethod,
		t.Resource, t.Scope, sealed[1], sealed[2], nullableMS(t.ExpiresAt)); err != nil {
		return fmt.Errorf("storing login: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE servers SET auth_error = '' WHERE id = ?`, serverID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// A session opened under an earlier login may be bound to that account.
	s.pool.close(serverID)
	return nil
}

func (s *Server) sealAll(plain ...string) ([]string, error) {
	out := make([]string, len(plain))
	for i, p := range plain {
		var err error
		if out[i], err = s.box.seal(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func nullableMS(ms int64) any {
	if ms == 0 {
		return nil
	}
	return ms
}

// refreshLock returns the server's refresh mutex.
func (s *Server) refreshLock(serverID int64) *sync.Mutex {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	mu, ok := s.refreshLocks[serverID]
	if !ok {
		mu = &sync.Mutex{}
		s.refreshLocks[serverID] = mu
	}
	return mu
}

// upstreamToken returns an access token for the server. rejected is the
// access token upstream just answered 401 to, or "" for an ordinary request:
// a rejected token is refreshed even if it has not expired, unless another
// caller already replaced it while this one waited for the lock.
func (s *Server) upstreamToken(ctx context.Context, serverID int64, rejected string) (string, error) {
	mu := s.refreshLock(serverID)
	mu.Lock()
	defer mu.Unlock()

	t, ok, err := s.loadToken(ctx, serverID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errNotLoggedIn
	}
	if rejected == "" && !t.expired(time.Now()) {
		return t.AccessToken, nil
	}
	if rejected != "" && t.AccessToken != rejected {
		return t.AccessToken, nil
	}
	return s.refreshLocked(ctx, serverID, t, rejected != "")
}

// refreshLocked refreshes t. The caller holds the server's refresh lock.
func (s *Server) refreshLocked(ctx context.Context, serverID int64, t storedToken, after401 bool) (string, error) {
	// Detached from the caller: once the AS has rotated the refresh token, a
	// cancelled request must not stop Bangle from storing the new one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discoveryTimeout)
	defer cancel()

	why := "the access token expired"
	if after401 {
		why = "the server rejected the access token"
	}
	if t.RefreshToken == "" {
		return "", s.dropLogin(ctx, serverID, why+" and the server issued no refresh token: log in again")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", t.RefreshToken)
	form.Set("resource", t.Resource)
	resp, err := postTokenForm(ctx, s.upstream, t.TokenEndpoint, t.Client, form)
	if err != nil {
		// A 400 or 401 is the AS refusing the grant itself (invalid_grant:
		// revoked, expired, or already rotated): the login is dead. Anything
		// else (network, 5xx) is transient and leaves the login alone.
		var rejected *tokenRejectedError
		if errors.As(err, &rejected) && (rejected.Status == http.StatusBadRequest || rejected.Status == http.StatusUnauthorized) {
			return "", s.dropLogin(ctx, serverID, why+" and refreshing it failed: "+err.Error())
		}
		return "", fmt.Errorf("refreshing the access token: %w", err)
	}

	t.AccessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		t.RefreshToken = resp.RefreshToken // rotated
	}
	t.ExpiresAt = expiryMS(resp.ExpiresIn)
	if resp.Scope != "" {
		t.Scope = resp.Scope
	}
	sealed, err := s.sealAll(t.AccessToken, t.RefreshToken)
	if err != nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE server_tokens SET access_token = ?, refresh_token = ?, expires_at = ?, scope = ? WHERE server_id = ?`,
		sealed[0], sealed[1], nullableMS(t.ExpiresAt), t.Scope, serverID)
	if err != nil {
		return "", fmt.Errorf("storing refreshed tokens: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return "", fmt.Errorf("storing refreshed tokens: login row vanished (rows %d, %v)", n, err)
	}
	log.Printf("server %d: refreshed access token (%s)", serverID, why)
	return t.AccessToken, nil
}

// dropLogin deletes a dead login and records why, returning that reason as
// the error.
func (s *Server) dropLogin(ctx context.Context, serverID int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_tokens WHERE server_id = ?`, serverID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE servers SET auth_error = ? WHERE id = ?`, reason, serverID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("server %d: login dropped: %s", serverID, reason)
	return errors.New(reason)
}

// upstreamAuth adds the server's bearer token to every request, and on a 401
// refreshes it and retries once.
type upstreamAuth struct {
	s        *Server
	serverID int64
	base     http.RoundTripper
}

func (t *upstreamAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.s.upstreamToken(req.Context(), t.serverID, "")
	if err != nil {
		closeBody(req)
		return nil, err
	}
	resp, err := t.send(req, token)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// A body that cannot be replayed cannot be retried; the 401 stands.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return resp, nil
	}
	// The 401's body is discarded unread, so its Close error has nothing to
	// report.
	resp.Body.Close()

	token, err = t.s.upstreamToken(req.Context(), t.serverID, token)
	if err != nil {
		return nil, err
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, err
		}
	}
	return t.send(retry, token)
}

func (t *upstreamAuth) send(req *http.Request, token string) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(out)
}

// closeBody honours RoundTrip's contract to close the body even on error.
func closeBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}
