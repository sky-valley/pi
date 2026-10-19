// Captures what real pi makes of OpenRouter's model list — the oracle behind
// TestOpenRouterCostMatchesPi and TestPersonalizeOpenRouterCatalogMatchesPi in
// this package (upstream ee409a85f).
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> openrouter-d2e7d8b7c.json d2e7d8b7c
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai`), a
// node_modules resolving pi-ai's dependencies (the npm build's), and
// packages/ai/src/providers/data copied from that build's dist/providers/data
// (openrouter.ts imports the generated catalog). Captured from src at
// d2e7d8b7c with the 1.3.0 build: v1.3.0 ships both files unchanged.
//
// `cost` rows run openRouterCost (providers/openrouter-api.ts) on pricing
// objects: OpenRouter's $/token decimal strings, the parseFloat edges (junk
// suffixes, whitespace, Infinity, numbers in place of strings, null), negative
// router prices, toFixed(6) rounding (0.0078125 is a binary tie), and the
// override rules (prompt-length tiers kept, time-of-day ones skipped, a
// present-but-null utc key counting as present). Non-finite numbers are written
// as strings ("Infinity"), which JSON.stringify would turn into null. `personalize` rows stub
// globalThis.fetch, run personalizeOpenRouterCatalog on a fixed catalog, and
// record the request it made and what it returned or threw.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { openRouterCost } = await load("providers/openrouter-api.ts");
const { personalizeOpenRouterCatalog } = await load("providers/openrouter.ts");

const pricings: unknown[] = [
	{ prompt: "0.000003", completion: "0.000015" },
	{ prompt: "0.000003", completion: "0.000015", input_cache_read: "0.0000003", input_cache_write: "0.00000375" },
	{},
	{ prompt: "-1", completion: "-1" },
	{ prompt: "0", completion: "0" },
	{ prompt: "-0", completion: "0.0" },
	{ prompt: "abc", completion: "" },
	{ prompt: "1.5e-6abc", completion: "  2e-6", input_cache_read: " 3e-7" },
	{ prompt: "Infinity", completion: "-Infinity" },
	{ prompt: 0.000002, completion: null },
	{ prompt: "0.0000000078125", completion: "0.0000000000005" },
	{ prompt: "0.1", completion: "0.0000001234565" },
	{
		prompt: "0.000001",
		completion: "0.000002",
		overrides: [
			{ min_prompt_tokens: 200000, prompt: "0.000002", completion: "0.000004" },
			{ min_prompt_tokens: 128000, input_cache_read: "-1" },
			{ min_prompt_tokens: 64000, utc_start: 0, utc_end: 8, prompt: "0.0000005" },
			{ utc_days: ["sat"], prompt: "0.0000005" },
			{ prompt: "0.0000009" },
			{ min_prompt_tokens: 1000, utc_start: null, prompt: "0.0000006" },
		],
	},
	{ prompt: "0.000001", overrides: [{ min_prompt_tokens: 64000, utc_days: [] }] },
	{ prompt: "0.000001", overrides: [] },
];

const zeroCost = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
const chat = (id: string) => ({
	id,
	name: id,
	api: "openai-completions",
	provider: "openrouter",
	baseUrl: "https://openrouter.ai/api/v1",
	reasoning: false,
	input: ["text"],
	cost: { input: 1, output: 2, cacheRead: 0.1, cacheWrite: 0 },
	contextWindow: 200_000,
	maxTokens: 50_000,
});
const catalog = [
	chat("listed"),
	chat("no-limits"),
	chat("router"),
	chat("context-only"),
	chat("duplicate"),
	chat("blocked"),
	{ type: "chat", ...chat("typed-chat") },
	{ type: "image", id: "image", name: "image", api: "openrouter-images", provider: "openrouter", baseUrl: "https://openrouter.ai/api/v1", input: ["text"], output: ["image"], cost: zeroCost },
	{ type: "classifier", id: "classifier", name: "classifier", api: "typesafe-system-one", provider: "openrouter", baseUrl: "https://openrouter.ai/api/v1", input: ["text"], cost: zeroCost, contextWindow: 64_000 },
];
const listed = {
	data: [
		{
			id: "listed",
			context_length: 1_000_000,
			top_provider: { context_length: 262_144, max_completion_tokens: 32_768 },
			pricing: { prompt: "0.000003", completion: "0.000015" },
		},
		{ id: "no-limits", context_length: 0, top_provider: { context_length: null, max_completion_tokens: null } },
		{ id: "router", pricing: { prompt: "-1", completion: "-1" } },
		{ id: "context-only", context_length: 131_072 },
		{ id: "duplicate", context_length: 1000 },
		{ id: "duplicate", context_length: 2000 },
		{ id: "typed-chat", pricing: {} },
		{ id: 7, context_length: 9 },
		"not an object",
		null,
		{ name: "no id" },
	],
};

type Case = { name: string; credential?: unknown; baseUrl?: string; status: number; body: string };
const cases: Case[] = [
	{ name: "api key", credential: { type: "api_key", key: "sk-or-test" }, status: 200, body: JSON.stringify(listed) },
	{ name: "oauth access token", credential: { type: "oauth", access: "oauth-access", refresh: "r", expires: 0 }, status: 200, body: JSON.stringify(listed) },
	{ name: "regional base URL with trailing slashes", credential: { type: "api_key", key: "k" }, baseUrl: "https://us.openrouter.ai/api/v1//", status: 200, body: JSON.stringify(listed) },
	{ name: "no credential", status: 200, body: JSON.stringify(listed) },
	{ name: "empty key", credential: { type: "api_key", key: "" }, status: 200, body: JSON.stringify(listed) },
	{ name: "401", credential: { type: "api_key", key: "k" }, status: 401, body: "" },
	{ name: "403", credential: { type: "api_key", key: "k" }, status: 403, body: "" },
	{ name: "404", credential: { type: "api_key", key: "k" }, status: 404, body: "not found" },
	{ name: "500", credential: { type: "api_key", key: "k" }, status: 500, body: "" },
	{ name: "429", credential: { type: "api_key", key: "k" }, status: 429, body: JSON.stringify(listed) },
	{ name: "not JSON", credential: { type: "api_key", key: "k" }, status: 200, body: "<html>" },
	{ name: "no data", credential: { type: "api_key", key: "k" }, status: 200, body: "{}" },
	{ name: "data not an array", credential: { type: "api_key", key: "k" }, status: 200, body: '{"data":{}}' },
	{ name: "array body", credential: { type: "api_key", key: "k" }, status: 200, body: "[]" },
	{ name: "empty list", credential: { type: "api_key", key: "k" }, status: 200, body: '{"data":[]}' },
];

const realFetch = globalThis.fetch;
const personalize = [];
for (const c of cases) {
	let request: { url: string; headers: Record<string, string> } | undefined;
	globalThis.fetch = (async (url: string, init: { headers: Record<string, string> }) => {
		request = { url: String(url), headers: { ...init.headers } };
		return new Response(c.body, { status: c.status });
	}) as typeof fetch;
	const context = {
		...(c.credential === undefined ? {} : { credential: c.credential }),
		...(c.baseUrl === undefined ? {} : { baseUrl: c.baseUrl }),
		publish: async () => true,
		allowNetwork: true,
		signal: new AbortController().signal,
	};
	let result: unknown;
	let error: string | undefined;
	try {
		const models = await personalizeOpenRouterCatalog(catalog, context);
		result = models === catalog ? "unchanged" : JSON.parse(JSON.stringify(models));
	} catch (e) {
		error = (e as Error).message;
	}
	personalize.push({ ...c, request, ...(error === undefined ? { result } : { error }) });
}
globalThis.fetch = realFetch;

const out = {
	sha,
	cost: pricings.map((pricing) => ({ pricing, cost: openRouterCost(pricing) })),
	catalog,
	personalize,
};
const nonFinite = (_key: string, value: unknown) =>
	typeof value === "number" && !Number.isFinite(value) ? String(value) : value;
fs.writeFileSync(outFile, `${JSON.stringify(out, nonFinite, "\t")}\n`);
console.log(`captured ${out.cost.length} cost rows and ${personalize.length} personalize cases at ${sha} -> ${outFile}`);
