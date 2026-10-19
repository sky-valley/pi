// Captures what pi makes of thinking blocks from another model (upstream
// 911309429): transformMessages drops them — readable, signed, redacted or
// empty — instead of converting readable thinking to text, and keeps a model's
// own thinking as before. It is the oracle behind
// TestTransformDropsCrossModelThinkingLikePi and
// TestCrossModelThinkingRequestsMatchPi.
//
//   node capture.mjs <pi-ai package dir | src extraction> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai cross-model-thinking-1.3.0.json
//
// `transformRows` record transformMessages(messages, model) with no id
// normalizer (upstream's transform-messages-cross-model-thinking.test.ts and
// edge cases); synthetic tool results' timestamps are written as 0.
// `requestRows` stream one cross-model history through streamSimple on every
// API whose adapter runs transformMessages and the port has (anthropic,
// openai-completions, openai-responses, google) and record the params
// onPayload saw.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [dir, outFile] = process.argv.slice(2);
if (!dir || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir | src extraction> <out.json>");
	process.exit(2);
}
const dist = fs.existsSync(path.join(dir, "dist/api/anthropic-messages.js"));
const load = (file) =>
	import(pathToFileURL(dist ? path.join(dir, "dist", `${file}.js`) : path.join(dir, "packages/ai/src", `${file}.ts`)).href);
const { transformMessages } = await load("api/transform-messages");
const { normalizeContext } = await load("utils/transcript");
const source = dist
	? `pi-ai ${JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version}`
	: `src ${path.basename(dir)}`;

const base = {
	reasoning: true,
	input: ["text", "image"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 1048576,
	maxTokens: 32768,
};
const kimi = { ...base, id: "moonshotai/Kimi-K3", name: "Kimi K3", api: "openai-completions", provider: "sference", baseUrl: "https://api.sference.com/v1" };
const usage = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 0,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};
const assistant = (model, content, origin = { api: "openai-completions", provider: "sference" }, stopReason = "toolUse") => ({
	role: "assistant",
	content,
	...origin,
	model,
	usage,
	stopReason,
	timestamp: 2,
});
const user = (content, timestamp = 1) => ({ role: "user", content, timestamp });
const thinking = (text, extra = { thinkingSignature: "reasoning_content" }) => ({ type: "thinking", thinking: text, ...extra });

const transformRows = [];
function transformRow(name, model, messages) {
	const result = transformMessages(messages, model).map((m) =>
		m.role === "toolResult" && m.content?.[0]?.text === "No result provided" ? { ...m, timestamp: 0 } : m,
	);
	transformRows.push({ name, model, messages, result: JSON.parse(JSON.stringify(result)) });
}

transformRow("upstream/readable thinking from another model", kimi, [
	user("Why does curl work but node fails?"),
	assistant("zai-org/GLM-5.3", [
		thinking("The user asks about ECONNRESET. Candidates: proxy, IPv6, TLS."),
		{ type: "text", text: "Let me check the TLS versions." },
		{ type: "toolCall", id: "call_1", name: "bash", arguments: { command: "curl -v" } },
	]),
	{ role: "toolResult", toolCallId: "call_1", toolName: "bash", content: [{ type: "text", text: "TLSv1.2" }], isError: false, timestamp: 3 },
]);
transformRow(
	"upstream/alternating models",
	kimi,
	[
		user("1"),
		assistant("moonshotai/Kimi-K3", [thinking("kimi 1"), { type: "text", text: "a" }], undefined, "stop"),
		user("2"),
		assistant("zai-org/GLM-5.3", [thinking("glm 1"), { type: "text", text: "b" }], undefined, "stop"),
		user("3"),
		assistant("moonshotai/Kimi-K3", [thinking("kimi 2"), { type: "text", text: "c" }], undefined, "stop"),
		user("4"),
		assistant("zai-org/GLM-5.3", [thinking("glm 2"), { type: "text", text: "d" }], undefined, "stop"),
		user("hi"),
	],
);
// Every kind of thinking block, from the model itself and from another one:
// unsigned readable, signed, redacted, empty, empty but signed. Another model's
// are all dropped; the model's own keep redacted and signed blocks even when
// empty and drop only an empty unsigned one.
const kinds = [
	thinking("readable", {}),
	thinking("signed", { thinkingSignature: "sig" }),
	thinking("", { thinkingSignature: "opaque", redacted: true }),
	thinking("   ", {}),
	thinking("", { thinkingSignature: "encrypted" }),
	{ type: "text", text: "answer" },
];
transformRow("every kind from another model", kimi, [user("q"), assistant("zai-org/GLM-5.3", kinds, undefined, "stop"), user("next")]);
transformRow("every kind from the same model", kimi, [user("q"), assistant("moonshotai/Kimi-K3", kinds, undefined, "stop"), user("next")]);
// The same model id on another provider or api is another model.
transformRow("same id, another provider", kimi, [
	user("q"),
	assistant("moonshotai/Kimi-K3", [thinking("other provider"), { type: "text", text: "a" }], { api: "openai-completions", provider: "together" }, "stop"),
	user("next"),
]);
transformRow("same id, another api", kimi, [
	user("q"),
	assistant("moonshotai/Kimi-K3", [thinking("other api"), { type: "text", text: "a" }], { api: "openai-responses", provider: "sference" }, "stop"),
	user("next"),
]);

// One history whose assistant turn came from Claude, replayed to a model of
// each API.
const foreign = [
	user("Look it up"),
	assistant(
		"claude-opus-5",
		[
			thinking("I should call the tool.", { thinkingSignature: "claude-signature" }),
			thinking("", { thinkingSignature: "redacted-payload", redacted: true }),
			{ type: "text", text: "Calling it." },
			{ type: "toolCall", id: "toolu_01", name: "lookup", arguments: { q: "x" } },
		],
		{ api: "anthropic-messages", provider: "anthropic" },
	),
	{ role: "toolResult", toolCallId: "toolu_01", toolName: "lookup", content: [{ type: "text", text: "found" }], isError: false, timestamp: 3 },
	user("And now?", 4),
];
const targets = [
	{ ...base, id: "claude-sonnet-4-5", name: "Claude Sonnet 4.5", api: "anthropic-messages", provider: "anthropic", baseUrl: "http://127.0.0.1:9", reasoning: false },
	{ ...kimi },
	{ ...base, id: "gpt-5", name: "GPT-5", api: "openai-responses", provider: "openai", baseUrl: "http://127.0.0.1:9" },
	{ ...base, id: "gemini-2.5-pro", name: "Gemini 2.5 Pro", api: "google-generative-ai", provider: "google", baseUrl: "http://127.0.0.1:9" },
];
const modules = {
	"anthropic-messages": await load("api/anthropic-messages"),
	"openai-completions": await load("api/openai-completions"),
	"openai-responses": await load("api/openai-responses"),
	"google-generative-ai": await load("api/google-generative-ai"),
};
const requestRows = [];
for (const model of targets) {
	let payload;
	const s = modules[model.api].streamSimple(model, normalizeContext({ messages: foreign }), {
		apiKey: "test-key",
		cacheRetention: "none",
		onPayload: (params) => {
			payload = JSON.parse(JSON.stringify(params));
			throw new Error("payload captured");
		},
	});
	const final = await s.result();
	if (!payload) throw new Error(`${model.api}: no payload: ${final.errorMessage}`);
	requestRows.push({ name: model.api, model, messages: foreign, payload });
}

fs.writeFileSync(outFile, `${JSON.stringify({ source, transformRows, requestRows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${transformRows.length} transform rows, ${requestRows.length} request rows`);
