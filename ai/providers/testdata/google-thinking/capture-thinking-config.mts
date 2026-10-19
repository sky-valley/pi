// Captures the generationConfig.thinkingConfig real pi sends through the
// google-generative-ai streamSimple for a table of models and reasoning levels —
// the oracle behind TestGoogleThinkingConfigMatchesPi in this package.
//
//   node --experimental-strip-types capture-thinking-config.mts <extraction> <out.json> <sha>
//   e.g. ... capture-thinking-config.mts <dir> thinking-config-1692bba63.json 1692bba63
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies (the npm
// build's: at 1692bba63 the lockfile's @google/genai 2.21.0 and typebox 1.3.27
// are the npm 1.3.0 build's, integrity for integrity). <extraction> may instead
// be a published @earendil-works/pi-ai package directory, whose dist is loaded:
// the BUILD wins where the two disagree. 1.3.0's dist and 1692bba63's src
// record the same file.
//
// Models are packages/ai/test/google-thinking-level-map.test.ts's googleModel
// (synthetic, catalog-independent: each case carries its own thinkingLevelMap,
// some shaped like the shipped catalog's rows). onPayload sees the @google/genai
// call params, a different layer from the REST body the Go adapter builds, so
// the body is captured where the SDK POSTs it (globalThis.fetch). Each entry
// records exactly one outcome: `thinkingConfig` (null when the body has none),
// `thrown` (streamSimple threw before returning a stream) or `error` (the
// stream ended in an error before any request was sent).
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-thinking-config.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const fromSource = fs.existsSync(path.join(extraction, "packages/ai/src"));
const load = (file: string) =>
	import(
		pathToFileURL(
			fromSource ? path.join(extraction, "packages/ai/src", `${file}.ts`) : path.join(extraction, "dist", `${file}.js`),
		).href
	);
const { normalizeContext } = await load("utils/transcript");
const { streamSimple } = await load("api/google-generative-ai");
// typebox as pi-ai resolves it: the first node_modules up from its package.
const typebox = createRequire(path.join(fromSource ? path.join(extraction, "packages/ai") : extraction, "package.json"))
	.resolve.paths("typebox")
	?.map((dir) => path.join(dir, "typebox"))
	.find((dir) => fs.existsSync(path.join(dir, "package.json")));
if (!typebox) throw new Error("typebox does not resolve from pi-ai");
const { Type } = await import(pathToFileURL(path.join(typebox, "build/index.mjs")).href);

type Case = {
	name: string;
	id: string;
	thinkingLevelMap?: Record<string, string | null>;
	reasoning?: string;
	thinkingBudgets?: Record<string, number>;
	modelReasoning?: boolean;
	// A json_schema tool with strict "require": unsupported on non-Gemini-3 ids,
	// so building its declaration throws.
	strictRequiredTool?: boolean;
};

// The shipped catalog's shapes for the three Gemini families that use levels.
const proMap = { off: null, minimal: null, low: "LOW", medium: null, high: "HIGH" };
const flashMap = { off: null };
const gemmaMap = { off: null, minimal: "MINIMAL", low: null, medium: null, high: "HIGH" };
// google-thinking-level-map.test.ts's models.dev-effort map (#9455).
const effortMap = { off: null, minimal: null, low: "low", medium: "medium", high: "high", xhigh: null, max: null };

const cases: Case[] = [
	// google-thinking-level-map.test.ts @16235fd93, google adapter.
	{ name: "upstream/omitted uses the lowest supported level", id: "gemini-3.8-flash", thinkingLevelMap: effortMap },
	{
		name: "upstream/native medium on Gemini 3.1 Pro",
		id: "gemini-3.1-pro-preview",
		thinkingLevelMap: effortMap,
		reasoning: "medium",
	},
	{ name: "upstream/omitted disables Gemini 2.5", id: "gemini-2.5-flash", thinkingLevelMap: {} },
	{ name: "upstream/xhigh maps", id: "gemini-3.7-flash", thinkingLevelMap: { xhigh: "high", max: "high" }, reasoning: "xhigh" },
	{ name: "upstream/max maps", id: "gemini-3.7-flash", thinkingLevelMap: { xhigh: "high", max: "high" }, reasoning: "max" },
	{ name: "upstream/uppercase provider value", id: "gemini-3.7-flash", thinkingLevelMap: { high: "LOW" }, reasoning: "high" },
	{
		name: "upstream/mapped level keys the token budget",
		id: "gemini-2.5-flash",
		thinkingLevelMap: { xhigh: "high" },
		reasoning: "xhigh",
		thinkingBudgets: { high: 1234 },
	},
	{ name: "upstream/unmappable level throws", id: "gemini-3.7-flash", thinkingLevelMap: { xhigh: "extreme" }, reasoning: "xhigh" },

	// Disabled thinking (reasoning omitted) per family, catalog-shaped maps.
	{ name: "disabled/pro catalog map", id: "gemini-3.1-pro-preview", thinkingLevelMap: proMap },
	{ name: "disabled/flash catalog map", id: "gemini-3-flash-preview", thinkingLevelMap: flashMap },
	{ name: "disabled/gemma catalog map", id: "gemma-4-31b-it", thinkingLevelMap: gemmaMap },
	{ name: "disabled/flash-latest catalog map", id: "gemini-flash-latest", thinkingLevelMap: flashMap },
	{ name: "disabled/flash-lite-latest catalog map", id: "gemini-flash-lite-latest", thinkingLevelMap: flashMap },
	{ name: "disabled/gemini 2.5 no map", id: "gemini-2.5-flash" },
	// Without a map "off" is supported, so level models disable with a budget too.
	{ name: "disabled/pro no map", id: "gemini-3-pro-preview" },
	{ name: "disabled/flash no map", id: "gemini-3-flash-preview" },
	{ name: "disabled/gemma4 no dash no map", id: "gemma4-26b" },
	// Only level models consult the map for a fallback.
	{ name: "disabled/budget model ignores an unsupported off", id: "gemini-2.5-flash", thinkingLevelMap: { off: null } },
	// The fallback clamps upward past every unsupported level.
	{
		name: "disabled/fallback clamps up to xhigh",
		id: "gemini-3.8-flash",
		thinkingLevelMap: { off: null, minimal: null, low: null, medium: null, high: null, xhigh: "high" },
	},
	// The fallback resolves through the map and can fail the request.
	{ name: "disabled/unmappable fallback fails", id: "gemini-3.8-flash", thinkingLevelMap: { off: null, minimal: "extreme" } },
	// The error renders the mapped value as written, not lowercased.
	{
		name: "disabled/unmappable fallback keeps the mapped value's case",
		id: "gemma-4-31b-it",
		thinkingLevelMap: { off: null, minimal: "MAX" },
	},
	// buildParams converts the tools before the thinking config.
	{
		name: "disabled/tool error precedes unmappable fallback",
		id: "gemma-4-31b-it",
		thinkingLevelMap: { off: null, minimal: "extreme" },
		strictRequiredTool: true,
	},
	{ name: "disabled/non-reasoning model sends none", id: "gemini-3-flash-preview", modelReasoning: false },

	// Explicit and clamped "off" (#9455).
	{ name: "off/explicit on gemini 2.5", id: "gemini-2.5-flash", reasoning: "off" },
	{ name: "off/explicit on flash no map", id: "gemini-3-flash-preview", reasoning: "off" },
	{
		name: "off/clamped when only off is supported",
		id: "gemini-3.8-flash",
		thinkingLevelMap: { minimal: null, low: null, medium: null, high: null },
		reasoning: "high",
	},
	{ name: "off/explicit on pro catalog map clamps up", id: "gemini-3.1-pro-preview", thinkingLevelMap: proMap, reasoning: "off" },

	// Enabled levels follow the map through the identity table.
	{ name: "level/pro catalog map minimal", id: "gemini-3.1-pro-preview", thinkingLevelMap: proMap, reasoning: "minimal" },
	{ name: "level/pro catalog map medium", id: "gemini-3.1-pro-preview", thinkingLevelMap: proMap, reasoning: "medium" },
	{ name: "level/pro no map medium", id: "gemini-3.1-pro-preview", reasoning: "medium" },
	{ name: "level/pro no map minimal", id: "gemini-3-pro-preview", reasoning: "minimal" },
	{ name: "level/gemma catalog map low", id: "gemma-4-31b-it", thinkingLevelMap: gemmaMap, reasoning: "low" },
	{ name: "level/gemma no map low", id: "gemma-4-31b-it", reasoning: "low" },
	{ name: "level/gemma4 no dash no map low", id: "gemma4-26b", reasoning: "low" },
	// The id match is case-insensitive.
	{ name: "level/mixed-case pro id", id: "Gemini-3.1-Pro-Preview", reasoning: "medium" },
	{ name: "level/flash-lite-latest no map minimal", id: "gemini-flash-lite-latest", reasoning: "minimal" },
	{ name: "level/flash catalog map minimal", id: "gemini-3.8-flash", thinkingLevelMap: flashMap, reasoning: "minimal" },
	{
		name: "level/unmappable value keeps its case",
		id: "gemini-3.7-flash",
		thinkingLevelMap: { high: "XHIGH" },
		reasoning: "high",
	},

	// Which ids use levels. The two aliases match only as whole ids; the Gemini 3
	// and Gemma 4 patterns match anywhere in the id; the Gemini 3 minor version is
	// one or more digits and the major version is exactly 3. A budget model sends
	// thinkingBudget (-1 for an unknown family, 0 when disabled).
	{ name: "id/flash-latest with a suffix uses budgets", id: "gemini-flash-latest-001", reasoning: "low" },
	{ name: "id/flash-latest with a prefix uses budgets", id: "models/gemini-flash-latest", thinkingLevelMap: flashMap },
	{ name: "id/flash-lite-latest with a suffix uses budgets", id: "gemini-flash-lite-latest-001", reasoning: "low" },
	{
		name: "id/flash-lite-latest with a prefix uses budgets",
		id: "models/gemini-flash-lite-latest",
		thinkingLevelMap: flashMap,
	},
	{ name: "id/gemini 3 after a path prefix uses levels", id: "models/gemini-3-pro-preview", reasoning: "low" },
	{ name: "id/gemini 3 two-digit minor uses levels", id: "gemini-3.10-flash-lite", reasoning: "low" },
	{ name: "id/gemini 3 non-digit minor uses budgets", id: "gemini-3.x-pro", reasoning: "low" },
	{ name: "id/gemini 4 pro uses levels", id: "gemini-4-pro", reasoning: "low" },
	{ name: "id/gemma 4 inside a longer name uses levels", id: "codegemma4", reasoning: "low" },

	// Gemini 4 and later use levels (1692bba63, #10890): a major version of 4 to
	// 9, or two digits not starting with 0, anywhere in the id.
	// google-thinking-level-map.test.ts @1692bba63, google adapter.
	{ name: "upstream/gemini 4 uses thinking levels", id: "gemini-4-argon", thinkingLevelMap: {}, reasoning: "medium" },
	// The shipped catalog's gemini-4-argon map, at every level.
	{ name: "gemini4/catalog map disabled", id: "gemini-4-argon", thinkingLevelMap: effortMap },
	{ name: "gemini4/catalog map off", id: "gemini-4-argon", thinkingLevelMap: effortMap, reasoning: "off" },
	{ name: "gemini4/catalog map minimal", id: "gemini-4-argon", thinkingLevelMap: effortMap, reasoning: "minimal" },
	{ name: "gemini4/catalog map low", id: "gemini-4-argon", thinkingLevelMap: effortMap, reasoning: "low" },
	{ name: "gemini4/catalog map high", id: "gemini-4-argon", thinkingLevelMap: effortMap, reasoning: "high" },
	{ name: "gemini4/catalog map xhigh", id: "gemini-4-argon", thinkingLevelMap: effortMap, reasoning: "xhigh" },
	{ name: "gemini4/opencode-go id", id: "gemini-4-argon-contributor", thinkingLevelMap: effortMap, reasoning: "high" },
	{ name: "gemini4/no map disabled", id: "gemini-4-argon" },
	{ name: "id/gemini 5 uses levels", id: "gemini-5-flash", reasoning: "low" },
	{ name: "id/gemini 9 uses levels", id: "gemini-9", reasoning: "low" },
	{ name: "id/gemini 10 uses levels", id: "gemini-10-pro", reasoning: "low" },
	{ name: "id/gemini 40 uses levels", id: "gemini-40", reasoning: "low" },
	{ name: "id/gemini 4 after a path prefix uses levels", id: "models/gemini-4-argon", reasoning: "low" },
	{ name: "id/mixed-case gemini 4 uses levels", id: "Gemini-4-Argon", reasoning: "low" },
	{ name: "id/gemini 04 uses budgets", id: "gemini-04-pro", reasoning: "low" },
	{ name: "id/gemini 1 uses budgets", id: "gemini-1-pro", reasoning: "low" },
	{ name: "id/gemini 2.5 uses budgets", id: "gemini-2.5-pro", reasoning: "low" },
	{ name: "id/gemini 3 lite uses budgets", id: "gemini-3-lite", reasoning: "low" },
];

function googleModel(c: Case) {
	return {
		id: c.id,
		name: c.id,
		api: "google-generative-ai",
		provider: "test-google",
		baseUrl: "https://example.invalid/v1beta",
		reasoning: c.modelReasoning ?? true,
		...(c.thinkingLevelMap !== undefined && { thinkingLevelMap: c.thinkingLevelMap }),
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 128000,
		maxTokens: 4096,
	};
}

function context(c: Case) {
	return normalizeContext({
		messages: [{ role: "user", content: "Hello", timestamp: 0 }],
		...(c.strictRequiredTool && {
			tools: [
				{
					name: "strict_tool",
					description: "strict tool",
					parameters: Type.Object({}),
					constrainedSampling: { type: "json_schema", strict: "require" },
				},
			],
		}),
	});
}

async function capture(c: Case): Promise<Record<string, unknown>> {
	const realFetch = globalThis.fetch;
	let body: string | undefined;
	globalThis.fetch = (async (_url: unknown, init?: { body?: unknown }) => {
		body ??= String(init?.body);
		throw new Error("body captured");
	}) as typeof fetch;
	try {
		let stream: { result(): Promise<{ errorMessage?: string }> };
		try {
			stream = streamSimple(googleModel(c), context(c), {
				apiKey: "test",
				maxRetries: 0,
				...(c.reasoning !== undefined && { reasoning: c.reasoning }),
				...(c.thinkingBudgets !== undefined && { thinkingBudgets: c.thinkingBudgets }),
			});
		} catch (error) {
			return { thrown: (error as Error).message };
		}
		const final = await stream.result();
		if (body === undefined) return { error: final.errorMessage };
		const generationConfig = JSON.parse(body).generationConfig ?? {};
		return { thinkingConfig: generationConfig.thinkingConfig ?? null };
	} finally {
		globalThis.fetch = realFetch;
	}
}

const out = { sha, cases: [] as unknown[] };
for (const c of cases) {
	out.cases.push({ ...c, ...(await capture(c)) });
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
