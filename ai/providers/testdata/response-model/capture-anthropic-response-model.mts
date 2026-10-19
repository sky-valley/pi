// Captures the assistant messages real pi produces when an Anthropic stream
// reports a model other than the requested one — the oracle behind
// TestAnthropicResponseModel* in this package (upstream 1283afd0d).
//
//   node --experimental-strip-types capture-anthropic-response-model.mts <extraction> <out.json> <sha>
//   e.g. ... capture-anthropic-response-model.mts <dir> anthropic-response-model-dc939913f.json dc939913f
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone), a node_modules resolving pi-ai's dependencies (the npm
// build's), and packages/ai/src/providers/data copied from that npm build's
// dist/providers/data: the generated catalog values are not in git, and the
// suite reads claude-opus-5 through compat.ts getModel.
//
// RE-VERIFIED against the build on 2026-09-20. 0.85.1 predated 1283afd0d, so
// the first capture was src-only and owed a re-check against the first build
// that shipped it. 0.86.1 does, and re-capturing at the f5c946480 pin — whose
// api/anthropic-messages.ts, api/transform-messages.ts and compat.ts are
// identical to the v0.86.1 tag's — reproduced every behavioural entry byte for
// byte. Only `model` moved, and only because the catalog did: claude-opus-5
// gained promptCache and two mid-convo compat keys.
//
// Re-captured at 95fbc0499 against the 0.87.0 build on 2026-09-22. The three
// source files are unchanged since f5c946480; again only `model` moved
// (claude-opus-5 gained inputLimits from f5c946480's generator half). The
// extraction also needs providers/data/.manifest.json — copy the directory
// with `cp -R <dist>/providers/data/. <dest>`, not a *.json glob.
//
// Re-captured at d86654abb (v0.99.1) against the 0.99.1 build on 2026-09-30.
// anthropic-messages.ts moved in between (the Claude Code version, provider
// stream events, 1-hour cache writes on deltas, all ported), yet every
// behavioural entry came back byte-identical. Only `model` moved: the catalog
// now types every chat model, so claude-opus-5 gained "type": "chat".
//
// Re-captured for the 1.3.0 regen on 2026-10-19, still at d86654abb's source
// but with dist/providers/data from the 0.99.2 build and anthropic.json from
// the 1.3.0 build. With 0.99.2's anthropic.json that extraction reproduces the
// previous file byte for byte; with 1.3.0's only `model` moves (dc939913f's
// generator half drops claude-opus-5's `off: null`). The source at v1.3.0
// (71023aec1) moves behavioural entries too (durationMs, where
// providerThinkingLevel is recorded, a dropped thinking text), and those are
// ports of their own: whichever lands them re-captures at its sha.
//
// Re-captured at f284a2460 (the response timing, 36a686ee8 + 3ba22ce17 +
// f284a2460) on 2026-10-19 with the 1.3.0 build's node_modules and the whole
// of its dist/providers/data. Against the d86654abb file only durationMs moved:
// every streamed assistant message, and the replayed copies of one, now carry
// it. dc939913f (where providerThinkingLevel is recorded) and 911309429 (a
// dropped thinking text) come later and re-capture with their own ports.
//
// Re-captured at dc939913f on 2026-10-19 (same extraction recipe). Values are
// unchanged against dc939913f^, which reproduces the f284a2460 file: only the
// KEY ORDER moved — providerThinkingLevel is assigned from the built params
// after the reply object exists, so it now follows stopReason instead of
// model.
//
// `relabeled` and `fallbackCost` are the two cases 1283afd0d added to
// packages/ai/test/anthropic-sse-parsing.test.ts, with the suite's model,
// client and SSE fixture; every other entry is an edge case of this port's,
// commented with the rule it pins. Streamed assistant timestamps are Date.now()
// and are dropped.
import path from "node:path";
import fs from "node:fs";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-anthropic-response-model.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { stream: streamAnthropic } = await load("api/anthropic-messages.ts");
const { transformMessages } = await load("api/transform-messages.ts");
const { getModel, normalizeContext } = await load("compat.ts");

type SseEvent = { event: string; data: string };

function sseBody(events: SseEvent[]): string {
	return events.map(({ event, data }) => `event: ${event}\ndata: ${data}\n`).join("\n");
}

// The suite's createSseResponse / createFakeAnthropicClient.
function createSseResponse(events: SseEvent[]): Response {
	return new Response(sseBody(events), { status: 200, headers: { "content-type": "text/event-stream" } });
}

function createFakeAnthropicClient(response: Response): any {
	return { beta: { messages: { create: () => ({ asResponse: async () => response }) } } };
}

type ResponseContentBlock = { type: "thinking"; thinking: string; signature: string } | { type: "text"; text: string };

function messageStart(model: string | undefined) {
	return {
		event: "message_start",
		data: JSON.stringify({
			type: "message_start",
			message: { id: "msg_response_model", model, usage: { input_tokens: 100, output_tokens: 0 } },
		}),
	};
}

// The suite's createResponseModelSseResponse, generalised to any number of
// message_start events (one per entry of `models`; undefined omits the field).
function createResponseModelSseResponse(
	models: Array<string | undefined>,
	contentBlock: ResponseContentBlock,
): Response {
	return createSseResponse([
		...models.map(messageStart),
		{
			event: "content_block_start",
			data: JSON.stringify({ type: "content_block_start", index: 0, content_block: contentBlock }),
		},
		{ event: "content_block_stop", data: JSON.stringify({ type: "content_block_stop", index: 0 }) },
		{
			event: "message_delta",
			data: JSON.stringify({
				type: "message_delta",
				delta: { stop_reason: "end_turn" },
				usage: { input_tokens: 100, output_tokens: 20 },
			}),
		},
		{ event: "message_stop", data: JSON.stringify({ type: "message_stop" }) },
	]);
}

// Streamed assistant timestamps are Date.now() and are dropped; durationMs
// (upstream 36a686ee8) is a monotonic measurement, so the capture records only
// that it is there and a number.
function withoutTimestamp(message: any) {
	if (message.role !== "assistant") return JSON.parse(JSON.stringify(message));
	const { timestamp: _timestamp, ...rest } = message;
	if (typeof rest.durationMs === "number") rest.durationMs = "number";
	return JSON.parse(JSON.stringify(rest));
}

async function run(model: any, models: Array<string | undefined>, block: ResponseContentBlock) {
	const context = normalizeContext({ messages: [{ role: "user", content: "Hello", timestamp: 1 }] });
	const message = await streamAnthropic(model, context, {
		client: createFakeAnthropicClient(createResponseModelSseResponse(models, block)),
	}).result();
	return { context, message };
}

function contentBlockStart(index: number, contentBlock: unknown): SseEvent {
	return {
		event: "content_block_start",
		data: JSON.stringify({ type: "content_block_start", index, content_block: contentBlock }),
	};
}

function textDelta(index: number, text: string): SseEvent {
	return {
		event: "content_block_delta",
		data: JSON.stringify({ type: "content_block_delta", index, delta: { type: "text_delta", text } }),
	};
}

function contentBlockStop(index: number): SseEvent {
	return { event: "content_block_stop", data: JSON.stringify({ type: "content_block_stop", index }) };
}

function messageDelta(stopReason: string): SseEvent {
	return {
		event: "message_delta",
		data: JSON.stringify({
			type: "message_delta",
			delta: { stop_reason: stopReason },
			usage: { input_tokens: 100, output_tokens: 20 },
		}),
	};
}

const messageStop: SseEvent = { event: "message_stop", data: JSON.stringify({ type: "message_stop" }) };

// Streams `events` on `model` and records, for every event pi pushes, its type
// and the model and responseModel of the message it carries (partial, message or
// error) AS THEY STOOD WHEN IT WAS PUSHED. pi pushes its one live output object,
// which later events keep mutating, so the record is taken inside push rather
// than when a consumer reads the event; the port pushes a clone per event, which
// is that same snapshot. push is replaced before the stream's first await
// resolves, so no event escapes it. `sse` is the exact body streamed, which the
// Go test replays.
async function runRecorded(model: any, events: SseEvent[]) {
	const context = normalizeContext({ messages: [{ role: "user", content: "Hello", timestamp: 1 }] });
	const stream = streamAnthropic(model, context, { client: createFakeAnthropicClient(createSseResponse(events)) });
	const pushed: Array<{ type: string; model: string; responseModel?: string }> = [];
	const push = stream.push.bind(stream);
	stream.push = (event: any) => {
		const carried = event.partial ?? event.message ?? event.error;
		pushed.push({ type: event.type, model: carried.model, responseModel: carried.responseModel });
		push(event);
	};
	const message = await stream.result();
	return { sse: sseBody(events), events: JSON.parse(JSON.stringify(pushed)), message: withoutTimestamp(message) };
}

const signedThinking: ResponseContentBlock = { type: "thinking", thinking: "reasoning", signature: "signature" };
const doneText: ResponseContentBlock = { type: "text", text: "done" };

const opus = getModel("anthropic", "claude-opus-5");
// The suite's fallback-cost model: claude-opus-5 whose compat is replaced by a
// priced allowedFallbackModels list.
const fallbackModel = "fallback-model";
const pricedOpus = {
	...opus,
	compat: {
		allowedFallbackModels: [
			{ provider: "anthropic", model: fallbackModel, cost: { input: 3, output: 5, cacheRead: 0, cacheWrite: 0 } },
		],
	},
};

const relabeled = await run(opus, ["kimi-for-coding"], signedThinking);

const out = {
	sha,
	model: JSON.parse(JSON.stringify(opus)),
	// 'keeps signed thinking replayable when a proxy relabels the model'.
	relabeled: {
		message: withoutTimestamp(relabeled.message),
		replayed: transformMessages([...relabeled.context.messages, relabeled.message], opus).map(withoutTimestamp),
		// The replay gate keys on the REQUESTED id: replaying into a model whose id
		// is the relabel is a cross-model replay, so the signed thinking degrades
		// to text.
		replayedIntoServedId: transformMessages([...relabeled.context.messages, relabeled.message], {
			...opus,
			id: "kimi-for-coding",
		}).map(withoutTimestamp),
	},
	// 'uses a returned fallback model for cost attribution'.
	fallbackCost: withoutTimestamp((await run(pricedOpus, [fallbackModel], doneText)).message),
	// A stream served by the requested model carries no responseModel.
	sameModel: withoutTimestamp((await run(pricedOpus, ["claude-opus-5"], doneText)).message),
	// A message_start without a model is a served model that differs: no
	// responseModel survives JSON (undefined), the requested model stays, and no
	// fallback pricing is found.
	missingModel: withoutTimestamp((await run(pricedOpus, [undefined], doneText)).message),
	// A second message_start naming the requested model leaves the first one's
	// responseModel standing, while its pricing drops back to the requested rates.
	relabeledThenRequested: withoutTimestamp(
		(await run(pricedOpus, [fallbackModel, "claude-opus-5"], doneText)).message,
	),
	// A second message_start naming a different model replaces responseModel and
	// is priced at its own fallback rates.
	requestedThenFallback: withoutTimestamp(
		(await run(pricedOpus, ["claude-opus-5", fallbackModel], doneText)).message,
	),
	// A later message_start without a model clears the responseModel an earlier
	// one set (pi assigns undefined).
	fallbackThenMissing: withoutTimestamp((await run(pricedOpus, [fallbackModel, undefined], doneText)).message),
	// Every event after message_start carries responseModel on its partial: pi
	// records it on the live output before any content event is pushed.
	relabeledEvents: await runRecorded(opus, [
		messageStart("kimi-for-coding"),
		contentBlockStart(0, { type: "text", text: "" }),
		textDelta(0, "done"),
		contentBlockStop(0),
		messageDelta("end_turn"),
		messageStop,
	]),
	// An errored stream keeps the requested model and the relabel: pi's catch sets
	// only stopReason and errorMessage on the output message_start already
	// relabeled. A fallback block after content is thrown mid-stream...
	midOutputFallbackError: await runRecorded(opus, [
		messageStart("kimi-for-coding"),
		contentBlockStart(0, doneText),
		contentBlockStart(1, { type: "fallback" }),
		contentBlockStop(0),
		messageDelta("end_turn"),
		messageStop,
	]),
	// ...and a refusal stop reason is thrown once the stream has completed.
	stopReasonErrorRelabel: await runRecorded(opus, [
		messageStart("kimi-for-coding"),
		contentBlockStart(0, doneText),
		contentBlockStop(0),
		messageDelta("refusal"),
		messageStop,
	]),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
