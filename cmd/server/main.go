package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"bangle/frontend"
	"bangle/internal/db"
	"bangle/internal/server"
	"bangle/migrations"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7060", "address to listen on: loopback, or all interfaces (0.0.0.0:7060) inside a container")
	flag.Parse()

	// The DB holds the upstream OAuth tokens: the keys to every account the
	// gateway fronts. Bangle has no logins, so the loopback listener is the
	// only gate. See request_guard.go for what protects it.
	//
	// The one exception is all interfaces (0.0.0.0, ::), asked for
	// explicitly. A container needs it: its loopback isn't the host's. It's
	// safe only when the host publishes the port on its own loopback
	// (docker run -p 127.0.0.1:7060:7060), so say that loudly. A specific
	// non-loopback address is never what a container needs, so it's refused.
	switch {
	case isLoopback(*listen):
	case isAllInterfaces(*listen):
		log.Printf("WARNING: listening on %s, every interface. Anything that can reach this port gets every login Bangle holds. "+
			"Only do this in a container whose port is published on the host's loopback: -p 127.0.0.1:7060:7060.", *listen)
	default:
		log.Fatalf("-listen must be a loopback address like 127.0.0.1:7060 (or 0.0.0.0:7060 inside a container), got %q", *listen)
	}

	// No default: where the tokens live is a decision, and a silent default
	// would scatter databases wherever the binary happened to start.
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		log.Fatal("DB_PATH environment variable is required (e.g. .data/bangle.db)")
	}

	// What the app is accessed as (https://7060.port.localhost locally,
	// through Caddy). It is the only non-loopback Host the request guard
	// accepts, and the origin every absolute URL is built from (OAuth
	// redirect URIs, resource metadata), never the request's Host. It has to
	// be a bare http(s) origin or those URLs break.
	baseURL := os.Getenv("BASE_URL")
	if baseURL == "" {
		log.Fatal("BASE_URL environment variable is required (e.g. https://7060.port.localhost)")
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		log.Fatalf("BASE_URL must be a bare http(s) origin like https://7060.port.localhost, got %q", baseURL)
	}

	// Seals the upstream tokens in the DB
	// (internal/server/secretbox.go). Checked here so a missing or malformed
	// key stops the boot, not the first login.
	box, err := server.NewSecretBox(os.Getenv("SECRETS_KEY"))
	if err != nil {
		log.Fatal(err)
	}

	// The commit this build is from, for the UI's footer. `go build` stamps
	// it; `go run` only with -buildvcs=true, which scripts/serve.sh passes.
	// Without one the build is "dev".
	build := buildInfo()

	static, staticSource, err := frontend.FS()
	if err != nil {
		log.Fatalf("locating the frontend bundle: %v", err)
	}
	log.Printf("serving frontend from %s", staticSource)

	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer database.Close()

	if err := migrations.Run(database); err != nil {
		log.Fatalf("running migrations: %v", err)
	}

	srv := server.New(database, *listen, u, static, box, build)

	log.Printf("Bangle %s listening on http://%s, accessed as %s", build, *listen, baseURL)
	httpServer := &http.Server{
		Addr:    *listen,
		Handler: srv,
		// A client must finish sending headers and body promptly. There is
		// deliberately no WriteTimeout: MCP streamable HTTP holds a response
		// open for server-sent events.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// On SIGTERM, SIGINT or SIGHUP (scripts/serve.sh killing its tmux
	// window to restart it), stop
	// taking requests, then close upstream sessions, which ends stdio
	// servers' processes instead of orphaning them.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	served := make(chan error, 1)
	go func() { served <- httpServer.ListenAndServe() }()
	select {
	case err := <-served:
		srv.Close()
		log.Fatalf("server failed: %v", err)
	case sig := <-stop:
		log.Printf("%v: shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		srv.Close()
	}
}

// isLoopback reports whether addr (host:port) is reachable only from this
// machine. An empty host (":7060") is every interface, so it is not.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isAllInterfaces reports whether addr (host:port) is every interface:
// an empty host, 0.0.0.0 or ::.
func isAllInterfaces(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// buildInfo reads the git commit the binary was built from, if it was
// stamped with one.
func buildInfo() server.Build {
	var b server.Build
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b
}
