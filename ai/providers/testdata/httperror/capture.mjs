// Captures what real pi surfaces as `errorMessage` for a non-2xx HTTP response,
// per adapter, over a table of bodies — the oracle behind the HTTP-error
// formatting tests in this package (TestResponsesHTTPErrorMatchesPi,
// TestCompletionsHTTPErrorMatchesPi) — and, for the adapters whose SDK error
// carries the response headers, the message pi's retryProviderRequest fails
// fast with when a 429 asks for a retry delay past maxRetryDelayMs.
//
//   node capture.mjs <pi-ai npm dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0 pi-ai-1.3.0.json
//   node --experimental-strip-types capture.mjs --src <extraction> <out.json> <sha>
//   e.g. ... capture.mjs --src <dir> pi-ai-src-<sha>.json <sha>
//
// Each adapter streams against a loopback server that answers with the given
// status and body, so neither side needs a key or the network. The capture is
// from the PUBLISHED build, which is what the goldens in this repo come from
// (.claude/skills/pi-parity-review/SKILL.md: when source and build disagree,
// the build wins) — except while no build ships a change the port has taken:
// then --src runs pi-ai's src at <sha> from an extraction holding
// `git archive <sha> packages/ai package-lock.json` and a node_modules
// resolving its dependencies, as an interim oracle to be replaced by the
// first published build that ships it. It refuses to write unless each SDK
// resolves to the version AND integrity the sha's package-lock.json locks at
// the path it resolved to: packages/ai/node_modules/<name> for a copy the
// lockfile nests for pi-ai, node_modules/<name> otherwise. Since ab30693d6
// (openai 7.19.0, nested; no build ships it yet) the committed oracle is
// src-captured: unpack `npm pack openai@7.19.0` (check its sha512 against the
// lockfile) to packages/ai/node_modules/openai and record that integrity in
// packages/ai/node_modules/.package-lock.json under "node_modules/openai".
// The "openai-responses/xai" column drives the same adapter as provider xai,
// which pi labels by its provider since upstream 0c7bb7c5c.
//
// `byteRows` carry bodies that are not valid UTF-8, as `bodyBase64`: fetch's
// text() decodes them with the WHATWG UTF-8 decoder (one U+FFFD per maximal
// subpart, a leading BOM dropped) before any client parses them. pi-ai 1.3.0
// (which ships ab30693d6) records the 2b0a123de src oracle's rows unchanged.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const args = process.argv.slice(2);
const srcMode = args[0] === "--src";
const [dir, outFile, sha] = srcMode ? args.slice(1) : args;
if (!dir || !outFile || (srcMode && !sha)) {
	console.error("usage: node capture.mjs <pi-ai npm dir> <out.json>\n       node --experimental-strip-types capture.mjs --src <extraction> <out.json> <sha>");
	process.exit(2);
}

let load;
let source;
if (srcMode) {
	const src = path.join(dir, "packages/ai/src");
	const require = createRequire(path.join(src, "api/openai-completions.ts"));
	const lock = JSON.parse(fs.readFileSync(path.join(dir, "package-lock.json"), "utf8"));
	const sdks = [];
	for (const name of ["@anthropic-ai/sdk", "openai", "@google/genai"]) {
		let pkg = path.dirname(require.resolve(name));
		while (!fs.existsSync(path.join(pkg, "package.json")) || JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).name !== name) {
			pkg = path.dirname(pkg);
		}
		const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
		let root = pkg;
		while (path.basename(root) !== "node_modules") root = path.dirname(root);
		const installed = JSON.parse(fs.readFileSync(path.join(root, ".package-lock.json"), "utf8")).packages[`node_modules/${name}`];
		// The lockfile entry for the path it resolved to: pi-ai's own nested
		// copy (packages/ai/node_modules/<name>, as openai is since ab30693d6)
		// or the root one (node_modules/<name>). Where the lockfile nests a copy
		// for pi-ai, pi-ai must resolve that one.
		const resolvedKey = path.relative(dir, pkg);
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
	load = (file) => import(pathToFileURL(path.join(src, file.replace(/\.js$/, ".ts"))).href);
	source = `upstream ${sha} packages/ai/src, ${sdks.join(", ")}, node ${process.version}`;
} else {
	const pkg = path.join(dir, "node_modules/@earendil-works/pi-ai");
	const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
	load = (file) => import(pathToFileURL(path.join(pkg, "dist", file)).href);
	source = `@earendil-works/pi-ai ${version} (published build), node ${process.version}`;
}
const responses = await load("api/openai-responses.js");
const adapters = {
	"openai-responses": ["openai", responses],
	"openai-responses/xai": ["xai", responses],
	"openai-completions": ["openai", await load("api/openai-completions.js")],
	"anthropic-messages": ["anthropic", await load("api/anthropic-messages.js")],
	"google-generative-ai": ["google", await load("api/google-generative-ai.js")],
};
// The adapters whose SDK error carries the response headers, so that pi's
// retryProviderRequest reads a server-requested delay and fails fast on one
// past maxRetryDelayMs (60s by default).
const failFastAdapters = ["openai-responses", "openai-responses/xai", "openai-completions", "anthropic-messages"];

const J = (o) => JSON.stringify(o);
const long = "x".repeat(4500);
// [name, status, body]
const cases = [
	["obj-message", 403, J({ error: { message: "blocked" } })],
	["obj-message-429", 429, J({ error: { message: "slow down" } })],
	["obj-multi-null", 403, J({ error: { message: "slow down", type: "rate_limit_error", code: null, param: null } })],
	["obj-no-message-keyorder", 403, J({ error: { type: "zeta", code: "alpha" } })],
	["obj-nested-message", 403, J({ error: { message: { nested: 1 } } })],
	["obj-escapes", 403, J({ error: { message: "café   </script> \"q\" \\ \t  \u{1F600}", z: 1, a: 2 } })],
	["obj-long-message", 403, J({ error: { message: long } })],
	["obj-short-message-long-sibling", 403, J({ error: { message: "short", details: long } })],
	["obj-astral-long", 403, J({ error: { message: "\u{1F600}".repeat(3000) } })],
	["obj-message-contains-body", 403, J({ error: { message: "{\"a\":1}", a: 1 } })],
	["str-error", 403, J({ error: "boom" })],
	["str-error-400", 400, J({ error: "boom" })],
	["str-error-long", 403, J({ error: long })],
	["empty-obj-error", 403, J({ error: {} })],
	["null-error", 403, J({ error: null })],
	["num-error", 403, J({ error: 0 })],
	["no-error-key", 403, J({ message: "no error key" })],
	["array", 403, J([1, 2])],
	["text", 403, "oops"],
	["text-500", 500, "oops"],
	["empty", 403, ""],
	["empty-503", 503, ""],
	["whitespace", 503, "   "],
	["padded", 403, "  " + J({ error: { message: "padded" } }) + "  "],
	["anthropic-shape", 403, J({ type: "error", error: { type: "authentication_error", message: "invalid x-api-key" } })],
	["google-shape", 403, J({ error: { code: 403, message: "PERMISSION_DENIED: no", status: "PERMISSION_DENIED" } })],
	// Valid JSON whose parsed value is falsy: the openai client passes the raw
	// text as the message (`errJSON ? undefined : errText`).
	["scalar-null", 403, "null"],
	["scalar-zero", 403, "0"],
	["scalar-false", 403, "false"],
	["scalar-emptystr", 403, '""'],
	["scalar-padded-zero", 403, "  0  "],
	// Numbers JSON.parse turns into Infinity (truthy; JSON.stringify -> null).
	["num-error-huge", 403, '{"error":1e400}'],
	["obj-num-huge", 403, '{"error":{"message":"x","n":1e400}}'],
	// A UTF-8 BOM: fetch's text() strips it before the client parses.
	["bom-obj", 403, "\uFEFF" + J({ error: { message: "bom" } })],
	["bom-text", 403, "\uFEFFoops"],
	// openai 7's makeStatusError: an object or array body whose own `error` is
	// absent or null is the APIError's `error` itself.
	["empty-object", 403, "{}"],
	["empty-array", 403, "[]"],
	["null-error-with-message", 403, J({ error: null, message: "m" })],
	["false-error-with-message", 403, J({ error: false, message: "m" })],
	["empty-obj-error-with-message", 403, J({ error: {}, message: "m" })],
	["no-error-key-object-message", 403, J({ message: { a: 1 } })],
	["no-error-key-long", 403, J({ message: "short", details: long })],
	["no-error-key-keyorder", 403, J({ z: 1, message: "m", a: 2 })],
	["array-of-objects", 403, J([{ message: "m" }])],
	["bom-no-error-key", 403, "\uFEFF" + J({ message: "bom" })],
	// error.metadata.raw, which openai-completions appends on its own line
	// unless the message already contains its String().
	["obj-metadata-raw", 502, J({ error: { message: "upstream failed", metadata: { raw: "backend timeout" } } })],
	["obj-metadata-raw-escaped", 502, J({ error: { message: "upstream failed", metadata: { raw: 'a "quoted" \\ b' } } })],
	["obj-metadata-raw-object", 502, J({ error: { message: "upstream failed", metadata: { raw: { a: 1 } } } })],
	["obj-metadata-raw-number", 502, J({ error: { message: "m", metadata: { raw: 5 } } })],
	["no-error-key-metadata-raw", 502, J({ message: "m", metadata: { raw: 'r "q"' } })],
	["null-error-metadata-raw", 502, J({ error: null, metadata: { raw: "a\nb" } })],
	["str-error-metadata-sibling", 502, J({ error: "boom", metadata: { raw: "x" } })],
];
// Bodies a 429 carrying retry-after: 120 answers with, under maxRetries 1:
// pi's retryProviderRequest quotes the SDK error's message when it refuses
// the delay.
const failFastCases = [
	["error-object", J({ error: { message: "slow down" } })],
	["error-string", J({ error: "boom" })],
	["no-error-key", J({ message: "slow down" })],
	["null-error-with-message", J({ error: null, message: "slow down" })],
	["array", J([1, 2])],
	["text", "slow down"],
	["empty", ""],
];

// [name, status, body bytes]
const byteCases = [
	["invalid-in-json-string", 400, Buffer.concat([Buffer.from('{"type":"error","error":{"type":"invalid_request_error","message":"bad '), Buffer.from([0xff]), Buffer.from(' byte"}}')])],
	["truncated-multibyte-text", 403, Buffer.concat([Buffer.from("oops "), Buffer.from([0xe2, 0x82])])],
	["latin1-html", 502, Buffer.concat([Buffer.from("<html>caf"), Buffer.from([0xe9]), Buffer.from("</html>")])],
	["bom-then-invalid-message", 403, Buffer.concat([Buffer.from([0xef, 0xbb, 0xbf]), Buffer.from('{"message":"a'), Buffer.from([0xc0]), Buffer.from('b"}')])],
	["overlong-and-surrogate", 403, Buffer.from([0x6f, 0xc0, 0xaf, 0xed, 0xa0, 0x80, 0x6b])],
];

async function run(api, provider, mod, status, body, headers = {}, options = {}) {
	const text = typeof body === "string" ? body : body.toString("latin1");
	const srv = http.createServer((req, res) => {
		req.resume();
		res.writeHead(status, { "content-type": /^\s*[\[{]/.test(text) ? "application/json" : "text/plain", ...headers });
		res.end(body);
	});
	await new Promise((r) => srv.listen(0, "127.0.0.1", r));
	const port = srv.address().port;
	const model = { id: "m", name: "m", api, provider, baseUrl: `http://127.0.0.1:${port}`, reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 1000 };
	let msg;
	try {
		const s = mod.stream(model, { messages: [{ role: "user", content: "hi", timestamp: 1 }] }, { apiKey: "sk", maxRetries: 0, ...options });
		msg = (await s.result()).errorMessage;
	} catch (e) {
		msg = "THREW: " + e.message;
	}
	srv.close();
	return msg;
}

const rows = [];
for (const [name, status, body] of cases) {
	const row = { name, status, body, errorMessage: {} };
	for (const [api, [provider, mod]] of Object.entries(adapters)) {
		row.errorMessage[api] = await run(api, provider, mod, status, body);
	}
	rows.push(row);
}
const byteRows = [];
for (const [name, status, bytes] of byteCases) {
	const row = { name, status, bodyBase64: bytes.toString("base64"), errorMessage: {} };
	for (const [api, [provider, mod]] of Object.entries(adapters)) {
		row.errorMessage[api] = await run(api, provider, mod, status, bytes);
	}
	byteRows.push(row);
}
const failFast = [];
for (const [name, body] of failFastCases) {
	const row = { name, status: 429, retryAfter: "120", maxRetries: 1, body, errorMessage: {} };
	for (const api of failFastAdapters) {
		const [provider, mod] = adapters[api];
		row.errorMessage[api] = await run(api, provider, mod, 429, body, { "retry-after": "120" }, { maxRetries: 1 });
	}
	failFast.push(row);
}
fs.writeFileSync(outFile, JSON.stringify({ source, rows, byteRows, failFast }, null, 1) + "\n");
console.log(`captured ${rows.length} rows, ${byteRows.length} byte rows and ${failFast.length} fail-fast rows (${source}) -> ${outFile}`);
