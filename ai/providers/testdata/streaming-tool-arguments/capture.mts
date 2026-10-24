// Captures the partial tool-call arguments pi's anthropic-messages,
// openai-completions, openai-responses, pi-messages and faux adapters show on
// every toolcall event, and the message each ends with, once every adapter
// feeds the streamed argument deltas to one ToolCallArgumentsStream per call
// (upstream f715a15c1) — the oracle behind TestStreamedToolCallArgumentsLikePi.
//
//   node capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> streaming-tool-arguments-f715a15c1.json f715a15c1
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai
// package-lock.json`, prefix kept) and a node_modules resolving its
// dependencies at the versions package-lock.json locks at <sha>
// (@anthropic-ai/sdk 0.129.0, openai 7.19.0, partial-json 0.1.7 at f715a15c1:
// the npm 1.4.0 build's node_modules carries those), checked below.
//
// The HTTP adapters read `body` through their `fetch` option; openai-responses
// reads `events` through processResponsesStream, the loop its stream() hands
// the SDK's parsed events to. A toolcall event records its type, contentIndex,
// delta and JSON.stringify of the call's arguments in the event's partial,
// taken as the event is pushed: an adapter keeps mutating the partial it
// pushed, so a consumer reading the event later would see later arguments
// (the port clones the partial into each event).
// Bodies: argument texts whose partial values changed with the parser
// (trailing whitespace in an open string, a raw control character, an invalid
// escape, a split \u escape or surrogate pair, NaN) and the stream-state edges
// each adapter's loop changed with it.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const lock = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8")).packages;
for (const [name, key] of [
	["@anthropic-ai/sdk", "node_modules/@anthropic-ai/sdk"],
	["openai", "packages/ai/node_modules/openai"],
	["partial-json", "node_modules/partial-json"],
]) {
	const have = JSON.parse(fs.readFileSync(path.join(extraction, "node_modules", name, "package.json"), "utf8")).version;
	if (have !== lock[key].version) {
		console.error(`${name} ${have} is not the locked ${lock[key].version}`);
		process.exit(1);
	}
}
const src = (file: string) => import(pathToFileURL(path.join(extraction, "packages/ai/src", file)).href);
const { stream: streamAnthropic } = await src("api/anthropic-messages.ts");
const { stream: streamCompletions } = await src("api/openai-completions.ts");
const { processResponsesStream } = await src("api/openai-responses-shared.ts");
const { stream: streamPiMessages } = await src("api/pi-messages.ts");
const { fauxAssistantMessage, fauxProvider, fauxToolCall } = await src("providers/faux.ts");
const { AssistantMessageEventStream } = await src("utils/event-stream.ts");
const { normalizeContext } = await src("utils/transcript.ts");

// The call's arguments as each toolcall event is pushed.
const pushedArguments = new WeakMap<object, string | undefined>();
const push = AssistantMessageEventStream.prototype.push;
AssistantMessageEventStream.prototype.push = function (event: any) {
	if (typeof event?.type === "string" && event.type.startsWith("toolcall_")) {
		const call = event.partial.content[event.contentIndex];
		pushedArguments.set(event, call === undefined || call === null ? undefined : JSON.stringify(call.arguments));
	}
	return push.call(this, event);
};

// Backslashes are written as B so this file carries no escape a tool could rewrite.
const B = "\\";
const baseModel = {
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 200_000,
	maxTokens: 64_000,
};
const context = normalizeContext({ messages: [{ role: "user", content: "write", timestamp: 0 }] });
const fetchOf = (body: string) => async () =>
	new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
const sse = (data: unknown, event?: string) => `${event ? `event: ${event}\n` : ""}data: ${JSON.stringify(data)}\n\n`;

type ToolEvent = { type: string; contentIndex: number; delta?: string; arguments?: string };
async function consume(events: AsyncIterable<any> & { result(): Promise<any> }) {
	const toolEvents: ToolEvent[] = [];
	for await (const event of events) {
		if (!event.type.startsWith("toolcall_")) continue;
		const record: ToolEvent = { type: event.type, contentIndex: event.contentIndex };
		if (event.delta !== undefined) record.delta = event.delta;
		if (!pushedArguments.has(event)) throw new Error(`${event.type} was not pushed through AssistantMessageEventStream.push`);
		const args = pushedArguments.get(event);
		if (args !== undefined) record.arguments = args;
		toolEvents.push(record);
	}
	const message = await events.result();
	return {
		toolEvents,
		stopReason: message.stopReason,
		errorMessage: message.errorMessage ?? null,
		content: JSON.stringify(message.content),
	};
}

const texts: string[][] = [
	['{"code":"x  ', "  ", 'y"}'],
	['{"cmd":"a\tb', ` ${B}d`, 'c"}'],
	['{"a":NaN', ',"b":1}'],
	['{"path":"a.txt",', '"content":"hi"}'],
	[`{"s":"${B}u00`, 'e9"}'],
	["[1,", "2]"],
	[`{"s":"${B}ud83d`, `${B}ude00"}`],
	["  ", '{"a":1}'],
	['{"n":12', '3,"m":tr', "ue}"],
	['{"a":{"b":[1,{"c":"d', '"}]}}'],
	['{"t":"tab', "\t", '"}'],
];

const rows: unknown[] = [];

// anthropic-messages
const anthropicModel = { ...baseModel, id: "claude-test", name: "c", api: "anthropic-messages", provider: "anthropic", baseUrl: "https://api.anthropic.com" };
const anthropicStart = sse({ type: "message_start", message: { id: "msg_1", model: "claude-test", usage: { input_tokens: 1, output_tokens: 1 } } }, "message_start");
const anthropicEnd =
	sse({ type: "message_delta", delta: { stop_reason: "tool_use" }, usage: { output_tokens: 10 } }, "message_delta") +
	sse({ type: "message_stop" }, "message_stop");
const toolUse = (index: number | undefined, id: string, input: unknown = {}) =>
	sse({ type: "content_block_start", ...(index === undefined ? {} : { index }), content_block: { type: "tool_use", id, name: "write", input } }, "content_block_start");
const jsonDelta = (index: number | undefined, partial: unknown) =>
	sse({ type: "content_block_delta", ...(index === undefined ? {} : { index }), delta: { type: "input_json_delta", partial_json: partial } }, "content_block_delta");
const stop = (index: number | undefined) => sse({ type: "content_block_stop", ...(index === undefined ? {} : { index }) }, "content_block_stop");
const anthropicBodies: [string, string][] = texts.map((pieces, i) => [
	`text${i}`,
	anthropicStart + toolUse(0, "toolu_1") + pieces.map((p) => jsonDelta(0, p)).join("") + stop(0) + anthropicEnd,
]);
anthropicBodies.push(
	// The start's input stands until the first delta.
	["start input", anthropicStart + toolUse(0, "toolu_1", { x: 1 }) + jsonDelta(0, " ") + jsonDelta(0, '{"y":2}') + stop(0) + anthropicEnd],
	// A second block started at an open block's index takes that index over.
	["index reused", anthropicStart + toolUse(0, "toolu_1") + jsonDelta(0, '{"a":1') + toolUse(0, "toolu_2") + jsonDelta(0, '{"b":2}') + stop(0) + jsonDelta(0, "}") + stop(0) + anthropicEnd],
	// Events with no index: a stopped block is gone, so a later delta finds nothing.
	["no index", anthropicStart + toolUse(undefined, "toolu_1") + jsonDelta(undefined, '{"a":1}') + stop(undefined) + jsonDelta(undefined, '{"z":1}') + stop(undefined) + anthropicEnd],
	// A delta after the block's stop.
	["after stop", anthropicStart + toolUse(0, "toolu_1") + jsonDelta(0, '{"a":1}') + stop(0) + jsonDelta(0, '{"b":2}') + anthropicEnd],
	// partial_json that is not a string.
	["null delta", anthropicStart + toolUse(0, "toolu_1") + jsonDelta(0, '{"a":"x') + jsonDelta(0, null) + stop(0) + anthropicEnd],
	["missing delta", anthropicStart + toolUse(0, "toolu_1") + jsonDelta(0, undefined) + stop(0) + anthropicEnd],
);
for (const [name, body] of anthropicBodies) {
	const result = await consume(streamAnthropic(anthropicModel, context, { apiKey: "sk-ant-test", fetch: fetchOf(body) }));
	rows.push({ api: "anthropic-messages", name, body, ...result });
}

// openai-completions
const completionsModel = { ...baseModel, id: "gpt-test", name: "g", api: "openai-completions", provider: "openai", baseUrl: "https://api.openai.com/v1" };
const chunkBase = { id: "chat_1", object: "chat.completion.chunk", created: 1, model: "test" };
const call = (fields: object) => sse({ ...chunkBase, choices: [{ index: 0, delta: { tool_calls: [{ index: 0, ...fields }] } }] });
const completionsEnd =
	sse({ ...chunkBase, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }) +
	"data: [DONE]\n\n";
const completionsBodies: [string, string][] = texts.map((pieces, i) => [
	`text${i}`,
	call({ id: "call_1", type: "function", function: { name: "write", arguments: "" } }) + pieces.map((p) => call({ function: { arguments: p } })).join("") + completionsEnd,
]);
completionsBodies.push(
	// A custom (grammar) tool's raw input, re-synthesized into JSON deltas.
	["custom", call({ id: "call_c", type: "custom", custom: { name: "g", input: "ab" } }) + call({ custom: { input: `c"d\n${B}` } }) + call({ custom: { input: "" } }) + completionsEnd],
	// A call that turns custom after function arguments arrived.
	["turns custom", call({ id: "call_c", type: "function", function: { name: "g", arguments: '{"a":' } }) + call({ custom: { input: "zz" } }) + call({ function: { arguments: "1}" } }) + completionsEnd],
	// Function arguments on a custom call, in the same chunk as custom input.
	["both kinds", call({ id: "call_c", type: "custom", custom: { name: "g", input: "a" } }) + call({ function: { arguments: '{"x":1}' }, custom: { input: "b" } }) + completionsEnd],
);
for (const [name, body] of completionsBodies) {
	const result = await consume(streamCompletions(completionsModel, context, { apiKey: "sk-test", fetch: fetchOf(body) }));
	rows.push({ api: "openai-completions", name, body, ...result });
}

// openai-responses
const responsesModel = { ...baseModel, id: "gpt-test", name: "g", api: "openai-responses", provider: "openai", baseUrl: "https://api.openai.com/v1" };
const fnItem = (args: string) => ({ type: "function_call", id: "fc_1", call_id: "call_1", name: "write", arguments: args, status: "in_progress" });
const added = (item: object) => ({ type: "response.output_item.added", output_index: 0, sequence_number: 0, item });
const argsDelta = (delta: string) => ({ type: "response.function_call_arguments.delta", output_index: 0, item_id: "fc_1", sequence_number: 0, delta });
const argsDone = (args: string) => ({ type: "response.function_call_arguments.done", output_index: 0, item_id: "fc_1", sequence_number: 0, arguments: args });
const itemDone = (item: object) => ({ type: "response.output_item.done", output_index: 0, sequence_number: 0, item });
const completed = { type: "response.completed", sequence_number: 0, response: { id: "resp_1", status: "completed", output: [], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } };
const customItem = (input: string | undefined) => ({ type: "custom_tool_call", id: "ct_1", call_id: "call_c", name: "g", ...(input === undefined ? {} : { input }) });
const customDelta = (delta: string) => ({ type: "response.custom_tool_call_input.delta", output_index: 0, item_id: "ct_1", sequence_number: 0, delta });
const customDone = (input: string) => ({ type: "response.custom_tool_call_input.done", output_index: 0, item_id: "ct_1", sequence_number: 0, input });
const responsesRows: [string, object[]][] = texts.map((pieces, i) => [
	`text${i}`,
	[added(fnItem("")), ...pieces.map(argsDelta), argsDone(pieces.join("")), itemDone({ ...fnItem(pieces.join("")), status: "completed" }), completed],
]);
responsesRows.push(
	["initial arguments", [added(fnItem('{"a":')), argsDelta("1"), argsDelta("}"), argsDone('{"a":1}'), itemDone(fnItem('{"a":1}')), completed]],
	["done continues", [added(fnItem("")), argsDelta('{"a":"x'), argsDone('{"a":"xyz"}'), itemDone(fnItem("")), completed]],
	["done restarts", [added(fnItem("")), argsDelta('{"a":"x'), argsDone('{"b":2}'), itemDone(fnItem('{"c":3}')), completed]],
	["item done wins", [added(fnItem("")), argsDelta('{"a":1}'), itemDone(fnItem('{"a":')), completed]],
	["custom initial", [added(customItem("ab")), customDelta("c"), customDone("abc"), itemDone(customItem("abc")), completed]],
	["custom initial no delta", [added(customItem("ab")), customDone("abc"), itemDone(customItem(undefined)), completed]],
	["custom", [added(customItem(undefined)), customDelta(`x"`), customDelta(""), itemDone(customItem(undefined)), completed]],
	["custom changed", [added(customItem("")), customDelta("ab"), customDone("xy"), completed]],
	["custom after close", [added(customItem("")), customDelta("ab"), customDone("ab"), customDelta("c"), completed]],
	["unfinished", [added(fnItem("")), argsDelta('{"a":1}'), { ...completed, response: { ...completed.response, output: [] } }]],
);
for (const [name, events] of responsesRows) {
	const output = {
		role: "assistant",
		content: [],
		api: "openai-responses",
		provider: "openai",
		model: "gpt-test",
		usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
		stopReason: "pending",
		timestamp: 0,
	} as any;
	const stream = new AssistantMessageEventStream();
	void (async () => {
		stream.push({ type: "start", partial: output });
		try {
			await processResponsesStream((async function* () { yield* events; })(), output, stream, responsesModel);
			stream.push({ type: "done", reason: output.stopReason, message: output });
		} catch (error) {
			output.stopReason = "error";
			output.errorMessage = error instanceof Error ? error.message : String(error);
			stream.push({ type: "error", reason: "error", error: output });
		}
		stream.end();
	})();
	const result = await consume(stream);
	rows.push({ api: "openai-responses", name, events, ...result });
}

// pi-messages
const piModel = { ...baseModel, id: "auto", name: "a", api: "pi-messages", provider: "radius", baseUrl: "https://example.test/v1" };
const piDone = sse({
	type: "done",
	reason: "toolUse",
	usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
});
const piEnd = (args: unknown) => sse({ type: "toolcall_end", contentIndex: 0, toolCall: { type: "toolCall", id: "call_1", name: "write", arguments: args } });
const piBodies: [string, string][] = texts.map((pieces, i) => [
	`text${i}`,
	sse({ type: "start" }) + sse({ type: "toolcall_start", contentIndex: 0, id: "call_1", toolName: "write" }) + pieces.map((delta) => sse({ type: "toolcall_delta", contentIndex: 0, delta })).join("") + piEnd({ ok: true }) + piDone,
]);
piBodies.push(
	// A delta for a call that never started.
	["no start", sse({ type: "start" }) + sse({ type: "toolcall_start", contentIndex: 0, id: "call_1", toolName: "write" }) + sse({ type: "toolcall_delta", contentIndex: 1, delta: '{"a":1}' }) + sse({ type: "toolcall_delta", contentIndex: 0, delta: '{"b":2' }) + piEnd({ b: 2 }) + piDone],
	// A delta after the call ended.
	["after end", sse({ type: "start" }) + sse({ type: "toolcall_start", contentIndex: 0, id: "call_1", toolName: "write" }) + sse({ type: "toolcall_delta", contentIndex: 0, delta: '{"b":2' }) + piEnd({ b: 2 }) + sse({ type: "toolcall_delta", contentIndex: 0, delta: "}" }) + piDone],
);
for (const [name, body] of piBodies) {
	const result = await consume(streamPiMessages(piModel, context, { apiKey: "test", fetch: fetchOf(body) }));
	rows.push({ api: "pi-messages", name, body, ...result });
}

// faux: four-character chunks of JSON.stringify(arguments).
for (const [name, args] of [
	["write", { path: "a.txt", content: "hello world  ", n: [1, 2.5, -3e-7], ok: true, none: null }],
	["nested", { a: { b: [{ c: "d\n" }] }, "2": "two", "1": "one" }],
	["empty", {}],
] as const) {
	const faux = fauxProvider({ tokenSize: { min: 1, max: 1 } });
	faux.setResponses([fauxAssistantMessage([fauxToolCall("write", args as never, { id: "call_1" })], { stopReason: "toolUse" })]);
	const result = await consume(faux.provider.streamSimple(faux.getModel(), context, {}));
	rows.push({ api: "faux", name, arguments: JSON.stringify(args), ...result });
}

fs.writeFileSync(
	outFile,
	`${JSON.stringify({ source: `pi ${sha} packages/ai/src (src capture), node ${process.version}`, rows }, null, "\t")}\n`,
);
console.log(`wrote ${outFile}: ${rows.length} rows`);
