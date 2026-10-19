// Captures the request bodies pi's openai-completions and openai-responses
// adapters build from sampling parameters (upstream 76dfb88f6: the model's
// samplingParams, overridden by samplingParamsByThinkingLevel for the effective
// thinking level, overridden by the request's) — the oracle behind
// TestSamplingParamsMatchPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai sampling-params-1.3.0.json
//
// The package is the published npm build. Each row runs compat.js's stream()
// (options as given) or streamSimple() (`simple`) with onPayload recording the
// body and stopping the request. The rows are upstream's
// sampling-options.test.ts cases for both APIs, and the thinking-level clamp
// (a level the model lacks, an explicit "off" a model cannot turn off, xhigh
// on a model without it, a model without reasoning) and a null level entry.
// The row records the model and options as JSON, so the Go test builds the
// same request from them.
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

const model = (api, samplingParams, overrides = {}) => ({
	id: "custom-model",
	name: "Custom Model",
	api,
	provider: "custom-provider",
	baseUrl: "http://127.0.0.1:9/v1",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 128000,
	maxTokens: 16384,
	...(samplingParams ? { samplingParams } : {}),
	...overrides,
});

const cases = [];
for (const api of ["openai-completions", "openai-responses"]) {
	const row = (name, simple, m, options) => cases.push({ name: `${api}/${name}`, api, simple, model: m, options });
	// upstream's sampling-options.test.ts
	row("request params", false, model(api), { samplingParams: { top_p: 0.95, top_k: 0, min_p: 0 } });
	row("none", false, model(api), {});
	row("model params under request params", false, model(api, { top_p: 0.95, min_p: 0.05 }), { samplingParams: { top_p: 0.5 } });
	row("simple request params", true, model(api), { samplingParams: { top_p: 0.5 } });
	row(
		"simple clamped level over model params",
		true,
		model(api, { temperature: 1, top_p: 0.95 }, {
			reasoning: true,
			thinkingLevelMap: { low: null, medium: null },
			samplingParamsByThinkingLevel: { high: { temperature: 0.8, top_k: 64 } },
		}),
		{ reasoning: "low" },
	);
	row("simple off level", true, model(api, undefined, { samplingParamsByThinkingLevel: { off: { temperature: 0.7 } } }), {});
	row(
		"simple request params over level params",
		true,
		model(api, undefined, { reasoning: true, samplingParamsByThinkingLevel: { low: { temperature: 0.6, top_p: 0.95 } } }),
		{ reasoning: "low", samplingParams: { top_p: 0.5 } },
	);
	row(
		"level params between model and request params",
		false,
		model(api, { temperature: 1, top_p: 0.95 }, { reasoning: true, samplingParamsByThinkingLevel: { low: { temperature: 0.6, top_k: 64 } } }),
		{ reasoningEffort: "low", samplingParams: { top_p: 0.5 } },
	);
	row("request params over named fields", false, model(api), { temperature: 0, samplingParams: { temperature: 1 } });
	// the clamp, and edges of the level table
	const levels = {
		off: { temperature: 0.1 },
		minimal: { temperature: 0.2 },
		low: { temperature: 0.3 },
		medium: { temperature: 0.4 },
		high: { temperature: 0.5 },
		xhigh: { temperature: 0.6 },
	};
	row("direct off on a model that cannot turn off", false, model(api, undefined, { reasoning: true, thinkingLevelMap: { off: null }, samplingParamsByThinkingLevel: levels }), {});
	row("simple off on a model that cannot turn off", true, model(api, undefined, { reasoning: true, thinkingLevelMap: { off: null }, samplingParamsByThinkingLevel: levels }), { reasoning: "off" });
	row("simple unset on a model that cannot turn off", true, model(api, undefined, { reasoning: true, thinkingLevelMap: { off: null }, samplingParamsByThinkingLevel: levels }), {});
	row("simple xhigh on a model without it", true, model(api, undefined, { reasoning: true, samplingParamsByThinkingLevel: levels }), { reasoning: "xhigh" });
	row("direct xhigh on a model without it", false, model(api, undefined, { reasoning: true, samplingParamsByThinkingLevel: levels }), { reasoningEffort: "xhigh" });
	row("simple high on a model without reasoning", true, model(api, { top_p: 0.9 }, { samplingParamsByThinkingLevel: levels }), { reasoning: "high" });
	row("a null level entry", false, model(api, { top_p: 0.9 }, { reasoning: true, samplingParamsByThinkingLevel: { low: null } }), { reasoningEffort: "low" });
	row("an empty model table", false, model(api, {}, { samplingParamsByThinkingLevel: {} }), {});
	if (api === "openai-responses") {
		row(
			"summary only uses medium",
			false,
			model(api, undefined, { reasoning: true, samplingParamsByThinkingLevel: { off: { temperature: 0.7 }, medium: { temperature: 0.8 } } }),
			{ reasoningSummary: "auto" },
		);
		row(
			"effort and summary",
			false,
			model(api, undefined, { reasoning: true, samplingParamsByThinkingLevel: { medium: { temperature: 0.8 }, high: { temperature: 0.9 } } }),
			{ reasoningEffort: "high", reasoningSummary: "detailed" },
		);
		row(
			"summary only without reasoning",
			false,
			model(api, undefined, { samplingParamsByThinkingLevel: { off: { temperature: 0.7 }, medium: { temperature: 0.8 } } }),
			{ reasoningSummary: "auto" },
		);
	}
}

const rows = [];
for (const c of cases) {
	let payload;
	const run = c.simple ? pi.streamSimple : pi.stream;
	await run(c.model, { messages: [{ role: "user", content: "Hello", timestamp: 0 }] }, {
		...c.options,
		apiKey: "fake-key",
		onPayload: (p) => {
			payload = p;
			throw new Error("captured");
		},
	}).result();
	if (!payload) throw new Error(`no payload for ${c.name}`);
	rows.push({ ...c, payload: JSON.stringify(payload) });
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
