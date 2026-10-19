// Captures pi's parseToolCallArguments (utils/json-parse.ts, upstream
// 87f9a3641) — the arguments of a FINISHED tool call: JSON.parse of the text,
// then of its repair when that differs, kept only when it is an object;
// otherwise `{}` and an argumentsError naming the problem — the oracle behind
// parseToolCallArguments in this package (json.go).
//
//   node capture.mjs <pi-ai package dir> <streaming-json capture> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai \
//        ../streaming-json/streaming-json-4b994d5e5.json tool-call-arguments-1.3.0.json
//
// The package is the published npm build (dist/utils/json-parse.js); its
// version is recorded in the output. JSON.parse's SyntaxError message is part
// of argumentsError, which tool validation hands to the model, so the capture
// records it; node's version is recorded with it.
//
// Inputs: every prefix of every document of the streaming-json capture — a
// tool call cut off by finish_reason "length" finishes with such a prefix —
// and the cases below. Each result is [arguments, message]: arguments is
// JSON.stringify of the returned arguments, and message is argumentsError
// without its "\n\nReceived arguments:\n<input>" tail, which this script
// checks is there, or null when there is none. `full` records a few complete
// argumentsError texts as they are.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, streamingCapture, outFile] = process.argv.slice(2);
if (!pkg || !streamingCapture || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <streaming-json capture> <out.json>");
	process.exit(2);
}
const { parseToolCallArguments } = await import(pathToFileURL(path.join(pkg, "dist/utils/json-parse.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

function result(input) {
	const { arguments: args, argumentsError, ...rest } = parseToolCallArguments(input);
	if (Object.keys(rest).length > 0) throw new Error(`unexpected keys ${Object.keys(rest)} for ${input}`);
	if (argumentsError === undefined) return [JSON.stringify(args), null];
	if (JSON.stringify(args) !== "{}") throw new Error(`an error with arguments for ${input}`);
	const tail = `\n\nReceived arguments:\n${input}`;
	if (!argumentsError.endsWith(tail)) throw new Error(`argumentsError without the received text for ${input}`);
	return [JSON.stringify(args), argumentsError.slice(0, -tail.length)];
}

// Backslashes are written as B so this file carries no escape a tool could
// rewrite.
const B = "\\";
const cases = [
	// upstream's json-parse.test.ts
	`{"kind":"seek","note":"${B}url"}`,
	"",
	`{"action":"apply","actions":[{"kind":"seek","time":.5}]}`,
	`{"kind":"seek","clipId":"clip-fl`,
	"[1,2]",
	// upstream's openai-responses-partial-json-cleanup.test.ts
	`{"path":"README.md","content":"updated","mode":.5}`,
	// blank by String.prototype.trim, which strips more than JSON whitespace
	" \t\n\r",
	String.fromCharCode(0xa0),
	String.fromCharCode(0xfeff, 0x2028, 0x3000),
	String.fromCharCode(0xa0) + `{"a":1}`,
	// valid JSON that is not an object
	"null",
	"12",
	`"x"`,
	"true",
	"[]",
	// JSON.parse's specially named sources
	"undefined",
	"NaN",
	"Infinity",
	"[object Object]",
	// the repair: raw control characters and invalid escapes inside strings
	`{"cmd":"echo a${String.fromCharCode(9)}b${String.fromCharCode(10)}c","q":"${B}q"}`,
	`{"a":"b${B}u12"}`,
	// ... when the repaired text still fails, its own error position is reported
	`{"a":"b${B}q",}`,
	`{"a":"${B}x${B}y${B}z","b":tru}`,
	`{"a":"line${String.fromCharCode(10)}two" "b":1}`,
	// positions count UTF-16 code units, lines and columns too
	`{"é😀":1,}`,
	`{\n  "a": 1,\n  "b": [1, 2,]\n}`,
	`{\r\n"a":1\r\n,}`,
	// trailing content
	`{"a":1}x`,
	`{"a":1} {"b":2}`,
	`{"a":1}   `,
	// key order, duplicate keys and __proto__ as JSON.parse builds the object
	`{"b":1,"2":"x","1":"y","b":3}`,
	`{"__proto__":{"x":1},"y":2}`,
	`{"a":1e400,"b":-1e400,"c":-0}`,
	// long sources quote ten code units either side of the error
	`{"path":"/a/very/long/path/name.txt","content":x}`,
	`{"x":y,"path":"/a/very/long/path/name.txt"}`,
	`{"path":"/a/very/long/path/name.txt","content"}`,
	`{"path":"/a/very/long/path/name.txt"}}`,
	`{'a':1}`,
	"{a:1}",
	`{"a":01}`,
	`{"a":-}`,
	`{"a":1.}`,
	`{"a":1e}`,
];

const documents = JSON.parse(fs.readFileSync(streamingCapture, "utf8")).documents.map((d) => d.doc);
const full = [
	`{"action":"apply","actions":[{"kind":"seek","time":.5}]}`,
	"[1,2]",
	`{"a":"b${B}q",}`,
].map((input) => ({ input, argumentsError: parseToolCallArguments(input).argumentsError }));

const out = {
	piAi: version,
	node: process.version,
	documents: documents.map((doc) => {
		const points = [...doc];
		return { doc, results: points.map((_, i) => result(points.slice(0, i + 1).join(""))) };
	}),
	cases: cases.map((input) => ({ input, result: result(input) })),
	full,
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${out.documents.reduce((n, d) => n + d.results.length, 0)} prefixes, ${cases.length} cases`);
