-- The whole schema, as of the first public commit. Later changes are new
-- files: NNN_name.sql, run in name order, each once (migrations.go).
--
-- Convention for every table: created_at and updated_at, with updated_at
-- kept by a trigger, never set from Go. SQLite has no trigger functions, so
-- each table gets its own. The WHEN clause stops the trigger's own UPDATE
-- from firing it again.

-- The runner's own bookkeeping.
CREATE TABLE migration_log (
    id integer PRIMARY KEY,
    migration_name text NOT NULL UNIQUE,
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Upstream MCP servers (remote, streamable HTTP) and the OAuth logins Bangle
-- holds for them.
-- name is the tool prefix (<server>__<tool>) and is renameable. The CHECK
-- mirrors validServerName in internal/server/servers.go: no underscores, so
-- "__" always splits a tool name unambiguously.
--
-- scopes is space-separated; '' means "use the resource's scopes_supported".
-- client_id/client_secret are a pre-registered OAuth client; '' means "use
-- dynamic client registration". client_secret is sealed (secretbox.go)
-- whenever it is not ''.
--
-- auth_error is why the last login or refresh failed, shown in the UI as the
-- server's status. A successful login clears it.
CREATE TABLE servers (
    id integer PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name <> '' AND name NOT GLOB '*[^a-z0-9-]*' AND length(name) <= 64),
    url text NOT NULL,
    scopes text NOT NULL DEFAULT '',
    client_id text NOT NULL DEFAULT '',
    client_secret text NOT NULL DEFAULT '',
    auth_error text NOT NULL DEFAULT '',
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at text NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER servers_updated_at AFTER UPDATE ON servers
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE servers SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

-- One upstream login per server: the tokens plus everything the refresh path
-- needs (which client they were issued to, where to refresh, which resource).
-- A row exists only while the login is usable; a refresh the AS rejects
-- deletes it. access_token, refresh_token and client_secret are always sealed,
-- '' included. expires_at is unix milliseconds, NULL when the AS gave no
-- expiry.
CREATE TABLE server_tokens (
    server_id integer PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
    token_endpoint text NOT NULL,
    client_id text NOT NULL,
    client_secret text NOT NULL,
    token_auth_method text NOT NULL CHECK (token_auth_method IN ('none', 'client_secret_post', 'client_secret_basic')),
    resource text NOT NULL,
    scope text NOT NULL,
    access_token text NOT NULL,
    refresh_token text NOT NULL,
    expires_at integer,
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at text NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER server_tokens_updated_at AFTER UPDATE ON server_tokens
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE server_tokens SET updated_at = CURRENT_TIMESTAMP WHERE server_id = NEW.server_id;
END;

-- Groups: a name plus a set of whole servers, served as one MCP endpoint at
-- /g/<name>/mcp. The CHECK mirrors validServerName (internal/server).
CREATE TABLE groups (
    id integer PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name <> '' AND name NOT GLOB '*[^a-z0-9-]*' AND length(name) <= 64),
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at text NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER groups_updated_at AFTER UPDATE ON groups
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE groups SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

CREATE TABLE group_servers (
    group_id integer NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    server_id integer NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (group_id, server_id)
);

CREATE TRIGGER group_servers_updated_at AFTER UPDATE ON group_servers
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE group_servers SET updated_at = CURRENT_TIMESTAMP
    WHERE group_id = NEW.group_id AND server_id = NEW.server_id;
END;

-- Bangle as an authorization server for harnesses (Claude Code, Codex, ...).
--
-- harness_clients are RFC 7591 registrations: public clients, PKCE only.
-- redirect_uris is a JSON array.
CREATE TABLE harness_clients (
    id integer PRIMARY KEY,
    client_id text NOT NULL UNIQUE,
    name text NOT NULL,
    redirect_uris text NOT NULL,
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at text NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER harness_clients_updated_at AFTER UPDATE ON harness_clients
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE harness_clients SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

-- A grant is one approval on the consent page: a client may use one group.
-- Tokens are stored as hex sha256, never as themselves. A grant holds one
-- access token and one refresh token at a time; a refresh replaces both.
-- access_expires_at is unix milliseconds. last_used_at is updated at most
-- once a minute (gateway.go).
CREATE TABLE harness_grants (
    id integer PRIMARY KEY,
    client_id integer NOT NULL REFERENCES harness_clients (id) ON DELETE CASCADE,
    group_id integer NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    access_hash text NOT NULL UNIQUE,
    access_expires_at integer NOT NULL,
    refresh_hash text NOT NULL UNIQUE,
    last_used_at text,
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at text NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER harness_grants_updated_at AFTER UPDATE ON harness_grants
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE harness_grants SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

-- The audit of every tools/call through a group endpoint (audit.go).
--
-- A row is written before the call is forwarded (outcome 'running') and
-- completed after, so no call happens without a record. Who and where are
-- kept as names, not foreign keys, so the history outlives a revoked client
-- or a deleted group or server. arguments and result are sealed
-- (secretbox.go): the arguments as sent, and the result as returned (or the
-- error). Rows older than 90 days are deleted.
CREATE TABLE tool_calls (
    id integer PRIMARY KEY,
    grant_id integer NOT NULL,
    client_name text NOT NULL,
    group_name text NOT NULL,
    server_name text NOT NULL,
    tool text NOT NULL,
    arguments text NOT NULL,
    result text NOT NULL DEFAULT '',
    outcome text NOT NULL DEFAULT 'running' CHECK (outcome IN ('running', 'ok', 'tool_error', 'error', 'unreachable')),
    error text NOT NULL DEFAULT '',
    duration_ms integer,
    created_at text NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at text NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX tool_calls_created_at ON tool_calls (created_at);

CREATE TRIGGER tool_calls_updated_at AFTER UPDATE ON tool_calls
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE tool_calls SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;
