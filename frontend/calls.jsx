import { useFetched } from "./api"
import { Time } from "./common"
import { JSONView, parseJSONContainer } from "./json"

// A call is internal/server/audit.go's callJSON: id, created_at,
// client_name, group_name, server_name, tool, outcome ("running" | "ok" |
// "tool_error" | "error" | "unreachable"), error, duration_ms, and on its
// own page, arguments and result.

const filterNames = [
	["client", "client"],
	["group", "group"],
	["server", "MCP server"],
]

function callsHref(params) {
	const q = new URLSearchParams(Object.entries(params).filter(([, v]) => v))
	const s = q.toString()
	return s ? `/calls?${s}` : "/calls"
}

export function Outcome({ call }) {
	switch (call.outcome) {
		case "ok":
			return <span class="ok">OK</span>
		case "running":
			return <span class="muted">Running</span>
		case "tool_error":
			return <span class="error">Tool error</span>
		case "unreachable":
			return <span class="error">Unreachable</span>
		default:
			return <span class="error">Error</span>
	}
}

export function duration(ms) {
	if (ms === null || ms === undefined) return "—"
	return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`
}

// ---- /calls

export function CallsPage() {
	const params = new URLSearchParams(location.search)
	const filters = Object.fromEntries(filterNames.map(([key]) => [key, params.get(key) || ""]))
	const before = params.get("before") || ""
	const [calls] = useFetched(`/api${callsHref({ ...filters, before })}`)
	const active = filterNames.filter(([key]) => filters[key])
	return (
		<>
			<h1>Call History</h1>
			<p class="muted">
				Every tool call coding agents made through Bangle, and every call made from a tool's page here, with its arguments and result. Kept for 90 days.
			</p>
			{active.length > 0 && (
				<p>
					Only calls with{" "}
					{active.map(([key, label], i) => (
						<span key={key}>
							{i > 0 && " and "}
							{label} <strong>{filters[key]}</strong>
						</span>
					))}
					. <a href="/calls">Show all</a>
				</p>
			)}
			{calls.state === "loading" ? (
				<p class="muted">Loading…</p>
			) : calls.state === "failed" ? (
				<p class="error">Failed to load calls: {calls.error}</p>
			) : calls.data.calls.length === 0 ? (
				<p class="muted">{before || active.length > 0 ? "No calls match." : "No calls yet."}</p>
			) : (
				<>
					<table>
						<thead>
							<tr>
								<th>Tool</th>
								<th>When</th>
								<th>Client</th>
								<th>Group</th>
								<th>Outcome</th>
								<th>Took</th>
							</tr>
						</thead>
						<tbody>
							{calls.data.calls.map((c) => (
								<tr key={c.id} class="linked">
									<td>
										<a class="row-link" href={`/calls/${c.id}`}>
											<code>
												{c.server_name}__{c.tool}
											</code>
										</a>
									</td>
									<td>
										<Time value={c.created_at} />
									</td>
									<td>{c.client_name}</td>
									<td>{c.group_name || <span class="muted">—</span>}</td>
									<td>
										<Outcome call={c} />
									</td>
									<td>{duration(c.duration_ms)}</td>
								</tr>
							))}
						</tbody>
					</table>
					{calls.data.more && (
						<p>
							<a href={callsHref({ ...filters, before: calls.data.calls.at(-1).id })}>Older calls</a>
						</p>
					)}
				</>
			)}
		</>
	)
}

// ---- /calls/<id>

export function CallPage({ id }) {
	const [call] = useFetched(`/api/calls/${encodeURIComponent(id)}`)
	if (call.state === "loading") return <p class="muted">Loading…</p>
	if (call.state === "failed")
		return (
			<>
				<h1>Call {id}</h1>
				<p class="error">{call.error}</p>
			</>
		)
	const c = call.data
	return (
		<>
			<h1>
				<code class="title-code">
					{c.server_name}__{c.tool}
				</code>
			</h1>
			<p class="muted title-sub">
				<Time value={c.created_at} />
			</p>
			<p>
				Outcome: <Outcome call={c} />, took {duration(c.duration_ms)}
				{c.error && (
					<>
						<br />
						<span class="error">{c.error}</span>
					</>
				)}
				<br />
				Client: <a href={callsHref({ client: c.client_name })}>{c.client_name}</a>
				<br />
				Group:{" "}
				{c.group_name ? (
					<a href={callsHref({ group: c.group_name })}>{c.group_name}</a>
				) : (
					<span class="muted">none: made from the tool's page</span>
				)}
				<br />
				MCP server: <a href={callsHref({ server: c.server_name })}>{c.server_name}</a>
			</p>
			<p class="muted small">The links list every call with the same client, group or MCP server.</p>

			<h2>Arguments</h2>
			<JSONView value={c.arguments} />

			<h2>Result</h2>
			{c.result === undefined ? (
				<p class="muted">No result yet: the call is still running.</p>
			) : (
				<Result result={c.result} />
			)}
		</>
	)
}

// Result shows a tool result's text content as text, which is how the model
// read it, and the whole result as JSON below. A text that is a JSON object
// or array is shown pretty-printed and highlighted instead; anything else is
// shown exactly as it came.
export function Result({ result }) {
	const texts = (result?.content ?? []).filter((item) => item.type === "text").map((item) => item.text)
	return (
		<>
			{texts.map((text, i) => {
				const parsed = parseJSONContainer(text)
				return parsed === undefined ? (
					<pre key={i} class="text">
						{text}
					</pre>
				) : (
					<JSONView key={i} value={parsed} />
				)
			})}
			<details open={texts.length === 0}>
				<summary>Raw result</summary>
				<JSONView value={result} />
			</details>
		</>
	)
}

