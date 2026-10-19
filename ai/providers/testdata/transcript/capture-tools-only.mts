// Captures the request bodies real pi builds for a context that declares tools
// but no system prompt — the oracle behind TestToolsOnlyContext*RequestBody and
// TestFauxToolsOnlySystemMessageDropsEmptyText in this package.
//
//   node --experimental-strip-types capture-tools-only.mts <extraction> <out.json> <sha>
//   e.g. ... capture-tools-only.mts <dir> tools-only-95fbc0499.json 95fbc0499
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
// normalizeContext turns {tools, messages} into a leading
// {role:"system", content:"", toolsAdded} message; every adapter must send the
// tools and nothing for the empty prompt text. The fixture and models are
// packages/ai/test/transcript-tool-changes.test.ts's; its compat.ts streamSimple
// is normalizeContext followed by the api's streamSimple, which is called
// directly here (compat.ts also loads generated catalog data a source extraction
// does not carry). The anthropic, responses and completions bodies are captured
// from onPayload. google-generative-ai hands onPayload the @google/genai call params
// ({model, contents, config}), a different layer from the REST body the Go
// adapter builds, so its body is captured where the SDK POSTs it
// (globalThis.fetch, which pi requires the SDK to use).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-tools-only.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { normalizeContext } = await load("utils/transcript.ts");
const apis: Record<string, any> = {
	"anthropic-messages": await load("api/anthropic-messages.ts"),
	"openai-responses": await load("api/openai-responses.ts"),
	"openai-completions": await load("api/openai-completions.ts"),
	"google-generative-ai": await load("api/google-generative-ai.ts"),
};
const streamSimple = (model: any, ctx: unknown, options: object) =>
	apis[model.api].streamSimple(model, normalizeContext(ctx), options);
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
const context = { tools: [tool("base_tool")], messages: [{ role: "user", content: "before", timestamp: 1 }] };

async function capturePayload(model: any, apiKey = "test-key"): Promise<unknown> {
	let captured: unknown;
	const stream = streamSimple(model, context, {
		apiKey,
		onPayload: (payload: unknown) => {
			captured = payload;
			throw new Error("payload captured");
		},
	});
	const final = await stream.result();
	if (captured === undefined) throw new Error(`no payload: ${final.errorMessage}`);
	return captured;
}

async function captureFetchBody(model: any): Promise<unknown> {
	const realFetch = globalThis.fetch;
	let captured: string | undefined;
	globalThis.fetch = (async (_url: unknown, init?: { body?: unknown }) => {
		captured ??= String(init?.body);
		throw new Error("body captured");
	}) as typeof fetch;
	try {
		const final = await streamSimple(model, context, { apiKey: "test-key", maxRetries: 0 }).result();
		if (captured === undefined) throw new Error(`no body: ${final.errorMessage}`);
		return JSON.parse(captured);
	} finally {
		globalThis.fetch = realFetch;
	}
}

// faux.ts renders the transcript with module-private helpers; evaluate the
// file's own definitions (contentToText through messageToText) rather than a
// copy. The prompt text is one `role:text` per message joined by a blank line:
// serializeContext until fe11328b0, withUsageEstimate's inline map since.
async function fauxSerializeContext(): Promise<string> {
	const faux = fs.readFileSync(path.join(src, "providers/faux.ts"), "utf8");
	const start = faux.indexOf("function contentToText(");
	const endMarker = "function commonPrefixLength(";
	const end = faux.indexOf(endMarker);
	if (start < 0 || end < 0) throw new Error("faux.ts helpers not found");
	const module = path.join(path.dirname(outFile), ".faux-serialize.tmp.mts");
	fs.writeFileSync(
		module,
		`import { getSystemMessageText } from ${JSON.stringify(pathToFileURL(path.join(src, "utils/text.ts")).href)};\n` +
			faux.slice(start, end) +
			"export { messageToText };\n",
	);
	try {
		const { messageToText } = await import(pathToFileURL(module).href);
		return normalizeContext(context)
			.messages.map((message: { role: string }) => `${message.role}:${messageToText(message)}`)
			.join("\n\n");
	} finally {
		fs.rmSync(module);
	}
}

const out = {
	sha,
	context: normalizeContext(context),
	"anthropic-messages": await capturePayload({
		...modelBase,
		id: "claude-sonnet-4-5",
		name: "Claude Sonnet 4.5",
		api: "anthropic-messages",
		provider: "anthropic",
	}),
	// An OAuth token prepends the Claude Code identity block to the system prompt.
	"anthropic-messages-oauth": await capturePayload(
		{
			...modelBase,
			id: "claude-sonnet-4-5",
			name: "Claude Sonnet 4.5",
			api: "anthropic-messages",
			provider: "anthropic",
		},
		"sk-ant-oat-test",
	),
	"openai-responses": await capturePayload({
		...modelBase,
		id: "gpt-4.1",
		name: "GPT-4.1",
		api: "openai-responses",
		provider: "openai",
		compat: { supportsAdditionalTools: true },
	}),
	"openai-completions": await capturePayload({
		...modelBase,
		id: "custom-model",
		name: "Custom model",
		api: "openai-completions",
		provider: "custom-provider",
		reasoning: false,
	}),
	"google-generative-ai": await captureFetchBody({
		...modelBase,
		id: "gemini-2.5-flash",
		name: "gemini-2.5-flash",
		api: "google-generative-ai",
		provider: "google",
		reasoning: false,
	}),
	fauxSerializedContext: await fauxSerializeContext(),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
