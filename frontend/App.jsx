import { Boxes, History, KeyRound, Server } from "lucide-preact"
import { useFetched } from "./api"
import { CallPage, CallsPage } from "./calls"
import { ClientsPage, ConsentPage } from "./clients"
import { iconSize } from "./common"
import { AddGroupPage, GroupPage, GroupsPage } from "./groups"
import { AddServerPage, ServerPage, ServersPage } from "./servers"
import { ToolPage } from "./toolcall"

// Routing is the path, read once per page load: every navigation is an
// <a href> (or a form submit), so there is no client-side router.
//
//   /                 MCP servers: a table with status, tool count, actions
//   /add              the add-MCP-server form
//   /servers/<name>   one MCP server: login or process, tools, settings
//   /groups           groups: sets of MCP servers, one endpoint each
//   /add-group        the add-group form
//   /groups/<name>    one group: how to connect an agent, members, tools
//   /clients          approved coding agents, with revoke
//   /calls            the audit: every tool call, newest first, filterable
//   /calls/<id>       one call, with its arguments and result
//   /consent/<id>     the consent page a harness's login lands on
const routes = [
	[/^\/$/, () => <ServersPage />, "servers"],
	[/^\/add$/, () => <AddServerPage />, "servers"],
	[/^\/servers\/([^/]+)$/, (name) => <ServerPage name={name} />, "servers"],
	[/^\/servers\/([^/]+)\/tools\/([^/]+)$/, (server, tool) => <ToolPage server={server} tool={tool} />, "servers"],
	[/^\/groups$/, () => <GroupsPage />, "groups"],
	[/^\/add-group$/, () => <AddGroupPage />, "groups"],
	[/^\/groups\/([^/]+)$/, (name) => <GroupPage name={name} />, "groups"],
	[/^\/clients$/, () => <ClientsPage />, "clients"],
	[/^\/calls$/, () => <CallsPage />, "calls"],
	[/^\/calls\/([^/]+)$/, (id) => <CallPage id={id} />, "calls"],
	[/^\/consent\/([^/]+)$/, (id) => <ConsentPage id={id} />, null],
]

// The nav: pages under a heading per section.
const navSections = [
	[
		"MCP",
		[
			["servers", "/", "Servers", Server],
			["groups", "/groups", "Groups", Boxes],
			["clients", "/clients", "Connected Clients", KeyRound],
			["calls", "/calls", "Call History", History],
		],
	],
]

export function App() {
	const path = location.pathname
	let page = <NotFound path={path} />
	let section = null
	for (const [pattern, render, sec] of routes) {
		const m = path.match(pattern)
		if (m) {
			page = render(...m.slice(1).map(decodeURIComponent))
			section = sec
			break
		}
	}
	// The layout is the whole viewport: the sidebar (brand, nav, footer) and
	// the main area each scroll on their own (.shell in styles.css).
	return (
		<div class="shell">
			<aside class="sidebar">
				<a class="brand" href="/">
					<img src="/static/favicon.svg" alt="" width="22" height="22" />
					Bangle
				</a>
				<nav>
					{navSections.map(([heading, links]) => (
						<section key={heading}>
							<h2>{heading}</h2>
							{links.map(([sec, href, label, Icon]) => (
								<a key={sec} href={href} aria-current={sec === section ? "page" : undefined}>
									<Icon size={iconSize} />
									{label}
								</a>
							))}
						</section>
					))}
				</nav>
				<footer class="foot">
					<span>Built by Shri</span>
					<Build />
				</footer>
			</aside>
			<main>
				<div class="page">{page}</div>
			</main>
		</div>
	)
}

function NotFound({ path }) {
	return (
		<>
			<h1>Not found</h1>
			<p>
				Nothing lives at <code>{path}</code>. <a href="/">Back to MCP servers</a>
			</p>
		</>
	)
}

// Build is the commit the server is running, from /api/health: "dev" when
// the build was not stamped with one, "(modified)" when the working tree had
// uncommitted changes.
function Build() {
	const [health] = useFetched("/api/health")
	if (health.state === "loading") return <span>Running …</span>
	if (health.state === "failed") return <span class="error">Server unreachable: {health.error}</span>
	const { commit, modified } = health.data.build
	if (!commit) return <span>Running a dev build</span>
	return (
		<span>
			Running commit <code>{commit.slice(0, 7)}</code>
			{modified && " (modified)"}
		</span>
	)
}
