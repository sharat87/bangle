package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Groups are a name plus a set of whole servers, served as one MCP endpoint
// (gateway.go). The API:
//
//	GET    /api/groups               list
//	POST   /api/groups               add {name, servers: [server names]}
//	GET    /api/groups/{name}        one
//	PATCH  /api/groups/{name}        {name?, servers?}: rename, set the members
//	DELETE /api/groups/{name}        delete, with every client's access to it
//	GET    /api/groups/{name}/tools  the group's live tools/list, as harnesses see it

type groupRow struct {
	ID   int64
	Name string
}

const groupColumns = "id, name"

func (s *Server) groupByName(ctx context.Context, name string) (groupRow, bool, error) {
	var g groupRow
	err := s.db.QueryRowContext(ctx, `SELECT `+groupColumns+` FROM groups WHERE name = ?`, name).Scan(&g.ID, &g.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return groupRow{}, false, nil
	}
	return g, err == nil, err
}

// groupMembers is the group's servers, by name.
func (s *Server) groupMembers(ctx context.Context, groupID int64) ([]serverRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixColumns("s.", serverColumns)+`
		FROM servers s JOIN group_servers gs ON gs.server_id = s.id
		WHERE gs.group_id = ? ORDER BY s.name`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []serverRow
	for rows.Next() {
		row, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func prefixColumns(prefix, columns string) string {
	parts := strings.Split(columns, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}

type groupJSON struct {
	Name    string   `json:"name"`
	Servers []string `json:"servers"`
	// LocalEndpoint is the one to give harnesses: plain http to loopback,
	// so no CA to trust. Endpoint is the same group through BASE_URL.
	LocalEndpoint string `json:"local_endpoint"`
	Endpoint      string `json:"endpoint"`
	// Clients is how many harness grants use the group (harness_oauth.go).
	Clients int `json:"clients"`
}

func (s *Server) groupEndpoint(name string) string {
	return endpointAt(s.baseURL, name)
}

func (s *Server) groupJSON(ctx context.Context, g groupRow) (groupJSON, error) {
	members, err := s.groupMembers(ctx, g.ID)
	if err != nil {
		return groupJSON{}, err
	}
	out := groupJSON{Name: g.Name, Servers: []string{}, Endpoint: s.groupEndpoint(g.Name), LocalEndpoint: endpointAt(s.localOrigin(), g.Name)}
	for _, m := range members {
		out.Servers = append(out.Servers, m.Name)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_grants WHERE group_id = ?`, g.ID).Scan(&out.Clients); err != nil {
		return groupJSON{}, err
	}
	return out, nil
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.QueryContext(r.Context(), `SELECT `+groupColumns+` FROM groups ORDER BY name`)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var groups []groupRow
		for rows.Next() {
			var g groupRow
			if err := rows.Scan(&g.ID, &g.Name); err != nil {
				rows.Close()
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			groups = append(groups, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := []groupJSON{}
		for _, g := range groups {
			j, err := s.groupJSON(r.Context(), g)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			out = append(out, j)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req struct {
			Name    string   `json:"name"`
			Servers []string `json:"servers"`
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
		s.saveGroup(w, r, groupRow{Name: name}, req.Servers, http.StatusCreated)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request, rest string) {
	name, sub, _ := strings.Cut(rest, "/")
	g, ok, err := s.groupByName(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no group named %q", name))
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		out, err := s.groupJSON(r.Context(), g)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case sub == "" && r.Method == http.MethodPatch:
		var req struct {
			Name    *string   `json:"name"`
			Servers *[]string `json:"servers"`
		}
		if err := readJSON(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		newName := g.Name
		if req.Name != nil {
			newName = strings.TrimSpace(*req.Name)
			if !validServerName.MatchString(newName) {
				writeJSONError(w, http.StatusBadRequest, "name must be 1–64 of a-z, 0-9 and -")
				return
			}
		}
		var servers []string
		if req.Servers != nil {
			servers = *req.Servers
		} else {
			current, err := s.groupJSON(r.Context(), g)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			servers = current.Servers
		}
		s.saveGroup(w, r, groupRow{ID: g.ID, Name: newName}, servers, http.StatusOK)
	case sub == "" && r.Method == http.MethodDelete:
		if _, err := s.db.ExecContext(r.Context(), `DELETE FROM groups WHERE id = ?`, g.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.gateway.forget(g.ID)
		writeJSON(w, http.StatusOK, map[string]string{})
	case sub == "tools" && r.Method == http.MethodGet:
		tools, failures, err := s.groupTools(r.Context(), g.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := struct {
			Tools    []toolJSON      `json:"tools"`
			Failures []memberFailure `json:"failures"`
		}{Tools: []toolJSON{}, Failures: failures}
		for _, t := range tools {
			out.Tools = append(out.Tools, toolOut(t))
		}
		writeJSON(w, http.StatusOK, out)
	case sub == "" || sub == "tools":
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

// saveGroup creates (g.ID 0) or updates a group, setting its members to
// exactly servers, in one transaction.
func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request, g groupRow, servers []string, status int) {
	id, name := g.ID, g.Name
	servers = slices.Compact(slices.Sorted(slices.Values(servers)))
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	if id == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO groups (name) VALUES (?)`, name)
		if isUniqueViolation(err) {
			writeJSONError(w, http.StatusConflict, fmt.Sprintf("a group named %q already exists", name))
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if id, err = res.LastInsertId(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		_, err := tx.ExecContext(ctx, `UPDATE groups SET name = ? WHERE id = ? AND name <> ?`, name, id, name)
		if isUniqueViolation(err) {
			writeJSONError(w, http.StatusConflict, fmt.Sprintf("a group named %q already exists", name))
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_servers WHERE group_id = ?`, id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	for _, server := range servers {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO group_servers (group_id, server_id) SELECT ?, id FROM servers WHERE name = ?`, id, server)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("no server named %q", server))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := s.groupJSON(ctx, groupRow{ID: id, Name: name})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, status, out)
}

// setServerGroups serves PUT /api/servers/{name}/groups: the server becomes a
// member of exactly the named groups, in one transaction, so the server's
// page sets its memberships without rewriting each group's whole list.
func (s *Server) setServerGroups(w http.ResponseWriter, r *http.Request, server serverRow) {
	var req struct {
		Groups []string `json:"groups"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Groups == nil {
		writeJSONError(w, http.StatusBadRequest, "groups is required")
		return
	}
	groups := slices.Compact(slices.Sorted(slices.Values(req.Groups)))
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_servers WHERE server_id = ?`, server.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, group := range groups {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO group_servers (group_id, server_id) SELECT id, ? FROM groups WHERE name = ?`, server.ID, group)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("no group named %q", group))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"groups": groups})
}
