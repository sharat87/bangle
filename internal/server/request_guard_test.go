package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The request guard is the only thing between a browser tab and the upstream
// tokens, so its two rules are pinned here.
func TestRequestGuard(t *testing.T) {
	s := &Server{listenAddr: "127.0.0.1:7060", baseHost: "7060.port.localhost", static: http.Dir(t.TempDir())}

	cases := []struct {
		name    string
		host    string
		method  string
		headers map[string]string
		want    int
	}{
		{"BASE_URL host (via Caddy)", "7060.port.localhost", "GET", nil, http.StatusOK},
		{"other port.localhost name", "7050.port.localhost", "GET", nil, http.StatusMisdirectedRequest},
		{"loopback ip", "127.0.0.1:7060", "GET", nil, http.StatusOK},
		{"localhost", "localhost:7060", "GET", nil, http.StatusOK},
		{"ipv6 loopback", "[::1]:7060", "GET", nil, http.StatusOK},
		{"dns rebinding host", "evil.example:7060", "GET", nil, http.StatusMisdirectedRequest},
		{"wrong port", "127.0.0.1:9999", "GET", nil, http.StatusMisdirectedRequest},
		{"no port", "127.0.0.1", "GET", nil, http.StatusMisdirectedRequest},
		{"cross-site write", "127.0.0.1:7060", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-origin write", "127.0.0.1:7060", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusNotFound},
		{"headerless write (curl, MCP clients)", "127.0.0.1:7060", "POST", nil, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := "/api/health"
			if c.method == "POST" {
				// No POST routes exist yet; an unknown API path proves the
				// request got past the guard (404) rather than stopped at it.
				path = "/api/nothing"
			}
			r := httptest.NewRequest(c.method, path, nil)
			r.Host = c.host
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("got %d, want %d (body %q)", w.Code, c.want, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("API response Content-Type = %q, want application/json", ct)
			}
		})
	}
}
