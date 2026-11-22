# MCPJungle notes (2026-11-06)

Why Bangle exists instead of a fork of MCPJungle 0.4.7, and what was learned running it.

## What worked

- **Upstream OAuth** (discovery, DCR, PKCE, browser flow) for Linear, Notion, PostHog, WordPress.com
  and Railway.
- **Groups of whole servers** (`included_servers` in the group JSON). The web UI can only pick tools.
- **Claude Code through a group endpoint:** 166 tools, and a real `linear__whoami` call returned the
  workspace.

## What didn't

1. **Group `/mcp` endpoints lose their session.** This is
   [#268](https://github.com/mcpjungle/MCPJungle/issues/268), fixed by
   [PR #269](https://github.com/mcpjungle/MCPJungle/pull/269), which is unmerged.
   - Cause: a new `StreamableHTTPServer` per request.
   - The client's stream-opening GET gets 400. Claude Code shrugs it off, but the MCP Inspector CLI
     reports 0 tools.
2. **Tool lists are a snapshot taken at `register`.** This is
   [#260](https://github.com/mcpjungle/MCPJungle/issues/260). New upstream tools never appear without
   re-registering.
3. **Railway needed explicit `oauth_scopes`.** It needed
   `["openid", "offline_access", "workspace:member", "project:member"]`; without them, its redirect
   carried no `code`/`state`. Railway access tokens last 1 hour, and a refresh token comes only with
   `offline_access`.
4. **GitHub's remote MCP has no DCR.** It needs a pre-registered OAuth App. It does accept
   `gh auth token` as a bearer token, but that hands every agent gh's full scopes. Decision: no GitHub
   MCP; agents use `gh`.
5. **The rest:**
   - Upstream OAuth is CLI-only.
   - The UI asks for transport and session mode, which could be discovered.
   - No server rename.
   - Group edits replace the whole definition.
   - Config files are one-shot inputs that silently go stale
     ([#266](https://github.com/mcpjungle/MCPJungle/issues/266)).
   - Downstream auth is either nothing (dev mode) or static bearer tokens (enterprise mode). There's no
     OAuth for harnesses.
6. **Stored credentials:** upstream tokens sit in plain SQLite (`upstream_o_auth_tokens`).

License is MPL-2.0: file-level copyleft, with no obligations for private use. Individual files can be
borrowed into Bangle if they stay MPL. `internal/service/mcp/upstream_oauth.go` is the useful one.

## Alternatives considered and rejected

The preference is Go, or at least not TypeScript, and something with traction.

- **R0Wi/mcp-gateway** (Python, ~3k LOC). The best OAuth of all of them:
  - upstream CIMD/DCR, PKCE and persisted refresh
  - pre-registered clients
  - Fernet-encrypted tokens
  - its own AS for clients

  But it's too small and young, has no LICENSE file, and lacks groups and stdio. Its `upstream.py` and
  `oauth_server.py` are a good reference for milestones 1 and 3.
- **1mcp-app/agent** (TypeScript, ~137k LOC). Feature-complete, including tag/preset groups and
  group-bound tokens, but it's TypeScript and huge.
- **IBM/mcp-context-forge** (Python, ~286k LOC). Heavy, groups are per tool, and downstream auth
  assumes an external IdP.
- **metatool-ai/metamcp** (TypeScript). Needs Postgres, has no token encryption, and has been stale
  since 2026-06.
- **docker/mcp-gateway** (Go). Needs Docker, and OAuth goes through Docker Desktop.
- **agentgateway** (Rust). No browser-based upstream OAuth client.
- **lucasbrito-wdt/mcpgw** (Go). Needs Postgres 18, and has a single commit.

## The MCPJungle setup Bangle replaced (retired 2026-11-06)

- It ran as `mcpjungle start --host 127.0.0.1 --port 8080 --sqlite-db-path
  ~/.local/share/mcpjungle/mcpjungle.db`, in dev mode under `nohup`, not as a launchd service.
- **Retired:** its servers and its one group moved to Bangle, with the same tools, now named
  `mcp__<group>-bangle__<server>__<tool>`.
- The process was stopped, the Homebrew formula and the `mcpjungle/mcpjungle` tap were removed, and
  `~/.local/share/mcpjungle` was deleted: its DB held the upstream tokens in plaintext, mode 0644.
- The OAuth clients MCPJungle registered at each provider may still be listed in their settings
  (Linear, Notion and so on, under authorized apps). With the DB gone their tokens exist nowhere, but
  revoking them there is tidy.
