import { useEffect, useState } from "preact/hooks"

// The one fetch wrapper. Every /api/* response is JSON, including errors
// (`{"error": "..."}`), so a non-2xx rejects with the server's own message.
// A network failure (server down) rejects with status 0.

export class ApiError extends Error {
	constructor(message, status) {
		super(message)
		this.status = status
	}
}

export async function api(path, init) {
	let res
	try {
		res = await fetch(path, {
			...init,
			headers: { "Content-Type": "application/json", ...init?.headers },
		})
	} catch {
		throw new ApiError("Bangle is not reachable", 0)
	}
	const body = await res.json().catch(() => {
		throw new ApiError(`Bad response from ${path} (${res.status})`, res.status)
	})
	if (!res.ok) throw new ApiError(body.error ?? `HTTP ${res.status}`, res.status)
	return body
}

// useFetched loads path, and again whenever it changes or reload() is called.
// Its value is one of three states, kept apart so an empty state is only ever
// rendered from data that arrived:
//   {state: "loading"} | {state: "failed", error} | {state: "loaded", data}
// A reload keeps showing the old data until the new data arrives.
export function useFetched(path) {
	const [value, setValue] = useState({ state: "loading" })
	const [nonce, setNonce] = useState(0)
	useEffect(() => {
		let live = true
		if (nonce === 0) setValue({ state: "loading" })
		api(path).then(
			(data) => live && setValue({ state: "loaded", data }),
			(e) => live && setValue({ state: "failed", error: e.message }),
		)
		return () => {
			live = false
		}
	}, [path, nonce])
	return [value, () => setNonce((n) => n + 1)]
}
