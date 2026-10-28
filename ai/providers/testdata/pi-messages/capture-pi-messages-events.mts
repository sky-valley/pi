// Captures how real pi's pi-messages adapter reads a wire stream — the events
// onProviderStreamEvent observes, the events the stream pushes and the final
// message — the oracle behind TestPiMessagesEventsMatchPi (upstream 002fc8385
// plus readPiMessagesEvents' frame rules).
//
//   node --experimental-strip-types capture-pi-messages-events.mts <extraction> <out.json> <sha>
//   e.g. ... capture-pi-messages-events.mts <dir> pi-messages-events-8676a0dcd.json 8676a0dcd
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai |
// tar -x -C <dir>` from the upstream clone), with packages/ai/node_modules
// resolving pi-ai's dependencies (the npm build's node_modules serves; this
// adapter calls fetch directly, so no SDK is involved). The adapter runs from
// SOURCE: onProviderStreamEvent is in no published build yet.
//
// The model is the upstream suite's createModel (pi-messages.test.ts), and the
// body is served through options.fetch, so no network is needed. Each row
// records:
//   sse        the exact body, which the Go test replays byte for byte (a
//              body that is not valid UTF-8 is sseBase64 instead);
//   observed   each value onProviderStreamEvent received, as JSON.stringify
//              text, so key order is part of the expectation;
//   pushed     the type of every event the stream pushed (null: no type);
//   pushedIndex the contentIndex every pushed event carries (null: none);
//   message    stopReason, errorMessage, responseId, content and usage of the
//              result, and the type, error and details of each diagnostic (not
//              its timestamp, nor its error's stack; a details timestampMs,
//              Date.now(), is recorded as 0 in its place);
//   v8Error    the frame data whose JSON.parse failure is the errorMessage:
//              V8's text, which the port does not reproduce.
// A row may make the observer throw ("observer boom") on the observed event at
// index throwAt, after aborting the request when abortFirst is set. A row with
// a status is served with that status, the standard reason phrase for it
// (which the Go replay's status line carries too; a server's own phrase is
// capture-pi-messages-status.mts's) and an application/json content type,
// from the suite model's baseUrl, which the details' url shows.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-pi-messages-events.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const { streamSimple } = await import(pathToFileURL(path.join(src, "api/pi-messages.ts")).href);
const { normalizeContext } = await import(pathToFileURL(path.join(src, "utils/transcript.ts")).href);

// The suite's createModel.
const model = {
	id: "auto",
	name: "Radius Auto",
	api: "pi-messages",
	provider: "radius",
	baseUrl: "http://pi-messages.invalid/v1",
	reasoning: false,
	input: ["text"],
	cost: { input: 1, output: 2, cacheRead: 0.1, cacheWrite: 0.2 },
	contextWindow: 128000,
	maxTokens: 16384,
};

// The suite's usage and upstream's 'forwards parsed wire events in order before
// converting them' wire events.
const usage = {
	input: 10,
	output: 5,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 15,
	cost: { input: 0.1, output: 0.2, cacheRead: 0, cacheWrite: 0, total: 0.3 },
};
const wireEvents = [
	{ type: "start" },
	{ type: "text_start", contentIndex: 0 },
	{ type: "text_delta", contentIndex: 0, delta: "Hello", gatewayField: "upstream-value" },
	{ type: "text_end", contentIndex: 0, content: "Hello" },
	{ type: "done", reason: "stop", usage, responseId: "resp_1" },
];
// The suite's server writes each event as one `data:` frame.
const frame = (data: string) => `data: ${data}\n\n`;
const framed = (...events: unknown[]) => events.map((event) => frame(JSON.stringify(event))).join("");
const [start, textStart, textDelta, textEnd, done] = wireEvents;
// Text JSON.stringify writes as itself where encoding/json escapes it: <, > and
// &, and U+2028 and U+2029 (spelled so no tool decodes an escape).
const markup = `<b>&x> ${String.fromCharCode(0x2028)} ${String.fromCharCode(0x2029)} end`;
// The UTF-8 byte-order mark, and a body's bytes from text and byte runs.
const BOM = String.fromCharCode(0xfeff);
const bytesOf = (...parts: Array<string | number[]>) =>
	new Uint8Array(Buffer.concat(parts.map((p) => (typeof p === "string" ? Buffer.from(p, "utf8") : Buffer.from(p)))));

type Case = {
	name: string;
	sse: string | Uint8Array;
	v8Error?: string;
	throwAt?: number;
	abortFirst?: boolean;
	status?: number;
	statusText?: string;
};
const cases: Case[] = [
	{ name: "forwardsWireEventsInOrder", sse: framed(...wireEvents) },
	// `if (event)`: a frame parsing to a JS-falsy value is neither observed nor
	// converted, and neither is a frame with no data line, an empty data line or
	// [DONE].
	{
		name: "falsyFramesAreSkipped",
		sse:
			framed(start) +
			frame("null") +
			frame("false") +
			frame("0") +
			frame("-0") +
			frame('""') +
			frame("") +
			frame("[DONE]") +
			"event: ping\n\n" +
			framed(textStart, textDelta, textEnd, done),
	},
	// A truthy array is observed, then converted into an event with no type
	// (pi returns `{...event, partial}`): `"contentIndex" in [1]` is false.
	{
		name: "truthyArrayIsObservedAndPushed",
		sse: framed(start) + frame("[1]") + framed(textStart, textDelta, textEnd, done),
	},
	// Any other truthy value that is not an object is observed, then fails the
	// stream: the converter's `"contentIndex" in event` throws V8's TypeError
	// for a primitive, which names it as String() writes it (upstream
	// 89c921d8b).
	{ name: "truthyNumberFrameFails", sse: framed(start) + frame("5") + framed(textStart, textDelta, textEnd, done) },
	{ name: "truthyBigNumberFrameFails", sse: framed(start) + frame("1e21") + framed(done) },
	{ name: "truthyInfinityFrameFails", sse: framed(start) + frame("-1e400") + framed(done) },
	{ name: "truthyStringFrameFails", sse: framed(start) + frame('"x \\"y\\" \\u00e9"') + framed(done) },
	{ name: "truthyTrueFrameFails", sse: framed(start) + frame("true") + framed(done) },
	// Only a frame's FIRST data line is read, trimmed as String.prototype.trim.
	{
		name: "firstDataLineOnly",
		sse: `data: ${JSON.stringify(start)}\ndata: ${JSON.stringify(textStart)}\n\n` + framed(textStart, textDelta, textEnd) + `data: \t${JSON.stringify(done)}  \n\n`,
	},
	// CRLF framing is normalized, and a trailing frame with no blank line after
	// it is still read.
	{
		name: "crlfAndTrailingFrame",
		sse: framed(start, textStart, textDelta, textEnd).replaceAll("\n", "\r\n") + `data: ${JSON.stringify(done)}`,
	},
	// JSON.parse throws on a frame that is not JSON: the stream fails with V8's
	// SyntaxError text.
	{ name: "unparseableFrameFails", sse: framed(start, textStart) + frame("{oops") + framed(textEnd, done), v8Error: "{oops" },
	// The trailing frame is parsed the same way.
	{ name: "unparseableTrailingFrameFails", sse: framed(start, textStart) + "data: [1,", v8Error: "[1," },
	// Unknown wire event types are observed and converted like any other.
	{
		name: "unknownTypeIsObservedAndPushed",
		sse: framed(start, { type: "gateway_note", note: "n" }, textStart, textDelta, textEnd, done),
	},
	// JSON.parse builds an ordinary object, which enumerates array-index keys
	// first, ascending, then the rest in wire order, at every depth: observed
	// shows it.
	{
		name: "observedKeysEnumerateIndicesFirst",
		sse:
			framed(start, textStart) +
			frame('{"type":"text_delta","contentIndex":0,"delta":"Hello","7":1,"b":{"z":1,"0":2},"1":3}') +
			framed(textEnd, done),
	},
	// JSON.parse reads a number past float64's range as +-Infinity (observed
	// as JSON.stringify's null), and one below the smallest subnormal as 0,
	// which is falsy: the frame is skipped. A bare Infinity is truthy, so it is
	// observed and then fails the stream as any truthy primitive does.
	{
		name: "overflowNumbersAreObserved",
		sse:
			framed(start) +
			frame("1e-400") +
			frame('{"type":"gateway_note","x":1e400}') +
			framed(textStart, textDelta, textEnd) +
			frame("1e400") +
			framed(done),
	},
	// observed is JSON.stringify text: <, >, &, U+2028 and U+2029 are written as
	// themselves, however deep in the event they sit.
	{
		name: "observedTextIsNotHTMLEscaped",
		sse: framed(start, textStart, { ...textDelta, delta: markup }, { ...textEnd, content: markup }, done),
	},
	// A member of an unexpected JSON type never drops the event: pi reads each
	// property as whatever it holds. An unknown type and an object with no type
	// are observed and pushed; a delta that is not a
	// string is appended as String() writes it (5 as "5", none as "undefined",
	// null as "null", [1,[2,null]] as "1,2,"); counts written 10.0 and 5e0 are
	// the numbers 10 and 5; and a rewrite's members, whatever they hold, are the
	// diagnostic's details.
	{
		name: "mistypedMembersStillConvert",
		sse:
			framed(start) +
			frame('{"type":"gateway_note","id":5}') +
			frame('{"note":1}') +
			framed(textStart) +
			frame('{"type":"text_delta","contentIndex":0,"delta":5}') +
			frame('{"type":"text_delta","contentIndex":0}') +
			frame('{"type":"text_delta","contentIndex":0,"delta":null}') +
			frame('{"type":"text_delta","contentIndex":0,"delta":[1,[2,null]]}') +
			frame(
				'{"type":"done","reason":"stop","usage":{"input":10.0,"output":5e0,"cacheRead":0,"cacheWrite":0,"totalTokens":15.0,' +
					'"cost":{"input":0.1,"output":0.2,"cacheRead":0,"cacheWrite":0,"total":0.3}},"responseId":"resp_1",' +
					'"rewrite":{"policyId":"p","policyVersion":"2","changed":true,"extra":[1]}}',
			),
	},
	// A property is read by its exact name: "Type", "ContentIndex" and "DELTA"
	// are not the event's type, contentIndex and delta, so the frame converts
	// as an event with no type.
	{
		name: "memberNamesMatchExactly",
		sse: framed(start, textStart) + frame('{"Type":"text_delta","ContentIndex":0,"DELTA":"x"}') + framed(textDelta, done),
	},
	// A delta with no string form (an object with its own toString member)
	// makes pi's `+=` throw V8's TypeError, which fails the stream.
	{
		name: "deltaWithNoStringFormFails",
		sse: framed(start, textStart) + frame('{"type":"text_delta","contentIndex":0,"delta":{"toString":1}}') + framed(textEnd, done),
	},
	// A contentIndex the event carries must be an integer number from 0 to the
	// last block — to the next block for a *_start — or the stream fails
	// (upstream 89c921d8b). Any number spelling that denotes such an integer
	// is one (-0 is 0, 1.0 and 1e0 are 1), and a start at an existing block
	// replaces it.
	{
		name: "contentIndexNumberSpellings",
		sse:
			framed(start, textStart) +
			frame('{"type":"text_delta","contentIndex":-0,"delta":"a"}') +
			frame('{"type":"thinking_start","contentIndex":1.0}') +
			frame('{"type":"thinking_delta","contentIndex":1e0,"delta":"b"}') +
			frame('{"type":"text_start","contentIndex":0.0}') +
			frame('{"type":"text_delta","contentIndex":0,"delta":"c"}') +
			frame('{"type":"text_end","contentIndex":0E0,"content":"c"}') +
			framed(done),
	},
	// An event without a contentIndex is not checked: its index is undefined,
	// which names the content array's ordinary property "undefined" — not
	// content, so the message never holds the block.
	{
		name: "absentContentIndexNamesAProperty",
		sse:
			framed(start, textStart) +
			frame('{"type":"text_start"}') +
			frame('{"type":"text_delta","delta":"x"}') +
			frame('{"type":"text_end","content":"x"}') +
			frame('{"type":"toolcall_start","id":"t1","toolName":"read"}') +
			frame('{"type":"toolcall_delta","delta":"{}"}') +
			frame('{"type":"toolcall_end"}') +
			framed(textDelta, done),
	},
	// Without a block there, an absent index reads a property of undefined,
	// and V8's TypeError fails the stream: a delta reads the block's text or
	// thinking, and an end is Object.assign onto it.
	{ name: "absentIndexTextDeltaFails", sse: framed(start) + frame('{"type":"text_delta","delta":"x"}') + framed(done) },
	{ name: "absentIndexThinkingDeltaFails", sse: framed(start) + frame('{"type":"thinking_delta","delta":"x"}') + framed(done) },
	{ name: "absentIndexTextEndFails", sse: framed(start) + frame('{"type":"text_end","content":"x"}') + framed(done) },
	// The error names the index as JSON.stringify writes it, and the event's
	// type. upstream's suite: a far start, a "__proto__" start and end, and a
	// delta before any block.
	{ name: "farStartIndexFails", sse: framed(start) + frame('{"type":"text_start","contentIndex":4294967000}') + framed(done) },
	{ name: "protoStartIndexFails", sse: framed(start) + frame('{"type":"text_start","contentIndex":"__proto__"}') + framed(done) },
	{
		name: "protoToolcallEndIndexFails",
		sse:
			framed(start, textStart) +
			frame('{"type":"toolcall_end","contentIndex":"__proto__","toolCall":{"polluted":true}}') +
			framed(done),
	},
	{ name: "textDeltaWithoutBlockFails", sse: framed(start) + frame('{"type":"text_delta","contentIndex":0,"delta":"x"}') + framed(done) },
	// A start may add only the next block, and every other event needs one.
	{ name: "startPastTheNextBlockFails", sse: framed(start) + frame('{"type":"text_start","contentIndex":1}') + framed(done) },
	{ name: "textEndWithoutBlockFails", sse: framed(start) + frame('{"type":"text_end","contentIndex":3,"content":"x"}') + framed(done) },
	{
		name: "toolcallEndWithoutBlockFails",
		sse:
			framed(start) +
			frame('{"type":"toolcall_end","contentIndex":0,"toolCall":{"type":"toolCall","id":"t1","name":"read","arguments":{}}}') +
			framed(done),
	},
	// The check runs before the delta looks for an arguments stream, so a
	// toolcall_delta for a block that was never started fails, while one for
	// a call that has ended — no stream, but a block — still changes nothing
	// (upstream f715a15c1).
	{
		name: "toolcallDeltaWithoutBlockFails",
		sse: framed(start) + frame('{"type":"toolcall_delta","contentIndex":0,"delta":"{}"}') + framed(done),
	},
	{
		name: "toolcallDeltaAfterTheEndIsIgnored",
		sse:
			framed(start) +
			frame('{"type":"toolcall_start","contentIndex":0,"id":"t1","toolName":"read"}') +
			frame('{"type":"toolcall_end","contentIndex":0}') +
			frame('{"type":"toolcall_delta","contentIndex":0,"delta":"{\\"a\\":1}"}') +
			framed(done),
	},
	// JSON.stringify of the wire value: a string quoted and escaped, a number
	// as JavaScript writes it (1.5, -1, 1e3 as 1000, 1E2 as 100, 1.0 as 1, -0
	// as 0, 1e21 as 1e+21, past float64's range as null), and an object or
	// array as JSON.parse built it (index keys first, a repeated key once,
	// with its last value).
	...(
		[
			["String", '"1"'],
			["Fraction", "1.5"],
			["Negative", "-1"],
			["Exponent", "1e3"],
			["UpperExponent", "1E2"],
			["WholeFraction", "1.0"],
			["NegativeZero", "-0"],
			["Huge", "1e21"],
			["Overflow", "1e400"],
			["Null", "null"],
			["True", "true"],
			["Array", '[1,"a",[null]]'],
			["Object", '{"b":1,"1":2,"__proto__":3,"b":4}'],
			["EscapedString", '"a\\"b\\\\c\\u0001\\n\\u2028<&>\\u00e9\\ud83d\\ude00"'],
			["LoneSurrogate", '"\\ud800x"'],
		] as const
	).map(([kind, index]) => ({
		name: `indexRendering${kind}`,
		sse: framed(start) + frame(`{"type":"text_delta","contentIndex":${index},"delta":"x"}`) + framed(done),
	})),
	// Every event carrying a contentIndex is checked — an unknown type, a
	// terminal event — against the next block when its type ends in "_start".
	{
		name: "unknownTypeIndexIsChecked",
		sse: framed(start, textStart) + frame('{"type":"gateway_note","contentIndex":1}') + framed(done),
	},
	{
		name: "unknownStartTypeMayNameTheNextBlock",
		sse: framed(start, textStart) + frame('{"type":"gateway_start","contentIndex":1}') + framed(textDelta, textEnd, done),
	},
	{ name: "doneIndexIsChecked", sse: framed(start, textStart, textDelta) + frame(JSON.stringify({ ...done, contentIndex: 1 })) },
	{ name: "doneWithAnExistingIndexIsDone", sse: framed(start, textStart, textDelta) + frame(JSON.stringify({ ...done, contentIndex: 0 })) },
	{
		name: "errorIndexIsChecked",
		sse: framed(start) + frame(JSON.stringify({ type: "error", reason: "error", usage, errorMessage: "backend failed", contentIndex: 0 })),
	},
	// The check reads `event.type.endsWith`, which throws V8's TypeError for
	// an event with a contentIndex and no string type.
	{ name: "indexWithoutTypeFails", sse: framed(start) + frame('{"contentIndex":0}') + framed(done) },
	{ name: "indexWithNullTypeFails", sse: framed(start) + frame('{"type":null,"contentIndex":0}') + framed(done) },
	{ name: "indexWithNumberTypeFails", sse: framed(start) + frame('{"type":5,"contentIndex":0}') + framed(done) },
	{ name: "indexWithArrayTypeFails", sse: framed(start) + frame('{"type":["text_start"],"contentIndex":0}') + framed(done) },
	// The partial tool JSON is kept in a Map keyed by the contentIndex number
	// (SameValueZero), so 0, -0 and 0.0 share one buffer.
	{
		name: "toolJsonIsKeyedByTheIndexValue",
		sse:
			framed(start) +
			frame('{"type":"toolcall_start","contentIndex":0,"id":"t1","toolName":"read"}') +
			frame('{"type":"toolcall_delta","contentIndex":0,"delta":"{\\"a\\":1"}') +
			frame('{"type":"toolcall_delta","contentIndex":-0,"delta":",\\"b\\":2"}') +
			frame('{"type":"toolcall_delta","contentIndex":0.0,"delta":"}"}') +
			framed(done),
	},
	// toolcall_end is Object.assign: the end event's toolCall members replace
	// the block's, and a member it lacks (here arguments) keeps the value the
	// deltas built. A null member is assigned as null. An end with no toolCall
	// leaves the block as it is.
	{
		name: "toolcallEndMergesIntoTheBlock",
		sse:
			framed(start) +
			frame('{"type":"toolcall_start","contentIndex":0,"id":"t1","toolName":"read"}') +
			frame('{"type":"toolcall_delta","contentIndex":0,"delta":"{\\"path\\":\\"a\\"}"}') +
			frame('{"type":"toolcall_end","contentIndex":0,"toolCall":{"type":"toolCall","name":"read2","thoughtSignature":"sig"}}') +
			frame('{"type":"toolcall_start","contentIndex":1,"id":"t2","toolName":"write"}') +
			frame('{"type":"toolcall_delta","contentIndex":1,"delta":"{\\"x\\":1}"}') +
			frame('{"type":"toolcall_end","contentIndex":1,"toolCall":{"type":"toolCall","id":"t2b","arguments":null}}') +
			frame('{"type":"toolcall_start","contentIndex":2,"id":"t3","toolName":"ls"}') +
			frame('{"type":"toolcall_end","contentIndex":2}') +
			framed(done),
	},
	// text_end and thinking_end assign the event's contentSignature (and
	// redacted) whether or not it carries one: a later end without one leaves
	// the block with none.
	{
		name: "endAssignsAnAbsentSignature",
		sse:
			framed(start, textStart) +
			frame('{"type":"text_end","contentIndex":0,"content":"a","contentSignature":"s1"}') +
			frame('{"type":"text_end","contentIndex":0,"content":"b"}') +
			frame('{"type":"thinking_start","contentIndex":1}') +
			frame('{"type":"thinking_end","contentIndex":1,"content":"t","contentSignature":"s2","redacted":true}') +
			frame('{"type":"thinking_end","contentIndex":1,"content":"u"}') +
			framed(done),
	},
	// The rewrite diagnostic's details are `{ ...rewrite }`: the spread lists
	// array-index keys first, ascending, then the rest in wire order, at every
	// depth, and JSON.stringify writes its numbers as JavaScript numbers — 10.0
	// as 10, 1e3 as 1000, -0 as 0, and a number past float64's range, which
	// JSON.parse reads as Infinity, as null.
	{
		name: "rewriteDetailsAreTheSpreadAsJSONStringifyWritesIt",
		sse:
			framed(start, textStart, textDelta, textEnd) +
			frame(
				'{"type":"done","reason":"stop","usage":' +
					JSON.stringify(usage) +
					',"rewrite":{"z":1,"saved":10.0,"big":1e400,"neg":-1e400,"negz":-0,"k":1e3,"tiny":1e-400,"a":2,"7":"seven","1":"one",' +
					'"n":{"x":1e400,"y":10.0,"b":1,"0":2,"arr":[1e400,-0,2.50]}}}',
			),
	},
	// An array spreads its elements under their indices, in index order, and a
	// string its UTF-16 code units: "10" and "11" come after "9".
	{
		name: "rewriteArraySpreadsItsIndicesInOrder",
		sse: framed(start, { ...done, rewrite: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11] }),
	},
	{ name: "rewriteStringSpreadsItsCodeUnitsInOrder", sse: framed(start, { ...done, rewrite: "abcdefghijkl" }) },
	// A truthy number or true spreads nothing: the diagnostic's details are an
	// empty object, which is still written.
	{ name: "rewriteNumberSpreadsNothing", sse: framed(start, { ...done, rewrite: 5 }) },
	{
		name: "rewriteTrueOnAnErrorSpreadsNothing",
		sse: framed(start, { type: "error", reason: "error", usage, errorMessage: "backend failed", rewrite: true }),
	},
	// A non-2xx response fails with pi_messages_response_failure, whose details
	// list version, provider, model, url, status, statusText, then the body's
	// error object as JSON.parse builds it (its order, its numbers) or else the
	// body, then timestampMs.
	{
		name: "responseFailureDetailsKeepTheirOrder",
		status: 401,
		statusText: "Unauthorized",
		sse: '{"error":{"message":"Token expired","code":"unauthorized","details":{"z":1,"a":[1e400,10.0]},"retryAfter":1e3,"1":"x"}}',
	},
	// An empty-string code is still the code: no suffix, but the diagnostic's
	// error carries it.
	{
		name: "responseFailureEmptyCodeIsKept",
		status: 403,
		statusText: "Forbidden",
		sse: '{"error":{"message":"no","code":""}}',
	},
	// An error member that is not an object leaves the body as the details'
	// body and the message's suffix.
	{
		name: "responseFailureWithoutAnErrorObjectKeepsTheBody",
		status: 500,
		statusText: "Internal Server Error",
		sse: '{"error":[1]}',
	},
	// The body is decoded by a TextDecoder: a byte-order mark at the very start
	// is dropped, so the first frame still reads...
	{ name: "leadingBOMIsDropped", sse: BOM + framed(...wireEvents) },
	// ...while one anywhere else is text: here it leads a frame's line, which is
	// then no `data:` line, so the frame is passed over and the delta after it
	// names a block that was never started.
	{ name: "bomAfterTheStartIsText", sse: framed(start) + BOM + framed(textStart, textDelta, textEnd, done) },
	// Invalid UTF-8 becomes one U+FFFD per maximal subpart: a truncated
	// sequence is one, an encoded surrogate, an overlong form and a stray byte
	// one per byte.
	{
		name: "invalidUTF8DecodesPerMaximalSubpart",
		sse: bytesOf(
			framed(start, textStart),
			'data: {"type":"text_delta","contentIndex":0,"delta":"a',
			[0xe2, 0x82],
			"Z",
			[0xf0, 0x9f],
			"b",
			[0xed, 0xa0, 0x80],
			"c",
			[0xc0, 0xaf],
			"d",
			[0xff],
			'e"}\n\n',
			framed(done),
		),
	},
	// An error response's body is response.text(): its byte-order mark
	// dropped, so the body still parses, and invalid UTF-8 decoded.
	{
		name: "responseFailureBodyIsDecodedText",
		status: 500,
		statusText: "Internal Server Error",
		sse: bytesOf(BOM + '{"error":{"message":"a', [0xe2, 0x82], 'Z"}}'),
	},
	{
		name: "responseFailureUnparsedBodyIsDecodedText",
		status: 502,
		statusText: "Bad Gateway",
		sse: bytesOf("bad ", [0xe2, 0x82], " gateway"),
	},
	// A throwing observer fails the stream with its message: on the first event,
	{ name: "observerThrowsOnFirstEvent", sse: framed(...wireEvents), throwAt: 0 },
	// on the terminal done event, which then never converts to done,
	{ name: "observerThrowsOnDone", sse: framed(...wireEvents), throwAt: 4 },
	// and, when the request was aborted first, as an aborted stream.
	{ name: "observerThrowsAfterAbort", sse: framed(...wireEvents), throwAt: 2, abortFirst: true },
];

const context = normalizeContext({ messages: [{ role: "user", content: "Hello", timestamp: 1 }] });
const rows = [];
for (const c of cases) {
	const observed: string[] = [];
	const controller = new AbortController();
	const s = streamSimple(model, context, {
		apiKey: "test-key",
		signal: controller.signal,
		fetch: async () =>
			new Response(c.sse, {
				status: c.status ?? 200,
				statusText: c.statusText ?? "",
				headers: { "content-type": c.status === undefined ? "text/event-stream" : "application/json" },
			}),
		onProviderStreamEvent: async (event: unknown, eventModel: unknown) => {
			await Promise.resolve();
			if (eventModel !== model) throw new Error(`${c.name}: observer got another model`);
			observed.push(JSON.stringify(event));
			if (c.throwAt === observed.length - 1) {
				if (c.abortFirst) controller.abort();
				throw new Error("observer boom");
			}
		},
	});
	const pushed: Array<string | null> = [];
	const pushedIndex: unknown[] = [];
	for await (const event of s) {
		pushed.push(event.type ?? null);
		pushedIndex.push((event as { contentIndex?: unknown }).contentIndex ?? null);
	}
	const message = await s.result();
	if (c.v8Error !== undefined && !/^(Unexpected|Expected)/.test(message.errorMessage)) {
		throw new Error(`${c.name}: not a JSON.parse failure: ${message.errorMessage}`);
	}
	rows.push({
		name: c.name,
		...(c.status !== undefined ? { status: c.status } : {}),
		...(typeof c.sse === "string" ? { sse: c.sse } : { sseBase64: Buffer.from(c.sse).toString("base64") }),
		...(c.v8Error !== undefined ? { v8Error: c.v8Error } : {}),
		...(c.throwAt !== undefined ? { throwAt: c.throwAt } : {}),
		...(c.abortFirst ? { abortFirst: true } : {}),
		observed,
		pushed,
		pushedIndex,
		message: {
			stopReason: message.stopReason,
			errorMessage: message.errorMessage ?? null,
			responseId: message.responseId ?? null,
			content: message.content,
			usage: message.usage,
			diagnostics: (message.diagnostics ?? []).map(
				(d: { type: string; error?: { name?: string; message: string; code?: unknown }; details?: Record<string, unknown> }) => {
					if (d.details && "timestampMs" in d.details) d.details.timestampMs = 0;
					const error = d.error && {
						name: d.error.name,
						message: d.error.message,
						...(d.error.code !== undefined ? { code: d.error.code } : {}),
					};
					return { type: d.type, ...(error ? { error } : {}), details: d.details ?? null };
				},
			),
		},
	});
}
// One row per line, so a re-capture diffs row by row.
const text = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(outFile, `{"sha":${JSON.stringify(sha)},"rows":[\n${text}\n]}\n`);
console.log(`captured ${rows.length} rows from packages/ai/src at ${sha} -> ${outFile}`);
