// Captures pi's StreamingJsonParser and ToolCallArgumentsStream
// (packages/ai/src/utils/streaming-json.ts, upstream f715a15c1) — the oracle
// behind TestStreamingJSONParserLikePi and TestToolCallArgumentsStreamLikePi in
// package ai (streaming_json.go).
//
//   node capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> streaming-json-f715a15c1.json f715a15c1
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai
// package-lock.json`, prefix kept) and a node_modules resolving its
// dependencies. streaming-json.ts imports only types and ./json-parse.ts, whose
// one dependency is partial-json: it must be the version package-lock.json
// locks at <sha> — 0.1.7 at f715a15c1, integrity
// sha512-Njv/59hHaokb/hRUjce3Hdv12wd60MtM9Z5Olmn+nehe0QDAsRtRbJPvJ0Z91TusF0SuZRIvnM+S4l6EIP8leA==
// — which the npm 1.4.0 build's node_modules carries. The version read and its
// lockfile integrity are recorded in the output, and the capture refuses to
// write when they differ.
//
// A row feeds pieces to one StreamingJsonParser, one append per piece, and
// records after each piece the changes append() returned (absent when there
// are none) and the snapshot: JSON.stringify of it (null while it is
// undefined; absent when it is the previous step's text again) and whether it
// is the very object the previous snapshot() returned (the copy-credit lag
// shows as a repeated snapshot). Rows marked "final" record only the last
// snapshot, and rows marked "size" record each snapshot's JSON length instead
// of its text. The pieces are `pieces`, or `documents[doc]` cut into
// `size`-unit chunks or at the `cuts` offsets. A piece or document holding a
// lone UTF-16 surrogate (a split surrogate pair) is written as its array of
// code units, since JSON text cannot carry it as a string.
//
// The pieces are every document below one code unit at a time, whole, and in
// 3- and 7-unit chunks; every hand-written one up to 90 units also split in
// two at every position; the escape document of upstream's split-escapes test
// cut in three at every pair of positions; JSONTestSuite's documents
// (upstream's test/data/json-test-suite.json) whole and in 1- and 3-unit
// chunks; and large documents whose open container outgrows the copy credit.
//
// `args` rows drive one ToolCallArgumentsStream through append, replace and
// finish (`ops`, or `documents[doc]` appended one code unit at a time and
// finished), recording what each returns (`same`: the very arguments object
// the previous call returned).
//
// Control: `control` counts the args steps whose arguments differ from what
// parseStreamingJson (json-parse.ts, unchanged by <sha>: the parser providers
// used before it) returns for the same text, the parser this oracle replaces.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src/utils");
const { StreamingJsonParser, ToolCallArgumentsStream } = await import(
	pathToFileURL(path.join(src, "streaming-json.ts")).href
);
const { parseStreamingJson } = await import(pathToFileURL(path.join(src, "json-parse.ts")).href);
const partialJSON = JSON.parse(
	fs.readFileSync(path.join(extraction, "node_modules/partial-json/package.json"), "utf8"),
).version;
const locked = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8")).packages[
	"node_modules/partial-json"
];
if (locked.version !== partialJSON) {
	console.error(`partial-json ${partialJSON} is not the locked ${locked.version}`);
	process.exit(1);
}

// Backslashes are written as B and other escapes as code units, so this file
// carries no escape a tool could rewrite.
const B = "\\";
const ch = (...units: number[]) => String.fromCharCode(...units);
const HIGH = ch(0xd83d);
const LOW = ch(0xde00);
const SMILE = HIGH + LOW;

const documents: string[] = [
	// The documents of the parseStreamingJson oracle (ai/providers/testdata/streaming-json).
	`{"path":"a.txt","overwrite":true,"mode":null,"append":false}`,
	`{"n":-12.5e+3,"m":0,"k":1E2,"j":-0,"i":0.25,"h":123456789012345678901}`,
	`{"path":"C:${B}${B}Users${B}${B}x","s":"x${B}u00e9y${B}ud83d${B}ude00z","t":"a${B}"b${B}/c${B}nd${B}te"}`,
	`{"list":[1,"two",[3,{"four":4}],{"five":[5,null]},true],"empty":[],"obj":{}}`,
	`{ "a" : 1 ,  "b" :[ "x" , "y" ] ,"c":{ "d" : "e" } }`,
	`{\n  "command": "ls -la",\n  "cwd": "/tmp",\n  "env": {"A": "1"}\n}`,
	`{"b":1,"2":"x","1":{"4":0,"3":0},"0":[{"1":1,"0":0}],"b":2}`,
	`{"text":"é${SMILE} raw ✓","key é":"v","${SMILE}":1}`,
	`{"cmd":"echo a\tb\nc","path":"C:${B}Users${B}new","q":"${B}q${B}x"}`,
	`{"a":"x,","b":"y:","c":"z}","d":"w]"}`,
	`{"a":Infinity,"b":-Infinity,"c":NaN,"d":1e400,"e":-1e400}`,
	`{"path":"a.tex","content":"${B}usepackage{x} caf${B}u00e9 ${B}u12g ${B}url","mode":"w"}`,
	`{"__proto__":{"x":1},"y":2,"__proto__":3,"z":{"__proto__":null}}`,
	`[1,{"a":2},"b"]`,
	`"just a string"`,
	`12.5`,
	// Upstream's streaming-json.test.ts.
	`{"a":"x${B}u00e9y"}`,
	`{"a":"x${B}ny"}`,
	`{"a":"x${B}${B}y"}`,
	`{"a":"x${B}"y"}`,
	`{"a":"x${B}/y"}`,
	`{"a":"${B}ud83d${B}ude00!"}`,
	`{"a":"${SMILE}!"}`,
	`{"a":"${B}ud83d`,
	`{"code":"x\t\t  `,
	`{"a":"x\ny ${B}d ${B}u12"`,
	`{"text":"caf${B}u00e9 ${B}url"}`,
	`{"text":"ab${B}u00`,
	`{"__proto__":{"polluted":true},"b":{"__proto__":"x"}}`,
	`{"a":1,"b":2,"a":"three"}`,
	`{"n":12,"s":"abc","t":true} {"ignored":1}`,
	`{"a":[1,{"b":["x","yz"]}],"a":"again"}`,
	`{"a":"x${B}u00e9${B}n${B}${B}${B}"${B}ud83d${B}ude00${B}q"}`,
	`{"a":1 2, "b" "x"}{"c":3}`,
	`${'{"a":'.repeat(40)}"deep"${"}".repeat(40)}`,
	`${"[".repeat(34)}1,[2]${"]".repeat(34)}`,
	// Upstream's malformed texts.
	'{"a":1 "b":2}',
	'{"a" 1, "b": 2}',
	"{a:1, 'b': 2}",
	'{"a":1,,"b":2,}',
	"[1 2, , 3,]",
	'{"a":[1,2}, "b":3]',
	"]]]}}}{",
	'{"a":NaN,"b":Infinity,"c":-Infinity,"d":undefined,"e":1}',
	'{"a":01,"b":1.,"c":.5,"d":+1,"e":0x10,"f":1e}',
	'{"a":"x"} trailing garbage {"b":2}',
	'{"a": tru, "b": nulll, "c": falsey}',
	'{"a":{"b":{"c":',
	`{"a${B}": 1}`,
	`"unterminated ${B}`,
	'// comment\n{"a":1}',
	`${ch(0)}{${ch(1)}"a"${ch(2)}:${ch(3)}1}`,
	'{"a":"x"}}}]]]',
	// More: raw control characters, astral characters, escapes cut anywhere,
	// numbers and literals at every stage, key order, invalid positions.
	`{"c":"${ch(0)}${ch(1)}${ch(0x1f)}${ch(0x7f)}${ch(0x2028)}${ch(0x2029)}","d":"${B}u0000${B}u001F${B}u2028"}`,
	`{"e":"${SMILE}${SMILE}a${HIGH}b${LOW}c","${SMILE}k":"${B}uD83D${B}uDE00${B}ud83dx${B}ude00"}`,
	`{"x":"${B}b${B}f${B}r${B}t${B}${B}${B}/${B}a${B}${B}u${B}u0${B}u00${B}u004${B}u0041${B}uABCDz"}`,
	`{"n":[0,-0,1,-1,1.5,-1.5e10,2E-3,1e+21,1e21,1e-7,0.000001,5e-324,1.7976931348623157e308,1e309]}`,
	`[1e,2e+,3e-,4.,-,-0.,00,1.2.3,1e5e5,0.1e1]`,
	`[t,tr,tru,true,f,fa,fal,fals,false,n,nu,nul,null,truex,nan]`,
	`{"long":12345678901234567890123456789012345678901234567890123456789012345678901234567890}`,
	`{"b":1,"10":2,"2":3,"a":4,"4294967295":5,"4294967294":6,"01":7,"-1":8,"1.5":9}`,
	`{"a":{"b":[{"c":[[{"d":"e"}]]}]},"f":[[],[[]],{}],"g":{"h":{}}}`,
	`{"a":[1,2,3],"a":[4],"b":{"c":1},"b":"s"}`,
	`  \n\t {"ws" : [ 1 , 2 ] , "t" : " x " }  \n `,
	`{"a":"tail space   `,
	`{"a":tr`,
	`{"a":-`,
	`{"a":1e`,
	`{"a":1.`,
	`{"a":[nul`,
	`[[[[`,
	`{"a"`,
	`{"a":`,
	`{"a":"b",`,
	`{"a":"b","c`,
	`]`,
	`x {"a":1}`,
	`nope "s"`,
	`{"a":{} "b":[] "c":"d" 5}`,
	`{"a":1}garbage`,
	`[]`,
	`{}`,
	`""`,
	`null`,
	`true`,
	`-0`,
	` `,
	``,
];

// Upstream's partial-json and fixJson vectors.
const vectors: string[] = [
	'"',
	'["',
	'{"": "',
	"n",
	"t",
	"f",
	"0",
	"-1.25e+4",
	"-1.25e+",
	"-1.25e",
	`" ${B}x12`,
	"I",
	"-I",
	"N",
	"nul",
	"fals",
	"12.2",
	"-12",
	"-",
	"2.5e",
	"2.5e-",
	"2.5e3",
	"-2.5e3",
	"2.5E",
	"2.5E-",
	"2.5E3",
	"-2.5E3",
	"12.34e",
	"5e",
	'"abc',
	`"value with ${B}"quoted${B}" text and ${B}${B} escape`,
	`"value with ${B}`,
	`"${B}u`,
	`"${B}u12`,
	`"text ${B}u00`,
	`{"a":"${B}u12`,
	'"value with unicode <"',
	"[",
	"[[1], [2",
	'[["1"], ["2',
	"[[false], [nu",
	"[[[]], [[]",
	"[[{}], [{",
	"[1, ",
	"[[], 123",
	'{"key":',
	'{"a": {"b": 1}, "c": {"d": 2',
	'{"a": {"b": "1"}, "c": {"d": 2',
	'{"a": {"b": false}, "c": {"d": 2',
	'{"a": {"b": []}, "c": {"d": 2',
	'{"a": {"b": {}}, "c": {"d": 2',
	'{"ke',
	'{"k1": 1, "k2',
	'{"k1": 1, "k2":',
	'{"key": "value"  ',
	'{"a": {"b": {}',
	"[1, [2, 3, [",
	"[false, [true, [",
	'{"key": {"subKey":',
	'{"key": 123, "key2": {"subKey":',
	'{"key": null, "key2": {"subKey":',
	'{"key": [1, 2, {',
	'[1, 2, {"key": "value",',
	'{"a": {"b": ["c", {"d": "e",',
	'{"a": {"b": {"c": {"d":',
	'{"a": 1, "b": [',
	'{"a": 1, "b": {',
	'{"a": 1, "b": "',
	'{\n  "a": [\n    {\n      "a1": "v1",\n      "a2": "v2",\n      "a3": "v3"\n    }\n  ],\n  "b": [\n    {\n      "b1": "n',
	'{"type":"div","children":[{"type":"Card","props":{}',
	"12.",
	"12.e",
];

// Upstream's random documents (streaming-json.test.ts), from the same generator.
function random(seed: number): () => number {
	let state = seed;
	return () => {
		state = (state * 1103515245 + 12345) & 0x7fffffff;
		return state / 0x7fffffff;
	};
}
type Random = () => number;
const pick = <T,>(rand: Random, items: readonly T[]): T => items[Math.floor(rand() * items.length)]!;
const STRING_PIECES = ["a", "b", "x y", " ", "\n", "\t", B, '"', "/", "é", SMILE, ch(1), ch(0x1f), ch(0x2028), "{", "]", ","];
function randomString(rand: Random): string {
	let text = "";
	const length = Math.floor(rand() * 6);
	for (let index = 0; index < length; index++) text += pick(rand, STRING_PIECES);
	return text;
}
function randomScalar(rand: Random): unknown {
	return pick(rand, [
		() => randomString(rand),
		() => Math.floor(rand() * 2000) - 1000,
		() => rand() * 100,
		() => 1e21,
		() => -5e-8,
		() => -0,
		() => 0,
		() => true,
		() => false,
		() => null,
	])();
}
function randomValue(rand: Random, depth: number): unknown {
	const roll = rand();
	if (depth > 3 || roll < 0.3) return randomScalar(rand);
	if (roll < 0.65) {
		const members: Record<string, unknown> = {};
		const count = Math.floor(rand() * 4);
		for (let index = 0; index < count; index++) members[randomString(rand)] = randomValue(rand, depth + 1);
		return members;
	}
	return Array.from({ length: Math.floor(rand() * 4) }, () => randomValue(rand, depth + 1));
}
function render(value: unknown, rand: Random, spaced: boolean): string {
	const space = () => (spaced ? pick(rand, ["", "", " ", "\n  ", "\t", "\r\n"]) : "");
	if (Array.isArray(value)) {
		return `[${space()}${value.map((item) => render(item, rand, spaced)).join(`${space()},${space()}`)}${space()}]`;
	}
	if (value !== null && typeof value === "object") {
		const members = Object.entries(value).map(
			([key, item]) => `${JSON.stringify(key)}${space()}:${space()}${render(item, rand, spaced)}`,
		);
		return `{${space()}${members.join(`${space()},${space()}`)}${space()}}`;
	}
	return Object.is(value, -0) ? "-0" : JSON.stringify(value);
}
function randomDocuments(seed: number, count: number, spaced: boolean): string[] {
	const rand = random(seed);
	return Array.from({ length: count }, () =>
		render(rand() < 0.8 ? { path: randomString(rand), args: randomValue(rand, 0) } : randomValue(rand, 0), rand, spaced),
	);
}
// Upstream's malformed-escape documents.
function malformedDocuments(): string[] {
	const out: string[] = [];
	const rand = random(6);
	const malformed = ["\n", "\t", ch(0), `${B}d`, `${B}x41`, `${B}u12`, `${B}uZZZZ`, `${B}u00e9`, `${B}${B}`, `${B}q`];
	for (let round = 0; round < 20; round++) {
		let content = "";
		for (let index = 0; index < 6; index++) content += pick(rand, [...malformed, "a", " ", "é"]);
		out.push(`{"path":"p","content":"${content}","n":${round}}`);
	}
	return out;
}

// A piece as JSON can carry it: the string, or its code units when it holds a lone surrogate.
function piece(text: string): string | number[] {
	return text.isWellFormed() ? text : Array.from({ length: text.length }, (_, i) => text.charCodeAt(i));
}

type Mode = "all" | "final" | "size";
type Step = { changes?: string; snapshot?: string | null; size?: number; same: boolean };
type Source = { doc: number; size: number } | { doc: number; cuts: number[] } | { pieces: (string | number[])[] };
const rows: ({ name: string; mode: Mode; steps: Step[] } & Source)[] = [];

// The pieces of document `doc` cut into `size`-unit chunks, or at `cuts`.
function piecesOf(source: Source): string[] {
	if ("pieces" in source) throw new Error("unreachable");
	const text = corpus[source.doc]!;
	if ("size" in source) return chunks(text, source.size);
	const bounds = [0, ...source.cuts, text.length];
	return bounds.slice(1).map((end, i) => text.slice(bounds[i], end));
}

function run(name: string, input: string[] | Source, mode: Mode = "all"): void {
	const source: Source = Array.isArray(input) ? { pieces: input.map(piece) } : input;
	const pieces = Array.isArray(input) ? input : piecesOf(input);
	const parser = new StreamingJsonParser();
	const steps: Step[] = [];
	let previous: unknown = undefined;
	let previousText: string | null | undefined = undefined;
	pieces.forEach((text, index) => {
		const changes = parser.append(text);
		const snapshot = parser.snapshot();
		const same = index > 0 && snapshot === previous;
		previous = snapshot;
		if (mode === "all") {
			const text = snapshot === undefined ? null : JSON.stringify(snapshot);
			const step: Step = { same };
			if (changes.length > 0) step.changes = JSON.stringify(changes);
			if (index === 0 || text !== previousText) step.snapshot = text;
			previousText = text;
			steps.push(step);
		} else if (mode === "size") {
			steps.push({ size: snapshot === undefined ? -1 : JSON.stringify(snapshot).length, same });
		} else if (index === pieces.length - 1) {
			steps.push({ snapshot: snapshot === undefined ? null : JSON.stringify(snapshot), same });
		}
	});
	rows.push({ name, mode, ...source, steps });
}

const chunks = (text: string, size: number) => {
	const out: string[] = [];
	for (let i = 0; i < text.length; i += size) out.push(text.slice(i, i + size));
	return out.length === 0 ? [""] : out;
};

// Every document but the generated ones, up to 90 units, is also split in two at every position.
const handWritten = documents.length;
documents.push(...randomDocuments(1, 40, true), ...randomDocuments(2, 40, false), ...malformedDocuments());
const corpus = [...documents, ...vectors];
corpus.forEach((text, doc) => {
	run(`doc${doc} chars`, { doc, size: 1 });
	run(`doc${doc} whole`, { doc, cuts: [] });
	run(`doc${doc} by3`, { doc, size: 3 });
	run(`doc${doc} by7`, { doc, size: 7 });
	if ((doc < handWritten || doc >= documents.length) && text.length <= 90) {
		for (let split = 0; split <= text.length; split++) run(`doc${doc} split${split}`, { doc, cuts: [split] });
	}
});

{
	const text = `{"a":"x${B}u00e9${B}n${B}${B}${B}"${B}ud83d${B}ude00${B}q"}`;
	const doc = corpus.indexOf(text);
	for (let first = 1; first < text.length; first++) {
		for (let second = first; second < text.length; second++) {
			run(`escapes ${first},${second}`, { doc, cuts: [first, second] });
		}
	}
}

const suite = JSON.parse(fs.readFileSync(path.join(extraction, "packages/ai/test/data/json-test-suite.json"), "utf8")) as {
	files: Record<string, string>;
};
for (const [name, base64] of Object.entries(suite.files)) {
	const text = Buffer.from(base64, "base64").toString("utf8");
	run(`suite ${name} whole`, [text]);
	run(`suite ${name} by1`, chunks(text, 1), "final");
	run(`suite ${name} by3`, chunks(text, 3), "final");
}

{
	const items = JSON.stringify({ items: Array.from({ length: 3000 }, (_, i) => i) });
	run("lag items by16", chunks(items, 16), "size");
	// Credit is earned per UTF-16 code unit: an astral character earns two.
	const astral = JSON.stringify({ items: Array.from({ length: 1500 }, (_, i) => (i % 3 === 0 ? SMILE : `${SMILE}é`)) });
	run("lag astral by9", chunks(astral, 9), "size");
	const content = Array.from({ length: 400 }, (_, line) => `const line${line} = "value ${line}"; // ${B} done\n`).join("");
	run("write content by32", chunks(JSON.stringify({ path: "/src/file.ts", content }), 32), "size");
	const nested = Array.from({ length: 600 }, (_, i) => `{"k${i}":[${i},`).join("");
	run("lag nested by5", chunks(nested, 5), "size");
	const deep = `${"[".repeat(2000)}1${"]".repeat(2000)}`;
	run("deep 2000 by7", chunks(deep, 7), "final");
}

// ToolCallArgumentsStream.
// An "appendValue" op appends `value`, a JSON value that is not a string, as
// an adapter hands pi's append a delta an event held. A row with `sizes`
// records each append's arguments as their JSON length.
type ArgsOp = { op: "append" | "appendValue" | "replace" | "finish"; text?: string | number[]; value?: unknown };
const argsRows: ({ name: string; results: unknown[] } & ({ ops: ArgsOp[] } | { doc: number }))[] = [];
let control = 0;
let controlSteps = 0;

// A row for document `doc` appends it one code unit at a time and finishes.
function runArgs(
	name: string,
	input: { op: "append" | "appendValue" | "replace" | "finish"; text?: string; value?: unknown }[] | number,
	checkControl = false,
	sizes = false,
): void {
	const doc = typeof input === "number" ? input : undefined;
	const ops = typeof input === "number" ? [...chunks(corpus[input]!, 1).map((text) => ({ op: "append" as const, text })), { op: "finish" as const }] : input;
	const args = new ToolCallArgumentsStream();
	let previous: unknown = undefined;
	const results: unknown[] = [];
	for (const { op, text, value: appended } of ops as { op: string; text?: string; value?: unknown }[]) {
		if (op === "append" || op === "appendValue") {
			const value = args.append((op === "append" ? text : appended) as string);
			results.push(sizes ? { size: JSON.stringify(value).length, same: value === previous } : { arguments: JSON.stringify(value), same: value === previous });
			if (checkControl) {
				controlSteps++;
				if (JSON.stringify(value) !== JSON.stringify(parseStreamingJson(args.text))) control++;
			}
			previous = value;
		} else if (op === "replace") {
			const { delta, arguments: value } = args.replace(text!);
			results.push({ delta: delta === undefined ? null : delta, arguments: JSON.stringify(value), same: value === previous, text: args.text });
			previous = value;
		} else {
			results.push(JSON.stringify(args.finish()));
		}
	}
	const source =
		doc !== undefined
			? { doc }
			: { ops: (ops as { op: string; text?: string; value?: unknown }[]).map((o) => (o.op === "appendValue" ? { op: o.op, value: o.value } : o.text === undefined ? { op: o.op } : { op: o.op, text: piece(o.text) })) };
	argsRows.push({ name, ...source, results });
}

corpus.forEach((_, doc) => runArgs(`doc${doc} chars`, doc, true));
runArgs("upstream partial", [
	{ op: "append", text: "  " },
	{ op: "append", text: '{"path":"a.ts","content":"x' },
	{ op: "append", text: "y" },
	{ op: "append", text: "" },
	{ op: "finish" },
	{ op: "append", text: '"}' },
	{ op: "finish" },
]);
runArgs("upstream non-object", [{ op: "append", text: "[1,2" }, { op: "append", text: "]" }, { op: "finish" }]);
runArgs("upstream replace", [
	{ op: "append", text: '{"a":"x' },
	{ op: "replace", text: '{"a":"xyz"}' },
	{ op: "replace", text: '{"b":2}' },
	{ op: "replace", text: '{"b":2}' },
	{ op: "replace", text: '{"b":2} ' },
	{ op: "finish" },
	{ op: "replace", text: "" },
	{ op: "finish" },
]);
runArgs("replace first", [{ op: "replace", text: '{"q":[1,' }, { op: "append", text: "2]}" }, { op: "finish" }]);
runArgs("replace shorter", [{ op: "append", text: '{"q":"abc"}' }, { op: "replace", text: '{"q":"ab' }, { op: "finish" }]);
runArgs("scalar then object", [
	{ op: "append", text: "12" },
	{ op: "append", text: " " },
	{ op: "append", text: '{"a":1}' },
	{ op: "finish" },
]);
runArgs("malformed finish", [{ op: "append", text: '{"a":tru' }, { op: "append", text: "e,}" }, { op: "finish" }]);
runArgs("repair finish", [{ op: "append", text: '{"a":"x\ty' }, { op: "append", text: `${B}d"}` }, { op: "finish" }]);
runArgs("empty finish", [{ op: "finish" }]);
runArgs("blank finish", [{ op: "append", text: " \n" }, { op: "finish" }]);
runArgs("split pair", [
	{ op: "append", text: `{"s":"a${HIGH}` },
	{ op: "append", text: `${LOW}b"` },
	{ op: "append", text: "}" },
	{ op: "finish" },
]);
// A delta that is not a string: the text grows by its String(), the parser
// reads nothing of it, and the copy credit becomes NaN, so the preview of a
// large open array stops lagging (the row without the value lags).
{
	const head = '{"a":[';
	const tail = `${"1,".repeat(900)}1]}`.split("");
	const ops = (value: unknown[]) => [
		{ op: "append" as const, text: head },
		...value.map((v) => ({ op: "appendValue" as const, value: v })),
		...tail.map((text) => ({ op: "append" as const, text })),
		{ op: "finish" as const },
	];
	runArgs("value disables lag", ops([5]), false, true);
	runArgs("lag without value", ops([]), false, true);
	runArgs("values", [
		{ op: "append", text: '{"a":"x' },
		{ op: "appendValue", value: 5 },
		{ op: "appendValue", value: true },
		{ op: "appendValue", value: { b: 1 } },
		{ op: "append", text: '"}' },
		{ op: "finish" },
	]);
}
runArgs("infinity", [{ op: "append", text: '{"a":1e400,"b":[-1e999' }, { op: "append", text: "]}" }, { op: "finish" }]);
if (control === 0) {
	console.error("control: parseStreamingJson agrees with ToolCallArgumentsStream on every step; the corpus pins nothing");
	process.exit(1);
}

const out = {
	source: `pi ${sha} packages/ai/src/utils/streaming-json.ts (StreamingJsonParser, ToolCallArgumentsStream), node ${process.version}`,
	partialJSON: { version: partialJSON, integrity: locked.integrity },
	control: `${control} of ${controlSteps} args steps differ from parseStreamingJson of the same text`,
	documents: corpus.map(piece),
	rows,
	args: argsRows,
};
fs.writeFileSync(outFile, `${JSON.stringify(out)}\n`);
console.log(`${rows.length} parser rows, ${argsRows.length} args rows; control: ${out.control}`);
