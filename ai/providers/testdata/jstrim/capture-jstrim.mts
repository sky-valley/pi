// Captures what real pi does with content that is blank only under one of the
// two whitespace vocabularies — U+FEFF (JavaScript trims it, Go's
// strings.TrimSpace does not) and U+0085 (the reverse) — the oracle behind
// jstrim_test.go in this package.
//
//   node --experimental-strip-types capture-jstrim.mts <extraction> <out.json> <sha>
//   e.g. ... capture-jstrim.mts <dir> jstrim-911309429.json 911309429
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone and a node_modules resolving pi-ai's dependencies (the npm
// build's). The script refuses to write unless each SDK it runs
// (@anthropic-ai/sdk, openai, @google/genai) resolves to the version AND
// integrity the sha's package-lock.json locks at the path it resolved to:
// packages/ai/node_modules/<name> for a copy the lockfile nests for pi-ai,
// node_modules/<name> otherwise. Since ab30693d6 it nests openai 7.19.0 there
// (the root keeps 6.40.0): unpack `npm pack openai@7.19.0` (check its sha512
// against the lockfile) to packages/ai/node_modules/openai and record that
// integrity in packages/ai/node_modules/.package-lock.json under
// "node_modules/openai", as npm records an installed package. The SDKs read,
// with their integrity, and the node version are recorded as `source`.
//
// `bodies` are request bodies: from onPayload, which throws, except google's,
// which is the REST body @google/genai POSTs to a local server (onPayload sees
// the SDK's call params, a different layer from the body the Go adapter
// builds). `auth` records whether a whitespace-only credential header counts as
// auth: the thrown message, or "payload" when the request got as far as its
// body. `streams` serve a recorded SSE body from a local server and record the
// outcome; every SSE body is recorded here so the Go test serves the same
// bytes. `transform`, `grammar`, `streamingJson` and `retryAfter` are pi's own
// helpers (and, for retryAfter, the exact expression getRetryDelayMs evaluates
// on the header fetch decoded from raw bytes).
import fs from "node:fs";
import http from "node:http";
import net from "node:net";
import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-jstrim.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const require = createRequire(path.join(src, "api/openai-completions.ts"));
const lock = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8"));
const sdks: string[] = [];
for (const name of ["@anthropic-ai/sdk", "openai", "@google/genai"]) {
	let dir = path.dirname(require.resolve(name));
	while (!fs.existsSync(path.join(dir, "package.json")) || JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).name !== name) {
		dir = path.dirname(dir);
	}
	const version = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version as string;
	let root = dir;
	while (path.basename(root) !== "node_modules") root = path.dirname(root);
	const installed = JSON.parse(fs.readFileSync(path.join(root, ".package-lock.json"), "utf8")).packages[`node_modules/${name}`];
	// The lockfile entry for the path it resolved to: pi-ai's own nested copy
	// (packages/ai/node_modules/<name>, as openai is since ab30693d6) or the
	// root one (node_modules/<name>). Where the lockfile nests a copy for
	// pi-ai, pi-ai must resolve that one.
	const resolvedKey = path.relative(extraction, dir);
	const key = resolvedKey.startsWith("..") || path.isAbsolute(resolvedKey) ? `node_modules/${name}` : resolvedKey;
	const nestedKey = `packages/ai/node_modules/${name}`;
	const locked = lock.packages[key];
	if ((lock.packages[nestedKey] && key !== nestedKey) || locked?.version !== version || locked.integrity !== installed?.integrity) {
		const want = lock.packages[nestedKey] ? nestedKey : key;
		console.error(`${name} mismatch: ${sha} locks ${lock.packages[want]?.version} ${lock.packages[want]?.integrity} at ${want}, resolved ${version} ${installed?.integrity} at ${key}`);
		process.exit(1);
	}
	sdks.push(`${name} ${version} ${locked.integrity}`);
}
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { normalizeContext } = await load("utils/transcript.ts");
const anthropic = await load("api/anthropic-messages.ts");
const completions = await load("api/openai-completions.ts");
const responses = await load("api/openai-responses.ts");
const google = await load("api/google-generative-ai.ts");
const piMessages = await load("api/pi-messages.ts");
const { resolveGrammarConstrainedSampling } = await load("api/constrained-sampling.ts");
const { parseStreamingJson } = await load("utils/json-parse.ts");
const { transformMessages } = await load("api/transform-messages.ts");
const { Type } = await import(pathToFileURL(path.join(extraction, "node_modules/typebox/build/index.mjs")).href);

const BOM = "\ufeff";
const NEL = "\u0085";

const modelBase = {
	baseUrl: "http://127.0.0.1:9",
	reasoning: true,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 100000,
	maxTokens: 1000,
};
const usage = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 0,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};
const assistant = (api: string, provider: string, model: string, content: unknown[], timestamp: number) => ({
	role: "assistant",
	content,
	api,
	provider,
	model,
	usage,
	stopReason: "stop",
	timestamp,
});

// A local server answering every request with `body`, recording request bodies.
async function serve(body: string): Promise<{ url: string; requests: string[]; close: () => void }> {
	const requests: string[] = [];
	const server = http.createServer((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", (c) => chunks.push(c));
		req.on("end", () => {
			requests.push(Buffer.concat(chunks).toString("utf8"));
			res.writeHead(200, { "content-type": "text/event-stream" });
			res.end(body);
		});
	});
	await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
	const { port } = server.address() as net.AddressInfo;
	return { url: `http://127.0.0.1:${port}`, requests, close: () => server.close() };
}

async function capturePayload(api: any, model: unknown, ctx: unknown, options: Record<string, unknown>) {
	let captured: unknown;
	let stream: any;
	try {
		stream = api.streamSimple(model, normalizeContext(ctx), {
			...options,
			onPayload: (payload: unknown) => {
				captured = payload;
				throw new Error("payload captured");
			},
		});
	} catch (error) {
		// anthropic's streamSimple asserts request auth before it returns a stream.
		return { error: (error as Error).message };
	}
	const final = await stream.result();
	if (captured === undefined) return { error: final.errorMessage };
	return { payload: JSON.parse(JSON.stringify(captured)) };
}

async function outcome(api: any, model: unknown, ctx: unknown, options: Record<string, unknown>) {
	const final = await api.streamSimple(model, normalizeContext(ctx), options).result();
	const text = final.content
		.filter((b: { type: string }) => b.type === "text")
		.map((b: { text: string }) => b.text)
		.join("");
	return { stopReason: final.stopReason, errorMessage: final.errorMessage ?? "", text };
}

// ---- anthropic-messages -----------------------------------------------------

const anthropicModel = {
	...modelBase,
	id: "claude-sonnet-4-5",
	name: "Claude Sonnet 4.5",
	api: "anthropic-messages",
	provider: "anthropic",
	headers: { "anthropic-beta": `${BOM}x-beta${BOM}, ${NEL}y-beta,\u3000` },
};
const anthropicContext = {
	systemPrompt: "base prompt",
	messages: [
		{ role: "user", content: BOM, timestamp: 1 },
		{ role: "user", content: NEL, timestamp: 2 },
		{
			role: "user",
			content: [
				{ type: "text", text: BOM },
				{ type: "text", text: NEL },
			],
			timestamp: 3,
		},
		assistant(
			"anthropic-messages",
			"anthropic",
			"claude-sonnet-4-5",
			[
				{ type: "text", text: BOM },
				{ type: "text", text: NEL },
				{ type: "thinking", thinking: BOM, thinkingSignature: BOM },
				{ type: "thinking", thinking: NEL, thinkingSignature: NEL },
				{ type: "thinking", thinking: "unsigned", thinkingSignature: "\u3000" },
			],
			4,
		),
		{ role: "user", content: "last", timestamp: 5 },
	],
};

// ---- openai-completions -----------------------------------------------------

const completionsModel = {
	...modelBase,
	id: "gpt-4o",
	name: "GPT-4o",
	api: "openai-completions",
	provider: "openai",
};
// The second assistant turn is another model's: transformMessages drops its
// thinking before the converter runs (since 911309429; before, it dropped the
// blank thinking and turned the rest into text).
const completionsContext = {
	systemPrompt: "base prompt",
	messages: [
		{ role: "user", content: "first", timestamp: 1 },
		assistant(
			"openai-completions",
			"openai",
			"gpt-4o",
			[
				{ type: "text", text: BOM },
				{ type: "text", text: NEL },
				{ type: "thinking", thinking: BOM, thinkingSignature: "reasoning_content" },
				{ type: "thinking", thinking: NEL, thinkingSignature: "reasoning_content" },
			],
			2,
		),
		{ role: "user", content: "second", timestamp: 3 },
		assistant(
			"anthropic-messages",
			"anthropic",
			"claude-sonnet-4-5",
			[
				{ type: "thinking", thinking: BOM },
				{ type: "thinking", thinking: NEL },
				{ type: "text", text: "visible" },
			],
			4,
		),
		{ role: "user", content: "third", timestamp: 5 },
	],
};

// ---- google-generative-ai ---------------------------------------------------

const googleContext = {
	messages: [
		{ role: "user", content: "Hi", timestamp: 1 },
		assistant(
			"google-generative-ai",
			"google",
			"gemini-3-pro-preview",
			[
				{ type: "text", text: BOM },
				{ type: "text", text: NEL },
				{ type: "thinking", thinking: BOM, thinkingSignature: BOM },
				{ type: "thinking", thinking: NEL, thinkingSignature: NEL },
				{ type: "toolCall", id: "call_1", name: "bash", arguments: { command: "ls" } },
			],
			2,
		),
	],
};

async function googleContents(ctx: unknown) {
	const server = await serve('data: {"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}]}\n\n');
	try {
		const model = {
			...modelBase,
			baseUrl: server.url,
			id: "gemini-3-pro-preview",
			name: "Gemini 3 Pro",
			api: "google-generative-ai",
			provider: "google",
		};
		const final = await google.streamSimple(model, normalizeContext(ctx), { apiKey: "k" }).result();
		if (server.requests.length !== 1) throw new Error(`no google request: ${final.errorMessage}`);
		return JSON.parse(server.requests[0]).contents;
	} finally {
		server.close();
	}
}

// ---- SSE bodies ---------------------------------------------------------------

const userHi = { messages: [{ role: "user", content: "hi", timestamp: 1 }] };

const completionsChunk = (delta: string, finish: string | null) =>
	`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"gpt-4o","choices":[{"index":0,"delta":${delta},"finish_reason":${finish === null ? "null" : `"${finish}"`}}]}`;
// `pad` wraps the JSON of the one data line that carries the text.
const completionsSSE = (before: string, after: string) =>
	`data:${before}${completionsChunk('{"role":"assistant","content":"hi"}', null)}${after}\n\n` +
	`data: ${completionsChunk("{}", "stop")}\n\n` +
	"data: [DONE]\n\n";

const responsesSSE = (before: string, after: string) =>
	'data: {"type":"response.created","response":{"id":"r"}}\n\n' +
	`data:${before}{"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}${after}\n\n` +
	'data: {"type":"response.content_part.added","part":{"type":"output_text","text":""}}\n\n' +
	'data: {"type":"response.output_text.delta","delta":"hi"}\n\n' +
	'data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hi"}]}}\n\n' +
	'data: {"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_tokens_details":{"cached_tokens":0}}}}\n\n';

const paddings: Array<[string, string, string]> = [
	["space", " ", ""],
	["tab", "\t", ""],
	["bomBefore", BOM, ""],
	["nbspBefore", "\u00a0", ""],
	["nelBefore", NEL, ""],
	["vtAfter", " ", ""],
	["nelAfter", " ", NEL],
	["emSpaceAfter", " ", "\u2003"],
];

const googleOK = '{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}]}';
const googleSSE: Record<string, string> = {
	plain: `data: ${googleOK}\n\n`,
	bomBeforeData: `${BOM}data: ${googleOK}\n\n`,
	nbspBeforeData: `\u00a0data: ${googleOK}\n\n`,
	nelBeforeData: `${NEL}data: ${googleOK}\n\n`,
	bomAfterColon: `data:${BOM}${googleOK}\n\n`,
	trailingBom: `data: ${googleOK}\n\n${BOM}`,
	trailingNel: `data: ${googleOK}\n\n${NEL}`,
};

const piFrames = (pad: string) =>
	[
		'{"type":"start"}',
		'{"type":"text_start","contentIndex":0}',
		'{"type":"text_delta","contentIndex":0,"delta":"hi"}',
		'{"type":"text_end","contentIndex":0,"content":"hi"}',
	]
		.map((f) => `data: ${f}\n\n`)
		.join("") +
	`data:${pad}{"type":"done","reason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}\n\n`;
const piMessagesSSE: Record<string, string> = {
	space: piFrames(" "),
	bomBefore: piFrames(BOM),
	nelBefore: piFrames(NEL),
};

async function streamCase(api: any, model: Record<string, unknown>, body: string, apiKey = "test-key") {
	const server = await serve(body);
	try {
		return {
			sse: body,
			...(await outcome(api, { ...model, baseUrl: `${server.url}${model.pathPrefix ?? ""}` }, userHi, { apiKey })),
		};
	} finally {
		server.close();
	}
}

const streams: Record<string, Record<string, unknown>> = {
	completions: {},
	responses: {},
	google: {},
	piMessages: {},
};
for (const [name, before, after] of paddings) {
	streams.completions[name] = await streamCase(completions, completionsModel, completionsSSE(before, after));
	streams.responses[name] = await streamCase(
		responses,
		{ ...modelBase, id: "gpt-5", name: "GPT-5", api: "openai-responses", provider: "openai" },
		responsesSSE(before, after),
	);
}
for (const [name, body] of Object.entries(googleSSE)) {
	streams.google[name] = await streamCase(
		google,
		{ ...modelBase, id: "gemini-2.5-flash", name: "Gemini 2.5 Flash", api: "google-generative-ai", provider: "google" },
		body,
		"k",
	);
}
for (const [name, body] of Object.entries(piMessagesSSE)) {
	streams.piMessages[name] = await streamCase(
		piMessages,
		{ ...modelBase, id: "auto", name: "Radius Auto", api: "pi-messages", provider: "radius", pathPrefix: "/v1" },
		body,
	);
}

// ---- helpers ----------------------------------------------------------------

const grammarTool = (lark: string | undefined, regex: string | undefined) => ({
	name: "g",
	description: "g",
	parameters: Type.Object({ input: Type.String() }),
	constrainedSampling: { type: "grammar", variants: { openai_lark: lark, openai_regex: regex } },
});
const grammarCases: Array<[string, string | undefined, string | undefined]> = [
	["bomLark", BOM, "a+"],
	["nelLark", NEL, "a+"],
	["bomLarkBlankRegex", BOM, "\u3000"],
	["nelRegexOnly", undefined, NEL],
];
const grammar: Record<string, unknown> = {};
for (const [name, lark, regex] of grammarCases) {
	try {
		grammar[name] = { lark, regex, result: resolveGrammarConstrainedSampling(grammarTool(lark, regex), true) };
	} catch (error) {
		grammar[name] = { lark, regex, error: (error as Error).message };
	}
}

// transformMessages on the model's own turn: unsigned thinking that is blank
// to String.prototype.trim is dropped, the rest kept. Every converter then
// drops blank text too, so a request body cannot tell what the trim let
// through. Another model's thinking is dropped whatever it holds (since
// 911309429; before, the trim decided which of it became text).
const thinkingTurn = (api: string, provider: string, model: string) =>
	assistant(
		api,
		provider,
		model,
		[
			{ type: "thinking", thinking: BOM },
			{ type: "thinking", thinking: NEL },
			{ type: "thinking", thinking: "\u3000" },
			{ type: "text", text: "visible" },
		],
		2,
	);
const transform = {
	sameModel: transformMessages(
		[{ role: "user", content: "hi", timestamp: 1 }, thinkingTurn("openai-completions", "openai", "gpt-4o")],
		completionsModel,
	)[1].content,
	otherModel: transformMessages(
		[{ role: "user", content: "hi", timestamp: 1 }, thinkingTurn("anthropic-messages", "anthropic", "claude-sonnet-4-5")],
		completionsModel,
	)[1].content,
};

const streamingJsonInputs = [
	BOM,
	NEL,
	'{"a":1\u00a0',
	`${BOM}{"a":1`,
	'{"a":"x\u00a0',
	`{"a":"x${NEL}`,
	'{"a":[1,2\u2028',
	`${NEL}{"a":1`,
	'{"a":1,\u3000',
];
const streamingJson = streamingJsonInputs.map((input) => ({ input, result: parseStreamingJson(input) }));

// getRetryDelayMs reads `Number.parseFloat(error.headers.get("retry-after"))`;
// fetch decodes header bytes as latin1, so the raw bytes are the input.
const retryAfterBytes = [
	[0x35],
	[0xa0, 0x35],
	[0x85, 0x35],
	[0xc2, 0xa0, 0x35],
	[0xc2, 0x85, 0x35],
	[0xef, 0xbb, 0xbf, 0x35],
	[0xe2, 0x80, 0x83, 0x35],
];
const retryAfter: unknown[] = [];
for (const bytes of retryAfterBytes) {
	const server = net.createServer((sock) => {
		sock.once("data", () => {
			sock.end(
				Buffer.concat([
					Buffer.from("HTTP/1.1 429 Too Many Requests\r\nretry-after: "),
					Buffer.from(bytes),
					Buffer.from("\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"),
				]),
			);
		});
	});
	await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
	const { port } = server.address() as net.AddressInfo;
	const res = await fetch(`http://127.0.0.1:${port}/`);
	const value = res.headers.get("retry-after") ?? "";
	const seconds = Number.parseFloat(value);
	retryAfter.push({ bytes, seconds: Number.isNaN(seconds) ? null : seconds });
	server.close();
}

const out = {
	sha,
	source: `upstream ${sha} packages/ai/src, ${sdks.join(", ")}, node ${process.version}`,
	bodies: {
		anthropic: await capturePayload(anthropic, anthropicModel, anthropicContext, { apiKey: "test-key" }),
		completions: await capturePayload(completions, completionsModel, completionsContext, { apiKey: "test-key" }),
		googleContents: await googleContents(googleContext),
	},
	auth: {
		anthropicBom: await capturePayload(anthropic, anthropicModel, userHi, { headers: { authorization: BOM } }),
		anthropicNel: await capturePayload(anthropic, anthropicModel, userHi, { headers: { "x-api-key": NEL } }),
		completionsBom: await capturePayload(completions, completionsModel, userHi, { headers: { authorization: BOM } }),
		completionsNel: await capturePayload(completions, completionsModel, userHi, { headers: { authorization: NEL } }),
	},
	streams,
	transform,
	grammar,
	streamingJson,
	retryAfter,
};
// Auth outcomes keep only the verdict: the payload itself is pinned elsewhere.
for (const [key, value] of Object.entries(out.auth)) {
	out.auth[key as keyof typeof out.auth] = "payload" in value ? { payload: true } : value;
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
