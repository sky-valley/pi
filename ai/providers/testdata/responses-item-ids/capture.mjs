// Captures the item ids pi's openai-responses adapter replays on a tool call's
// function_call / custom_tool_call input item (upstream bc2d8dc1c: the id's
// prefix must match the replayed item type — fc_ for function_call, ctc_ for
// custom_tool_call — and every id is dropped for a reply from another model)
// — the oracle behind TestResponsesReplayedItemIDsMatchPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai item-ids-1.3.0.json
//
// The package is the published npm build; compat.js's streamSimple builds the
// request and onPayload records its `input` and stops it. Rows cross the
// replayed call's id (fc_, ctc_, another prefix, no item id) with grammar tool
// support on or off (custom_tool_call or function_call), and the reply's
// origin: the same model, another model of the provider, and a foreign
// provider's model (upstream's constrained-sampling.test.ts case, whose ids
// transformMessages normalizes first).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const pi = await import(pathToFileURL(path.join(pkg, "dist/compat.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const tool = {
	name: "gram",
	description: "gram tool",
	parameters: { type: "object", properties: { query: { type: "string" } }, required: ["query"] },
	constrainedSampling: { type: "grammar", variants: { openai_lark: "start: /.+/" } },
};
const origins = {
	sameModel: { api: "openai-responses", provider: "openai", model: "gpt-5" },
	otherModel: { api: "openai-responses", provider: "openai", model: "gpt-other" },
	foreignProvider: { api: "pi-messages", provider: "radius", model: "gpt-other" },
};
const ids = ["call_1|fc_1", "call_1|ctc_1", "call_1|msg_1", "call_1"];

const rows = [];
for (const grammar of [true, false]) {
	const model = {
		id: "gpt-5",
		name: "x",
		api: "openai-responses",
		provider: "openai",
		baseUrl: "https://api.openai.com/v1",
		reasoning: true,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 1000,
		maxTokens: 100,
		...(grammar ? { compat: { supportsOpenAIGrammarTools: true } } : {}),
	};
	for (const [originName, origin] of Object.entries(origins)) {
		for (const id of ids) {
			const context = {
				messages: [
					{ role: "user", content: "hi", timestamp: 0 },
					{ role: "assistant", ...origin, content: [{ type: "toolCall", id, name: "gram", arguments: { query: "SELECT 1" } }], usage, stopReason: "toolUse", timestamp: 0 },
					{ role: "toolResult", toolCallId: id, toolName: "gram", content: [{ type: "text", text: "ok" }], isError: false, timestamp: 0 },
				],
				tools: [tool],
			};
			let payload;
			await pi
				.streamSimple(model, context, {
					apiKey: "k",
					onPayload: (p) => {
						payload = p;
						throw new Error("captured");
					},
				})
				.result();
			if (!payload) throw new Error(`no payload for ${originName} ${id}`);
			rows.push({ grammar, origin: originName, id, input: JSON.stringify(payload.input) });
		}
	}
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, origins, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
