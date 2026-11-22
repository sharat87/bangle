import { Plus, Save, Trash2 } from "lucide-preact"
import { useEffect, useRef, useState } from "preact/hooks"
import { api, useFetched } from "./api"
import { CopyButton, groupPath, iconSize, NameHeader, serverPath, ToolItems } from "./common"

// A group is internal/server/groups.go's groupJSON: name, servers (names),
// local_endpoint (what agents use: http to loopback), endpoint (the same
// through BASE_URL) and clients (how many harness grants use it). Agents
// see every member's tool, as <server>__<tool>.

function apiPath(name) {
	return `/api${groupPath(name)}`
}

// ---- /groups

export function GroupsPage() {
	const [groups, reload] = useFetched("/api/groups")
	return (
		<>
			<div class="row spread">
				<h1>Groups</h1>
				<a class="button" href="/add-group">
					<Plus size={iconSize} /> Add group
				</a>
			</div>
			<p class="muted">
				A group is a set of MCP servers served as one MCP endpoint. Point a project's coding agent at its group.
			</p>
			{groups.state === "loading" ? (
				<p class="muted">Loading…</p>
			) : groups.state === "failed" ? (
				<p class="error">Failed to load groups: {groups.error}</p>
			) : groups.data.length === 0 ? (
				<p class="muted">No groups yet.</p>
			) : (
				<table>
					<thead>
						<tr>
							<th>Group</th>
							<th>MCP servers</th>
							<th>Clients</th>
							<th />
						</tr>
					</thead>
					<tbody>
						{groups.data.map((g) => (
							<tr key={g.name} class="linked">
								<td>
									<a class="row-link" href={groupPath(g.name)}>
										{g.name}
									</a>
								</td>
								<td>
									{g.servers.length === 0 ? <span class="muted">none</span> : g.servers.join(", ")}
								</td>
								<td>{g.clients}</td>
								<td class="row-actions">
									<DeleteGroupButton name={g.name} onDelete={reload} compact />
								</td>
							</tr>
						))}
					</tbody>
				</table>
			)}
		</>
	)
}

function DeleteGroupButton({ name, onDelete, compact }) {
	const [error, setError] = useState(null)
	async function remove() {
		if (!confirm(`Delete group ${name}? Every client's access to it is revoked; the MCP servers stay.`)) return
		setError(null)
		try {
			await api(apiPath(name), { method: "DELETE" })
			onDelete()
		} catch (err) {
			setError(err.message)
		}
	}
	return (
		<>
			<button type="button" class="danger" title="Delete" aria-label="Delete" onClick={remove}>
				<Trash2 size={iconSize} />
				{!compact && " Delete"}
			</button>
			{error && <span class="error">{error}</span>}
		</>
	)
}

// ServerChoices is a checkbox per MCP server, from /api/servers.
function ServerChoices({ selected, onChange }) {
	const [servers] = useFetched("/api/servers")
	if (servers.state === "loading") return <p class="muted">Loading MCP servers…</p>
	if (servers.state === "failed") return <p class="error">Failed to load MCP servers: {servers.error}</p>
	if (servers.data.length === 0)
		return (
			<p class="muted">
				No MCP servers yet. <a href="/add">Add one</a> first.
			</p>
		)
	const toggle = (name) =>
		onChange(selected.includes(name) ? selected.filter((n) => n !== name) : [...selected, name])
	return (
		<div class="choices">
			{servers.data.map((s) => (
				<label key={s.name} class="inline">
					<input type="checkbox" checked={selected.includes(s.name)} onChange={() => toggle(s.name)} />
					{s.name}
					<span class="muted small">{s.url}</span>
				</label>
			))}
		</div>
	)
}

// ---- /add-group

export function AddGroupPage() {
	const [name, setName] = useState("")
	const [servers, setServers] = useState([])
	const [busy, setBusy] = useState(false)
	const [error, setError] = useState(null)
	const nameInput = useRef(null)
	useEffect(() => nameInput.current.focus(), [])

	async function submit(e) {
		e.preventDefault()
		setBusy(true)
		setError(null)
		try {
			const created = await api("/api/groups", { method: "POST", body: JSON.stringify({ name, servers }) })
			location.assign(groupPath(created.name))
		} catch (err) {
			setError(err.message)
			setBusy(false)
		}
	}

	return (
		<>
			<h1>Add a group</h1>
			<form class="stack" onSubmit={submit}>
				<label>
					Name
					<input
						ref={nameInput}
						required
						pattern="[a-z0-9\-]+"
						placeholder="myproject"
						value={name}
						onInput={(e) => setName(e.currentTarget.value)}
					/>
					<span class="muted small">
						a–z, 0–9 and -. Usually the project's name. Agents connect to{" "}
						<code>/g/{name || "name"}/mcp</code>.
					</span>
				</label>
				<fieldset>
					<legend>MCP servers</legend>
					<ServerChoices selected={servers} onChange={setServers} />
				</fieldset>
				<div class="row">
					<button type="submit" disabled={busy}>
						<Plus size={iconSize} /> {busy ? "Adding…" : "Add group"}
					</button>
					<a href="/groups">Cancel</a>
				</div>
				{error && <p class="error">{error}</p>}
			</form>
		</>
	)
}

// ---- /groups/<name>

export function GroupPage({ name }) {
	const [group, reload] = useFetched(apiPath(name))
	if (group.state === "loading")
		return (
			<>
				<h1>{name}</h1>
				<p class="muted">Loading…</p>
			</>
		)
	if (group.state === "failed")
		return (
			<>
				<h1>{name}</h1>
				<p class="error">
					Failed to load group {name}: {group.error}
				</p>
			</>
		)
	const g = group.data
	return (
		<>
			<NameHeader
				name={g.name}
				subtitle={
					<>
						{g.local_endpoint}
						<CopyButton text={g.local_endpoint} label="Copy endpoint" />
					</>
				}
				patchPath={apiPath(g.name)}
				pathFor={groupPath}
				note="The endpoint URL changes; agents need the new one."
			>
				<DeleteGroupButton name={g.name} onDelete={() => location.assign("/groups")} />
			</NameHeader>

			<ConnectAgent group={g} />

			<h2>MCP servers</h2>
			<Members group={g} onSaved={reload} />

			<h2>Tools</h2>
			<GroupTools name={g.name} />
		</>
	)
}

// ConnectAgent is how a coding agent connects: the endpoint is an ordinary
// remote MCP server that logs in with OAuth, so any harness that supports
// those works, and Bangle's consent page is where access is granted.
function ConnectAgent({ group }) {
	// The entry is <group>-bangle, so it can't clash with a harness's own
	// entry of the same name, like an older gateway's. Its tools
	// show up in Claude Code as mcp__<group>-bangle__<server>__<tool>.
	const entry = `${group.name}-bangle`
	const claude = `claude mcp add --transport http ${entry} ${group.local_endpoint}`
	return (
		<>
			<h2>Connect a coding agent</h2>
			<p>
				<strong>Claude Code.</strong> In the project's directory:
			</p>
			<div class="row command">
				<code>{claude}</code>
				<CopyButton text={claude} label="Copy command" />
			</div>
			<p class="muted">
				Then run <code>/mcp</code> in Claude Code, choose <code>{entry}</code> and Authenticate (or run{" "}
				<code>claude mcp login {entry}</code>). A browser tab
				opens on Bangle's consent page: approve it. Add <code>--scope project</code> to share the entry through the
				repo's <code>.mcp.json</code>.
			</p>
			<p>
				<strong>Any other MCP client</strong> that supports remote servers with OAuth: give it{" "}
				<code>{group.local_endpoint}</code>. It finds Bangle's login on its own (the endpoint answers 401 with
				where to log in), registers itself, and sends you to the same consent page.
			</p>
			<p class="muted">
				The endpoint is plain http to this machine on purpose. The same group is at <code>{group.endpoint}</code>,
				but Node- and Rust-based agents don't trust Caddy's local certificate authority unless told to (
				<code>NODE_EXTRA_CA_CERTS</code>).
			</p>
			<p class="muted">
				Approved clients are listed under <a href="/clients">Connected Clients</a>, where you can revoke them. Access is
				for this group only; {group.clients === 1 ? "1 client has" : `${group.clients} clients have`} it now.
			</p>
		</>
	)
}

function Members({ group, onSaved }) {
	const [selected, setSelected] = useState(group.servers)
	const [busy, setBusy] = useState(false)
	const [error, setError] = useState(null)
	const changed = [...selected].sort().join() !== [...group.servers].sort().join()

	async function save(e) {
		e.preventDefault()
		setBusy(true)
		setError(null)
		try {
			await api(apiPath(group.name), { method: "PATCH", body: JSON.stringify({ servers: selected }) })
			onSaved()
		} catch (err) {
			setError(err.message)
		}
		setBusy(false)
	}

	return (
		<form class="stack" onSubmit={save}>
			<ServerChoices selected={selected} onChange={setSelected} />
			<div class="row">
				<button type="submit" disabled={busy || !changed}>
					<Save size={iconSize} /> Save
				</button>
				<span class="muted">Connected agents see the change when they next list tools, usually on reconnect.</span>
				{error && <span class="error">{error}</span>}
			</div>
		</form>
	)
}

function GroupTools({ name }) {
	const [result] = useFetched(`${apiPath(name)}/tools`)
	if (result.state === "loading") return <p class="muted">Listing tools…</p>
	if (result.state === "failed") return <p class="error">Failed to list tools: {result.error}</p>
	const { tools, failures } = result.data
	return (
		<>
			{failures.map((f) => (
				<p key={f.server} class="error">
					<a href={serverPath(f.server)}>{f.server}</a> is left out: {f.error}
				</p>
			))}
			{tools.length === 0 ? (
				<p class="muted">This group serves no tools.</p>
			) : (
				<>
					<p class="muted">
						{tools.length} tools, live from the MCP servers. Agents see every one of them.
					</p>
					<ToolItems tools={tools} />
				</>
			)}
		</>
	)
}
