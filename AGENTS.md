# Bangle

A local MCP gateway. It holds the upstream OAuth logins for every MCP server I use (Linear,
Notion, PostHog, Railway, WordPress.com, …), and exposes **groups** of those servers as MCP endpoints,
one per project. Every agent harness (Claude Code, Pi, Codex, …) points at its project's group instead
of doing its own OAuth per server.

It replaces a stock MCPJungle 0.4.7 setup that worked but was half-built: tool lists were snapshots
taken at registration (#260), a fresh MCP server per request broke sessions (#268), and its config was
one-shot JSON files.

## Working philosophy

- Keep things predictable, single, obvious, and light.
- If something is not providing value _today_, it probably should not exist.
- If something is not an asset, it is a liability.
- Prefer fewer dependencies. A new dependency should buy clear present-day value and simplify the system
  more than it complicates it. Prefer the standard library or small local code when that keeps things
  simpler.
- Fail early and loudly. Validate prerequisites instead of stumbling forward in a half-working state.
- Avoid fallbacks unless they are genuinely necessary. Hidden alternate paths make behavior harder to
  reason about.
- Never silently swallow errors. An ignored error return or an empty catch block is a hidden fallback in
  disguise. Propagate it or fail the operation.
- Keep `AGENTS.md` accurate and concise as the implementation evolves. It records what is true; the plan
  section below is the only place for what is not built yet, and items move out of it as they land.
- Build the frontend after frontend changes.
- Add comments near critical flows when fixing bugs so they do not regress.
- **Always commit when done.** Do not wait for the user to ask. Never leave the working copy dirty at the
  end of a turn. A follow-up correction can amend the commit.
- **Every new environment variable needs explicit approval before being added.**

## Product shape (decided)

- **Single user, single machine.** No accounts, no logins, no admin model. The listener is loopback
  only.
- **It runs as me, in tmux (`make`), and agents manage it end to end** (build, restart, deploy).
  Running as a dedicated `_bangle` user was built and rolled back on 2026-11-06. It kept the tokens
  from agents, but only because nothing running as me could change the code `_bangle` ran, so every
  deploy needed my password. **The accepted risk:** any process running as me, coding agents
  included, can read `.env` (`SECRETS_KEY`) and the DB, so every upstream login. Don't reintroduce
  isolation without asking.
- **The web UI is the only interface. There is no CLI.** Its JSON API is internal to it for now.
- **The DB is the only state.** Servers, groups and tokens live in SQLite and are managed from the web
  UI. There are no config files to drift out of date. MCPJungle's one-shot JSON files were the thing
  that annoyed most.
- **Groups are made of whole servers.** A group is a name plus a set of servers. There is no per-tool
  picking until someone actually needs it.
- **A group's endpoint lists every member's tool.** One group's are 1043 tools and 4.8 MB of
  schemas, so it's for a harness that defers tools itself (Claude Code's tool search); one that
  doesn't gets every schema in context. A search mode (three tools, `search`, `describe` and
  `execute`, finding and calling members' tools) was built and removed on 2026-11-22, with its
  `tool_mode` column. It's in no history here: the repo starts after.
- **Tool lists are near-live.** They're read from the upstream servers, kept at most 5 minutes (see
  Upstream sessions), never a snapshot taken at registration. That's MCPJungle #260, and the reason
  for not forking it.
- **Tool names are `<server>__<tool>`,** the convention MCPJungle used. They're what the harness
  lists and calls. Server names are `[a-z0-9-]+`, are shown in the UI, and can be renamed.
  A rename changes tool names and keeps the stored OAuth token, so you don't approve the login again.
- **One redirect URI for every upstream login:** `BASE_URL` + `/oauth/upstream/callback`.
- **Upstream logins need dynamic client registration** (RFC 7591). Pre-registered clients
  (a client id and sealed secret per server) were supported and removed on 2026-11-23: no server
  used one. GitHub has no DCR and is deliberately not used (agents use `gh`).
- **Upstream servers are remote only: streamable HTTP with an OAuth login.** One that answers an
  unauthenticated request with anything but 401 is refused at Connect. Local (stdio) servers were
  supported, with sealed per-server environment variables and an allowlisted environment, and
  removed on 2026-11-22: none was in use. Bring them back when someone needs one.
- **Agents connect over plain http to loopback,** `http://127.0.0.1:7060/g/<group>/mcp`. Node- and
  rustls-based harnesses don't trust Caddy's local CA, so `BASE_URL` (https) is for the browser. The
  same endpoint works through `BASE_URL` for a harness that does trust it.

- **No LLM gateway, for now.** One was built (forwarding model API calls to providers by a
  `<provider>/` model prefix, recording each request and response) and removed on 2026-11-22: it
  wasn't being used.

### Later, not now: don't design for these beyond not blocking them

- **An OpenAPI spec for the server API,** so `curl` can serve as the CLI. Keep the API JSON-in/JSON-out
  and resource-shaped so a spec can describe it.
- **An agent chat in the web UI** that makes config changes on request: "create a group with Linear
  and Notion", "add a PostHog server". It would drive the same API the UI uses.

### Open questions, deliberately undecided: do not build these without asking

- Per-tool include/exclude within a group.
- Forwarding upstream `notifications/tools/list_changed` to connected agents. Today nothing polls;
  lists are fetched on use and kept 5 minutes.

## Upstream OAuth

How Bangle logs in to a server (`upstream_oauth.go`), all of it in my browser from the server's page:

1. **Discovery.** An unauthenticated `initialize` must get a 401. Its `WWW-Authenticate:
   resource_metadata` names the RFC 9728 metadata, else `/.well-known/oauth-protected-resource[/path]`
   (path-inserted, then root). The first `authorization_servers` entry's metadata is RFC 8414, else OIDC
   (path-inserted), else OIDC path-appended. Its `issuer` must match. Every discovered URL must be https,
   or http to localhost.
2. **Client.** RFC 7591 DCR as a public client, fresh per login. An AS without DCR is refused.
3. **Scopes.** The configured ones, else the resource's `scopes_supported`. Add `offline_access` whenever
   the AS lists it, and with it `prompt=consent` (OIDC Core §11: without it, an OIDC AS like Railway's
   ignores `offline_access` and issues no refresh token).
4. **Connect is a form POST** to `/oauth/upstream/start`, answered with 303 to the AS, with PKCE S256,
   `state` and `resource` (RFC 8707). Pending logins are in memory for 15 minutes; a restart forgets
   them.
5. **The callback** checks `state` (single use) and `iss` (RFC 9207), exchanges the code and stores the
   login sealed, then 303s back to `/servers/<name>`.

Every failure is stored as the server's `auth_error`. Status is one rule (`serverStatus`): `error` if
there's an `auth_error`, else `connected` if a login is stored, else
`needs_login`. A successful login clears the error.

**Refresh has one code path,** `upstreamToken` in `upstream_token.go`:
- It is serialized per server by a mutex, so concurrent callers refresh once.
- It refreshes a minute before expiry, and when upstream answers 401. `upstreamAuth`, the
  `RoundTripper` under every upstream MCP request, retries the request once with the new token.
- It persists the rotated refresh token before handing out the new access token. The refresh runs
  detached from the caller's context, so a cancelled request can't lose a rotated token.
- A 400/401 from the token endpoint, or an expired token with no refresh token, deletes the login and
  sets `auth_error`. A network error or 5xx leaves the login alone.

**Disconnect** (`DELETE /api/servers/{name}/login`) forgets the login and the error, back to
`needs_login`. It doesn't revoke the grant upstream.

## Upstream sessions (`upstream_pool.go`)

- **One open go-sdk client session per server,** opened on first use and shared by every group and
  page, with `upstreamAuth` underneath it.
- A session is dropped when an RPC fails at the transport level, and reopened on next use. It is
  closed on delete, disconnect and a new login.
- **A server's tool list is kept 5 minutes, tied to the session it was listed on,** so any close or
  drop of that session (a new login, disconnect, a transport failure) refreshes it. Every page and
  the gateway read it; reconnecting the server, or restarting Bangle, shows a change sooner. Without
  it, every listing fetched PostHog's 4.8 MB list, 4.5 s each time. (A Restart endpoint was removed
  on 2026-11-22, with stdio servers.)
- **Retries:** a `tools/list` that fails at the transport level is retried once on a fresh session.
  A `tools/call` is retried only on `ErrSessionMissing` (the upstream 404'd the session, so the call
  never ran). A tool must never run twice for one request.

## The gateway (`gateway.go`)

- `/g/<group>/mcp` is a streamable-HTTP MCP server with **one cached `mcp.Server` and handler per
  group**, keyed by id, for the process's life. A fresh one per request was MCPJungle #268.
- It registers no tools. A receiving middleware answers `tools/list` with every member's live tool,
  renamed `<server>__<tool>`, read in parallel on every request, so membership changes need no
  rebuild. A `tools/call` is split at the first `__`, checked for membership, forwarded, and its
  result returned unchanged.
- **A member that fails** (not logged in, unreachable) is left out of `tools/list`, so one broken
  server doesn't take the group down. The group's page names it.
- Every mistake the model can make (an unknown tool, a server not in the group, a member Bangle
  can't reach) is a tool error it can read. An upstream's own JSON-RPC error passes through
  unchanged. `null` arguments are forwarded as absent.
- The go-sdk's own DNS-rebinding check is off (it refuses Caddy's Host); `request_guard.go` does it.
- Gateway request bodies, and a tool page's call, may be 32 MB (tool arguments can be files); everything else is 64 KB.
- There is no upstream `notifications/tools/list_changed` forwarding. A harness keeps the list it
  read on connect, so a member's new tool, or a new member, shows on reconnect.

## OAuth for harnesses (`harness_oauth.go`)

Bangle is the authorization server for its own group endpoints, so a harness logs in to a group the
way it logs in to any remote MCP server:

1. An unauthenticated `/g/<group>/mcp` request gets 401 and `WWW-Authenticate: Bearer
   resource_metadata=".../.well-known/oauth-protected-resource/g/<group>/mcp"`. That names Bangle
   as the AS (`/.well-known/oauth-authorization-server`).
2. **DCR** at `/oauth/register`: public clients only, PKCE S256 required. Redirect URIs are https,
   http to loopback (any port at authorize time, RFC 8252), or an app's own scheme, never
   javascript/data/file. Registrations nobody approved are pruned after a day.
3. `/oauth/authorize` checks the request and sends the browser to `/consent/<id>`, which shows the
   client's name, the group and where the browser goes back to. Approve or Deny is a form post to
   `/oauth/consent`. Errors before client and redirect URI check out are a page, never a redirect.
4. `/oauth/token` exchanges the code, and refresh tokens. Access tokens last an hour; every refresh
   replaces both tokens.

- **A grant is bound to one group:** the authorize request's `resource` (RFC 8707) must be that
  group's endpoint, and its tokens work there only. Tokens are stored as sha256.
- Pending consents and codes are in memory: a restart mid-login means logging in again.
- **Two origins:** everything a harness discovers (challenge, metadata, issuer, endpoints) is built
  from the origin the request came in on (`originFor`): `BASE_URL`, or the loopback listener. That
  isn't trusting `Host`: the request guard admits only those two.
- **No page may be framed** (`frame-ancestors 'none'`, `X-Frame-Options: DENY`), so the consent page's
  Approve can't be clickjacked.
- **Claude Code keeps an HTTP server's OAuth tokens per server name and URL, not per project.** Two
  projects with an entry of the same name and URL share one login, and `claude mcp remove` in one
  logs the other out too. Test with a different entry name than your real one.
- The Connected Clients page lists grants (client, group, approved, last used: updated at most once a minute)
  and revokes them. Deleting a group revokes its grants.

## The audit (`audit.go`)

- **Every `tools/call` the gateway forwards, and every call from a tool's page, is recorded** in `tool_calls`: when, the grant's client
  and group, the server and tool, the arguments, the result (or the error), the outcome (`ok`,
  `tool_error`, `error` for the server's JSON-RPC error, `unreachable`) and the duration.
- Arguments and results are stored sealed. Who and where are kept as names, not foreign keys, so the
  history outlives a revoked client or a deleted group or server.
- **No call without a record:** the row is written (`running`) before the call is forwarded, and the
  call is refused if that write fails. It's completed after, detached from the request.
- Rows go after 90 days, checked at most once an hour on insert.
- Not audited: `tools/list`, and calls refused before routing (unknown tool, server not in the
  group). A call is recorded as the member's tool it called, without the `<server>__` prefix.
- The grant reaches the middleware through the go-sdk's token info (`auth.RequireBearerToken` with a
  verifier that returns what `authorizeGateway` already found). Its `UserID` (`grant:<id>`) also
  makes the SDK bind each MCP session to the grant that opened it.
- `GET /api/calls?server=&group=&client=&before=<id>` lists calls newest first, without contents, a
  page of 100; `GET /api/calls/{id}` is one call with arguments and result.

## UI pages

- `/` MCP servers (table: status, live tool count, actions), `/add`, `/servers/<name>` (with its groups
  as checkboxes: `PUT /api/servers/{name}/groups` sets exactly those memberships).
- `/groups` (table), `/add-group`, and `/groups/<name>`: how to connect a coding agent (the exact
  `claude mcp add` command, naming the entry `<group>-bangle` so it can't clash with an older entry;
  any OAuth-capable MCP client works), members as checkboxes, and the members' live tools, with members left out and why.
- `/clients` (grants, with revoke), and `/consent/<id>`.
- `/servers/<name>/tools/<tool>` (one tool, and a form to call it).
- `/calls` (the audit, newest first; `?client=`, `?group=`, `?server=` filter it) and `/calls/<id>`
  (arguments, the result's text as the model read it, and the raw result). JSON is highlighted, and a
  result text that is a JSON object or array is pretty-printed; any other text is shown as is.
- The sidebar has one headed section, "MCP" (Servers, Groups, Connected Clients, Call History), and
  those pages' headings match.

A server's page lists its live tools, and the table counts them. That is the proof a server works.
Each tool shows its signature, `(id: string, limit?: number)`, read from its input schema
(`ToolSignature` in `common.jsx`): required parameters first, an enum of more than 4 values as its base
type, and each parameter's description (and a long enum's values) as its tooltip. A tool's name, and
the "Try it" link beside it (the name alone didn't read as a link), go to its page, `/servers/<name>/tools/<tool>` (`toolcall.jsx`), which calls it: a form built from the schema
(text and number inputs, radios for enums and booleans, a JSON textarea for anything else; each
parameter's description is markdown, its first block shown and the rest behind More) or the
arguments as JSON, then the result as on a call's page. The call is `POST /api/servers/{name}/call`
(`callFromUI`), audited like an agent's, as client `Bangle UI` with no group, and its body may be 32 MB.

## Stack

- **Backend:** Go, plain `net/http` (no framework, no ORM), one flat `internal/server` package.
  - MCP is `github.com/modelcontextprotocol/go-sdk`.
  - SQLite is `modernc.org/sqlite`, pure Go, so `go build` stays a single static binary with no cgo.
- **Frontend:** Preact in plain JavaScript with JSX (`.jsx`), bundled by `build.mjs` (the esbuild JS
  API), with one plain CSS file (`frontend/styles.css`). No TypeScript and no formatter: esbuild never
  type-checked, so the types bought nothing. Icons are `lucide-preact` (inline SVG; esbuild bundles only the
  ones imported). No CSS framework, no preprocessor, no icon font, no router or state library.
  - `frontend/` is also the Go package `frontend`. It embeds `frontend/dist`, but **serves from disk
    whenever `frontend/dist` exists under the working directory.** That's the case for `make` and
    `make dev-server`, both `go run` from the repo, so a frontend change is `make
    build-frontend` and a reload. A binary run elsewhere serves its embedded copy.
  - `frontend/dist/.gitkeep` is committed so the embed always compiles, and `build.mjs` rewrites it.
  - `TestFSPrefersDiskOverEmbedded` pins this behavior.
- **Database:** SQLite at `DB_PATH`, in WAL mode, with migrations embedded and run at startup.

### Frontend rules

- Colours are tokens (`light-dark()` in `:root`). Every view works in light and dark.
- Anything that navigates is an `<a href>`, never a button with an onClick that navigates.
- Every fetched section has three distinguishable states: loading, failed, and loaded-but-empty. An empty
  state is a claim about the data. Never render one from data that did not arrive.
- No cards, no grid rows/columns, no toggle switches. Use document-style section headers with content
  directly beneath, flex with gap for layout, and radio buttons or checkboxes for settings. A list of
  records with the same fields (the servers) is a plain `<table>`. A row that opens a page is
  `tr.linked`: its name is the one real `<a class="row-link">`, stretched over the row by CSS, and
  `td.row-actions` sits above it so its buttons still work. The link reads as the row's name, not
  as a link, and the row highlights on hover. The servers and groups tables show no URLs; their
  pages do. Cells have padding on both sides and are vertically centred.
- Upstream servers are called "MCP servers" in the UI. The code, API and tables say `servers`.
- The layout fills the viewport: a left sidebar ("Bangle", linking home, the nav links with icons in
  headed sections, and at its bottom the footer: "Built by Shri" and the running commit, from `/api/health`) and the
  main area, each with its own scrollbar; the document itself never scrolls. Below 48rem the sidebar
  becomes a top bar and the page scrolls as a whole. Navigation is still full page loads.
- Tool descriptions are markdown, rendered by `markdown.jsx` into Preact elements, never HTML strings.
- Every button has an icon. Text is optional where the icon makes the intent obvious; an icon-only
  button has `title` and `aria-label`.
- Every control (button, `.button` link, input) is exactly `--control-height` tall. Height is never
  derived from content, so an icon-only button lines up with an icon-and-text one.
- Login buttons follow `logged_in`, not `status`: Connect without a stored login, Reconnect and
  Disconnect with one.
- One fetch wrapper (`frontend/api.js`). Use it, don't hand-roll `fetch` + `res.json()`.

## Commands

The server runs in tmux as me, started by `make` (`scripts/serve.sh`):
- A window named `bangle` in the `daemons` tmux server (`tmux -L daemons`). It runs `go run -buildvcs=true ./cmd/server` in this repo with `.env` sourced.
  `-buildvcs=true` stamps the git commit, which `go run` otherwise leaves out, so the footer can show
  it. A build without one runs as `dev`.
- It logs to `.logs/server.log`, emptied on each start.
- Restarting kills the window; Bangle treats that SIGHUP like SIGTERM and closes its upstream
  sessions. Then whatever still listens on 7060 is stopped.
- Nothing restarts it if it crashes or the machine reboots: run `make` again.

- `make`: gofmt and vet, build the frontend, and (re)start the server in tmux. It exits right away;
  check `.logs/server.log` for the boot lines.
- `make build-frontend`: `pnpm install` + `node build.mjs`. That's all a frontend-only change needs,
  then reload; there's no watch mode.
- `make dev-server`: a throwaway instance on `http://127.0.0.1:7061` with its own DB and key in
  `.data/` (`dev.db`, `dev.key`), for trying changes without touching the real logins. Run it in the
  background, check, and stop it. Never connect real accounts to it.
- `make test`: gofmt check, `go vet`, frontend build, `go test ./...`. Tests make their own DBs.
- `make build`: `bin/bangle`, with the frontend embedded.
- To inspect the DB: `sqlite3 "$(. ./.env; echo $DB_PATH)" .tables`.

## Environment

`.env` is gitignored and sourced by `scripts/serve.sh` and by `make test`. There is no dotenv library.
`.env.example` lists every variable, and `cmd/server` validates each one at boot.

| Variable | Required | Meaning |
| --- | --- | --- |
| `DB_PATH` | yes | SQLite file, relative to the repo root. Locally it's `.data/bangle.db`, which is gitignored. |
| `BASE_URL` | yes | What the app is accessed as, as a bare http(s) origin. `.env.example` has `http://127.0.0.1:7060`; mine is `https://7060.port.localhost` (Caddy, below). It's the only non-loopback `Host` the request guard accepts, and the origin every absolute URL is built from (OAuth redirect URIs, resource metadata). It's never taken from the request's `Host`. |
| `SECRETS_KEY` | yes | 32 random bytes, base64 (`openssl rand -base64 32`). Upstream tokens (and any client secret DCR issued) are AES-256-GCM sealed under it (`secretbox.go`). Losing it means connecting every server again. |

The listen address is a flag, `-listen`, defaulting to `127.0.0.1:7060`. Caddy, set up for `*.port.localhost`,
terminates TLS for `https://7060.port.localhost` and forwards to it. Some OAuth
providers redirect only to https, which is what that's for.


## Access

There are no logins. Two things keep a web page in my own browser from driving the API
(`request_guard.go`):
- **`Host` must be `BASE_URL`'s host, or loopback on the listen port.** A DNS-rebinding page sends its
  own `Host` and gets 421.
- **Cross-origin writes are refused** (`http.NewCrossOriginProtection`). Requests with neither
  `Sec-Fetch-Site` nor `Origin` (curl, MCP clients) pass.

Group endpoints (`/g/<group>/mcp`) are the exception: they need a bearer token granted on the consent
page for that group (`harness_oauth.go`). The UI and its API have no login of their own.

Nothing stops a local process: any process on the machine, other users' included, can use the UI's
API, and with it every login. The README says to run it only on a machine that's
yours alone.

Don't bind a non-loopback interface: `cmd/server` refuses to start on one, except every interface
in a container (see Open source, CI and the image). The upstream tokens in the DB
are the keys to my Linear/Notion/Railway accounts.

## Open source, CI and the image

- MIT (`LICENSE`). The README is for someone running it; this file is for working on it.
- `.github/workflows/ci.yml` runs `make test` on pushes to main and on PRs.
- **No releases, no version tags.** One image, `ghcr.io/sharat87/bangle:latest`, for amd64 and
  arm64, rebuilt by `ci.yml` on every push to main that passes the tests. (Tagged GitHub releases
  with per-platform binaries were set up and dropped on 2026-11-22.)
- **The image** (`Dockerfile`): its runtime is Alpine with CA certificates and the static binary
  (about 30 MB); it runs as `bangle` (uid 1000), with the DB in `/data`.
  The frontend and Go stages run on the build machine's platform and Go cross-compiles, so the
  arm64 image needs no emulated build (it took 15 minutes under QEMU); only the runtime stage's
  `apk add` is emulated.
- **A container's loopback isn't the host's,** so the image runs `-listen 0.0.0.0:7060`. That's the
  one exception to loopback-only: `cmd/server` accepts every interface (0.0.0.0, ::, an empty host)
  when asked, with a loud warning, and still refuses a specific non-loopback address. It's safe
  only published as `-p 127.0.0.1:7060:7060`, which the README and the Dockerfile say. The UI then
  tells harnesses `http://127.0.0.1:<port>` (`localOrigin`), never 0.0.0.0. The request guard
  needs nothing new: the browser's `Host` is loopback on the same port, or `BASE_URL`'s.

## Project map

Backend:
- `cmd/server/main.go`: flags and env checks, the build's commit, opening the DB, migrations,
  `server.New`, and `ListenAndServe`.
- `internal/db/db.go`: `Open`: SQLite with WAL, foreign keys and a busy timeout, then a ping.
- `internal/server/server.go`: `Server`, `ServeHTTP` (the only middleware layer, one routing `switch`),
  static assets and the SPA fallback.
- `internal/server/request_guard.go`: the loopback `Host` check and cross-origin refusal.
- `internal/server/helpers.go`: `writeJSON`, `writeJSONError` and `readJSON`. `/api/*` returns JSON,
  including every error.
- `internal/server/secretbox.go`: `SecretBox`, sealing under `SECRETS_KEY`.
- `internal/server/servers.go`: the servers API (`/api/servers[/{name}[/tools|/login]]`), name and URL
  validation, and `serverStatus`.
- `internal/server/upstream_oauth.go`: discovery, DCR, PKCE, `/oauth/upstream/start` and
  `/oauth/upstream/callback`.
- `internal/server/upstream_token.go`: the stored login, the one refresh path, and `upstreamAuth`.
- `internal/server/upstream_pool.go`: cached upstream sessions, list and call.
- `internal/server/groups.go`: the groups API (`/api/groups[/{name}[/tools]]`).
- `internal/server/gateway.go`: `/g/<group>/mcp`, one cached server per group, the middleware, and
  routing a call to its member.
- `internal/server/harness_oauth.go`: Bangle as AS for harnesses: metadata, DCR, authorize, consent,
  token, the gateway's bearer check, and the clients API (`/api/clients`).
- `internal/server/audit.go`: the tool call audit, its pruning, and `/api/calls`.
- `internal/server/upstream_test.go`: upstream OAuth against `fakeUpstream`, an httptest MCP server
  plus its AS.
- `internal/server/gateway_test.go`: groups, the gateway, harness OAuth (with the go-sdk's own OAuth
  client as the harness).
- `scripts/serve.sh`: (re)starts the server in tmux, for `make`.
- `migrations/`: `NNN_name.sql` files and `migrations.go`, the embedded runner tracked in
  `migration_log`. Each file and its log row run in one transaction.

Frontend:
- `frontend/frontend.go`: `FS()`, which serves the bundle from disk when present, else the embedded
  copy.
- `frontend/favicon.svg`: the logo (a ring strung with beads: servers on one band), the favicon and
  the header bar's mark. `build.mjs` copies it into `dist`, unhashed.
- `frontend/index.html`, `frontend/main.jsx` (mount), `frontend/App.jsx` (the sidebar layout, routes
  on the path, the nav, the footer), `frontend/common.jsx` (icons size, NameHeader, CopyButton, tool
  lists), `frontend/servers.jsx`, `frontend/groups.jsx`, `frontend/clients.jsx` (clients and consent), `frontend/calls.jsx` (the audit),
  `frontend/toolcall.jsx` (a tool's page and its call form),
  `frontend/markdown.jsx` (basic markdown for tool descriptions), `frontend/json.jsx` (highlighted JSON),
  `frontend/api.js` (the fetch wrapper and `useFetched`),
  `frontend/styles.css` (tokens and every utility, listed in its header comment).

## Data rules

- Every change to DB shape goes through a migration. Never `CREATE TABLE` from Go.
- Every table has `created_at` and `updated_at`. `updated_at` is maintained by a per-table trigger.
  **Never set `updated_at` from Go.**
- Migrations only add. To retire a column, stop using it in one commit and drop it in a later one.
  `001_init.sql` is the whole schema as of the first public commit (earlier migrations were squashed
  into it); every change after it is a new file.
- Secrets (upstream access/refresh tokens, DCR-issued client secrets) are stored sealed, never returned by any
  endpoint, and never logged.
- `servers` holds a server's config (URL, scopes) and `auth_error`. `server_tokens`
  holds its login, one row per server, keyed by id so a rename keeps it.
- `groups` and `group_servers`; `harness_clients` (registrations) and `harness_grants` (one per
  approval, token hashes only); `tool_calls` (the audit, arguments and results sealed, 90 days).

## Plan (not built yet)

Nothing is planned yet.
