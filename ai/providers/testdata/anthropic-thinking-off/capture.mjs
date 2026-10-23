// Captures pi's Anthropic thinking params on managed-effort models
// (compat.supportsMidConvoEffort) once "off" is supported there (upstream
// dc939913f): a reasoning model asked for thinkingEnabled:false whose
// thinkingLevelMap does not mark off unsupported (off !== null) gets
// {type: "disabled"} with no effort markers, no output_config and no
// block_binding, and the reply records no providerThinkingLevel — which is now
// read from the params built, not set when the reply starts. It is the oracle
// behind TestAnthropicThinkingOffMatchesPi.
//
//   node capture.mjs <pi-ai package dir | src extraction> <out.json>
//   e.g. node capture.mjs .../npm-1.4.0/node_modules/@earendil-works/pi-ai thinking-off-1.4.0.json
//
// A src extraction (`git archive <sha> packages/ai` plus a node_modules) lacks
// the generated catalog data, which upstream does not commit: copy a build's
// dist/providers/data/*.json and .manifest.json into
// packages/ai/src/providers/data first. thinking-off-e3bc05e87.json is src at
// e3bc05e87 with the 1.4.0 build's data.
//
// `stream` rows replay upstream's anthropic-mid-conversation-effort.test.ts:
// its managedModel and assistants, the adapter's stream with thinkingEnabled
// true unless the row says otherwise, cacheRetention "none". `catalog` rows
// stream streamSimple over the build's own catalog models at each reasoning
// setting. Each row records the params onPayload saw (it throws to stop the
// request) and the failed reply's providerThinkingLevel (null when absent).
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
const { getModel, normalizeContext } = await load("compat");
const source = dist
	? `pi-ai ${JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version}`
	: `src ${path.basename(dir)}`;

const managedModel = (patch = {}) => ({
	id: "claude-fable-5-1",
	name: "Claude Fable 5.1",
	api: "anthropic-messages",
	provider: "anthropic",
	baseUrl: "http://127.0.0.1:9",
	reasoning: true,
	thinkingLevelMap: { off: null, minimal: "low", low: "low", medium: "medium", high: "high", max: "max" },
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 200000,
	maxTokens: 32000,
	compat: { forceAdaptiveThinking: true, supportsMidConvoEffort: true },
	...patch,
});
const usage = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 0,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};
const assistant = (model, level, patch = {}) => ({
	role: "assistant",
	content: [
		{ type: "thinking", thinking: "reasoning", thinkingSignature: "signature" },
		{ type: "text", text: "answer" },
	],
	api: "anthropic-messages",
	provider: model.provider,
	model: model.id,
	...(level === undefined ? {} : { providerThinkingLevel: level }),
	usage,
	stopReason: "stop",
	timestamp: 1,
	...patch,
});
const user = (text, timestamp) => ({ role: "user", content: text, timestamp });

async function payloadOf(run) {
	let payload;
	const onPayload = (value) => {
		payload = JSON.parse(JSON.stringify(value));
		throw new Error("payload captured");
	};
	const message = await run(onPayload).result();
	if (!payload) throw new Error(`no payload: ${message.errorMessage}`);
	return { payload, providerThinkingLevel: message.providerThinkingLevel ?? null };
}

const streamRows = [];
async function streamRow(name, model, messages, options = {}) {
	const got = await payloadOf((onPayload) =>
		anthropic.stream(model, normalizeContext({ messages }), {
			apiKey: "test-key",
			cacheRetention: "none",
			thinkingEnabled: true,
			...options,
			onPayload,
		}),
	);
	// thinkingEnabled is written true, false, or null for a key set to
	// undefined (pi's unset), which JSON.stringify would drop.
	const recorded = {
		effort: options.effort ?? null,
		thinkingEnabled: "thinkingEnabled" in options ? (options.thinkingEnabled ?? null) : true,
	};
	streamRows.push({ name, model, messages, options: recorded, ...got });
}

{
	const model = managedModel();
	await streamRow("historical marker prefix/first", model, [user("one", 1)], { effort: "low" });
	await streamRow("historical marker prefix/second", model, [user("one", 1), assistant(model, "low"), user("two", 2)], { effort: "high" });
	for (const effort of ["low", "medium", "high", "xhigh", "max"]) {
		await streamRow(`native effort ${effort}`, model, [user("one", 1)], { effort });
	}
	await streamRow("omitted effort", model, [user("one", 1)]);
	await streamRow(
		"no markers for legacy or other-provider assistants",
		model,
		[user("one", 1), assistant(model), user("two", 2), { ...assistant(model, "low"), provider: "other-provider" }, user("three", 3)],
		{ effort: "medium" },
	);
	const offModel = managedModel({ thinkingLevelMap: { minimal: null, low: "low", medium: "medium", high: "high", max: "max" } });
	await streamRow(
		"off supported",
		offModel,
		[user("one", 1), assistant(offModel, "low"), user("two", 2), assistant(offModel, "medium"), user("three", 3)],
		{ thinkingEnabled: false },
	);
	await streamRow("off unsupported", model, [user("one", 1)], { thinkingEnabled: false });
	// thinkingEnabled left unset is not "off": managed effort as before.
	await streamRow("thinking unset", offModel, [user("one", 1)], { thinkingEnabled: undefined });
	// A model that does not reason never sends disabled thinking.
	await streamRow("off on a model that does not reason", managedModel({ reasoning: false, thinkingLevelMap: undefined }), [user("one", 1)], {
		thinkingEnabled: false,
	});
	await streamRow("unsupported model keeps top-level effort", managedModel({ compat: { forceAdaptiveThinking: true } }), [user("one", 1)], {
		effort: "low",
	});
}

const catalogRows = [];
for (const id of ["claude-opus-5", "claude-haiku-5-5", "claude-opus-5-5", "claude-fable-5-1"]) {
	const model = getModel("anthropic", id);
	for (const reasoning of [undefined, "low", "high", "xhigh"]) {
		const got = await payloadOf((onPayload) =>
			anthropic.streamSimple(model, normalizeContext({ messages: [user("one", 1)] }), {
				apiKey: "test-key",
				cacheRetention: "none",
				...(reasoning ? { reasoning } : {}),
				onPayload,
			}),
		);
		catalogRows.push({ name: `${id}/${reasoning ?? "unset"}`, provider: "anthropic", id, reasoning: reasoning ?? null, ...got });
	}
}

fs.writeFileSync(outFile, `${JSON.stringify({ source, streamRows, catalogRows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${streamRows.length} stream rows, ${catalogRows.length} catalog rows`);
