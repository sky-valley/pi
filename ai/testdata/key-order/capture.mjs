// Captures the key order of the assistant messages and content blocks pi
// produces — what JSON.stringify writes to a session file or the wire — for
// each adapter the port has, and as the agent loop hands them on (it appends
// thinkingLevel to the stream's result with Object.assign, agent-loop.ts). It
// is the oracle behind TestAssistantMessageKeyOrderMatchesPi.
//
//   node capture.mjs <npm dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0 key-order-1.3.0.json
//
// <npm dir> holds node_modules with @earendil-works/pi-ai and pi-agent-core.
// Each row streams one response body (served through options.fetch) through
// the adapter's streamSimple — and, for `agent` rows, through pi-agent-core's
// agentLoop with that streamSimple — and records the final message's keys and
// each content block's keys, plus the message itself.
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const agentPkg = path.join(npmDir, "node_modules/@earendil-works/pi-agent-core");
const load = (file) => import(pathToFileURL(path.join(pkg, "dist", `${file}.js`)).href);
const { agentLoop } = await import(pathToFileURL(path.join(agentPkg, "dist/agent-loop.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const sse = (events, named = false) =>
	events.map((e) => (named ? `event: ${e.type}\n` : "") + `data: ${JSON.stringify(e)}\n\n`).join("");
const cost = { input: 1, output: 2, cacheRead: 0.1, cacheWrite: 0.2 };
const base = { reasoning: true, input: ["text"], cost, contextWindow: 100000, maxTokens: 1000, baseUrl: "http://127.0.0.1:9" };

const adapters = [
	{
		api: "anthropic-messages",
		model: {
			...base,
			id: "claude-fable-5-1",
			name: "Claude Fable 5.1",
			api: "anthropic-messages",
			provider: "anthropic",
			compat: { forceAdaptiveThinking: true, supportsMidConvoEffort: true },
		},
		body: sse(
			[
				{ type: "message_start", message: { id: "msg_1", model: "claude-fable-5-1-relabeled", usage: { input_tokens: 5, output_tokens: 0, cache_creation_input_tokens: 4, cache_creation: { ephemeral_1h_input_tokens: 4 } }, input_transformations: [{ type: "thinking_dropped", path: "messages[0]", reason: "r" }] } },
				{ type: "content_block_start", index: 0, content_block: { type: "thinking", thinking: "", signature: "" } },
				{ type: "content_block_delta", index: 0, delta: { type: "thinking_delta", thinking: "hmm" } },
				{ type: "content_block_delta", index: 0, delta: { type: "signature_delta", signature: "sig" } },
				{ type: "content_block_stop", index: 0 },
				{ type: "content_block_start", index: 1, content_block: { type: "text", text: "" } },
				{ type: "content_block_delta", index: 1, delta: { type: "text_delta", text: "hi" } },
				{ type: "content_block_stop", index: 1 },
				{ type: "content_block_start", index: 2, content_block: { type: "tool_use", id: "toolu_1", name: "read", input: {} } },
				{ type: "content_block_delta", index: 2, delta: { type: "input_json_delta", partial_json: '{"path":"a"}' } },
				{ type: "content_block_stop", index: 2 },
				{ type: "message_delta", delta: { stop_reason: "tool_use" }, usage: { output_tokens: 3 } },
				{ type: "message_stop" },
			],
			true,
		),
		options: { reasoning: "low" },
	},
	{
		api: "openai-completions",
		model: { ...base, id: "gpt-4o", name: "GPT-4o", api: "openai-completions", provider: "openai" },
		body:
			sse([
				{ id: "chatcmpl_1", model: "gpt-4o-2024", choices: [{ index: 0, delta: { role: "assistant", reasoning_content: "hmm" } }] },
				{ id: "chatcmpl_1", model: "gpt-4o-2024", choices: [{ index: 0, delta: { content: "hi" } }] },
				{ id: "chatcmpl_1", model: "gpt-4o-2024", choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: "call_1", type: "function", function: { name: "read", arguments: '{"path":"a"}' } }] } }] },
				{ id: "chatcmpl_1", model: "gpt-4o-2024", choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }], usage: { prompt_tokens: 5, completion_tokens: 3, total_tokens: 8 } },
			]) + "data: [DONE]\n\n",
		options: { reasoning: "low" },
	},
	{
		api: "openai-responses",
		model: { ...base, id: "gpt-6-luna", name: "GPT-6 Luna", api: "openai-responses", provider: "openai", compat: { supportsMidConvoEffort: true } },
		body: sse([
			{ type: "response.created", response: { id: "resp_1" } },
			{ type: "response.output_item.added", output_index: 0, item: { type: "reasoning", id: "rs_1", summary: [] } },
			{ type: "response.reasoning_summary_text.delta", output_index: 0, summary_index: 0, delta: "hmm" },
			{ type: "response.output_item.done", output_index: 0, item: { type: "reasoning", id: "rs_1", summary: [{ type: "summary_text", text: "hmm" }] } },
			{ type: "response.output_item.added", output_index: 1, item: { type: "message", id: "msg_1", role: "assistant", content: [] } },
			{ type: "response.output_text.delta", output_index: 1, content_index: 0, delta: "hi" },
			{ type: "response.output_item.done", output_index: 1, item: { type: "message", id: "msg_1", role: "assistant", status: "completed", content: [{ type: "output_text", text: "hi", annotations: [] }] } },
			{ type: "response.output_item.added", output_index: 2, item: { type: "function_call", id: "fc_1", call_id: "call_1", name: "read", arguments: "" } },
			{ type: "response.function_call_arguments.delta", output_index: 2, delta: '{"path":"a"}' },
			{ type: "response.output_item.done", output_index: 2, item: { type: "function_call", id: "fc_1", call_id: "call_1", name: "read", arguments: '{"path":"a"}', status: "completed" } },
			{ type: "response.completed", response: { id: "resp_1", status: "completed", usage: { input_tokens: 5, output_tokens: 3, total_tokens: 8 } } },
		]),
		options: { reasoning: "low" },
	},
	{
		api: "google-generative-ai",
		model: { ...base, id: "gemini-2.5-pro", name: "Gemini 2.5 Pro", api: "google-generative-ai", provider: "google" },
		body: sse([
			{ responseId: "r_1", modelVersion: "gemini-2.5-pro-002", candidates: [{ content: { role: "model", parts: [{ text: "hmm", thought: true, thoughtSignature: "c2ln" }] } }] },
			{ responseId: "r_1", candidates: [{ content: { role: "model", parts: [{ text: "hi" }] } }] },
			{ responseId: "r_1", candidates: [{ content: { role: "model", parts: [{ functionCall: { name: "read", args: { path: "a" } } }] }, finishReason: "STOP" }], usageMetadata: { promptTokenCount: 5, candidatesTokenCount: 3, totalTokenCount: 8 } },
		]),
		options: { reasoning: "low" },
	},
	{
		api: "pi-messages",
		model: { ...base, id: "auto", name: "Auto", api: "pi-messages", provider: "radius", baseUrl: "http://127.0.0.1:9/v1" },
		body: sse([
			{ type: "start", responseId: "r_1" },
			{ type: "thinking_start", contentIndex: 0 },
			{ type: "thinking_delta", contentIndex: 0, delta: "hmm" },
			{ type: "thinking_end", contentIndex: 0, content: "hmm", thinkingSignature: "sig" },
			{ type: "text_start", contentIndex: 1 },
			{ type: "text_delta", contentIndex: 1, delta: "hi" },
			{ type: "text_end", contentIndex: 1, content: "hi" },
			{ type: "toolcall_start", contentIndex: 2, id: "call_1", toolName: "read" },
			{ type: "toolcall_delta", contentIndex: 2, delta: '{"path":"a"}' },
			{ type: "toolcall_end", contentIndex: 2, toolCall: { type: "toolCall", id: "call_1", name: "read", arguments: { path: "a" } } },
			{ type: "done", reason: "toolUse", usage: { input: 5, output: 3, cacheRead: 0, cacheWrite: 0, totalTokens: 8, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, responseId: "r_1" },
		]),
		options: { reasoning: "low" },
	},
];

const errorBody = (api) =>
	api === "google-generative-ai"
		? JSON.stringify({ error: { code: 400, message: "bad request", status: "INVALID_ARGUMENT" } })
		: JSON.stringify({ type: "error", error: { type: "invalid_request_error", message: "bad request" } });

// The keys JSON.stringify writes, in its order: a key holding undefined is
// left out.
const keysOf = (live) => {
	const message = JSON.parse(JSON.stringify(live));
	return {
		keys: Object.keys(message),
		contentKeys: (message.content ?? []).map((block) => Object.keys(block)),
		usageKeys: message.usage ? Object.keys(message.usage) : null,
	};
};

// google refuses a custom fetch, so its bodies come from a loopback server
// that answers every request with the body the current row set.
let serverResponse;
const server = http.createServer((req, res) => {
	req.resume();
	req.on("end", () => {
		res.writeHead(serverResponse.status, { "content-type": serverResponse.contentType });
		res.end(serverResponse.body);
	});
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const serverBase = `http://127.0.0.1:${server.address().port}`;

const rows = [];
const user = { role: "user", content: "hi", timestamp: 1 };
const tools = [{ name: "read", description: "read", parameters: { type: "object", properties: { path: { type: "string" } } } }];
for (const { api, model, body, options } of adapters) {
	const mod = await load(`api/${api}`);
	for (const scenario of ["success", "error"]) {
		const response = () =>
			scenario === "success"
				? new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } })
				: new Response(errorBody(api), { status: 400, headers: { "content-type": "application/json" } });
		const fetch = api === "google-generative-ai" ? undefined : async () => response();
		if (api === "google-generative-ai") {
			model.baseUrl = serverBase;
			serverResponse =
				scenario === "success"
					? { status: 200, contentType: "text/event-stream", body }
					: { status: 400, contentType: "application/json", body: errorBody(api) };
		}
		const streamOptions = { apiKey: "test-key", cacheRetention: "none", maxRetries: 0, ...(fetch ? { fetch } : {}), ...options };
		const direct = await mod.streamSimple(model, { messages: [{ role: "system", content: "", toolsAdded: tools, timestamp: 0 }, user] }, streamOptions).result();
		rows.push({ name: `${api}/${scenario}`, api, scenario, layer: "stream", ...keysOf(direct), message: direct });

		// The agent loop, as pi's coding agent persists the message it emits.
		const streamFn = (m, context, opts) =>
			mod.streamSimple(m, context, { ...opts, ...(fetch ? { fetch } : {}), cacheRetention: "none", maxRetries: 0 });
		const context = { systemPrompt: "", messages: [], tools: [] };
		const config = { model, reasoning: options.reasoning, apiKey: "test-key", convertToLlm: (messages) => messages };
		// The loop would go on to run the tool call and ask again; stop it once
		// the first reply has ended.
		const controller = new AbortController();
		let ended;
		for await (const event of agentLoop([user], context, config, controller.signal, streamFn)) {
			if (event.type === "message_end" && event.message.role === "assistant") {
				ended = event.message;
				controller.abort();
				break;
			}
		}
		rows.push({ name: `${api}/${scenario}/agent`, api, scenario, layer: "agent", ...keysOf(ended), message: ended });
	}
}
server.close();
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
