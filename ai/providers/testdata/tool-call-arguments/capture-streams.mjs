// Captures the finished tool calls pi's anthropic-messages, openai-completions
// and openai-responses adapters build when the arguments text a call finishes
// with is malformed, not an object, empty or valid (upstream 87f9a3641:
// parseToolCallArguments at each adapter's final-arguments site) — the oracle
// behind TestFinishedToolCallArgumentsLikePi.
//
//   node capture-streams.mjs <pi-ai package dir> <out.json>
//   e.g. node capture-streams.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai \
//        tool-call-streams-1.3.0.json
//
// The package is the published npm build; compat.js's stream() dispatches each
// model by its api. A loopback server answers every request with the row's SSE
// body. Each row records the body, the final message's stopReason and
// JSON.stringify of its content, so the key order of a call carrying
// argumentsError — what a session file persists — is recorded too.
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture-streams.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const pi = await import(pathToFileURL(path.join(pkg, "dist/compat.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

let body = "";
const server = http.createServer((req, res) => {
	req.resume();
	req.on("end", () => {
		res.writeHead(200, { "content-type": "text/event-stream" });
		res.end(body);
	});
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const baseUrl = `http://127.0.0.1:${server.address().port}`;

const models = {
	"anthropic-messages": { id: "claude-x", name: "x", api: "anthropic-messages", provider: "anthropic" },
	"openai-completions": { id: "gpt-x", name: "x", api: "openai-completions", provider: "openai" },
	"openai-responses": { id: "gpt-5-mini", name: "x", api: "openai-responses", provider: "openai" },
};

const sse = (events) => events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join("");
// Each body streams `pieces` as the call's argument deltas and finishes it with
// `final` where the API repeats the arguments on the finished item.
const bodies = {
	"anthropic-messages": (pieces) =>
		[
			{ type: "message_start", message: { id: "msg_1", usage: { input_tokens: 1, output_tokens: 0 } } },
			{ type: "content_block_start", index: 0, content_block: { type: "tool_use", id: "toolu_1", name: "write", input: {} } },
			...pieces.map((p) => ({ type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: p } })),
			{ type: "content_block_stop", index: 0 },
			{ type: "message_delta", delta: { stop_reason: "tool_use" }, usage: { output_tokens: 5 } },
			{ type: "message_stop" },
		]
			.map((e) => `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`)
			.join(""),
	"openai-completions": (pieces) =>
		sse([
			{ id: "c1", choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: "call_1", type: "function", function: { name: "write", arguments: "" } }] } }] },
			...pieces.map((p) => ({ id: "c1", choices: [{ index: 0, delta: { tool_calls: [{ index: 0, function: { arguments: p } }] } }] })),
			{ id: "c1", choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }] },
		]) + "data: [DONE]\n\n",
	"openai-responses": (pieces, final) =>
		sse([
			{ type: "response.created", response: { id: "resp_1" } },
			{ type: "response.output_item.added", output_index: 0, item: { type: "function_call", id: "fc_1", call_id: "call_1", name: "write", arguments: "" } },
			...pieces.map((p) => ({ type: "response.function_call_arguments.delta", output_index: 0, item_id: "fc_1", delta: p })),
			{ type: "response.output_item.done", output_index: 0, item: { type: "function_call", id: "fc_1", call_id: "call_1", name: "write", arguments: final ?? pieces.join("") } },
			{ type: "response.completed", response: { id: "resp_1", status: "completed", usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } },
		]),
};

const argumentTexts = [
	// upstream's openai-responses-partial-json-cleanup.test.ts
	['{"path":"README.md",', '"content":"updated",', '"mode":.5}'],
	// cut off: the prefix parses, the whole does not
	['{"path":"a.txt","content":"par', "tial"],
	// valid JSON, not an object
	["[1,", "2]"],
	["null"],
	// repaired: an invalid escape and a raw control character
	['{"note":"\\url","cmd":"a', "\tb", '"}'],
	// valid
	['{"b":1,"2":"x",', '"1":"y"}'],
	// no arguments at all
	[],
	// blank
	[" \n"],
];

const rows = [];
for (const [api, model] of Object.entries(models)) {
	for (const pieces of argumentTexts) {
		body = bodies[api](pieces);
		const m = { ...model, baseUrl, reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 1000, maxTokens: 100 };
		const result = await pi.stream(m, { messages: [{ role: "user", content: "hi", timestamp: 0 }] }, { apiKey: "k" }).result();
		rows.push({ api, body, stopReason: result.stopReason, errorMessage: result.errorMessage ?? null, content: JSON.stringify(result.content) });
	}
}
// responses: the finished item's arguments win over the streamed deltas, and
// an empty one falls back to them
for (const [pieces, final] of [
	[['{"a":1}'], '{"a":'],
	[['{"a":'], ""],
]) {
	body = bodies["openai-responses"](pieces, final);
	const m = { ...models["openai-responses"], baseUrl, reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 1000, maxTokens: 100 };
	const result = await pi.stream(m, { messages: [{ role: "user", content: "hi", timestamp: 0 }] }, { apiKey: "k" }).result();
	rows.push({ api: "openai-responses", body, stopReason: result.stopReason, errorMessage: result.errorMessage ?? null, content: JSON.stringify(result.content) });
}
server.close();
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, node: process.version, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
