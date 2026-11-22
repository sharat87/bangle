package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

func TestLookupCandidates(t *testing.T) {
	got := lookupCandidates([]context7Snippet{
		{Source: "https://linear.app/docs/mcp", Text: "Use `https://mcp.linear.app/mcp`. Or https://mcp.linear.app/sse, or https://mcp.linear.app/mcp?readonly=true."},
		{Source: "https://linear.app/docs/mcp", Text: `{"url": "https://mcp.linear.app/mcp/"} and https://mcp.linear.app/mcp`},
		{Source: "https://github.com/someone/proxy", Text: "npx mcp-remote https://proxy.example.com/linear/mcp https://github.com/someone/proxy"},
		{Source: "https://github.com/someone/other", Text: "https://proxy.example.com/linear/mcp"},
		{Source: "https://linear.app/docs", Text: "See https://linear.app/changelog for news."},
	})
	urls := make([]string, len(got))
	for i, c := range got {
		urls[i] = c.URL
	}
	// Verified first even with fewer mentions; the trailing-slash spelling
	// is folded in; /sse, query strings and URLs without "mcp" are dropped.
	want := []string{"https://mcp.linear.app/mcp", "https://proxy.example.com/linear/mcp"}
	if !slices.Equal(urls, want) {
		t.Fatalf("candidates = %q, want %q", urls, want)
	}
	if !got[0].Verified || got[0].Mentions != 2 || !slices.Equal(got[0].Sources, []string{"https://linear.app/docs/mcp"}) {
		t.Errorf("linear candidate = %+v", got[0])
	}
	if got[1].Verified || got[1].Mentions != 2 {
		t.Errorf("proxy candidate = %+v", got[1])
	}
}

func TestSameSite(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"mcp.linear.app", "linear.app", true},
		{"mcp.posthog.com", "posthog.com", true},
		{"api.githubcopilot.com", "docs.github.com", false},
		{"mcp.example.co.uk", "docs.example.co.uk", true},
		{"mcp.other.co.uk", "docs.example.co.uk", false},
		{"", "linear.app", false},
	} {
		if got := sameSite(c.a, c.b); got != c.want {
			t.Errorf("sameSite(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestLookup runs a lookup against a fake Context7 whose snippets name a
// real (fake) OAuth MCP server, a docs page that isn't one, and a server
// that has nothing to do with the name: only the first comes back.
func TestLookup(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	var (
		mu      sync.Mutex
		queries []string
	)
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-context7-key" || r.URL.Query().Get("type") != "json" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		writeTestJSON(w, map[string]any{
			"codeSnippets": []any{map[string]any{
				"codeId":    srv.URL + "/acme/docs",
				"codeTitle": "Add the server",
				"codeList":  []any{map[string]any{"code": "claude mcp add --transport http acme " + srv.URL + "/mcp"}},
			}},
			"infoSnippets": []any{
				map[string]any{"pageId": srv.URL + "/acme/docs", "content": "Read " + srv.URL + "/docs/mcp for details."},
				// Not about acme: never checked.
				map[string]any{"pageId": srv.URL + "/spec", "content": "Try " + srv.URL + "/example/mcp."},
			},
		})
	})
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token"})
	})
	mux.HandleFunc("/docs/mcp", func(w http.ResponseWriter, r *http.Request) {})

	s := newTestServer(t)
	s.upstream = srv.Client()
	s.context7URL = srv.URL + "/search"

	res := decode[lookupResult](t, call(t, s, "GET", "/api/lookup?name=acme-corp", nil))
	if res.Checked != 2 || len(res.Candidates) != 1 || res.Candidates[0].URL != srv.URL+"/mcp" || !res.Candidates[0].Verified {
		t.Fatalf("lookup = %+v", res)
	}
	if len(queries) != len(lookupQueries) || !slices.Contains(queries, "acme corp remote MCP server URL") {
		t.Errorf("queries = %q", queries)
	}

	if w := call(t, s, "GET", "/api/lookup?name=", nil); w.Code != http.StatusBadRequest {
		t.Errorf("empty name: HTTP %d", w.Code)
	}
	s.context7Key = "wrong"
	if w := call(t, s, "GET", "/api/lookup?name=acme", nil); w.Code != http.StatusBadGateway {
		t.Errorf("Context7 refusing: HTTP %d", w.Code)
	}

	// CONTEXT7_API_KEY is optional: without it, health says so and the
	// lookup is refused without calling Context7.
	if h := decode[map[string]any](t, call(t, s, "GET", "/api/health", nil)); h["lookup"] != true {
		t.Errorf("health with a key: %v", h)
	}
	s.context7Key = ""
	asked := len(queries)
	if w := call(t, s, "GET", "/api/lookup?name=acme", nil); w.Code != http.StatusNotFound || len(queries) != asked {
		t.Errorf("no key: HTTP %d, %d queries sent", w.Code, len(queries)-asked)
	}
	if h := decode[map[string]any](t, call(t, s, "GET", "/api/health", nil)); h["lookup"] != false {
		t.Errorf("health without a key: %v", h)
	}
}
