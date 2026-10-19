// Captures pi's fallback when Anthropic rejects strict tools for its own
// limits (upstream d4d53612b): a 400 invalid_request_error whose message says
// the compiled grammar is too large, or there are too many strict tools, or too
// many union-typed parameters, is retried once with every strict:"prefer" tool
// sent with its original non-strict schema (strict:"require" stays strict), the
// params rebuilt and handed to onPayload again, and the reply carries an
// anthropic_strict_tools_fallback diagnostic. It is the oracle behind
// TestAnthropicStrictToolFallbackMatchesPi.
//
//   node capture.mjs <pi-ai package dir | src extraction> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai strict-fallback-1.3.0.json
//
// The rows replay upstream's anthropic-strict-tool-fallback.test.ts: its model,
// tools and responses (served through options.fetch, one per request, the last
// repeating), with maxRetries 0. Each row records what it ran with, every
// request body, how often onPayload and onResponse ran, the stream's event
// types, and the reply's stopReason, errorMessage and diagnostics (timestamps
// written as 0).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [dir, outFile] = process.argv.slice(2);
if (!dir || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir | src extraction> <out.json>");
	process.exit(2);
}
const dist = fs.existsSync(path.join(dir, "dist/api/anthropic-messages.js"));
const load = (file) =>
	import(pathToFileURL(dist ? path.join(dir, "dist", `${file}.js`) : path.join(dir, "packages/ai/src", `${file}.ts`)).href);
const anthropic = await load("api/anthropic-messages");
const { normalizeContext } = await load("utils/transcript");
const source = dist
	? `pi-ai ${JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version}`
	: `src ${path.basename(dir)}`;

const grammarError =
	"The compiled grammar is too large, which would cause performance issues. Simplify your tool schemas or reduce the number of strict tools.";
const tooManyStrict =
	"Too many strict tools (32). The maximum number of strict tools supported is 20. Try reducing the number of tools marked as strict.";
const tooManyUnions =
	"Schemas contains too many parameters with union types (480 parameters with type arrays or anyOf). This causes exponential compilation cost. Reduce the number of nullable or union-typed parameters (limit: 16 parameters with unions).";
const model = {
	id: "claude-haiku-4-5",
	name: "Test Claude",
	api: "anthropic-messages",
	provider: "anthropic",
	baseUrl: "https://anthropic.example",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 200000,
	maxTokens: 100,
	compat: { supportsStrictTools: true },
};
const nativeModel = {
	...model,
	compat: { ...model.compat, supportsMidConvoSystemMessages: true, supportsMidConvoToolChanges: true },
};
const lookupSchema = {
	type: "object",
	properties: { value: { type: "string" }, optional: { type: "number" } },
	required: ["value"],
};
const tool = (name, strict, parameters = lookupSchema) => ({
	name,
	description: "Look up a value",
	parameters,
	...(strict ? { constrainedSampling: { type: "json_schema", strict } } : {}),
});
const user = { role: "user", content: "Hello", timestamp: 1 };
const errorResponse = (message = grammarError, status = 400, type = "invalid_request_error") => ({
	status,
	contentType: "application/json",
	body: JSON.stringify({ type: "error", error: { type, message } }),
});
const success = {
	status: 200,
	contentType: "text/event-stream",
	body: [
		{ type: "message_start", message: { id: "msg_test", model: model.id, usage: { input_tokens: 10, output_tokens: 0 } } },
		{ type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 1 } },
		{ type: "message_stop" },
	]
		.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`)
		.join(""),
};
const sseError = {
	status: 200,
	contentType: "text/event-stream",
	body: `event: error\ndata: ${JSON.stringify({ type: "error", error: { type: "invalid_request_error", message: grammarError } })}\n\n`,
};

const rows = [];
// opts: apiKey, cacheRetention, onPayload ("inferenceGeo": spread the params,
// add inference_geo "us" and stream false), abortOnRequest (abort the signal
// inside fetch on that request), streams (run the row's stream that many times
// in turn).
async function row(name, m, messages, responses, opts = {}) {
	const payloads = [];
	const controller = new AbortController();
	const fetch = async (_input, init) => {
		payloads.push(JSON.parse(String(init?.body)));
		if (opts.abortOnRequest === payloads.length) controller.abort();
		const r = responses[Math.min(payloads.length, responses.length) - 1];
		return new Response(r.body, { status: r.status, headers: { "content-type": r.contentType } });
	};
	let onPayloadCalls = 0;
	let onResponseCalls = 0;
	const options = {
		apiKey: opts.apiKey ?? "test-key",
		fetch,
		maxRetries: 0,
		signal: controller.signal,
		onResponse: () => {
			onResponseCalls++;
		},
		onPayload: (payload) => {
			onPayloadCalls++;
			return opts.onPayload === "inferenceGeo" ? { ...payload, inference_geo: "us", stream: false } : undefined;
		},
		...(opts.cacheRetention ? { cacheRetention: opts.cacheRetention } : {}),
	};
	const results = [];
	for (let i = 0; i < (opts.streams ?? 1); i++) {
		const s = anthropic.stream(m, normalizeContext({ messages }), options);
		const events = [];
		for await (const event of s) events.push(event.type);
		const message = await s.result();
		results.push({
			events,
			stopReason: message.stopReason,
			errorMessage: message.errorMessage ?? null,
			diagnostics: message.diagnostics?.map((d) => ({ ...d, timestamp: 0 })) ?? null,
		});
	}
	rows.push({ name, model: m, messages, responses, options: opts, payloads, onPayloadCalls, onResponseCalls, results });
}

const system = (tools) => ({ role: "system", content: "", toolsAdded: tools, timestamp: 0 });
for (const [label, message] of [
	["compiled grammar", grammarError],
	["too many strict tools", tooManyStrict],
	["too many unions", tooManyUnions],
]) {
	await row(
		`retries ${label} with prefer non-strict`,
		model,
		[system([tool("preferred", "prefer"), tool("required", "require"), tool("ordinary")]), user],
		[errorResponse(message), success],
		{ onPayload: "inferenceGeo", cacheRetention: "long" },
	);
}
await row(
	"downgrades inline additions and redefinitions",
	nativeModel,
	[
		system([tool("lookup", "require")]),
		user,
		{ role: "system", content: "", toolsAdded: [tool("lookup", "prefer")], timestamp: 2 },
		{ role: "system", content: "", toolsAdded: [tool("lookup", "require")], timestamp: 3 },
	],
	[errorResponse(), success],
	{ apiKey: "sk-ant-oat-test" },
);
for (const [label, tools] of [
	["required", [tool("lookup", "require")]],
	["ordinary", [tool("lookup")]],
	["locally unsupported prefer", [tool("lookup", "prefer", { type: "object", properties: { value: { type: "number", minimum: 1 } }, required: ["value"] })]],
]) {
	await row(`no retry for a ${label} tool set`, model, [system(tools), user], [errorResponse()]);
}
for (const [label, message, status, type] of [
	["an unrelated 400", "Invalid tool schema", 400, "invalid_request_error"],
	["the wrong error type", grammarError, 400, "authentication_error"],
	["the wrong HTTP status", grammarError, 500, "invalid_request_error"],
]) {
	await row(`no downgrade on ${label}`, model, [system([tool("lookup", "prefer")]), user], [errorResponse(message, status, type)]);
}
await row(
	"a second rejection surfaces",
	model,
	[system([tool("lookup", "prefer"), tool("required", "require")]), user],
	[errorResponse(), errorResponse("Required grammar is still too large")],
);
await row("cancellation before the fallback attempt", model, [system([tool("lookup", "prefer")]), user], [errorResponse()], {
	abortOnRequest: 1,
});
await row("no retry of an SSE error after the headers", model, [system([tool("lookup", "prefer")]), user], [sseError]);
await row("later requests stay strict", model, [system([tool("lookup", "prefer")]), user], [errorResponse(), success, success], {
	streams: 2,
});

fs.writeFileSync(outFile, `${JSON.stringify({ source, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
