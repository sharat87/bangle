import { Plug, Plus, RefreshCw, Save, Search, Trash2, Unplug } from "lucide-preact"
import { useEffect, useRef, useState } from "preact/hooks"
import { api, useFetched } from "./api"
import { groupPath, iconSize, NameHeader, serverPath, ToolItems } from "./common"

// A server is internal/server/servers.go's serverJSON: name, url, scopes,
// client_id, has_client_secret, status ("connected" | "needs_login" |
// "error"), error, logged_in, token_expires_at, refreshable. A tool is
// upstream_pool.go's toolJSON: name, title, description, inputSchema.
// A tool's own page, where it can be called, is toolcall.jsx.

function apiPath(name) {
	return `/api${serverPath(name)}`
}

function Status({ server }) {
	switch (server.status) {
		case "connected":
			return <span class="ok">Connected</span>
		case "needs_login":
			return <span class="muted">Needs login</span>
		default:
			return <span class="error">Error: {server.error}</span>
	}
}

// hasTools is whether listing the server's tools can work: a login.
function hasTools(server) {
	return server.status === "connected"
}

// ---- /

export function ServersPage() {
	const [servers, reload] = useFetched("/api/servers")
	return (
		<>
			<div class="row spread">
				<h1>MCP Servers</h1>
				<a class="button" href="/add">
					<Plus size={iconSize} /> Add MCP server
				</a>
			</div>
			{servers.state === "loading" ? (
				<p class="muted">Loading…</p>
			) : servers.state === "failed" ? (
				<p class="error">Failed to load MCP servers: {servers.error}</p>
			) : servers.data.length === 0 ? (
				<p class="muted">No MCP servers yet.</p>
			) : (
				<table>
					<thead>
						<tr>
							<th>MCP server</th>
							<th>Status</th>
							<th>Tools</th>
							<th />
						</tr>
					</thead>
					<tbody>
						{servers.data.map((s) => (
							<tr key={s.name} class="linked">
								<td>
									<a class="row-link" href={serverPath(s.name)}>
										{s.name}
									</a>
								</td>
								<td>
									<Status server={s} />
								</td>
								<td>
									{hasTools(s) ? <ToolCount name={s.name} /> : <span class="muted">—</span>}
								</td>
								<td class="row-actions">
									<ServerActions server={s} onChange={reload} onDelete={reload} compact />
								</td>
							</tr>
						))}
					</tbody>
				</table>
			)}
		</>
	)
}

// ToolCount lists the server's tools live, only to count them: it is the
// same check as the server page, so a count is proof the server works.
function ToolCount({ name }) {
	const [tools] = useFetched(`${apiPath(name)}/tools`)
	if (tools.state === "loading") return <span class="muted">…</span>
	if (tools.state === "failed") return <span class="error small">{tools.error}</span>
	return <span>{tools.data.length}</span>
}

// ServerActions are a server's controls, shared by the table and the server
// page. compact (the table) shows the secondary ones as icons.
//
// The login buttons follow logged_in, not status: a server in
// "error" may or may not still hold a login (a refused refresh drops it; a
// failed reconnect keeps the old one), and only a stored login can be
// reconnected or disconnected. Connect and Reconnect are form posts, not
// fetches: the answer is a redirect to the provider's login page, then back
// to the server's page.
function ServerActions({ server, onChange, onDelete, compact }) {
	const [error, setError] = useState(null)

	async function act(confirmText, method, path, done) {
		if (confirmText && !confirm(confirmText)) return
		setError(null)
		try {
			await api(path, { method })
			done()
		} catch (err) {
			setError(err.message)
		}
	}

	return (
		<div class="row">
			<form method="post" action="/oauth/upstream/start">
				<input type="hidden" name="server" value={server.name} />
				{server.logged_in ? (
					<button type="submit" class="quiet">
						<RefreshCw size={iconSize} /> Reconnect
					</button>
				) : (
					<button type="submit">
						<Plug size={iconSize} /> Connect
					</button>
				)}
			</form>
			{server.logged_in && (
				<button
					type="button"
					class="quiet"
					title="Disconnect"
					aria-label="Disconnect"
					onClick={() =>
						act(
							`Disconnect ${server.name}? Bangle forgets its login; the server stays.`,
							"DELETE",
							`${apiPath(server.name)}/login`,
							onChange,
						)
					}
				>
					<Unplug size={iconSize} />
					{!compact && " Disconnect"}
				</button>
			)}
			<button
				type="button"
				class="danger"
				title="Delete"
				aria-label="Delete"
				onClick={() =>
					act(`Delete ${server.name}? It leaves every group it is in.`, "DELETE", apiPath(server.name), onDelete)
				}
			>
				<Trash2 size={iconSize} />
				{!compact && " Delete"}
			</button>
			{error && <span class="error">{error}</span>}
		</div>
	)
}

// ---- /add

export function AddServerPage() {
	const [name, setName] = useState("")
	const [url, setURL] = useState("")
	const [scopes, setScopes] = useState("")
	const [clientID, setClientID] = useState("")
	const [clientSecret, setClientSecret] = useState("")
	const [busy, setBusy] = useState(false)
	const [error, setError] = useState(null)
	// The last URL lookup (lookup.go): null before one, else
	// {state: "loading"} | {state: "failed", error} | {state: "loaded", name, data}.
	const [lookup, setLookup] = useState(null)
	// Whether lookup is on: CONTEXT7_API_KEY is optional.
	const [health] = useFetched("/api/health")
	// Focus from an effect: the autofocus attribute is only honoured for
	// markup present when the page loads, not for what Preact renders after.
	const nameInput = useRef(null)
	useEffect(() => nameInput.current.focus(), [])

	async function findURL() {
		const asked = name
		setLookup({ state: "loading" })
		try {
			const data = await api(`/api/lookup?name=${encodeURIComponent(asked)}`)
			setLookup({ state: "loaded", name: asked, data })
			if (data.candidates.length > 0) setURL(data.candidates[0].url)
		} catch (err) {
			setLookup({ state: "failed", error: err.message })
		}
	}

	async function submit(e) {
		e.preventDefault()
		setBusy(true)
		setError(null)
		try {
			const body = { name, url, scopes, client_id: clientID, client_secret: clientSecret }
			const created = await api("/api/servers", { method: "POST", body: JSON.stringify(body) })
			location.assign(serverPath(created.name))
		} catch (err) {
			setError(err.message)
			setBusy(false)
		}
	}

	return (
		<>
			<h1>Add an MCP server</h1>
			<form class="stack" onSubmit={submit}>
				<label>
					Name
					<input
						ref={nameInput}
						required
						pattern="[a-z0-9\-]+"
						placeholder="linear"
						value={name}
						onInput={(e) => setName(e.currentTarget.value)}
					/>
					<span class="muted small">
						a–z, 0–9 and -. Tools are named <code>{name || "name"}__tool</code>.
					</span>
				</label>
				<label>
					MCP URL
					<input
						required
						type="url"
						class="big"
						placeholder="https://mcp.linear.app/mcp"
						value={url}
						onInput={(e) => setURL(e.currentTarget.value)}
					/>
				</label>
				<FindURL health={health} name={name} lookup={lookup} findURL={findURL} />
				<LookupResult lookup={lookup} url={url} setURL={setURL} />
				<details>
					<summary>
						Scopes and client <span class="muted">(optional)</span>
					</summary>
					<div class="stack">
						<label>
							Scopes
							<input
								placeholder="the server's own list"
								value={scopes}
								onInput={(e) => setScopes(e.currentTarget.value)}
							/>
							<span class="muted small">
								Space-separated. offline_access is added whenever the authorization server offers it.
							</span>
						</label>
						<label>
							Client ID
							<input
								placeholder="dynamic registration"
								value={clientID}
								onInput={(e) => setClientID(e.currentTarget.value)}
							/>
							<span class="muted small">
								Only for a provider without dynamic client registration. Register the redirect URI{" "}
								<code>{location.origin}/oauth/upstream/callback</code>.
							</span>
						</label>
						<label>
							Client secret
							<input
								type="password"
								autocomplete="off"
								value={clientSecret}
								onInput={(e) => setClientSecret(e.currentTarget.value)}
							/>
						</label>
					</div>
				</details>
				<div class="row">
					<button type="submit" disabled={busy}>
						<Plus size={iconSize} /> {busy ? "Adding…" : "Add MCP server"}
					</button>
					<a href="/">Cancel</a>
				</div>
				{error && <p class="error">{error}</p>}
			</form>
		</>
	)
}

// FindURL is the Add page's "Find the URL" button, offered only when the
// server has a Context7 key (/api/health's lookup).
function FindURL({ health, name, lookup, findURL }) {
	if (health.state === "loading") return null
	if (health.state === "failed") return <p class="error">Couldn't check whether lookup is on: {health.error}</p>
	if (!health.data.lookup) return null
	return (
		<div class="row">
			<button type="button" class="quiet" disabled={!name || lookup?.state === "loading"} onClick={findURL}>
				<Search size={iconSize} />{" "}
				{lookup?.state === "loading" ? "Looking up…" : `Find the URL for “${name || "name"}”`}
			</button>
			<span class="muted small">Searches vendors' docs through Context7.</span>
		</div>
	)
}

// LookupResult shows a lookup's candidates as radio buttons that set the URL
// field. Every one has passed Connect's discovery; whether it is the vendor's
// own server is for me to judge from its domain and its docs page.
function LookupResult({ lookup, url, setURL }) {
	if (!lookup || lookup.state === "loading") return null
	if (lookup.state === "failed") return <p class="error">{lookup.error}</p>
	const { candidates, checked } = lookup.data
	if (candidates.length === 0) {
		return (
			<p class="muted">
				No URL in the docs for “{lookup.name}” answers as an OAuth MCP server ({checked} checked). Enter it yourself.
			</p>
		)
	}
	return (
		<fieldset class="choices">
			<legend>Found for “{lookup.name}”</legend>
			{candidates.map((c) => (
				<label class="inline" key={c.url}>
					<input type="radio" name="candidate" checked={url === c.url} onChange={() => setURL(c.url)} />
					<span>
						<code>{c.url}</code>{" "}
						<span class={c.verified ? "muted small" : "error small"}>
							{c.verified ? "on the docs' own domain" : "not on the docs' domain: check it's the vendor's"}
							{" · from "}
							{c.sources.map((src, i) => (
								<span key={src}>
									{i > 0 && ", "}
									<a href={src} target="_blank" rel="noreferrer">
										{src.replace(/^https:\/\//, "")}
									</a>
								</span>
							))}
						</span>
					</span>
				</label>
			))}
		</fieldset>
	)
}

// ---- /servers/<name>

export function ServerPage({ name }) {
	const [server, reload] = useFetched(apiPath(name))
	if (server.state === "loading")
		return (
			<>
				<h1>{name}</h1>
				<p class="muted">Loading…</p>
			</>
		)
	if (server.state === "failed")
		return (
			<>
				<h1>{name}</h1>
				<p class="error">
					Failed to load {name}: {server.error}
				</p>
			</>
		)
	return <ServerDetail server={server.data} reload={reload} />
}

function ServerDetail({ server, reload }) {
	const expires = server.token_expires_at ? new Date(server.token_expires_at).toLocaleString() : null
	return (
		<>
			<NameHeader
				name={server.name}
				subtitle={server.url}
				patchPath={apiPath(server.name)}
				pathFor={serverPath}
				note="Tool names change; the login is kept."
			/>

			<h2>Login</h2>
			<p>
				<Status server={server} />
				{server.status === "connected" && (
					<>
						<br />
						<span class="muted">
							{expires ? `Access token expires ${expires}. ` : "No stated expiry. "}
							{server.refreshable
								? "It refreshes on its own."
								: "There is no refresh token: log in again when it expires."}
						</span>
					</>
				)}
			</p>
			<ServerActions server={server} onChange={reload} onDelete={() => location.assign("/")} />

			<h2>Groups</h2>
			<ServerGroups name={server.name} />

			<h2>Tools</h2>
			{hasTools(server) ? (
				<ToolList name={server.name} />
			) : (
				<p class="muted">Connect to list this server's tools.</p>
			)}

			<h2>Settings</h2>
			<p>
				Scopes:{" "}
				{server.scopes ? <code>{server.scopes}</code> : <span class="muted">the server's own list</span>}
				<br />
				Client:{" "}
				{server.client_id ? (
					<>
						<code>{server.client_id}</code>
						{server.has_client_secret && <span class="muted"> (with a secret)</span>}
					</>
				) : (
					<span class="muted">dynamic registration</span>
				)}
			</p>
		</>
	)
}

// ServerGroups is a checkbox per group, from /api/groups, checked where the
// server is a member. Save sets exactly those memberships
// (PUT /api/servers/{name}/groups); the groups' other members are untouched.
function ServerGroups({ name }) {
	const [groups, reload] = useFetched("/api/groups")
	if (groups.state === "loading") return <p class="muted">Loading groups…</p>
	if (groups.state === "failed") return <p class="error">Failed to load groups: {groups.error}</p>
	if (groups.data.length === 0)
		return (
			<p class="muted">
				No groups yet. <a href="/add-group">Add one</a>.
			</p>
		)
	const current = groups.data.filter((g) => g.servers.includes(name)).map((g) => g.name)
	// Keyed by the saved memberships, so a save (or a rename) starts the
	// checkboxes over from what the server now has.
	return <ServerGroupsForm key={current.join()} name={name} groups={groups.data} current={current} onSaved={reload} />
}

function ServerGroupsForm({ name, groups, current, onSaved }) {
	const [selected, setSelected] = useState(current)
	const [busy, setBusy] = useState(false)
	const [error, setError] = useState(null)
	const changed = [...selected].sort().join() !== [...current].sort().join()
	const toggle = (g) => setSelected(selected.includes(g) ? selected.filter((n) => n !== g) : [...selected, g])

	async function save(e) {
		e.preventDefault()
		setBusy(true)
		setError(null)
		try {
			await api(`${apiPath(name)}/groups`, { method: "PUT", body: JSON.stringify({ groups: selected }) })
			onSaved()
		} catch (err) {
			setError(err.message)
		}
		setBusy(false)
	}

	return (
		<form class="stack" onSubmit={save}>
			<div class="choices">
				{groups.map((g) => (
					<label key={g.name} class="inline">
						<input type="checkbox" checked={selected.includes(g.name)} onChange={() => toggle(g.name)} />
						<a href={groupPath(g.name)}>{g.name}</a>
						<span class="muted small">
							{g.servers.length === 1 ? "1 MCP server" : `${g.servers.length} MCP servers`}
						</span>
					</label>
				))}
			</div>
			<div class="row">
				<button type="submit" disabled={busy || !changed}>
					<Save size={iconSize} /> Save
				</button>
				{error && <span class="error">{error}</span>}
			</div>
		</form>
	)
}

function ToolList({ name }) {
	const [tools] = useFetched(`${apiPath(name)}/tools`)
	if (tools.state === "loading") return <p class="muted">Listing tools…</p>
	if (tools.state === "failed") return <p class="error">Failed to list tools: {tools.error}</p>
	if (tools.data.length === 0) return <p class="muted">This server lists no tools.</p>
	return (
		<>
			<p class="muted">{tools.data.length} tools, live from the server.</p>
			<ToolItems tools={tools.data} prefix={`${name}__`} />
		</>
	)
}
