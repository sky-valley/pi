// Captures pi-ai's prompt cache warming policy (cache-warming.ts, upstream
// c9552c7d8): the oracle behind TestCacheWarmingMatchesPi in package ai.
//
//   node capture.mjs <src extraction> <out.json>
//   e.g. node capture.mjs .../pisrc/ea1cfcc4c cache-warming-ea1cfcc4c.json
//
// <src extraction> holds packages/ai (src/**). The module is new in src and not
// in any published build yet, so this is a src capture; re-verify it against
// the first pi-ai build that ships c9552c7d8 (pass that package dir: its
// dist/cache-warming.js is loaded instead).
//
// Every row records its own model, so the Go test needs no catalog. The models
// cover base-rate pricing with and without cache writes, a pricing tier on both
// sides of its threshold, unknown prices, and the promptCache tiers.
// JSON.stringify prints the shortest text that round-trips a double, so the Go
// test compares the costs bit for bit.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [dir, outFile] = process.argv.slice(2);
if (!dir || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir | src extraction> <out.json>");
	process.exit(2);
}
const dist = fs.existsSync(path.join(dir, "dist/cache-warming.js"));
const load = (file) =>
	import(pathToFileURL(dist ? path.join(dir, "dist", `${file}.js`) : path.join(dir, "packages/ai/src", `${file}.ts`)).href);
const cw = await load("cache-warming");
const source = dist
	? `pi-ai ${JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version}`
	: `src ${path.basename(dir)}`;

const base = {
	id: "m",
	name: "M",
	api: "anthropic-messages",
	provider: "anthropic",
	baseUrl: "https://example.invalid",
	reasoning: true,
	input: ["text"],
	contextWindow: 200000,
	maxTokens: 64000,
};
const models = {
	sonnet: { ...base, id: "sonnet", cost: { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 }, promptCache: { short: 300, long: 3600 } },
	opus: { ...base, id: "opus", cost: { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 }, promptCache: { short: 300 } },
	adaptive: {
		...base,
		id: "adaptive",
		cost: { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 },
		promptCache: { short: 300, long: 3600 },
		compat: { forceAdaptiveThinking: true },
	},
	adaptiveOff: { ...base, id: "adaptive-off", cost: { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 }, compat: { forceAdaptiveThinking: false } },
	noWrite: {
		...base,
		id: "no-write",
		api: "openai-responses",
		provider: "openai",
		cost: { input: 1.25, output: 10, cacheRead: 0.125, cacheWrite: 0 },
		promptCache: { short: 600.5 },
	},
	tiered: {
		...base,
		id: "tiered",
		api: "openai-responses",
		provider: "openai",
		cost: {
			input: 2.5,
			output: 15,
			cacheRead: 0.25,
			cacheWrite: 0,
			tiers: [{ inputTokensAbove: 272000, input: 5, output: 22.5, cacheRead: 0.5, cacheWrite: 0 }],
		},
		promptCache: { long: 86400 },
	},
	free: { ...base, id: "free", cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
	google: {
		...base,
		id: "google",
		api: "google-generative-ai",
		provider: "google",
		cost: { input: 0.3, output: 2.5, cacheRead: 0.075, cacheWrite: 0 },
		promptCache: { short: 10 },
	},
};

const decisions = [];
for (const [name, model] of Object.entries(models)) {
	for (const promptTokens of [0, 1, 1024, 8000, 31337, 120000, 272000, 272001, 950000]) {
		for (const phase of ["streaming", "idle"]) {
			decisions.push({ model: name, promptTokens, phase, decision: cw.evaluateCacheWarming(model, promptTokens, phase) });
		}
	}
}

const delays = [];
for (const ttlMs of [0, 1, 9999, 10000, 10001, 10500, 50000, 99999, 100000, 100001, 300000, 600500, 3600000, 86400000, 12345.6]) {
	const delayMs = cw.getCacheWarmingDelayMs(ttlMs);
	delays.push({ ttlMs, delayMs: delayMs ?? null });
}

const ttls = [];
for (const [name, model] of Object.entries(models)) {
	for (const [label, options, env] of [
		["none", undefined, undefined],
		["empty", {}, undefined],
		["short", { cacheRetention: "short" }, undefined],
		["long", { cacheRetention: "long" }, undefined],
		["off", { cacheRetention: "none" }, undefined],
		["env long", { env: { PI_CACHE_RETENTION: "long" } }, { PI_CACHE_RETENTION: "long" }],
		["env other", { env: { PI_CACHE_RETENTION: "short" } }, { PI_CACHE_RETENTION: "short" }],
		["explicit short over env long", { cacheRetention: "short", env: { PI_CACHE_RETENTION: "long" } }, { PI_CACHE_RETENTION: "long" }],
	]) {
		const ttlMs = cw.getPromptCacheTtlMs(model, options);
		ttls.push({ model: name, label, cacheRetention: options?.cacheRetention ?? "", env: env ?? null, ttlMs: ttlMs ?? null });
	}
}

const replayable = [];
for (const [name, model] of Object.entries(models)) {
	for (const reasoning of ["", "minimal", "high", "xhigh"]) {
		const options = reasoning ? { reasoning } : {};
		replayable.push({ model: name, reasoning, replayable: cw.isCacheWarmingReplayable(model, options) });
	}
	replayable.push({ model: name, reasoning: null, replayable: cw.isCacheWarmingReplayable(model, undefined) });
}

const controller = new AbortController();
const requestOptions = cw.getCacheWarmingRequestOptions(
	{ maxTokens: 4096, maxRetries: 5, disableContinuation: false, cacheRetention: "long", sessionId: "s", reasoning: "high" },
	controller.signal,
);
const { signal, ...requestOptionsWithoutSignal } = requestOptions;

fs.writeFileSync(
	outFile,
	`${JSON.stringify(
		{
			source,
			constants: {
				maxAgeMs: cw.CACHE_WARMING_MAX_AGE_MS,
				maxIdleAgeMs: cw.CACHE_WARMING_MAX_IDLE_AGE_MS,
				minimumExpectedSavings: cw.CACHE_WARMING_MINIMUM_EXPECTED_SAVINGS,
				idleContinuationProbability: cw.CACHE_WARMING_IDLE_CONTINUATION_PROBABILITY,
			},
			models,
			decisions,
			delays,
			ttls,
			replayable,
			requestOptions: { ...requestOptionsWithoutSignal, signalIsCallers: signal === controller.signal },
		},
		null,
		"\t",
	)}\n`,
);
console.log(`wrote ${outFile}: ${decisions.length} decisions, ${delays.length} delays, ${ttls.length} ttls, ${replayable.length} replay checks`);
