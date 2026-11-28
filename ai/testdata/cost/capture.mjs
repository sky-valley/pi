// Captures what real pi's calculateCost returns, bit for bit — the oracle
// behind TestCalculateCostMatchesPi in package ai.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/1.7.0 calculate-cost-1.7.0.json
//
// The capture is from the PUBLISHED build (dist/models.js and the catalog in
// dist/models.generated.js), which is what the goldens in this repo come from.
// Each row records the model's cost table as pi's catalog has it, so the Go
// test prices the same rates without depending on the embedded catalog: a regen
// that moves a price does not stale this file.
//
// V8 rounds every multiply and add of calculateCost separately. Go may fuse
// x*y + z into a single-rounding FMA (arm64 does; so does amd64 at GOAMD64=v3),
// which lands a last-bit different cost on some usages, and those are the
// usages this table is built from:
//   - every catalog model with a pricing tier, at its threshold (base rates)
//     and one token above it (tier rates), with 1h cache writes in the split;
//   - base-rate models from several providers, with and without 1h writes;
//   - usages a sweep found to differ: grok-4.7 on both sides of its tier, and
//     cacheWrite (the (short + 2x-input long) / 1e6 sum), which differs far
//     less often than total;
//   - image output (upstream ccda1f74b): every IMAGE_MODELS entry with an
//     imageOutput rate, with the provider's image-token split, without it, and
//     with a split above output; and models-runtime.test.ts's own rates.
// JSON.stringify prints the shortest text that round-trips a double, so Go's
// strconv reads back the exact bits pi computed.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const load = (file) => import(pathToFileURL(path.join(pkg, "dist", file)).href);
const { MODELS, IMAGE_MODELS } = await load("models.generated.js");
const { calculateCost } = await load("models.js");

// splitAt spreads `total` input-side tokens over input, cacheRead and
// cacheWrite (a share of it 1h), so the tier check sees exactly `total`.
function splitAt(total, output) {
	const cacheRead = Math.floor(total * 0.37) + 11;
	const cacheWrite = Math.floor(total * 0.07) + 7;
	const cacheWrite1h = Math.floor(cacheWrite * 0.41) + 3;
	return { input: total - cacheRead - cacheWrite, output, cacheRead, cacheWrite, cacheWrite1h };
}

const cases = [];
for (const [provider, models] of Object.entries(MODELS)) {
	for (const [id, model] of Object.entries(models)) {
		for (const tier of model.cost.tiers ?? []) {
			cases.push([provider, id, splitAt(tier.inputTokensAbove, 23457)]);
			cases.push([provider, id, splitAt(tier.inputTokensAbove + 1, 31337)]);
		}
	}
}
const plain = [
	{ input: 213280, output: 37248, cacheRead: 16288, cacheWrite: 128, cacheWrite1h: 0 },
	{ input: 173664, output: 30528, cacheRead: 26688, cacheWrite: 30128, cacheWrite1h: 0 },
	{ input: 222432, output: 20480, cacheRead: 257984, cacheWrite: 31824, cacheWrite1h: 12345 },
	{ input: 91264, output: 61184, cacheRead: 243296, cacheWrite: 40544, cacheWrite1h: 40544 },
];
for (const key of [
	"amazon-bedrock/amazon.nova-2-lite-v1:0",
	"amazon-bedrock/amazon.nova-micro-v1:0",
	"anthropic/claude-opus-5-5",
	"anthropic/claude-sonnet-4-6",
	"google/gemini-2.5-pro",
	"openrouter/moonshotai/kimi-k2.5",
]) {
	const slash = key.indexOf("/");
	for (const usage of plain) cases.push([key.slice(0, slash), key.slice(slash + 1), usage]);
}
cases.push(
	// grok-4.7 at its 200k threshold and one token above it.
	["xai", "grok-4.7", { input: 164300, output: 4686, cacheRead: 33294, cacheWrite: 2406, cacheWrite1h: 167 }],
	["xai", "grok-4.7", { input: 184451, output: 1679, cacheRead: 14291, cacheWrite: 1259, cacheWrite1h: 918 }],
	["github-copilot", "gpt-6-luna", { input: 204451, output: 15616, cacheRead: 49150, cacheWrite: 18400, cacheWrite1h: 10146 }],
	["openai-codex", "gpt-5.6-luna", { input: 70401, output: 35384, cacheRead: 199520, cacheWrite: 2080, cacheWrite1h: 577 }],
	["openai-codex", "gpt-6-luna", { input: 265984, output: 27648, cacheRead: 5632, cacheWrite: 384, cacheWrite1h: 299 }],
	["openai-codex", "gpt-6-luna", { input: 113488, output: 31544, cacheRead: 151633, cacheWrite: 6880, cacheWrite1h: 2748 }],
	// One usage per conversion the rows above leave unlocked: fusing the
	// cacheRead product into the total, and the short cacheWrite product into
	// the cacheWrite sum, each land a last bit off pi's here.
	["amazon-bedrock", "amazon.nova-2-lite-v1:0", { input: 145664, output: 32768, cacheRead: 73248, cacheWrite: 22656, cacheWrite1h: 22092 }],
	["amazon-bedrock", "amazon.nova-2-lite-v1:0", { input: 115328, output: 29312, cacheRead: 64608, cacheWrite: 46816, cacheWrite1h: 31298 }],
);

// Image rows: catalog image models carry imageOutput; output = text +
// reasoning + image tokens, and imageOutput is the provider's image share.
const imageUsages = [
	{ input: 10, output: 1370, cacheRead: 0, cacheWrite: 0, reasoning: 250, imageOutput: 1120 },
	{ input: 10, output: 1370, cacheRead: 0, cacheWrite: 0, reasoning: 250 },
	{ input: 10, output: 1370, cacheRead: 0, cacheWrite: 0, reasoning: 250, imageOutput: 5000 },
	{ input: 4182, output: 6913, cacheRead: 517, cacheWrite: 0, imageOutput: 0 },
	{ input: 4182, output: 6913, cacheRead: 517, cacheWrite: 0, imageOutput: 5329 },
];
const imageCases = [];
for (const [provider, models] of Object.entries(IMAGE_MODELS)) {
	for (const [id, model] of Object.entries(models)) {
		if (model.cost.imageOutput === undefined) continue;
		for (const usage of imageUsages) imageCases.push([`${provider}/${id}`, model.cost, usage]);
	}
}
// models-runtime.test.ts "prices image output tokens at the image rate".
const testRates = { input: 2, output: 12, imageOutput: 120, cacheRead: 0.2, cacheWrite: 0.375 };
for (const usage of imageUsages.slice(0, 3)) imageCases.push(["models-runtime.test.ts", testRates, usage]);
const { imageOutput: _, ...withoutImageRate } = testRates;
imageCases.push(["models-runtime.test.ts", withoutImageRate, imageUsages[0]]);
if (imageCases.length < 20) throw new Error(`only ${imageCases.length} image rows: has IMAGE_MODELS lost imageOutput?`);

const price = (label, rates, usage) => {
	const u = { ...usage, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
	calculateCost({ cost: rates }, u);
	return { model: label, rates, usage, cost: u.cost };
};

const rows = cases.map(([provider, id, usage]) => {
	const model = MODELS[provider]?.[id];
	if (!model) throw new Error(`catalog has no ${provider}/${id}`);
	const u = { ...usage, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
	calculateCost(model, u);
	return { model: `${provider}/${id}`, rates: model.cost, usage, cost: u.cost };
});
for (const [label, rates, usage] of imageCases) rows.push(price(label, rates, usage));
// One row per line, so a re-capture diffs row by row.
const body = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(outFile, `{"pi-ai":${JSON.stringify(version)},"rows":[\n${body}\n]}\n`);
console.log(`captured ${rows.length} rows from @earendil-works/pi-ai ${version} -> ${outFile}`);
