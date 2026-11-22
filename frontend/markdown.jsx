// Basic markdown for tool descriptions, which upstream servers write in it.
// It builds Preact elements, never HTML strings, so a description cannot
// inject markup: everything that isn't recognised is shown as text.
//
// Blocks: paragraphs (split on blank lines), "#" headings, "-"/"*"/"1."
// lists, ``` fences. Inline: `code`, **bold**, *italic*, [text](http(s) url).
// _underscores_ are deliberately not italics: tool names are full of them.

// splitMarkdown parses text into its first block and the rest, so a long
// description can be shown as its lead plus "More".
export function splitMarkdown(text) {
	const all = blocks(text)
	return [all.slice(0, 1), all.slice(1)]
}

// Markdown renders the whole of text.
export function Markdown({ text }) {
	return <MarkdownBlocks blocks={blocks(text)} />
}

export function MarkdownBlocks({ blocks }) {
	return <div class="md">{blocks.map(renderBlock)}</div>
}

function blocks(text) {
	const out = []
	const lines = text.replace(/\r\n?/g, "\n").split("\n")
	let para = []
	let list = null
	const flush = () => {
		if (para.length) out.push({ type: "p", text: para.join(" ") })
		if (list) out.push(list)
		para = []
		list = null
	}
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i]
		const trimmed = line.trim()
		if (trimmed.startsWith("```")) {
			flush()
			const code = []
			for (i++; i < lines.length && !lines[i].trim().startsWith("```"); i++) code.push(lines[i])
			out.push({ type: "pre", text: code.join("\n") })
			continue
		}
		if (trimmed === "") {
			flush()
			continue
		}
		const heading = trimmed.match(/^(#{1,6})\s+(.*)$/)
		if (heading) {
			flush()
			out.push({ type: "h", text: heading[2] })
			continue
		}
		const item = trimmed.match(/^([-*]|\d+[.)])\s+(.*)$/)
		if (item) {
			const ordered = /\d/.test(item[1])
			if (para.length || (list && list.ordered !== ordered)) flush()
			if (!list) list = { type: "list", ordered, items: [] }
			list.items.push(item[2])
			continue
		}
		if (list) {
			// An indented line continues the last item; anything else ends the list.
			if (/^\s/.test(line)) {
				list.items[list.items.length - 1] += ` ${trimmed}`
				continue
			}
			flush()
		}
		para.push(trimmed)
	}
	flush()
	return out
}

function renderBlock(b, i) {
	switch (b.type) {
		case "pre":
			return <pre key={i}>{b.text}</pre>
		case "h":
			return (
				<p key={i}>
					<strong>{inline(b.text)}</strong>
				</p>
			)
		case "list": {
			const items = b.items.map((t, j) => <li key={j}>{inline(t)}</li>)
			return b.ordered ? <ol key={i}>{items}</ol> : <ul key={i}>{items}</ul>
		}
		default:
			return <p key={i}>{inline(b.text)}</p>
	}
}

const inlinePattern = /`([^`]+)`|\*\*([^*]+)\*\*|\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)|\*([^*\s][^*]*)\*/g

function inline(text) {
	const out = []
	let last = 0
	for (const m of text.matchAll(inlinePattern)) {
		if (m.index > last) out.push(text.slice(last, m.index))
		const k = out.length
		if (m[1] !== undefined) out.push(<code key={k}>{m[1]}</code>)
		else if (m[2] !== undefined) out.push(<strong key={k}>{inline(m[2])}</strong>)
		else if (m[3] !== undefined)
			out.push(
				<a key={k} href={m[4]} target="_blank" rel="noreferrer">
					{m[3]}
				</a>,
			)
		else out.push(<em key={k}>{m[5]}</em>)
		last = m.index + m[0].length
	}
	if (last < text.length) out.push(text.slice(last))
	return out
}
