// Captures how pi's google-generative-ai adapter ends a stream that carries no
// finish reason (upstream 117eac3a0, #12330): a blocked prompt reports the
// block — formatGooglePromptBlockError's reason, message and safety ratings —
// and is not retryable; any other such stream names the chunks it received
// and stays retryable. It is the oracle behind
// TestGooglePromptBlockVersusIncompleteStream.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> google-prompt-block-117eac3a0.json 117eac3a0
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai`)
// and a node_modules resolving its dependencies (the npm 1.6.0 build's, with
// the generated src/providers/data catalogs copied from its dist). 117eac3a0
// is after the 1.6.0 release, so this is a src capture: re-verify it against
// the first build that ships it (the BUILD wins).
//
// Each row's SSE body is served through options.fetch; the first two are
// upstream's google-stream-errors.test.ts, the rest probe the JavaScript
// reading of mistyped feedback fields.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = (file: string) => import(pathToFileURL(path.join(extraction, "packages/ai/src", file)).href);
const { streamSimple } = await src("api/google-generative-ai.ts");
const { isRetryableAssistantError } = await src("utils/retry.ts");
const { normalizeContext } = await src("utils/transcript.ts");

const context = normalizeContext({ messages: [{ role: "user", content: "hello", timestamp: 1 }] });
const model = {
	id: "test-model",
	name: "Test Model",
	api: "google-generative-ai",
	provider: "test-provider",
	baseUrl: "https://upstream.test/v1",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 10_000,
	maxTokens: 1_000,
};
const sse = (...chunks: unknown[]) => chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("");
const text = (t: string, finishReason?: string) => ({
	candidates: [{ content: { parts: [{ text: t }] }, ...(finishReason ? { finishReason } : {}) }],
});
const block = (feedback: unknown) => ({ promptFeedback: feedback });
const harm = [{ category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", probability: "HIGH" }];

const rows: [string, string][] = [
	["blocked prompt", sse(block({ blockReason: "PROHIBITED_CONTENT", safetyRatings: harm }))],
	["empty stream", ""],
	["one chunk, no finish reason", sse(text("hi"))],
	["two chunks, no finish reason", sse(text("hi"), text(" there"))],
	[
		"message, several ratings, some without a category or probability",
		sse(
			block({
				blockReason: "SAFETY",
				blockReasonMessage: "Blocked by policy",
				safetyRatings: [
					{ category: "HARM_CATEGORY_HATE_SPEECH", probability: "LOW" },
					{ probability: "HIGH" },
					{ category: "HARM_CATEGORY_HARASSMENT" },
					"x",
					7,
				],
			}),
		),
	],
	["empty ratings and an empty message", sse(block({ blockReason: "OTHER", blockReasonMessage: "", safetyRatings: [] }))],
	["no rating with a category", sse(block({ blockReason: "OTHER", safetyRatings: [{ probability: "HIGH" }] }))],
	[
		"mistyped values",
		sse(block({ blockReason: 3, blockReasonMessage: true, safetyRatings: [{ category: ["a", "b"], probability: 0 }] })),
	],
	["two blocks", sse(block({ blockReason: "FIRST" }), block({ blockReason: "SECOND" }))],
	["falsy blockReason", sse(block({ blockReason: "", safetyRatings: [{ category: "C" }] }))],
	["null promptFeedback", sse(block(null))],
	["block, then a finish reason", sse(block({ blockReason: "SAFETY" }), text("ok", "STOP"))],
	["block among text chunks", sse(text("a"), block({ blockReason: "SAFETY" }), text("b"))],
	["null rating", sse(block({ blockReason: "SAFETY", safetyRatings: [null] }))],
	["string ratings", sse(block({ blockReason: "SAFETY", safetyRatings: "HIGH" }))],
	["object ratings without a length", sse(block({ blockReason: "SAFETY", safetyRatings: { category: "C" } }))],
	["object ratings with a length", sse(block({ blockReason: "SAFETY", safetyRatings: { length: 1 } }))],
	["object blockReason", sse(block({ blockReason: { a: 1 } }))],
	["object message", sse(block({ blockReason: "SAFETY", blockReasonMessage: { a: 1 } }))],
	["array message", sse(block({ blockReason: "SAFETY", blockReasonMessage: [1, null, "x"] }))],
	["unstringifiable category", sse(block({ blockReason: "SAFETY", safetyRatings: [{ category: { toString: 1 } }] }))],
	["unstringifiable message", sse(block({ blockReason: "SAFETY", blockReasonMessage: { toString: 1 } }))],
];

const out = [];
for (const [name, body] of rows) {
	const fetch = async () => new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
	const result = await streamSimple(model, context, { apiKey: "test-key", fetch }).result();
	out.push({
		name,
		sse: body,
		stopReason: result.stopReason,
		errorMessage: result.errorMessage ?? null,
		retryable: isRetryableAssistantError(result),
	});
}
fs.writeFileSync(outFile, `${JSON.stringify({ sha, rows: out }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${out.length} rows`);
