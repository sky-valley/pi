// Captures what real pi does with openai-responses models that accept
// mid-conversation system messages — the oracle behind the capture tests in
// openai_responses_midconvo_test.go.
//
//   node --experimental-strip-types capture-responses-midconvo.mts <extraction> <out.json> <sha>
//   e.g. ... capture-responses-midconvo.mts <dir> responses-midconvo-530015d93.json 530015d93
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies (the npm
// build's). The npm build 0.85.1 predates upstream 9e05370b2, so these are src
// captures: re-verify them against the first build that ships it (the BUILD
// wins).
//
// Re-captured at caa6967b6 (2026-10-20) with the 1.3.0 build's node_modules
// and packages/telemetry/src at the sha as @earendil-works/pi-telemetry (the
// 1.3.0 build's copy lacks exports pi-ai src now imports). Control: this
// script as of 9e05370b2's cases, run on src at the parent 73f8de80b, wrote
// responses-midconvo-9e05370b2.json byte for byte. caa6967b6 declares every
// tool of an additional_tools model in additional_tools items and announces
// removals as text, and it added the redefinition, redeclaration, history and
// unled additional_tools cases below.
//
// Re-captured at 530015d93 (2026-10-20), same setup. Control: this script as
// of caa6967b6's cases, run on src at caa6967b6, wrote
// responses-midconvo-caa6967b6.json byte for byte. 530015d93 lists as still
// unavailable only the removals earlier messages announced (removal-fallback
// moved), and added "additional-tools-same-message-add-and-remove".
// "additional-tools-multi-removal" was added at 530015d93 after review, to pin
// the JS Set order of the "Still unavailable" list (control: the script
// without it rewrote the golden byte for byte first).
//
// Each "bodies" case records its model, its context and the body pi hands
// onPayload, so the Go test decodes the very inputs pi saw. compat.ts
// streamSimple is normalizeContext followed by the api's streamSimple, which is
// called directly here (compat.ts also loads generated catalog data a source
// extraction does not carry). The first three cases are the openai-responses
// cases of packages/ai/test/transcript-tool-changes.test.ts, same models and
// fixtures; the rest pin what those cases leave loose: the tool_search call_id
// seed (msgIndex, which the leading system message does not advance and a held
// system message takes at its transformed position), rendered section updates,
// the instruction role (which additional_tools items never take),
// tool_search_output's defer_loading on custom and strict tools,
// additional_tools winning over tool search, later system messages that add no
// tools, a transcript without a leading system message, and one whose leading
// system message follows a dropped aborted assistant message. The
// "additional-tools-redefinition", "-redeclared-after-removal",
// "-same-message-add-and-remove" and the two
// "*-history-<step>" series are the suite's later cases, one body per step.
//
// "loneSurrogates" is a body case whose system text carries lone UTF-16
// surrogates; encoding/json cannot decode those, so its Go test builds the
// context itself. "streams" feeds pi a recorded SSE response through the fetch
// option and records the tool calls it parses: the stream reads grammar tool
// input properties from the transcript it resolved.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-responses-midconvo.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { normalizeContext } = await load("utils/transcript.ts");
const responses = await load("api/openai-responses.ts");
const { Type } = await import(pathToFileURL(path.join(extraction, "node_modules/typebox/build/index.mjs")).href);

function tool(name: string) {
	return { name, description: `${name} tool`, parameters: Type.Object({}) };
}

// transcript-tool-changes.test.ts `modelBase`, `context` and `additionContext`.
const modelBase = {
	baseUrl: "http://127.0.0.1:9",
	reasoning: true,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 100000,
	maxTokens: 1000,
};
const baseTool = tool("base_tool");
const lateTool = tool("late_tool");
const context = {
	messages: [
		{
			role: "system",
			content: "base prompt",
			sections: { rules: "<rules>\nold rules\n</rules>", docs: "<docs>\nread docs\n</docs>" },
			toolsAdded: [baseTool],
			timestamp: 0,
		},
		{ role: "user", content: "before", timestamp: 1 },
		{
			role: "system",
			content: "updated guidance",
			sections: { rules: "<rules>\nnew rules\n</rules>", docs: null },
			toolsRemoved: [{ name: "base_tool" }],
			toolsAdded: [lateTool],
			timestamp: 2,
		},
	],
};
const additionContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		{ role: "system", content: "updated guidance", toolsAdded: [lateTool], timestamp: 2 },
	],
};

function gpt54(compat: Record<string, unknown>) {
	return { ...modelBase, id: "gpt-5.4", name: "GPT-5.4", api: "openai-responses", provider: "openai", compat };
}

const grammarTool = {
	name: "grammar_tool",
	description: "grammar_tool tool",
	parameters: Type.Object({ query: Type.String() }),
	constrainedSampling: { type: "grammar", variants: { openai_lark: "start: /.+/" } },
};
const strictTool = {
	name: "strict_tool",
	description: "strict_tool tool",
	parameters: Type.Object({ path: Type.String() }),
	constrainedSampling: { type: "json_schema", strict: "require" },
};
const assistant = (content: unknown[], stopReason: string, timestamp: number) => ({
	role: "assistant",
	content,
	api: "openai-responses",
	provider: "openai",
	model: "gpt-5.4",
	usage: {
		input: 0,
		output: 0,
		cacheRead: 0,
		cacheWrite: 0,
		totalTokens: 0,
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
	},
	stopReason,
	timestamp,
});

// A later system message with rendered section changes and two tools, a tool
// call to the grammar tool it declared, a tools-only system message landing
// while that call is pending (transformMessages holds it until the next
// assistant message), and fallback message ids on both sides of each.
const seededContext = {
	messages: [
		{
			role: "system",
			content: "",
			sections: { rules: "<rules>\nr1\n</rules>" },
			toolsAdded: [baseTool],
			timestamp: 0,
		},
		{ role: "user", content: "before", timestamp: 1 },
		assistant(
			[
				{ type: "text", text: "first" },
				{ type: "text", text: "second" },
			],
			"stop",
			2,
		),
		{
			role: "system",
			content: "use grammar_tool",
			sections: { rules: "<rules>\nr2\n</rules>", docs: null },
			toolsAdded: [grammarTool, strictTool],
			timestamp: 3,
		},
		assistant(
			[{ type: "toolCall", id: "call_g|ctc_g", name: "grammar_tool", arguments: { query: "abc" } }],
			"toolUse",
			4,
		),
		{ role: "system", content: "", toolsAdded: [lateTool], timestamp: 5 },
		{
			role: "toolResult",
			toolCallId: "call_g|ctc_g",
			toolName: "grammar_tool",
			content: [{ type: "text", text: "done" }],
			isError: false,
			timestamp: 6,
		},
		assistant([{ type: "text", text: "third" }], "stop", 7),
		{ role: "user", content: "next", timestamp: 8 },
	],
};

// No leading system message: nothing goes in body.tools, and the first system
// message is still a later one.
const unledContext = {
	messages: [
		{ role: "user", content: "before", timestamp: 1 },
		{ role: "system", content: "", sections: { rules: "<rules>\nr1\n</rules>" }, toolsAdded: [lateTool], timestamp: 2 },
		{ role: "user", content: "after", timestamp: 3 },
	],
};

// A grammar tool removed after the model called it: its replayed call stays a
// custom tool call, because grammar input properties come from every tool the
// transcript declared, not only the current ones.
const removedGrammarContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [grammarTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		assistant(
			[{ type: "toolCall", id: "call_g|ctc_g", name: "grammar_tool", arguments: { query: "abc" } }],
			"toolUse",
			2,
		),
		{
			role: "toolResult",
			toolCallId: "call_g|ctc_g",
			toolName: "grammar_tool",
			content: [{ type: "text", text: "done" }],
			isError: false,
			timestamp: 3,
		},
		{ role: "system", content: "", toolsRemoved: [{ name: "grammar_tool" }], toolsAdded: [lateTool], timestamp: 4 },
		{ role: "user", content: "after", timestamp: 5 },
	],
};

// Later system messages that add no tools, on a model that anchors additions:
// neither gets an additional_tools item or a tool_search pair, and the later
// one that does add a tool keeps its msgIndex seed.
const toolsFreeUpdateContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		assistant([{ type: "text", text: "first" }], "stop", 2),
		{ role: "system", content: "no new tools", timestamp: 3 },
		{ role: "user", content: "middle", timestamp: 4 },
		{ role: "system", content: "", sections: { rules: "<rules>\nr1\n</rules>" }, timestamp: 5 },
		{ role: "system", content: "", toolsAdded: [lateTool], timestamp: 6 },
		{ role: "user", content: "after", timestamp: 7 },
	],
};

// transformMessages drops an aborted assistant message, so the system message
// after it is the first transformed message and therefore the leading one: its
// complete text is sent, its tools anchor nothing, and it does not advance
// msgIndex — although resolveTranscriptTools, reading the untransformed
// messages, finds no leading system message and sends no body tools.
const abortedFirstContext = {
	messages: [
		assistant([{ type: "text", text: "partial" }], "aborted", 0),
		{
			role: "system",
			content: "guidance",
			sections: { rules: "<rules>\nr1\n</rules>" },
			toolsAdded: [baseTool],
			timestamp: 1,
		},
		{ role: "user", content: "before", timestamp: 2 },
		assistant([{ type: "text", text: "reply" }], "stop", 3),
		{ role: "system", content: "more guidance", toolsAdded: [lateTool], timestamp: 4 },
		{ role: "user", content: "after", timestamp: 5 },
	],
};

// Lone surrogates in leading and later system text and in a section value are
// removed (sanitizeSurrogates); the paired one survives.
const loneSurrogateContext = {
	messages: [
		{ role: "system", content: "base \uD83D prompt 🙈", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		{
			role: "system",
			content: "updated \uDE48 guidance",
			sections: { rules: "<rules>\nr\uD83D\n</rules>" },
			timestamp: 2,
		},
	],
};

// A grammar tool declared and then removed, and a model that calls it anyway.
// The stream parses the call with the grammar input property of every tool the
// resolved transcript declared: a native model keeps the declaring system
// message ({query}), a collapsed transcript declares only the current tools
// ({input}).
const removedGrammarStreamContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [grammarTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		{ role: "system", content: "", toolsRemoved: [{ name: "grammar_tool" }], toolsAdded: [lateTool], timestamp: 2 },
		{ role: "user", content: "after", timestamp: 3 },
	],
};
const grammarCallSSE = [
	`data: {"type":"response.created","response":{"id":"resp_1"}}`,
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"grammar_tool","input":""}}`,
	`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"abc"}`,
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"grammar_tool","input":"abc"}}`,
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	`data: [DONE]`,
	``,
].join("\n\n");

const dropTool = tool("drop_tool");

// transcript-tool-changes.test.ts `captureToolHistory`: the transcript after
// each step (initial tools, removal of drop_tool, then two loads). Each step's
// transcript extends the previous one, so the bodies show whether the request
// prefix survives the removal and the loads after it.
function toolHistory(): unknown[] {
	const steps = [
		[{ role: "user", content: "first", timestamp: 1 }],
		[
			{ role: "system", content: "", toolsRemoved: [{ name: "drop_tool" }], timestamp: 2 },
			{ role: "user", content: "after removal", timestamp: 3 },
		],
		[
			{ role: "system", content: "", toolsAdded: [lateTool], timestamp: 4 },
			{ role: "user", content: "after first load", timestamp: 5 },
		],
		[
			{ role: "system", content: "", toolsAdded: [tool("later_tool")], timestamp: 6 },
			{ role: "user", content: "after second load", timestamp: 7 },
		],
	];
	const messages: unknown[] = [{ role: "system", content: "base prompt", toolsAdded: [baseTool, dropTool], timestamp: 0 }];
	return steps.map((step) => {
		messages.push(...step);
		return { messages: [...messages] };
	});
}

// 'redefines an OpenAI tool with additional_tools instead of announcing a removal'.
const redefinitionContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		{
			role: "system",
			content: "",
			toolsRemoved: [{ name: "base_tool" }],
			toolsAdded: [{ ...baseTool, description: "changed" }],
			timestamp: 2,
		},
	],
};

// 'stops listing a tool as unavailable once it is declared again'.
const redeclaredAfterRemovalContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool, dropTool], timestamp: 0 },
		{ role: "system", content: "", toolsRemoved: [{ name: "drop_tool" }], timestamp: 1 },
		{ role: "system", content: "", toolsAdded: [dropTool], timestamp: 2 },
		{ role: "user", content: "go", timestamp: 3 },
	],
};

// 'does not list a tool as still unavailable before its removal is announced'.
const sameMessageAddAndRemoveContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool, dropTool], timestamp: 0 },
		{
			role: "system",
			content: "",
			toolsAdded: [lateTool],
			toolsRemoved: [{ name: "drop_tool" }],
			timestamp: 1,
		},
		{ role: "user", content: "go", timestamp: 2 },
	],
};

// Several removals across messages, in an order that is not sorted: the
// incremental tools notice lists the still-unavailable tools in JS Set
// insertion order — a re-added tool leaves the set, its re-removal appends it
// again, a duplicate removal in one message adds once, a removal of a name
// never declared (nope) is listed too, and a redefinition (c, removed and
// added in one message) is not a removal. The notices read "c, a", "c",
// "c, b", "b, a, nope" and "b, a, nope, x".
const multiRemovalContext = (() => {
	const [A, B, C, D, X, Y, Z] = ["a", "b", "c", "d", "x", "y", "z"].map((name) => tool(name));
	const u = (content: string, timestamp: number) => ({ role: "user", content, timestamp });
	return {
		messages: [
			{ role: "system", content: "base", toolsAdded: [A, B, C, D], timestamp: 0 },
			u("1", 1),
			{
				role: "system",
				content: "rm c a",
				sections: { rules: "<rules>\nr\n</rules>" },
				toolsRemoved: [{ name: "c" }, { name: "a" }],
				timestamp: 2,
			},
			u("2", 3),
			{ role: "system", content: "", toolsAdded: [X], timestamp: 4 },
			u("3", 5),
			{ role: "system", content: "", toolsRemoved: [{ name: "b" }], toolsAdded: [A], timestamp: 6 },
			u("4", 7),
			{ role: "system", content: "", toolsAdded: [Y], timestamp: 8 },
			{ role: "system", content: "", toolsRemoved: [{ name: "a" }, { name: "a" }, { name: "nope" }], timestamp: 9 },
			u("5", 10),
			{
				role: "system",
				content: "z",
				toolsAdded: [Z, { ...C, description: "c2" }],
				toolsRemoved: [{ name: "c" }, { name: "x" }],
				timestamp: 11,
			},
			u("6", 12),
			{ role: "system", content: "", toolsAdded: [D], timestamp: 13 },
			u("7", 14),
		],
	};
})();

const cases: Record<string, { model: unknown; context: unknown }> = {
	// 'declares every OpenAI tool in additional_tools at its developer message'
	// (before upstream caa6967b6: 'anchors OpenAI additions at their developer message')
	"anchors-additional-tools": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context: additionContext,
	},
	// 'maps system-message additions into synthetic tool search'
	"tool-search": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsToolSearch: true }),
		context: additionContext,
	},
	// 'announces OpenAI removals as text' (before upstream caa6967b6: 'falls back
	// to the complete current tool state when removals are unsupported')
	"removal-fallback": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context,
	},
	"without-tool-additions": {
		model: gpt54({ supportsMidConvoSystemMessages: true }),
		context: additionContext,
	},
	"additional-tools-over-tool-search": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true, supportsToolSearch: true }),
		context: additionContext,
	},
	"system-instruction-role": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsDeveloperRole: false }),
		context,
	},
	"tool-search-seeds": {
		model: gpt54({
			supportsMidConvoSystemMessages: true,
			supportsToolSearch: true,
			supportsStrictMode: true,
			supportsOpenAIGrammarTools: true,
		}),
		context: seededContext,
	},
	"additional-tools-seeds": {
		model: gpt54({
			supportsMidConvoSystemMessages: true,
			supportsAdditionalTools: true,
			supportsStrictMode: true,
			supportsOpenAIGrammarTools: true,
		}),
		context: seededContext,
	},
	"without-leading-system-message": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsToolSearch: true }),
		context: unledContext,
	},
	"removed-grammar-tool-replay": {
		model: gpt54({
			supportsMidConvoSystemMessages: true,
			supportsAdditionalTools: true,
			supportsOpenAIGrammarTools: true,
		}),
		context: removedGrammarContext,
	},
	// The instruction role is system here; additional_tools stays developer.
	"additional-tools-tools-free-updates": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true, supportsDeveloperRole: false }),
		context: toolsFreeUpdateContext,
	},
	"tool-search-tools-free-updates": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsToolSearch: true }),
		context: toolsFreeUpdateContext,
	},
	"aborted-first-assistant": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsToolSearch: true }),
		context: abortedFirstContext,
	},
	// 'redefines an OpenAI tool with additional_tools instead of announcing a removal'
	"additional-tools-redefinition": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context: redefinitionContext,
	},
	// 'stops listing a tool as unavailable once it is declared again'
	"additional-tools-redeclared-after-removal": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context: redeclaredAfterRemovalContext,
	},
	// 'does not list a tool as still unavailable before its removal is announced'
	"additional-tools-same-message-add-and-remove": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context: sameMessageAddAndRemoveContext,
	},
	"additional-tools-multi-removal": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context: multiRemovalContext,
	},
	// No leading system message: the first additional_tools item is the later
	// system message's, so no incremental notice precedes it.
	"additional-tools-without-leading-system-message": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context: unledContext,
	},
};
// 'keeps the OpenAI request prefix across a removal and later loads' and
// 'keeps the OpenAI tool search prefix across loads after a removal': one body
// per step of toolHistory.
toolHistory().forEach((ctx, step) => {
	cases[`additional-tools-history-${step}`] = {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }),
		context: ctx,
	};
	cases[`tool-search-history-${step}`] = {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsToolSearch: true }),
		context: ctx,
	};
});

const streamCases: Record<string, { model: unknown; context: unknown }> = {
	"removed-grammar-tool-native": {
		model: gpt54({ supportsMidConvoSystemMessages: true, supportsOpenAIGrammarTools: true }),
		context: removedGrammarStreamContext,
	},
	"removed-grammar-tool-collapsed": {
		model: gpt54({ supportsOpenAIGrammarTools: true }),
		context: removedGrammarStreamContext,
	},
};

async function capturePayload(model: any, ctx: unknown): Promise<unknown> {
	let captured: unknown;
	const stream = responses.streamSimple(model, normalizeContext(ctx), {
		apiKey: "test-key",
		onPayload: (payload: unknown) => {
			captured = payload;
			throw new Error("payload captured");
		},
	});
	const final = await stream.result();
	if (captured === undefined) throw new Error(`no payload: ${final.errorMessage}`);
	return captured;
}

// captureToolCalls streams sse through pi's fetch option and returns what the
// stream parsed; any stream error fails the capture.
async function captureToolCalls(model: any, ctx: unknown, sse: string): Promise<unknown> {
	const final = await responses
		.streamSimple(model, normalizeContext(ctx), {
			apiKey: "test-key",
			fetch: async () => new Response(sse, { status: 200, headers: { "content-type": "text/event-stream" } }),
		})
		.result();
	if (final.stopReason === "error" || final.stopReason === "aborted") {
		throw new Error(`stream ended ${final.stopReason}: ${final.errorMessage}`);
	}
	const toolCalls = final.content
		.filter((block: any) => block.type === "toolCall")
		.map((block: any) => ({ name: block.name, arguments: block.arguments }));
	return { stopReason: final.stopReason, toolCalls };
}

// The Go tests decode the recorded JSON, which drops typebox's symbol keys; pi
// must behave the same on that JSON as on the fixture itself.
async function recordBody(name: string, model: unknown, ctx: unknown): Promise<unknown> {
	const body = await capturePayload(model, ctx);
	const recordedModel = JSON.parse(JSON.stringify(model));
	const recordedContext = JSON.parse(JSON.stringify(ctx));
	const recordedBody = await capturePayload(recordedModel, recordedContext);
	if (JSON.stringify(recordedBody) !== JSON.stringify(body)) {
		throw new Error(`${name}: the JSON-decoded fixture builds a different body`);
	}
	return { model: recordedModel, context: recordedContext, body };
}

const bodies: Record<string, unknown> = {};
for (const [name, { model, context: ctx }] of Object.entries(cases)) {
	bodies[name] = await recordBody(name, model, ctx);
}
const loneSurrogates = await recordBody(
	"loneSurrogates",
	gpt54({ supportsMidConvoSystemMessages: true }),
	loneSurrogateContext,
);
const streams: Record<string, unknown> = {};
for (const [name, { model, context: ctx }] of Object.entries(streamCases)) {
	const parsed = await captureToolCalls(model, ctx, grammarCallSSE);
	const recordedModel = JSON.parse(JSON.stringify(model));
	const recordedContext = JSON.parse(JSON.stringify(ctx));
	const recordedParsed = await captureToolCalls(recordedModel, recordedContext, grammarCallSSE);
	if (JSON.stringify(recordedParsed) !== JSON.stringify(parsed)) {
		throw new Error(`${name}: the JSON-decoded fixture parses a different stream`);
	}
	streams[name] = { model: recordedModel, context: recordedContext, sse: grammarCallSSE, ...(parsed as object) };
}
fs.writeFileSync(outFile, `${JSON.stringify({ sha, bodies, loneSurrogates, streams }, null, "\t")}\n`);
