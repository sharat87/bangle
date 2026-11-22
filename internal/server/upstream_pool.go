package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Talking MCP to upstream servers, through the go-sdk client.
//
// The pool holds one open session per server, opened on first use and shared
// by every group and page that needs that server, with upstreamAuth
// underneath. A session is dropped when an RPC on it fails at the transport
// level (the server forgot it) and reopened on the next use. It is closed when the
// server is deleted, disconnected or logged in again.
//
// A server's tool list is kept for upstreamToolsTTL, tied to the session it
// was listed on: any close or drop of that session (a new login,
// disconnect, a transport failure) makes it stale at once. Without it every
// gateway listing fetched PostHog's 4.8 MB list again, 4.5 s each time.
//
// Retries: a tools/list that fails at the transport level is retried once
// on a fresh session, since listing is harmless to repeat. A tools/call is
// retried only on ErrSessionMissing: the upstream answered 404 for the
// session, so the call never ran. Any other failure might have run it, and
// a tool must never run twice for one request.

const (
	upstreamConnectTimeout = 30 * time.Second
	upstreamListTimeout    = 30 * time.Second
	upstreamCallTimeout    = 10 * time.Minute
	upstreamToolsTTL       = 5 * time.Minute
)

type upstreamPool struct {
	mu       sync.Mutex
	sessions map[int64]*mcp.ClientSession
	// connecting serializes opening a server's session, so concurrent first
	// uses open one, not several.
	connecting map[int64]*sync.Mutex
	tools      map[int64]listedTools
}

// listedTools is a server's tool list, and the session it came from.
type listedTools struct {
	session *mcp.ClientSession
	at      time.Time
	tools   []*mcp.Tool
}

func newUpstreamPool() *upstreamPool {
	return &upstreamPool{sessions: map[int64]*mcp.ClientSession{}, connecting: map[int64]*sync.Mutex{}, tools: map[int64]listedTools{}}
}

// cachedTools is the server's tool list, if it was listed on the current
// session within upstreamToolsTTL. Callers must not modify it.
func (p *upstreamPool) cachedTools(serverID int64) ([]*mcp.Tool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.tools[serverID]
	if !ok || l.session != p.sessions[serverID] || time.Since(l.at) > upstreamToolsTTL {
		return nil, false
	}
	return l.tools, true
}

func (p *upstreamPool) putTools(serverID int64, session *mcp.ClientSession, tools []*mcp.Tool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools[serverID] = listedTools{session: session, at: time.Now(), tools: tools}
}

func (p *upstreamPool) connectLock(serverID int64) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	mu, ok := p.connecting[serverID]
	if !ok {
		mu = &sync.Mutex{}
		p.connecting[serverID] = mu
	}
	return mu
}

func (p *upstreamPool) get(serverID int64) *mcp.ClientSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[serverID]
}

func (p *upstreamPool) put(serverID int64, session *mcp.ClientSession) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[serverID] = session
}

// drop forgets and closes session, but only if it is still the server's
// current one: a concurrent caller may already have replaced it.
func (p *upstreamPool) drop(serverID int64, session *mcp.ClientSession) {
	p.mu.Lock()
	current := p.sessions[serverID] == session
	if current {
		delete(p.sessions, serverID)
	}
	p.mu.Unlock()
	if current {
		closeSession(serverID, session)
	}
}

// close forgets and closes whatever session the server has.
func (p *upstreamPool) close(serverID int64) {
	p.mu.Lock()
	session, ok := p.sessions[serverID]
	delete(p.sessions, serverID)
	p.mu.Unlock()
	if ok {
		closeSession(serverID, session)
	}
}

// closeAll closes every session, at shutdown.
func (p *upstreamPool) closeAll() {
	p.mu.Lock()
	sessions := p.sessions
	p.sessions = map[int64]*mcp.ClientSession{}
	p.mu.Unlock()
	for id, session := range sessions {
		closeSession(id, session)
	}
}

// closeSession closes a session that is already out of the pool. Close sends
// a DELETE or waits for a process, so it never runs under the pool's lock.
// Its error is only logged: the session is gone either way.
func closeSession(serverID int64, session *mcp.ClientSession) {
	if err := session.Close(); err != nil && !errors.Is(err, mcp.ErrConnectionClosed) {
		log.Printf("server %d: closing upstream session: %v", serverID, err)
	}
}

// upstreamSession returns the server's open session, opening one if needed.
func (s *Server) upstreamSession(ctx context.Context, row serverRow) (*mcp.ClientSession, error) {
	lock := s.pool.connectLock(row.ID)
	lock.Lock()
	defer lock.Unlock()
	if session := s.pool.get(row.ID); session != nil {
		return session, nil
	}
	// Fail before connecting, so "not logged in" is not buried in a
	// transport error.
	if _, err := s.upstreamToken(ctx, row.ID, ""); err != nil {
		return nil, err
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:   row.URL,
		HTTPClient: &http.Client{Transport: &upstreamAuth{s: s, serverID: row.ID, base: s.upstream.Transport}},
		// Bangle only makes requests; it has nothing to receive unprompted.
		DisableStandaloneSSE: true,
	}
	// The session outlives the request that opened it, so the handshake has
	// its own deadline rather than the request's cancellation.
	connectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamConnectTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "bangle", Version: "0.1.0"}, nil)
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", row.Name, err)
	}
	s.pool.put(row.ID, session)
	return session, nil
}

// onUpstream runs fn on the server's session. retry says whether a
// transport-level failure may be retried once on a fresh session.
func (s *Server) onUpstream(ctx context.Context, row serverRow, retry func(error) bool, fn func(*mcp.ClientSession) error) error {
	for attempt := 0; ; attempt++ {
		session, err := s.upstreamSession(ctx, row)
		if err != nil {
			return err
		}
		err = fn(session)
		if err == nil {
			return nil
		}
		// A JSON-RPC error is the server answering: the session is fine.
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			return err
		}
		s.pool.drop(row.ID, session)
		if attempt > 0 || !retry(err) || ctx.Err() != nil {
			return err
		}
		log.Printf("server %q: session failed (%v); retrying on a new one", row.Name, err)
	}
}

// listUpstreamTools is the server's tools/list, every page of it, at most
// upstreamToolsTTL old (see the pool).
func (s *Server) listUpstreamTools(ctx context.Context, row serverRow) ([]*mcp.Tool, error) {
	if tools, ok := s.pool.cachedTools(row.ID); ok {
		return tools, nil
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamListTimeout)
	defer cancel()
	var tools []*mcp.Tool
	err := s.onUpstream(ctx, row, func(error) bool { return true }, func(session *mcp.ClientSession) error {
		tools = nil
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				return err
			}
			tools = append(tools, tool)
		}
		s.pool.putTools(row.ID, session, tools)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s's tools: %w", row.Name, err)
	}
	return tools, nil
}

// callUpstreamTool forwards one tools/call and returns the result unchanged.
func (s *Server) callUpstreamTool(ctx context.Context, row serverRow, params *mcp.CallToolParamsRaw) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamCallTimeout)
	defer cancel()
	var result *mcp.CallToolResult
	sessionMissing := func(err error) bool { return errors.Is(err, mcp.ErrSessionMissing) }
	err := s.onUpstream(ctx, row, sessionMissing, func(session *mcp.ClientSession) error {
		var err error
		result, err = session.CallTool(ctx, &mcp.CallToolParams{
			Meta:      params.Meta,
			Name:      params.Name,
			Arguments: rawJSON(params.Arguments), // absent stays absent, not null
		})
		return err
	})
	return result, err
}

// toolJSON is a tool as the UI shows it. InputSchema is the upstream's JSON
// Schema as it sent it, for the tool's signature.
type toolJSON struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

func toolOut(t *mcp.Tool) toolJSON {
	return toolJSON{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: t.InputSchema}
}

// serverTools answers GET /api/servers/{name}/tools with the server's live
// tools/list.
func (s *Server) serverTools(w http.ResponseWriter, r *http.Request, row serverRow) {
	tools, err := s.listUpstreamTools(r.Context(), row)
	if errors.Is(err, errNotLoggedIn) {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("%s is not connected", row.Name))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := []toolJSON{}
	for _, t := range tools {
		out = append(out, toolOut(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// rawJSON forwards tool arguments, dropping absent or null ones: MCP's
// arguments is an optional object, and servers refuse null (Linear: "expected
// record, received null").
func rawJSON(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}
