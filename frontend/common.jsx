import { Check, Copy, Pencil, Play, X } from "lucide-preact"
import { useEffect, useState } from "preact/hooks"
import { api } from "./api"
import { MarkdownBlocks, splitMarkdown } from "./markdown"

// Pieces shared by the pages.
//
// Every button has an icon (lucide-preact, inline SVG, only the ones
// imported are bundled). Text is optional where the icon is obvious; an
// icon-only button carries title and aria-label instead.
export const iconSize = 16

export function serverPath(name) {
	return `/servers/${encodeURIComponent(name)}`
}

export function toolPath(server, tool) {
	return `${serverPath(server)}/tools/${encodeURIComponent(tool)}`
}

export function groupPath(name) {
	return `/groups/${encodeURIComponent(name)}`
}

// Time shows a CURRENT_TIMESTAMP value from SQLite ("YYYY-MM-DD HH:MM:SS",
// UTC) as a relative time ("5 minutes ago"), with the full ISO timestamp in
// local time, offset included, as its tooltip. It re-renders every minute,
// so a page left open doesn't keep saying "just now". null is "never".
export function Time({ value }) {
	const [, setTick] = useState(0)
	useEffect(() => {
		const id = setInterval(() => setTick((n) => n + 1), 60_000)
		return () => clearInterval(id)
	}, [])
	if (!value) return <span class="muted">never</span>
	const date = new Date(`${value.replace(" ", "T")}Z`)
	return (
		<time datetime={date.toISOString()} title={localISO(date)}>
			{relativeTime(date)}
		</time>
	)
}

const relative = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" })

const relativeUnits = [
	["year", 365 * 24 * 3600],
	["month", 30 * 24 * 3600],
	["week", 7 * 24 * 3600],
	["day", 24 * 3600],
	["hour", 3600],
	["minute", 60],
]

function relativeTime(date) {
	const seconds = (date.getTime() - Date.now()) / 1000
	if (Math.abs(seconds) < 60) return "just now"
	for (const [unit, size] of relativeUnits) {
		if (Math.abs(seconds) >= size) return relative.format(Math.round(seconds / size), unit)
	}
	return "just now"
}

// localISO is ISO 8601 in local time with its offset: 2026-11-06T15:47:19+05:30.
function localISO(date) {
	const pad = (n) => String(Math.floor(Math.abs(n))).padStart(2, "0")
	const offset = -date.getTimezoneOffset()
	const sign = offset >= 0 ? "+" : "-"
	return (
		`${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
		`T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}` +
		`${sign}${pad(offset / 60)}:${pad(offset % 60)}`
	)
}

// CopyButton copies text to the clipboard, and says so for a moment.
export function CopyButton({ text, label = "Copy" }) {
	const [copied, setCopied] = useState(false)
	const [error, setError] = useState(null)
	async function copy() {
		try {
			await navigator.clipboard.writeText(text)
			setError(null)
			setCopied(true)
			setTimeout(() => setCopied(false), 1500)
		} catch (err) {
			setError(err.message)
		}
	}
	return (
		<>
			<button type="button" class="quiet" title={label} aria-label={label} onClick={copy}>
				{copied ? <Check size={iconSize} /> : <Copy size={iconSize} />}
			</button>
			{error && <span class="error">Copy failed: {error}</span>}
		</>
	)
}

// NameHeader is a page's <h1> and subtitle, with a Rename button (and any
// other buttons, as children) under them. Rename swaps in an inline form
// that PATCHes {name} to patchPath and reloads at pathFor(new name).
export function NameHeader({ name, subtitle, patchPath, pathFor, note, children }) {
	const [editing, setEditing] = useState(false)
	const [next, setNext] = useState(name)
	const [busy, setBusy] = useState(false)
	const [error, setError] = useState(null)

	async function submit(e) {
		e.preventDefault()
		setBusy(true)
		setError(null)
		try {
			const renamed = await api(patchPath, { method: "PATCH", body: JSON.stringify({ name: next }) })
			location.replace(pathFor(renamed.name))
		} catch (err) {
			setError(err.message)
			setBusy(false)
		}
	}

	return (
		<>
			<h1>{name}</h1>
			{subtitle && <p class="muted title-sub">{subtitle}</p>}
			{editing ? (
				<form class="row under-title" onSubmit={submit}>
					<input
						required
						pattern="[a-z0-9\-]+"
						aria-label="New name"
						value={next}
						onInput={(e) => setNext(e.currentTarget.value)}
					/>
					<button type="submit" disabled={busy || next === name}>
						<Check size={iconSize} /> Save
					</button>
					<button
						type="button"
						class="quiet"
						onClick={() => {
							setEditing(false)
							setNext(name)
							setError(null)
						}}
					>
						<X size={iconSize} /> Cancel
					</button>
					{note && <span class="muted">{note}</span>}
					{error && <span class="error">{error}</span>}
				</form>
			) : (
				<div class="row under-title">
					<button type="button" class="quiet" onClick={() => setEditing(true)}>
						<Pencil size={iconSize} /> Rename
					</button>
					{children}
				</div>
			)}
		</>
	)
}

// ToolItems lists tools by their full names, each linking to its page (where
// it can be called), with a "Try it" link to the same page beside the name,
// since a bold code name doesn't read as a link. Then its signature, its description's first block and
// the rest behind "More": some servers write a page per tool. A tool's name
// is <server>__<tool> once prefixed, which is what hrefs are built from.
export function ToolItems({ tools, prefix = "" }) {
	return (
		<ul class="list tools">
			{tools.map((t) => (
				<li key={t.name}>
					<div class="tool-head">
						<a class="tool-name" href={toolHref(prefix + t.name)}>
							<code>
								{prefix}
								{t.name}
							</code>
						</a>
						<a class="try-it small" href={toolHref(prefix + t.name)}>
							<Play size={12} /> Try it
						</a>
					</div>
					<ToolSignature schema={t.inputSchema} />
					{t.description && <ToolDescription text={t.description} />}
				</li>
			))}
		</ul>
	)
}

function toolHref(fullName) {
	const [server, ...rest] = fullName.split("__")
	return toolPath(server, rest.join("__"))
}

function ToolDescription({ text }) {
	return (
		<div class="tool-description">
			<MarkdownLead text={text} />
		</div>
	)
}

// MarkdownLead is markdown's first block, and the rest behind "More".
export function MarkdownLead({ text }) {
	const [lead, rest] = splitMarkdown(text)
	return (
		<>
			<MarkdownBlocks blocks={lead} />
			{rest.length > 0 && (
				<details>
					<summary>More</summary>
					<MarkdownBlocks blocks={rest} />
				</details>
			)}
		</>
	)
}

// ToolSignature shows a tool's input schema as a call: (id: string,
// limit?: number). Required parameters come first, in the schema's order;
// a parameter's description, and every value of a long enum, is its tooltip.
// A schema with no properties is ().
export function ToolSignature({ schema }) {
	const properties = schema?.properties ?? {}
	const required = new Set(schema?.required ?? [])
	const names = Object.keys(properties)
	names.sort((a, b) => required.has(b) - required.has(a))
	return (
		<code class="tool-signature">
			(
			{names.map((name, i) => (
				<span key={name} class="tool-param" title={paramTooltip(properties[name])}>
					{i > 0 && ", "}
					<span class="tool-param-name">
						{name}
						{required.has(name) ? "" : "?"}
					</span>
					: {schemaType(properties[name])}
				</span>
			))}
			)
		</code>
	)
}

// An enum of more values than this is shown as its base type: Railway's
// event types would make a signature several lines long.
const maxInlineEnum = 4

// schemaType is a JSON Schema as a short type: string, number[],
// "a" | "b", string | null, object. Anything it can't name is "any".
export function schemaType(s) {
	if (s === true || s == null || typeof s !== "object") return "any"
	if (s.const !== undefined) return JSON.stringify(s.const)
	if (Array.isArray(s.enum) && s.enum.length <= maxInlineEnum) {
		return s.enum.map((v) => JSON.stringify(v)).join(" | ")
	}
	const union = s.anyOf ?? s.oneOf
	if (Array.isArray(union)) return [...new Set(union.map(schemaType))].join(" | ")
	if (Array.isArray(s.allOf)) return s.allOf.map(schemaType).join(" & ")
	if (typeof s.$ref === "string") return s.$ref.split("/").pop()
	const types = Array.isArray(s.type) ? s.type : s.type ? [s.type] : []
	if (types.length === 0) return s.properties ? "object" : Array.isArray(s.enum) ? "enum" : "any"
	return types.map((t) => (t === "array" ? arrayType(s.items) : t)).join(" | ")
}

function arrayType(items) {
	const inner = schemaType(items)
	return inner.includes(" ") ? `(${inner})[]` : `${inner}[]`
}

function paramTooltip(s) {
	if (s == null || typeof s !== "object") return undefined
	const values = s.enum ?? s.items?.enum
	const lines = [s.description]
	if (Array.isArray(values) && values.length > maxInlineEnum) {
		lines.push(`One of: ${values.map((v) => JSON.stringify(v)).join(", ")}`)
	}
	return lines.filter(Boolean).join("\n\n") || undefined
}
