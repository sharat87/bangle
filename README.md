# Bangle

A local MCP gateway for one person's machine.

Log in to Linear, Notion, PostHog, Railway and other remote MCP servers once, in Bangle. Put them in
**groups**, say one per project, and point each agent harness (Claude Code, Codex, Pi, …) at its
group's endpoint instead of re-authorizing every server in every harness. Remote servers only, for now:
streamable HTTP with an OAuth login and dynamic client registration.
A group's endpoint lists every member's tool as `<server>__<tool>`, read live from the servers.

Every tool call, with its arguments and result, is recorded and browsable in the web UI.

## Trust model: read this first

Bangle holds the OAuth tokens for your real accounts, and **its web UI and API have no login.** What
keeps them yours:

- It listens on loopback only and refuses to start on anything else.
- It refuses foreign `Host` headers (DNS rebinding) and cross-origin writes, so a web page in your
  browser can't drive it.
- Agents reach a group's endpoint only with a token you approved for that group in the browser.

What that doesn't cover: **any process on the machine can use the UI's API**, and with it every login
Bangle holds. That includes software running as you, and other users' processes on a shared
machine. Only run Bangle on a machine that's yours alone, and don't expose its port: no tunnels, and
in Docker, publish it on `127.0.0.1` only.

## Running it

Docker needs nothing else. Building from source needs Go (the version in `go.mod`), Node, pnpm and,
for `make`, tmux.

**With Docker,** from the one published image, `latest`, built from every commit to main:

```sh
docker run -d --name bangle -p 127.0.0.1:7060:7060 \
  -e SECRETS_KEY="$(openssl rand -base64 32)" -v bangle-data:/data \
  ghcr.io/sharat87/bangle
```

Keep that `SECRETS_KEY`: the container needs the same one every time it starts. The `127.0.0.1:` in `-p`
is what keeps Bangle off your network. Add `-e BASE_URL=…` if you reach it through a proxy.

**From source,** in tmux:

```sh
cp .env.example .env               # then set SECRETS_KEY
make                               # builds the UI, (re)starts Bangle in tmux
open http://127.0.0.1:7060
```

`make` runs it in a window of the `daemons` tmux server (`tmux -L daemons attach`) and logs to
`.logs/server.log`. Run it again to restart.

| Command | What it does |
| --- | --- |
| `make` | Format, build the frontend, (re)start Bangle in tmux |
| `make build-frontend` | Rebuild the UI; it's served live from disk, so just reload |
| `make dev-server` | A throwaway instance on `127.0.0.1:7061` with its own DB, for trying changes |
| `make test` | gofmt check, vet, frontend build, `go test ./...` |
| `make build` | `bin/bangle`, one binary with the frontend embedded, to run however you keep things running |

### Settings

All settings are environment variables; `.env.example` describes each one.

| Variable | |
| --- | --- |
| `DB_PATH` | The SQLite file. Everything Bangle knows is in it. |
| `BASE_URL` | What you open in the browser, e.g. `http://127.0.0.1:7060`. Upstream OAuth redirects come back to `BASE_URL/oauth/upstream/callback`. If a provider insists on https, put a local TLS proxy such as Caddy in front and use its origin. |
| `SECRETS_KEY` | Seals tokens and keys in the DB. Losing it means logging in to every server again. |

## Using it

1. **Add an MCP server** by its URL and click Connect to log in.
2. **Make a group** of servers. Its page shows the command to add it to Claude Code, e.g.
   `claude mcp add --transport http myproject-bangle http://127.0.0.1:7060/g/myproject/mcp`.
   Any MCP client that does OAuth works: it logs in to Bangle and you approve it in the browser.

`AGENTS.md` documents the architecture and the reasoning behind it.

## License

MIT
