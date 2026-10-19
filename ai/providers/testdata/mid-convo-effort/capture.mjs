// Captures pi's openai-responses requests for compat.supportsMidConvoEffort
// (upstream 60ac3bcc3): with an API key and a reasoning effort, reasoning.effort
// stays "medium" and the level goes in configuration_update input items — one
// before each earlier reply of this model that recorded its level, one at the
// end — and the reply records the level as providerThinkingLevel. It is the
// oracle behind TestResponsesMidConvoEffortMatchesPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai mid-convo-effort-1.3.0.json
//
// The package is the published npm build. The runs replay upstream's
// openai-responses-mid-convo-effort.test.ts: its model, its fake response
// bodies (served through options.fetch) and its conversations, each run's reply
// joining the history of the next. Every run records the model, the messages
// and options it was given, the response body, the request body and the reply.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const { stream } = await import(pathToFileURL(path.join(pkg, "dist/api/openai-responses.js")).href);
const { normalizeContext } = await import(pathToFileURL(path.join(pkg, "dist/utils/transcript.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const thinkingLevelMap = { off: "none", minimal: null, low: "low", medium: "medium", high: "high", xhigh: "xhigh", max: "max" };
const openAIModel = (compat = { supportsMidConvoEffort: true }) => ({
	id: "gpt-6-luna",
	name: "GPT-6 Luna",
	api: "openai-responses",
	provider: "openai",
	baseUrl: "https://api.openai.com/v1",
	reasoning: true,
	thinkingLevelMap,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 272000,
	maxTokens: 128000,
	compat,
});
const user = (text) => ({ role: "user", content: text, timestamp: 0 });
const toolResult = (callId) => ({
	role: "toolResult",
	toolCallId: callId,
	toolName: "lookup",
	content: [{ type: "text", text: "result" }],
	isError: false,
	timestamp: 0,
});
function responseBody(responseId, withToolCall) {
	const item = withToolCall
		? { type: "function_call", id: "fc_1", call_id: "call_1", name: "lookup", arguments: "{}", status: "completed" }
		: { type: "message", id: `msg_${responseId}`, role: "assistant", status: "completed", content: [{ type: "output_text", text: "answer", annotations: [] }] };
	return [
		{ type: "response.created", response: { id: responseId } },
		{ type: "response.output_item.added", output_index: 0, item },
		{ type: "response.output_item.done", output_index: 0, item },
		{ type: "response.completed", response: { id: responseId, status: "completed" } },
	]
		.map((event) => `data: ${JSON.stringify(event)}\n\n`)
		.join("");
}
// A stored message carries no timestamp or duration that could move a request.
const stable = (message) => ({ ...message, timestamp: 0, durationMs: undefined });

const runs = [];
async function run(name, messages, reasoningEffort, { apiKey = "sk-test", model = openAIModel(), withToolCall = false } = {}) {
	const body = responseBody("resp_1", withToolCall);
	let payload;
	const fetch = async (_url, init) => {
		payload = JSON.parse(String(init?.body));
		return new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
	};
	const message = stable(await stream(model, normalizeContext({ messages }), { apiKey, reasoningEffort, fetch }).result());
	if (!payload || message.stopReason === "error") throw new Error(`${name}: ${message.errorMessage}`);
	runs.push({ name, model, messages, apiKey, reasoningEffort: reasoningEffort ?? null, body, payload, message });
	return message;
}

{
	const history = [user("one")];
	history.push(await run("level change/first", [...history], "high"), user("two"));
	history.push(await run("level change/second", [...history], "high"), user("three"));
	await run("level change/third", [...history], "low");
}
{
	const history = [user("one")];
	history.push(await run("tool loop/call", [...history], "high", { withToolCall: true }), toolResult("call_1|fc_1"));
	await run("tool loop/after the result", [...history], "low");
}
{
	const unstamped = await run("origins/unstamped", [user("one")], "high", { model: openAIModel({}) });
	const otherProvider = { ...unstamped, provider: "opencode", providerThinkingLevel: "high" };
	const otherModel = { ...unstamped, model: "gpt-6-sol", providerThinkingLevel: "high" };
	const recorded = { ...unstamped, providerThinkingLevel: "high" };
	await run(
		"origins/only this model's recorded replies",
		[user("one"), unstamped, user("two"), otherProvider, user("three"), otherModel, user("four"), recorded, user("five")],
		"low",
	);
}
for (const [name, apiKey, model, level] of [
	["Sign in with ChatGPT", "chatgpt-access-token", openAIModel(), "low"],
	["compat flag off", "sk-test", openAIModel({ supportsMidConvoEffort: false }), "low"],
	["thinking off", "sk-test", openAIModel(), undefined],
]) {
	const first = await run(`reasoning.effort/${name}/first`, [user("one")], "high", { apiKey, model });
	await run(`reasoning.effort/${name}/second`, [user("one"), first, user("two")], level, { apiKey, model });
}
// A level the map sends as another value: the item carries the mapped value.
await run("mapped level", [user("one")], "off", { model: { ...openAIModel(), thinkingLevelMap: { ...thinkingLevelMap, off: "none", high: "HIGH-ish" } } });
await run("mapped level/high", [user("one")], "high", { model: { ...openAIModel(), thinkingLevelMap: { ...thinkingLevelMap, high: "HIGH-ish" } } });

fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, runs }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${runs.length} runs`);
