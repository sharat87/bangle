// JSON renders a value as indented JSON with syntax highlighting, as Preact
// elements (never an HTML string). The output is exactly
// JSON.stringify(value, null, 2), with each token wrapped in a span.
export function JSONView({ value }) {
	return <pre class="json">{render(value ?? null, "")}</pre>
}

// parseJSONContainer returns the object or array a text holds, or undefined
// when the text isn't JSON, or is JSON but a bare string, number or literal.
// Those are shown as the text itself: re-rendering them would add quotes and
// escapes the model never saw.
export function parseJSONContainer(text) {
	let value
	try {
		value = JSON.parse(text)
	} catch {
		// Not JSON: the caller shows the text as is. Nothing is lost.
		return undefined
	}
	return value !== null && typeof value === "object" ? value : undefined
}

function render(value, indent) {
	if (value === null) return <span class="j-literal">null</span>
	switch (typeof value) {
		case "string":
			return <span class="j-string">{JSON.stringify(value)}</span>
		case "number":
			return <span class="j-number">{JSON.stringify(value)}</span>
		case "boolean":
			return <span class="j-literal">{String(value)}</span>
	}
	const inner = indent + "  "
	if (Array.isArray(value)) {
		if (value.length === 0) return "[]"
		return [
			"[\n",
			...value.flatMap((item, i) => [inner, render(item, inner), i < value.length - 1 ? ",\n" : "\n"]),
			indent + "]",
		]
	}
	const entries = Object.entries(value)
	if (entries.length === 0) return "{}"
	return [
		"{\n",
		...entries.flatMap(([key, item], i) => [
			inner,
			<span class="j-key">{JSON.stringify(key)}</span>,
			": ",
			render(item, inner),
			i < entries.length - 1 ? ",\n" : "\n",
		]),
		indent + "}",
	]
}
