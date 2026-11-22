package server

import (
	"database/sql"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxRequestBody caps every request body. The API takes small JSON forms;
// nothing legitimate comes near this. Gateway requests carry tool arguments,
// which can be whole files, so they get maxGatewayBody.
const (
	maxRequestBody = 64 << 10
	maxGatewayBody = 32 << 20
)

type Server struct {
	db         *sql.DB
	listenAddr string
	// baseURL is BASE_URL without a trailing slash, the origin every
	// absolute URL is built from (the upstream redirect URI). baseHost is its
	// host (e.g. 7060.port.localhost), the name the app is reached by through
	// Caddy. See request_guard.go.
	baseURL  string
	baseHost string
	static   http.FileSystem
	box      *SecretBox
	build    Build
	// context7Key is CONTEXT7_API_KEY, for looking up servers' URLs
	// (lookup.go), empty when that's off; context7URL is Context7's search endpoint, replaced in
	// tests.
	context7Key string
	context7URL string
	// upstream carries all traffic to upstream servers and their
	// authorization servers. It has no Timeout: MCP streams stay open, so
	// each call bounds itself with a context.
	upstream *http.Client
	flows    *pendingFlows
	pool     *upstreamPool
	gateway  *gateway
	harness  *harnessFlows
	// auditPrune paces deleting old audit rows (audit.go).
	auditPrune auditPruner
	// refreshLocks serializes refresh per server (upstream_token.go).
	refreshMu    sync.Mutex
	refreshLocks map[int64]*sync.Mutex
}

// Build is the git commit the running binary was built from ("" for a build
// without one, shown as "dev"), and whether the working tree had uncommitted
// changes. /api/health reports it for the UI's footer.
type Build struct {
	Commit   string `json:"commit"`
	Modified bool   `json:"modified"`
}

func (b Build) String() string {
	if b.Commit == "" {
		return "dev"
	}
	short := b.Commit[:min(7, len(b.Commit))]
	if b.Modified {
		return short + " (modified)"
	}
	return short
}

// New takes BASE_URL (a bare origin), the static filesystem from frontend.FS,
// the SECRETS_KEY box and CONTEXT7_API_KEY; cmd/server validates them first.
func New(database *sql.DB, listenAddr string, baseURL *url.URL, static http.FileSystem, box *SecretBox, context7Key string, build Build) *Server {
	return &Server{
		db:           database,
		listenAddr:   listenAddr,
		baseURL:      baseURL.Scheme + "://" + baseURL.Host,
		baseHost:     baseURL.Host,
		static:       static,
		box:          box,
		build:        build,
		context7Key:  context7Key,
		context7URL:  context7SearchURL,
		upstream:     &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		flows:        &pendingFlows{flows: map[string]pendingFlow{}},
		pool:         newUpstreamPool(),
		gateway:      newGateway(),
		harness:      newHarnessFlows(),
		refreshLocks: map[int64]*sync.Mutex{},
	}
}

// Close ends every upstream session.
func (s *Server) Close() {
	s.pool.closeAll()
}

// hashedAsset matches build.mjs's content-hashed bundle names. Only those are
// cached for good; anything else under /static/ is revalidated.
var hashedAsset = regexp.MustCompile(`^/static/(main|styles)-[A-Za-z0-9]{8}\.(js|css|js\.map)$`)

// ServeHTTP is the only middleware layer: the request guard, the body cap,
// then one routing switch.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.allowRequest(w, r) {
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// No page of Bangle's may be framed: the consent page's Approve button
	// must not be clickable through someone else's page.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	limit := int64(maxRequestBody)
	// A tool call's arguments can be files, from an agent or from a tool's
	// page in the UI alike.
	if strings.HasPrefix(r.URL.Path, "/g/") ||
		(strings.HasPrefix(r.URL.Path, "/api/servers/") && strings.HasSuffix(r.URL.Path, "/call")) {
		limit = maxGatewayBody
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	switch {
	case r.URL.Path == "/api/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "build": s.build, "lookup": s.context7Key != ""})
	case r.URL.Path == "/api/servers":
		s.handleServers(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/servers/"):
		s.handleServer(w, r, strings.TrimPrefix(r.URL.Path, "/api/servers/"))
	case r.URL.Path == "/api/lookup":
		s.handleLookup(w, r)
	case r.URL.Path == "/api/groups":
		s.handleGroups(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/groups/"):
		s.handleGroup(w, r, strings.TrimPrefix(r.URL.Path, "/api/groups/"))
	case r.URL.Path == "/api/clients" || strings.HasPrefix(r.URL.Path, "/api/clients/"):
		s.handleClients(w, r, strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/clients"), "/"))
	case r.URL.Path == "/api/calls" || strings.HasPrefix(r.URL.Path, "/api/calls/"):
		s.handleCalls(w, r, strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/calls"), "/"))
	case strings.HasPrefix(r.URL.Path, "/api/consent/"):
		s.consentJSON(w, r, strings.TrimPrefix(r.URL.Path, "/api/consent/"))
	case strings.HasPrefix(r.URL.Path, "/g/"):
		s.serveGateway(w, r)
	case strings.HasPrefix(r.URL.Path, protectedResourcePrefix+"/"):
		s.serveProtectedResource(w, r)
	case r.URL.Path == authServerMetadataPath:
		s.serveAuthServerMetadata(w, r)
	case r.URL.Path == harnessRegisterPath:
		s.handleHarnessRegister(w, r)
	case r.URL.Path == harnessAuthorizePath:
		s.handleHarnessAuthorize(w, r)
	case r.URL.Path == harnessConsentPath:
		s.handleHarnessConsent(w, r)
	case r.URL.Path == harnessTokenPath:
		s.handleHarnessToken(w, r)
	case r.URL.Path == upstreamStartPath:
		s.handleUpstreamStart(w, r)
	case r.URL.Path == upstreamCallbackPath:
		s.handleUpstreamCallback(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		writeJSONError(w, http.StatusNotFound, "not found")
	case strings.HasPrefix(r.URL.Path, "/static/"):
		if hashedAsset.MatchString(r.URL.Path) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.StripPrefix("/static/", http.FileServer(s.static)).ServeHTTP(w, r)
	default:
		s.serveIndex(w, r)
	}
}

// serveIndex serves the SPA shell for every non-API, non-static path, so a
// reload of any client-side route lands on the app.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := s.static.Open("index.html")
	if err != nil {
		http.Error(w, "frontend not built: run make build-frontend", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, f)
}
