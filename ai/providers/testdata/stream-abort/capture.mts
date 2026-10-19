// Captures how pi's adapters end a stream whose signal aborts once the
// request is on its way, or whose connection drops mid-body — the oracle
// behind TestStreamAbortMatchesPi in this package.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> stream-abort-71023aec1.json 71023aec1
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone, plus a node_modules resolving pi-ai's dependencies (the npm
// build's). The script refuses to write unless each SDK it runs
// (@anthropic-ai/sdk, openai, @google/genai) resolves to the version AND
// integrity the sha's package-lock.json locks at the path it resolved to:
// packages/ai/node_modules/<name> for a copy the lockfile nests for pi-ai,
// node_modules/<name> otherwise. Since ab30693d6 it nests openai 7.19.0 there
// (the root keeps 6.40.0): unpack `npm pack openai@7.19.0` (check its sha512
// against the lockfile) to packages/ai/node_modules/openai and record that
// integrity in packages/ai/node_modules/.package-lock.json under
// "node_modules/openai", as npm records an installed package; pi-messages calls node's own
// fetch.
//
// A raw TCP server answers every request 200, text/event-stream, chunked, and
// writes each of the run's segments as one HTTP chunk, which reaches the
// adapter as one body read (undici reads one chunk at a time); then it holds
// the connection open. The anthropic-messages, openai-completions,
// openai-responses and pi-messages adapters run every mode (the SDK adapters'
// request modes are abort-before-response's); google-generative-ai runs the error-body modes but the retryable
// drop, the no-response ones and "an abort from the callback on the last event
// of a body already complete". Modes:
//   - "an abort while a read is pending": the signal aborts 150ms after the
//     first segment is written, while the adapter waits on its next read;
//   - "an abort from the callback with events left in its read": the
//     onProviderStreamEvent callback aborts on the segment's first event;
//   - "an abort from the callback on its read's last event": it aborts on
//     the segment's last one;
//   - "an abort from the callback with the next read already in": both
//     segments go out in one socket write, and the callback aborts on the
//     first segment's first event;
//   - "an abort from the callback on the last event of a body already
//     complete": both segments and the terminating chunk go out in one
//     socket write — the body has ended before the adapter reads it, and no
//     terminal event is in it — and the callback aborts on the last event;
//   - "an abort while an error body is read": the server answers 500,
//     application/json, and writes the start of the error body as one chunk;
//     the signal aborts 150ms after that;
//   - "an abort while a retryable error body is read" (the adapters that
//     retry through retryProviderRequest): the same with a 429 carrying
//     retry-after: 120, past the 60s maxRetryDelayMs default, under
//     maxRetries 1;
//   - "an already-aborted signal" and "an abort before the response arrives"
//     (pi-messages only; the other four adapters' are in
//     abort-before-response): the server never answers, and the signal is
//     aborted before the call or 150ms into it;
//   - "the connection drops mid-body": no abort; the server destroys the
//     socket 100ms after the first segment, inside the chunked body;
//   - "the connection drops while an error body is read": the same after a
//     500's start;
//   - "the connection drops while a retryable error body is read" (the SDK
//     adapters, whose fail-fast error quotes their SDK's message): the same
//     after the start of a 429 carrying retry-after: 120, under maxRetries 1;
//   - "a custom fetch's body fails mid-body": the caller's options.fetch
//     answers 200 with a body that delivers the first segment and then fails
//     with its own error (customBodyError); no server is involved;
//   - "a custom fetch's retryable error body fails mid-body" (the SDK
//     adapters): options.fetch answers 429 with retry-after: 120 and a body
//     that delivers the start of the error body and then fails with
//     customBodyError, under maxRetries 1;
//   - "the request gets no response: ...": the connection is refused (port
//     1), also at a /timeout path (the SDKs test a rejection's text for
//     /timed? ?out/i, and undici's does not hold the URL), the server
//     closes it on the request, the server answers with bytes that are not
//     HTTP, a header value (U+0001) undici's client refuses, or (the adapters
//     whose SDK takes timeoutMs: anthropic-messages and both openai loops)
//     the server holds the request past timeoutMs, 200; and "a custom fetch
//     rejects" (customFetchError), with its own error or with one that says
//     it timed out, and (the SDK adapters) with an AbortError of its own.
//     pi-messages and google never hand timeoutMs to fetch and wait on
//     undici's 300s headers timeout (D13), too long to capture;
//   - a custom fetch and an abort (anthropic, pi-messages): "a custom fetch
//     rejects once the signal aborts" (150ms into the call, with
//     customFetchError); "a custom fetch's body ignores an abort from the
//     callback" (the body delivers the whole stream, one segment per read,
//     whatever the signal; the callback aborts on the first event); "a custom
//     fetch's body fails once the signal aborts" (the first segment, then a
//     read that waits for the abort, 150ms later, and fails with
//     customBodyError);
//   - "a custom fetch's body hangs, ignoring an abort while a read is
//     pending" (the openai adapters only): the body delivers the first
//     segment, and its next read never settles; the signal aborts 150ms
//     later, while the adapter waits on that read. openai 7.19.0 races each
//     body read against the signal, so the stream still ends; the other
//     adapters' pi would wait on that read forever.
// Recorded per run: the status and segments the server wrote, the event types
// onProviderStreamEvent received, the stream's event types, and the final
// stopReason, errorMessage, content and diagnostic types.
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
const require = createRequire(path.join(src, "api/anthropic-messages.ts"));
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

type Mode =
	| "an abort while a read is pending"
	| "an abort from the callback with events left in its read"
	| "an abort from the callback on its read's last event"
	| "an abort from the callback with the next read already in"
	| "an abort from the callback on the last event of a body already complete"
	| "an abort while an error body is read"
	| "an abort while a retryable error body is read"
	| "an already-aborted signal"
	| "an abort before the response arrives"
	| "the connection drops mid-body"
	| "the connection drops while an error body is read"
	| "the connection drops while a retryable error body is read"
	| "a custom fetch's body fails mid-body"
	| "a custom fetch's retryable error body fails mid-body"
	| "the request gets no response: the connection is refused"
	| "the request gets no response: the connection is refused at a /timeout path"
	| "the request gets no response: the server closes the connection"
	| "the request gets no response: the response is not HTTP"
	| "the request gets no response: undici's client refuses a header value"
	| "the request gets no response: timeoutMs passes first"
	| "a custom fetch rejects"
	| "a custom fetch rejects with a timeout"
	| "a custom fetch rejects with an AbortError of its own"
	| "a custom fetch rejects once the signal aborts"
	| "a custom fetch's body ignores an abort from the callback"
	| "a custom fetch's body fails once the signal aborts"
	| "a custom fetch's body hangs, ignoring an abort while a read is pending";
const streamModes: Mode[] = [
	"an abort while a read is pending",
	"an abort from the callback with events left in its read",
	"an abort from the callback on its read's last event",
	"an abort from the callback with the next read already in",
];
const completeBody: Mode = "an abort from the callback on the last event of a body already complete";
const errorBody: Mode = "an abort while an error body is read";
const retryableErrorBody: Mode = "an abort while a retryable error body is read";
const requestModes: Mode[] = ["an already-aborted signal", "an abort before the response arrives"];
const drops: Mode = "the connection drops mid-body";
const errorBodyDrops: Mode = "the connection drops while an error body is read";
const retryableErrorBodyDrops: Mode = "the connection drops while a retryable error body is read";
const customBody: Mode = "a custom fetch's body fails mid-body";
const customRetryableErrorBody: Mode = "a custom fetch's retryable error body fails mid-body";
const customBodyError = "the custom fetch's body failed";
const refused: Mode = "the request gets no response: the connection is refused";
const closes: Mode = "the request gets no response: the server closes the connection";
const notHTTP: Mode = "the request gets no response: the response is not HTTP";
const badHeader: Mode = "the request gets no response: undici's client refuses a header value";
const refusedAtTimedOutPath: Mode = "the request gets no response: the connection is refused at a /timeout path";
const timesOut: Mode = "the request gets no response: timeoutMs passes first";
const timeoutMs = 200;
const customRejects: Mode = "a custom fetch rejects";
const customFetchError = "the custom fetch failed";
const customRejectsTimedOut: Mode = "a custom fetch rejects with a timeout";
const customTimeoutError = "the custom fetch timed out";
const customRejectsAbortError: Mode = "a custom fetch rejects with an AbortError of its own";
const noResponseModes = [refused, refusedAtTimedOutPath, closes, notHTTP, badHeader, customRejects, customRejectsTimedOut];
// The modes only the adapters whose SDK wraps a rejected fetch run.
const sdkNoResponseModes = [timesOut, customRejectsAbortError];
const badHeaderValue = `a${String.fromCharCode(1)}b`;
const customRejectsOnAbort: Mode = "a custom fetch rejects once the signal aborts";
const customBodyIgnoresAbort: Mode = "a custom fetch's body ignores an abort from the callback";
const customBodyFailsOnAbort: Mode = "a custom fetch's body fails once the signal aborts";
const customAbortModes = [customRejectsOnAbort, customBodyIgnoresAbort, customBodyFailsOnAbort];
const customBodyHangs: Mode = "a custom fetch's body hangs, ignoring an abort while a read is pending";

const anthropicEvent = (event: string, data: unknown) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
// A data-only event, as pi-messages and google send them.
const dataEvent = (data: unknown) => `data: ${JSON.stringify(data)}\n\n`;
// Each adapter's segments: the first carries three events, the second one
// more.
const adapters = [
	{
		api: "anthropic-messages",
		provider: "anthropic",
		id: "claude-sonnet-4-5",
		module: await load("api/anthropic-messages.ts"),
		modes: [...streamModes, completeBody, errorBody, drops, errorBodyDrops, customBody, retryableErrorBody, retryableErrorBodyDrops, customRetryableErrorBody, ...noResponseModes, ...sdkNoResponseModes, ...customAbortModes],
		segments: [
			anthropicEvent("message_start", {
				type: "message_start",
				message: { id: "msg_1", model: "claude-sonnet-4-5", usage: { input_tokens: 3, output_tokens: 0 } },
			}) +
				anthropicEvent("content_block_start", { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }) +
				anthropicEvent("content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "in hand" } }),
			anthropicEvent("content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: " and more" } }),
		],
		// The rest of a stream that finishes.
		finish:
			anthropicEvent("content_block_stop", { type: "content_block_stop", index: 0 }) +
			anthropicEvent("message_delta", { type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 2 } }) +
			anthropicEvent("message_stop", { type: "message_stop" }),
	},
	{
		api: "pi-messages",
		provider: "radius",
		id: "auto",
		module: await load("api/pi-messages.ts"),
		modes: [...streamModes, completeBody, errorBody, ...requestModes, drops, errorBodyDrops, customBody, ...noResponseModes, ...customAbortModes],
		segments: [
			dataEvent({ type: "start" }) +
				dataEvent({ type: "text_start", contentIndex: 0 }) +
				dataEvent({ type: "text_delta", contentIndex: 0, delta: "in hand" }),
			dataEvent({ type: "text_delta", contentIndex: 0, delta: " and more" }),
		],
		finish:
			dataEvent({ type: "text_end", contentIndex: 0, content: "in hand and more" }) +
			dataEvent({
				type: "done",
				reason: "stop",
				usage: { input: 1, output: 2, cacheRead: 0, cacheWrite: 0, totalTokens: 3, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
			}),
	},
	{
		api: "openai-completions",
		provider: "openai",
		id: "gpt-4o",
		module: await load("api/openai-completions.ts"),
		modes: [...streamModes, completeBody, errorBody, drops, errorBodyDrops, customBody, retryableErrorBody, retryableErrorBodyDrops, customRetryableErrorBody, ...noResponseModes, ...sdkNoResponseModes, ...customAbortModes, customBodyHangs],
		segments: [
			dataEvent({ id: "c1", choices: [{ index: 0, delta: { content: "in" } }] }) +
				dataEvent({ id: "c1", choices: [{ index: 0, delta: { content: " " } }] }) +
				dataEvent({ id: "c1", choices: [{ index: 0, delta: { content: "hand" } }] }),
			dataEvent({ id: "c1", choices: [{ index: 0, delta: { content: " and more" } }] }),
		],
		finish: dataEvent({ id: "c1", choices: [{ index: 0, delta: {}, finish_reason: "stop" }] }) + "data: [DONE]\n\n",
	},
	{
		api: "openai-responses",
		provider: "openai",
		id: "gpt-5-mini",
		module: await load("api/openai-responses.ts"),
		modes: [...streamModes, completeBody, errorBody, drops, errorBodyDrops, customBody, retryableErrorBody, retryableErrorBodyDrops, customRetryableErrorBody, ...noResponseModes, ...sdkNoResponseModes, ...customAbortModes, customBodyHangs],
		segments: [
			dataEvent({ type: "response.output_item.added", output_index: 0, item: { type: "message", id: "msg_1", role: "assistant", content: [] } }) +
				dataEvent({ type: "response.content_part.added", output_index: 0, part: { type: "output_text", text: "" } }) +
				dataEvent({ type: "response.output_text.delta", output_index: 0, delta: "in hand" }),
			dataEvent({ type: "response.output_text.delta", output_index: 0, delta: " and more" }),
		],
		finish:
			dataEvent({
				type: "response.output_item.done",
				output_index: 0,
				item: { type: "message", id: "msg_1", role: "assistant", content: [{ type: "output_text", text: "in hand and more" }] },
			}) +
			dataEvent({ type: "response.completed", response: { id: "resp_1", status: "completed", usage: { input_tokens: 1, output_tokens: 2, total_tokens: 3 } } }),
	},
	{
		api: "google-generative-ai",
		provider: "google",
		id: "gemini-2.5-flash",
		module: await load("api/google-generative-ai.ts"),
		modes: [completeBody, errorBody, errorBodyDrops, retryableErrorBody, ...noResponseModes],
		segments: [
			dataEvent({ candidates: [{ content: { parts: [{ text: "in hand" }], role: "model" } }] }),
			dataEvent({
				candidates: [{ content: { parts: [{ text: " and more" }], role: "model" }, finishReason: "STOP" }],
				usageMetadata: { promptTokenCount: 1, candidatesTokenCount: 2, totalTokenCount: 3 },
			}),
		],
	},
];
const firstSegmentEvents = 3;
const errorBodyStart = '{"error":{"message":"cut short';

let status = "";
let extraHead = "";
let segments: string[] = [];
let answer = true;
let dropAfterWrite = false;
let endBody = false;
let onSegmentWritten = () => {};
const chunk = (s: string) => `${Buffer.byteLength(s).toString(16)}\r\n${s}\r\n`;
const server = net.createServer((sock) => {
	let buf = Buffer.alloc(0);
	sock.on("data", (d) => {
		buf = Buffer.concat([buf, d]);
		const headEnd = buf.indexOf("\r\n\r\n");
		if (headEnd < 0) return;
		const len = Number(/content-length:\s*(\d+)/i.exec(buf.subarray(0, headEnd).toString())?.[1] ?? 0);
		if (buf.length < headEnd + 4 + len) return;
		buf = buf.subarray(headEnd + 4 + len);
		if (!answer) return;
		const contentType = status === "200 OK" ? "text/event-stream" : "application/json";
		sock.write(
			`HTTP/1.1 ${status}\r\nContent-Type: ${contentType}\r\n${extraHead}Transfer-Encoding: chunked\r\n\r\n${segments.map(chunk).join("")}${endBody ? "0\r\n\r\n" : ""}`,
		);
		onSegmentWritten();
		if (dropAfterWrite) setTimeout(() => sock.destroy(), 100);
	});
	sock.on("error", () => {});
});
await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const port = (server.address() as net.AddressInfo).port;
// The no-response servers: one closes the connection on the request, one
// answers bytes that are not HTTP.
const closer = net.createServer((sock) => {
	sock.on("data", () => sock.destroy());
	sock.on("error", () => {});
});
await new Promise<void>((resolve) => closer.listen(0, "127.0.0.1", resolve));
const notHTTPServer = net.createServer((sock) => {
	sock.on("data", () => sock.end("NOT HTTP\r\n\r\n"));
	sock.on("error", () => {});
});
await new Promise<void>((resolve) => notHTTPServer.listen(0, "127.0.0.1", resolve));
const baseUrlFor = (mode: Mode) => {
	if (mode === refused) return "http://127.0.0.1:1";
	if (mode === refusedAtTimedOutPath) return "http://127.0.0.1:1/timeout";
	if (mode === closes || mode === badHeader) return `http://127.0.0.1:${(closer.address() as net.AddressInfo).port}`;
	if (mode === notHTTP) return `http://127.0.0.1:${(notHTTPServer.address() as net.AddressInfo).port}`;
	return `http://127.0.0.1:${port}`;
};

const runs = [];
for (const a of adapters) {
	for (const mode of a.modes) {
		const retryable = mode === retryableErrorBody || mode === retryableErrorBodyDrops || mode === customRetryableErrorBody;
		const errorStatus = retryable || mode === errorBody || mode === errorBodyDrops;
		status = retryable ? "429 Too Many Requests" : errorStatus ? "500 Internal Server Error" : "200 OK";
		extraHead = retryable ? "retry-after: 120\r\n" : "";
		segments = errorStatus
			? [errorBodyStart]
			: mode === "an abort from the callback with the next read already in" || mode === completeBody
				? a.segments
				: a.segments.slice(0, 1);
		endBody = mode === completeBody;
		answer =
			!requestModes.includes(mode) &&
			mode !== customBody &&
			mode !== customRetryableErrorBody &&
			!noResponseModes.includes(mode) &&
			!sdkNoResponseModes.includes(mode) &&
			!customAbortModes.includes(mode) &&
			mode !== customBodyHangs;
		dropAfterWrite = mode === drops || mode === errorBodyDrops || mode === retryableErrorBodyDrops;
		const controller = new AbortController();
		onSegmentWritten =
			mode === "an abort while a read is pending" || mode === errorBody || mode === retryableErrorBody
				? () => setTimeout(() => controller.abort(), 150)
				: () => {};
		if (mode === "an already-aborted signal") controller.abort();
		if (mode === "an abort before the response arrives") setTimeout(() => controller.abort(), 150);
		const model = {
			id: a.id,
			name: a.id,
			api: a.api,
			provider: a.provider,
			baseUrl: baseUrlFor(mode),
			reasoning: false,
			input: ["text"],
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow: 100000,
			maxTokens: 1000,
		};
		const observed: string[] = [];
		// A custom fetch answers 200 with a body that delivers the first
		// segment and then fails with the fetch's own error.
		const customFetch = async () => {
			let sent = false;
			const body = new ReadableStream({
				pull(c) {
					if (sent) c.error(new Error(customBodyError));
					else c.enqueue(new TextEncoder().encode(a.segments[0]));
					sent = true;
				},
			});
			return new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
		};
		const eventStream = { status: 200, headers: { "content-type": "text/event-stream" } };
		// A custom fetch answers 429 with retry-after: 120 and a body that
		// delivers the start of the error and then fails with its own error.
		const customRetryableErrorFetch = async () => {
			let sent = false;
			const body = new ReadableStream({
				pull(c) {
					if (sent) c.error(new Error(customBodyError));
					else c.enqueue(new TextEncoder().encode(errorBodyStart));
					sent = true;
				},
			});
			return new Response(body, { status: 429, headers: { "content-type": "application/json", "retry-after": "120" } });
		};
		// The whole stream as the ignoring body delivers it, one read each.
		const whole = [...a.segments, (a as { finish?: string }).finish ?? ""];
		// The custom fetches that meet an abort: one that rejects once the
		// signal aborts, one whose body ignores the signal, one whose body's
		// next read fails once it aborts.
		const customAbortFetches: Partial<Record<Mode, () => Promise<Response>>> = {
			[customRejectsOnAbort]: () =>
				new Promise((_, reject) => controller.signal.addEventListener("abort", () => reject(new Error(customFetchError)))),
			[customBodyIgnoresAbort]: async () => {
				let next = 0;
				const body = new ReadableStream({
					pull(c) {
						if (next < whole.length) c.enqueue(new TextEncoder().encode(whole[next++]));
						else c.close();
					},
				});
				return new Response(body, eventStream);
			},
			[customBodyHangs]: async () => {
				let sent = false;
				const body = new ReadableStream({
					pull(c) {
						if (sent) return new Promise<void>(() => {});
						sent = true;
						c.enqueue(new TextEncoder().encode(a.segments[0]));
						setTimeout(() => controller.abort(), 150);
					},
				});
				return new Response(body, eventStream);
			},
			[customBodyFailsOnAbort]: async () => {
				let sent = false;
				const body = new ReadableStream({
					pull(c) {
						if (sent) {
							return new Promise<void>((resolve) =>
								controller.signal.addEventListener("abort", () => {
									c.error(new Error(customBodyError));
									resolve();
								}),
							);
						}
						sent = true;
						c.enqueue(new TextEncoder().encode(a.segments[0]));
						setTimeout(() => controller.abort(), 150);
					},
				});
				return new Response(body, eventStream);
			},
		};
		if (mode === customRejectsOnAbort) setTimeout(() => controller.abort(), 150);
		const out = a.module.stream(model, { messages: [{ role: "user", content: "hi", timestamp: 1 }] }, {
			apiKey: "test-api-key",
			signal: controller.signal,
			...(retryable ? { maxRetries: 1 } : {}),
			...(mode === customBody ? { fetch: customFetch } : {}),
			...(mode === customRetryableErrorBody ? { fetch: customRetryableErrorFetch } : {}),
			...(customAbortFetches[mode] ? { fetch: customAbortFetches[mode] } : {}),
			...(mode === customRejects || mode === customRejectsTimedOut
				? {
						fetch: async () => {
							throw new Error(mode === customRejects ? customFetchError : customTimeoutError);
						},
					}
				: {}),
			...(mode === timesOut ? { timeoutMs } : {}),
			...(mode === customRejectsAbortError
				? {
						fetch: async () => {
							throw new DOMException("This operation was aborted", "AbortError");
						},
					}
				: {}),
			...(mode === badHeader ? { headers: { "x-test": badHeaderValue } } : {}),
			onProviderStreamEvent: async (event: { type?: string }) => {
				observed.push(event.type ?? "");
				const first =
					mode === "an abort from the callback with events left in its read" ||
					mode === "an abort from the callback with the next read already in" ||
					mode === customBodyIgnoresAbort;
				if (first && observed.length === 1) controller.abort();
				if (mode === "an abort from the callback on its read's last event" && observed.length === firstSegmentEvents) controller.abort();
				if (mode === completeBody && observed.length === a.segments.join("").split("data: ").length - 1) controller.abort();
			},
		});
		const events: string[] = [];
		for await (const ev of out) events.push(ev.type);
		const msg = await out.result();
		runs.push({
			api: a.api,
			mode,
			...(answer ? { status: Number.parseInt(status), segments } : {}),
			...(retryable ? { retryAfter: "120", maxRetries: 1 } : {}),
			...(mode === customBody ? { status: 200, segments: a.segments.slice(0, 1), customBodyError } : {}),
			...(mode === customRetryableErrorBody ? { status: 429, segments: [errorBodyStart], customBodyError } : {}),
			...(mode === badHeader ? { headers: { "x-test": badHeaderValue } } : {}),
			...(mode === customRejects || mode === customRejectsOnAbort ? { customFetchError } : {}),
			...(mode === customRejectsTimedOut ? { customFetchError: customTimeoutError } : {}),
			...(mode === timesOut ? { timeoutMs } : {}),
			...(mode === customBodyIgnoresAbort ? { status: 200, segments: whole } : {}),
			...(mode === customBodyFailsOnAbort ? { status: 200, segments: a.segments.slice(0, 1), customBodyError } : {}),
			...(mode === customBodyHangs ? { status: 200, segments: a.segments.slice(0, 1) } : {}),
			observed,
			events,
			stopReason: msg.stopReason,
			errorMessage: msg.errorMessage,
			content: JSON.stringify(msg.content),
			diagnostics: (msg.diagnostics ?? []).map((d: { type: string }) => d.type),
		});
	}
}
server.close();
closer.close();
notHTTPServer.close();

fs.writeFileSync(
	outFile,
	`${JSON.stringify({ source: `upstream ${sha} packages/ai/src/api, ${sdks.join(", ")}, node ${process.version} (undici ${process.versions.undici})`, runs }, null, "\t")}\n`,
);
console.log(`wrote ${runs.length} runs to ${outFile}`);
process.exit(0);
