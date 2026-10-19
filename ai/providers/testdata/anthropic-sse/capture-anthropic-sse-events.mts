// Captures how real pi's anthropic-messages adapter iterates an SSE body — the
// events onProviderStreamEvent observes, the events the stream pushes, the
// final message, and the exact error a malformed event fails with. It is the
// oracle behind TestAnthropicSSEEventsMatchPi (upstream 002fc8385 plus the
// iterateAnthropicEvents parse-failure message).
//
//   node --experimental-strip-types capture-anthropic-sse-events.mts <extraction> <pi npm dir> <out.json> <sha>
//   e.g. ... capture-anthropic-sse-events.mts <dir> <npm-1.3.0 dir> anthropic-sse-events-87f9a3641.json 87f9a3641
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai |
// tar -x -C <dir>` from the upstream clone), with packages/ai/node_modules
// resolving pi-ai's dependencies. The npm build's node_modules serves once its
// copies match what <sha>'s package-lock.json locks; at 87f9a3641 that is
// @anthropic-ai/sdk 0.129.0 and partial-json 0.1.7, integrity-identical to
// the npm 1.3.0 build's (at 49681e1b7 it was 0.124.0, 0.87.1's). The adapter
// runs from SOURCE, so a row moves with the sha that moved it. The model is
// the npm build's MODELS entry the upstream suite uses
// (anthropic/claude-haiku-4-5).
//
// Every row streams through the upstream suite's fake client
// (anthropic-sse-parsing.test.ts createFakeAnthropicClient), so neither side
// needs a key or the network — except an oauth row, which needs pi to create
// its own client for an OAuth token: that client's fetch serves the body, and
// the Go test streams with the same token. Each row records:
//   sse        the exact body, which the Go test replays byte for byte (a
//              body that is not valid UTF-8 is sseBase64 instead);
//   observed   each value onProviderStreamEvent received, as JSON.stringify
//              text, so key order is part of the expectation;
//   pushed     the type of every event the stream pushed (null: no type);
//   message    stopReason, errorMessage, responseId, content and usage of the
//              result, and the type and details of each diagnostic (not its
//              timestamp), when it has any;
//   v8Cause    true when errorMessage embeds a V8 JSON.parse message, which
//              the port does not reproduce: only the text around it is pi's;
//   rawSeed    true when the message's content still holds a block member as
//              a non-string — the raw value content_block_start seeded it
//              with (the stream failed before a delta converted it), or a
//              number `+` made of it — which the port's string field cannot
//              hold: that member is not compared;
//   oneByte    what pi made of the same body read one byte per read, when
//              that differs from the whole body in one read (observed, pushed,
//              message and v8Cause as above): pi's reader splits lines per
//              read, so a "\r" that ends a read ends its line, and a "\r\n"
//              split across two reads is two line breaks.
// A row may make the observer throw ("observer boom") on the observed event at
// index throwAt, after aborting the request when abortFirst is set.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, npmDir, outFile, sha] = process.argv.slice(2);
if (!extraction || !npmDir || !outFile || !sha) {
	console.error(
		"usage: node --experimental-strip-types capture-anthropic-sse-events.mts <extraction> <pi npm dir> <out.json> <sha>",
	);
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const { stream: streamAnthropic } = await import(pathToFileURL(path.join(src, "api/anthropic-messages.ts")).href);
const { normalizeContext } = await import(pathToFileURL(path.join(src, "utils/transcript.ts")).href);
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const { MODELS } = await import(pathToFileURL(path.join(pkg, "dist/models.generated.js")).href);
const model = MODELS.anthropic["claude-haiku-4-5"];
if (!model) throw new Error("catalog has no anthropic/claude-haiku-4-5");

function createFakeAnthropicClient(body: string | Uint8Array | ReadableStream<Uint8Array>): any {
	const response = new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
	return { beta: { messages: { create: () => ({ asResponse: async () => response }) } } };
}

// A body that hands its reader one byte per read.
function oneBytePerRead(sse: string | Uint8Array): ReadableStream<Uint8Array> {
	const bytes = typeof sse === "string" ? new TextEncoder().encode(sse) : sse;
	let i = 0;
	return new ReadableStream({
		pull(c) {
			if (i < bytes.length) c.enqueue(bytes.slice(i, ++i));
			else c.close();
		},
	});
}

// A framed event, as the suite's createSseResponse writes one.
const ev = (event: string, data: string) => `event: ${event}\ndata: ${data}\n`;
const frames = (...events: string[]) => events.join("\n");

// The suite's minimalAnthropicEvents.
const messageStart = ev(
	"message_start",
	JSON.stringify({
		type: "message_start",
		message: {
			id: "msg_test",
			usage: { input_tokens: 12, output_tokens: 0, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
		},
	}),
);
const blockStart = ev(
	"content_block_start",
	JSON.stringify({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }),
);
const textDelta = (text: string) =>
	ev("content_block_delta", JSON.stringify({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text } }));
const blockStop = ev("content_block_stop", JSON.stringify({ type: "content_block_stop", index: 0 }));
const messageDelta = ev(
	"message_delta",
	JSON.stringify({
		type: "message_delta",
		delta: { stop_reason: "end_turn" },
		usage: { input_tokens: 12, output_tokens: 5, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
	}),
);
const messageStop = ev("message_stop", JSON.stringify({ type: "message_stop" }));
const minimal = [messageStart, blockStart, textDelta("Hello"), blockStop, messageDelta, messageStop];

// A message_start and a message_delta whose input_transformations are the
// given JSON text (written as text: JSON.stringify cannot write 1e400).
const startTransforming = (transformations: string) =>
	ev(
		"message_start",
		`{"type":"message_start","message":{"id":"msg_test","input_transformations":${transformations},"usage":{"input_tokens":12,"output_tokens":0}}}`,
	);
const deltaTransforming = (transformations: string, stopReason = "end_turn") =>
	ev(
		"message_delta",
		`{"type":"message_delta","input_transformations":${transformations},"delta":{"stop_reason":"${stopReason}"},"usage":{"output_tokens":5}}`,
	);

type Case = {
	name: string;
	sse: string | Uint8Array;
	v8Cause?: boolean;
	throwAt?: number;
	abortFirst?: boolean;
	oauth?: boolean;
	rawSeed?: boolean;
};
const B = String.fromCharCode(92); // a backslash, spelled so no tool decodes an escape
// U+2028 and U+2029, spelled so no tool decodes an escape: JSON.stringify writes
// both literally, as it does <, > and &.
const LS = String.fromCharCode(0x2028);
const PS = String.fromCharCode(0x2029);
// The UTF-8 byte-order mark, and a body's bytes from text and byte runs.
const BOM = String.fromCharCode(0xfeff);
const bytesOf = (...parts: Array<string | number[]>) =>
	new Uint8Array(Buffer.concat(parts.map((p) => (typeof p === "string" ? Buffer.from(p, "utf8") : Buffer.from(p)))));
const cases: Case[] = [
	// 'forwards parsed provider stream events in order'.
	{ name: "forwardsInOrder", sse: frames(...minimal) },
	// The filter is on the SSE event NAME: ping, an unknown name, a nameless
	// data-only frame and comment-only blocks are never observed, whatever their
	// JSON `type` says.
	{
		name: "skipsPingUnknownAndNameless",
		sse: frames(
			ev("ping", JSON.stringify({ type: "ping" })),
			messageStart,
			": keepalive\n",
			ev("content_block_delta_v2", JSON.stringify({ type: "content_block_delta", index: 0 })),
			`data: ${JSON.stringify({ type: "message_start", message: { id: "nameless" } })}\n`,
			blockStart,
			textDelta("Hello"),
			blockStop,
			messageDelta,
			messageStop,
		),
	},
	// A named event whose data is JSON but not an object is observed, and then
	// matches no branch (its `.type` is undefined): the stream carries on.
	{
		name: "nonObjectEventsAreObservedAndIgnored",
		sse: frames(
			messageStart,
			blockStart,
			ev("content_block_delta", "5"),
			ev("content_block_delta", "[1,2]"),
			ev("content_block_delta", '"x"'),
			ev("content_block_delta", "true"),
			textDelta("Hello"),
			blockStop,
			messageDelta,
			messageStop,
		),
	},
	// parseJsonWithRepair: an invalid escape and a raw tab inside a string are
	// repaired, and the observer sees the REPAIRED value.
	{
		name: "repairedEventIsObservedRepaired",
		sse: frames(
			messageStart,
			blockStart,
			ev(
				"content_block_delta",
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a${B}qb\tc"}}`,
			),
			blockStop,
			messageDelta,
			messageStop,
		),
	},
	// `data: null` parses, and then iterateAnthropicEvents reads `.type` of it
	// inside its try: the stream fails with that TypeError wrapped, and the
	// observer never sees null.
	{ name: "nullMessageStartFails", sse: frames(ev("message_start", "null")) },
	{ name: "nullAfterStartFails", sse: frames(messageStart, blockStart, ev("content_block_delta", "null"), blockStop) },
	// raw= lists every non-empty line of the event, comments included. A block
	// with neither an event name nor data is not flushed, so its lines stay in
	// the buffer and lead the next event's raw...
	{
		name: "rawKeepsUnflushedLines",
		sse: ": keepalive\n\nid: 3\nretry: 5\n\n" + frames(ev("message_start", "null")),
	},
	// ...while a flushed event, even one that is then skipped, resets it.
	{
		name: "rawResetsAfterSkippedEvent",
		sse: frames(ev("ping", "{}"), ev("message_start", "null")),
	},
	// Lines split on a lone CR too.
	{ name: "rawSplitsOnCR", sse: "event: message_start\rdata: null\r\r" },
	// A "\r\n" is one line break within a read. Read one byte per read, each
	// "\r" ends its line at the end of its read and the "\n" that starts the
	// next read ends an empty one, which flushes the event before its data
	// line: its oneByte outcome is a parse failure of empty data.
	{ name: "crlfLineEndings", sse: frames(...minimal).replaceAll("\n", "\r\n") },
	// A JSON syntax error: the message embeds V8's JSON.parse text, then data=
	// (the data lines joined with a newline) and raw= (the lines joined with a
	// literal backslash-n).
	{
		name: "syntaxErrorCarriesDataAndRaw",
		v8Cause: true,
		sse: messageStart + "\n" + ': comment line\nid: 7\nevent: content_block_delta\ndata: {"type":\ndata: oops\n\n',
	},
	// The last event is flushed at EOF without its blank line.
	{
		name: "syntaxErrorAtEOF",
		v8Cause: true,
		sse: messageStart + "\n" + ": c\nevent: message_start\ndata: nul",
	},
	// JSON.parse builds an ordinary object, which enumerates array-index keys
	// first, ascending, then the rest in wire order, at every depth: observed
	// shows it.
	{
		name: "observedKeysEnumerateIndicesFirst",
		sse: frames(
			messageStart,
			blockStart,
			ev(
				"content_block_delta",
				'{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello","1":"x"},"10":1,"2":2,"a":3}',
			),
			blockStop,
			messageDelta,
			messageStop,
		),
	},
	// JSON.parse reads a number past float64's range as Infinity (observed as
	// JSON.stringify's null), inside an event or as the whole event, and the
	// stream carries on.
	{
		name: "overflowNumbersAreObserved",
		sse: frames(
			messageStart,
			blockStart,
			ev("content_block_delta", "1e400"),
			textDelta("Hello"),
			blockStop,
			messageDelta,
			ev("message_stop", '{"type":"message_stop","big":1e400}'),
		),
	},
	// observed is JSON.stringify text: <, >, &, U+2028 and U+2029 are written as
	// themselves, however deep in the event they sit.
	{
		name: "observedTextIsNotHTMLEscaped",
		sse: frames(messageStart, blockStart, textDelta(`<b>&x> ${LS} ${PS} end`), blockStop, messageDelta, messageStop),
	},
	// pi reads each event's properties as whatever they hold, so a member of an
	// unexpected type never fails the event: a count written 5.0 is 5, a
	// cache_creation that is not an object has no 1h count, and a string index
	// is strictly unequal to every block's number, so its delta finds no block.
	// Every event is observed.
	{
		name: "mistypedMembersAreRead",
		sse: frames(
			messageStart,
			blockStart,
			ev("content_block_delta", '{"type":"content_block_delta","index":"0","delta":{"type":"text_delta","text":"x"}}'),
			textDelta("Hello"),
			blockStop,
			ev(
				"message_delta",
				'{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":5.0,"cache_creation":5}}',
			),
			messageStop,
		),
	},
	// A delta's text is appended as String() writes it: a missing text as
	// "undefined", 5 as "5". A tool block starts with the arguments its input
	// holds, which the stream's failure then leaves in the message.
	{
		name: "deltasAppendStringForms",
		sse: frames(
			messageStart,
			blockStart,
			ev("content_block_delta", '{"type":"content_block_delta","index":0,"delta":{"type":"text_delta"}}'),
			ev("content_block_delta", '{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":5}}'),
			blockStop,
			ev(
				"content_block_start",
				'{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"read","input":{"path":"a","2":"b"}}}',
			),
			ev("content_block_delta", "null"),
		),
	},
	// content_block_stop deletes the block's index, so a later event with no
	// index (undefined === undefined) finds that block: its partialJson, also
	// deleted, is appended to as "undefined", which parses to no arguments.
	{
		name: "deletedIndexMatchesAnEventWithoutOne",
		sse: frames(
			messageStart,
			ev("content_block_start", '{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"read"}}'),
			ev("content_block_delta", '{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\\"a\\":1}"}}'),
			ev("content_block_stop", '{"type":"content_block_stop","index":0}'),
			ev("content_block_delta", '{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":""}}'),
			ev("content_block_stop", '{"type":"content_block_stop"}'),
			messageDelta,
			messageStop,
		),
	},
	// message_start reads no reasoning breakdown; message_delta's does.
	{
		name: "reasoningOnlyFromMessageDelta",
		sse: frames(
			ev(
				"message_start",
				'{"type":"message_start","message":{"id":"msg_test","usage":{"input_tokens":12,"output_tokens":1,"output_tokens_details":{"thinking_tokens":1}}}}',
			),
			blockStart,
			textDelta("Hello"),
			blockStop,
			ev("message_delta", '{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}'),
			messageStop,
		),
	},
	// A stop reason that is not a string matches no case: its String() is in
	// the error. A refusal's explanation reaches the error through
	// `new Error(...)`, which takes String() of it too.
	{ name: "nonStringStopReasonIsUnhandled", sse: frames(messageStart, ev("message_delta", '{"type":"message_delta","delta":{"stop_reason":5}}')) },
	{
		name: "refusalExplanationIsStringified",
		sse: frames(
			messageStart,
			ev("message_delta", '{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"explanation":[1,2]}}}'),
			messageStop,
		),
	},
	// The explanation is kept when it is truthy, not when its String() is
	// non-empty: an empty array is truthy, so the error is its String(), "",
	// and not the "An unknown error occurred" an empty message falls back to.
	{
		name: "refusalExplanationTruthyButEmpty",
		sse: frames(
			messageStart,
			ev("message_delta", '{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"explanation":[]}}}'),
			messageStop,
		),
	},
	// A property read from a missing or null sub-object throws V8's TypeError,
	// in pi's read order: message.id before message.usage.input_tokens, the
	// block's type, the delta's type or stop_reason. What was assigned before
	// the throw (the response id) stays in the message.
	{ name: "messageStartWithoutUsageFails", sse: frames(ev("message_start", '{"type":"message_start","message":{"id":"m1"}}')) },
	{ name: "messageStartNullMessageFails", sse: frames(ev("message_start", '{"type":"message_start","message":null}')) },
	{ name: "messageStartWithoutMessageFails", sse: frames(ev("message_start", '{"type":"message_start"}')) },
	{ name: "messageStartScalarMessageFailsOnUsage", sse: frames(ev("message_start", '{"type":"message_start","message":5}')) },
	{ name: "contentBlockStartWithoutBlockFails", sse: frames(messageStart, ev("content_block_start", '{"type":"content_block_start","index":0}')) },
	{ name: "contentBlockDeltaWithoutDeltaFails", sse: frames(messageStart, blockStart, ev("content_block_delta", '{"type":"content_block_delta","index":0}')) },
	{ name: "messageDeltaWithoutDeltaFails", sse: frames(messageStart, ev("message_delta", '{"type":"message_delta","usage":{"output_tokens":5}}')) },
	// With an OAuth token, a tool_use block's name maps back to the current
	// tool it lowercases to (fromClaudeCodeName)...
	{
		name: "oauthToolNameMapsToACurrentTool",
		oauth: true,
		sse: frames(
			messageStart,
			ev("content_block_start", '{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"read","input":{}}}'),
			ev("content_block_stop", '{"type":"content_block_stop","index":0}'),
			ev("message_delta", '{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}'),
			messageStop,
		),
	},
	// ...and a name that is not a string makes its toLowerCase() throw V8's
	// TypeError, which fails the stream.
	{
		name: "oauthToolUseWithoutNameFails",
		oauth: true,
		sse: frames(messageStart, ev("content_block_start", '{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","input":{}}}')),
	},
	{
		name: "oauthToolUseNullNameFails",
		oauth: true,
		sse: frames(messageStart, ev("content_block_start", '{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":null,"input":{}}}')),
	},
	{
		name: "oauthToolUseNumericNameFails",
		oauth: true,
		sse: frames(messageStart, ev("content_block_start", '{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":5,"input":{}}}')),
	},
	// content_block_start seeds a block's text, thinking and signature with the
	// raw value (`?? ""`; a redacted block's data as it is), which the first
	// `+=` converts with String(). A signature_delta first does
	// `thinkingSignature ||= ""`-style `|| ""`, so a falsy seed (0, false, -0,
	// an absent or null data) is dropped; a truthy one is converted, and so is
	// a thinking seed however falsy.
	{
		name: "signatureDeltaDropsAFalsySeed",
		sse: frames(
			messageStart,
			...["0", "false", "-0", '""', "{}", "1", "true", '"s"'].flatMap((seed, i) => [
				ev("content_block_start", `{"type":"content_block_start","index":${i},"content_block":{"type":"thinking","signature":${seed}}}`),
				ev("content_block_delta", `{"type":"content_block_delta","index":${i},"delta":{"type":"signature_delta","signature":"sig"}}`),
				ev("content_block_stop", `{"type":"content_block_stop","index":${i}}`),
			]),
			ev("content_block_start", '{"type":"content_block_start","index":8,"content_block":{"type":"thinking","thinking":false,"signature":0}}'),
			ev("content_block_delta", '{"type":"content_block_delta","index":8,"delta":{"type":"thinking_delta","thinking":"x"}}'),
			ev("content_block_delta", '{"type":"content_block_delta","index":8,"delta":{"type":"signature_delta","signature":"a"}}'),
			ev("content_block_delta", '{"type":"content_block_delta","index":8,"delta":{"type":"signature_delta","signature":"b"}}'),
			ev("content_block_stop", '{"type":"content_block_stop","index":8}'),
			messageDelta,
			messageStop,
		),
	},
	{
		name: "redactedDataConvertsOnTheFirstSignatureDelta",
		sse: frames(
			messageStart,
			...['"data":5', '"data":[1]', '"data":true', '"data":{}', '"data":null', "", '"data":false', '"data":"d"'].flatMap((data, i) => [
				ev("content_block_start", `{"type":"content_block_start","index":${i},"content_block":{"type":"redacted_thinking"${data ? "," + data : ""}}}`),
				ev("content_block_delta", `{"type":"content_block_delta","index":${i},"delta":{"type":"signature_delta","signature":"sig"}}`),
				ev("content_block_stop", `{"type":"content_block_stop","index":${i}}`),
			]),
			messageDelta,
			messageStop,
		),
	},
	// A seed with no string form (an object with its own toString member) makes
	// the first `+=` onto it throw V8's TypeError, failing the stream — for a
	// text, a thinking, a signature and a redacted block's data alike.
	...[
		["textSeedWithNoStringFormFails", '"type":"text","text":{"toString":1}', '"type":"text_delta","text":"a"'],
		["thinkingSeedWithNoStringFormFails", '"type":"thinking","thinking":{"toString":1},"signature":"s"', '"type":"thinking_delta","thinking":"a"'],
		["signatureSeedWithNoStringFormFails", '"type":"thinking","signature":{"toString":1}', '"type":"signature_delta","signature":"a"'],
		["redactedDataWithNoStringFormFails", '"type":"redacted_thinking","data":{"toString":1}', '"type":"signature_delta","signature":"a"'],
	].map(([name, block, delta]) => ({
		name,
		rawSeed: true,
		sse: frames(
			messageStart,
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{${block}}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{${delta}}}`),
			blockStop,
			messageDelta,
			messageStop,
		),
	})),
	// A seed converts once: later deltas append to the string it became.
	{
		name: "aSeedConvertsOnce",
		sse: frames(
			messageStart,
			ev("content_block_start", '{"type":"content_block_start","index":0,"content_block":{"type":"text","text":5}}'),
			textDelta("a"),
			textDelta("b"),
			blockStop,
			messageDelta,
			messageStop,
		),
	},
	// `+=` is JavaScript's `+`: a seed that is a number or a boolean ADDS a
	// delta that is a number, a boolean, null or absent (undefined) — 5 + 3 is
	// 8, 5 + undefined NaN — and the member stays a number until a string
	// delta concatenates onto the sum's String(). A signature's `|| ""` drops a
	// sum that came to 0.
	{
		name: "numericSeedsAddNonStringDeltas",
		sse: frames(
			messageStart,
			...(
				[
					['{"type":"text","text":5}', ["3", "true", "null", '"x"'], "text_delta", "text"],
					['{"type":"text","text":true}', ["1", '"!"'], "text_delta", "text"],
					['{"type":"text","text":5}', [undefined, '"x"'], "text_delta", "text"],
					['{"type":"text","text":1e400}', ["-1e400", '"n"'], "text_delta", "text"],
					['{"type":"thinking","thinking":0.1,"signature":"sig"}', ["0.2", '"t"'], "thinking_delta", "thinking"],
					['{"type":"thinking","thinking":1e21,"signature":"sig"}', ["1", '"e"'], "thinking_delta", "thinking"],
					['{"type":"thinking","thinking":-0,"signature":"sig"}', ["0", '"z"'], "thinking_delta", "thinking"],
					['{"type":"thinking","thinking":"t","signature":5}', ["3", '"s"'], "signature_delta", "signature"],
					['{"type":"thinking","thinking":"t","signature":2}', ["-2", '"s"'], "signature_delta", "signature"],
					['{"type":"redacted_thinking","data":5}', ["3", '"s"'], "signature_delta", "signature"],
				] as Array<[string, Array<string | undefined>, string, string]>
			).flatMap(([block, deltas, deltaType, member], i) => [
				ev("content_block_start", `{"type":"content_block_start","index":${i},"content_block":${block}}`),
				...deltas.map((value) =>
					ev(
						"content_block_delta",
						`{"type":"content_block_delta","index":${i},"delta":{"type":"${deltaType}"${value === undefined ? "" : `,"${member}":${value}`}}}`,
					),
				),
				ev("content_block_stop", `{"type":"content_block_stop","index":${i}}`),
			]),
			messageDelta,
			messageStop,
		),
	},
	// A sum no string delta follows leaves the member a number, which the
	// port's string field cannot hold (rawSeed): the rest is compared.
	{
		name: "aNumericSumStaysANumber",
		rawSeed: true,
		sse: frames(
			messageStart,
			ev("content_block_start", '{"type":"content_block_start","index":0,"content_block":{"type":"text","text":5}}'),
			ev("content_block_delta", '{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":3}}'),
			blockStop,
			messageDelta,
			messageStop,
		),
	},
	// The body is decoded by a TextDecoder: a byte-order mark at the very start
	// is dropped, so the first event still reads...
	{ name: "leadingBOMIsDropped", sse: BOM + frames(...minimal) },
	// ...while one anywhere else is text: here it leads a field name, which is
	// then no field pi knows, so that event is never named and never read.
	{
		name: "bomAfterTheStartIsText",
		sse: frames(messageStart, BOM + blockStart, textDelta("Hello"), blockStop, messageDelta, messageStop),
	},
	// Invalid UTF-8 becomes one U+FFFD per maximal subpart: a truncated
	// sequence is one, an encoded surrogate, an overlong form and a stray byte
	// one per byte.
	{
		name: "invalidUTF8DecodesPerMaximalSubpart",
		sse: bytesOf(
			frames(messageStart, blockStart),
			'\nevent: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a',
			[0xe2, 0x82],
			"Z",
			[0xf0, 0x9f],
			"b",
			[0xed, 0xa0, 0x80],
			"c",
			[0xc0, 0xaf],
			"d",
			[0xff],
			'e"}}\n\n',
			frames(blockStop, messageDelta, messageStop),
		),
	},
	// input_transformations: any array replaces the list (Array.isArray is the
	// only guard), and on a turn that succeeds each entry becomes {type, path,
	// reason}, each `?? undefined`. A null entry makes that `.type` read throw
	// V8's TypeError, failing the stream — whether message_start or a
	// message_delta restating the list carried it.
	{
		name: "inputTransformationsNullEntryFails",
		sse: frames(startTransforming("[null]"), blockStart, textDelta("Hello"), blockStop, messageDelta, messageStop),
	},
	{
		name: "inputTransformationsDeltaRestatesWithANullEntry",
		sse: frames(
			startTransforming('[{"type":"a"}]'),
			blockStart,
			textDelta("Hello"),
			blockStop,
			deltaTransforming('[{"type":"b"},null]'),
			messageStop,
		),
	},
	// ...but a refused turn throws first, so its null entry is never read.
	{
		name: "inputTransformationsNullEntryOnARefusedTurn",
		sse: frames(startTransforming("[null]"), deltaTransforming("[null]", "refusal"), messageStop),
	},
	// A number past float64's range is Infinity to JSON.parse, not a failure:
	// the list it is in still replaces the last one, and the diagnostic writes
	// it null.
	{
		name: "inputTransformationsDeltaRestatesWithAnOverflow",
		sse: frames(
			startTransforming('[{"type":"a"}]'),
			blockStart,
			textDelta("Hello"),
			blockStop,
			deltaTransforming('[{"type":"b","path":1e400}]'),
			messageStop,
		),
	},
	{
		name: "inputTransformationsOverflowingEntry",
		sse: frames(startTransforming("[1e400]"), blockStart, textDelta("Hello"), blockStop, messageDelta, messageStop),
	},
	// Each entry is written {type, path, reason} in that order, each member as
	// JSON.parse made it (its own key order, JavaScript numbers); an entry
	// that is not an object, and a member that is null, contribute nothing,
	// while false and 0 are kept.
	{
		name: "inputTransformationsEntriesAsPiWritesThem",
		sse: frames(
			startTransforming(
				'[{"reason":{"z":1,"a":10.0},"type":"x","path":[1e3,-0],"extra":1},5,"s",true,[1],{},{"type":null,"path":false,"reason":0}]',
			),
			blockStart,
			textDelta("Hello"),
			blockStop,
			messageDelta,
			messageStop,
		),
	},
	// A throwing observer fails the stream with its message (pi awaits the
	// callback inside the adapter's try): on the first event...
	{ name: "observerThrowsOnFirstEvent", sse: frames(...minimal), throwAt: 0 },
	// ...mid-stream, before the event it was handed is applied...
	{ name: "observerThrowsMidStream", sse: frames(...minimal), throwAt: 2 },
	// ...and, when the request was aborted first, as an aborted stream.
	{ name: "observerThrowsAfterAbort", sse: frames(...minimal), throwAt: 1, abortFirst: true },
];

const context = normalizeContext({ messages: [{ role: "user", content: "Hello", timestamp: 1 }] });
// An oauth row streams with an OAuth token through the SDK client pi creates,
// its fetch serving the body, and with one current tool, Read.
const oauthToken = "sk-ant-oat01-capture";
const oauthContext = normalizeContext({
	messages: [
		{
			role: "system",
			content: "You are a test.",
			toolsAdded: [{ name: "Read", description: "Read a file", parameters: { type: "object", properties: {} } }],
			timestamp: 1,
		},
		{ role: "user", content: "Hello", timestamp: 1 },
	],
});
const v8ParseFailure = /^Could not parse Anthropic SSE event [a-z_]+: (Unexpected|Expected)/;

// run streams one case, the body whole in one read or one byte per read.
async function run(c: Case, oneByte: boolean) {
	const observed: string[] = [];
	const controller = new AbortController();
	const body = () => (oneByte ? oneBytePerRead(c.sse) : c.sse);
	const transport = c.oauth
		? {
				apiKey: oauthToken,
				fetch: async () => new Response(body(), { status: 200, headers: { "content-type": "text/event-stream" } }),
			}
		: { client: createFakeAnthropicClient(body()) };
	const s = streamAnthropic(model, c.oauth ? oauthContext : context, {
		...transport,
		signal: controller.signal,
		onProviderStreamEvent: async (event: unknown, eventModel: unknown) => {
			await Promise.resolve();
			if (eventModel !== model) throw new Error(`${c.name}: observer got another model`);
			if (event === null) throw new Error(`${c.name}: observer got null`);
			if (c.throwAt === observed.length) {
				observed.push(JSON.stringify(event));
				if (c.abortFirst) controller.abort();
				throw new Error("observer boom");
			}
			observed.push(JSON.stringify(event));
		},
	});
	const pushed: Array<string | null> = [];
	for await (const event of s) pushed.push(event.type ?? null);
	const message = await s.result();
	return {
		observed,
		pushed,
		message: {
			stopReason: message.stopReason,
			errorMessage: message.errorMessage ?? null,
			responseId: message.responseId ?? null,
			content: message.content,
			usage: message.usage,
			...(message.diagnostics
				? {
						diagnostics: message.diagnostics.map((d: { type: string; details?: unknown }) => ({
							type: d.type,
							details: d.details ?? null,
						})),
					}
				: {}),
		},
	};
}

const rows = [];
for (const c of cases) {
	const whole = await run(c, false);
	if (c.v8Cause && !v8ParseFailure.test(whole.message.errorMessage)) {
		throw new Error(`${c.name}: not a JSON.parse failure: ${whole.message.errorMessage}`);
	}
	const oneByte = await run(c, true);
	const differs = JSON.stringify(oneByte) !== JSON.stringify(whole);
	rows.push({
		name: c.name,
		...(typeof c.sse === "string" ? { sse: c.sse } : { sseBase64: Buffer.from(c.sse).toString("base64") }),
		...(c.v8Cause ? { v8Cause: true } : {}),
		...(c.throwAt !== undefined ? { throwAt: c.throwAt } : {}),
		...(c.abortFirst ? { abortFirst: true } : {}),
		...(c.oauth ? { oauth: true } : {}),
		...(c.rawSeed ? { rawSeed: true } : {}),
		...whole,
		...(differs
			? { oneByte: { ...oneByte, ...(v8ParseFailure.test(oneByte.message.errorMessage ?? "") ? { v8Cause: true } : {}) } }
			: {}),
	});
}
// One row per line, so a re-capture diffs row by row.
const text = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(
	outFile,
	`{"sha":${JSON.stringify(sha)},"pi-ai":${JSON.stringify(version)},"model":"anthropic/claude-haiku-4-5","rows":[\n${text}\n]}\n`,
);
console.log(`captured ${rows.length} rows from packages/ai/src at ${sha} (model: pi-ai ${version}) -> ${outFile}`);
