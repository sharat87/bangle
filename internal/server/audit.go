package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The audit: every tools/call that the gateway forwards to a server, with
// who made it (the grant's client and group), the arguments, the result,
// the outcome and how long it took. Arguments and results are stored
// sealed. They can be anything: issue text, file contents, secrets.
//
// The row is written before the call is forwarded and completed after, so
// a call never happens without a record: if the first write fails, the call
// is refused. Rows go after auditRetention. tools/list is not audited, and
// neither are calls the gateway refuses before routing (unknown tool, server
// not in the group).
//
//	GET /api/calls?server=&group=&client=&before=<id>  newest first, without contents
//	GET /api/calls/{id}                                one call, with arguments and result

const (
	auditRetention = 90 * 24 * time.Hour
	auditPageSize  = 100
)

// auditStart records a call about to be forwarded and returns its id.
func (s *Server) auditStart(ctx context.Context, grant gatewayGrant, server, tool string, args json.RawMessage) (int64, error) {
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	sealedArgs, err := s.box.seal(string(args))
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_calls (grant_id, client_name, group_name, server_name, tool, arguments)
		VALUES (?, ?, ?, ?, ?, ?)`, grant.ID, grant.ClientName, grant.GroupName, server, tool, sealedArgs)
	if err != nil {
		return 0, err
	}
	s.pruneAuditSometimes()
	return res.LastInsertId()
}

// auditFinish completes a call's record. It runs detached from the request:
// the call has happened and must be recorded even if the client is gone.
// A failure here can only be logged; the result is already on its way back.
func (s *Server) auditFinish(ctx context.Context, id int64, started time.Time, result *mcp.CallToolResult, callErr error) {
	outcome, errText, body := callOutcome(result, callErr)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := func() error {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		sealed, err := s.box.seal(string(raw))
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `
			UPDATE tool_calls SET outcome = ?, error = ?, result = ?, duration_ms = ? WHERE id = ?`,
			outcome, errText, sealed, time.Since(started).Milliseconds(), id)
		return err
	}()
	if err != nil {
		log.Printf("audit: call %d happened but its result was not recorded: %v", id, err)
	}
}

// callOutcome is how a call ended, and what is recorded as its result. The
// outcomes: ok; tool_error, the tool's own failure (IsError); error, the
// server's JSON-RPC error; unreachable, Bangle couldn't get an answer at all.
func callOutcome(result *mcp.CallToolResult, callErr error) (outcome, errText string, body any) {
	var rpcErr *jsonrpc.Error
	switch {
	case callErr != nil && errors.As(callErr, &rpcErr):
		return "error", callErr.Error(), map[string]any{"error": rpcErr}
	case callErr != nil:
		return "unreachable", callErr.Error(), map[string]string{"error": callErr.Error()}
	case result.IsError:
		return "tool_error", "", result
	}
	return "ok", "", result
}

// auditedCall forwards one tools/call to row, recorded. No call without a
// record: if the audit can't write, the call isn't made (callID 0).
func (s *Server) auditedCall(ctx context.Context, grant gatewayGrant, row serverRow, params *mcp.CallToolParamsRaw) (callID int64, result *mcp.CallToolResult, err error) {
	callID, err = s.auditStart(ctx, grant, row.Name, params.Name, params.Arguments)
	if err != nil {
		return 0, nil, fmt.Errorf("bangle: the call could not be recorded, so it was not made: %w", err)
	}
	started := time.Now()
	result, err = s.callUpstreamTool(ctx, row, params)
	s.auditFinish(ctx, callID, started, result, err)
	return callID, result, err
}

// uiGrant is who a call made from a tool's page in the UI is recorded as:
// no grant, no group.
var uiGrant = gatewayGrant{ClientName: "Bangle UI"}

// callFromUI answers POST /api/servers/{name}/call {tool, arguments}: the
// tool's page calling it. It is audited like a gateway call, as uiGrant.
// The answer is 200 whenever the call was made, whatever its outcome:
// {id, outcome, error, duration_ms, result}.
func (s *Server) callFromUI(w http.ResponseWriter, r *http.Request, row serverRow) {
	var in struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Tool == "" {
		writeJSONError(w, http.StatusBadRequest, "tool is required")
		return
	}
	started := time.Now()
	id, result, err := s.auditedCall(r.Context(), uiGrant, row, &mcp.CallToolParamsRaw{Name: in.Tool, Arguments: in.Arguments})
	if id == 0 {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	outcome, errText, body := callOutcome(result, err)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          id,
		"outcome":     outcome,
		"error":       errText,
		"duration_ms": time.Since(started).Milliseconds(),
		"result":      body,
	})
}

// pruneAuditSometimes deletes rows past retention, at most once an hour.
type auditPruner struct {
	mu   sync.Mutex
	last time.Time
}

func (s *Server) pruneAuditSometimes() {
	s.auditPrune.mu.Lock()
	due := time.Since(s.auditPrune.last) > time.Hour
	if due {
		s.auditPrune.last = time.Now()
	}
	s.auditPrune.mu.Unlock()
	if !due {
		return
	}
	cutoff := time.Now().Add(-auditRetention).UTC().Format("2006-01-02 15:04:05")
	res, err := s.db.Exec(`DELETE FROM tool_calls WHERE created_at < ?`, cutoff)
	if err != nil {
		log.Printf("audit: pruning tool calls older than %s: %v", cutoff, err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		log.Printf("audit: pruned %d tool calls older than %s", n, cutoff)
	}
}

type callJSON struct {
	ID         int64           `json:"id"`
	CreatedAt  string          `json:"created_at"`
	ClientName string          `json:"client_name"`
	GroupName  string          `json:"group_name"`
	ServerName string          `json:"server_name"`
	Tool       string          `json:"tool"`
	Outcome    string          `json:"outcome"`
	Error      string          `json:"error"`
	DurationMS *int64          `json:"duration_ms"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

const callColumns = `id, created_at, client_name, group_name, server_name, tool, outcome, error, duration_ms`

func scanCall(row interface{ Scan(...any) error }, extra ...any) (callJSON, error) {
	var c callJSON
	var duration sql.NullInt64
	dst := append([]any{&c.ID, &c.CreatedAt, &c.ClientName, &c.GroupName, &c.ServerName, &c.Tool, &c.Outcome, &c.Error, &duration}, extra...)
	if err := row.Scan(dst...); err != nil {
		return callJSON{}, err
	}
	if duration.Valid {
		c.DurationMS = &duration.Int64
	}
	return c, nil
}

func (s *Server) handleCalls(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if rest != "" {
		s.callDetail(w, r, rest)
		return
	}
	q := r.URL.Query()
	where := []string{"1 = 1"}
	var args []any
	for param, column := range map[string]string{"server": "server_name", "group": "group_name", "client": "client_name"} {
		if v := q.Get(param); v != "" {
			where = append(where, column+" = ?")
			args = append(args, v)
		}
	}
	if v := q.Get("before"); v != "" {
		before, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "before must be a call id")
			return
		}
		where = append(where, "id < ?")
		args = append(args, before)
	}
	// One more than a page says whether there is an older page.
	args = append(args, auditPageSize+1)
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+callColumns+` FROM tool_calls WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	calls := []callJSON{}
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	more := len(calls) > auditPageSize
	if more {
		calls = calls[:auditPageSize]
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls, "more": more})
}

func (s *Server) callDetail(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	var sealedArgs, sealedResult string
	c, err := scanCall(s.db.QueryRowContext(r.Context(),
		`SELECT `+callColumns+`, arguments, result FROM tool_calls WHERE id = ?`, id), &sealedArgs, &sealedResult)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no call %d (calls are kept for 90 days)", id))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, f := range []struct {
		sealed string
		dst    *json.RawMessage
	}{{sealedArgs, &c.Arguments}, {sealedResult, &c.Result}} {
		if f.sealed == "" {
			continue // a call still running has no result yet
		}
		plain, err := s.box.open(f.sealed)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		*f.dst = json.RawMessage(plain)
	}
	writeJSON(w, http.StatusOK, c)
}
