// Captures how pi times a Models stream whose setup fails AFTER auth work took
// a while: lazyStream reads the call's start, then creates the stream, then
// runs setup (provider lookup, applyAuth with its credential resolution and
// transformHeaders); a setup failure ends the stream with an error message
// stamped at the call's start, which the stream times from its creation — so
// durationMs covers the setup. It is the oracle behind
// TestModelsSetupErrorTimingMatchesPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai setup-error-timing-1.3.0.json
//
// Each row runs Models.stream, streamSimple or streamDeferred against a
// provider whose api key resolution waits waitMs and finds none, or whose key
// resolves at once while the request's transformHeaders waits waitMs and
// throws. Recorded: the events, the error message, the message
// timestamp's offset from Date.now() read just before the call, and durationMs.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const { createModels, createProvider } = await import(pathToFileURL(path.join(pkg, "dist/models.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const waitMs = 80;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const chatModel = (provider) => ({
	id: "m",
	name: "M",
	api: "test-api",
	provider,
	baseUrl: "http://127.0.0.1:9",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 1000,
	maxTokens: 100,
});
const never = () => {
	throw new Error("the provider was reached");
};
// "slow" takes waitMs to find no key; "keyed" resolves one at once.
const provider = (id, resolve) =>
	createProvider({
		id,
		name: id,
		auth: { apiKey: { resolve } },
		models: [chatModel(id)],
		api: { stream: never, streamSimple: never, fetchDeferred: never },
	});
const models = createModels();
models.setProvider(
	provider("slow", async () => {
		await sleep(waitMs);
		return undefined;
	}),
);
models.setProvider(provider("keyed", async () => ({ auth: { apiKey: "k" } })));

const context = { messages: [{ role: "user", content: "hi", timestamp: 1 }] };
const handle = { id: "h" };
const entries = {
	stream: (model, options) => models.stream(model, context, options),
	streamSimple: (model, options) => models.streamSimple(model, context, options),
	streamDeferred: (model, options) => models.streamDeferred(model, handle, options),
};
const setups = {
	"slow auth finds no key": [chatModel("slow"), {}],
	"transformHeaders fails after a wait": [
		chatModel("keyed"),
		{
			transformHeaders: async () => {
				await sleep(waitMs);
				throw new Error("headers refused");
			},
		},
	],
};

const rows = [];
for (const [entry, run] of Object.entries(entries)) {
	for (const [setup, [model, options]] of Object.entries(setups)) {
		const callStart = Date.now();
		const s = run(model, options);
		const events = [];
		for await (const event of s) events.push(event.type);
		const message = await s.result();
		rows.push({
			name: `${entry}/${setup}`,
			entry,
			setup,
			waitMs,
			events,
			stopReason: message.stopReason,
			errorMessage: message.errorMessage,
			timestampOffset: message.timestamp - callStart,
			durationMs: message.durationMs ?? null,
		});
	}
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
