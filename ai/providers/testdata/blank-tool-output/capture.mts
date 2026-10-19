// Captures the request pi's openai-completions, openai-responses and
// google-generative-ai adapters build for a tool result whose text is blank or
// whitespace (upstream 4dd2b149d: text counts only when textResult.trim() is
// non-empty, so a bare "\r\n" from bash gets the empty-output placeholder) —
// the oracle behind TestBlankToolOutputMatchesPi.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> blank-tool-output-4dd2b149d.json 4dd2b149d
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai`)
// and a node_modules resolving its dependencies: the npm 1.3.0 build's, whose
// @google/genai 2.21.0 and openai 7.19.0 are what 4dd2b149d's lockfile locks.
// The generated catalogs src imports (src/providers/data/*.json) are not in
// git: copy them from the 1.3.0 build's dist/providers/data. 4dd2b149d is
// after the 1.3.0 release, so this is a src capture.
//
// streamSimple from src/compat.ts builds each request. The openai bodies are
// what onPayload sees; the google body is read where @google/genai POSTs it
// (globalThis.fetch), the REST body the Go adapter builds. Each row records the
// tool result's text blocks, whether it carries an image, the model's input
// kinds, and the request's messages / input / contents.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const { streamSimple } = await import(pathToFileURL(path.join(extraction, "packages/ai/src/compat.ts")).href);

const image = { type: "image", data: "aW1n", mimeType: "image/png" };
const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const C = String.fromCharCode;
// Text blocks of the tool result; JS trim strips every one of these whitespace
// characters, but not U+200B or U+0085.
const texts = [
	[""],
	["\r\n"],
	[" \n\t"],
	[C(0xa0)],
	[C(0xfeff, 0x2028, 0x2029, 0x3000, 0x1680, 0x0b, 0x0c)],
	[C(0x200b)],
	[C(0x85)],
	[" ", " "],
	[" ", "x"],
	["x"],
	[" x "],
];
const models = {
	"openai-completions": { id: "gpt-x", api: "openai-completions", provider: "openai", baseUrl: "https://api.openai.com/v1" },
	"openai-responses": { id: "gpt-x", api: "openai-responses", provider: "openai", baseUrl: "https://api.openai.com/v1" },
	"google-generative-ai": { id: "gemini-3-pro-preview", api: "google-generative-ai", provider: "google", baseUrl: "https://generativelanguage.googleapis.com/v1beta" },
};

let googleBody;
globalThis.fetch = (async (_url: unknown, init?: { body?: unknown }) => {
	googleBody ??= String(init?.body);
	throw new Error("body captured");
}) as typeof fetch;

const rows = [];
for (const [api, base] of Object.entries(models)) {
	for (const input of [["text"], ["text", "image"]]) {
		for (const withImage of [false, true]) {
			for (const textBlocks of texts) {
				for (const isError of api === "google-generative-ai" ? [false, true] : [false]) {
					const model = { ...base, name: "x", reasoning: false, input, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 100 };
					const content = [...textBlocks.map((text) => ({ type: "text", text })), ...(withImage ? [image] : [])];
					const messages = [
						{ role: "user", content: "run", timestamp: 0 },
						{ role: "assistant", api, provider: base.provider, model: base.id, content: [{ type: "toolCall", id: "call_1", name: "bash", arguments: { command: "true" } }], usage, stopReason: "toolUse", timestamp: 0 },
						{ role: "toolResult", toolCallId: "call_1", toolName: "bash", content, isError, timestamp: 0 },
					];
					let payload;
					googleBody = undefined;
					const final = await streamSimple(model, { messages }, {
						apiKey: "k",
						maxRetries: 0,
						onPayload: api === "google-generative-ai" ? undefined : (p: unknown) => {
							payload = p;
							throw new Error("captured");
						},
					}).result();
					if (api === "google-generative-ai") payload = googleBody === undefined ? undefined : JSON.parse(googleBody);
					if (!payload) throw new Error(`${api}: ${final.errorMessage}`);
					const request = payload.messages ?? payload.input ?? payload.contents;
					rows.push({ api, input, isError, content, request: JSON.stringify(request) });
				}
			}
		}
	}
}
fs.writeFileSync(outFile, `${JSON.stringify({ sha, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
