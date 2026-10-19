// Captures what pi's Models records into an InMemoryTelemetryContext — the
// spans, events, attributes, statuses, links and measurements of the
// pi.ai.request / pi.ai.poll observation (packages/ai/src/telemetry.ts,
// upstream 2ec55d808) — the oracle behind TestModelsTelemetryMatchesPi and
// TestModelsTelemetryCreatesInstrumentsOncePerMetrics.
//
//   node capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> telemetry-d2e7d8b7c.json d2e7d8b7c
//
// <extraction> holds packages/ai/src and packages/telemetry/src at <sha>
// (`git archive <sha> packages/ai packages/telemetry`), a node_modules
// resolving pi-ai's dependencies (the npm build's), and a
// packages/ai/node_modules/@earendil-works/pi-telemetry package whose "."
// export is ./src/index.ts with src linked to packages/telemetry/src: pi-ai at
// <sha> reads telemetry exports no published pi-telemetry has. It is a src
// capture; no release ships 2ec55d808 yet.
//
// Every row runs one Models request under a caller span against a provider
// that replays a scripted event list, waits for every span to settle, and
// records the recorder's spans and measurements. Carriers are written as the
// recorder-local span id they name (their scope is a random UUID), and a
// duration is written as "duration" (pi reads performance.now()). Factory
// calls record each instrument a request created, with the metadata the
// schema handed the adapter.
import path from "node:path";
import fs from "node:fs";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const load = (file: string) => import(pathToFileURL(path.join(extraction, file)).href);
const { createModels, createProvider } = await load("packages/ai/src/models.ts");
const { AssistantMessageEventStream } = await load("packages/ai/src/utils/event-stream.ts");
const { InMemoryTelemetryContext } = await load("packages/telemetry/src/index.ts");

const model = {
	id: "model",
	name: "Model",
	api: "telemetry-test",
	provider: "test",
	baseUrl: "https://example.test",
	reasoning: true,
	input: ["text"],
	cost: { input: 1, output: 2, cacheRead: 3, cacheWrite: 4 },
	contextWindow: 1000,
	maxTokens: 100,
};
const handle = { provider: model.provider, modelId: model.id, api: model.api, id: "deferred-secret" };

// Text and tool deltas carry astral characters so block chars pin UTF-16
// lengths: "a😀b" is 4, '{"v":"😀"}' is 10.
function message(stopReason: string, extra: Record<string, unknown> = {}) {
	return {
		role: "assistant",
		api: model.api,
		provider: model.provider,
		model: model.id,
		content: [
			{ type: "text", text: "a😀b" },
			{ type: "thinking", thinking: "thinking-secret" },
			{ type: "toolCall", id: "call-secret", name: "lookup", arguments: { v: "😀" } },
		],
		usage: {
			input: 11,
			output: 22,
			cacheRead: 33,
			cacheWrite: 44,
			totalTokens: 110,
			cost: { input: 0.1, output: 0.2, cacheRead: 0.3, cacheWrite: 0.4, total: 1.5 },
		},
		responseId: "response",
		stopReason,
		timestamp: Date.now(),
		...extra,
	};
}

function terminal(result: any) {
	return result.stopReason === "error" || result.stopReason === "aborted"
		? { type: "error", reason: result.stopReason, error: result }
		: { type: "done", reason: result.stopReason === "pending" ? "deferred" : result.stopReason, message: result };
}

function blockEvents(result: any) {
	const toolCall = result.content[2];
	return [
		{ type: "start", partial: result },
		{ type: "text_delta", contentIndex: 0, delta: "a😀b", partial: result },
		{ type: "text_end", contentIndex: 0, content: "a😀b", partial: result },
		{ type: "thinking_end", contentIndex: 1, content: "thinking-secret", partial: result },
		{ type: "toolcall_delta", contentIndex: 2, delta: '{"v":', partial: result },
		{ type: "toolcall_delta", contentIndex: 2, delta: '"😀"}', partial: result },
		{ type: "toolcall_end", contentIndex: 2, toolCall, partial: result },
		terminal(result),
	];
}

// Tool deltas for blocks 2 and 3 interleave; block 4 ends with no delta.
function interleavedEvents(result: any) {
	const toolCall = { type: "toolCall", id: "call", name: "lookup", arguments: {} };
	return [
		{ type: "toolcall_delta", contentIndex: 2, delta: "ab", partial: result },
		{ type: "toolcall_delta", contentIndex: 3, delta: "123", partial: result },
		{ type: "toolcall_delta", contentIndex: 2, delta: "c", partial: result },
		{ type: "toolcall_end", contentIndex: 3, toolCall, partial: result },
		{ type: "toolcall_end", contentIndex: 2, toolCall, partial: result },
		{ type: "toolcall_end", contentIndex: 4, toolCall, partial: result },
		terminal(result),
	];
}

function modelsReplaying(events: any[], result: any) {
	const models = createModels();
	const respond = () => {
		const stream = new AssistantMessageEventStream();
		for (const event of events) stream.push(event);
		stream.end(result);
		return stream;
	};
	models.setProvider(
		createProvider({
			id: model.provider,
			models: [model],
			auth: { apiKey: { name: "Test", resolve: async () => ({ auth: {} }) } },
			api: { stream: respond, streamSimple: respond, fetchDeferred: respond },
		}),
	);
	return models;
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
async function settled(telemetry: any): Promise<void> {
	for (let i = 0; i < 500; i++) {
		if (telemetry.getSpans().every((span: any) => span.settled)) return;
		await sleep(2);
	}
	throw new Error("spans never settled");
}

function spanRef(carrier: any): number | null {
	return carrier ? Number(carrier["pi-memory-span"]) : null;
}

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

// spyFactories logs each instrument the recorder creates and the metadata it
// receives, then delegates.
function spyFactories(telemetry: any, calls: any[]) {
	for (const kind of ["counter", "upDownCounter", "histogram"]) {
		const original = telemetry[kind].bind(telemetry);
		telemetry[kind] = (name: string, options?: any) => {
			calls.push({
				kind,
				name,
				description: options?.description ?? null,
				unit: options?.unit ?? null,
				boundaries: kind === "histogram" ? (options?.boundaries ?? null) : null,
			});
			return original(name, options);
		};
	}
}

// request runs one Models entry point and returns the delivered event types
// and the stream's result.
async function request(models: any, entry: string, telemetry: any, reasoning?: string) {
	const options = { telemetry, ...(reasoning === undefined ? {} : { reasoning }) };
	const stream =
		entry === "Stream"
			? models.stream(model, { messages: [] }, options)
			: entry === "StreamSimple"
				? models.streamSimple(model, { messages: [] }, options)
				: models.streamDeferred(model, handle, options);
	const delivered: string[] = [];
	for await (const event of stream) delivered.push(event.type);
	return { delivered, result: await stream.result() };
}

const scripts: Record<string, (result: any) => any[]> = {
	blocks: blockEvents,
	interleaved: interleavedEvents,
	"blocks-without-terminal": (result) => blockEvents(result).slice(0, -1),
	none: () => [],
};

interface Row {
	name: string;
	entry: "Stream" | "StreamSimple" | "StreamDeferred";
	events: keyof typeof scripts;
	result: () => any;
	reasoning?: string;
	tracingOnly?: boolean;
	noProvider?: boolean;
}

const stop = () => message("stop");
const rows: Row[] = [
	{ name: "stream", entry: "Stream", events: "blocks", result: stop },
	{ name: "streamSimple-reasoning", entry: "StreamSimple", events: "blocks", result: stop, reasoning: "high" },
	{ name: "streamSimple-no-reasoning", entry: "StreamSimple", events: "blocks", result: stop },
	{ name: "streamDeferred", entry: "StreamDeferred", events: "blocks", result: stop },
	{ name: "length", entry: "StreamSimple", events: "blocks", result: () => message("length") },
	{ name: "toolUse", entry: "Stream", events: "blocks", result: () => message("toolUse") },
	{
		// 499 units, then an astral character the 500-unit cap splits.
		name: "error-bounded",
		entry: "StreamSimple",
		events: "blocks",
		result: () => message("error", { errorMessage: `${"x".repeat(499)}😀discarded-tail` }),
	},
	{ name: "error-short", entry: "Stream", events: "blocks", result: () => message("error", { errorMessage: "error-secret" }) },
	{ name: "error-missing-message", entry: "StreamDeferred", events: "blocks", result: () => message("error") },
	{ name: "aborted", entry: "StreamSimple", events: "blocks", result: () => message("aborted", { errorMessage: "error-secret" }) },
	{ name: "poll-pending", entry: "StreamDeferred", events: "blocks", result: () => message("pending") },
	{ name: "poll-deferred", entry: "StreamDeferred", events: "blocks", result: () => message("deferred") },
	{ name: "poll-error", entry: "StreamDeferred", events: "blocks", result: () => message("error", { errorMessage: "poll-secret" }) },
	{ name: "request-deferred", entry: "StreamSimple", events: "blocks", result: () => message("deferred") },
	{ name: "interleaved-tools", entry: "Stream", events: "interleaved", result: stop },
	{ name: "result-without-terminal-event", entry: "Stream", events: "blocks-without-terminal", result: stop },
	{ name: "no-events", entry: "StreamSimple", events: "none", result: stop },
	{ name: "setup-failure-stream", entry: "Stream", events: "blocks", result: stop, noProvider: true },
	{ name: "setup-failure-streamSimple", entry: "StreamSimple", events: "blocks", result: stop, reasoning: "low", noProvider: true },
	{ name: "setup-failure-streamDeferred", entry: "StreamDeferred", events: "blocks", result: stop, noProvider: true },
	{ name: "tracing-only", entry: "StreamSimple", events: "blocks", result: stop, tracingOnly: true },
];

const out: any = { sha, rows: [] };
for (const row of rows) {
	const telemetry = new InMemoryTelemetryContext();
	const factoryCalls: any[] = [];
	spyFactories(telemetry, factoryCalls);
	const result = row.result();
	const models = row.noProvider ? createModels() : modelsReplaying(scripts[row.events](result), result);
	let outcome: any;
	await telemetry.startSpan({ name: "caller" }, async (parent: any) => {
		const span = row.tracingOnly ? { ...parent, metrics: undefined } : parent;
		outcome = await request(models, row.entry, span, row.reasoning);
	});
	await settled(telemetry);
	out.rows.push({
		name: row.name,
		entry: row.entry,
		events: row.events,
		reasoning: row.reasoning ?? null,
		tracingOnly: row.tracingOnly ?? false,
		noProvider: row.noProvider ?? false,
		resultStopReason: row.noProvider ? null : result.stopReason,
		resultErrorMessage: row.noProvider ? null : (result.errorMessage ?? null),
		delivered: outcome.delivered,
		stopReason: outcome.result.stopReason,
		errorMessage: outcome.result.errorMessage ?? null,
		factoryCalls,
		...record(telemetry),
	});
}

// One recorder across three requests: instruments are created once per
// metrics object (pi's WeakMap cache), and every request still measures.
{
	const telemetry = new InMemoryTelemetryContext();
	const factoryCalls: any[] = [];
	spyFactories(telemetry, factoryCalls);
	const result = message("stop");
	const models = modelsReplaying(blockEvents(result), result);
	for (const entry of ["StreamSimple", "StreamDeferred", "Stream"]) {
		await telemetry.startSpan({ name: "caller" }, (parent: any) => request(models, entry, parent));
	}
	await settled(telemetry);
	out.instrumentsOnce = { factoryCalls, ...record(telemetry) };
}

// One row per line keeps the file small and its diffs readable.
const lines = out.rows.map((row: unknown) => `\t\t${JSON.stringify(row)}`).join(",\n");
fs.writeFileSync(
	outFile,
	`{\n\t"sha": ${JSON.stringify(sha)},\n\t"rows": [\n${lines}\n\t],\n\t"instrumentsOnce": ${JSON.stringify(out.instrumentsOnce)}\n}\n`,
);
console.log(`wrote ${out.rows.length} rows to ${outFile}`);
