import * as crypto from "crypto"
import * as esbuild from "esbuild"
import * as fs from "fs"
import * as path from "path"
import { fileURLToPath } from "url"

// Builds frontend/dist from scratch: the old one is removed first, so nothing
// a previous build wrote (an older hashed bundle, a renamed asset) outlives
// it and gets embedded. The script and the stylesheet are named by a hash of
// their contents (main-<hash>.js, styles-<hash>.css), and index.html is
// written pointing at them, so a new build is new URLs, which the server lets
// browsers cache for good (hashedAsset in internal/server/server.go).

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const outdir = path.join(__dirname, "frontend", "dist")

fs.rmSync(outdir, { recursive: true, force: true })
fs.mkdirSync(outdir, { recursive: true })
// Committed, so frontend.go's embed pattern matches before any build. See
// frontend/frontend.go.
fs.writeFileSync(path.join(outdir, ".gitkeep"), "")

// The logo, unhashed: it is the favicon and the header bar's mark, and is
// served with revalidation like any non-hashed asset.
fs.copyFileSync(path.join(__dirname, "frontend", "favicon.svg"), path.join(outdir, "favicon.svg"))

const css = fs.readFileSync(path.join(__dirname, "frontend", "styles.css"), "utf8")
const cssName = `styles-${crypto.createHash("sha256").update(css).digest("hex").slice(0, 8)}.css`
fs.writeFileSync(path.join(outdir, cssName), css)

const result = await esbuild.build({
	entryPoints: [path.join(__dirname, "frontend", "main.jsx")],
	bundle: true,
	outdir,
	entryNames: "[name]-[hash]",
	jsx: "automatic",
	jsxImportSource: "preact",
	format: "iife",
	sourcemap: true,
	minify: true,
	metafile: true,
})
const jsName = Object.keys(result.metafile.outputs)
	.map((p) => path.basename(p))
	.find((n) => n.endsWith(".js"))
if (!jsName) throw new Error("esbuild wrote no .js output")

// index.html, pointing at this build's names. The source names the plain
// ones; a reference missing from it is a failed build, not a page that loads
// nothing.
let html = fs.readFileSync(path.join(__dirname, "frontend", "index.html"), "utf8")
for (const [plain, hashed] of [
	["/static/styles.css", cssName],
	["/static/main.js", jsName],
]) {
	if (!html.includes(plain)) throw new Error(`frontend/index.html does not reference ${plain}`)
	html = html.replace(plain, `/static/${hashed}`)
}
fs.writeFileSync(path.join(outdir, "index.html"), html)

console.log(`built ${jsName}, ${cssName}`)
