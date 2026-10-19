// Captures the blocks pi's openai-completions adapter builds when one stream
// delta carries several of reasoning, reasoning_details, content and tool_calls
// (upstream 63035734f: a delta is handled in that order — reasoning fields,
// then reasoning_details, then content, then tool_calls) — the oracle behind
// TestOpenAICompletionsDeltaOrderMatchesPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai delta-order-1.3.0.json
//
// The package is the published npm build. Each row's SSE body is served from a
// loopback server to compat.js's stream(); the row records the body, the type
// and contentIndex of every event, and JSON.stringify of the final content.
// The rows are upstream's: openai-completions-reasoning-details.test.ts's two
// one-delta cases and openai-completions-tool-choice.test.ts's mixed-deltas
// stream, and the port's: each pair of fields in one delta.
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
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
const model = {
	id: "gpt-x",
	name: "x",
	api: "openai-completions",
	provider: "openai",
	baseUrl: `http://127.0.0.1:${server.address().port}`,
	reasoning: true,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 100000,
	maxTokens: 100,
};

const sse = (chunks) => chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n";
const chunk = (delta, finish_reason = null) => ({ id: "c", choices: [{ index: 0, delta, finish_reason }] });
const detail = { type: "reasoning.encrypted", id: "call_1", data: "encrypted-signature" };
const call = (index, id, name, args) => ({ index, id, type: "function", function: { name, arguments: args } });

const rows = {
	// openai-completions-reasoning-details.test.ts @63035734f
	"upstream/reasoning_content and content": [chunk({ role: "assistant", reasoning_content: "thinking", content: "answer" }, "stop")],
	"upstream/reasoning_details and content": [chunk({ reasoning_details: [detail], content: "answer" }, "stop")],
	// openai-completions-tool-choice.test.ts's mixed-deltas stream
	"upstream/mixed content, reasoning and parallel tool calls": [
		chunk({
			content: "answer 1",
			reasoning_content: "think 1",
			tool_calls: [
				call(0, "tc_read_initial", "read", '{"path":"README'),
				call(1, "tc_grep_initial", "grep", '{"pattern":"TODO'),
				{ id: "tc_list_no_index", type: "function", function: { name: "list", arguments: '{"path":"packages' } },
				{ id: "tc_write_no_index", type: "function", function: { name: "write", arguments: '{"path":"out' } },
			],
		}),
		chunk({
			content: " answer 2",
			tool_calls: [
				{ index: 1, id: "tc_grep_changed", type: "function", function: { arguments: '","path":"src' } },
				{ id: "tc_write_no_index", type: "function", function: { arguments: '.txt","content":"ok"}' } },
				{ id: "tc_list_no_index", type: "function", function: { arguments: '/ai"}' } },
			],
		}),
		chunk(
			{
				content: "\n",
				reasoning_content: " think 2",
				tool_calls: [
					{ index: 0, id: "tc_read_changed", type: "function", function: { arguments: '.md"}' } },
					{ index: 1, type: "function", function: { arguments: '"}' } },
				],
			},
			"tool_calls",
		),
	],
	// each field with every later one, in one delta
	"reasoning and tool_calls": [chunk({ tool_calls: [call(0, "call_1", "f", "{}")], reasoning: "r" }, "tool_calls")],
	"reasoning_details and tool_calls": [chunk({ tool_calls: [call(0, "call_1", "f", "{}")], reasoning_details: [detail] }, "tool_calls")],
	"content and tool_calls": [chunk({ tool_calls: [call(0, "call_1", "f", "{}")], content: "hi" }, "tool_calls")],
	"reasoning and reasoning_details": [chunk({ reasoning_details: [detail], reasoning_text: "r" }, "stop")],
	"all four": [chunk({ tool_calls: [call(0, "call_1", "f", "{}")], content: "hi", reasoning_details: [detail], reasoning_content: "r" }, "tool_calls")],
	// a text block opened by an earlier delta keeps its place
	"text first, then reasoning with content": [chunk({ content: "a" }), chunk({ reasoning_content: "r", content: "b" }, "stop")],
};

const out = [];
for (const [name, chunks] of Object.entries(rows)) {
	body = sse(chunks);
	const s = pi.stream(model, { messages: [{ role: "user", content: "hi", timestamp: 0 }] }, { apiKey: "k" });
	const events = [];
	for await (const event of s) events.push([event.type, event.contentIndex ?? null]);
	const result = await s.result();
	out.push({ name, body, events, stopReason: result.stopReason, errorMessage: result.errorMessage ?? null, content: JSON.stringify(result.content) });
}
server.close();
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, rows: out }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${out.length} rows`);
