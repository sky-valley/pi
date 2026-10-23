// Captures how real pi applies OpenRouter's billed cost — the oracle behind
// TestApplyOpenRouterReportedCostMatchesPi and TestIsOpenRouterModelMatchesPi
// in package providers.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> openrouter-cost-cae8d69e5.json cae8d69e5
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone). utils/openrouter-cost.ts imports only types, so no
// node_modules is needed. It is a src capture: the 1.4.0 build predates
// cae8d69e5, so re-verify it against the first build that ships it.
//
// Each `apply` row starts from openrouter-cost.test.ts's usage() (catalog
// buckets 0.25 + 0.75, total 1), optionally with totalFromProvider already
// set, applies the reported accounting as JSON.parse would hand it over, and
// records the cost that results (totalFromProvider absent stays absent). The
// rows are the suite's four cases and the JavaScript semantics the port must
// reproduce: strict `=== false` / `=== true` on is_byok, `!details` on
// cost_details, `=== null` (not undefined) choosing the component sum, and
// isValidCost's typeof-number, finite and non-negative checks.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const { applyOpenRouterReportedCost, isOpenRouterModel } = await import(
	pathToFileURL(path.join(extraction, "packages/ai/src/utils/openrouter-cost.ts")).href
);

type Row = { covers: string; reported: string; totalFromProvider?: boolean };
const row = (covers: string, reported: unknown, totalFromProvider?: boolean): Row => ({
	covers,
	reported: JSON.stringify(reported),
	...(totalFromProvider === undefined ? {} : { totalFromProvider }),
});
const details = (upstream: unknown, prompt: unknown = 0.6, completions: unknown = 0.3) => ({
	upstream_inference_cost: upstream,
	upstream_inference_prompt_cost: prompt,
	upstream_inference_completions_cost: completions,
});

const rows: Row[] = [
	// openrouter-cost.test.ts.
	row("pi: non-BYOK uses the reported cost", { cost: 0, is_byok: false, cost_details: details(0, 0, 0) }),
	row("pi: BYOK adds the upstream inference cost", { cost: 0.1, is_byok: true, cost_details: details(0.9) }),
	row("pi: BYOK sums the components when the total is null", { cost: 0.1, is_byok: true, cost_details: details(null) }),
	row("pi: incomplete accounting keeps the catalog total", { cost: null, is_byok: false, cost_details: null }, false),
	row("pi: incomplete accounting keeps the catalog total", { cost: 0.1, is_byok: true, cost_details: null }, false),
	// The production stream fields of the two stream regressions (#9980).
	row("pi: anthropic stream non-BYOK", { cost: 0.004, is_byok: false, cost_details: { upstream_inference_cost: 0.003 } }),
	row("pi: completions stream BYOK", { cost: 0.0002, is_byok: true, cost_details: { upstream_inference_cost: 0.003 } }),

	// cost must be a finite, non-negative number.
	row("cost: absent", { is_byok: false }),
	row("cost: string", { cost: "0.1", is_byok: false }),
	row("cost: boolean", { cost: true, is_byok: false }),
	row("cost: negative", { cost: -0.1, is_byok: false }),
	row("cost: large", { cost: 1e308, is_byok: false }),
	row("cost: object", { cost: {}, is_byok: false }),
	row("cost: invalid replaces nothing that was set", { cost: -1, is_byok: false }, true),

	// is_byok is compared strictly.
	row("is_byok: absent", { cost: 0.1 }),
	row("is_byok: null", { cost: 0.1, is_byok: null }),
	row("is_byok: 0", { cost: 0.1, is_byok: 0 }),
	row("is_byok: 1", { cost: 0.1, is_byok: 1, cost_details: details(0.9) }),
	row("is_byok: string false", { cost: 0.1, is_byok: "false" }),
	row("is_byok: string true", { cost: 0.1, is_byok: "true", cost_details: details(0.9) }),
	row("non-BYOK ignores cost_details", { cost: 0.1, is_byok: false, cost_details: details(-5, "x", null) }),
	row("non-BYOK replaces a provider total", { cost: 0.2, is_byok: false }, true),
	row("non-BYOK replaces a catalog flag", { cost: 0.2, is_byok: false }, false),

	// cost_details must be truthy; a non-object has no upstream_inference_cost.
	row("details: absent", { cost: 0.1, is_byok: true }),
	row("details: empty object", { cost: 0.1, is_byok: true, cost_details: {} }),
	row("details: array", { cost: 0.1, is_byok: true, cost_details: [0.9] }),
	row("details: string", { cost: 0.1, is_byok: true, cost_details: "0.9" }),
	row("details: number", { cost: 0.1, is_byok: true, cost_details: 0.9 }),
	row("details: zero", { cost: 0.1, is_byok: true, cost_details: 0 }),
	row("details: true", { cost: 0.1, is_byok: true, cost_details: true }),

	// The upstream total: a valid number is used; null falls back to the parts;
	// absent (undefined) does not.
	row("upstream: zero", { cost: 0.1, is_byok: true, cost_details: details(0) }),
	row("upstream: absent", { cost: 0.1, is_byok: true, cost_details: { upstream_inference_prompt_cost: 0.6, upstream_inference_completions_cost: 0.3 } }),
	row("upstream: negative", { cost: 0.1, is_byok: true, cost_details: details(-0.9) }),
	row("upstream: string", { cost: 0.1, is_byok: true, cost_details: details("0.9") }),
	row("upstream: object", { cost: 0.1, is_byok: true, cost_details: details({}) }),
	row("upstream: null, a part absent", { cost: 0.1, is_byok: true, cost_details: { upstream_inference_cost: null, upstream_inference_prompt_cost: 0.6 } }),
	row("upstream: null, a part null", { cost: 0.1, is_byok: true, cost_details: details(null, null, 0.3) }),
	row("upstream: null, a part negative", { cost: 0.1, is_byok: true, cost_details: details(null, 0.6, -0.3) }),
	row("upstream: null, a part string", { cost: 0.1, is_byok: true, cost_details: details(null, "0.6", 0.3) }),
	row("upstream: null, parts zero", { cost: 0.1, is_byok: true, cost_details: details(null, 0, 0) }),
	row("upstream: null, float sum", { cost: 0.1, is_byok: true, cost_details: details(null, 0.1, 0.2) }),
	row("upstream: float sum with cost", { cost: 0.1, is_byok: true, cost_details: details(0.2) }),
	row("upstream: server_tool_cost is already in cost", { cost: 0.1, is_byok: true, cost_details: { ...details(0.9), server_tool_cost: 5 } }),

	// A reported value that is not an object has no fields at all.
	row("reported: null", null),
	row("reported: array", [0.1]),
	row("reported: number", 0.1),
];

const usage = (totalFromProvider?: boolean) => ({
	input: 1,
	output: 1,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 2,
	cost: {
		input: 0.25,
		output: 0.75,
		cacheRead: 0,
		cacheWrite: 0,
		total: 1,
		...(totalFromProvider === undefined ? {} : { totalFromProvider }),
	},
});

const models = [
	{ provider: "openrouter", baseUrl: "https://example.com" },
	{ provider: "openrouter-work", baseUrl: "https://openrouter.ai/api/v1" },
	{ provider: "custom", baseUrl: "https://example.com/v1" },
	{ provider: "custom", baseUrl: "https://proxy.example/openrouter.ai/v1" },
	{ provider: "custom", baseUrl: "https://OpenRouter.ai/api/v1" },
	{ provider: "OpenRouter", baseUrl: "https://example.com" },
	{ provider: "openrouter-images", baseUrl: "" },
];

const out = {
	sha,
	apply: rows.map((r) => {
		const value = usage(r.totalFromProvider);
		const reported = JSON.parse(r.reported);
		// pi calls it only with an object (the anthropic path checks
		// `event.usage`, the completions path has a usage object), but the
		// function itself reads properties of whatever it is given; null would
		// throw, which no caller can reach, so it is recorded as such.
		let threw: string | undefined;
		try {
			applyOpenRouterReportedCost(value, reported);
		} catch (error) {
			threw = String(error);
		}
		return { ...r, ...(threw ? { threw } : { cost: value.cost }) };
	}),
	isOpenRouterModel: models.map((m) => ({ ...m, openRouter: isOpenRouterModel(m) })),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`captured ${out.apply.length} apply rows, ${out.isOpenRouterModel.length} model rows at ${sha} -> ${outFile}`);
