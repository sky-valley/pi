// Captures the request bodies real pi builds on openai-completions for models
// that take mid-conversation system messages in place — the oracle behind
// transcript_completions_native_test.go in this package.
//
//   node --experimental-strip-types capture-completions-native.mts <extraction> <out.json> <sha>
//   e.g. ... capture-completions-native.mts <dir> completions-native-caa6967b6.json caa6967b6
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies (the npm
// build's). The npm build 0.85.1 predates upstream 9e05370b2, so these are src
// captures: re-verify them against the first build that ships it (the BUILD
// wins).
//
// Re-captured at 95fbc0499 with the 0.87.0 build's node_modules on 2026-09-22,
// when 890f92088 flipped openai-completions' detected supportsStrictMode to
// false: the only movement was `"strict":false` leaving every openai-completions
// tool, since none of these models sets the key. 0.87.0 ships that runtime.
//
// Re-captured at caa6967b6 (2026-10-20) with the 1.3.0 build's node_modules
// and packages/telemetry/src at the sha as @earendil-works/pi-telemetry.
// Control: this script as of 95fbc0499's cases, run on src at the parent
// 73f8de80b, wrote completions-native-95fbc0499.json byte for byte, and so did
// it on caa6967b6: every earlier case is unchanged. caa6967b6 moves the Kimi
// top-level tools to the state after the last removal or redeclaration and
// anchors the loads after it; the kimi-k3-redeclared-after-removal,
// kimi-k3-aborted-first-assistant and kimi-k3-history-<step> cases pin that.
//
// Each case records its model and its raw context next to the body. The two
// Kimi cases are packages/ai/test/transcript-tool-changes.test.ts's
// ('anchors Kimi additions in tool-bearing system messages', 'keeps Kimi K2
// system text inline without dynamic tool messages'), same models and
// `additionContext`. The rest pin what those cases leave open: the instruction
// role and update framing of later messages, the Kimi tools message with empty
// update text and no leading message, lastRole after a system message between
// tool results and a user message, and grammar tool calls replayed against the
// declared (not only the current) tools. The cases after those were added once
// an independent verification found rules the first set left unpinned: the
// index that makes a system message "later", the Kimi tools message's role,
// conversion and place in the cache-control scan, redeclarations, the additions
// flag on its own, whitespace-only update text, and a streamed custom tool call
// parsed against the declared tools. compat.ts streamSimple is normalizeContext
// followed by the api's streamSimple, which is called directly here; bodies come
// from onPayload, and the streamed case's response from a fetch that answers
// with its recorded SSE.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-completions-native.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { normalizeContext } = await load("utils/transcript.ts");
const completions = await load("api/openai-completions.ts");
const { Type } = await import(pathToFileURL(path.join(extraction, "node_modules/typebox/build/index.mjs")).href);

function tool(name: string, description = `${name} tool`) {
	return { name, description, parameters: Type.Object({}) };
}

// transcript-tool-changes.test.ts `modelBase`.
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
const dropTool = tool("drop_tool");

// transcript-tool-changes.test.ts `captureToolHistory`: the transcript after
// each step (initial tools, removal of drop_tool, then two loads).
function toolHistory(): any[] {
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
	const messages: any[] = [{ role: "system", content: "base prompt", toolsAdded: [baseTool, dropTool], timestamp: 0 }];
	return steps.map((step) => {
		messages.push(...step);
		return { messages: [...messages] };
	});
}

// The suite's `context` (sections, a removal and an addition) and
// `additionContext` (purely additive).
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

const customModel = { ...modelBase, id: "custom-model", name: "Custom model", api: "openai-completions", provider: "custom-provider", reasoning: false };
const midConvo = { supportsMidConvoSystemMessages: true, supportsMidConvoToolAdditions: true };
const kimiK3 = { ...modelBase, id: "kimi-k3", name: "Kimi K3", api: "openai-completions", provider: "moonshotai", compat: midConvo };
const gptTest = { ...modelBase, id: "gpt-test", name: "GPT test", api: "openai-completions", provider: "openai", compat: midConvo };

function assistantToolCall(id: string, name: string, args: object, timestamp: number) {
	return {
		role: "assistant",
		content: [{ type: "toolCall", id, name, arguments: args }],
		api: "openai-completions",
		provider: "custom-provider",
		model: "custom-model",
		usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
		stopReason: "toolUse",
		timestamp,
	};
}
function toolResult(toolCallId: string, toolName: string, text: string, timestamp: number) {
	return { role: "toolResult", toolCallId, toolName, content: [{ type: "text", text }], isError: false, timestamp };
}

// A tool result, then `between` (none, or system messages), then a user message.
function toolResultThen(...between: object[]) {
	return {
		messages: [
			{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
			{ role: "user", content: "before", timestamp: 1 },
			assistantToolCall("call_1", "base_tool", {}, 2),
			toolResult("call_1", "base_tool", "done", 3),
			...between,
			{ role: "user", content: "after", timestamp: 5 },
		],
	};
}

const gram = {
	name: "gram",
	description: "gram tool",
	parameters: Type.Object({ query: Type.String() }),
	constrainedSampling: { type: "grammar", variants: { openai_lark: "start: /.+/" } },
};
// A grammar tool called, then removed by a later system message.
const removedGrammarToolContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [gram], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		assistantToolCall("call_g", "gram", { query: "SELECT 1" }, 2),
		toolResult("call_g", "gram", "1", 3),
		{ role: "system", content: "", toolsRemoved: [{ name: "gram" }], timestamp: 4 },
		{ role: "user", content: "after", timestamp: 5 },
	],
};

const cases: Record<string, { model: any; context: any }> = {
	"kimi-k3-anchors-additions": { model: kimiK3, context: additionContext },
	"kimi-k2-text-inline": {
		model: {
			...modelBase,
			id: "kimi-k2.7-code",
			name: "Kimi K2.7 Code",
			api: "openai-completions",
			provider: "moonshotai",
			compat: { supportsMidConvoSystemMessages: true },
		},
		context: additionContext,
	},
	// provider openai: supportsDeveloperRole, so with reasoning every instruction
	// is a developer message; the removal makes the history non-additive, so
	// nothing anchors and the current tools are the request's.
	"developer-updates-non-additive": { model: gptTest, context },
	// No leading system message: the update sits at index 1, the Kimi tools
	// message carries the addition, its empty text sends nothing, and no
	// initial tools means no request tools.
	"kimi-k3-addition-without-leading-message": {
		model: kimiK3,
		context: {
			messages: [
				{ role: "user", content: "before", timestamp: 1 },
				{ role: "system", content: "", toolsAdded: [lateTool], timestamp: 2 },
				{ role: "user", content: "after", timestamp: 3 },
			],
		},
	},
	// The control: a user message right after tool results gets the bridge.
	"bridge-after-tool-result": {
		model: { ...customModel, compat: { supportsMidConvoSystemMessages: true, requiresAssistantAfterToolResult: true } },
		context: toolResultThen(),
	},
	// A system message in between sets lastRole to "system": no bridge, whether
	// or not it sends any text.
	"no-bridge-after-system-update": {
		model: { ...customModel, compat: { supportsMidConvoSystemMessages: true, requiresAssistantAfterToolResult: true } },
		context: toolResultThen({ role: "system", content: "updated guidance", timestamp: 4 }),
	},
	"no-bridge-after-empty-system-update": {
		model: { ...customModel, compat: { supportsMidConvoSystemMessages: true, requiresAssistantAfterToolResult: true } },
		context: toolResultThen({ role: "system", content: "", timestamp: 4 }),
	},
	"grammar-call-replays-declared-tool": {
		model: { ...customModel, compat: { supportsMidConvoSystemMessages: true, supportsOpenAIGrammarTools: true } },
		context: removedGrammarToolContext,
	},
	"grammar-call-collapsed-current-tools": {
		model: { ...customModel, compat: { supportsOpenAIGrammarTools: true } },
		context: removedGrammarToolContext,
	},
	// "Later" is the message's index in the transformed transcript, not how many
	// messages were pushed before it: a tools-only leading message sends nothing,
	// yet the system message after it is at index 1, so it anchors its additions
	// in a Kimi tools message and renders as an update ...
	"kimi-k3-update-after-empty-leading-message": {
		model: kimiK3,
		context: {
			messages: [
				{ role: "system", content: "", toolsAdded: [baseTool], timestamp: 0 },
				{ role: "system", content: "x", sections: { rules: "R" }, toolsAdded: [lateTool], timestamp: 1 },
				{ role: "user", content: "hi", timestamp: 2 },
			],
		},
	},
	// ... also when it anchors nothing, so nothing at all precedes it.
	"update-after-empty-leading-message": {
		model: { ...customModel, compat: { supportsMidConvoSystemMessages: true } },
		context: {
			messages: [
				{ role: "system", content: "", toolsAdded: [baseTool], timestamp: 0 },
				{ role: "system", content: "x", sections: { rules: "R" }, timestamp: 1 },
				{ role: "user", content: "hi", timestamp: 2 },
			],
		},
	},
	// The Kimi tools message is a system message whatever the instruction role,
	// and its tools convert with the provider's compat (strict on openai), while
	// a reasoning model on a developer-role provider sends the update text as a
	// developer message.
	"developer-updates-anchor-additions": { model: gptTest, context: additionContext },
	// A grammar tool anchored in a Kimi tools message converts to a custom tool
	// and its call replays as a custom call; a second addition anchors after it.
	"anchored-grammar-tool-and-later-additions": {
		model: { ...customModel, compat: { ...midConvo, supportsOpenAIGrammarTools: true } },
		context: {
			messages: [
				{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
				{ role: "user", content: "before", timestamp: 1 },
				{ role: "system", content: "", toolsAdded: [gram], timestamp: 2 },
				assistantToolCall("call_g", "gram", { query: "SELECT 1" }, 3),
				toolResult("call_g", "gram", "1", 4),
				{ role: "system", content: "more", toolsAdded: [lateTool, tool("later_tool")], timestamp: 5 },
				{ role: "user", content: "after", timestamp: 6 },
			],
		},
	},
	// Cache control marks the first system or developer message only: when that
	// is a content-less Kimi tools message, no instruction text is marked.
	"cache-control-first-instruction-is-kimi-tools-message": {
		model: { ...customModel, compat: { ...midConvo, cacheControlFormat: "anthropic" } },
		context: {
			messages: [
				{ role: "user", content: "before", timestamp: 1 },
				{ role: "system", content: "guidance", toolsAdded: [lateTool], timestamp: 2 },
				{ role: "user", content: "after", timestamp: 3 },
			],
		},
	},
	// Redeclaring a leading tool is not an addition: nothing anchors, and the
	// request declares the redefined current tools.
	"kimi-k3-redeclaration-is-non-additive": {
		model: kimiK3,
		context: {
			messages: [
				{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
				{ role: "user", content: "before", timestamp: 1 },
				{ role: "system", content: "", toolsAdded: [tool("base_tool", "redefined")], timestamp: 2 },
				{ role: "user", content: "after", timestamp: 3 },
			],
		},
	},
	// Tool additions alone keep nothing in place: the transcript collapses, so a
	// grammar tool removed later is no longer declared and its call replays as
	// a function call.
	"grammar-call-additions-flag-alone-collapses": {
		model: { ...customModel, compat: { supportsMidConvoToolAdditions: true, supportsOpenAIGrammarTools: true } },
		context: removedGrammarToolContext,
	},
	// Whitespace is text (pi tests `text.length > 0`): the update is sent.
	"whitespace-only-system-update": {
		model: { ...customModel, compat: { supportsMidConvoSystemMessages: true } },
		context: {
			messages: [
				{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
				{ role: "user", content: "before", timestamp: 1 },
				{ role: "system", content: "  ", timestamp: 2 },
				{ role: "user", content: "after", timestamp: 3 },
			],
		},
	},
	// A removed tool that a later tool message declares again counts as a
	// redeclaration: the top-level tools become the state after it, and the
	// new tool after it stays anchored (upstream caa6967b6's 'resolves the
	// top-level tool boundary per transport', through the request).
	"kimi-k3-redeclared-after-removal": {
		model: kimiK3,
		context: {
			messages: [
				{ role: "system", content: "base prompt", toolsAdded: [baseTool, dropTool], timestamp: 0 },
				{ role: "user", content: "before", timestamp: 1 },
				{ role: "system", content: "", toolsRemoved: [{ name: "drop_tool" }], timestamp: 2 },
				{ role: "system", content: "", toolsAdded: [dropTool], timestamp: 3 },
				{ role: "system", content: "", toolsAdded: [lateTool], timestamp: 4 },
				{ role: "user", content: "after", timestamp: 5 },
			],
		},
	},
	// transformMessages drops the aborted assistant message, so the system
	// message after it is the first transformed message, while the
	// untransformed transcript has no leading system message: no request
	// tools, and both system messages anchor their tools.
	"kimi-k3-aborted-first-assistant": {
		model: kimiK3,
		context: {
			messages: [
				{
					role: "assistant",
					content: [{ type: "text", text: "partial" }],
					api: "openai-completions",
					provider: "moonshotai",
					model: "kimi-k3",
					usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
					stopReason: "aborted",
					timestamp: 0,
				},
				{ role: "system", content: "guidance", toolsAdded: [baseTool], timestamp: 1 },
				{ role: "user", content: "before", timestamp: 2 },
				{ role: "system", content: "", toolsAdded: [lateTool], timestamp: 3 },
				{ role: "user", content: "after", timestamp: 4 },
			],
		},
	},
};
// 'keeps the Kimi prefix across loads after a removal': one body per step of
// the suite's captureToolHistory, on the K3 fixture rather than the catalog's
// kimi-coding k3 (a source extraction carries no generated catalog).
toolHistory().forEach((ctx, step) => {
	cases[`kimi-k3-history-${step}`] = { model: kimiK3, context: ctx };
});

// Cases streamed to the end against a recorded response: pi's final content,
// stopReason and errorMessage are recorded next to the SSE it parsed.
const streamedCases: Record<string, { model: any; context: any; chunks: object[] }> = {
	// A custom tool call is parsed against the tools the resolved transcript
	// declared: a grammar tool removed in place still maps its input to the
	// tool's own property.
	"streamed-custom-call-to-removed-grammar-tool": {
		model: { ...customModel, compat: { supportsMidConvoSystemMessages: true, supportsOpenAIGrammarTools: true } },
		context: {
			messages: [
				{ role: "system", content: "base prompt", toolsAdded: [gram], timestamp: 0 },
				{ role: "user", content: "before", timestamp: 1 },
				{ role: "system", content: "", toolsRemoved: [{ name: "gram" }], timestamp: 2 },
				{ role: "user", content: "after", timestamp: 3 },
			],
		},
		chunks: [
			{ id: "c1", model: "custom-model", choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: "call_g", type: "custom", custom: { name: "gram", input: "SELECT " } }] } }] },
			{ id: "c1", model: "custom-model", choices: [{ index: 0, delta: { tool_calls: [{ index: 0, custom: { input: "1" } }] } }] },
			{ id: "c1", model: "custom-model", choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }] },
		],
	},
};

// Cases whose stream fails before any body is built: pi's stopReason and
// errorMessage are recorded instead.
const failingCases: Record<string, { model: any; context: any }> = {
	// A Kimi-anchored addition is converted like a request tool, so a tool the
	// provider cannot honor fails the stream even though no request tool does.
	"kimi-k3-anchored-tool-conversion-fails": {
		model: kimiK3,
		context: {
			messages: [
				{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
				{ role: "user", content: "before", timestamp: 1 },
				{
					role: "system",
					content: "updated guidance",
					toolsAdded: [{ ...lateTool, constrainedSampling: { type: "json_schema", strict: "require" } }],
					timestamp: 2,
				},
			],
		},
	},
};

async function run(model: any, ctx: any): Promise<{ payload: unknown; final: any }> {
	let payload: unknown;
	const stream = completions.streamSimple(model, normalizeContext(ctx), {
		apiKey: "test-key",
		onPayload: (captured: unknown) => {
			payload = captured;
			throw new Error("payload captured");
		},
	});
	const final = await stream.result();
	return { payload, final };
}

const out: Record<string, unknown> = { sha };
for (const [name, { model, context: ctx }] of Object.entries(cases)) {
	const { payload, final } = await run(model, ctx);
	if (payload === undefined) throw new Error(`${name}: no payload: ${final.errorMessage}`);
	out[name] = { model, context: ctx, body: payload };
}
for (const [name, { model, context: ctx }] of Object.entries(failingCases)) {
	const { payload, final } = await run(model, ctx);
	if (payload !== undefined) throw new Error(`${name}: a payload was built`);
	out[name] = { model, context: ctx, stopReason: final.stopReason, errorMessage: final.errorMessage };
}
for (const [name, { model, context: ctx, chunks }] of Object.entries(streamedCases)) {
	const sse = `${chunks.map((chunk) => `data: ${JSON.stringify(chunk)}\n\n`).join("")}data: [DONE]\n\n`;
	const fetch = async () => new Response(sse, { status: 200, headers: { "content-type": "text/event-stream" } });
	const final = await completions.streamSimple(model, normalizeContext(ctx), { apiKey: "test-key", fetch }).result();
	out[name] = {
		model,
		context: ctx,
		sse,
		content: final.content,
		stopReason: final.stopReason,
		errorMessage: final.errorMessage,
	};
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
