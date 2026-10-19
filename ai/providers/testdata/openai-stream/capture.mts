// Captures how the openai SDK's stream decoder and pi's two openai adapters
// read an SSE body — the oracle behind openai_stream_test.go in this package.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> openai-stream-87f9a3641.json 87f9a3641
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone (prefix kept) and a node_modules resolving pi-ai's
// dependencies. The script reads the openai package pi-ai's src resolves, and
// refuses to write unless it is the version AND integrity the sha's
// package-lock.json locks at the path it resolved to: since ab30693d6 the
// lockfile nests pi-ai's openai (7.19.0, integrity
// sha512-MX2s3u2L5racTO0CC/SWpCOasJQBCJrqLKXK+l82cAhdeF8mPMBEe/gxMm0ZFa2xpKpOFLRjxv5afYEZbBXmbQ==)
// at packages/ai/node_modules/openai, while the root node_modules keeps 6.40.0
// for the other workspaces. With an npm build's node_modules as the root,
// put openai 7.19.0 at packages/ai/node_modules/openai (`npm pack
// openai@7.19.0`, its sha512 checked against the lockfile, or the npm 1.3.0
// build's root copy, which is that package) and record that integrity in
// packages/ai/node_modules/.package-lock.json under "node_modules/openai", as
// npm records an installed package. The version, integrity and node version are
// recorded in the output.
//
// Every body is served from a loopback server, so nothing needs a key or the
// network, and recorded here so the Go test serves the same bytes.
//
//   dispatch  SSE bodies read three ways: `sdk` is what the SDK's stream
//             iterator yields (each item JSON.stringify'd) and what it throws,
//             through chat.completions.create and responses.create alike, and
//             over the body delivered whole and one byte per read (the script
//             fails if any two differ) — and, the same two ways, over the body
//             followed by an aborted read and by a failed one, and with the
//             stream's own signal aborted as its first item is yielded;
//             `completions` and `responses`
//             are pi's adapters over the same body: the provider stream events
//             onProviderStreamEvent observed, and how the stream ended.
//   completions / responses
//             adapter-specific bodies, with the same record as above.
//   hooks     the callback and onResponse failing, a non-2xx response, a
//             request aborted while the server holds the connection open, and
//             a response whose body is null (a 204, a 205, a custom fetch's).
//   pricing   responses' service-tier pricing: which tier a completed
//             response's service_tier leaves in force.
//   lines     the SDK's LineDecoder over a body cut into reads that split a
//             line ending, as the port's line splitter can meet one.
import fs from "node:fs";
import http from "node:http";
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
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { normalizeContext } = await load("utils/transcript.ts");
const completions = await load("api/openai-completions.ts");
const responses = await load("api/openai-responses.ts");
// sdkSource is the directory, version and integrity of the copy of the npm
// package name pi-ai's src resolves. It must be what the sha's
// package-lock.json locks at the path it resolved to: packages/ai/node_modules/
// <name> when the lockfile nests it for pi-ai (and then pi-ai must resolve that
// copy), node_modules/<name> otherwise.
const lock = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8"));
function sdkSource(name: string): { dir: string; version: string; integrity: string } {
	let dir = path.dirname(createRequire(path.join(src, "api/openai-completions.ts")).resolve(name));
	while (!fs.existsSync(path.join(dir, "package.json")) || JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).name !== name) {
		dir = path.dirname(dir);
	}
	const version = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version as string;
	let root = dir;
	while (path.basename(root) !== "node_modules") root = path.dirname(root);
	const installed = JSON.parse(fs.readFileSync(path.join(root, ".package-lock.json"), "utf8")).packages[`node_modules/${name}`];
	const resolvedKey = path.relative(extraction, dir);
	const key = resolvedKey.startsWith("..") || path.isAbsolute(resolvedKey) ? `node_modules/${name}` : resolvedKey;
	const nestedKey = `packages/ai/node_modules/${name}`;
	const locked = lock.packages[key];
	if ((lock.packages[nestedKey] && key !== nestedKey) || locked?.version !== version || locked.integrity !== installed?.integrity) {
		const want = lock.packages[nestedKey] ? nestedKey : key;
		console.error(`${name} mismatch: ${sha} locks ${lock.packages[want]?.version} ${lock.packages[want]?.integrity} at ${want}, resolved ${version} ${installed?.integrity} at ${key}`);
		process.exit(1);
	}
	return { dir, version, integrity: locked.integrity };
}
const openai = sdkSource("openai");
const openaiDir = openai.dir;
const openaiVersion = openai.version;
const { default: OpenAI } = await import(pathToFileURL(path.join(openaiDir, "index.mjs")).href);
const { Stream } = await import(pathToFileURL(path.join(openaiDir, "core/streaming.mjs")).href);
const { LineDecoder } = await import(pathToFileURL(path.join(openaiDir, "internal/decoders/line.mjs")).href);

// ---- the loopback server ---------------------------------------------------

// A held reply writes its body and keeps the connection open, so the stream
// can end only through the client giving up on it.
// A body is text, or bytes where it must carry invalid UTF-8.
type Body = string | Buffer;
type Reply = { status: number; contentType: string; body: Body; hold?: boolean };
const replies = new Map<string, Reply>();
const held = new Set<http.ServerResponse>();
let nextRoute = 0;
const server = http.createServer((req, res) => {
	req.resume();
	req.on("end", () => {
		const route = (req.url ?? "").split("/")[1];
		const reply = replies.get(route);
		if (!reply) {
			res.writeHead(404).end();
			return;
		}
		res.writeHead(reply.status, { "content-type": reply.contentType });
		if (reply.hold) {
			res.write(reply.body);
			held.add(res);
			return;
		}
		res.end(reply.body);
	});
});
await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
const { port } = server.address() as net.AddressInfo;

// route registers a reply and returns the base URL that reaches it.
function route(reply: Reply): string {
	const name = `r${nextRoute++}`;
	replies.set(name, reply);
	return `http://127.0.0.1:${port}/${name}/v1`;
}
const sse = (body: Body) => route({ status: 200, contentType: "text/event-stream", body });

// ---- the SDK's own reading -------------------------------------------------

type Thrown = { name: string; message: string; error?: string };
type SDKReading = { yields: string[]; threw: Thrown | null };
// A dispatch row's `sdk` also records the body read to its end and then
// failing: `aborted` with an AbortError while the stream's signal is live, as a
// custom fetch's body can throw one (the SDK's Stream swallows it and simply
// ends), and `readFailed` with any other error (it propagates); and
// `abortedOnFirst`, the body read whole with the stream's own signal aborted as
// its first item is yielded (the SDK dispatches nothing after that).
type SDKReadings = SDKReading & { aborted: SDKReading; readFailed: SDKReading; abortedOnFirst: SDKReading };

function thrown(error: unknown): Thrown {
	const e = error as { constructor?: { name?: string }; message?: string; error?: unknown };
	const out: Thrown = { name: e?.constructor?.name ?? typeof error, message: String(e?.message) };
	if (e && typeof e === "object" && "error" in e) out.error = JSON.stringify(e.error);
	return out;
}

async function sdkRead(body: Body, open: (client: any) => Promise<AsyncIterable<unknown>>): Promise<SDKReading> {
	const client = new OpenAI({ apiKey: "k", baseURL: sse(body), maxRetries: 0 });
	const yields: string[] = [];
	try {
		for await (const item of await open(client)) yields.push(JSON.stringify(item));
	} catch (error) {
		return { yields, threw: thrown(error) };
	}
	return { yields, threw: null };
}

// sdkReadStream is the SDK's Stream over the body delivered as a stream of
// reads, whole or one byte per read, and ending cleanly or with a failed read
// (end). A line ending split across reads ("\r" | "\n") is measured rather
// than assumed to read like the whole body. With abortOnItem, the Stream's
// signal aborts as that item (1-based) is yielded.
async function sdkReadStream(body: Body, bytewise: boolean, end?: Error, abortOnItem?: number): Promise<SDKReading> {
	const bytes = typeof body === "string" ? new TextEncoder().encode(body) : new Uint8Array(body);
	let at = 0;
	const byteStream = new ReadableStream<Uint8Array>({
		pull(controller) {
			if (at < bytes.length) {
				const next = bytewise ? at + 1 : bytes.length;
				controller.enqueue(bytes.slice(at, next));
				at = next;
			} else if (end) {
				controller.error(end);
			} else {
				controller.close();
			}
		},
	});
	const yields: string[] = [];
	const controller = new AbortController();
	try {
		for await (const item of Stream.fromSSEResponse(new Response(byteStream), controller)) {
			yields.push(JSON.stringify(item));
			if (yields.length === abortOnItem) controller.abort();
		}
	} catch (error) {
		return { yields, threw: thrown(error) };
	}
	return { yields, threw: null };
}

// sdkReadEndingIn reads the body whole and one byte per read, ending in end,
// and fails unless the two agree.
async function sdkReadEndingIn(body: Body, end?: () => Error, abortOnItem?: number): Promise<SDKReading> {
	const whole = await sdkReadStream(body, false, end?.(), abortOnItem);
	const bytewise = await sdkReadStream(body, true, end?.(), abortOnItem);
	if (JSON.stringify(whole) !== JSON.stringify(bytewise)) {
		throw new Error(`the body read whole and one byte at a time differ:\n${JSON.stringify(whole)}\n${JSON.stringify(bytewise)}`);
	}
	return whole;
}

// The SDK logs an unparseable event before rethrowing; keep the capture quiet.
const consoleError = console.error;
async function quietly<T>(run: () => Promise<T>): Promise<T> {
	console.error = () => {};
	try {
		return await run();
	} finally {
		console.error = consoleError;
	}
}

async function sdkReading(body: Body): Promise<SDKReadings> {
	return quietly(async () => {
		const chat = await sdkRead(body, (c) => c.chat.completions.create({ model: "m", messages: [], stream: true }));
		const resp = await sdkRead(body, (c) => c.responses.create({ model: "m", input: "hi", stream: true }));
		if (JSON.stringify(chat) !== JSON.stringify(resp)) {
			throw new Error(`chat and responses streams read differently:\n${JSON.stringify(chat)}\n${JSON.stringify(resp)}`);
		}
		const streamed = await sdkReadEndingIn(body);
		if (JSON.stringify(chat) !== JSON.stringify(streamed)) {
			throw new Error(`the body served and streamed read differently:\n${JSON.stringify(chat)}\n${JSON.stringify(streamed)}`);
		}
		return {
			...chat,
			aborted: await sdkReadEndingIn(body, () => new DOMException("This operation was aborted", "AbortError")),
			readFailed: await sdkReadEndingIn(body, () => new TypeError("terminated")),
			abortedOnFirst: await sdkReadEndingIn(body, undefined, 1),
		};
	});
}

// ---- pi's adapters -----------------------------------------------------------

const completionsModel = {
	id: "openrouter/auto",
	name: "OpenRouter Auto",
	api: "openai-completions",
	provider: "openrouter",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 200000,
	maxTokens: 8192,
};
const responsesModel = {
	id: "gpt-5-mini",
	name: "GPT-5 Mini",
	api: "openai-responses",
	provider: "openai",
	reasoning: true,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 400000,
	maxTokens: 128000,
};
const context = normalizeContext({ systemPrompt: "", messages: [{ role: "user", content: "hi", timestamp: 1 }] });

type Outcome = {
	observed: string[];
	sameModel: boolean;
	onResponseCalls: number;
	stopReason: string;
	errorMessage: string;
	text: string;
	// responseId and rawStopReason are String() of pi's values, which need not
	// be strings: pi assigns what the provider sent. null, undefined and a
	// value String() throws on are "".
	responseId: string;
	rawStopReason: string;
	// usage holds each member of the message's usage as String() of a number,
	// and as "<typeof>:<String()>" of anything else, which pi keeps where a
	// provider sent a string (`completion_tokens || 0`) or computes one (`+`).
	usage: Record<string, string>;
	// toolCalls are the message's tool calls, JSON.stringify'd, with id and
	// name as String() of pi's values.
	toolCalls: string[];
	// thinking are the message's thinking blocks, JSON.stringify'd, with the
	// signature as String() of pi's value.
	thinking: string[];
};

type Hooks = {
	failOnEvent?: "throw" | "abort-then-throw";
	failOnResponse?: boolean;
	// abortOnEvent aborts the request, without throwing, as the observer sees
	// that event (1-based).
	abortOnEvent?: number;
	// nullBodyStatus answers the request with a custom fetch's
	// `new Response(null, { status })` instead of the server.
	nullBodyStatus?: number;
};

async function piRun(api: any, baseModel: Record<string, unknown>, baseUrl: string, hooks: Hooks = {}): Promise<Outcome> {
	const model = { ...baseModel, baseUrl };
	const controller = new AbortController();
	const observed: string[] = [];
	let sameModel = true;
	let onResponseCalls = 0;
	const status = hooks.nullBodyStatus;
	const final = await quietly(() =>
		api.streamSimple(model, context, {
			apiKey: "k",
			signal: controller.signal,
			...(status === undefined ? {} : { fetch: async () => new Response(null, { status }) }),
			onProviderStreamEvent: (data: unknown, eventModel: unknown) => {
				observed.push(JSON.stringify(data));
				sameModel &&= eventModel === model;
				if (hooks.abortOnEvent === observed.length) controller.abort();
				if (hooks.failOnEvent === "abort-then-throw") controller.abort();
				if (hooks.failOnEvent) throw new Error("observer boom");
			},
			onResponse: () => {
				onResponseCalls++;
				if (hooks.failOnResponse) throw new Error("response veto");
			},
		}).result(),
	);
	return {
		observed,
		sameModel,
		onResponseCalls,
		stopReason: final.stopReason,
		errorMessage: final.errorMessage ?? "",
		text: final.content
			.filter((b: { type: string }) => b.type === "text")
			.map((b: { text: string }) => b.text)
			.join(""),
		responseId: jsString(final.responseId),
		rawStopReason: jsString(final.rawStopReason),
		usage: Object.fromEntries(
			["input", "output", "cacheRead", "cacheWrite", "reasoning", "totalTokens"].map((k) => {
				const v = final.usage[k];
				return [k, typeof v === "number" ? String(v) : `${typeof v}:${jsString(v)}`];
			}),
		),
		toolCalls: final.content
			.filter((b: { type: string }) => b.type === "toolCall")
			.map((b: { id: unknown; name: unknown; arguments: unknown }) =>
				J({ id: jsString(b.id), name: jsString(b.name), arguments: b.arguments }),
			),
		thinking: final.content
			.filter((b: { type: string }) => b.type === "thinking")
			.map((b: { thinking: string; thinkingSignature: unknown }) =>
				J({ thinking: b.thinking, thinkingSignature: jsString(b.thinkingSignature) }),
			),
	};
}

// jsString is String(v), "" for null, undefined and a value it throws on.
function jsString(v: unknown): string {
	if (v == null) return "";
	try {
		return String(v);
	} catch {
		return "";
	}
}

// ---- bodies --------------------------------------------------------------------

const J = (v: unknown) => JSON.stringify(v);
const BOM = "\ufeff";
const chunk = (id: string, content: string) => J({ id, choices: [{ index: 0, delta: { content } }] });
const FIN = J({ id: "f", choices: [{ index: 0, delta: {}, finish_reason: "stop" }] });
const A = chunk("a", "x");
const B = chunk("b", "y");
// invalidUTF8Content is a chunk whose content is "x", the bytes, "y".
const invalidUTF8Content = (bytes: number[]) =>
	Buffer.concat([
		Buffer.from(`data: {"id":"u","choices":[{"index":0,"delta":{"content":"x`),
		Buffer.from(bytes),
		Buffer.from(`y"}}]}\n\ndata: ${FIN}\n\n`),
	]);
const errorChunk = (error: unknown) => `data: ${A}\n\ndata: ${J({ error })}\n\ndata: ${FIN}\n\n`;

// Dispatch bodies are read by the SDK and both adapters.
const dispatch: Record<string, Body> = {
	"blank-line-dispatch": `data: ${A}\n\ndata: ${B}\n\ndata: ${FIN}\n\ndata: [DONE]\n\n`,
	"multi-line-data": `data: {"id":"m",\ndata: "choices":[{"index":0,"delta":{"content":"joined"}}]}\n\ndata: ${FIN}\n\n`,
	"unterminated-trailing-event": `data: ${A}\n\ndata: ${FIN}\n`,
	"unterminated-no-newline": `data: ${A}\n\ndata: ${FIN}`,
	// Only data that is exactly "[DONE]" ends the stream; anything else is JSON.
	"done-prefix": `data: ${A}\n\ndata: ${FIN}\n\ndata: [DONE]extra\n\ndata: ${B}\n\n`,
	"done-trailing-space": `data: ${A}\n\ndata: ${FIN}\n\ndata: [DONE] \n\n`,
	"done-exact-first": `data: [DONE]\n\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	"done-mid-line": `data: ${A}\n\ndata: [DONE\n\ndata: ${FIN}\n\n`,
	crlf: `data: ${A}\r\n\r\ndata: ${B}\r\n\r\ndata: ${FIN}\r\n\r\n`,
	"lone-cr": `data: ${A}\r\rdata: ${B}\r\rdata: ${FIN}\r\r`,
	// Any two line endings in a row end an event: "\n\r\n" and "\r\n\n" too.
	"lf-crlf-separators": `data: ${A}\n\r\ndata: ${B}\n\r\ndata: ${FIN}\n\r\n`,
	"crlf-lf-separators": `data: ${A}\r\n\ndata: ${B}\r\n\ndata: ${FIN}\r\n\n`,
	"mixed-line-endings": `data: ${A}\r\n\ndata: ${B}\n\r\ndata: ${FIN}\r\r\n`,
	// "\r\n" is one line ending, so an event's fields stay one event: a second
	// line ending inside it would dispatch the half read so far.
	"crlf-multi-line-data": `data: {"id":"m",\r\ndata: "choices":[{"index":0,"delta":{"content":"joined"}}]}\r\n\r\ndata: ${FIN}\r\n\r\n`,
	"crlf-thread-event": `event: thread.message\r\ndata: {"id":"t"}\r\n\r\ndata: ${A}\r\n\r\ndata: ${FIN}\r\n\r\n`,
	"comments-and-other-fields": `: keepalive\n\nid: 7\nretry: 100\ndata: ${A}\n\n: ping\n\ndata: ${FIN}\n\n`,
	"field-value-spacing": `data:${A}\n\ndata:  ${FIN}\n\n`,
	"data-field-without-colon": `data\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	"bom-at-line-start": `data: ${A}\n\n${BOM}data: ${B}\n\n${BOM}${BOM}data: ${chunk("c", "z")}\n\ndata: ${FIN}\n\n`,
	// An event named "error" fails the stream with the APIError for its data's
	// `error` member, or for the data itself when that member is null or
	// absent (`data?.error ?? data`); only the exact name counts.
	"event-named-error": `event: error\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	"event-named-error-with-error": `data: ${A}\n\nevent: error\ndata: ${J({ error: { message: "inner", metadata: { raw: "r2" } } })}\n\ndata: ${FIN}\n\n`,
	"event-named-error-null-error": `data: ${A}\n\nevent: error\ndata: ${J({ error: null, message: "m" })}\n\ndata: ${FIN}\n\n`,
	"event-named-error-falsy-error": `data: ${A}\n\nevent: error\ndata: ${J({ error: false, message: "m" })}\n\ndata: ${FIN}\n\n`,
	"event-named-error-metadata-raw": `data: ${A}\n\nevent: error\ndata: ${J({ message: "m", metadata: { raw: "r" } })}\n\ndata: ${FIN}\n\n`,
	"event-named-error-null": `data: ${A}\n\nevent: error\ndata: null\n\ndata: ${FIN}\n\n`,
	"event-named-error-string": `data: ${A}\n\nevent: error\ndata: "boom"\n\ndata: ${FIN}\n\n`,
	"event-named-error-array": `data: ${A}\n\nevent: error\ndata: [1,{"message":"m"}]\n\ndata: ${FIN}\n\n`,
	"event-named-error-unparseable": `data: ${A}\n\nevent: error\ndata: {oops\n\ndata: ${FIN}\n\n`,
	"event-named-error-case": `data: ${A}\n\nevent: Error\ndata: ${J({ message: "m" })}\n\ndata: ${FIN}\n\n`,
	// The OpenAI Responses API's own error event, named on its event: line.
	"event-named-error-responses-shape": `data: ${A}\n\nevent: error\ndata: ${J({ type: "error", code: "rate_limit_exceeded", message: "slow down", param: null, sequence_number: 1 })}\n\ndata: ${FIN}\n\n`,
	"event-name-empty": `event:\n\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	"thread-event": `event: thread.message\ndata: {"id":"t","error":{"message":"not thrown"}}\n\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	scalars: `data: 5\n\ndata: "s"\n\ndata: [1,{"k":2}]\n\ndata: true\n\ndata: false\n\ndata: 0\n\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	"null-event": `data: ${A}\n\ndata: null\n\ndata: ${FIN}\n\n`,
	"no-choices-chunks": `data: ${A}\n\ndata: ${J({ id: "u", choices: [], usage: { prompt_tokens: 3, completion_tokens: 1, total_tokens: 4 } })}\n\ndata: ${J({ id: "o", openrouter_metadata: { strategy: "direct" } })}\n\ndata: ${FIN}\n\n`,
	"key-order": `data: ${J({ z: 1, id: "k", a: { y: [{ q: 1, b: 2 }], x: null }, choices: [{ index: 0, delta: { content: "<&>" } }] })}\n\ndata: ${FIN}\n\n`,
	// JSON.parse reads a number past float64's range as ±Infinity, which
	// JSON.stringify writes as null; written out, since J() would already
	// have made it null.
	"numbers-past-float64": `data: {"id":"a","x":1e400,"y":-1e400,"z":1e-400,"w":[1e400,{"v":-1e400}],"choices":[{"index":0,"delta":{"content":"x"}}]}\n\ndata: 1e400\n\ndata: -0\n\ndata: ${FIN}\n\n`,
	// Written out, not J(): a JS object literal would already list its
	// array-index keys first, ascending, as JSON.parse's object does.
	"index-keys": `data: {"z":1,"id":"k","2":"x","1":"y","-1":0,"01":2,"4294967295":3,"4294967294":4,"choices":[{"index":0,"delta":{"content":"i"},"logprobs":{"2":1,"1":2}}]}\n\ndata: ${FIN}\n\n`,
	"error-chunk-openrouter": errorChunk({ message: "boom", code: 502, metadata: { raw: "upstream exploded", provider_name: "p" } }),
	"error-chunk-raw-contained": errorChunk({ message: "boom: upstream exploded", metadata: { raw: "upstream exploded" } }),
	"error-chunk-raw-number": errorChunk({ message: "boom", metadata: { raw: 5 } }),
	"error-chunk-raw-object": errorChunk({ message: "boom", metadata: { raw: { a: 1 } } }),
	"error-chunk-raw-array": errorChunk({ message: "boom", metadata: { raw: [1, [2, null], "x"] } }),
	"error-chunk-raw-true": errorChunk({ message: "boom", metadata: { raw: true } }),
	"error-chunk-raw-falsy": errorChunk({ message: "boom", metadata: { raw: "" } }),
	// Falsy raws whose String() the message does not contain: only truthiness
	// keeps them off it.
	"error-chunk-raw-zero": errorChunk({ message: "boom", metadata: { raw: 0 } }),
	"error-chunk-raw-false": errorChunk({ message: "boom", metadata: { raw: false } }),
	"error-chunk-raw-null": errorChunk({ message: "boom", metadata: { raw: null } }),
	"error-chunk-metadata-string": errorChunk({ message: "boom", metadata: "raw" }),
	"error-chunk-no-message": errorChunk({ code: "x", type: "y" }),
	"error-chunk-message-object": errorChunk({ message: { b: 1, a: 2 } }),
	"error-chunk-message-number": errorChunk({ message: 42 }),
	"error-chunk-message-empty": errorChunk({ message: "", code: 1 }),
	"error-chunk-message-escapes": errorChunk({ message: 'caf\u00e9 </script> "q"' }),
	"error-chunk-string": errorChunk("boom"),
	"error-chunk-number": errorChunk(7),
	"error-chunk-true": errorChunk(true),
	"error-chunk-empty-object": errorChunk({}),
	"error-chunk-empty-array": errorChunk([]),
	"error-chunk-integer-keys": errorChunk({ b: 1, 2: "x", 1: "y", code: 1.5e21 }),
	// An error item whose message names Expo fetch's
	// FetchRequestCanceledException fails the stream like any other: openai
	// 6's Stream swallowed it, taking it for an abort; 7.19.0's catch spares
	// only a transport abort, never an APIError.
	"error-chunk-fetch-canceled": errorChunk({ message: "upstream: FetchRequestCanceledException" }),
	"error-chunk-fetch-canceled-string": errorChunk("FetchRequestCanceledException"),
	"error-chunk-fetch-canceled-object-message": errorChunk({ message: { cause: "FetchRequestCanceledException" } }),
	"error-chunk-fetch-canceled-then-finish": `data: ${A}\n\ndata: ${FIN}\n\ndata: ${J({ error: { message: "FetchRequestCanceledException" } })}\n\ndata: ${B}\n\n`,
	"error-chunk-fetch-canceled-in-code": errorChunk({ message: "boom", code: "FetchRequestCanceledException" }),
	"error-values-falsy": `data: ${J({ id: "n", error: null })}\n\ndata: ${J({ id: "e", error: "" })}\n\ndata: ${J({ id: "z", error: 0 })}\n\ndata: ${J({ id: "f", error: false })}\n\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	"error-key-case": `data: ${J({ Error: { message: "boom" } })}\n\ndata: ${A}\n\ndata: ${FIN}\n\n`,
	"error-after-done": `data: ${A}\n\ndata: ${FIN}\n\ndata: [DONE]\n\ndata: ${J({ error: { message: "late" } })}\n\n`,
	// JSON.parse rejects these, and the SDK fails the stream with its own
	// SyntaxError text, whatever JSON.parse's reason. Nothing is repaired
	// first, however little would fix it: a raw control character in a string,
	// an escape JSON does not have.
	"unparseable-data": `data: ${A}\n\ndata: {not json\n\ndata: ${FIN}\n\n`,
	"event-without-data": `data: ${A}\n\nevent: ping\n\ndata: ${FIN}\n\n`,
	"control-character-in-string": `data: ${A}\n\ndata: {"id":"c","choices":[{"index":0,"delta":{"content":"a\tb"}}]}\n\ndata: ${FIN}\n\n`,
	"invalid-escape": `data: ${A}\n\ndata: {"id":"c","choices":[{"index":0,"delta":{"content":"a\\qb"}}]}\n\ndata: ${FIN}\n\n`,
	"unexpected-token-in-context": `data: ${A}\n\ndata: {"id":"c","choices":[],"n":nul}\n\ndata: ${FIN}\n\n`,
	"thread-event-unparseable": `data: ${A}\n\nevent: thread.run\ndata: {not json\n\ndata: ${FIN}\n\n`,
	// [DONE] ends the stream: the SDK reads nothing after it.
	"unparseable-after-done": `data: ${A}\n\ndata: ${FIN}\n\ndata: [DONE]\n\ndata: {not json\n\n`,
	// Each line is decoded by a TextDecoder, which writes one U+FFFD per
	// maximal subpart of an invalid UTF-8 sequence (WHATWG), not one per byte.
	"utf8-truncated-3": invalidUTF8Content([0xe2, 0x82]),
	"utf8-truncated-4": invalidUTF8Content([0xf0, 0x9f, 0x98]),
	"utf8-overlong": invalidUTF8Content([0xc0, 0xaf]),
	"utf8-surrogate": invalidUTF8Content([0xed, 0xa0, 0x80]),
	"utf8-above-max": invalidUTF8Content([0xf4, 0x90, 0x80, 0x80]),
	"utf8-lone-continuation": invalidUTF8Content([0x80, 0xbf]),
	"utf8-invalid-lead": invalidUTF8Content([0xff, 0xe0, 0x80]),
	"utf8-truncated-then-valid": invalidUTF8Content([0xe2, 0x82, 0xe2, 0x82, 0xac]),
	"utf8-in-event-name": Buffer.concat([
		Buffer.from("event: thread.m"),
		Buffer.from([0xe2, 0x82]),
		Buffer.from(`\ndata: {"id":"t"}\n\ndata: ${A}\n\ndata: ${FIN}\n\n`),
	]),
};

// Adapter-specific bodies.
// C is a completions body of its chunks, each written out verbatim.
const C = (...chunks: string[]) => chunks.map((c) => `data: ${c}\n\n`).join("");
const hi = (fields: string) => `{"id":"a",${fields}"choices":[{"index":0,"delta":{"content":"hi"}}]}`;
const withDelta = (delta: string, extra = "") => `{"id":"a","choices":[{"index":0,"delta":${delta}${extra}}]}`;
const TOOL_FIN = J({ id: "a", choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }] });
// cutOff is a tool call whose arguments stream as far as args and then stop
// at finish_reason "length": pi keeps what parseStreamingJson makes of them.
const BS = String.fromCharCode(92);
const cutOff = (args: string) =>
	C(
		J({ id: "a", choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: "c1", function: { name: "f", arguments: args } }] } }] }),
		J({ id: "a", choices: [{ index: 0, delta: {}, finish_reason: "length" }] }),
	);
const completionsBodies: Record<string, string> = {
	// pi reads each member it uses off the parsed chunk with JS semantics —
	// exact keys, any type, its own typeof / Array.isArray / truthiness guards,
	// JS's conversions where it concatenates or computes — so no member can
	// cost the chunk's other members.
	"id-number": C(`{"id":5,"choices":[{"index":0,"delta":{"content":"hi"}}]}`, FIN),
	"id-zero-then-string": C(`{"id":0,"choices":[{"index":0,"delta":{"content":"hi"}}]}`, FIN),
	"id-key-case": C(`{"ID":"zz","choices":[{"index":0,"delta":{"content":"hi"}}]}`, FIN),
	"model-number": C(hi(`"model":5,`), FIN),
	"model-other": C(hi(`"model":"other/model",`), FIN),
	"usage-fraction": C(hi(`"usage":{"prompt_tokens":1.5,"completion_tokens":2},`), FIN),
	"usage-numeric-string": C(hi(`"usage":{"prompt_tokens":"3","completion_tokens":2},`), FIN),
	"usage-string-output": C(hi(`"usage":{"prompt_tokens":3,"completion_tokens":"2"},`), FIN),
	"usage-nullish-cached": C(hi(`"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":4,"cached_tokens":3},`), FIN),
	"usage-nullish-fallthrough": C(hi(`"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":null},"prompt_cache_hit_tokens":null,"cached_tokens":3,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":1}},`), FIN),
	"usage-uncoercible": C(hi(`"usage":{"prompt_tokens":{"toString":1}},`), FIN),
	"usage-scalar": C(hi(`"usage":5,`), FIN),
	"choice-usage-string": C(`{"id":"a","choices":[{"index":0,"delta":{"content":"hi"},"usage":"x"}]}`, FIN),
	"choice-usage-fallback": C(`{"id":"a","choices":[{"index":0,"delta":{"content":"hi"},"usage":{"prompt_tokens":7,"completion_tokens":1}}]}`, FIN),
	"choices-object": C(`{"id":"a","choices":{"0":{"index":0,"delta":{"content":"hi"}}}}`, FIN),
	"choice-scalar": C(`{"id":"a","choices":[5]}`, `{"id":"b","choices":[null]}`, `{"id":"c","choices":[]}`, FIN),
	"delta-string": C(`{"id":"a","choices":[{"index":0,"delta":"hi"}]}`, FIN),
	"content-array": C(withDelta(`{"content":["q",["r",null],null,"s"]}`), FIN),
	"content-empty-array": C(withDelta(`{"content":[]}`), FIN),
	"content-number": C(withDelta(`{"content":5}`), withDelta(`{"content":true}`), FIN),
	"content-object-length": C(withDelta(`{"content":{"length":2}}`), withDelta(`{"content":{"length":"0"}}`), withDelta(`{"content":{"length":[1]}}`), FIN),
	"content-uncoercible": C(withDelta(`{"content":{"length":1,"toString":1}}`), FIN),
	"content-uncoercible-length": C(withDelta(`{"content":{"length":{"toString":1}}}`), FIN),
	"content-key-case": C(withDelta(`{"Content":"hi"}`), FIN),
	"content-duplicate-case": C(withDelta(`{"content":"hi","CONTENT":"x"}`), FIN),
	"reasoning-number": C(withDelta(`{"content":"hi","reasoning":5,"reasoning_text":"r"}`), FIN),
	"finish-number": C(`{"id":"a","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":5}]}`),
	"finish-array": C(`{"id":"a","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":["length"]}]}`),
	"finish-uncoercible": C(`{"id":"a","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":{"toString":1}}]}`),
	"tool-index-string": C(withDelta(`{"tool_calls":[{"index":"0","id":"c1","type":"function","function":{"name":"f","arguments":"{\\"a\\":1}"}}]}`), TOOL_FIN),
	"tool-index-fraction": C(
		withDelta(`{"tool_calls":[{"index":0.5,"id":"c1","function":{"name":"f","arguments":"{\\"a\\":"}}]}`),
		withDelta(`{"tool_calls":[{"index":0.5,"function":{"arguments":"1}"}}]}`),
		TOOL_FIN,
	),
	"tool-id-number-key": C(
		withDelta(`{"tool_calls":[{"id":7,"function":{"name":"f","arguments":"{\\"a\\":"}}]}`),
		withDelta(`{"tool_calls":[{"id":"7","function":{"name":"g","arguments":"{\\"b\\":2}"}}]}`),
		withDelta(`{"tool_calls":[{"id":7,"function":{"arguments":"1}"}}]}`),
		TOOL_FIN,
	),
	"tool-name-number": C(withDelta(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":5,"arguments":"{}"}}]}`), TOOL_FIN),
	"tool-arguments-number": C(withDelta(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\\"a\\":"}}]}`), withDelta(`{"tool_calls":[{"index":0,"function":{"arguments":7}}]}`), withDelta(`{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}`), TOOL_FIN),
	"tool-calls-string": C(withDelta(`{"tool_calls":"ab"}`), TOOL_FIN),
	"tool-calls-number": C(withDelta(`{"content":"hi","tool_calls":5}`), TOOL_FIN),
	"tool-calls-object": C(withDelta(`{"tool_calls":{"0":{"index":0}}}`), TOOL_FIN),
	"tool-call-null-entry": C(withDelta(`{"tool_calls":[null]}`), TOOL_FIN),
	"tool-call-scalar-entries": C(withDelta(`{"tool_calls":[5,"x",true,[]]}`), TOOL_FIN),
	"tool-call-key-case": C(withDelta(`{"tool_calls":[{"Index":0,"ID":"c1","Function":{"name":"f","arguments":"{}"},"function":{"NAME":"g","Arguments":"{}"}}]}`), TOOL_FIN),
	"tool-custom-null": C(withDelta(`{"tool_calls":[{"index":0,"id":"c1","custom":null}]}`), TOOL_FIN),
	"tool-custom-input": C(
		withDelta(`{"tool_calls":[{"index":0,"id":"c1","custom":{"name":"g","input":"abc"}}]}`),
		withDelta(`{"tool_calls":[{"index":0,"custom":{"input":5}}]}`),
		TOOL_FIN,
	),
	"reasoning-details": C(
		withDelta(`{"reasoning_details":[{"type":"reasoning.text","text":"a","index":0},{"type":"reasoning.text","text":"b","index":0,"format":"f"}]}`),
		withDelta(`{"reasoning_details":[{"type":"reasoning.encrypted","data":"x","id":"e1"},{"type":"reasoning.summary","summary":"s","2":1,"1":2}]}`),
		FIN,
	),
	"reasoning-details-invalid": C(
		withDelta(
			`{"reasoning_details":[{"type":"reasoning.text","text":"a","index":"0"},{"type":"reasoning.text","text":"b","id":5},` +
				`{"type":"reasoning.text","text":"c","format":null},{"type":"reasoning.text","text":"d","signature":5},` +
				`{"type":"reasoning.summary","summary":1},{"type":"reasoning.encrypted","data":null},{"type":"x"},null,"s",[],` +
				`{"type":"reasoning.text","text":"ok","signature":null,"id":null}]}`,
		),
		FIN,
	),
	// A number past float64's range is still a number, which JSON.stringify
	// writes null; written out, since J() would already have made it null.
	"reasoning-details-index-past-float64": C(withDelta(`{"reasoning_details":[{"type":"reasoning.text","text":"a","index":1e400}]}`), FIN),
	"length-partial-literal": cutOff(`{"path":"a.txt","overwrite":tr`),
	"length-partial-number": cutOff(`{"path":"a.txt","n":1e`),
	"length-minus": cutOff(`{"path":"a.txt","n":-`),
	"length-trailing-backslash": cutOff(`{"path":"C:${BS}`),
	"length-partial-unicode-escape": cutOff(`{"path":"a.txt","s":"x${BS}u00`),
	"length-dangling-key": cutOff(`{"path":"a.txt","content`),
	"length-infinity": cutOff(`{"path":"a.txt","n":Infinity,"m":1e400,"k":Na`),
	"reasoning-details-object": C(withDelta(`{"content":"hi","reasoning_details":{"type":"reasoning.text","text":"x"}}`), FIN),
	"array-chunk": C(`[{"id":"x","choices":[{"index":0,"delta":{"content":"no"}}]}]`, hi(""), FIN),
	// packages/ai/test/openai-completions-provider-stream-event.test.ts
	"openrouter-metadata": `data: ${J({
		id: "chatcmpl-1",
		model: "anthropic/claude-sonnet-4.6",
		choices: [{ index: 0, delta: { content: "hello" } }],
	})}\n\ndata: ${J({
		id: "chatcmpl-1",
		model: "anthropic/claude-sonnet-4.6",
		choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
		usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12, cost: 0.0012, is_byok: false },
		openrouter_metadata: { strategy: "direct", region: "iad" },
	})}\n\ndata: [DONE]\n\n`,
};

const R = (v: Record<string, unknown>) => `data: ${J(v)}\n\n`;
const created = R({ type: "response.created", sequence_number: 0, response: { id: "resp_1" } });
const messageItem = { type: "message", id: "msg_1", role: "assistant", content: [] };
const textEvents =
	R({ type: "response.output_item.added", output_index: 0, item: messageItem }) +
	R({ type: "response.content_part.added", output_index: 0, part: { type: "output_text", text: "" } }) +
	R({ type: "response.output_text.delta", output_index: 0, delta: "hi" }) +
	R({
		type: "response.output_item.done",
		output_index: 0,
		item: { ...messageItem, content: [{ type: "output_text", text: "hi" }] },
	});
const completed = R({
	type: "response.completed",
	response: { id: "resp_1", status: "completed", usage: { input_tokens: 5, output_tokens: 1, total_tokens: 6 } },
});
const errorEvent = (fields: Record<string, unknown>) => created + R({ type: "error", ...fields }) + completed;
const failed = (response: Record<string, unknown>) =>
	created + R({ type: "response.failed", response: { id: "resp_1", status: "failed", ...response } });

// backfill is a stream whose reasoning item rs_1 ends without
// encrypted_content (plus stored's members) and whose completed response
// lists it with outputItem's members.
const backfill = (outputItem: Record<string, unknown>, stored: Record<string, unknown> = {}) =>
	created +
	R({ type: "response.output_item.added", output_index: 0, item: { type: "reasoning", id: "rs_1", summary: [] } }) +
	R({
		type: "response.output_item.done",
		output_index: 0,
		item: { type: "reasoning", id: "rs_1", summary: [{ type: "summary_text", text: "t" }], ...stored },
	}) +
	R({
		type: "response.completed",
		response: { id: "resp_1", status: "completed", output: [{ type: "reasoning", id: "rs_1", summary: [], ...outputItem }] },
	});

const responsesBodies: Record<string, string> = {
	// packages/ai/test/openai-responses-terminal-event.test.ts, as SSE.
	"terminal-missing":
		R({ type: "response.created", sequence_number: 0, response: { id: "resp_wrapper_early_eof" } }) +
		R({
			type: "response.output_item.added",
			sequence_number: 1,
			output_index: 0,
			item: { type: "reasoning", id: "rs_wrapper_early_eof", summary: [] },
		}) +
		R({
			type: "response.reasoning_text.delta",
			sequence_number: 2,
			output_index: 0,
			content_index: 0,
			item_id: "rs_wrapper_early_eof",
			delta: "partial reasoning before the wrapper stream ends",
		}),
	completed: created + textEvents + completed,
	"null-mid-stream": `${created}data: null\n\n${textEvents}${completed}`,
	"scalar-events": `${created}data: 5\n\ndata: "s"\n\ndata: [1]\n\ndata: true\n\ndata: false\n\ndata: 0\n\n${textEvents}${completed}`,
	"unknown-event-types": created + R({ type: "response.in_progress", response: { id: "resp_1" } }) + R({ id: "no-type" }) + textEvents + completed,
	"error-event": errorEvent({ code: "rate_limit_exceeded", message: "slow down", param: null, sequence_number: 1 }),
	"error-event-no-code": errorEvent({ message: "slow down" }),
	"error-event-null-code": errorEvent({ code: null, message: "slow down" }),
	"error-event-number-code": errorEvent({ code: 500, message: "slow down" }),
	"error-event-no-message": errorEvent({ code: "x" }),
	"error-event-object-message": errorEvent({ code: "x", message: { a: 1 } }),
	"error-event-array-code": errorEvent({ code: [1, [2, null]], message: "m" }),
	"error-event-uncoercible-code": errorEvent({ code: { toString: 1 }, message: "m" }),
	"error-event-code-past-float64": `${created}data: {"type":"error","code":1e400,"message":"m"}\n\n${completed}`,
	"response-failed":
		created +
		R({
			type: "response.failed",
			response: { id: "resp_1", status: "failed", error: { code: "server_error", message: "bad" } },
		}),
	"response-failed-number-code": failed({ error: { code: 500, message: "bad" } }),
	"response-failed-true-code": failed({ error: { code: true, message: "bad" } }),
	"response-failed-falsy-members": failed({ error: { code: "", message: 0 } }),
	"response-failed-object-message": failed({ error: { code: "x", message: { a: 1 } } }),
	"response-failed-uncoercible-message": failed({ error: { code: "x", message: { toString: 1 } } }),
	"response-failed-string-error": failed({ error: "boom", incomplete_details: { reason: "max_output_tokens" } }),
	"response-failed-empty-error": failed({ error: {}, incomplete_details: { reason: "max_output_tokens" } }),
	"response-failed-false-error": failed({ error: false, incomplete_details: { reason: "max_output_tokens" } }),
	"response-failed-number-reason": failed({ error: null, incomplete_details: { reason: 5 } }),
	"response-failed-empty-reason": failed({ incomplete_details: { reason: "" } }),
	"response-failed-no-details": failed({}),
	"incomplete-number-reason":
		created +
		textEvents +
		R({
			type: "response.incomplete",
			response: { id: "resp_1", status: "incomplete", incomplete_details: { reason: 5 } },
		}),
	// pi reads each member it uses off the parsed event with JS semantics: the
	// key is exact (a JS property read), a member of any type is read as
	// whatever it is, and reading off a null or absent response throws.
	"error-event-code-key-case": errorEvent({ CODE: "x", message: "m" }),
	"error-event-code-duplicate-case": `${created}data: {"type":"error","code":"a","Code":"b","message":"m"}\n\n${completed}`,
	"type-key-case": `${created}${textEvents}data: {"TYPE":"response.completed","response":{"id":"resp_1","status":"completed"}}\n\n`,
	"type-duplicate-case": `${created}${textEvents}data: {"type":"response.completed","Type":"x","response":{"id":"resp_1","status":"completed"}}\n\n`,
	"response-key-case": `${created}data: {"type":"response.failed","Response":{"error":{"code":"c","message":"m"}}}\n\n`,
	"response-failed-error-key-case": failed({ ERROR: { code: "c", message: "m" } }),
	"response-failed-number-response": created + R({ type: "response.failed", response: 5 }),
	"response-failed-null-response": created + R({ type: "response.failed", response: null }),
	"response-failed-number-status": failed({ status: 5, error: { code: "c", message: "m" } }),
	"response-failed-number-id": failed({ id: 5, error: { code: "c", message: "m" } }),
	"created-null-response": R({ type: "response.created", response: null }) + textEvents + completed,
	"created-no-response": R({ type: "response.created" }) + textEvents + completed,
	"created-number-response":
		R({ type: "response.created", response: 5 }) +
		textEvents +
		R({ type: "response.completed", response: { status: "completed" } }),
	"completed-null-response": created + textEvents + R({ type: "response.completed", response: null }),
	"completed-no-response": created + textEvents + R({ type: "response.completed" }),
	"completed-string-response": created + textEvents + R({ type: "response.completed", response: "x" }),
	"completed-number-id": created + textEvents + R({ type: "response.completed", response: { id: 5, status: "completed" } }),
	"completed-false-id": created + textEvents + R({ type: "response.completed", response: { id: false, status: "completed" } }),
	"completed-number-status": created + textEvents + R({ type: "response.incomplete", response: { id: "resp_1", status: 5 } }),
	"completed-false-status": created + textEvents + R({ type: "response.completed", response: { id: "resp_1", status: false } }),
	"completed-object-status-with-reason":
		created +
		textEvents +
		R({ type: "response.completed", response: { status: { a: 1 }, incomplete_details: { reason: "r" } } }),
	"completed-no-status-with-reason":
		created + textEvents + R({ type: "response.incomplete", response: { id: "resp_1", incomplete_details: { reason: "max_output_tokens" } } }),
	"completed-null-status-with-reason":
		created +
		textEvents +
		R({ type: "response.incomplete", response: { id: "resp_1", status: null, incomplete_details: { reason: "max_output_tokens" } } }),
	"completed-uncoercible-status-with-reason":
		created +
		textEvents +
		R({ type: "response.completed", response: { status: { toString: 1 }, incomplete_details: { reason: "r" } } }),
	"completed-number-output": created + textEvents + R({ type: "response.completed", response: { status: "completed", output: 5 } }),
	"completed-object-output": created + textEvents + R({ type: "response.completed", response: { status: "completed", output: {} } }),
	"completed-string-output": created + textEvents + R({ type: "response.completed", response: { status: "completed", output: "ab" } }),
	"completed-null-output-item": created + textEvents + R({ type: "response.completed", response: { status: "completed", output: [null] } }),
	"completed-scalar-output-items":
		created + textEvents + R({ type: "response.completed", response: { status: "completed", output: [5, "x", true, []] } }),
	"completed-string-usage-member":
		created +
		textEvents +
		R({ type: "response.completed", response: { status: "completed", usage: { input_tokens: "5", output_tokens: 1, total_tokens: 6 } } }),
	"completed-fractional-usage-member":
		created +
		textEvents +
		R({ type: "response.completed", response: { status: "completed", usage: { input_tokens: 1.5, output_tokens: 1, total_tokens: 6 } } }),
	// A reasoning item's signature is JSON.stringify(item) at
	// output_item.done — the parsed item, not the provider's bytes — and a
	// completed response's truthy encrypted_content is backfilled into it as
	// JSON.stringify({...storedItem, encrypted_content}).
	"reasoning-signature-normalized":
		created +
		R({ type: "response.output_item.added", output_index: 0, item: { type: "reasoning", id: "rs_1", summary: [] } }) +
		`data: {"type":"response.output_item.done","output_index":0,"item": {"type":"reasoning", "id":"rs_1","summary":[{"type":"summary_text","text":"t\\u00e9 </x>"}],"2":1,"x":1.0,"x":1E2,"1":[-0]}}\n\n` +
		completed,
	"backfill-string": backfill({ encrypted_content: "enc" }),
	"backfill-html": backfill({ encrypted_content: "a<b" }, { x: "<&>" }),
	"backfill-number": backfill({ encrypted_content: 5 }),
	"backfill-object": backfill({ encrypted_content: { b: 1, a: [2] } }),
	"backfill-falsy": backfill({ encrypted_content: "" }),
	"backfill-stored-empty": backfill({ encrypted_content: "enc" }, { encrypted_content: "", z: 1 }),
	"backfill-stored-null": backfill({ encrypted_content: "enc" }, { encrypted_content: null, z: 1 }),
	"backfill-stored-set": backfill({ encrypted_content: "enc" }, { encrypted_content: "old" }),
	"backfill-id-absent": backfill({ id: undefined, encrypted_content: "enc" }, { id: undefined }),
	"backfill-other-id": backfill({ id: "rs_2", encrypted_content: "enc" }),
	// Members the typed events carry, mistyped on an event that ends the
	// stream, which pi never reads there.
	"completed-mistyped-other-member":
		created + textEvents + R({ type: "response.completed", output_index: "x", item: 5, response: { id: "resp_1", status: "completed" } }),
	"error-event-mistyped-other-member": errorEvent({ code: "c", message: "m", delta: 5, part: "p" }),
	// A response cut off mid-arguments keeps what parseStreamingJson makes of
	// the text streamed so far.
	"incomplete-partial-arguments":
		created +
		R({ type: "response.output_item.added", output_index: 0, item: { type: "function_call", id: "fc_1", call_id: "call_1", name: "f", arguments: "" } }) +
		R({ type: "response.function_call_arguments.delta", output_index: 0, delta: `{"path":"a.txt","overwrite":fal` }) +
		R({ type: "response.incomplete", response: { id: "resp_1", status: "incomplete", incomplete_details: { reason: "max_output_tokens" } } }),
	"fetch-canceled-after-completed": `${created}${textEvents}${completed}data: ${J({ error: { message: "FetchRequestCanceledException" } })}\n\n`,
	"fetch-canceled-before-completed": `${created}${textEvents}data: ${J({ error: { message: "FetchRequestCanceledException" } })}\n\n${completed}`,
	"completed-number-service-tier":
		created + textEvents + R({ type: "response.completed", response: { status: "completed", service_tier: 5 } }),
};

// ---- pricing ---------------------------------------------------------------------

// Pricing bodies end in response.completed with a million input tokens, read
// through responses.stream with the serviceTier option on a model priced at
// 1 per million input tokens: the recorded cost is the tier's multiplier.
// The response's service_tier wins over the option unless it is null or
// absent (`response?.service_tier ?? options.serviceTier`).
const pricedModel = { ...responsesModel, cost: { input: 1, output: 0, cacheRead: 0, cacheWrite: 0 } };
const pricedBody = (serviceTier: Record<string, unknown>) =>
	created +
	R({
		type: "response.completed",
		response: {
			id: "resp_1",
			status: "completed",
			usage: { input_tokens: 1000000, output_tokens: 0, total_tokens: 1000000 },
			...serviceTier,
		},
	});
const pricingBodies: Record<string, [string, string]> = {
	"tier-absent": [pricedBody({}), "priority"],
	"tier-null": [pricedBody({ service_tier: null }), "priority"],
	"tier-empty": [pricedBody({ service_tier: "" }), "priority"],
	"tier-number": [pricedBody({ service_tier: 5 }), "priority"],
	"tier-flex": [pricedBody({ service_tier: "flex" }), "priority"],
	"tier-default": [pricedBody({ service_tier: "default" }), "priority"],
};

// ---- line splitting --------------------------------------------------------------

// The SDK's LineDecoder gets a body in iterSSEChunks' pieces, each ending at
// an event separator, so the dispatch bodies read one byte per read above
// never reach it with a line ending cut in two. The port's line splitter does
// meet one: its scanner's buffer can end inside a piece (an event of 64 KiB
// or more). Each sequence here is decoded one chunk at a time and then
// flushed, as the body's end flushes it.
const lineChunks: Record<string, string[]> = {
	"crlf-split": ["a\r", "\nb"],
	"crlf-split-thrice": ["a", "\r", "\n", "b\r\n"],
	"blank-crlf-split": ["\r", "\n\r", "\n"],
	"crlf-then-crlf-split": ["a\r", "\n", "\r", "\n"],
	"cr-then-text": ["a\r", "b\n"],
	"cr-then-cr": ["a\r", "\r"],
	"cr-cr-then-lf": ["a\r\r", "\nb"],
	"cr-at-end": ["a\r"],
	"cr-text-in-one": ["a\rb"],
	"lf-then-cr": ["a\n", "\rb"],
};

function sdkLines(chunks: string[]): string[] {
	const decoder = new LineDecoder();
	const lines: string[] = chunks.flatMap((c) => decoder.decode(new TextEncoder().encode(c)));
	return [...lines, ...decoder.flush()];
}

async function piPricing(body: string, serviceTier: string) {
	const final = await responses
		.stream({ ...pricedModel, baseUrl: sse(body) }, context, { apiKey: "k", serviceTier })
		.result();
	return { stopReason: final.stopReason, costTotal: final.usage.cost.total };
}

// ---- capture ---------------------------------------------------------------------

// A Row's body is `sse`, or `sseBase64` when it is bytes that are not UTF-8.
type Row = { sse?: string; sseBase64?: string; sdk?: SDKReadings; completions?: Outcome; responses?: Outcome };
const bodyFields = (body: Body) =>
	typeof body === "string" ? { sse: body } : { sseBase64: body.toString("base64") };
const out: {
	sha: string;
	openai: string;
	openaiIntegrity: string;
	node: string;
	dispatch: Record<string, Row>;
	completions: Record<string, Row>;
	responses: Record<string, Row>;
	hooks: Record<
		string,
		{
			adapter: string;
			sse: string;
			status: number;
			hold: boolean;
			abortOnEvent: number;
			// customNullBody: a custom fetch answered with `new Response(null, { status })`.
			customNullBody?: boolean;
			outcome: Outcome;
		}
	>;
	pricing: Record<string, { sse: string; serviceTier: string; stopReason: string; costTotal: number }>;
	lines: Record<string, { chunks: string[]; lines: string[] }>;
} = {
	sha,
	openai: openaiVersion,
	openaiIntegrity: openai.integrity,
	node: process.version,
	dispatch: {},
	completions: {},
	responses: {},
	hooks: {},
	pricing: {},
	lines: {},
};

for (const [name, body] of Object.entries(dispatch)) {
	out.dispatch[name] = {
		...bodyFields(body),
		sdk: await sdkReading(body),
		completions: await piRun(completions, completionsModel, sse(body)),
		responses: await piRun(responses, responsesModel, sse(body)),
	};
}
for (const [name, body] of Object.entries(completionsBodies)) {
	out.completions[name] = { sse: body, completions: await piRun(completions, completionsModel, sse(body)) };
}
for (const [name, body] of Object.entries(responsesBodies)) {
	out.responses[name] = { sse: body, responses: await piRun(responses, responsesModel, sse(body)) };
}

const hookBodies = { completions: dispatch["blank-line-dispatch"], responses: responsesBodies.completed };
const adapters = {
	completions: [completions, completionsModel],
	responses: [responses, responsesModel],
} as const;
// The request is aborted as the observer sees the last event of a held body:
// the SDK's Stream, which races its next body read against the signal, then
// simply ends, so the adapter's loop ends and its post-loop checks decide the
// message. Responses checks for a terminal event before its abort guard.
const eventCount = (body: string) =>
	body.split("\n\n").filter((e) => e.startsWith("data: ") && !e.startsWith("data: [DONE]")).length;
const doneHeldBodies = { completions: dispatch["blank-line-dispatch"], responses: `${responsesBodies.completed}data: [DONE]\n\n` };
const heldCases: Record<string, Record<string, string>> = {
	completions: {
		"abort-held": `data: ${A}\n\n`,
		"abort-held-after-finish": dispatch["blank-line-dispatch"],
	},
	responses: {
		"abort-held": created + textEvents,
		"abort-held-after-terminal": responsesBodies.completed,
	},
};
for (const [adapter, [api, model]] of Object.entries(adapters)) {
	const body = hookBodies[adapter as keyof typeof hookBodies];
	const cases: Record<string, [Reply, Hooks]> = {
		"callback-throws": [{ status: 200, contentType: "text/event-stream", body }, { failOnEvent: "throw" }],
		"callback-aborts": [{ status: 200, contentType: "text/event-stream", body }, { failOnEvent: "abort-then-throw" }],
		"on-response-throws": [{ status: 200, contentType: "text/event-stream", body }, { failOnResponse: true }],
		"http-400": [{ status: 400, contentType: "application/json", body: J({ error: { message: "bad request" } }) }, {}],
		"http-400-on-response-throws": [
			{ status: 400, contentType: "application/json", body: J({ error: { message: "bad request" } }) },
			{ failOnResponse: true },
		],
	};
	for (const [name, heldBody] of Object.entries(heldCases[adapter])) {
		cases[name] = [
			{ status: 200, contentType: "text/event-stream", body: heldBody, hold: true },
			{ abortOnEvent: eventCount(heldBody) },
		];
	}
	// A body that ends in [DONE] and is then held open, with no abort: the SDK
	// reads nothing after [DONE], so the stream ends while the server still
	// holds the connection.
	cases["done-then-held"] = [{ status: 200, contentType: "text/event-stream", body: doneHeldBodies[adapter as keyof typeof doneHeldBodies], hold: true }, {}];
	// A 2xx response whose body is null: fetch gives a 204 or 205 one whatever
	// the server sends, and a custom fetch can answer with one at any status.
	// The SDK's Stream throws on its first iteration, after onResponse and
	// start; a non-2xx one reads as an empty body. An empty body is not null.
	cases["empty-body"] = [{ status: 200, contentType: "text/event-stream", body: "" }, {}];
	cases["null-body-204"] = [{ status: 204, contentType: "text/event-stream", body: "" }, {}];
	cases["null-body-205"] = [{ status: 205, contentType: "text/event-stream", body }, {}];
	cases["custom-fetch-null-body"] = [{ status: 200, contentType: "text/event-stream", body: "" }, { nullBodyStatus: 200 }];
	cases["custom-fetch-null-body-500"] = [{ status: 500, contentType: "application/json", body: "" }, { nullBodyStatus: 500 }];
	for (const [name, [reply, hooks]] of Object.entries(cases)) {
		out.hooks[`${adapter}/${name}`] = {
			adapter,
			sse: reply.body,
			status: reply.status,
			hold: reply.hold ?? false,
			abortOnEvent: hooks.abortOnEvent ?? 0,
			...(hooks.nullBodyStatus === undefined ? {} : { customNullBody: true }),
			outcome: await piRun(api, model, route(reply), hooks),
		};
	}
}

for (const [name, [body, serviceTier]] of Object.entries(pricingBodies)) {
	out.pricing[name] = { sse: body, serviceTier, ...(await piPricing(body, serviceTier)) };
}
for (const [name, chunks] of Object.entries(lineChunks)) {
	out.lines[name] = { chunks, lines: sdkLines(chunks) };
}

for (const res of held) res.destroy();
server.close();
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${outFile}`);
