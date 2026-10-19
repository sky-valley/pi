// Captures how pi sends error tool results that carry images to Anthropic
// (upstream 34a8bb38b, then 5ab7fe74c): Anthropic rejects an error tool_result
// holding anything but text, so each run of images in one is replaced by
// "(image attached after the tool results)" and the images follow ALL tool
// results of the turn, in the same user message, under a caption naming the
// tool call. It is the oracle behind TestAnthropicErrorToolResultImagesMatchPi.
//
//   node capture.mjs <pi-ai package dir | src extraction> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai error-images-1.3.0.json
//
// The first two rows are upstream's anthropic-error-tool-result-images.test.ts;
// the rest are edge cases of this port's. Each row records the model, the
// messages and options streamSimple got, and the params onPayload saw.
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
const anthropic = await load("api/anthropic-messages");
const { normalizeContext } = await load("utils/transcript");
const source = dist
	? `pi-ai ${JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version}`
	: `src ${path.basename(dir)}`;

const model = {
	id: "claude-test",
	name: "Claude Test",
	api: "anthropic-messages",
	provider: "anthropic",
	baseUrl: "http://127.0.0.1:9",
	reasoning: false,
	input: ["text", "image"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 200000,
	maxTokens: 1024,
};
const usage = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 0,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};
const assistant = (calls, origin = { api: "anthropic-messages", provider: "anthropic", model: "claude-test" }) => ({
	role: "assistant",
	content: calls.map(([id, name]) => ({ type: "toolCall", id, name, arguments: {} })),
	...origin,
	usage,
	stopReason: "toolUse",
	timestamp: 2,
});
const user = (text) => ({ role: "user", content: text, timestamp: 1 });
const text = (t) => ({ type: "text", text: t });
const image = (data) => ({ type: "image", data, mimeType: "image/png" });
const result = (toolCallId, toolName, content, isError) => ({ role: "toolResult", toolCallId, toolName, content, isError, timestamp: 3 });

const rows = [];
async function row(name, messages, { m = model, cacheRetention = "none" } = {}) {
	let payload;
	const s = anthropic.streamSimple(m, normalizeContext({ messages }), {
		apiKey: "fake-key",
		cacheRetention,
		onPayload: (params) => {
			payload = JSON.parse(JSON.stringify(params));
			throw new Error("payload captured");
		},
	});
	const final = await s.result();
	if (!payload) throw new Error(`${name}: no payload: ${final.errorMessage}`);
	rows.push({ name, model: m, messages, cacheRetention, payload });
}

await row("upstream/moves the images after the tool results", [
	user("Show the screenshots"),
	assistant([["call-1", "codemode"], ["call-2", "read"]]),
	result("call-1", "codemode", [text("Script failed"), image("AAAA"), image("CCCC"), text("Script error")], true),
	result("call-2", "read", [image("BBBB")], false),
]);
await row("upstream/keeps error results without images", [
	user("Run it"),
	assistant([["call-1", "codemode"]]),
	result("call-1", "codemode", [text("Script failed")], true),
]);
// An images-only error result keeps one placeholder as its whole text.
await row("images-only error result", [
	user("Run it"),
	assistant([["call-1", "codemode"]]),
	result("call-1", "codemode", [image("AAAA"), image("BBBB")], true),
]);
// Two error results with images around a successful one: every caption and
// its images follow all three results, in tool-result order; runs of images
// apart collapse to one placeholder each.
await row("two error results", [
	user("Run them"),
	assistant([["call-1", "a"], ["call-2", "b"], ["call-3", "c"]]),
	result("call-1", "a", [image("A1"), text("between"), image("A2")], true),
	result("call-2", "b", [text("fine"), image("B1")], false),
	result("call-3", "c", [text("failed"), image("C1")], true),
]);
// A text that already reads as the placeholder absorbs the image after it.
await row("placeholder text before an image", [
	user("Run it"),
	assistant([["call-1", "codemode"]]),
	result("call-1", "codemode", [text("(image attached after the tool results)"), image("AAAA"), text("done")], true),
]);
// A model without image input downgrades the images first, so nothing moves.
await row("model without image input", [
	user("Run it"),
	assistant([["call-1", "codemode"]]),
	result("call-1", "codemode", [text("Script failed"), image("AAAA")], true),
], { m: { ...model, input: ["text"] } });
// With caching on, the last block of the message — the last moved image — takes
// the conversation's cache breakpoint.
await row("cache breakpoint on the last image", [
	user("Run it"),
	assistant([["call-1", "codemode"]]),
	result("call-1", "codemode", [text("Script failed"), image("AAAA")], true),
], { cacheRetention: "short" });
// A call from another model gets a normalized id; the caption names the id the
// tool_result carries.
await row("normalized tool call id", [
	user("Run it"),
	assistant([["call|abc/1", "codemode"]], { api: "openai-responses", provider: "openai", model: "gpt-5" }),
	result("call|abc/1", "codemode", [text("Script failed"), image("AAAA")], true),
]);

fs.writeFileSync(outFile, `${JSON.stringify({ source, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
