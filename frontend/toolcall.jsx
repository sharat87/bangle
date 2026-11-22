import { Play } from "lucide-preact"
import { useState } from "preact/hooks"
import { api, useFetched } from "./api"
import { duration, Outcome, Result } from "./calls"
import { iconSize, MarkdownLead, schemaType, serverPath, ToolSignature } from "./common"
import { Markdown } from "./markdown"

// /servers/<server>/tools/<tool>: one tool, and a form to call it.
//
// The form is built from the tool's input schema, one field per parameter
// (fieldKind says which). Arguments can also be written as JSON, for what
// the form can't express (null, a nested object typed field by field).
// Switching between the two carries the arguments over, or says why it
// can't. A call is POST /api/servers/<server>/call, which records it in the
// audit like an agent's: its answer is audit.go's callFromUI JSON.

export function ToolPage({ server, tool }) {
	const [tools] = useFetched(`/api${serverPath(server)}/tools`)
	const title = (
		<>
			<h1>
				<code class="title-code">
					{server}__{tool}
				</code>
			</h1>
			<p class="muted title-sub">
				<span>
					A tool of <a href={serverPath(server)}>{server}</a>
				</span>
			</p>
		</>
	)
	if (tools.state === "loading")
		return (
			<>
				{title}
				<p class="muted">Listing tools…</p>
			</>
		)
	if (tools.state === "failed")
		return (
			<>
				{title}
				<p class="error">Failed to list tools: {tools.error}</p>
			</>
		)
	const t = tools.data.find((t) => t.name === tool)
	if (!t)
		return (
			<>
				{title}
				<p class="error">
					{server} lists no tool named <code>{tool}</code>.
				</p>
			</>
		)
	return (
		<>
			{title}
			<ToolSignature schema={t.inputSchema} />
			{t.description && (
				<div class="tool-description">
					<Markdown text={t.description} />
				</div>
			)}
			<h2>Call</h2>
			<p class="muted">
				This runs the tool for real, as Bangle's login to {server}. It's recorded in <a href="/calls">Call History</a>.
			</p>
			<CallForm server={server} tool={t} />
		</>
	)
}

function CallForm({ server, tool }) {
	const fields = toolFields(tool.inputSchema)
	const [mode, setMode] = useState("form")
	const [values, setValues] = useState({})
	const [json, setJSON] = useState("{}")
	const [problem, setProblem] = useState(null)
	const [busy, setBusy] = useState(false)
	const [answer, setAnswer] = useState(null) // {state: "failed", error} | {state: "loaded", data}

	function switchTo(next) {
		try {
			if (next === "json") setJSON(JSON.stringify(buildArgs(fields, values), null, 2))
			else setValues(valuesFrom(fields, parseObject(json)))
			setProblem(null)
			setMode(next)
		} catch (err) {
			setProblem(err.message)
		}
	}

	async function submit(e) {
		e.preventDefault()
		let args
		try {
			args = mode === "form" ? buildArgs(fields, values) : parseObject(json)
		} catch (err) {
			setProblem(err.message)
			return
		}
		setProblem(null)
		setBusy(true)
		try {
			const data = await api(`/api${serverPath(server)}/call`, {
				method: "POST",
				body: JSON.stringify({ tool: tool.name, arguments: args }),
			})
			setAnswer({ state: "loaded", data })
		} catch (err) {
			setAnswer({ state: "failed", error: err.message })
		} finally {
			setBusy(false)
		}
	}

	const set = (name, value) => setValues({ ...values, [name]: value })
	return (
		<>
			<form class="stack" onSubmit={submit}>
				<fieldset class="row" aria-label="Arguments as">
					<label class="inline">
						<input type="radio" checked={mode === "form"} onChange={() => switchTo("form")} /> Form
					</label>
					<label class="inline">
						<input type="radio" checked={mode === "json"} onChange={() => switchTo("json")} /> JSON
					</label>
				</fieldset>
				{mode === "json" ? (
					<label>
						Arguments
						<textarea rows={12} value={json} onInput={(e) => setJSON(e.currentTarget.value)} />
					</label>
				) : fields.length === 0 ? (
					<p class="muted">This tool takes no arguments.</p>
				) : (
					fields.map((f) => <Field key={f.name} field={f} value={values[f.name] ?? ""} onChange={set} />)
				)}
				<div class="row">
					<button type="submit" disabled={busy}>
						<Play size={iconSize} /> {busy ? "Calling…" : "Call"}
					</button>
					{problem && <span class="error">{problem}</span>}
				</div>
			</form>
			{answer && <Answer answer={answer} />}
		</>
	)
}

function Answer({ answer }) {
	if (answer.state === "failed") return <p class="error">The call was not made: {answer.error}</p>
	const c = answer.data
	return (
		<>
			<h2>Result</h2>
			<p>
				<Outcome call={c} />, took {duration(c.duration_ms)}. <a href={`/calls/${c.id}`}>Recorded as call {c.id}</a>.
				{c.error && (
					<>
						<br />
						<span class="error">{c.error}</span>
					</>
				)}
			</p>
			<Result result={c.result} />
		</>
	)
}

// Field is one parameter. Its value is always a string: the input's text,
// or for an enum the chosen value as JSON ("" is not set). Its description
// is markdown, its first block shown and the rest behind "More" (PostHog's
// exec documents a whole CLI in one parameter), so it sits after the label (a label holds only inline
// content), tied to the input by aria-describedby.
function Field({ field: f, value, onChange }) {
	const change = (e) => onChange(f.name, e.currentTarget.value)
	const heading = (
		<>
			<code>{f.name}</code>
			{!f.required && <span class="muted"> (optional)</span>}
		</>
	)
	const helpID = `help-${f.name}`
	const describedBy = f.description ? helpID : undefined
	const help = f.description && (
		<div id={helpID} class="field-help small muted">
			<MarkdownLead text={f.description} />
		</div>
	)
	if (f.kind === "enum")
		return (
			<fieldset>
				<legend>{heading}</legend>
				<div class="row options" aria-describedby={describedBy}>
					{!f.required && (
						<label class="inline">
							<input type="radio" name={f.name} value="" checked={value === ""} onChange={change} />
							<span class="muted">not set</span>
						</label>
					)}
					{f.values.map((v) => {
						const key = JSON.stringify(v)
						return (
							<label class="inline" key={key}>
								<input
									type="radio"
									name={f.name}
									value={key}
									required={f.required}
									checked={value === key}
									onChange={change}
								/>
								<code>{typeof v === "string" ? v : key}</code>
							</label>
						)
					})}
				</div>
				{help}
			</fieldset>
		)
	return (
		<div>
			<label>
				{heading}
				{f.kind === "json" ? (
					<textarea
						rows={3}
						placeholder={f.type}
						required={f.required}
						aria-describedby={describedBy}
						value={value}
						onInput={change}
					/>
				) : (
					<input
						type={f.kind === "string" ? "text" : "number"}
						step={f.kind === "integer" ? 1 : "any"}
						placeholder={f.type}
						required={f.required}
						aria-describedby={describedBy}
						value={value}
						onInput={change}
					/>
				)}
			</label>
			{help}
		</div>
	)
}

// toolFields is the schema's parameters as form fields, required first.
// kind is how a value is entered: string, number or integer (an input),
// enum (radios; a boolean is the enum true | false), or json (a textarea,
// for everything else). A type that also allows null is entered as its
// other type: null itself takes the JSON view.
function toolFields(schema) {
	const properties = schema?.properties ?? {}
	const required = new Set(schema?.required ?? [])
	return Object.entries(properties)
		.map(([name, p]) => ({
			name,
			required: required.has(name),
			description: p?.description,
			type: schemaType(p),
			...fieldKind(p),
		}))
		.sort((a, b) => b.required - a.required)
}

function fieldKind(p) {
	let s = p
	if (s && typeof s === "object") {
		const union = (s.anyOf ?? s.oneOf)?.filter((m) => m?.type !== "null")
		if (union?.length === 1) s = union[0]
	}
	if (!s || typeof s !== "object") return { kind: "json" }
	if (Array.isArray(s.enum)) return { kind: "enum", values: s.enum.filter((v) => v !== null) }
	const types = (Array.isArray(s.type) ? s.type : [s.type]).filter((t) => t && t !== "null")
	if (types.length !== 1) return { kind: "json" }
	switch (types[0]) {
		case "string":
			return { kind: "string" }
		case "number":
		case "integer":
			return { kind: types[0] }
		case "boolean":
			return { kind: "enum", values: [true, false] }
		default:
			return { kind: "json" }
	}
}

// buildArgs is the form's arguments object. An empty field is left out.
function buildArgs(fields, values) {
	const args = {}
	for (const f of fields) {
		const raw = values[f.name] ?? ""
		if (raw === "") continue
		switch (f.kind) {
			case "string":
				args[f.name] = raw
				break
			case "number":
			case "integer": {
				const n = Number(raw)
				if (!Number.isFinite(n)) throw new Error(`${f.name} is not a number`)
				args[f.name] = n
				break
			}
			default:
				try {
					args[f.name] = JSON.parse(raw)
				} catch {
					throw new Error(`${f.name} is not valid JSON`)
				}
		}
	}
	return args
}

// valuesFrom is the inverse of buildArgs, for arguments written as JSON. It
// refuses what the form can't show rather than drop it.
function valuesFrom(fields, args) {
	const byName = new Map(fields.map((f) => [f.name, f]))
	const values = {}
	for (const [name, v] of Object.entries(args)) {
		const f = byName.get(name)
		if (!f) throw new Error(`The form has no field ${name}`)
		const cant = new Error(`The form can't show ${name}: ${JSON.stringify(v)}`)
		switch (f.kind) {
			case "string":
				if (typeof v !== "string") throw cant
				values[name] = v
				break
			case "number":
			case "integer":
				if (typeof v !== "number") throw cant
				values[name] = String(v)
				break
			case "enum":
				if (!f.values.some((x) => JSON.stringify(x) === JSON.stringify(v))) throw cant
				values[name] = JSON.stringify(v)
				break
			default:
				values[name] = JSON.stringify(v, null, 2)
		}
	}
	return values
}

function parseObject(text) {
	let v
	try {
		v = JSON.parse(text)
	} catch {
		throw new Error("The arguments are not valid JSON")
	}
	if (v === null || typeof v !== "object" || Array.isArray(v)) throw new Error("The arguments must be a JSON object")
	return v
}
