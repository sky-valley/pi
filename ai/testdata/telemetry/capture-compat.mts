// Captures what pi's compat stream()/streamSimple() record into an
// InMemoryTelemetryContext on the Cloudflare branch (compat.ts: a built-in
// cloudflare-* model without a resolved key routes through compatModels, the
// Models runtime, which observes the request) — the oracle behind
// TestCompatCloudflareStreamsThroughModels.
//
//   env -i PATH="$PATH" HOME="$HOME" node capture-compat.mts <extraction> <out.json> <sha>
//
// <extraction> is capture.mts's (packages/ai/src and packages/telemetry/src
// at <sha>, pi-ai's node_modules, and the pi-telemetry link), plus the
// generated catalog compat.ts loads: packages/ai/src/providers/data/ is not
// in git, so copy the npm build's dist/providers/data/ (both rows' models are
// in it). Run it with an empty environment: every row expects no Cloudflare
// credentials, so none reaches the network. A src capture; no release ships
// 2ec55d808 yet.
import path from "node:path";
import fs from "node:fs";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node capture-compat.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
for (const name of Object.keys(process.env)) {
	if (name.startsWith("CLOUDFLARE_")) throw new Error(`run with an empty environment: ${name} is set`);
}
const load = (file: string) => import(pathToFileURL(path.join(extraction, file)).href);
const compat = await load("packages/ai/src/compat.ts");
const { InMemoryTelemetryContext } = await load("packages/telemetry/src/index.ts");

async function settled(telemetry: any): Promise<void> {
	for (let i = 0; i < 500; i++) {
		if (telemetry.getSpans().every((span: any) => span.settled)) return;
		await new Promise((resolve) => setTimeout(resolve, 2));
	}
	throw new Error("spans never settled");
}

const spanRef = (carrier: any): number | null => (carrier ? Number(carrier["pi-memory-span"]) : null);

// capture.mts's record().
function record(telemetry: any) {
	const spans = telemetry.getSpans().map((span: any) => ({
		id: span.id,
		parentId: span.parentId,
		context: spanRef(span.context),
		parentContext: spanRef(span.parentContext),
		links: span.links.map((link: any) => ({ context: spanRef(link.context), attributes: link.attributes })),
		name: span.name,
		attributes: span.attributes,
		events: span.events,
		status: span.status,
		settled: span.settled,
		endSequence: span.endSequence ?? null,
	}));
	const measurements = telemetry.getMetrics().map((m: any) => ({
		kind: m.kind,
		name: m.name,
		description: m.description ?? null,
		unit: m.unit ?? null,
		boundaries: m.boundaries ?? null,
		value: m.name.endsWith(".duration") ? (typeof m.value === "number" && m.value >= 0 ? "duration" : m.value) : m.value,
		attributes: m.attributes,
		context: spanRef(m.context),
	}));
	return { spans, measurements };
}

interface Row {
	name: string;
	entry: "stream" | "streamSimple";
	provider: string;
	modelId: string;
	type?: string;
	reasoning?: string;
}

const rows: Row[] = [
	{ name: "workers-ai-stream", entry: "stream", provider: "cloudflare-workers-ai", modelId: "@cf/deepseek-ai/deepseek-v4-flash-0731" },
	{ name: "workers-ai-streamSimple", entry: "streamSimple", provider: "cloudflare-workers-ai", modelId: "@cf/deepseek-ai/deepseek-v4-flash-0731", reasoning: "low" },
	{ name: "ai-gateway-stream", entry: "stream", provider: "cloudflare-ai-gateway", modelId: "claude-haiku-4-5" },
	{ name: "ai-gateway-streamSimple", entry: "streamSimple", provider: "cloudflare-ai-gateway", modelId: "claude-haiku-4-5" },
	// A caller-built non-chat model: compatModels' requireChatProvider refuses it.
	{ name: "workers-ai-not-chat", entry: "stream", provider: "cloudflare-workers-ai", modelId: "@cf/deepseek-ai/deepseek-v4-flash-0731", type: "image" },
];

const out: any = { sha, rows: [] };
for (const row of rows) {
	const catalog = compat.getModel(row.provider, row.modelId);
	if (!catalog) throw new Error(`${row.provider}/${row.modelId} is not in the catalog at ${sha}`);
	const model = row.type ? { ...catalog, type: row.type } : catalog;
	const telemetry = new InMemoryTelemetryContext();
	const delivered: string[] = [];
	let result: any;
	await telemetry.startSpan({ name: "caller" }, async (span: any) => {
		const options = { telemetry: span, ...(row.reasoning ? { reasoning: row.reasoning } : {}) };
		const stream =
			row.entry === "stream"
				? compat.stream(model, { messages: [] }, options)
				: compat.streamSimple(model, { messages: [] }, options);
		for await (const event of stream) delivered.push(event.type);
		result = await stream.result();
	});
	await settled(telemetry);
	out.rows.push({
		...row,
		type: row.type ?? null,
		reasoning: row.reasoning ?? null,
		delivered,
		stopReason: result.stopReason,
		errorMessage: result.errorMessage ?? null,
		...record(telemetry),
	});
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${out.rows.length} rows to ${outFile}`);
