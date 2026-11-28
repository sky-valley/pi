// Captures the usage.cost real pi reports after service-tier pricing, bit for
// bit — the oracle behind TestResponsesServiceTierPricingMatchesPi.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/1.7.0 service-tier-1.7.0.json
//
// Each row streams the published build's openai-responses adapter against a
// loopback server that answers with the row's SSE body, so neither side needs a
// key or the network. The row records that body, the cost table pi priced it
// at (the catalog's, with a case's overrides), and the requested service tier;
// the Go test replays the same body at the same rates.
//
// applyServiceTierPricing multiplies each bucket and then re-adds the total.
// V8 rounds every multiply and add separately; Go may fuse x*y + z into one
// FMA (arm64; amd64 at GOAMD64=v3), which moves the total's last bit when the
// multiplier is not a power of two: gpt-5.5 at priority (x2.5). The usages are
// ones a sweep found to differ that way: the first rows move the total when the
// input or output product fuses, usage(890, ...) when the cacheRead one does.
// No catalog rate can show the cacheWrite product fusing (every gpt-5.5 entry
// has cacheWrite 0, and the other multipliers are powers of two), so the last
// row prices gpt-5.5 at a synthetic cacheWrite rate, which the row records like
// any other. The x2 and x0.5 rows are exact either way and pin the multiplier
// selection.
import http from "node:http";
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
const { MODELS } = await load("models.generated.js");
const { stream } = await load("api/openai-responses.js");

// sse builds the stream the server answers with: one text item, then
// response.completed carrying the usage and, when set, the reported tier.
function sse(usage, responseTier) {
	const completed = { id: "r", status: "completed" };
	if (responseTier) completed.service_tier = responseTier;
	completed.usage = usage;
	return [
		{ type: "response.created", response: { id: "r" } },
		{ type: "response.output_item.added", item: { type: "message", id: "msg_1" } },
		{ type: "response.content_part.added", part: { type: "output_text", text: "" } },
		{ type: "response.output_text.delta", delta: "hi" },
		{ type: "response.output_item.done", item: { type: "message", id: "msg_1", content: [{ type: "output_text", text: "hi" }] } },
		{ type: "response.completed", response: completed },
	]
		.map((event) => `data: ${JSON.stringify(event)}\n\n`)
		.join("");
}

const usage = (input, output, cached, cacheWrite) => ({
	input_tokens: input + cached + cacheWrite,
	output_tokens: output,
	total_tokens: input + cached + cacheWrite + output,
	input_tokens_details: { cached_tokens: cached, cache_write_tokens: cacheWrite },
});

// [model id, requested serviceTier, response service_tier, usage, rates over the catalog's]
const cases = [
	["gpt-5.5", "priority", "", usage(77945, 61893, 23632, 0)],
	["gpt-5.5", "priority", "", usage(256288, 31010, 8780, 0)],
	["gpt-5.5", "", "priority", usage(77945, 61893, 23632, 0)],
	["gpt-5.5", "priority", "", usage(190001, 40961, 90000, 12345)],
	["gpt-5", "priority", "", usage(77945, 61893, 23632, 0)],
	["gpt-5", "flex", "", usage(256288, 31010, 8780, 0)],
	["gpt-5.5", "priority", "default", usage(77945, 61893, 23632, 0)],
	["gpt-5.5", "priority", "", usage(890, 17103, 149970, 17074)],
	["gpt-5.5", "priority", "", usage(89278, 1675, 10506, 18820), { cacheWrite: 6.25 }],
];

async function run(model, serviceTier, body) {
	const srv = http.createServer((req, res) => {
		req.resume();
		res.writeHead(200, { "content-type": "text/event-stream" });
		res.end(body);
	});
	await new Promise((r) => srv.listen(0, "127.0.0.1", r));
	try {
		const options = { apiKey: "sk", maxRetries: 0 };
		if (serviceTier) options.serviceTier = serviceTier;
		const s = stream(
			{ ...model, baseUrl: `http://127.0.0.1:${srv.address().port}` },
			{ messages: [{ role: "user", content: "hi", timestamp: 1 }] },
			options,
		);
		const message = await s.result();
		if (message.stopReason !== "stop") throw new Error(`stopReason ${message.stopReason}: ${message.errorMessage}`);
		return message.usage;
	} finally {
		srv.close();
	}
}

const rows = [];
for (const [id, serviceTier, responseTier, u, rates] of cases) {
	const catalog = MODELS.openai[id];
	if (!catalog) throw new Error(`catalog has no openai/${id}`);
	const model = rates ? { ...catalog, cost: { ...catalog.cost, ...rates } } : catalog;
	const body = sse(u, responseTier);
	const got = await run(model, serviceTier, body);
	rows.push({ model: `openai/${id}`, rates: model.cost, serviceTier, sse: body, cost: got.cost });
}
// One row per line, so a re-capture diffs row by row.
const text = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(outFile, `{"pi-ai":${JSON.stringify(version)},"rows":[\n${text}\n]}\n`);
console.log(`captured ${rows.length} rows from @earendil-works/pi-ai ${version} -> ${outFile}`);
