// Captures the request bodies real pi builds for Anthropic models that accept
// system messages and tool changes mid-conversation — the oracle behind
// TestAnthropicNative*RequestBody in this package.
//
//   node --experimental-strip-types capture-anthropic-native.mts <extraction> <out.json> <sha>
//   e.g. ... capture-anthropic-native.mts <dir> anthropic-native-9e05370b2.json 9e05370b2
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies (the npm
// build's). The npm build 0.85.1 predates upstream 9e05370b2, so these are src
// captures: re-verify them against the first build that ships it (the BUILD
// wins).
//
// The models, `context` and the two fallback contexts are
// packages/ai/test/transcript-tool-changes.test.ts's; every other context is an
// edge case of this port's, commented with the rule it pins. The suite's compat.ts
// streamSimple is normalizeContext followed by the api's streamSimple, which is
// called directly here (compat.ts also loads generated catalog data a source
// extraction does not carry). Bodies are captured from onPayload, which throws.
import path from "node:path";
import fs from "node:fs";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-anthropic-native.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { normalizeContext } = await load("utils/transcript.ts");
const anthropic = await load("api/anthropic-messages.ts");
const { Type } = await import(pathToFileURL(path.join(extraction, "node_modules/typebox/build/index.mjs")).href);

function tool(name: string) {
	return { name, description: `${name} tool`, parameters: Type.Object({}) };
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
const nativeModel = {
	...modelBase,
	id: "claude-opus-5",
	name: "Claude Opus 5",
	api: "anthropic-messages",
	provider: "anthropic",
	compat: { supportsMidConvoSystemMessages: true, supportsMidConvoToolChanges: true },
};

const usage = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 0,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};

const baseTool = tool("base_tool");
const lateTool = tool("late_tool");
// The suite's `context`.
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
// The suite's two fallback contexts at 9e05370b2. Since b271b0a52 the
// redefinition is sent natively (the new definition inline, no removal for the
// re-added name); only the history without an initial tool still falls back.
const redefinedTool = { ...baseTool, description: "changed" };
const redefinition = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{
			role: "system",
			content: "updated guidance",
			toolsRemoved: [{ name: "base_tool" }],
			toolsAdded: [redefinedTool],
			timestamp: 2,
		},
	],
};
const fallbackNoInitialTools = {
	messages: [
		{ role: "system", content: "base prompt", timestamp: 0 },
		{ role: "system", content: "updated guidance", toolsAdded: [redefinedTool], timestamp: 2 },
	],
};

// OAuth: tool names Claude Code knows are renamed on the wire, in the tool list,
// in tool_use blocks, in tool_removal references and in the inline definitions
// of tool_addition blocks. The update
// lands between a tool call and its result, so transformMessages holds it past
// the result and convertMessages holds it again until the end of the
// transcript, where it follows the last user message.
const oauthContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [tool("read"), baseTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		{
			role: "assistant",
			content: [{ type: "toolCall", id: "toolu_1", name: "read", arguments: { path: "a.txt" } }],
			api: "anthropic-messages",
			provider: "anthropic",
			model: "claude-opus-5",
			usage,
			stopReason: "toolUse",
			timestamp: 2,
		},
		{
			role: "system",
			content: "updated guidance",
			toolsRemoved: [{ name: "read" }],
			toolsAdded: [tool("bash")],
			timestamp: 3,
		},
		{
			role: "toolResult",
			toolCallId: "toolu_1",
			toolName: "read",
			content: [{ type: "text", text: "contents" }],
			isError: false,
			timestamp: 4,
		},
		{ role: "user", content: "after", timestamp: 5 },
	],
};

// Managed effort on a native model: a held update is flushed immediately before
// the next assistant message, ahead of that turn's effort-only system message,
// and the final flush precedes the trailing effort message.
const effortModel = {
	...nativeModel,
	compat: { ...nativeModel.compat, supportsMidConvoEffort: true },
};
const effortContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "first", timestamp: 1 },
		{ role: "system", content: "mid guidance", toolsAdded: [lateTool], timestamp: 2 },
		{
			role: "assistant",
			content: [{ type: "text", text: "ok" }],
			api: "anthropic-messages",
			provider: "anthropic",
			model: "claude-opus-5",
			providerThinkingLevel: "low",
			usage,
			stopReason: "stop",
			timestamp: 3,
		},
		{ role: "user", content: "second", timestamp: 4 },
		{ role: "system", content: "closing guidance", toolsRemoved: [{ name: "late_tool" }], timestamp: 5 },
	],
};

// An update's rendered text goes through sanitizeSurrogates: the lone high
// surrogate is deleted and the paired one (an emoji) survives.
const surrogateContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		{
			role: "system",
			content: "lone \uD83D here",
			sections: { rules: "paired 🙈 rules" },
			toolsAdded: [lateTool],
			timestamp: 2,
		},
	],
};

// A plain-string user message is sent as a string, an array-form one as
// blocks, and a blank one not at all; only the LAST message is turned into a
// cache_control-bearing block list. Not a transcript case: the native update is
// what first leaves a string-form user message ahead of the last message in
// these fixtures, so its bytes are pinned on their own.
const plainModel = {
	...modelBase,
	id: "claude-sonnet-4-5",
	name: "Claude Sonnet 4.5",
	api: "anthropic-messages",
	provider: "anthropic",
};
const plainReply = (text: string, timestamp: number) => ({
	role: "assistant",
	content: [{ type: "text", text }],
	api: "anthropic-messages",
	provider: "anthropic",
	model: "claude-sonnet-4-5",
	usage,
	stopReason: "stop",
	timestamp,
});
const userContentForms = {
	systemPrompt: "base prompt",
	messages: [
		{ role: "user", content: "first", timestamp: 1 },
		plainReply("ok", 2),
		{ role: "user", content: [{ type: "text", text: "array form" }], timestamp: 3 },
		{ role: "user", content: "   ", timestamp: 4 },
		plainReply("ok again", 5),
		{ role: "user", content: "last", timestamp: 6 },
	],
};

// A later system message that renders to nothing is not sent: a tools-only
// update carries no text, and its tool changes become blocks only under native
// tool changes — so it is dropped on a model with system messages alone, and on
// a native model whose history falls back (here: no initial tool).
const toolsOnlyUpdate = (toolsAdded: unknown[]) => ({ role: "system", content: "", toolsAdded, timestamp: 2 });
const systemMessagesOnlyToolsOnlyUpdate = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		toolsOnlyUpdate([lateTool]),
	],
};
const fallbackToolsOnlyUpdate = {
	messages: [
		{ role: "system", content: "base prompt", timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		toolsOnlyUpdate([lateTool]),
	],
};

// Tool references take Claude Code names only under OAuth: with an API key a
// tool named `read` is removed as `read` and `bash` added as `bash`.
const apiKeyClaudeCodeNamesContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [tool("read")], timestamp: 0 },
		{ role: "user", content: "before", timestamp: 1 },
		{ role: "system", content: "", toolsRemoved: [{ name: "read" }], toolsAdded: [tool("bash")], timestamp: 2 },
	],
};

// A held update is flushed on reaching an assistant message even when that
// message converts to no blocks (whitespace-only text), so it lands before the
// next user message rather than at the end.
const emptyAssistantContext = {
	messages: [
		{ role: "system", content: "base prompt", toolsAdded: [baseTool], timestamp: 0 },
		{ role: "user", content: "first", timestamp: 1 },
		{ role: "system", content: "mid guidance", toolsAdded: [lateTool], timestamp: 2 },
		{
			role: "assistant",
			content: [{ type: "text", text: "   " }],
			api: "anthropic-messages",
			provider: "anthropic",
			model: "claude-opus-5",
			usage,
			stopReason: "stop",
			timestamp: 3,
		},
		{ role: "user", content: "second", timestamp: 4 },
	],
};

// A string-form user message goes through sanitizeSurrogates: the lone high
// surrogate is deleted from the non-last message (sent as a string), and the
// paired one survives in the last (turned into the cache_control block).
const userStringSurrogates = {
	systemPrompt: "base prompt",
	messages: [
		{ role: "user", content: "lone \uD83D here", timestamp: 1 },
		plainReply("ok", 2),
		{ role: "user", content: "paired 🙈 last", timestamp: 3 },
	],
};

async function capturePayload(model: any, ctx: unknown, apiKey = "test-key"): Promise<unknown> {
	let captured: unknown;
	const stream = anthropic.streamSimple(model, normalizeContext(ctx), {
		apiKey,
		onPayload: (payload: unknown) => {
			captured = payload;
			throw new Error("payload captured");
		},
	});
	const final = await stream.result();
	if (captured === undefined) throw new Error(`no payload: ${final.errorMessage}`);
	return JSON.parse(JSON.stringify(captured));
}

const out = {
	sha,
	native: await capturePayload(nativeModel, context),
	nativeInitial: await capturePayload(nativeModel, { messages: context.messages.slice(0, 2) }),
	nativeOAuth: await capturePayload(nativeModel, oauthContext, "sk-ant-oat-test"),
	nativeEffort: await capturePayload(effortModel, effortContext),
	// Mid-conversation system messages without native tool changes: updates are
	// sent in place as text, and the request declares the current tools.
	systemMessagesOnly: await capturePayload(
		{ ...nativeModel, compat: { supportsMidConvoSystemMessages: true } },
		context,
	),
	redefinition: await capturePayload(nativeModel, redefinition),
	fallbackNoInitialTools: await capturePayload(nativeModel, fallbackNoInitialTools),
	userContentForms: await capturePayload(plainModel, userContentForms),
	nativeSurrogates: await capturePayload(nativeModel, surrogateContext),
	systemMessagesOnlyToolsOnlyUpdate: await capturePayload(
		{ ...nativeModel, compat: { supportsMidConvoSystemMessages: true } },
		systemMessagesOnlyToolsOnlyUpdate,
	),
	fallbackToolsOnlyUpdate: await capturePayload(nativeModel, fallbackToolsOnlyUpdate),
	nativeApiKeyClaudeCodeNames: await capturePayload(nativeModel, apiKeyClaudeCodeNamesContext),
	// Native initial tools take the tool cache breakpoint only when the model
	// supports cache_control on tools.
	nativeNoToolCacheControl: await capturePayload(
		{ ...nativeModel, compat: { ...nativeModel.compat, supportsCacheControlOnTools: false } },
		context,
	),
	nativeEmptyAssistant: await capturePayload(nativeModel, emptyAssistantContext),
	userStringSurrogates: await capturePayload(plainModel, userStringSurrogates),
	// A configured anthropic-beta header replaces the computed beta list, so the
	// inline-tools beta is not added, while the native blocks are still sent.
	nativeConfiguredBetaHeader: await capturePayload(
		{ ...nativeModel, headers: { "anthropic-beta": "x-beta, x-beta ,y-beta" } },
		context,
	),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
