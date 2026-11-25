// Captures which HTTP responses pi's SDK-based adapters hand onResponse since
// upstream 8a57d42a5 (#12376) — every attempt's, error responses included —
// the oracle behind provider_error_response_test.go in this package.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> provider-error-response-8a57d42a5.json 8a57d42a5
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone (prefix kept) and a node_modules resolving pi-ai's
// dependencies at the versions the sha's package-lock.json locks: the npm
// 1.6.0 build's serves for @anthropic-ai/sdk 0.129.0 and openai 7.59.0, and
// 8a57d42a5 locks @google/genai 3.6.1 (`npm pack @google/genai@3.6.1`, its
// sha512 checked against the lockfile, unpacked at
// packages/ai/node_modules/@google/genai). The SDK versions it resolved are
// recorded in the output. Every reply comes from a loopback server, so
// nothing needs a key or the network.
//
//   attempts   upstream's provider-error-response.test.ts, widened: the
//              anthropic-messages, openai-completions, openai-responses and
//              google-generative-ai adapters (azure-openai-responses has no Go
//              base), each answered with a reply sequence (the last repeating):
//              a 429 on every attempt with maxRetries 1; a 400, which is not
//              retried, with maxRetries 2; a 503 and then a 200 stream with
//              maxRetries 1; and a 429 whose onResponse throws, with
//              maxRetries 2. Each row records the requests the server got,
//              every onResponse call — its status, the error replies' own
//              headers (content-type, retry-after, x-request-id) as the record
//              holds them, and whether the record was empty — and how the
//              stream ended.
//   hooks      openai-stream/capture.mts's non-2xx hooks rows, re-run here as
//              that capture runs them (its own self-checks no longer pass over
//              the lockfile's openai 7.59.0): the onResponse calls, stopReason
//              and errorMessage of a 400, of a 400 whose onResponse throws, and
//              of a custom fetch's null-body 500.
import fs from "node:fs";
import http from "node:http";
import type net from "node:net";
import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const anthropic = await load("api/anthropic-messages.ts");
const completions = await load("api/openai-completions.ts");
const responses = await load("api/openai-responses.ts");
const google = await load("api/google-generative-ai.ts");
const { normalizeContext } = await load("utils/transcript.ts");

const require = createRequire(path.join(src, "api/anthropic-messages.ts"));
function sdkVersion(name: string): string {
	let dir = path.dirname(require.resolve(name));
	while (!fs.existsSync(path.join(dir, "package.json")) || JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).name !== name) {
		dir = path.dirname(dir);
	}
	return JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version;
}

// ---- the loopback server ---------------------------------------------------

type Reply = { status: number; contentType: string; body: string; headers?: Record<string, string> };
const routes = new Map<string, { replies: Reply[]; requests: number }>();
let nextRoute = 0;
const server = http.createServer((req, res) => {
	req.resume();
	req.on("end", () => {
		const entry = routes.get((req.url ?? "").split("/")[1]);
		if (!entry) {
			res.writeHead(404).end();
			return;
		}
		const reply = entry.replies[Math.min(entry.requests, entry.replies.length - 1)];
		entry.requests++;
		res.writeHead(reply.status, { "content-type": reply.contentType, ...reply.headers });
		res.end(reply.body);
	});
});
await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
const { port } = server.address() as net.AddressInfo;

// route registers a reply sequence and returns the base URL that reaches it.
function route(replies: Reply[]): { baseUrl: string; requests: () => number } {
	const name = `r${nextRoute++}`;
	const entry = { replies, requests: 0 };
	routes.set(name, entry);
	return { baseUrl: `http://127.0.0.1:${port}/${name}`, requests: () => entry.requests };
}

// ---- attempts ----------------------------------------------------------------

const errorHeaders = { "retry-after": "0", "x-request-id": "req-1" };
const errorReply = (status: number): Reply => ({
	status,
	contentType: "application/json",
	body: JSON.stringify({ error: { message: "rate limited", type: "rate_limit_error", code: status } }),
	headers: errorHeaders,
});
const ev = (event: string, data: object) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
const okBodies: Record<string, string> = {
	"anthropic-messages":
		ev("message_start", { type: "message_start", message: { id: "msg_1", usage: { input_tokens: 1, output_tokens: 0 } } }) +
		ev("content_block_start", { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }) +
		ev("content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "ok" } }) +
		ev("content_block_stop", { type: "content_block_stop", index: 0 }) +
		ev("message_delta", { type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 1 } }) +
		ev("message_stop", { type: "message_stop" }),
	"openai-completions":
		`data: ${JSON.stringify({ id: "c1", choices: [{ index: 0, delta: { content: "ok" } }] })}\n\n` +
		`data: ${JSON.stringify({ id: "c1", choices: [{ index: 0, delta: {}, finish_reason: "stop" }] })}\n\ndata: [DONE]\n\n`,
	"openai-responses":
		`data: ${JSON.stringify({ type: "response.created", response: { id: "resp_1" } })}\n\n` +
		`data: ${JSON.stringify({ type: "response.completed", response: { id: "resp_1", status: "completed", usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } })}\n\n`,
	"google-generative-ai": `data: ${JSON.stringify({ candidates: [{ content: { role: "model", parts: [{ text: "ok" }] }, finishReason: "STOP" }] })}\n\n`,
};
const okReply = (api: string): Reply => ({ status: 200, contentType: "text/event-stream", body: okBodies[api] });

const adapters: Record<string, any> = {
	"anthropic-messages": anthropic,
	"openai-completions": completions,
	"openai-responses": responses,
	"google-generative-ai": google,
};
type Case = { replies: (api: string) => Reply[]; maxRetries: number; throws?: boolean };
const cases: Record<string, Case> = {
	"429-every-attempt": { replies: () => [errorReply(429)], maxRetries: 1 },
	"400-not-retried": { replies: () => [errorReply(400)], maxRetries: 2 },
	"503-then-ok": { replies: (api) => [errorReply(503), okReply(api)], maxRetries: 1 },
	"on-response-throws": { replies: () => [errorReply(429)], maxRetries: 2, throws: true },
};
const context = normalizeContext({ messages: [{ role: "user", content: "hi", timestamp: 0 }] });
const pick = (headers: Record<string, string>) =>
	Object.fromEntries(["content-type", "retry-after", "x-request-id"].filter((k) => k in headers).map((k) => [k, headers[k]]));

const attempts: Record<string, unknown> = {};
for (const [api, adapter] of Object.entries(adapters)) {
	for (const [name, c] of Object.entries(cases)) {
		const replies = c.replies(api);
		const { baseUrl, requests } = route(replies);
		const model = {
			id: "test-model",
			name: "Test Model",
			api,
			provider: "test",
			baseUrl,
			reasoning: false,
			input: ["text"],
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow: 1000,
			maxTokens: 100,
		};
		const calls: unknown[] = [];
		let sameModel = true;
		const s = adapter.stream(model, context, {
			apiKey: "test-key",
			maxRetries: c.maxRetries,
			onResponse: (response: { status: number; headers: Record<string, string> }, responseModel: unknown) => {
				sameModel &&= responseModel === model;
				calls.push({
					status: response.status,
					headers: pick(response.headers),
					empty: Object.keys(response.headers).length === 0,
				});
				if (c.throws) throw new Error("response veto");
			},
		});
		const events: string[] = [];
		for await (const event of s) events.push(event.type);
		const final = await s.result();
		attempts[`${api}/${name}`] = {
			api,
			replies,
			maxRetries: c.maxRetries,
			...(c.throws ? { throws: true } : {}),
			requests: requests(),
			calls,
			sameModel,
			events,
			stopReason: final.stopReason,
			errorMessage: final.errorMessage ?? "",
		};
	}
}

// ---- openai-stream's non-2xx hooks rows ------------------------------------

const hookModels: Record<string, [any, Record<string, unknown>]> = {
	completions: [
		completions,
		{
			id: "openrouter/auto",
			name: "OpenRouter Auto",
			api: "openai-completions",
			provider: "openrouter",
			reasoning: false,
			input: ["text"],
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow: 200000,
			maxTokens: 8192,
		},
	],
	responses: [
		responses,
		{
			id: "gpt-5-mini",
			name: "GPT-5 Mini",
			api: "openai-responses",
			provider: "openai",
			reasoning: true,
			input: ["text"],
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow: 400000,
			maxTokens: 128000,
		},
	],
};
const hookContext = normalizeContext({ systemPrompt: "", messages: [{ role: "user", content: "hi", timestamp: 1 }] });
const badRequest = JSON.stringify({ error: { message: "bad request" } });
type HookCase = { status: number; body: string; failOnResponse?: boolean; nullBodyStatus?: number };
const hookCases: Record<string, HookCase> = {
	"http-400": { status: 400, body: badRequest },
	"http-400-on-response-throws": { status: 400, body: badRequest, failOnResponse: true },
	"custom-fetch-null-body-500": { status: 500, body: "", nullBodyStatus: 500 },
};
const hooks: Record<string, unknown> = {};
for (const [adapter, [api, baseModel]] of Object.entries(hookModels)) {
	for (const [name, h] of Object.entries(hookCases)) {
		const { baseUrl } = route([{ status: h.status, contentType: "application/json", body: h.body }]);
		const calls: number[] = [];
		const final = await api
			.streamSimple({ ...baseModel, baseUrl: `${baseUrl}/v1` }, hookContext, {
				apiKey: "k",
				...(h.nullBodyStatus === undefined ? {} : { fetch: async () => new Response(null, { status: h.nullBodyStatus }) }),
				onResponse: (response: { status: number }) => {
					calls.push(response.status);
					if (h.failOnResponse) throw new Error("response veto");
				},
			})
			.result();
		hooks[`${adapter}/${name}`] = {
			onResponseStatuses: calls,
			stopReason: final.stopReason,
			errorMessage: final.errorMessage ?? "",
		};
	}
}

server.close();
const out = {
	sha,
	node: process.version,
	sdks: Object.fromEntries(["@anthropic-ai/sdk", "openai", "@google/genai"].map((name) => [name, sdkVersion(name)])),
	attempts,
	hooks,
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`captured ${Object.keys(attempts).length} attempts and ${Object.keys(hooks).length} hooks rows at ${sha} -> ${outFile}`);
