import { Ban, Check, X } from "lucide-preact"
import { useState } from "preact/hooks"
import { api, useFetched } from "./api"
import { groupPath, iconSize, Time } from "./common"

// ---- /clients
//
// Every approval a harness got on the consent page (harness_oauth.go's
// grants): which client, for which group, when, and when it was last used.

export function ClientsPage() {
	const [clients, reload] = useFetched("/api/clients")
	return (
		<>
			<h1>Connected Clients</h1>
			<p class="muted">
				Coding agents you approved on the consent page. Each one can use one group. Revoking cuts it off at
				once; it can ask again.
			</p>
			{clients.state === "loading" ? (
				<p class="muted">Loading…</p>
			) : clients.state === "failed" ? (
				<p class="error">Failed to load clients: {clients.error}</p>
			) : clients.data.length === 0 ? (
				<p class="muted">
					No clients yet. A group's page says how to <a href="/groups">connect one</a>.
				</p>
			) : (
				<table>
					<thead>
						<tr>
							<th>Client</th>
							<th>Group</th>
							<th>Approved</th>
							<th>Last used</th>
							<th />
						</tr>
					</thead>
					<tbody>
						{clients.data.map((c) => (
							<tr key={c.id}>
								<td>{c.client_name}</td>
								<td>
									<a href={groupPath(c.group)}>{c.group}</a>
								</td>
								<td>
									<Time value={c.created_at} />
								</td>
								<td>
									<Time value={c.last_used_at} />
								</td>
								<td>
									<RevokeButton client={c} onRevoke={reload} />
								</td>
							</tr>
						))}
					</tbody>
				</table>
			)}
		</>
	)
}

function RevokeButton({ client, onRevoke }) {
	const [error, setError] = useState(null)
	async function revoke() {
		if (!confirm(`Revoke ${client.client_name}'s access to ${client.group}?`)) return
		setError(null)
		try {
			await api(`/api/clients/${client.id}`, { method: "DELETE" })
			onRevoke()
		} catch (err) {
			setError(err.message)
		}
	}
	return (
		<>
			<button type="button" class="danger" title="Revoke" aria-label="Revoke" onClick={revoke}>
				<Ban size={iconSize} />
			</button>
			{error && <span class="error">{error}</span>}
		</>
	)
}

// ---- /consent/<id>
//
// Where /oauth/authorize sends the browser. Approve and Deny are one form
// post to /oauth/consent, which redirects back to the harness.

export function ConsentPage({ id }) {
	const [consent] = useFetched(`/api/consent/${encodeURIComponent(id)}`)
	if (consent.state === "loading") return <p class="muted">Loading…</p>
	if (consent.state === "failed")
		return (
			<>
				<h1>Allow access?</h1>
				<p class="error">{consent.error}</p>
			</>
		)
	const c = consent.data
	return (
		<>
			<h1>Allow access?</h1>
			<p>
				<strong>{c.client_name}</strong> wants to use the MCP servers in group{" "}
				<a href={groupPath(c.group)}>{c.group}</a>, with your logins.
			</p>
			<p class="muted">
				Its endpoint is <code>{c.endpoint}</code>. After you choose, the browser goes back to{" "}
				<code>{c.redirect_host}</code>. Only approve a login you just started from a coding agent.
			</p>
			<form method="post" action="/oauth/consent" class="row">
				<input type="hidden" name="id" value={c.id} />
				<button type="submit" name="decision" value="approve">
					<Check size={iconSize} /> Approve
				</button>
				<button type="submit" name="decision" value="deny" class="quiet">
					<X size={iconSize} /> Deny
				</button>
			</form>
		</>
	)
}
