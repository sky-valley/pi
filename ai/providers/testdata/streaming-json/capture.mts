// Captures pi's parseStreamingJson — JSON.parse, then its repair, then
// partial-json's parse (Allow.ALL) of the text and of its repair — the oracle
// behind parseStreamingJSON in this package (json.go, partial_json.go).
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> streaming-json-4b994d5e5.json 4b994d5e5
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai`,
// prefix kept) and a node_modules resolving its dependencies. partial-json there
// must be the version package-lock.json locks at <sha> — 0.1.7 at 4b994d5e5,
// integrity
// sha512-Njv/59hHaokb/hRUjce3Hdv12wd60MtM9Z5Olmn+nehe0QDAsRtRbJPvJ0Z91TusF0SuZRIvnM+S4l6EIP8leA==
// — which the npm 1.3.0 build's node_modules carries. The version read is
// recorded in the output.
//
// A tool call's arguments are parseStreamingJson of the text streamed so far,
// on every delta and once more when the block ends — so every prefix of an
// arguments text is an input pi parses, and the last one is what a call cut
// off by finish_reason "length" keeps. Each document below is read at every
// prefix (by code point, since a Go string cannot hold half a surrogate pair).
// The documents are hand-written ones, reaching each branch of partial-json's
// parser, and pseudo-random tool arguments from a fixed seed. `cases` are
// single inputs that are not prefixes of a document.
//
// A result is JSON.stringify of what parseStreamingJson returned when it is an
// object, and null otherwise: pi then holds an array, a string, a number or a
// boolean as the arguments, which the port's map cannot.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const { parseStreamingJson } = await import(
	pathToFileURL(path.join(extraction, "packages/ai/src/utils/json-parse.ts")).href
);
const partialJSONVersion = JSON.parse(
	fs.readFileSync(path.join(extraction, "node_modules/partial-json/package.json"), "utf8"),
).version;

function result(input: string): string | null {
	const value = parseStreamingJson(input);
	return value !== null && typeof value === "object" && !Array.isArray(value) ? JSON.stringify(value) : null;
}

// Backslashes are written as B so this file carries no escape a tool could
// rewrite.
const B = "\\";
const documents: string[] = [
	`{"path":"a.txt","overwrite":true,"mode":null,"append":false}`,
	`{"n":-12.5e+3,"m":0,"k":1E2,"j":-0,"i":0.25,"h":123456789012345678901}`,
	`{"path":"C:${B}${B}Users${B}${B}x","s":"x${B}u00e9y${B}ud83d${B}ude00z","t":"a${B}"b${B}/c${B}nd${B}te"}`,
	`{"list":[1,"two",[3,{"four":4}],{"five":[5,null]},true],"empty":[],"obj":{}}`,
	`{ "a" : 1 ,  "b" :[ "x" , "y" ] ,"c":{ "d" : "e" } }`,
	`{\n  "command": "ls -la",\n  "cwd": "/tmp",\n  "env": {"A": "1"}\n}`,
	`{"b":1,"2":"x","1":{"4":0,"3":0},"0":[{"1":1,"0":0}],"b":2}`,
	`{"text":"é😀 raw ✓","key é":"v","😀":1}`,
	// Text a model writes that JSON.parse rejects but pi's repair fixes: a
	// raw control character in a string, an escape JSON does not have.
	`{"cmd":"echo a\tb\nc","path":"C:${B}Users${B}new","q":"${B}q${B}x"}`,
	`{"a":"x,","b":"y:","c":"z}","d":"w]"}`,
	`{"a":Infinity,"b":-Infinity,"c":NaN,"d":1e400,"e":-1e400}`,
	// A \u not followed by four hex digits is not an escape (4b994d5e5): the
	// repair doubles its backslash, as it does any other invalid escape.
	`{"path":"a.tex","content":"${B}usepackage{x} caf${B}u00e9 ${B}u12g ${B}url","mode":"w"}`,
	`{"__proto__":{"x":1},"y":2,"__proto__":3,"z":{"__proto__":null}}`,
	`[1,{"a":2},"b"]`,
	`"just a string"`,
	`12.5`,
];

// mulberry32, a fixed-seed PRNG, so the capture regenerates the same inputs.
function prng(seed: number): () => number {
	return () => {
		seed |= 0;
		seed = (seed + 0x6d2b79f5) | 0;
		let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
		t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	};
}
const random = prng(0x5eed);
const pick = <T,>(items: T[]): T => items[Math.floor(random() * items.length)];

const keys = ["path", "command", "content", "a", "0", "12", "x y", "é", `k${B}"q`, "old_string", "new_string"];
// Each string is JSON text for a string, some with escapes JSON.stringify
// would not write.
const strings = [
	`"hello"`,
	`"C:${B}${B}Users${B}${B}me"`,
	`"line${B}nbreak"`,
	`"tab${B}there"`,
	`"quote${B}"s"`,
	`"uni${B}u00e9"`,
	`"astral ${B}ud83d${B}ude00"`,
	`"slash${B}/ok"`,
	`"raw é 😀"`,
	`""`,
	`"a,b:c}d]e"`,
	`"x${B}${B}"`,
];
const numbers = ["0", "-1", "3.14", "1e10", "-2.5e-3", "12345678901234567890", "1E2", "-0", "0.5"];
const literals = ["true", "false", "null"];
const space = () => pick(["", "", "", " ", "\n  ", "\t"]);

function value(depth: number): string {
	const r = random();
	if (depth < 2 && r < 0.15) return object(depth + 1);
	if (depth < 2 && r < 0.3) {
		const n = Math.floor(random() * 4);
		return `[${space()}${Array.from({ length: n }, () => value(depth + 1)).join(`,${space()}`)}${space()}]`;
	}
	if (r < 0.65) return pick(strings);
	if (r < 0.85) return pick(numbers);
	return pick(literals);
}
function object(depth: number): string {
	const n = 1 + Math.floor(random() * 4);
	const members = Array.from({ length: n }, () => `${space()}"${pick(keys)}"${space()}:${space()}${value(depth)}`);
	return `{${members.join(",")}${space()}}`;
}
for (let i = 0; i < 60; i++) documents.push(object(0));

const cases: string[] = [
	"",
	"   ",
	String.fromCharCode(0xa0) + `{"a":1}`, // trim's NBSP, which JSON.parse rejects
	`{"a":1}x`,
	`{"a" "b"}`,
	`{a:1}`,
	`{"a":1,,"b":2}`,
	`{"a":[ ],"b":"e"}x`,
	`{"a":tr ue}`,
	// The colon is skipped unread, and a number runs to the next , ] or }.
	`{"a"x1}`,
	`{"a" 1,"b":2}`,
	`{"a":1 2}`,
	`{"a":1 ,"b":2`,
	`{"a":nulx}`,
	`{"a":-}`,
	`{"a":-I`,
	`{"a":Inf`,
	`{"a":Na`,
	`{"a":1.}`,
	`{"a":1e}`,
	`{"a":1e+}`,
	`{"a":.5}`,
	`{"a":01}`,
	`{"a":"b${B}u12"}`,
	`{"a":"b${B}u12`,
	`{"a":"b${B}`,
	`{"a":"b${B}${B}`,
	`{"a":"b${B}${B}${B}`,
	`{"a":"b\nc`,
	`{"a":"b${B}qc`,
	`{"2":1,"b":{"__proto__":[1],"c":2}}x`,
	`{"__proto__":null,"__proto__":1,"a":2,"__proto__":3}x`,
	`{"__proto__":{"a":1},"__proto__":null,"__proto__":{"b":2}}x`,
	`{"a":{"b":{"c":[1,[2,[3`,
	"nu",
	"-",
	"12",
	"[1,2",
	`"abc`,
	`{"a":1} {"b":2}`,
	`{"é":"😀`,
	// The inputs of upstream's json-parse.test.ts (4b994d5e5, #10868).
	`{"text":"${B}url{x}"}`,
	`{"path":"a.tex","content":"${B}usepackage{x}","mode":"w"}`,
	`{"text":"caf${B}u00e9 ${B}url"}`,
	`{"text":"ab${B}u00`,
];

const out = {
	sha,
	partialJSON: partialJSONVersion,
	documents: documents.map((doc) => {
		const points = [...doc];
		return { doc, results: points.map((_, i) => result(points.slice(0, i + 1).join(""))) };
	}),
	cases: cases.map((input) => ({ input, result: result(input) })),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${out.documents.reduce((n, d) => n + d.results.length, 0)} prefixes, ${cases.length} cases`);
