// Captures how pi's adapters end a stream whose signal aborts before a
// response arrives — the oracle behind TestAbortBeforeResponseMatchesPi in
// this package.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> abort-before-response-d4d53612b.json d4d53612b
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone, plus a node_modules resolving pi-ai's dependencies. The
// script refuses to write unless each SDK it runs (@anthropic-ai/sdk, openai,
// @google/genai) resolves to the version AND integrity the sha's
// package-lock.json locks at the path it resolved to:
// packages/ai/node_modules/<name> for a copy the lockfile nests for pi-ai,
// node_modules/<name> otherwise. Since ab30693d6 it nests openai 7.19.0 there
// (the root keeps 6.40.0): unpack `npm pack openai@7.19.0` (check its sha512
// against the lockfile) to packages/ai/node_modules/openai and record that
// integrity in packages/ai/node_modules/.package-lock.json under
// "node_modules/openai", as npm records an installed package.
//
// Every adapter that routes its request through retryProviderRequest
// (anthropic-messages, openai-completions, openai-responses, and
// google-generative-ai through retryGoogleRequest) runs three ways against a
// raw TCP server: with a signal aborted before the call; with the server
// never answering and the signal aborted 200ms in; and with the server
// answering 503 (retry-after: 5) under maxRetries 2, the signal aborted 200ms
// into the wait. Recorded per run: the stream's event types, the final
// stopReason and errorMessage, how many times onPayload ran, and how many
// requests reached the server.
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}

const src = path.join(extraction, "packages/ai/src");
const require = createRequire(path.join(src, "api/google-generative-ai.ts"));
const lock = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8"));
const sdks: string[] = [];
for (const name of ["@anthropic-ai/sdk", "openai", "@google/genai"]) {
	let dir = path.dirname(require.resolve(name));
	while (!fs.existsSync(path.join(dir, "package.json")) || JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).name !== name) {
		dir = path.dirname(dir);
	}
	const version = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version as string;
	let root = dir;
	while (path.basename(root) !== "node_modules") root = path.dirname(root);
	const installed = JSON.parse(fs.readFileSync(path.join(root, ".package-lock.json"), "utf8")).packages[`node_modules/${name}`];
	// The lockfile entry for the path it resolved to: pi-ai's own nested copy
	// (packages/ai/node_modules/<name>, as openai is since ab30693d6) or the
	// root one (node_modules/<name>). Where the lockfile nests a copy for
	// pi-ai, pi-ai must resolve that one.
	const resolvedKey = path.relative(extraction, dir);
	const key = resolvedKey.startsWith("..") || path.isAbsolute(resolvedKey) ? `node_modules/${name}` : resolvedKey;
	const nestedKey = `packages/ai/node_modules/${name}`;
	const locked = lock.packages[key];
	if ((lock.packages[nestedKey] && key !== nestedKey) || locked?.version !== version || locked.integrity !== installed?.integrity) {
		const want = lock.packages[nestedKey] ? nestedKey : key;
		console.error(`${name} mismatch: ${sha} locks ${lock.packages[want]?.version} ${lock.packages[want]?.integrity} at ${want}, resolved ${version} ${installed?.integrity} at ${key}`);
		process.exit(1);
	}
	sdks.push(`${name} ${version} ${locked.integrity}`);
}

const load = async (file: string) => import(pathToFileURL(path.join(src, file)).href);
const adapters = [
	{ api: "anthropic-messages", provider: "anthropic", id: "claude-sonnet-4-5", module: await load("api/anthropic-messages.ts"), body: '{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}' },
	{ api: "openai-completions", provider: "openai", id: "gpt-4o", module: await load("api/openai-completions.ts"), body: '{"error":{"message":"overloaded","type":"server_error"}}' },
	{ api: "openai-responses", provider: "openai", id: "gpt-5-mini", module: await load("api/openai-responses.ts"), body: '{"error":{"message":"overloaded","type":"server_error"}}' },
	{ api: "google-generative-ai", provider: "google", id: "gemini-2.5-flash", module: await load("api/google-generative-ai.ts"), body: '{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}' },
];

type Mode = "an already-aborted signal" | "an abort before the response arrives" | "an abort during a retry wait";
const modes: Mode[] = ["an already-aborted signal", "an abort before the response arrives", "an abort during a retry wait"];

let mode: Mode = modes[0];
let body = "";
let requests = 0;
const server = net.createServer((sock) => {
	let buf = Buffer.alloc(0);
	sock.on("data", (d) => {
		buf = Buffer.concat([buf, d]);
		const headEnd = buf.indexOf("\r\n\r\n");
		if (headEnd < 0) return;
		const len = Number(/content-length:\s*(\d+)/i.exec(buf.subarray(0, headEnd).toString())?.[1] ?? 0);
		if (buf.length < headEnd + 4 + len) return;
		buf = buf.subarray(headEnd + 4 + len);
		requests++;
		if (mode === "an abort during a retry wait") {
			sock.write(`HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nretry-after: 5\r\nContent-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`);
		}
		// Otherwise never answer.
	});
	sock.on("error", () => {});
});
await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const port = (server.address() as net.AddressInfo).port;

const results = [];
for (const a of adapters) {
	for (const m of modes) {
		mode = m;
		body = a.body;
		requests = 0;
		const model = {
			id: a.id,
			name: a.id,
			api: a.api,
			provider: a.provider,
			baseUrl: `http://127.0.0.1:${port}`,
			reasoning: false,
			input: ["text"],
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow: 100000,
			maxTokens: 1000,
		};
		const controller = new AbortController();
		if (m === "an already-aborted signal") controller.abort();
		else setTimeout(() => controller.abort(), 200);
		let onPayload = 0;
		const out = a.module.stream(model, { messages: [{ role: "user", content: "hi", timestamp: 1 }] }, {
			apiKey: "test-api-key",
			signal: controller.signal,
			...(m === "an abort during a retry wait" && { maxRetries: 2 }),
			onPayload: () => {
				onPayload++;
				return undefined;
			},
		});
		const events: string[] = [];
		for await (const ev of out) events.push(ev.type);
		const msg = await out.result();
		results.push({ api: a.api, mode: m, events, stopReason: msg.stopReason, errorMessage: msg.errorMessage, onPayload, requests });
	}
}
server.close();

fs.writeFileSync(
	outFile,
	`${JSON.stringify({ source: `upstream ${sha} packages/ai/src/api (retryProviderRequest), ${sdks.join(", ")}, node ${process.version}`, runs: results }, null, "\t")}\n`,
);
console.log(`wrote ${results.length} runs to ${outFile}`);
process.exit(0);
