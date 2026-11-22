package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The gateway: /g/<group>/mcp is a streamable-HTTP MCP server per group.
//
// Each group has ONE cached mcp.Server and handler, kept for the process's
// life (keyed by group id, so a rename keeps live sessions). A fresh server
// per request was MCPJungle #268: the client's follow-up requests landed on
// a server that had never seen its session, and the stream-opening GET got
// 400.
//
// The server registers no tools. A receiving middleware answers tools/list
// and tools/call itself: tools/list is every member's live tool, renamed
// <server>__<tool>, and a tools/call of one is split at the first "__"
// (server names have no underscores), checked for membership, forwarded,
// and its result returned unchanged. A member that fails (not logged in,
// unreachable) is left out of tools/list, and the group's page names it, so
// one broken server doesn't take the group down.
//
// Lists are read through the pool's 5-minute cache on every request, never
// a snapshot taken at registration (MCPJungle #260), so membership changes
// need no rebuild. A harness keeps the list it read when it connected;
// Bangle sends no list_changed, so it sees a new tool on reconnect.
//
// One group's members advertise over a thousand tools, 4.8 MB of schemas.
// That's for a harness that defers tools itself (Claude Code's tool
// search); one that doesn't gets every schema in context. A search mode
// (three tools: search, describe, execute) was built for that and removed
// on 2026-11-22.
//
// Every request needs a bearer token granted for this group
// (harness_oauth.go).

const toolSeparator = "__"

// gatewayGrantKey is where serveGateway puts the request's gatewayGrant in
// the token info's Extra.
const gatewayGrantKey = "bangle.grant"

type gateway struct {
	mu       sync.Mutex
	handlers map[int64]http.Handler
}

func newGateway() *gateway {
	return &gateway{handlers: map[int64]http.Handler{}}
}

// forget drops a deleted group's server.
func (g *gateway) forget(groupID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.handlers, groupID)
}

func (s *Server) gatewayHandler(groupID int64) http.Handler {
	s.gateway.mu.Lock()
	defer s.gateway.mu.Unlock()
	if h, ok := s.gateway.handlers[groupID]; ok {
		return h
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "bangle", Version: "0.1.0"}, &mcp.ServerOptions{
		// Tools are served by the middleware, not registered, so the
		// capability is declared rather than inferred.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	server.AddReceivingMiddleware(s.gatewayMiddleware(groupID))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		// The SDK's own DNS-rebinding check refuses a non-loopback Host on a
		// loopback connection, which is every request through Caddy
		// (7060.port.localhost). request_guard.go already makes that check,
		// allowing BASE_URL's host.
		DisableLocalhostProtection: true,
	})
	s.gateway.handlers[groupID] = h
	return h
}

// serveGateway serves /g/<group>/mcp.
func (s *Server) serveGateway(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/g/")
	name, tail, _ := strings.Cut(rest, "/")
	if tail != "mcp" {
		writeJSONError(w, http.StatusNotFound, "not found: group endpoints are /g/<group>/mcp")
		return
	}
	g, ok, err := s.groupByName(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no group named %q", name))
		return
	}
	grant, ok := s.authorizeGateway(w, r, g)
	if !ok {
		return
	}
	// The go-sdk's RequireBearerToken is the only way to put token info in
	// the request context, where the middleware reads it as
	// req.Extra.TokenInfo (for the audit). Bangle has already checked the
	// token, so its verifier just returns the grant. UserID also makes the
	// SDK bind each MCP session to the grant that opened it: another grant's
	// token can't use that session (403 "session user mismatch").
	info := &auth.TokenInfo{
		UserID:     fmt.Sprintf("grant:%d", grant.ID),
		Expiration: grant.Expires,
		Extra:      map[string]any{gatewayGrantKey: grant},
	}
	verified := func(context.Context, string, *http.Request) (*auth.TokenInfo, error) { return info, nil }
	auth.RequireBearerToken(verified, nil)(s.gatewayHandler(g.ID)).ServeHTTP(w, r)
}

func (s *Server) gatewayMiddleware(groupID int64) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/list":
				tools, _, err := s.groupTools(ctx, groupID)
				if err != nil {
					return nil, err
				}
				return &mcp.ListToolsResult{Tools: tools}, nil
			case "tools/call":
				call := req.(*mcp.CallToolRequest)
				grant, ok := grantOf(call.Extra)
				if !ok {
					return nil, errors.New("bangle: a tools/call with no grant attached")
				}
				return s.gatewayCall(ctx, groupID, grant, call.Params)
			}
			return next(ctx, method, req)
		}
	}
}

// toolError is a tool result the model reads as an error: Bangle's
// mistakes and refusals are said this way, not as protocol errors, which a
// harness may not show the model.
func toolError(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

type memberFailure struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

// groupTools is the group's aggregated live tool list, and the members left
// out of it.
func (s *Server) groupTools(ctx context.Context, groupID int64) ([]*mcp.Tool, []memberFailure, error) {
	members, err := s.groupMembers(ctx, groupID)
	if err != nil {
		return nil, nil, err
	}
	lists := make([][]*mcp.Tool, len(members))
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Go(func() { lists[i], errs[i] = s.listUpstreamTools(ctx, m) })
	}
	wg.Wait()
	tools := []*mcp.Tool{}
	failures := []memberFailure{}
	for i, m := range members {
		if errs[i] != nil {
			failures = append(failures, memberFailure{Server: m.Name, Error: errs[i].Error()})
			continue
		}
		for _, t := range lists[i] {
			renamed := *t
			renamed.Name = m.Name + toolSeparator + t.Name
			tools = append(tools, &renamed)
		}
	}
	return tools, failures, nil
}

// grantOf reads the gatewayGrant serveGateway attached to a request.
func grantOf(extra *mcp.RequestExtra) (gatewayGrant, bool) {
	if extra == nil || extra.TokenInfo == nil {
		return gatewayGrant{}, false
	}
	grant, ok := extra.TokenInfo.Extra[gatewayGrantKey].(gatewayGrant)
	return grant, ok
}

// gatewayCall routes one call to its server, audited (audit.go). params.Name
// is <server>__<tool>.
func (s *Server) gatewayCall(ctx context.Context, groupID int64, grant gatewayGrant, params *mcp.CallToolParamsRaw) (mcp.Result, error) {
	serverName, tool, ok := strings.Cut(params.Name, toolSeparator)
	if !ok || tool == "" {
		return toolError(fmt.Sprintf("unknown tool %q: tools are named <server>__<tool>", params.Name)), nil
	}
	members, err := s.groupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	var row *serverRow
	for i := range members {
		if members[i].Name == serverName {
			row = &members[i]
		}
	}
	if row == nil {
		return toolError(fmt.Sprintf("unknown tool %q: no server %q in this group", params.Name, serverName)), nil
	}
	forwarded := *params
	forwarded.Name = tool
	callID, result, err := s.auditedCall(ctx, grant, *row, &forwarded)
	if callID == 0 {
		return nil, err
	}
	if err != nil {
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			return nil, rpcErr // the upstream's own answer, unchanged
		}
		// Bangle could not reach the server: say so as a tool error, which
		// the model sees, rather than a protocol error, which it may not.
		return toolError(fmt.Sprintf("Bangle could not call %s on %s: %v", tool, serverName, err)), nil
	}
	return result, nil
}
