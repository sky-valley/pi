// Captures when real pi's anthropic-messages adapter calls onResponse and what
// a throw from it does — the oracle behind TestAnthropicOnResponseMatchesPi.
//
//   node --experimental-strip-types capture-anthropic-on-response.mts <extraction> <pi npm dir> <out.json> <sha>
//   e.g. ... capture-anthropic-on-response.mts <dir> <npm 1.6.0 dir> anthropic-on-response-8a57d42a5.json 8a57d42a5
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai |
// tar -x -C <dir>` from the upstream clone), with packages/ai/node_modules
// resolving pi-ai's dependencies. The npm build's node_modules serves once its
// copies match what <sha>'s package-lock.json locks; at 8a57d42a5 that is
// @anthropic-ai/sdk 0.129.0, the npm 1.6.0 build's.
// No fake client here: the adapter builds its real SDK client, whose fetch
// reads a loopback server that writes each row's exact bytes, because it is
// the SDK that decides how a non-2xx response reaches onResponse (it throws,
// and since 8a57d42a5 pi's retryProviderRequest reports the thrown error's
// status and headers). The model is the npm build's
// anthropic/claude-haiku-4-5.
//
// Each row records the raw response, every onResponse call (status and
// headers), the type of every pushed event and the final stopReason and
// errorMessage. A row may make onResponse throw ("response veto"), after
// aborting the request when abortFirst is set.
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, npmDir, outFile, sha] = process.argv.slice(2);
if (!extraction || !npmDir || !outFile || !sha) {
	console.error(
		"usage: node --experimental-strip-types capture-anthropic-on-response.mts <extraction> <pi npm dir> <out.json> <sha>",
	);
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const { stream: streamAnthropic } = await import(pathToFileURL(path.join(src, "api/anthropic-messages.ts")).href);
const { normalizeContext } = await import(pathToFileURL(path.join(src, "utils/transcript.ts")).href);
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const { MODELS } = await import(pathToFileURL(path.join(pkg, "dist/models.generated.js")).href);
const catalogModel = MODELS.anthropic["claude-haiku-4-5"];
if (!catalogModel) throw new Error("catalog has no anthropic/claude-haiku-4-5");

const ev = (event: string, data: object) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
const sse =
	ev("message_start", { type: "message_start", message: { id: "msg_test", usage: { input_tokens: 12, output_tokens: 0 } } }) +
	ev("content_block_start", { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }) +
	ev("content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "Hello" } }) +
	ev("content_block_stop", { type: "content_block_stop", index: 0 }) +
	ev("message_delta", { type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 5 } }) +
	ev("message_stop", { type: "message_stop" });

// No Connection header: Go's transport deletes a "Connection: close" from the
// header map before any adapter sees it, a transport difference that is not
// onResponse's.
const response = (status: string, headerLines: string[], body: string) =>
	[`HTTP/1.1 ${status}`, ...headerLines, `Content-Length: ${Buffer.byteLength(body)}`, "", body].join("\r\n");
const ok = response("200 OK", ["Content-Type: text/event-stream", "Request-Id: req_1", "X-Multi: a", "X-Multi: b"], sse);
const errorBody = JSON.stringify({ type: "error", error: { type: "invalid_request_error", message: "bad" } });

type Case = { name: string; response: string; throws?: boolean; abortFirst?: boolean };
const cases: Case[] = [
	// A 2xx response reaches onResponse, with pi's headersToRecord record.
	{ name: "okResponseIsReported", response: ok },
	// The SDK throws on a non-2xx status, and since 8a57d42a5 pi's
	// retryProviderRequest reports the thrown error's status and headers.
	{
		name: "clientErrorIsReported",
		response: response("400 Bad Request", ["Content-Type: application/json", "Request-Id: req_2"], errorBody),
	},
	{
		name: "serverErrorIsReported",
		response: response("500 Internal Server Error", ["Content-Type: application/json"], errorBody),
	},
	// pi awaits onResponse before pushing start: a throw fails the stream with
	// its message and nothing else is pushed...
	{ name: "throwFailsTheStream", response: ok, throws: true },
	// ...as an aborted stream when the request was aborted first.
	{ name: "throwAfterAbortIsAborted", response: ok, throws: true, abortFirst: true },
];

async function run(c: Case) {
	const server = net.createServer((socket) => {
		socket.once("data", () => socket.end(c.response));
	});
	await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
	try {
		const { port } = server.address() as net.AddressInfo;
		const model = { ...catalogModel, baseUrl: `http://127.0.0.1:${port}` };
		const controller = new AbortController();
		const calls: Array<{ status: number; headers: Record<string, string> }> = [];
		const s = streamAnthropic(model, normalizeContext({ messages: [{ role: "user", content: "Hello", timestamp: 1 }] }), {
			apiKey: "sk-ant-api03-test",
			maxRetries: 0,
			signal: controller.signal,
			onResponse: async (response: { status: number; headers: Record<string, string> }) => {
				calls.push({ status: response.status, headers: response.headers });
				if (c.throws) {
					if (c.abortFirst) controller.abort();
					throw new Error("response veto");
				}
			},
		});
		const pushed: string[] = [];
		for await (const event of s) pushed.push(event.type);
		const message = await s.result();
		return { calls, pushed, stopReason: message.stopReason, errorMessage: message.errorMessage ?? null };
	} finally {
		server.close();
	}
}

const rows = [];
for (const c of cases) {
	rows.push({
		name: c.name,
		response: c.response,
		...(c.throws ? { throws: true } : {}),
		...(c.abortFirst ? { abortFirst: true } : {}),
		...(await run(c)),
	});
}
// One row per line, so a re-capture diffs row by row.
const text = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(outFile, `{"sha":${JSON.stringify(sha)},"model":"anthropic/claude-haiku-4-5","rows":[\n${text}\n]}\n`);
console.log(`captured ${rows.length} rows from packages/ai/src at ${sha} -> ${outFile}`);
