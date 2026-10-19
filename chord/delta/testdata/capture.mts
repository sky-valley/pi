// Captures what real pi's chord/delta does — the overlay tracker and its
// drafts, and diffRevisions — the oracle behind chord/delta/golden_test.go.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> upstream_delta.json d5cba1d97
//
// <extraction> is a src extraction of packages/chord at <sha>:
//   git -C ~/.cache/pi-upstream archive -o chord.tar <sha> packages/chord
//   tar -xf chord.tar -C <dir>
// packages/chord/src/delta imports nothing outside packages/chord/src but
// type-only names (erased by --experimental-strip-types) and, from 19a0361be
// on, ../json.ts, and no third-party package, so it needs no node_modules and
// no lockfile pinning. No npm build carries these shas: they landed after
// v0.87.1 (f07218c4d).
//
// upstream_delta.json names the sha it was captured at in its "sha" field;
// the header of golden_test.go says which, and under which node.
//
// What is captured, and how the Go test reads it:
//
//   - "diffs": diffRevisions over arbitrary revision pairs. A case's values may
//     share objects by identity through {"$ref": name} into its "refs" table —
//     the diff anchors on identity, so a shared object is not the same input as
//     an equal copy. "gen" cases build large inputs by name; golden_test.go
//     builds the same ones.
//   - "scenarios": scripts of tracker/draft steps (the step language below),
//     run against pi's track(); every step's result — ops, values, returned
//     values, or the error message — is recorded.
//   - "generated": tracker cases too large to record, built by name here and
//     in golden_test.go, recorded as their ops (or a hash) and a value hash.
//   - "fuzz": state-fuzz.test.ts's randomized revisions, seed by seed, as a
//     hash of every step's ops and value.
//   - "differential": random scripts over a richer document and every draft
//     operation, generated here against pi's live draft and recorded whole, so
//     Go replays exactly what pi ran.
//
// Canonical JSON. Values and ops are written the way Go's encoding/json writes
// them (object keys in byte order, U+2028/U+2029 escaped, -0 as "-0"), so the
// Go test compares bytes. A tracker's batches are pi's own, in pi's order: the
// overlay emits them in the order the change made its edits, which the Go
// overlay keeps too. diffRevisions walks an object's members in insertion
// order, which a Go map does not have: that is a recorded divergence
// (docs/UPSTREAM.md, D69), and it is the ONE normalization applied to pi's
// output — a diff's "ops" list each object's member ops in Go's enumeration
// order (integer-like keys ascending, then the rest by UTF-16 code unit;
// deletes after sets, as diffRevisions emits them), and "raw" keeps pi's
// actual order wherever the two differ.
//
// Step language (a step is a JSON object; "t", "c", "p" name the tracker,
// change and prepared change and default to "main"):
//   begin | prepare | adopt | abort | value | revision lifecycle; value and revision read the tracker
//   replace {value}                                   tracker.prepareReplace(value)
//   abortPrepared | baseRevision                      prepared.abort(), prepared.baseRevision
//   shared {at}               whether prepared.value holds prepared.base's container at "at"
//   keys {at}                 Object.keys of the draft at "at"
//   set {at, key, value}      append {at, key, text}  delete {at, key}      get {at, key}
//   push {at, items}          unshift {at, items}     pop {at, as?}         shift {at, as?}
//   splice {at, start, deleteCount, items}            reverse {at}          setLength {at, length}
//   sort {at, by, desc?, bump?, write?, unshiftOnce?} fill {at, value, start, end}
//     (by: a numeric field, "." for the element itself, or null for the
//     default order; bump: a field the comparator increments on both sides;
//     write: [index, value] the comparator writes over an element, every call;
//     unshiftOnce: items the comparator unshifts on its first call. Each is
//     the same whichever pairs the sort compares, and how often: V8 and Go
//     sort by different algorithms — D74)
//   copyWithin {at, target, start, end}               hold {at, as}
// "at" is a path of keys and indices from the change's root, or from the held
// draft named by "from". Values may hold {"$held": name} (that draft),
// {"$nan": true}, {"$inf": true}, {"$date": true}, {"$cycle": true}, and
// {"$repeat": [text, count]}.
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const delta = await import(pathToFileURL(path.join(extraction, "packages/chord/src/delta/index.ts")).href);
const { track, diffRevisions, applyImmutable } = delta;

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
type Op = readonly unknown[];

// ─── Canonical JSON ──────────────────────────────────────────────────────────

const byteOrder = (a: string, b: string): number => Buffer.compare(Buffer.from(a, "utf8"), Buffer.from(b, "utf8"));

// U+2028 and U+2029, which Go escapes and JSON.stringify does not.
const LS = String.fromCharCode(0x2028);
const PS = String.fromCharCode(0x2029);

function canon(v: unknown): string {
	if (v === null) return "null";
	switch (typeof v) {
		case "number":
			return Object.is(v, -0) ? "-0" : JSON.stringify(v);
		case "boolean":
			return String(v);
		case "string":
			return JSON.stringify(v).split(LS).join("\\u2028").split(PS).join("\\u2029");
		case "object": {
			if (Array.isArray(v)) {
				const parts: string[] = [];
				for (let i = 0; i < v.length; i++) parts.push(canon(v[i]));
				return `[${parts.join(",")}]`;
			}
			const record = v as Record<string, unknown>;
			const keys = Object.keys(record).sort(byteOrder);
			return `{${keys.map((k) => `${canon(k)}:${canon(record[k])}`).join(",")}}`;
		}
	}
	throw new Error(`not JSON: ${typeof v}`);
}

const hash = (text: string): string => createHash("sha256").update(text).digest("hex");

// ─── Go's member order ───────────────────────────────────────────────────────

const canonicalIndex = (s: string): number | undefined => {
	const n = Number(s);
	return Number.isInteger(n) && n >= 0 && n <= 2 ** 32 - 2 && String(n) === s ? n : undefined;
};

const goKeyOrder = (a: string, b: string): number => {
	const ia = canonicalIndex(a);
	const ib = canonicalIndex(b);
	if (ia !== undefined && ib !== undefined) return ia - ib;
	if (ia !== undefined) return -1;
	if (ib !== undefined) return 1;
	return a < b ? -1 : a > b ? 1 : 0; // UTF-16 code units, as JavaScript compares
};

const pathOf = (op: Op): readonly (string | number)[] => (op[0] === "r" ? [] : (op[1] as (string | number)[]));

const samePrefix = (a: readonly unknown[], b: readonly unknown[], n: number): boolean => {
	for (let i = 0; i < n; i++) if (a[i] !== b[i]) return false;
	return true;
};

// Reorder each object's member ops into Go's order. diffObject emits, for one
// object at path P, a contiguous run: one group per member of after (the ops
// of that member's diff, all under P+[key]) in enumeration order, then one "d"
// per member only before holds. Object members are string segments; array
// elements are numbers, and their order is left alone.
function goOrder(ops: readonly Op[], depth = 0): Op[] {
	const out: Op[] = [];
	let i = 0;
	while (i < ops.length) {
		const p = pathOf(ops[i]!);
		if (p.length > depth && typeof p[depth] === "string") {
			const groups: { key: string; ops: Op[]; del: boolean }[] = [];
			while (i < ops.length) {
				const q = pathOf(ops[i]!);
				if (!(q.length > depth && typeof q[depth] === "string" && samePrefix(q, p, depth))) break;
				const key = q[depth] as string;
				const last = groups[groups.length - 1];
				if (last !== undefined && last.key === key) last.ops.push(ops[i]!);
				else groups.push({ key, ops: [ops[i]!], del: false });
				i++;
			}
			for (const g of groups) g.del = g.ops.length === 1 && g.ops[0]![0] === "d" && pathOf(g.ops[0]!).length === depth + 1;
			groups.sort((a, b) => (a.del === b.del ? goKeyOrder(a.key, b.key) : a.del ? 1 : -1));
			for (const g of groups) out.push(...goOrder(g.ops, depth + 1));
		} else if (p.length > depth) {
			const run: Op[] = [];
			while (i < ops.length) {
				const q = pathOf(ops[i]!);
				if (!(q.length > depth && samePrefix(q, p, depth + 1))) break;
				run.push(ops[i]!);
				i++;
			}
			out.push(...goOrder(run, depth + 1));
		} else {
			out.push(ops[i]!);
			i++;
		}
	}
	return out;
}

// The record of one batch: its ops in Go's member order, and pi's own order
// when that differs. Large batches are recorded by hash.
type Batch = { ops?: unknown; raw?: unknown; opsHash?: string; opsBytes?: number; opsHead?: string };
const LARGE = 16_384;
function batch(ops: readonly Op[]): Batch {
	const ordered = goOrder(ops);
	const text = canon(ordered);
	const rawText = canon(ops);
	const out: Batch = {};
	if (text.length > LARGE) {
		out.opsHash = hash(text);
		out.opsBytes = text.length;
		out.opsHead = text.slice(0, 120);
		if (rawText !== text) out.raw = { hash: hash(rawText) };
		return out;
	}
	out.ops = RAW(text);
	if (rawText !== text) out.raw = RAW(rawText);
	return out;
}

// The record of a tracker's batch: pi's ops in pi's order. Large batches are
// recorded by hash.
function trackerBatch(ops: readonly Op[]): Batch {
	const text = canon(ops);
	if (text.length > LARGE) return { opsHash: hash(text), opsBytes: text.length, opsHead: text.slice(0, 120) };
	return { ops: RAW(text) };
}

// RAW marks canonical text to splice into the output verbatim.
class Raw {
	readonly text: string;
	constructor(text: string) {
		this.text = text;
	}
}
const RAW = (text: string): Raw => new Raw(text);

// write serializes the golden itself: canonical JSON, with Raw spliced in.
function write(v: unknown, indent = ""): string {
	if (v instanceof Raw) return v.text;
	if (Array.isArray(v)) {
		if (v.length === 0) return "[]";
		const oneLine = `[${v.map((x) => write(x, "")).join(",")}]`;
		if (oneLine.length <= 400 && !oneLine.includes("\n")) return oneLine;
		const inner = `${indent}\t`;
		return `[\n${v.map((x) => inner + write(x, inner)).join(",\n")}\n${indent}]`;
	}
	if (v !== null && typeof v === "object") {
		const keys = Object.keys(v as object).filter((k) => (v as Record<string, unknown>)[k] !== undefined);
		if (keys.length === 0) return "{}";
		const oneLine = `{${keys.map((k) => `${JSON.stringify(k)}:${write((v as Record<string, unknown>)[k], "")}`).join(",")}}`;
		if (oneLine.length <= 400 && !oneLine.includes("\n")) return oneLine;
		const inner = `${indent}\t`;
		return `{\n${keys.map((k) => `${inner}${JSON.stringify(k)}: ${write((v as Record<string, unknown>)[k], inner)}`).join(",\n")}\n${indent}}`;
	}
	return canon(v);
}

// ─── Values with markers ─────────────────────────────────────────────────────

const J = (text: string): Json => JSON.parse(text) as Json;

type Env = { held: Map<string, unknown>; refs?: Record<string, Json>; built?: Map<string, unknown> };

function build(spec: unknown, env: Env): unknown {
	if (spec === null || typeof spec !== "object") return spec;
	if (Array.isArray(spec)) return spec.map((x) => build(x, env));
	const o = spec as Record<string, unknown>;
	if ("$ref" in o) {
		const name = o.$ref as string;
		env.built ??= new Map();
		if (!env.built.has(name)) env.built.set(name, build(env.refs![name], env));
		return env.built.get(name);
	}
	if ("$held" in o) return env.held.get(o.$held as string);
	if ("$nan" in o) return Number.NaN;
	if ("$inf" in o) return Number.POSITIVE_INFINITY;
	if ("$date" in o) return new Date(0);
	if ("$cycle" in o) {
		const cyclic: Record<string, unknown> = {};
		cyclic.self = cyclic;
		return cyclic;
	}
	if ("$repeat" in o) {
		const [text, count] = o.$repeat as [string, number];
		return text.repeat(count);
	}
	// Built through JSON.parse so that a "__proto__" key is an own property,
	// as it is in a parsed JSON document.
	const out = JSON.parse("{}") as Record<string, unknown>;
	for (const key of Object.keys(o)) {
		Object.defineProperty(out, key, { value: build(o[key], env), writable: true, enumerable: true, configurable: true });
	}
	return out;
}

// ─── Diffs ───────────────────────────────────────────────────────────────────

const diffs: unknown[] = [];

function diffCase(name: string, refs: Record<string, Json>, before: Json, after: Json): void {
	const env: Env = { held: new Map(), refs };
	const b = build(before, env) as Json;
	const a = build(after, env) as Json;
	const ops = diffRevisions(b, a) as Op[];
	const replayed = applyImmutable(b, ops);
	// Through JSON first: a replica converges on the JSON value, where -0 is 0.
	const json = (v: unknown) => canon(JSON.parse(JSON.stringify(v)));
	if (json(replayed) !== json(a)) throw new Error(`${name}: pi's own ops do not replay`);
	diffs.push({ name, refs: Object.keys(refs).length > 0 ? refs : undefined, before, after, ...batch(ops) });
}

function genCase(name: string, before: Json, after: Json): void {
	const ops = diffRevisions(before, after) as Op[];
	diffs.push({ name, gen: name, ...batch(ops) });
}

const R = (name: string): Json => ({ $ref: name });
const ids = (...names: string[]): Record<string, Json> => Object.fromEntries(names.map((n) => [n, { id: n }]));

// state-diff.test.ts, case for case.
diffCase("sets and deletes", {}, J(`{"keep":1,"change":1,"remove":true}`), J(`{"keep":1,"change":2,"add":3}`));
diffCase("string append", {}, J(`{"text":"hello"}`), J(`{"text":"hello world"}`));
diffCase("string front truncation", {}, J(`{"text":"hello world"}`), J(`{"text":"world"}`));
diffCase("string rolling window", {}, J(`{"text":"abcdefgh"}`), J(`{"text":"defghxyz"}`));
diffCase("array insertion", ids("a", "b", "c"), { values: [R("a"), R("b")] }, { values: [R("a"), R("c"), R("b")] });
diffCase("array removal", ids("a", "b", "c"), { values: [R("a"), R("b"), R("c")] }, { values: [R("b"), R("c")] });
diffCase("queue update", ids("a", "b", "c", "d"), { values: [R("a"), R("b"), R("c")] }, { values: [R("b"), R("c"), R("d")] });
diffCase("pure reorder", ids("a", "b", "c"), { values: [R("a"), R("b"), R("c")] }, { values: [R("c"), R("a"), R("b")] });
diffCase(
	"reordered distinct deeply-equal objects",
	{ first: J(`{"nested":{"value":1}}`), second: J(`{"nested":{"value":1}}`) },
	{ values: [R("first"), R("second")] },
	{ values: [R("second"), R("first")] },
);
diffCase("reconstructed equal values", {}, J(`{"value":{"nested":[1,2]}}`), J(`{"value":{"nested":[1,2]}}`));
diffCase("reconstructed equal rows", {}, J(`{"values":[{"id":1},{"id":2}]}`), J(`{"values":[{"id":1},{"id":2}]}`));
diffCase("reconstructed equal scalars", {}, J(`{"values":[true,true,true]}`), J(`{"values":[true,true,true]}`));
diffCase(
	"leaf edit in reconstructed array",
	{},
	J(`{"values":[{"id":1,"label":"one"},{"id":2,"label":"two"}]}`),
	J(`{"values":[{"id":1,"label":"one"},{"id":2,"label":"changed"}]}`),
);
diffCase(
	"scattered removals",
	ids("a", "b", "c", "d", "e"),
	{ values: [R("a"), R("b"), R("c"), R("d"), R("e")] },
	{ values: [R("a"), R("c"), R("e")] },
);
for (const [name, before, after] of [
	["front", [1, 2, 3, 4], [2, 3, 4]],
	["tail", [1, 2, 3, 4], [1, 2, 3]],
	["middle", [1, 2, 3, 4], [1, 3, 4]],
	["all", [1, 2, 3, 4], []],
	["none", [1, 2, 3, 4], [1, 2, 3, 4]],
] as const) {
	diffCase(`${name} removal`, {}, { values: [...before] }, { values: [...after] });
}
diffCase(
	"unrelated shifted objects with common fields",
	{},
	J(`{"values":[{"type":"row","value":1},{"type":"row","value":2},{"type":"row","value":3}]}`),
	J(`{"values":[{"type":"row","value":2},{"type":"row","value":3},{"type":"row","value":4}]}`),
);
diffCase(
	"coincidental id and key fields",
	{ left: J(`{"value":"left"}`), right: J(`{"value":"right"}`) },
	{ values: [R("left"), J(`{"id":1,"key":"a","value":"first"}`), J(`{"id":2,"key":"b","value":"second"}`), R("right")] },
	{
		values: [R("left"), J(`{"id":2,"key":"b","value":"edited-second"}`), J(`{"id":1,"key":"a","value":"edited-first"}`), R("right")],
	},
);
diffCase(
	"removals, append and a shared-subtree survivor edit",
	{
		as: {},
		bs: {},
		cs: {},
		ds: {},
		es: {},
		a: { id: "a", stable: R("as"), detail: { text: "a" } },
		b: { id: "b", stable: R("bs"), detail: { text: "b" } },
		c: { id: "c", stable: R("cs"), detail: { text: "c" } },
		d: { id: "d", stable: R("ds"), detail: { text: "d" } },
		changedC: { id: "c", stable: R("cs"), detail: { text: "changed" } },
		e: { id: "e", stable: R("es"), detail: { text: "e" } },
	},
	{ values: [R("a"), R("b"), R("c"), R("d")] },
	{ values: [R("a"), R("changedC"), R("d"), R("e")] },
);
diffCase(
	"ambiguous equal-count moved-and-edited gap",
	{ left: J(`{"value":"left"}`), right: J(`{"value":"right"}`) },
	{ values: [R("left"), J(`{"id":1,"value":"a"}`), J(`{"id":2,"value":"b"}`), R("right")] },
	{ values: [R("left"), J(`{"id":2,"value":"edited"}`), J(`{"id":1,"value":"also-edited"}`), R("right")] },
);

// Arbitrary revision pairs beyond the upstream suite.
diffCase("root string", {}, "a", "b");
diffCase("root string append", {}, "abc", "abcd");
diffCase("root number", {}, 1, 2);
diffCase("root kind change", {}, J(`[1,2]`), J(`{"a":1}`));
diffCase("root identical scalar", {}, 5, 5);
diffCase("root array splice", {}, J(`[1,2,3]`), J(`[1,9,3,4]`));
diffCase("root array replaced", {}, J(`[1,2]`), J(`[3,4]`));
diffCase("root array emptied", {}, J(`[1,2]`), J(`[]`));
diffCase("root array filled", {}, J(`[]`), J(`[1,2]`));
diffCase("root permutation with duplicates", {}, J(`[1,2,1,3]`), J(`[3,1,2,1]`));
diffCase("permutation of objects keeps identity", ids("a", "b", "c", "d"), [R("a"), R("b"), R("c"), R("d")], [R("d"), R("b"), R("c"), R("a")]);
diffCase("equal-count scalar swap", {}, J(`{"v":[1,2,3,4]}`), J(`{"v":[1,3,2,4]}`));
diffCase("type change below root", {}, J(`{"a":"1","b":{"x":1},"c":null,"d":[1]}`), J(`{"a":1,"b":[1],"c":{"x":1},"d":"x"}`));
diffCase("astral append", {}, J(`{"t":"a😀b"}`), J(`{"t":"a😀bc😀"}`));
diffCase("astral rolling window", {}, J(`{"t":"😀😁😂🤣😃"}`), J(`{"t":"😂🤣😃😄"}`));
diffCase("string shrink without overlap", {}, J(`{"t":"abcdef"}`), J(`{"t":"xy"}`));
diffCase("string to empty", {}, J(`{"t":"abc"}`), J(`{"t":""}`));
diffCase("string from empty", {}, J(`{"t":""}`), J(`{"t":"abc"}`));
diffCase("string at array index", {}, J(`{"v":["ab","cd"]}`), J(`{"v":["abX","cd"]}`));
diffCase("integer-like keys, sorted insertion", {}, J(`{"9":1,"10":1,"a":1,"b":1}`), J(`{"9":2,"10":2,"a":2,"b":2}`));
diffCase("integer-like keys added", {}, J(`{"a":1}`), J(`{"a":1,"2":2,"10":3,"1":4}`));
diffCase("member order, unsorted insertion", {}, J(`{"b":1,"a":1}`), J(`{"b":2,"a":2}`));
diffCase("sets, adds and deletes in one object", {}, J(`{"z":1,"m":1,"a":1}`), J(`{"m":2,"q":1}`));
diffCase("nested member order", {}, J(`{"o":{"y":{"q":1,"p":1},"x":1}}`), J(`{"o":{"y":{"q":2,"p":2},"x":2}}`));
diffCase("reserved key object is set whole", {}, J(`{"x":{"__proto__":1,"v":1}}`), J(`{"x":{"__proto__":1,"v":2}}`));
diffCase("reserved key object unchanged", {}, J(`{"x":{"__proto__":1,"v":1}}`), J(`{"x":{"__proto__":1,"v":1}}`));
diffCase("reserved key at root", {}, J(`{"constructor":1}`), J(`{"constructor":2}`));
diffCase("reserved key added", {}, J(`{"x":{"v":1}}`), J(`{"x":{"v":1,"prototype":true}}`));
diffCase("nested arrays", {}, J(`{"grid":[[1,2],[3,4],[5]]}`), J(`{"grid":[[1,2,9],[3],[5],[6]]}`));
diffCase(
	"shared rows with a moved survivor",
	{ s1: {}, s2: {}, s3: {}, r1: { id: 1, s: R("s1") }, r2: { id: 2, s: R("s2") }, r3: { id: 3, s: R("s3") } },
	{ rows: [R("r1"), R("r2"), R("r3")] },
	{ rows: [R("r3"), { id: 1, s: R("s1"), x: 1 }, R("r2")] },
);
diffCase("null and false members", {}, J(`{"a":null,"b":false,"c":0}`), J(`{"a":false,"b":null,"c":""}`));
// SameValueZero: a permutation matches -0 to 0.
diffCase("permutation matches -0 and 0", {}, { v: [0, 1] }, { v: [1, -0] });
// UTF-16 order, not UTF-8: U+FF61 sorts after an astral character's high surrogate.
diffCase("astral and high-BMP keys", {}, J(`{"😀":1,"｡":1,"a":1}`), J(`{"😀":2,"｡":2,"a":2}`));
diffCase("negative zero", {}, J(`{"a":0}`), { a: -0 });
diffCase("large numbers", {}, J(`{"a":1,"b":1}`), J(`{"a":1e21,"b":1.5e-7}`));
// Every [] JSON.parse makes is an array of its own, so none anchors an
// alignment by identity - though encoding/json gives every [] it decodes the
// same address. A [] shared through a ref IS one array, and anchors.
diffCase("parsed empty arrays are distinct", {}, J(`[1,[],2]`), J(`[2,[],1]`));
diffCase("parsed empty arrays are distinct, nested", {}, J(`{"a":[1,[],2,[]]}`), J(`{"a":[[],2,[],1]}`));
diffCase("parsed empty arrays do not align", {}, J(`[[[],1]]`), J(`[[[],2],3]`));
diffCase("parsed empty arrays do not align, nested", {}, J(`{"a":[[[],1]]}`), J(`{"a":[[[],2],3]}`));
diffCase("parsed empty arrays do not permute", {}, J(`{"v":[[],1,[]]}`), J(`{"v":[[],1,[],[]]}`));
diffCase("a shared empty array anchors", { e: [] }, { v: [R("e"), 1] }, { v: [1, R("e")] });
diffCase("a shared empty array aligns", { e: [] }, { v: [[R("e"), 1]] }, { v: [[R("e"), 2], 3] });
// Two sibling leaves four levels down: each op keeps a path of its own.
diffCase("sibling leaves four levels down", {}, J(`{"a":{"b":{"c":{"x":1,"y":1}}}}`), J(`{"a":{"b":{"c":{"x":2,"y":2}}}}`));
// A reserved key on either side sets the object whole (diff.ts:428 checks
// both key lists).
diffCase("reserved key only before", {}, J(`{"o":{"__proto__":1,"a":1}}`), J(`{"o":{"a":2}}`));
// Keys that look like array indices and are not ("01", 2^32 - 1, "-1",
// "1.5") enumerate with the strings.
diffCase(
	"index-like keys that are not indices",
	{},
	J(`{"1":0,"4294967294":0,"01":0,"1.5":0,"-1":0,"4294967295":0,"a":0}`),
	J(`{"1":1,"4294967294":1,"01":1,"1.5":1,"-1":1,"4294967295":1,"a":1}`),
);

// Large generated inputs; golden_test.go builds the same ones by name.
{
	const retained = (payload: string) => ["a", "b", "c", "d", "e"].map((id) => ({ id, payload }));
	for (const size of [256 * 1024, 1024 * 1024]) {
		const r = retained("x".repeat(size));
		genCase(`retained-payload-${size}`, { values: r }, { values: [r[0]!, r[2]!, r[4]!] });
	}
	for (const size of [1_001, 10_000]) {
		const values = Array.from({ length: size }, (_, value) => ({ value }));
		genCase(`narrow-push-${size}`, { values }, { values: [...values, { value: size }] });
		genCase(`narrow-pop-${size}`, { values }, { values: values.slice(0, -1) });
		const middle = Math.floor(size / 2);
		genCase(`narrow-middle-${size}`, { values }, { values: [...values.slice(0, middle), ...values.slice(middle + 1)] });
	}
	const rows = Array.from({ length: 40_000 }, (_, value) => ({ value, stable: { value } }));
	const sparse = rows.slice();
	for (let index = 100; index < sparse.length; index += 400) sparse[index] = { value: -index, stable: rows[index]!.stable };
	genCase("sparse-40000", { values: rows }, { values: sparse });
	const labels = Array.from({ length: 1_000 }, (_, value) => ({ value, label: `row-${value}` }));
	const edited = structuredClone(labels);
	edited[700]!.label = "changed";
	genCase("reconstructed-1000", { values: labels }, { values: edited });
	const payloadRows = Array.from({ length: 10_000 }, (_, value) => ({ value, payload: "x".repeat(100) }));
	const inserted = Array.from({ length: 500 }, (_, value) => ({ value: -value - 1 }));
	genCase("unshift-500", { values: payloadRows }, { values: [...inserted, ...payloadRows] });
	const wideBefore: Record<string, Json> = {};
	const wideAfter: Record<string, Json> = {};
	for (let index = 0; index < 20_000; index++) {
		wideBefore[`field${index}`] = 0;
		wideAfter[`field${index}`] = 1;
	}
	genCase("wide-object-20000", wideBefore, wideAfter);
	genCase(
		"base-fallback-40000",
		{ values: Array.from({ length: 40_000 }, () => 0) },
		{ values: Array.from({ length: 40_000 }, () => 1) },
	);
	// The operation cap: 4,096 ops publish; one more is a replacement.
	for (const n of [4_096, 4_097]) {
		const before: Record<string, Json> = {};
		const after: Record<string, Json> = {};
		for (let i = 0; i < n; i++) {
			before[i.toString(36)] = 0;
			after[i.toString(36)] = 1;
		}
		genCase(`op-cap-${n}`, before, after);
	}
	// The snapshot threshold, per op kind: a batch of that kind costing over
	// 65,536 against a snapshot padded by an unchanged member "k3" of L units.
	// Which side of the threshold a batch lands on is wire-visible, so each
	// kind's cost formula is pinned by the two cases either side of the L where
	// pi's answer flips (found here by scanning), and two more around them.
	const cost = pinCosts();
	for (const [kind, flip] of cost) {
		for (let L = Math.max(0, flip - 2); L <= flip + 2; L++) {
			const [before, after] = costCase(kind, L);
			genCase(`cost-${kind}-${L}`, before, after);
		}
	}
	// The snapshot threshold itself: one append whose batch costs n + 17,
	// against a snapshot of n + 15. Below 65,536 a batch is published without
	// the comparison; at exactly 65,536 it is compared, and loses.
	const threshold = (n: number): [Json, Json] => [{ t: "x" }, { t: `x${"C".repeat(n)}` }];
	let lo = 0;
	let hi = 1 << 20;
	while (hi - lo > 1) {
		const mid = (lo + hi) >> 1;
		if ((diffRevisions(...threshold(mid)) as Op[])[0]![0] === "r") hi = mid;
		else lo = mid;
	}
	if (hi + 17 !== 65_536) throw new Error(`the threshold flips at n = ${hi}, not where the batch costs 65,536`);
	for (const n of [hi - 1, hi]) genCase(`threshold-${n}`, ...threshold(n));
	// The identity-anchor budget: [b0, a x N, b1] -> [c0, a x 500, c1], one
	// shared object a, is N x 500 candidates. 200,000 is still the patience
	// search; one more row of them is the greedy choice.
	for (const n of [400, 401]) {
		const a = { id: "a" };
		genCase(`anchor-budget-${n}`, ["b0", ...Array(n).fill(a), "b1"], ["c0", ...Array(500).fill(a), "c1"]);
	}
	// The semantic table's budget: rows that share a child k align by value
	// only through the LCS table, which 256 x 256 = 65,536 cells still gets;
	// one more row does not, and the region is spliced whole.
	for (const n of [256, 257]) {
		const k = {};
		genCase(
			`semantic-cells-${n}`,
			Array.from({ length: n }, (_, v) => ({ k, v })),
			Array.from({ length: 256 }, (_, v) => ({ k, v: v + 1_000 })),
		);
	}
}

// padOf is the unchanged member a cost case pads its snapshot with: L of one
// kind of value, so that the pad's cost per element decides where pi's batch
// flips from a replacement to a delta.
function padOf(kind: string, L: number): Json {
	const spellings = [1e21, 1.5e-7, 1e-6, -0, 123.456, -1e-7, 2 ** 53, 0.1, 1e-7, 5e-324];
	switch (kind) {
		case "num":
			return Array(L).fill(1e6);
		case "null":
			return Array(L).fill(null);
		case "true":
			return Array(L).fill(true);
		case "false":
			return Array(L).fill(false);
		case "spell":
			return Array.from({ length: L }, (_, i) => spellings[i % spellings.length]!);
		case "keys":
			return Object.fromEntries(Array.from({ length: L }, (_, i) => [`é${i}`, 0]));
	}
	throw new Error(`unknown pad ${kind}`);
}

// costCase builds golden_test.go's generatedDiff "cost-<kind>-<L>" inputs.
function costCase(kind: string, L: number): [Json, Json] {
	const r = (s: string, n: number) => s.repeat(n);
	const pad = kind === "astral" ? r("😀", L) : r("E", L);
	switch (kind) {
		case "s":
		case "astral":
			return [
				{ k1: r("A", 40_000), k2: r("B", 40_000), k3: pad },
				{ k1: r("C", 40_000), k2: r("D", 40_000), k3: pad },
			];
		case "a":
			return [
				{ t1: "x", t2: "y", k3: pad },
				{ t1: `x${r("C", 40_000)}`, t2: `y${r("D", 40_000)}`, k3: pad },
			];
		case "t":
			return [{ t: r("Q", 100) + r("Z", 10), k3: pad }, { t: r("Z", 10) + r("C", 80_000), k3: pad }];
		case "p":
			return [{ v: [], k3: pad }, { v: [r("C", 80_000)], k3: pad }];
		case "m": {
			// Single-digit values, so the permutation's indices outweigh them.
			const v = Array.from({ length: 20_000 }, (_, i) => i % 10);
			return [{ v, k3: pad }, { v: v.slice().reverse(), k3: pad }];
		}
		case "d":
			return [
				{ k1: r("A", 40_000), k2: r("B", 40_000), d0: 0, d1: 0, d2: 0, d3: 0, d4: 0, k3: pad },
				{ k1: r("C", 40_000), k2: r("D", 40_000), k3: pad },
			];
		case "index": {
			// 4,000 appends at ["v", i]: index segments in every path.
			const v = Array.from({ length: 8_000 }, (_, i): Json => (i % 2 === 0 ? 0 : "x"));
			return [{ k3: pad, v }, { k3: pad, v: v.map((x) => (x === "x" ? "xy" : x)) }];
		}
		case "num":
		case "null":
		case "true":
		case "false":
		case "spell":
		case "keys": {
			// 4,000 small sets cost far more than the members they change, so the
			// pad is large where pi flips and its cost per element is multiplied.
			const m0: Record<string, Json> = {};
			const m1: Record<string, Json> = {};
			for (let i = 0; i < 4_000; i++) {
				const key = `k${String(i).padStart(4, "0")}`;
				m0[key] = 0;
				m1[key] = 1;
			}
			const padding = padOf(kind, L);
			return [
				{ m: m0, pad: padding },
				{ m: m1, pad: padding },
			];
		}
	}
	throw new Error(`unknown cost kind ${kind}`);
}

// pinCosts finds, per kind, the first L at which pi's batch changes from a
// replacement to a delta. A longer pad only grows the snapshot, so the answer
// is monotonic in L and a binary search finds the flip.
function pinCosts(): Map<string, number> {
	const out = new Map<string, number>();
	for (const kind of ["s", "astral", "a", "t", "p", "m", "d", "index", "num", "null", "true", "false", "spell", "keys"]) {
		const verb = (L: number) => {
			const [before, after] = costCase(kind, L);
			return (diffRevisions(before, after) as Op[])[0]![0];
		};
		if (verb(0) !== "r") throw new Error(`cost kind ${kind} is a delta even unpadded`);
		let lo = 0;
		let hi = 1 << 20;
		if (verb(hi) === "r") throw new Error(`cost kind ${kind} never crosses the threshold`);
		while (hi - lo > 1) {
			const mid = (lo + hi) >> 1;
			if (verb(mid) === "r") lo = mid;
			else hi = mid;
		}
		out.set(kind, hi);
	}
	return out;
}

// Random arrays drawn from a pool of four shared objects and three numbers —
// every branch of the array diff (identity subsequence, identity anchors, the
// semantic table, positional and 1x1 matches) against pi's.
{
	let seed = 1;
	const rnd = () => {
		seed = (seed * 1103515245 + 12345) & 0x7fffffff;
		return seed / 0x7fffffff;
	};
	const int = (n: number) => Math.floor(rnd() * n);
	const pool = Object.fromEntries(Array.from({ length: 4 }, (_, i) => [`o${i}`, { id: i }]));
	const element = (): Json => {
		const pick = rnd();
		if (pick < 0.45) return R(`o${int(4)}`);
		if (pick < 0.85) return int(3);
		// A fresh object that shares one pool child: semantically aligned with any
		// other that shares it.
		return { c: R(`o${int(4)}`), n: int(2) };
	};
	for (let i = 0; i < 120; i++) {
		const gen = () => Array.from({ length: int(7) }, element);
		diffCase(`random arrays ${i}`, pool, { v: gen() }, { v: gen() });
	}
	// Inputs on which disabling the identity-subsequence pass, or feeding the
	// anchors' LIS its candidates in forward order, changes pi's own output
	// (found by brute force against patched copies of diff.ts).
	const arr = (spec: string): Json[] => spec.split(",").map((s) => (s.startsWith("o") ? R(s) : Number(s)));
	for (const [before, after] of [
		["o1", "2,o1,0,o1,o0"],
		["o0", "2,o3,o0,o2,o0,2"],
		["0", "o0,0,2,o0,0,2"],
		["1,1,o2", "o3,1,o3,0,o2"],
		["1,0,1,o0,o2,o3", "o0,1,2,o0,o2"],
		["o0,0,1,2,1,2", "1,2,o0,0"],
	] as const) {
		diffCase(`array pass order ${before} -> ${after}`, pool, { v: arr(before) }, { v: arr(after) });
	}
}

// ─── Scenarios ───────────────────────────────────────────────────────────────

type Step = Record<string, unknown>;

class Runner {
	readonly trackers = new Map<string, any>();
	readonly changes = new Map<string, any>();
	readonly prepared = new Map<string, any>();
	readonly env: Env = { held: new Map() };
	readonly hashValues: boolean;
	constructor(hashValues = false) {
		this.hashValues = hashValues;
	}

	nav(step: Step): any {
		const from = step.from as string | undefined;
		let d: any = from !== undefined ? this.env.held.get(from) : this.changes.get((step.c as string) ?? "main").state;
		for (const seg of (step.at as (string | number)[]) ?? []) d = d[seg];
		return d;
	}

	result(v: unknown): Record<string, unknown> {
		return v === undefined ? { missing: true } : { value: RAW(canon(v)) };
	}

	prepared_(p: any): Record<string, unknown> {
		const out: Record<string, unknown> = { ...trackerBatch(p.ops as Op[]), noop: p.value === p.base };
		const text = canon(p.value);
		if (this.hashValues) out.valueHash = hash(text);
		else out.value = RAW(text);
		return out;
	}

	exec(step: Step): Record<string, unknown> {
		const t = (step.t as string) ?? "main";
		const c = (step.c as string) ?? "main";
		const p = (step.p as string) ?? "main";
		const value = () => build(step.value, this.env);
		const items = () => ((step.items as unknown[]) ?? []).map((x) => build(x, this.env));
		switch (step.do) {
			case "begin":
				this.changes.set(c, this.trackers.get(t).beginChange());
				return {};
			case "prepare": {
				const prepared = this.changes.get(c).prepare();
				this.prepared.set(p, prepared);
				return this.prepared_(prepared);
			}
			case "replace": {
				const prepared = this.trackers.get(t).prepareReplace(value());
				this.prepared.set(p, prepared);
				return this.prepared_(prepared);
			}
			case "adopt":
				this.trackers.get(t).adopt(this.prepared.get(p));
				return {};
			case "abort":
				this.changes.get(c).abort();
				return {};
			case "abortPrepared":
				this.prepared.get(p).abort();
				return {};
			case "baseRevision":
				return { value: this.prepared.get(p).baseRevision };
			case "revision":
				return { value: this.trackers.get(t).revision };
			case "value":
				return this.hashValues
					? { valueHash: hash(canon(this.trackers.get(t).value)) }
					: { value: RAW(canon(this.trackers.get(t).value)) };
			case "hold":
				this.env.held.set(step.as as string, this.nav(step));
				return {};
			case "shared": {
				// Whether the prepared revision holds the base's own container at
				// "at": identity, which ops cannot show.
				const prepared = this.prepared.get(p);
				let value = prepared.value;
				let base = prepared.base;
				for (const seg of step.at as (string | number)[]) {
					value = value[seg];
					base = base[seg];
				}
				return { value: value === base };
			}
			case "keys":
				return { value: RAW(canon(Object.keys(this.nav(step)))) };
			case "get":
				return this.result(this.nav(step)[step.key as string]);
			case "set":
				this.nav(step)[step.key as string] = value();
				return {};
			case "append":
				this.nav(step)[step.key as string] += step.text as string;
				return {};
			case "delete":
				delete this.nav(step)[step.key as string];
				return {};
			case "push":
				return { value: this.nav(step).push(...items()) };
			case "unshift":
				return { value: this.nav(step).unshift(...items()) };
			case "pop":
			case "shift": {
				const out = this.nav(step)[step.do as string]();
				if (step.as !== undefined) this.env.held.set(step.as as string, out);
				return this.result(out);
			}
			case "splice":
				return this.result(this.nav(step).splice(step.start, step.deleteCount, ...items()));
			case "reverse":
				this.nav(step).reverse();
				return {};
			case "sort": {
				const by = step.by as string | null;
				if (by === null) {
					this.nav(step).sort();
					return {};
				}
				const direction = step.desc === true ? -1 : 1;
				const bump = step.bump as string | undefined;
				const write = step.write as [number, unknown] | undefined;
				const unshiftOnce = step.unshiftOnce as unknown[] | undefined;
				const field = (x: any) => (by === "." ? x : x[by]);
				const array = this.nav(step);
				let unshifted = false;
				array.sort((l: any, r: any) => {
					if (bump !== undefined) {
						l[bump]++;
						r[bump]++;
					}
					if (write !== undefined) array[write[0]] = build(write[1], this.env);
					if (unshiftOnce !== undefined && !unshifted) {
						unshifted = true;
						array.unshift(...unshiftOnce.map((x) => build(x, this.env)));
					}
					return (field(l) - field(r)) * direction;
				});
				return {};
			}
			case "fill":
				this.nav(step).fill(value(), step.start, step.end);
				return {};
			case "copyWithin":
				this.nav(step).copyWithin(step.target, step.start, step.end);
				return {};
			case "setLength":
				this.nav(step).length = step.length;
				return {};
		}
		throw new Error(`unknown step ${String(step.do)}`);
	}

	run(steps: Step[]): unknown[] {
		const results: unknown[] = [];
		for (const step of steps) {
			try {
				results.push(this.exec(step));
			} catch (error) {
				results.push({ error: (error as Error).message });
			}
		}
		return results;
	}
}

const scenarios: unknown[] = [];

function scenario(name: string, trackers: Record<string, Json>, steps: Step[]): void {
	const runner = new Runner();
	for (const [t, initial] of Object.entries(trackers)) runner.trackers.set(t, track(build(initial, runner.env)));
	scenarios.push({ name, trackers, steps: RAW(canon(steps)), results: runner.run(steps) });
}

const S = (text: string): Step[] => JSON.parse(text) as Step[];

// delta.test.ts "immutable tracker lifecycle".
scenario("lifecycle across a pause", { main: J(`{"count":1,"nested":{"text":"a"},"values":[1]}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":[],"key":"count","value":2},
	{"do":"append","at":["nested"],"key":"text","text":"b"},
	{"do":"push","at":["values"],"items":[2]},
	{"do":"value"},
	{"do":"prepare"},
	{"do":"get","at":[],"key":"count"},
	{"do":"value"},
	{"do":"adopt"},
	{"do":"value"}
]`));
scenario("abort revokes", { main: J(`{"child":{"value":1}}`) }, S(`[
	{"do":"begin"},
	{"do":"hold","at":["child"],"as":"child"},
	{"do":"set","from":"child","at":[],"key":"value","value":2},
	{"do":"abort"},
	{"do":"value"},
	{"do":"get","from":"child","at":[],"key":"value"},
	{"do":"set","from":"child","at":[],"key":"value","value":3},
	{"do":"abort"},
	{"do":"prepare"},
	{"do":"begin","c":"next"},
	{"do":"abort","c":"next"}
]`));
scenario("deep no-op keeps identity", { main: J(`{"value":{"nested":[1,2]}}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":[],"key":"value","value":{"nested":[1,2]}},
	{"do":"prepare"},
	{"do":"adopt"}
]`));
scenario("replacement no-op", { main: J(`{"nested":{"value":1}}`) }, S(`[
	{"do":"replace","value":{"nested":{"value":2}}},
	{"do":"adopt"},
	{"do":"replace","value":{"nested":{"value":2}}},
	{"do":"value"}
]`));
scenario("prepared rows", { main: J(`{"rows":[]}`) }, S(`[
	{"do":"begin"},
	{"do":"push","at":["rows"],"items":[{"id":1}]},
	{"do":"prepare"}
]`));
scenario("foreign, stale, repeated and overlapping", { main: J(`{"value":0}`), other: J(`{"value":0}`) }, S(`[
	{"do":"begin"},
	{"do":"begin","c":"second"},
	{"do":"set","at":[],"key":"value","value":1},
	{"do":"prepare"},
	{"do":"adopt","t":"other"},
	{"do":"adopt"},
	{"do":"adopt"},
	{"do":"replace","value":{"value":2},"p":"stale"},
	{"do":"replace","value":{"value":3},"p":"winner"},
	{"do":"adopt","p":"winner"},
	{"do":"adopt","p":"stale"},
	{"do":"adopt","p":"stale"},
	{"do":"value"}
]`));
scenario("abort after prepare", { main: J(`{"value":0}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":[],"key":"value","value":1},
	{"do":"prepare"},
	{"do":"abort"},
	{"do":"adopt"},
	{"do":"abort"},
	{"do":"value"}
]`));
scenario("competing no-ops", { main: J(`{"value":{"count":1}}`) }, S(`[
	{"do":"replace","value":{"value":{"count":1}},"p":"first"},
	{"do":"replace","value":{"value":{"count":1}},"p":"competing"},
	{"do":"adopt","p":"first"},
	{"do":"adopt","p":"first"},
	{"do":"adopt","p":"competing"}
]`));
scenario("adopt during a change", { main: J(`{"value":0}`) }, S(`[
	{"do":"replace","value":{"value":1},"p":"waiting"},
	{"do":"begin"},
	{"do":"adopt","p":"waiting"},
	{"do":"replace","value":{"value":2}},
	{"do":"abort"},
	{"do":"adopt","p":"waiting"},
	{"do":"value"}
]`));
// A used or aborted prepared change is refused as such while another change
// is open.
scenario("adopt refuses used and aborted changes while another is open", { main: J(`{"value":0}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":[],"key":"value","value":1},
	{"do":"prepare","p":"used"},
	{"do":"adopt","p":"used"},
	{"do":"begin","c":"second"},
	{"do":"set","c":"second","at":[],"key":"value","value":2},
	{"do":"prepare","c":"second","p":"aborted"},
	{"do":"abort","c":"second"},
	{"do":"begin","c":"open"},
	{"do":"adopt","p":"used"},
	{"do":"adopt","p":"aborted"},
	{"do":"abort","c":"open"},
	{"do":"value"}
]`));
// change.abort() after the prepared change was adopted leaves it used, not
// aborted (tracker.ts's `status === "prepared"` guard).
scenario("abort after adopt", { main: J(`{"value":0}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":[],"key":"value","value":1},
	{"do":"prepare"},
	{"do":"adopt"},
	{"do":"abort"},
	{"do":"adopt"},
	{"do":"value"}
]`));
// delta.test.ts "canonical strings".
scenario("canonical strings", { main: J(`{"text":"abcdefgh"}`) }, S(`[
	{"do":"begin"},
	{"do":"append","at":[],"key":"text","text":"ij"},
	{"do":"prepare"},
	{"do":"adopt"},
	{"do":"begin"},
	{"do":"set","at":[],"key":"text","value":"defghijxyz"},
	{"do":"prepare"}
]`));
// codec_test.go's stream, through the tracker.
scenario("codec stream", { main: J(`{"a":{"deep":""},"b":{"deep":""}}`) }, S(
	JSON.stringify(
		Array.from({ length: 6 }, (_, i) => [
			{ do: "begin" },
			{ do: "append", at: ["a"], key: "deep", text: `x${i}` },
			{ do: "append", at: ["b"], key: "deep", text: `y${i}` },
			{ do: "prepare" },
			{ do: "adopt" },
		]).flat(),
	),
));
// state-draft.test.ts.
scenario("copies only changed branches", { main: J(`{"changed":{"count":1,"sibling":{"value":"kept"}},"untouched":{"value":2}}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":["changed"],"key":"count","value":3},
	{"do":"prepare"}
]`));
scenario("native array mutators", { main: J(`{"optional":"remove","values":[3,1,2]}`) }, S(`[
	{"do":"begin"},
	{"do":"delete","at":[],"key":"optional"},
	{"do":"push","at":["values"],"items":[4]},
	{"do":"pop","at":["values"]},
	{"do":"unshift","at":["values"],"items":[0]},
	{"do":"shift","at":["values"]},
	{"do":"splice","at":["values"],"start":1,"deleteCount":1,"items":[5,4]},
	{"do":"sort","at":["values"],"by":"."},
	{"do":"reverse","at":["values"]},
	{"do":"fill","at":["values"],"value":9,"start":1,"end":3},
	{"do":"copyWithin","at":["values"],"target":1,"start":0,"end":2},
	{"do":"prepare"}
]`));
scenario("inserted draft does not alias its handle", { main: J(`{"values":[{"value":1},{"value":2}]}`) }, S(`[
	{"do":"begin"},
	{"do":"hold","at":["values",0],"as":"held"},
	{"do":"unshift","at":["values"],"items":[{"$held":"held"}]},
	{"do":"set","from":"held","at":[],"key":"value","value":9},
	{"do":"prepare"}
]`));
scenario("comparator edits survive", { main: J(`{"rows":[{"rank":2,"comparisons":0},{"rank":1,"comparisons":0}]}`) }, S(`[
	{"do":"begin"},
	{"do":"sort","at":["rows"],"by":"rank","bump":"comparisons"},
	{"do":"prepare"}
]`));
scenario("detached handles", { main: J(`{"child":{"value":"removed"},"items":[{"value":"removed"},{"value":"kept"}]}`) }, S(`[
	{"do":"begin"},
	{"do":"hold","at":["child"],"as":"child"},
	{"do":"shift","at":["items"],"as":"shifted"},
	{"do":"delete","at":[],"key":"child"},
	{"do":"set","from":"child","at":[],"key":"value","value":"detached child"},
	{"do":"set","from":"shifted","at":[],"key":"value","value":"detached item"},
	{"do":"prepare"}
]`));
scenario("invalid writes leave the base alone", { main: J(`{"values":[1,2]}`) }, S(`[
	{"do":"begin"},
	{"do":"delete","at":["values"],"key":0},
	{"do":"abort"},
	{"do":"begin"},
	{"do":"set","at":["values"],"key":0,"value":{"$nan":true}},
	{"do":"set","at":["values"],"key":0,"value":{"$inf":true}},
	{"do":"set","at":["values"],"key":0,"value":{"$date":true}},
	{"do":"set","at":["values"],"key":0,"value":{"$cycle":true}},
	{"do":"setLength","at":["values"],"length":-1},
	{"do":"abort"},
	{"do":"value"}
]`));
// state.test.ts, the tracker half of each replicatedState case.
scenario("structurally shared revision", { main: J(`{"changed":{"value":1},"retained":{"value":2}}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":["changed"],"key":"value","value":3},
	{"do":"set","at":["changed"],"key":"value","value":4},
	{"do":"prepare"}
]`));
scenario("copies assigned values by value", { main: J(`{"left":null,"right":null}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":[],"key":"left","value":{"value":1}},
	{"do":"set","at":[],"key":"right","value":{"value":1}},
	{"do":"set","at":["left"],"key":"value","value":2},
	{"do":"prepare"}
]`));
scenario("reused subtrees become independent placements", { main: J(`{"left":{"value":1},"right":{"value":2}}`) }, S(`[
	{"do":"replace","value":{"left":{"value":1},"right":{"value":1}}},
	{"do":"adopt"},
	{"do":"begin"},
	{"do":"set","at":["left"],"key":"value","value":9},
	{"do":"prepare"}
]`));
scenario("compact string, splice and permutation", { main: J(`{"text":"abcdefgh","values":[{"id":"a"},{"id":"b"},{"id":"c"}]}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":[],"key":"text","value":"defghxyz"},
	{"do":"shift","at":["values"]},
	{"do":"prepare"},
	{"do":"adopt"},
	{"do":"begin"},
	{"do":"reverse","at":["values"]},
	{"do":"prepare"}
]`));
scenario("deeply equal replacement", { main: J(`{"value":{"nested":1},"retained":{"nested":2}}`) }, S(`[
	{"do":"replace","value":{"value":{"nested":1},"retained":{"nested":2}}},
	{"do":"adopt"},
	{"do":"replace","value":{"value":{"nested":2},"retained":{"nested":2}}},
	{"do":"adopt"},
	{"do":"value"}
]`));
// A scalar root. track() takes any root as it is, but a change drafts the
// revision and a replacement roots an overlay on its value, and both key the
// overlay's WeakMap by that container: a scalar throws.
scenario("scalar roots", { main: J(`1`), none: J(`null`), obj: J(`{"a":1}`) }, S(`[
	{"do":"value"},
	{"do":"begin"},
	{"do":"replace","value":1},
	{"do":"replace","value":2},
	{"do":"adopt"},
	{"do":"value"},
	{"do":"replace","value":{"b":2}},
	{"do":"adopt"},
	{"do":"value"},
	{"do":"begin","t":"none"},
	{"do":"value","t":"none"},
	{"do":"replace","t":"none","p":"none","value":"s"},
	{"do":"replace","t":"obj","p":"obj","value":null},
	{"do":"replace","t":"obj","p":"obj","value":false},
	{"do":"replace","t":"obj","p":"obj","value":-0},
	{"do":"adopt","t":"obj","p":"obj"},
	{"do":"replace","t":"obj","p":"obj","value":0},
	{"do":"replace","t":"obj","p":"obj","value":[2]},
	{"do":"adopt","t":"obj","p":"obj"},
	{"do":"begin","t":"obj","c":"obj"},
	{"do":"push","t":"obj","c":"obj","at":[],"items":[3]},
	{"do":"prepare","t":"obj","c":"obj","p":"obj"}
]`));
// Draft array operations, one at a time and in combination.
const arrays = (steps: string) => S(`[{"do":"begin"},${steps},{"do":"prepare"}]`);
scenario("push and pop cancel", { main: J(`{"v":[1,2]}`) }, arrays(`{"do":"push","at":["v"],"items":[3]},{"do":"pop","at":["v"]}`));
scenario("pop and shift", { main: J(`{"v":[1,2,3,4]}`) }, arrays(`{"do":"pop","at":["v"]},{"do":"shift","at":["v"]}`));
scenario("pop and shift an empty array", { main: J(`{"v":[]}`) }, arrays(`{"do":"pop","at":["v"]},{"do":"shift","at":["v"]}`));
scenario("unshift many", { main: J(`{"v":[1]}`) }, arrays(`{"do":"unshift","at":["v"],"items":[7,8,9]}`));
scenario("splice negative start", { main: J(`{"v":[1,2,3,4,5]}`) }, arrays(`{"do":"splice","at":["v"],"start":-2,"deleteCount":1,"items":["x","y"]}`));
scenario("splice clamps", { main: J(`{"v":[1,2,3]}`) }, arrays(`{"do":"splice","at":["v"],"start":10,"deleteCount":5,"items":[4]},{"do":"splice","at":["v"],"start":-10,"deleteCount":-3,"items":[0]}`));
scenario("splice everything", { main: J(`{"v":[1,2,3]}`) }, arrays(`{"do":"splice","at":["v"],"start":0,"deleteCount":3,"items":[]}`));
scenario("splice at the root", { main: J(`[1,2,3]`) }, arrays(`{"do":"splice","at":[],"start":1,"deleteCount":1,"items":[9]}`));
// tracker.test.ts (091bcc099) "emits removals from the end backwards, then
// inserts, when kept elements keep their order".
scenario("kept elements keep their order", { main: J(`{"values":[0,1,2,3,4,5,6,7,8,9]}`) }, arrays(`{"do":"shift","at":["values"]},{"do":"splice","at":["values"],"start":1,"deleteCount":2,"items":[20]},{"do":"splice","at":["values"],"start":5,"deleteCount":1,"items":[]},{"do":"pop","at":["values"]},{"do":"push","at":["values"],"items":[30]}`));
scenario("root array emptied", { main: J(`[1,2,3]`) }, arrays(`{"do":"setLength","at":[],"length":0}`));
scenario("reverse scalars", { main: J(`{"v":[1,2,3]}`) }, arrays(`{"do":"reverse","at":["v"]}`));
scenario("reverse a palindrome", { main: J(`{"v":[1,2,1]}`) }, arrays(`{"do":"reverse","at":["v"]}`));
scenario("reverse at the root", { main: J(`[{"a":1},{"b":2}]`) }, arrays(`{"do":"reverse","at":[]}`));
scenario("sort numbers by default order", { main: J(`{"v":[10,9,1,100,-1,0.5]}`) }, arrays(`{"do":"sort","at":["v"],"by":null}`));
scenario("sort mixed values by default order", { main: J(`{"v":["b",10,null,true,"a",[1,2],{"x":1},[1],false,"10"]}`) }, arrays(`{"do":"sort","at":["v"],"by":null}`));
scenario("sort astral strings by default order", { main: J(`{"v":["｡","😀","a","é"]}`) }, arrays(`{"do":"sort","at":["v"],"by":null}`));
scenario("sort objects descending", { main: J(`{"v":[{"id":2},{"id":3},{"id":1},{"id":3,"second":true}]}`) }, arrays(`{"do":"sort","at":["v"],"by":"id","desc":true}`));
scenario("sort already sorted", { main: J(`{"v":[{"id":1},{"id":2}]}`) }, arrays(`{"do":"sort","at":["v"],"by":"id"}`));
scenario("length shrink", { main: J(`{"v":[1,2,3,4]}`) }, arrays(`{"do":"setLength","at":["v"],"length":2}`));
scenario("length grow then fill", { main: J(`{"v":[1]}`) }, arrays(`{"do":"setLength","at":["v"],"length":4},{"do":"fill","at":["v"],"value":0,"start":1,"end":4}`));
scenario("length grow then fill objects", { main: J(`{"v":[]}`) }, arrays(`{"do":"setLength","at":["v"],"length":2},{"do":"fill","at":["v"],"value":{"n":1},"start":0,"end":2},{"do":"set","at":["v",0],"key":"n","value":2}`));
scenario("length grow unfilled", { main: J(`{"v":[1]}`) }, arrays(`{"do":"setLength","at":["v"],"length":3},{"do":"set","at":["v"],"key":1,"value":2}`));
scenario("length write through set", { main: J(`{"v":[1,2,3]}`) }, arrays(`{"do":"set","at":["v"],"key":"length","value":1},{"do":"get","at":["v"],"key":"length"}`));
scenario("index write past the end is refused", { main: J(`{"v":[1]}`) }, arrays(`{"do":"set","at":["v"],"key":3,"value":4},{"do":"get","at":["v"],"key":2},{"do":"set","at":["v"],"key":1,"value":2},{"do":"set","at":["v"],"key":2,"value":3}`));
scenario("named property on an array", { main: J(`{"v":[1],"w":[]}`) }, arrays(`{"do":"set","at":["v"],"key":"name","value":{"n":1}},{"do":"get","at":["v","name"],"key":"n"},{"do":"set","at":["v"],"key":"name","value":2},{"do":"delete","at":["v"],"key":"name"},{"do":"hold","at":["v"],"as":"v"},{"do":"set","at":[],"key":"copy","value":{"$held":"v"}},{"do":"set","at":["w"],"key":4294967295,"value":1},{"do":"set","at":["w"],"key":-1,"value":1},{"do":"get","at":["w"],"key":-1}`));
scenario("string-spelled index", { main: J(`{"v":[1,2]}`) }, arrays(`{"do":"set","at":["v"],"key":"1","value":9},{"do":"get","at":["v"],"key":"0"}`));
scenario("fill negative bounds", { main: J(`{"v":[1,2,3,4,5]}`) }, arrays(`{"do":"fill","at":["v"],"value":"z","start":-3,"end":-1}`));
scenario("fill empty range skips the value check", { main: J(`{"v":[1,2]}`) }, arrays(`{"do":"fill","at":["v"],"value":{"$nan":true},"start":1,"end":1}`));
scenario("copyWithin overlapping", { main: J(`{"v":[1,2,3,4,5]}`) }, arrays(`{"do":"copyWithin","at":["v"],"target":1,"start":0,"end":4}`));
scenario("copyWithin copies live drafts", { main: J(`{"v":[{"n":1},{"n":2},{"n":3}]}`) }, arrays(`{"do":"set","at":["v",0],"key":"n","value":7},{"do":"copyWithin","at":["v"],"target":2,"start":0,"end":1},{"do":"set","at":["v",0],"key":"n","value":8}`));
scenario("copyWithin over grown nulls", { main: J(`{"v":[1]}`) }, arrays(`{"do":"setLength","at":["v"],"length":3},{"do":"copyWithin","at":["v"],"target":0,"start":1,"end":3}`));
scenario("nested permute", { main: J(`{"o":{"rows":[{"id":1},{"id":2},{"id":3}]}}`) }, arrays(`{"do":"sort","at":["o","rows"],"by":"id","desc":true}`));
scenario("held handle follows reorder", { main: J(`{"v":[{"n":1},{"n":2},{"n":3}]}`) }, arrays(`{"do":"hold","at":["v",0],"as":"first"},{"do":"reverse","at":["v"]},{"do":"unshift","at":["v"],"items":[{"n":0}]},{"do":"set","from":"first","at":[],"key":"n","value":10}`));
scenario("splice returns detached drafts", { main: J(`{"v":[{"n":1},{"n":2}]}`) }, arrays(`{"do":"splice","at":["v"],"start":0,"deleteCount":1,"items":[]},{"do":"get","at":["v",0],"key":"n"}`));
scenario("assign a draft copies its current state", { main: J(`{"a":{"n":1,"deep":{"m":1}},"b":null}`) }, arrays(`{"do":"set","at":["a","deep"],"key":"m","value":2},{"do":"set","at":["a"],"key":"n","value":3},{"do":"hold","at":["a"],"as":"a"},{"do":"set","at":[],"key":"b","value":{"$held":"a"}},{"do":"set","from":"a","at":[],"key":"n","value":5}`));
scenario("sort nested arrays by default order", { main: J(`{"v":[[2],[1],[1,0],[10],[],[null,1],[[3]]]}`) }, arrays(`{"do":"sort","at":["v"],"by":null}`));
scenario("shift and pop after growing with nulls", { main: J(`{"v":[1]}`) }, arrays(`{"do":"setLength","at":["v"],"length":3},{"do":"reverse","at":["v"]},{"do":"shift","at":["v"]},{"do":"get","at":["v"],"key":0},{"do":"pop","at":["v"]},{"do":"set","at":["v"],"key":0,"value":5}`));
scenario("default sort after growing with nulls", { main: J(`{"v":["~","b"]}`) }, arrays(`{"do":"setLength","at":["v"],"length":4},{"do":"reverse","at":["v"]},{"do":"sort","at":["v"],"by":null},{"do":"get","at":["v"],"key":1},{"do":"get","at":["v"],"key":2},{"do":"fill","at":["v"],"value":0,"start":2,"end":4}`));
scenario("negative zero write with another change", { main: J(`{"a":0,"b":1}`) }, arrays(`{"do":"set","at":[],"key":"a","value":-0},{"do":"set","at":[],"key":"b","value":2}`));
scenario("delete then re-add a member", { main: J(`{"b":1,"a":1}`) }, arrays(`{"do":"delete","at":[],"key":"b"},{"do":"set","at":[],"key":"b","value":2},{"do":"set","at":[],"key":"a","value":2}`));
scenario("reserved key written through a draft", { main: J(`{"x":{"v":1}}`) }, arrays(`{"do":"set","at":["x"],"key":"__proto__","value":{"polluted":true}}`));
scenario("reserved key at the root", { main: J(`{"v":1}`) }, arrays(`{"do":"set","at":[],"key":"constructor","value":1}`));
scenario("integer-like keys", { main: J(`{"a":1}`) }, arrays(`{"do":"set","at":[],"key":"10","value":1},{"do":"set","at":[],"key":2,"value":1},{"do":"set","at":[],"key":"b","value":1}`));
scenario("no-op scalar writes", { main: J(`{"a":1,"s":"x","n":null,"v":[true]}`) }, arrays(`{"do":"set","at":[],"key":"a","value":1},{"do":"set","at":[],"key":"s","value":"x"},{"do":"set","at":[],"key":"n","value":null},{"do":"set","at":["v"],"key":0,"value":true}`));
scenario("negative zero write", { main: J(`{"a":0}`) }, arrays(`{"do":"set","at":[],"key":"a","value":-0}`));
scenario("write then restore", { main: J(`{"a":1,"o":{"x":1}}`) }, arrays(`{"do":"set","at":[],"key":"a","value":2},{"do":"set","at":[],"key":"a","value":1},{"do":"set","at":["o"],"key":"x","value":5},{"do":"set","at":["o"],"key":"x","value":1}`));
scenario("rolling window with astral characters", { main: J(`{"t":"😀😁😂"}`) }, arrays(`{"do":"set","at":[],"key":"t","value":"😁😂🤣"}`));
// The default sort order is each element's String(): ToPrimitive, then
// ToString. A JSON object's own "toString" member is never callable, and
// Object.prototype.valueOf returns the object, so an object holding one has no
// primitive at all - a TypeError, at the element or joined inside an array. A
// thrown sort leaves the array as it was (V8 sorts a copy), and one element is
// never compared.
const sortBy = (initial: string, ...pre: string[]) =>
	scenario(`default sort of ${initial}${pre.length > 0 ? " after writes" : ""}`, { main: J(initial) }, arrays([...pre, `{"do":"sort","at":["xs"],"by":null}`, `{"do":"get","at":[],"key":"xs"}`].join(",")));
sortBy(`{"xs":[{"a":2},{"toString":"b"},{"a":1},3,1]}`);
sortBy(`{"xs":[[{"toString":1}],[0]]}`);
sortBy(`{"xs":[[[{"toString":1}]],[0]]}`);
sortBy(`{"xs":[{"toString":null},{"a":1}]}`);
sortBy(`{"xs":[{"valueOf":"v"},{"a":1}]}`);
sortBy(`{"xs":[{"toString":1}]}`);
sortBy(`{"xs":[[null,[1,[2]]],[true,"x"],{"toString2":1},[{}]]}`);
// A named "toString", "join" or "valueOf" on an array draft is refused (only
// indices and length can be written); the sort then runs as usual.
sortBy(`{"xs":[[2],[1]]}`, `{"do":"set","at":["xs",0],"key":"toString","value":1}`);
sortBy(`{"xs":[[1],[2],["[object Array]0"]]}`, `{"do":"set","at":["xs",0],"key":"join","value":1}`);
sortBy(`{"xs":[[2],[1]]}`, `{"do":"set","at":["xs",0],"key":"valueOf","value":1}`);
// JavaScript's number spellings, which order the default sort: the shortest
// round-trip digits, positional from 1e-6 up to 1e21, exponent outside it
// with no leading zero in the exponent, and -0 as "0".
sortBy(`{"xs":[1e-7,1e-10]}`);
sortBy(`{"xs":[1e21,"1a"]}`);
sortBy(`{"xs":[0.000001,0.00001]}`);
sortBy(`{"xs":[999999999999999900000,1e21,"1e+20"]}`);
sortBy(`{"xs":[-1e-7,-1,"-1e-6",0.1,1.5e-7]}`);
sortBy(`{"xs":[1,0]}`, `{"do":"set","at":["xs"],"key":1,"value":-0}`);
sortBy(`{"xs":[["a"],[["b"]]]}`);
sortBy(`{"xs":[["l"],[{}]]}`);
// The sort reads each element as the draft holds it now, through nested
// drafts too: a written element orders by its new value (and a named property
// on a nested array is refused, as above).
sortBy(`{"xs":[[2],[1]]}`, `{"do":"set","at":["xs",0],"key":0,"value":0}`);
sortBy(`{"xs":[[[2]],[[1]]]}`, `{"do":"set","at":["xs",0,0],"key":0,"value":0}`);
sortBy(`{"xs":[[[2]],[[1]]]}`, `{"do":"set","at":["xs",0,0],"key":"toString","value":1}`);
sortBy(`{"xs":[[[2]],[[1]]]}`, `{"do":"set","at":["xs",0,0],"key":"join","value":1}`);
// Ties keep their order: the sort is stable. Past 12 elements an unstable
// sort (Go's pdqsort) would move distinguishable ties such as 1 and "1" here.
sortBy(`{"xs":[3,"3",1,"1",[1],[[1]],["1"],[["1"]],2,"2",[3],[2],1]}`);
// A branch that was only read keeps its identity in the prepared revision,
// even when the change has other effects.
scenario("a read-only branch keeps its identity", { main: J(`{"n":0,"o":{"p":{"q":1},"r":[1]}}`) }, S(`[
	{"do":"begin"},
	{"do":"get","at":["o","p"],"key":"q"},
	{"do":"get","at":["o","r"],"key":0},
	{"do":"set","at":[],"key":"n","value":1},
	{"do":"prepare"},
	{"do":"shared","at":["o"]},
	{"do":"shared","at":["o","p"]},
	{"do":"shared","at":[]}
]`));
// Only a canonical index below 2^32 - 1 is an array index: "01" and
// "4294967295" are named properties, which an array draft refuses.
scenario("index-like keys on an array", { main: J(`{"xs":[1,2,3,4]}`) }, S(`[
	{"do":"begin"},{"do":"set","at":["xs"],"key":"01","value":9},{"do":"get","at":["xs"],"key":1},{"do":"prepare"},
	{"do":"begin"},{"do":"set","at":["xs"],"key":"4294967295","value":9},{"do":"get","at":["xs"],"key":"length"},{"do":"prepare"}
]`));
scenario("copyWithin negative target", { main: J(`{"xs":[1,2,3,4]}`) }, arrays(`{"do":"copyWithin","at":["xs"],"target":-1,"start":0,"end":4}`));
scenario("pop after growing with nulls", { main: J(`{"v":[1]}`) }, arrays(`{"do":"setLength","at":["v"],"length":3},{"do":"pop","at":["v"]},{"do":"pop","at":["v"]},{"do":"pop","at":["v"]},{"do":"pop","at":["v"]}`));
// Object.keys: an array's indices; an object's keys (inserted in Go's order
// here - D69).
scenario("keys", { main: J(`{"o":{"a":1,"b":{},"2":0,"10":0},"xs":[1,2]}`) }, S(`[
	{"do":"begin"},
	{"do":"keys","at":[]},
	{"do":"keys","at":["o"]},
	{"do":"keys","at":["o","b"]},
	{"do":"keys","at":["xs"]},
	{"do":"set","at":["xs"],"key":"name","value":"n"},
	{"do":"setLength","at":["xs"],"length":4},
	{"do":"set","at":["xs"],"key":3,"value":4},
	{"do":"keys","at":["xs"]},
	{"do":"abort"}
]`));
sortBy(`{"xs":[[1,[2,[3]]],[1,2,3],"1,2,3 ",[null],[[]],""]}`);
// `array.length = v` is ArraySetLength: v goes through ToNumber (null 0,
// booleans 0 and 1, strings as numeric literals after trimming JavaScript
// whitespace, arrays and objects through their string form), and the result
// must be an integer from 0 to 2^32 - 1.
{
	const lengths: unknown[] = [
		null, true, false, "2", " 2 ", "", "  ", "\t\n2\r", "0x2", "0B11", "0o7", "1e0", "2.", "00", "-0",
		" 2 ", "﻿1", "　2", "2\u0085", "x", "Infinity", "-0x2", "+0x2", "0x", "1_0", ".5",
		"0X2", "0O7", "0b2", "1e", "1e+", "e5", ".", "+", "2e0.", "0x1p1", "1e1000", "4294967296", "1 2",
		[2], [], [[3]], [" 1 "], ["0x1"], [1, 2], [0.5], [true], {},
		{ toString: 1 }, { valueOf: 1 }, [{ toString: 1 }], 2.5, 4294967296, -1, -0, 3, "3",
		// Every whitespace class the trim knows, and hex digits of both cases.
		"\v2", "\f2", "2\u2029", "0xa", "0xA", "0xf", "0XB",
	];
	const steps: Step[] = [];
	for (const value of lengths) {
		steps.push({ do: "begin" }, { do: "set", at: ["list"], key: "length", value }, { do: "prepare" });
	}
	scenario("length assignments convert as JavaScript does", { main: J(`{"list":[1,2,3]}`) }, steps);
}
scenario("revoked draft", { main: J(`{"o":{"n":1},"v":[1]}`) }, S(`[
	{"do":"begin"},
	{"do":"hold","at":["o"],"as":"o"},
	{"do":"hold","at":["v"],"as":"v"},
	{"do":"prepare"},
	{"do":"get","from":"o","at":[],"key":"n"},
	{"do":"set","from":"o","at":[],"key":"n","value":2},
	{"do":"delete","from":"o","at":[],"key":"n"},
	{"do":"push","from":"v","at":[],"items":[2]},
	{"do":"pop","from":"v","at":[]},
	{"do":"sort","from":"v","at":[],"by":null},
	{"do":"setLength","from":"v","at":[],"length":0}
]`));
// An array index runs to 2^32 - 2, past a 32-bit int: "2147483648" and
// "4294967294" enumerate first, by value, and "4294967295" with the other
// named keys (inserted in Go's order - D69).
scenario("keys past 2^31", { main: J(`{"o":{}}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":["o"],"key":"!","value":1},
	{"do":"set","at":["o"],"key":"4294967295","value":2},
	{"do":"set","at":["o"],"key":"4294967294","value":3},
	{"do":"set","at":["o"],"key":"2147483648","value":4},
	{"do":"set","at":["o"],"key":"7","value":5},
	{"do":"keys","at":["o"]},
	{"do":"prepare"},
	{"do":"adopt"},
	{"do":"value"}
]`));

// Changes compete: any number may be open on one revision. Adopting one stales
// every other open change, whose drafts are then settled and which cannot be
// prepared; a prepared competitor is refused as stale — or as aborted, once
// aborted. Revisions count adoptions, no-ops included.
scenario("competing changes", { main: J(`{"child":{"n":1},"value":0}`) }, S(`[
	{"do":"begin","c":"a"},
	{"do":"begin","c":"b"},
	{"do":"hold","c":"b","at":["child"],"as":"bchild"},
	{"do":"set","c":"a","at":[],"key":"value","value":1},
	{"do":"set","c":"b","at":[],"key":"value","value":2},
	{"do":"prepare","c":"a","p":"a"},
	{"do":"baseRevision","p":"a"},
	{"do":"adopt","p":"a"},
	{"do":"revision"},
	{"do":"get","c":"b","at":[],"key":"value"},
	{"do":"get","from":"bchild","at":[],"key":"n"},
	{"do":"set","from":"bchild","at":[],"key":"n","value":5},
	{"do":"prepare","c":"b","p":"b"},
	{"do":"prepare","c":"b","p":"b"},
	{"do":"abort","c":"b"},
	{"do":"prepare","c":"b","p":"b"},
	{"do":"begin","c":"c"},
	{"do":"set","c":"c","at":["child"],"key":"n","value":3},
	{"do":"prepare","c":"c","p":"c"},
	{"do":"baseRevision","p":"c"},
	{"do":"adopt","p":"c"},
	{"do":"revision"},
	{"do":"value"}
]`));
scenario("prepared competitors", { main: J(`{"value":0}`) }, S(`[
	{"do":"replace","value":{"value":1},"p":"first"},
	{"do":"replace","value":{"value":2},"p":"second"},
	{"do":"begin","c":"third"},
	{"do":"set","c":"third","at":[],"key":"value","value":3},
	{"do":"prepare","c":"third","p":"third"},
	{"do":"replace","value":{"value":4},"p":"fourth"},
	{"do":"adopt","p":"first"},
	{"do":"adopt","p":"second"},
	{"do":"adopt","p":"second"},
	{"do":"abortPrepared","p":"third"},
	{"do":"adopt","p":"third"},
	{"do":"abort","c":"third"},
	{"do":"abortPrepared","p":"fourth"},
	{"do":"adopt","p":"fourth"},
	{"do":"abortPrepared","p":"first"},
	{"do":"adopt","p":"first"},
	{"do":"revision"},
	{"do":"value"}
]`));
scenario("revisions count no-ops", { main: J(`{"v":1}`) }, S(`[
	{"do":"revision"},
	{"do":"begin"},{"do":"prepare"},{"do":"baseRevision"},{"do":"adopt"},
	{"do":"revision"},
	{"do":"replace","value":{"v":1}},{"do":"baseRevision"},{"do":"adopt"},
	{"do":"revision"},
	{"do":"begin"},{"do":"abort"},
	{"do":"revision"},
	{"do":"value"}
]`));
// Arrays stay dense: an index past the length and an element deletion are
// refused; growing the length inserts nulls, and shrinking it removes.
scenario("arrays stay dense", { main: J(`{"v":[1,2]}`) }, arrays(`{"do":"set","at":["v"],"key":3,"value":4},{"do":"delete","at":["v"],"key":0},{"do":"set","at":["v"],"key":2,"value":3},{"do":"setLength","at":["v"],"length":5},{"do":"get","at":["v"],"key":4},{"do":"setLength","at":["v"],"length":4},{"do":"set","at":["v"],"key":4,"value":5}`));
// A base element written back to its own scalar drops its override, a
// repeated identical write keeps it, and null is a value like any other.
scenario("overrides: null, restored, repeated", { main: J(`{"v":[1,2,3,4]}`) }, arrays(`{"do":"set","at":["v"],"key":0,"value":null},{"do":"set","at":["v"],"key":1,"value":9},{"do":"set","at":["v"],"key":1,"value":2},{"do":"set","at":["v"],"key":2,"value":7},{"do":"set","at":["v"],"key":2,"value":7},{"do":"get","at":["v"],"key":0}`));
scenario("cancelled structural edits publish nothing", { main: J(`{"v":[1,2,3]}`) }, arrays(`{"do":"set","at":["v"],"key":1,"value":9},{"do":"set","at":["v"],"key":1,"value":2},{"do":"reverse","at":["v"]},{"do":"reverse","at":["v"]},{"do":"push","at":["v"],"items":[4]},{"do":"pop","at":["v"]}`));
// A placement is checked whole before the draft changes: of push(valid,
// invalid), nothing is inserted.
scenario("a refused placement changes nothing", { main: J(`{"o":{},"v":[1]}`) }, arrays(`{"do":"push","at":["v"],"items":[2,{"$nan":true}]},{"do":"unshift","at":["v"],"items":[{"$cycle":true}]},{"do":"splice","at":["v"],"start":0,"deleteCount":1,"items":[{"$date":true}]},{"do":"fill","at":["v"],"value":{"$inf":true},"start":0,"end":1},{"do":"set","at":["o"],"key":"x","value":{"a":[1,{"$nan":true}]}},{"do":"get","at":["v"],"key":"length"},{"do":"keys","at":["o"]}`));
// No op addresses a reserved key: an edit below one folds into a set of the
// nearest safe ancestor, and writing or deleting one sets its object whole.
scenario("a reserved key in the path folds into its safe ancestor", { main: J(`{"w":1,"x":{"__proto__":{"y":1},"z":1}}`) }, arrays(`{"do":"set","at":["x","__proto__"],"key":"y","value":2},{"do":"set","at":[],"key":"w","value":2}`));
scenario("deleting a reserved key sets its object", { main: J(`{"x":{"constructor":1,"k":1}}`) }, arrays(`{"do":"delete","at":["x"],"key":"constructor"}`));
// Edits inside a container this change placed publish with the placement.
scenario("edits inside a placed container publish with it", { main: J(`{"a":null,"list":[]}`) }, arrays(`{"do":"set","at":[],"key":"a","value":{"b":1,"c":[]}},{"do":"set","at":["a"],"key":"b","value":2},{"do":"push","at":["a","c"],"items":[1]},{"do":"push","at":["list"],"items":[{"t":[]}]},{"do":"push","at":["list",0,"t"],"items":["x"]}`));
// A structural edit publishes before the edits inside its entries, which
// address the entries' final indices.
scenario("a permutation, then edits at final paths", { main: J(`{"v":[{"n":1},{"n":2},{"n":3}]}`) }, arrays(`{"do":"hold","at":["v",0],"as":"first"},{"do":"reverse","at":["v"]},{"do":"set","from":"first","at":[],"key":"n","value":10},{"do":"set","at":["v",0],"key":"n","value":30},{"do":"unshift","at":["v"],"items":[{"n":0}]}`));
scenario("a removed entry's handle is detached; its copy is not", { main: J(`{"v":[{"n":1},{"n":2}]}`) }, arrays(`{"do":"pop","at":["v"],"as":"popped"},{"do":"push","at":["v"],"items":[{"$held":"popped"}]},{"do":"set","from":"popped","at":[],"key":"n","value":9},{"do":"get","at":["v",1],"key":"n"},{"do":"set","at":["v",1],"key":"n","value":7}`));
// A comparator's writes over elements are put back once the sort ends (its
// writes inside elements stay); one that unshifts leaves an entry the sort
// places twice, which becomes a copy.
scenario("a sort puts back what its comparator wrote over elements", { main: J(`{"v":[3,1,2],"w":[3,1,2]}`) }, S(`[
	{"do":"begin"},
	{"do":"sort","at":["v"],"by":".","write":[0,99]},
	{"do":"get","at":["v"],"key":0},
	{"do":"set","at":["w"],"key":1,"value":50},
	{"do":"sort","at":["w"],"by":".","write":[1,77]},
	{"do":"get","at":["w"],"key":2},
	{"do":"prepare"}
]`));
scenario("a sort whose comparator unshifts", { main: J(`{"v":[{"n":3},{"n":1},{"n":2}]}`) }, S(`[
	{"do":"begin"},
	{"do":"sort","at":["v"],"by":"n","unshiftOnce":[{"n":0}]},
	{"do":"get","at":["v"],"key":"length"},
	{"do":"set","at":["v",3],"key":"n","value":30},
	{"do":"prepare"}
]`));
// A deeply equal container assignment keeps the committed container, and a
// branch only read keeps its identity.
scenario("a deeply equal assignment keeps the committed container", { main: J(`{"n":1,"o":{"a":[1,2]},"p":{"q":{}}}`) }, S(`[
	{"do":"begin"},
	{"do":"set","at":["o"],"key":"a","value":[1,2]},
	{"do":"get","at":["p","q"],"key":"x"},
	{"do":"set","at":[],"key":"n","value":2},
	{"do":"prepare"},
	{"do":"shared","at":["o","a"]},
	{"do":"shared","at":["o"]},
	{"do":"shared","at":["p"]},
	{"do":"shared","at":[]}
]`));

// ─── Generated tracker cases ─────────────────────────────────────────────────

const generated: unknown[] = [];
{
	// generate records one change built by name: golden_test.go builds the same
	// root and makes the same edits.
	const generate = (name: string, initial: object, edit: (draft: any) => void): void => {
		const tracker = track(initial);
		const base = JSON.parse(JSON.stringify(tracker.value));
		const change = tracker.beginChange();
		edit(change.state);
		const prepared = change.prepare();
		if (canon(applyImmutable(base, prepared.ops)) !== canon(prepared.value)) throw new Error(`${name}: pi's own ops do not replay`);
		generated.push({ name, ...trackerBatch(prepared.ops), valueHash: hash(canon(prepared.value)) });
	};
	const range = (n: number) => Array.from({ length: n }, (_, value) => value);

	// delta.test.ts "keeps large prepareReplace array edits narrow" — now one
	// whole-root replacement.
	{
		const rows = range(10_000).map((value) => ({ value, stable: { value } }));
		const tracker = track({ rows });
		const replacement = tracker.value.rows.slice();
		replacement[5_000] = { value: -1, stable: replacement[5_000]!.stable };
		const prepared = tracker.prepareReplace({ rows: replacement });
		generated.push({ name: "prepareReplace-rows-10000", ...trackerBatch(prepared.ops), valueHash: hash(canon(prepared.value)) });
	}
	// state-draft.test.ts "inserts 100,000 items with unshift/splice".
	for (const method of ["unshift", "splice"] as const) {
		generate(`draft-${method}-100000`, { values: [-1] }, (draft) => {
			const items = range(100_000);
			if (method === "unshift") Reflect.apply(draft.values.unshift, draft.values, items);
			else Reflect.apply(draft.values.splice, draft.values, [1, 0, ...items]);
		});
	}
	// tracker.test.ts's piece arrays.
	generate("dense-rows-1000", { rows: range(1_000).map((value) => ({ value })) }, (draft) => {
		for (const row of draft.rows) row.value += 1;
	});
	generate("dense-values-600", { values: range(1_000) }, (draft) => {
		for (let index = 0; index < 600; index++) draft.values[index] = -index - 1;
	});
	generate("dense-nested-2000", { rows: range(2_000).map((value) => ({ nested: { value } })) }, (draft) => {
		draft.rows[0].nested.value = -1;
		for (let index = 500; index < 1_000; index++) draft.rows[index].nested.value = -index;
	});
	{
		const rows: any[] = range(400).map(() => ({ flag: 0 }));
		rows[100].special = JSON.parse(`{"__proto__":{"values":[${range(400).join(",")}]}}`);
		generate("dense-reserved-400", { rows }, (draft) => {
			for (let index = 0; index < 256; index++) draft.rows[index].flag = 1;
			for (let index = 0; index < 256; index++) draft.rows[100].special.__proto__.values[index] = -index - 1;
		});
	}
	generate(
		"dense-covered-400",
		{ rows: range(400).map((index) => ({ flag: 0, values: index === 100 ? range(400).map((value) => ({ value })) : [] })) },
		(draft) => {
			for (let index = 0; index < 256; index++) draft.rows[index].flag = 1;
			for (let index = 0; index < 256; index++) draft.rows[100].values[index].value = -index - 1;
			draft.rows[100].values.push({ value: 999 });
		},
	);
	generate("dense-disjoint-1400", { values: range(1_400).map((value) => ({ value })) }, (draft) => {
		for (const index of [97, 358, 897, 1_158]) draft.values[index].value = -index - 1;
		for (let index = 100; index < 356; index++) draft.values[index].value = -index - 1;
		for (let index = 900; index < 1_156; index++) draft.values[index].value = -index - 1;
	});
	generate("queue-20000", { values: range(20_000).map((value) => ({ value })) }, (draft) => {
		for (let index = 0; index < 10_000; index++) {
			draft.values.shift();
			draft.values.push({ value: 20_000 + index });
		}
	});
	generate("fragmentation-20000", { values: range(20_000).map((value) => ({ value })) }, (draft) => {
		const held = [1, 1_001, 5_001, 10_001, 15_001, 19_999].map((index) => draft.values[index]);
		for (let index = 0; index < 20_000; index += 2) draft.values.splice(index, 1, { value: -index - 1 });
		for (const value of held) value.value += 100_000;
	});
	generate("null-overrides-10000", { values: range(10_000) }, (draft) => {
		draft.values[17] = null;
		draft.values[9_000] = null;
	});
	// tracker.test.ts (091bcc099) "emits one insert for an append to a long array".
	generate("append-10000", { values: range(10_000) }, (draft) => {
		draft.values.push(-1);
	});
	// The operation cap: 4,096 ops publish; one more is a replacement.
	for (const n of [4_096, 4_097]) {
		const initial: Record<string, number> = {};
		for (let i = 0; i < n; i++) initial[i.toString(36)] = 0;
		generate(`op-cap-object-${n}`, initial, (draft) => {
			for (let i = 0; i < n; i++) draft[i.toString(36)] = 1;
		});
		generate(`op-cap-array-${n}`, { v: range(n).map(() => ({ x: 0 })), w: range(n).map(() => 0) }, (draft) => {
			draft.v.reverse();
			for (let i = 0; i < n; i++) draft.w[i] = i % 3 === 0 ? 1 : 0;
		});
	}
}

// ─── Fuzz: state-fuzz.test.ts ────────────────────────────────────────────────

const random =
	(seed: number): (() => number) =>
	() => {
		seed = (seed + 0x6d2b79f5) | 0;
		let value = Math.imul(seed ^ (seed >>> 15), 1 | seed);
		value = (value + Math.imul(value ^ (value >>> 7), 61 | value)) ^ value;
		return ((value ^ (value >>> 14)) >>> 0) / 4_294_967_296;
	};

const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;

function fuzzMutate(document: any, choice: number, value: number): void {
	const item = () => ({ id: value, text: `item-${value}`, score: value % 7 });
	switch (choice) {
		case 0:
			document.text += `-${value}`;
			break;
		case 1:
			document.text = `${document.text.slice(Math.min(2, document.text.length))}${value}`;
			break;
		case 2:
			document.items.push(item());
			break;
		case 3:
			document.items.unshift(item());
			break;
		case 4:
			if (document.items.length > 0) document.items.shift();
			break;
		case 5:
			if (document.items.length > 0) document.items.pop();
			break;
		case 6: {
			const index = document.items.length === 0 ? 0 : value % (document.items.length + 1);
			document.items.splice(index, document.items.length === 0 ? 0 : value % 2, item());
			break;
		}
		case 7:
			document.items.reverse();
			break;
		case 8:
			document.items.sort((left: any, right: any) => left.id - right.id);
			break;
		case 9:
			if (document.items.length > 0) document.items[value % document.items.length].score = value;
			break;
		case 10:
			document.meta.revision += 1;
			document.meta.label = `revision-${value}`;
			break;
		case 11:
			delete document.meta.label;
			break;
		case 12:
			if (document.items.length > 1) document.items[1] = clone(document.items[0]);
			break;
		default:
			for (let index = 0; index < Math.min(2, document.items.length); index++) document.items[index] = item();
	}
}

const fuzz: unknown[] = [];
for (let seed = 1; seed <= 100; seed++) {
	const rng = random(seed);
	const initial = {
		items: Array.from({ length: 4 }, (_, id) => ({ id, text: `item-${id}`, score: 0 })),
		text: "start",
		meta: { revision: 0 },
	};
	const tracker = track(initial);
	const expected = clone(initial);
	let replica = clone(tracker.value);
	const stream = createHash("sha256");
	for (let step = 0; step < 100; step++) {
		const choice = Math.floor(rng() * 14);
		const value = seed * 1_000 + step;
		fuzzMutate(expected, choice, value);
		const change = tracker.beginChange();
		fuzzMutate(change.state, choice, value);
		const prepared = change.prepare();
		replica = applyImmutable(replica, prepared.ops);
		tracker.adopt(prepared);
		if (canon(tracker.value) !== canon(expected) || canon(replica) !== canon(expected)) {
			throw new Error(`fuzz seed ${seed} step ${step} diverged in pi itself`);
		}
		stream.update(`${canon(prepared.ops)}\n${canon(tracker.value)}\n`);
	}
	fuzz.push({ seed, hash: stream.digest("hex") });
}

// ─── Differential: random scripts over every draft operation ─────────────────

const differential: unknown[] = [];
{
	const pool = ["a", "b", "z", "é", "10", "7", "0", "k"];
	const chunks = ["x", "yz", "😀", "é", "", "0123456789", "\n", "\"q\""];
	for (let script = 0; script < 20; script++) {
		const rng = random(10_000 + script);
		const int = (n: number) => Math.floor(rng() * n);
		const pick = <T>(xs: readonly T[]): T => xs[int(xs.length)]!;
		const initial = J(
			`{"items":[{"id":1,"tags":["a"],"text":"one"},{"id":2,"tags":[],"text":"two"}],"meta":{"n":0},"text":"start","grid":[[1,2],[3]],"flags":[true,false,null]}`,
		);
		const runner = new Runner(true);
		runner.trackers.set("main", track(initial));
		const steps: Step[] = [];
		const results: unknown[] = [];
		const run = (step: Step): Record<string, unknown> | undefined => {
			steps.push(step);
			let result: Record<string, unknown>;
			try {
				result = runner.exec(step);
			} catch (error) {
				result = { error: (error as Error).message };
			}
			results.push(result);
			return result;
		};
		let nextId = 100;
		const item = () => ({ id: nextId++, tags: Array.from({ length: int(3) }, () => pick(pool)), text: pick(chunks) });
		for (let change = 0; change < 14; change++) {
			if (int(10) === 0) {
				const value = clone(runner.trackers.get("main").value);
				if (value.items.length > 0) value.items[int(value.items.length)].text += "!";
				value.meta[pick(pool)] = int(5);
				run({ do: "replace", value });
				run({ do: "adopt" });
				continue;
			}
			run({ do: "begin" });
			const state = runner.changes.get("main").state;
			const mutations = 1 + int(4);
			for (let m = 0; m < mutations; m++) {
				const items = state.items.length;
				switch (int(20)) {
					case 0:
						run({ do: "append", at: [], key: "text", text: pick(chunks) });
						break;
					case 1: {
						const chars = [...(state.text as string)];
						run({ do: "set", at: [], key: "text", value: chars.slice(int(chars.length + 1)).join("") + pick(chunks) });
						break;
					}
					case 2:
						run({ do: "push", at: ["items"], items: Array.from({ length: 1 + int(2) }, item) });
						break;
					case 3:
						run({ do: "unshift", at: ["items"], items: [item()] });
						break;
					case 4:
						run({ do: pick(["pop", "shift"]), at: ["items"] });
						break;
					case 5:
						run({
							do: "splice",
							at: ["items"],
							start: int(items + 3) - 2,
							deleteCount: int(3),
							items: Array.from({ length: int(3) }, item),
						});
						break;
					case 6:
						run({ do: "sort", at: ["items"], by: "id", desc: int(2) === 0 });
						break;
					case 7:
						run({ do: "sort", at: ["flags"], by: null });
						break;
					case 8:
						run({ do: "reverse", at: pick([["items"], ["flags"], ["grid"], ["grid", 0]]) });
						break;
					case 9:
						if (items > 0) {
							const at = ["items", int(items)];
							const current = state.items[at[1] as number].text as string;
							switch (int(3)) {
								case 0:
									run({ do: "append", at, key: "text", text: pick(chunks) });
									break;
								case 1:
									run({ do: "set", at, key: "text", value: [...current].slice(1).join("") + pick(chunks) });
									break;
								default:
									run({ do: "set", at, key: "id", value: nextId++ });
							}
						}
						break;
					case 10:
						run({ do: "set", at: ["meta"], key: pick(pool), value: pick([int(9), pick(chunks), null, { n: int(3) }, [int(3)]]) });
						break;
					case 11:
						run({ do: "delete", at: ["meta"], key: pick(pool) });
						break;
					case 12:
						if (items > 1) run({ do: "set", at: ["items"], key: int(items), value: { $held: hold(["items", int(items)]) } });
						break;
					case 13: {
						const row = int(state.grid.length);
						const length = state.grid[row].length;
						run({ do: "fill", at: ["grid", row], value: pick([0, "f", null, { g: 1 }]), start: int(length + 2) - 1, end: int(length + 2) });
						break;
					}
					case 14:
						if (items > 0) run({ do: "copyWithin", at: ["items"], target: int(items), start: int(items), end: int(items + 1) });
						break;
					case 15: {
						const row = int(state.grid.length);
						const length = state.grid[row].length;
						const next = int(length + 3);
						run({ do: "setLength", at: ["grid", row], length: next });
						if (next > length) run({ do: "fill", at: ["grid", row], value: int(9), start: length, end: next });
						break;
					}
					case 16:
						if (items > 0) {
							const name = hold(["items", int(items)]);
							run({ do: "unshift", at: ["items"], items: [item()] });
							run({ do: "set", from: name, at: ["tags"], key: 0, value: pick(pool) });
						}
						break;
					case 17:
						run({ do: "set", at: ["grid"], key: int(state.grid.length + 1), value: Array.from({ length: int(3) }, () => int(9)) });
						break;
					case 18:
						if (items > 0) run({ do: "set", at: ["items", int(items)], key: "tags", value: [pick(pool), pick(pool)] });
						break;
					default:
						run({ do: "set", at: [], key: "text", value: state.text });
				}
			}
			if (int(12) === 0) {
				run({ do: "abort" });
				continue;
			}
			run({ do: "prepare" });
			run({ do: "adopt" });
		}
		function hold(at: (string | number)[]): string {
			const name = `h${steps.length}`;
			run({ do: "hold", at, as: name });
			return name;
		}
		run({ do: "value" });
		differential.push({ script, initial, steps: RAW(canon(steps)), results });
	}
}

const out = { sha, diffs, scenarios, generated, fuzz, differential };
fs.writeFileSync(outFile, `${write(out)}\n`);
console.log(`wrote ${outFile}: ${diffs.length} diffs, ${scenarios.length} scenarios, ${generated.length} generated, ${fuzz.length} fuzz seeds, ${differential.length} differential scripts`);
