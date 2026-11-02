// Captures what upstream pi-schema (packages/schema/src) does for a corpus of
// schemas, options and values — the oracle behind TestGoldenMatchesUpstream,
// TestResolveURIMatchesUpstream, TestRegexMatchesUpstream and
// TestTypeBoxConvertMatchesUpstream in schema/golden_test.go.
//
//   node capture.mjs <upstream tree> <typebox package dir> > capture-<sha>.json
//
// <upstream tree> is an extracted upstream checkout at the sha
// (`git -C ~/.cache/pi-upstream archive <sha> packages/schema | tar -x -C
// <tree>`); nothing is written into it. <typebox package dir> is TypeBox
// 1.3.27, upstream's dev dependency (`npm install typebox@1.3.27` in a scratch
// directory, then pass its node_modules/typebox). Node v22 strips the types of
// the .ts sources.
//
// Sections:
//   - rows: compile(schema, options) and, per value, check, errors (path,
//     keyword, message, in order), and with coerce: true coerce (the value,
//     and whether it came back as itself) and parse; or the compile error.
//     A call that throws is recorded as {"$":"throws","message": ...} in
//     place of its result (a coerce that throws has no coerceSame).
//   - uris: resolver.ts resolveUri(ref, base).
//   - regexes: shared.ts sharedRegex(source): its SyntaxError, or test() of
//     each string.
//   - typebox: every Value.Convert of test/coerce-typebox.test.ts (pass 2
//     alone), run with stand-ins for vitest's describe/it/expect, recorded
//     at Coercer.prototype.convertOnly; the tests the file skips are listed.
//
// Values that JSON cannot hold are tagged objects {"$": ...}: "undefined",
// "-0", "NaN", "Infinity", "-Infinity", {"$":"bigint","v":"12"} and
// {"$":"object","v":[[key, value], ...]} for an object with a "$" key; a
// value of any other kind is {"$":"unsupported"}. The formats named in
// options are the stand-ins defined below (the real ones are tested with
// schema/formats).
import { registerHooks } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [tree, typeboxDir] = process.argv.slice(2);
if (!tree || !typeboxDir) {
	console.error("usage: node capture.mjs <upstream tree> <typebox package dir>");
	process.exit(2);
}
const src = (file) => pathToFileURL(path.join(tree, "packages/schema/src", file)).href;
const { compile } = await import(src("index.ts"));
const { resolveUri } = await import(src("resolver.ts"));
const { sharedRegex } = await import(src("shared.ts"));
const { Coercer } = await import(src("coerce.ts"));

// Stand-in formats, mirrored in golden_test.go.
const FORMATS = {
	even: (s) => s.length % 2 === 0,
	lower: (s) => s === s.toLowerCase(),
};

function enc(v) {
	if (v === undefined) return { $: "undefined" };
	if (typeof v === "number") {
		if (Object.is(v, -0)) return { $: "-0" };
		if (Number.isNaN(v)) return { $: "NaN" };
		if (v === Infinity) return { $: "Infinity" };
		if (v === -Infinity) return { $: "-Infinity" };
		return v;
	}
	if (typeof v === "bigint") return { $: "bigint", v: String(v) };
	if (v === null || typeof v === "string" || typeof v === "boolean") return v;
	if (v instanceof Uint8Array) return { $: "bytes", v: [...v] };
	if (Array.isArray(v)) return v.map(enc);
	if (typeof v === "object" && Object.getPrototypeOf(v) === Object.prototype) {
		if (Object.hasOwn(v, "$")) return { $: "object", v: Object.entries(v).map(([k, x]) => [k, enc(x)]) };
		return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, enc(x)]));
	}
	return { $: "unsupported", type: typeof v };
}

function capture(name, schema, options, values) {
	const row = { name, schema: enc(schema), options: encOptions(options) };
	let validator;
	try {
		validator = compile(schema, { ...options, formats: options.formats && pick(options.formats) });
	} catch (error) {
		row.compileError = String(error.message);
		return row;
	}
	row.cases = values.map((value) => {
		const out = { value: enc(value), check: attempt(() => validator.check(value)), errors: attempt(() => validator.errors(value)) };
		if (options.coerce) {
			let coerced;
			try {
				coerced = validator.coerce(value);
				out.coerce = enc(coerced);
				out.coerceSame = coerced === value;
			} catch (error) {
				out.coerce = thrown(error);
			}
			out.parse = attempt(() => {
				const parsed = validator.parse(value);
				return parsed.ok ? { ok: true, value: enc(parsed.value) } : { ok: false, errors: parsed.errors };
			});
		}
		return out;
	});
	return row;
}

function thrown(error) {
	return { $: "throws", message: String(error.message) };
}

function attempt(f) {
	try {
		return f();
	} catch (error) {
		return thrown(error);
	}
}

function encOptions(options) {
	const out = {};
	if (options.draft) out.draft = options.draft;
	if (options.coerce) out.coerce = true;
	if (options.unresolvedRefs) out.unresolvedRefs = options.unresolvedRefs;
	if (options.formats) out.formats = options.formats;
	if (options.schemas) out.schemas = Object.fromEntries(Object.entries(options.schemas).map(([k, s]) => [k, enc(s)]));
	return out;
}

function pick(names) {
	return Object.fromEntries(names.map((n) => [n, FORMATS[n]]));
}

// ---------------------------------------------------------------------------------------------------------------
// The corpus
// ---------------------------------------------------------------------------------------------------------------

const rows = [];
const add = (name, schema, values, options = {}) => rows.push(capture(name, schema, options, values));
const coerceRows = (name, schema, values, options = {}) => add(name, schema, values, { ...options, coerce: true });

const scalars = [null, true, false, 0, -0, 1, 1.5, -7, "", "a", "1", [], {}, [1], { a: 1 }];

// type
for (const t of ["string", "number", "integer", "boolean", "null", "array", "object", "bytes", "constructor"]) {
	add(`type ${t}`, { type: t }, [...scalars, new Uint8Array(2)]);
}
add("type list", { type: ["integer", "string"] }, scalars);
add("type list with non-strings", { type: [1, null, "string", ["a", null]] }, [1, "x"]);
add("type null value", { type: null }, [null, 1]);
add("type with title", { type: "string", title: "t", description: "d" }, [1, "x"]);

// enum and const
add("enum short", { enum: ["a", 1, null, true] }, ["a", 1, null, true, "b", 2, false, -0]);
add("enum NaN-free long", { enum: ["a", "b", "c", "d", "e", "f", "g", "h", "i", 1, 2] }, ["i", 2, "z", 3, -0]);
add("enum objects", { enum: [{ a: 1, b: [1, 2] }, [1, { c: null }], "x"] }, [
	{ b: [1, 2], a: 1 },
	{ a: 1, b: [2, 1] },
	[1, { c: null }],
	[1, { c: 0 }],
	"x",
	"y",
]);
add("enum long message", { enum: Array.from({ length: 25 }, (_, i) => `v${i}`) }, ["v0", "nope"]);
add("enum with numbers message", { enum: [1e21, 0.1, -0, 1e-7, 123456789012345680000] }, [5]);
add("const primitive", { const: "x" }, ["x", "y", 1]);
add("const number", { const: 0 }, [0, -0, "0", false]);
add("const object", { const: { b: 1, a: [true, null] } }, [{ a: [true, null], b: 1 }, { a: [true], b: 1 }, null]);
add("const array", { const: [1, "2"] }, [[1, "2"], ["1", 2], [1, "2", 3]]);
add("const null", { const: null }, [null, 0, "null"]);

// strings
add("minLength maxLength", { type: "string", minLength: 2, maxLength: 3 }, ["a", "ab", "abcd", "😀", "😀😀", "😀😀😀😀", "\ud800A", "\ud800", "\udc00\ud800", "é"]);
add("length only (no type)", { minLength: 1 }, ["", 5, "x"]);
add("pattern", { type: "string", pattern: "^[a-z]+\\d$" }, ["ab1", "AB1", "ab", 3]);
add("pattern unanchored", { pattern: "b" }, ["abc", "xyz", null]);
add("pattern escapes", { pattern: "^\\w\\s\\d\\/\\.$" }, ["a 1/.", "é 1/.", "a\u00a01/.", "a\u20281/."]);
add("pattern dot", { pattern: "^.$" }, ["a", "\n", "\r", "\u2028", "😀", "é"]);
add("pattern non-BMP", { pattern: "^🐲*$" }, ["", "🐲🐲", "🐉"]);
add("pattern unicode property", { pattern: "^\\p{L}+$" }, ["héllo", "123"]);
add("pattern annex B", { pattern: "^\\a{1,2}\\-]$" }, ["a-]", "aa-]", "a"]);
add("format stand-in", { type: "string", format: "even" }, ["ab", "abc", 1], { formats: ["even", "lower"] });
add("format unknown name", { format: "nope" }, ["anything"], { formats: ["even"] });
add("format without formats", { format: "even" }, ["abc"]);
add("format and length", { type: "string", format: "lower", minLength: 2 }, ["A", "ab", "AB"], { formats: ["lower"] });

// numbers
add("number bounds", { type: "number", minimum: 1, maximum: 10, exclusiveMinimum: 0, exclusiveMaximum: 10 }, [0, 1, 5, 10, 11, -0, "5"]);
add("exclusive only", { exclusiveMinimum: 1.5, exclusiveMaximum: 2.5 }, [1.5, 2, 2.5]);
add("integer bounds", { type: "integer", minimum: 1 }, [0, 1, 1.5, 2 ** 53 + 2, 1e21]);
add("multipleOf decimal", { multipleOf: 0.1 }, [0.1 + 0.2, 0.3, 0.35, 1e21, 7, -0.2]);
add("multipleOf small", { multipleOf: 0.0001 }, [0.0075, 0.00751, 1, 1e-5]);
add("multipleOf integer", { multipleOf: 3 }, [9, 10, -9, 2 ** 53 + 1, 1.5]);
add("multipleOf huge quotient", { multipleOf: 1e-308 }, [1e308, 1]);
add("multipleOf exponent forms", { multipleOf: 1e-7 }, [3e-7, 3.5e-7, 1e21, 123456789]);
add("number messages", { minimum: 1e21, maximum: -1e-7, multipleOf: 0.30000000000000004 }, [5]);

// objects
const read = {
	type: "object",
	properties: { path: { type: "string" }, offset: { type: "integer", minimum: 1 } },
	required: ["path"],
	additionalProperties: false,
};
add("closed object", read, [{ path: "a" }, { offset: 0, extra: true }, { path: 1, offset: 1.5, b: 1, a: 2 }, "x", []]);
add("closed object index keys", read, [JSON.parse('{"b":1,"10":2,"path":3,"2":4,"a":5}')]);
add("required not declared", { required: ["a", "b"], properties: { a: { type: "string" } } }, [{}, { a: 1 }, { b: 1 }, 5]);
add("required non-strings", { required: ["a", 1, null] }, [{}, { a: 1 }]);
add("open object additional schema", {
	type: "object",
	properties: { a: { type: "number" } },
	additionalProperties: { type: "string" },
}, [{ a: 1, b: "x" }, { a: "1", b: 2, c: 3 }]);
add("patternProperties", {
	patternProperties: { "^n_": { type: "number" }, "_x$": { minimum: 5 } },
	additionalProperties: false,
}, [{ n_a: 1, n_x: 7 }, { n_x: 1, other: 1, n_b: "s" }]);
add("pointer escapes", { type: "object", properties: { "a/b~c": { type: "string" } } }, [{ "a/b~c": 1 }]);
add("propertyNames", { propertyNames: { pattern: "^[a-z]+$", maxLength: 3 } }, [{ ab: 1 }, { ABC: 1, abcd: 2, ok: 3 }]);
add("property counts", { minProperties: 2, maxProperties: 3 }, [{ a: 1 }, { a: 1, b: 2 }, { a: 1, b: 2, c: 3, d: 4 }]);
add("dependentRequired", { dependentRequired: { a: ["b", "c"], d: [] } }, [{ a: 1 }, { a: 1, b: 1, c: 1 }, { d: 1 }]);
add("dependentSchemas", { dependentSchemas: { a: { required: ["z"] }, b: false } }, [{ a: 1 }, { b: 1 }, { c: 1 }]);
add("dependencies draft 7", { dependencies: { a: ["b"], c: { required: ["d"] } } }, [{ a: 1, c: 1 }, { a: 1, b: 1 }], { draft: "7" });
add("dependentRequired ignored in draft 7", { dependentRequired: { a: ["b"] } }, [{ a: 1 }], { draft: "7" });
add("dependencies and dependentRequired merged", {
	dependentRequired: { a: ["x"], b: ["y"] },
	dependencies: { a: ["z"] },
}, [{ a: 1, b: 2 }]);
add("Object.prototype names", {
	type: "object",
	properties: { constructor: { type: "string" }, toString: { type: "number" } },
	required: ["constructor"],
	additionalProperties: false,
}, [{}, { constructor: "a" }, { constructor: "a", toString: "b" }, JSON.parse('{"__proto__":1,"constructor":"x"}')]);
add("error order across keywords", {
	type: "object",
	properties: { a: { type: "string" }, b: { type: "string" }, c: { minimum: 3 } },
	required: ["z", "a", "y"],
	patternProperties: { "^c": { maximum: 1 } },
	additionalProperties: false,
	propertyNames: { maxLength: 1 },
	minProperties: 9,
}, [{ c: 2, b: 1, extra: 0, a: 2 }]);

// arrays
add("items", { type: "array", items: { type: "string" }, minItems: 1, maxItems: 2 }, [[], ["a"], [1, "b", 2], "x"]);
add("prefixItems closed", { type: "array", prefixItems: [{ type: "string" }, { type: "number" }], items: false }, [["a", 1], ["a", "b"], ["a", 1, 2], [1]]);
add("prefixItems in draft 7 ignored", { prefixItems: [{ type: "string" }] }, [[1]], { draft: "7" });
add("draft 7 items array", { items: [{ type: "string" }], additionalItems: { type: "number" } }, [["a", 1], [1, "b"]], { draft: "7" });
add("items array in 2020-12", { items: [{ type: "string" }], additionalItems: false }, [["a"], ["a", 1]]);
add("uniqueItems", { uniqueItems: true }, [[1, 2], [1, 1], [{ a: 1, b: 2 }, { b: 2, a: 1 }], [[1], [1]], [0, -0], [1, true]]);
add("contains", { type: "array", contains: { type: "number" } }, [["a"], [1], []]);
add("contains bounds", { contains: { type: "number" }, minContains: 2, maxContains: 3 }, [[1], [1, 2], [1, 2, 3, 4]]);
add("contains max only", { contains: { type: "string" }, maxContains: 0, minContains: 0 }, [["a"], [1]]);
add("contains max one", { contains: { type: "string" }, maxContains: 1 }, [["a", "b"]]);
add("contains in draft 7", { contains: { type: "number" }, minContains: 2 }, [[1], []], { draft: "7" });

// combinators
const block = {
	anyOf: [
		{ type: "object", properties: { kind: { const: "text" }, text: { type: "string" } }, required: ["kind", "text"], additionalProperties: false },
		{ type: "object", properties: { kind: { const: "image" }, width: { type: "integer", minimum: 0 } }, required: ["kind", "width"], additionalProperties: false },
	],
};
add("tagged union", { type: "array", items: block }, [[{ kind: "text", text: "a" }, { kind: "image", width: -1 }], [{ kind: "video" }], [{}], ["x"]]);
add("tagged oneOf", { oneOf: block.anyOf }, [{ kind: "text", text: 1 }, { kind: 1 }]);
add("anyOf deepest", { anyOf: [{ type: "string", minLength: 3 }, { type: "object", properties: { a: { properties: { b: { type: "string" } } } } }] }, ["ab", { a: { b: 1 } }, 5]);
add("anyOf declared most", {
	anyOf: [
		{ type: "object", properties: { x: { type: "string" } }, required: ["x"] },
		{ type: "object", properties: { y: { type: "string" }, z: { type: "string" } }, required: ["y"] },
	],
}, [{ y: 1, z: 2 }, { x: 1 }, {}]);
add("anyOf no errors", { anyOf: [{ not: {} }, false] }, [1]);
add("oneOf more than one", { oneOf: [{ type: "number" }, { minimum: 0 }] }, [1, -1, "x"]);
add("oneOf none", { oneOf: [{ type: "string" }, { type: "boolean" }] }, [1]);
add("const union", { anyOf: [{ const: "a" }, { const: 1, title: "one" }, { const: null }] }, ["a", 1, null, "b"]);
add("const union oneOf duplicates", { oneOf: [{ const: 1 }, { const: 1 }] }, [1, 2]);
add("allOf", { allOf: [{ minimum: 1 }, { maximum: 5 }, { multipleOf: 2 }] }, [3, 0, 8]);
add("not", { not: { type: "string" } }, ["a", 1]);
add("if then else", { if: { type: "number" }, then: { minimum: 1 }, else: { type: "string" } }, [0, 1, "a", true]);
add("if without then or else", { if: { type: "number" }, minimum: 2 }, [1, "a"]);
add("false schema", false, [1, null]);
add("true schema", true, [1]);
add("property false", { properties: { a: false } }, [{ a: 1 }, {}]);
add("empty schema", {}, [1]);

// references
add("$ref $defs", {
	$defs: { point: { type: "object", properties: { x: { type: "number" } }, required: ["x"] } },
	type: "array",
	items: { $ref: "#/$defs/point" },
}, [[{ x: 1 }, {}], [{ x: "a" }]]);
add("recursive tree", {
	type: "object",
	properties: { value: { type: "number" }, children: { type: "array", items: { $ref: "#" } } },
	required: ["value"],
}, [{ value: 1, children: [{ value: 2, children: [{}] }] }, { value: 1, children: [{ value: 2 }] }]);
add("same object through a ref at two paths", {
	type: "object",
	properties: { a: { $ref: "#/$defs/P" }, b: { $ref: "#/$defs/P" } },
	$defs: { P: { type: "object", properties: { n: { type: "integer" } } } },
}, [(() => { const shared = { n: "x" }; return { a: shared, b: shared }; })()]);
add("recursive oneOf filter", {
	type: "object",
	properties: { where: { $ref: "#/$defs/F" } },
	$defs: {
		F: {
			oneOf: [
				{ type: "object", properties: { field: { type: "string" }, eq: { type: "string" } }, required: ["field", "eq"], additionalProperties: false },
				{ type: "object", properties: { and: { type: "array", items: { $ref: "#/$defs/F" } } }, required: ["and"], additionalProperties: false },
			],
		},
	},
}, [{ where: { and: [{ field: "a", eq: 1, x: 2 }] } }, { where: { field: "a" } }]);
add("$anchor and $id", {
	$id: "https://example.com/root.json",
	$defs: { a: { $anchor: "thing", type: "string" }, b: { $id: "other.json", type: "number" } },
	properties: { a: { $ref: "#thing" }, b: { $ref: "other.json" }, c: { $ref: "https://example.com/other.json" } },
}, [{ a: 1, b: "x", c: "y" }, { a: "s", b: 1, c: 2 }]);
add("registered document", { $ref: "https://example.com/point.json" }, [{ x: 1 }, {}], {
	schemas: { "https://example.com/point.json": { type: "object", properties: { x: { type: "number" } }, required: ["x"] } },
});
add("percent-encoded pointer", {
	$defs: { "a b": { type: "string" }, "c%d": { type: "number" }, "é": { type: "boolean" }, "x/y": { type: "null" } },
	properties: { p: { $ref: "#/$defs/a%20b" }, q: { $ref: "#/$defs/c%25d" }, r: { $ref: "#/$defs/%C3%A9" }, s: { $ref: "#/$defs/x~1y" } },
}, [{ p: 1, q: "x", r: 1, s: 1 }]);
add("unresolved throws", { type: "object", properties: { t: { $ref: "#/definitions/Missing" } } }, [{}]);
add("unresolved remote throws", { properties: { t: { $ref: "https://example.com/schemas/t.json" } } }, [{}]);
add("unresolved anchor throws", { properties: { t: { $ref: "#nothing" } } }, [{}]);
add("unresolved reject", { type: "object", properties: { t: { $ref: "#/definitions/Missing" } } }, [{}, { t: 1 }, { t: { a: 1 } }], { unresolvedRefs: "reject" });
add("unresolved reject remote", { properties: { t: { $ref: "https://example.com/schemas/t.json" }, u: { $ref: "A" } } }, [{ t: 1, u: { x: "1" } }], { unresolvedRefs: "reject" });
add("unresolved reject coerce", { type: "object", properties: { t: { $ref: "#/definitions/Missing" }, n: { type: "number" } } }, [{ t: "1", n: "2" }], { unresolvedRefs: "reject", coerce: true });
add("draft 7 ref overrides siblings", { definitions: { s: { type: "string" } }, $ref: "#/definitions/s", maxLength: 1 }, ["ab", 1], { draft: "7" });
add("draft 2020 ref with siblings", { $defs: { s: { type: "string" } }, $ref: "#/$defs/s", maxLength: 1 }, ["ab", 1]);
add("dynamicRef extendible", {
	$id: "https://example.com/tree",
	$dynamicAnchor: "node",
	type: "object",
	properties: { data: true, children: { type: "array", items: { $dynamicRef: "#node" } } },
	$defs: {
		strict: {
			$id: "https://example.com/strict-tree",
			$dynamicAnchor: "node",
			$ref: "tree",
			unevaluatedProperties: false,
		},
	},
}, [{ children: [{ daat: 1 }] }, { children: [{ data: 1 }] }]);
add("dynamicRef strict", { $ref: "https://example.com/strict-tree" }, [{ children: [{ daat: 1 }] }, { children: [{ data: 1 }] }], {
	schemas: {
		"https://example.com/tree": {
			$id: "https://example.com/tree",
			$dynamicAnchor: "node",
			type: "object",
			properties: { data: true, children: { type: "array", items: { $dynamicRef: "#node" } } },
		},
		"https://example.com/strict-tree": {
			$id: "https://example.com/strict-tree",
			$dynamicAnchor: "node",
			$ref: "tree",
			unevaluatedProperties: false,
		},
	},
});
add("dynamicRef plain", { $defs: { a: { type: "string" } }, $dynamicRef: "#/$defs/a" }, [1, "x"]);

// unevaluated
add("unevaluatedProperties", {
	type: "object",
	properties: { a: { type: "string" } },
	allOf: [{ properties: { b: { type: "number" } } }],
	anyOf: [{ properties: { c: true }, required: ["c"] }, { properties: { d: true } }],
	unevaluatedProperties: false,
}, [{ a: "x", b: 1, c: 1 }, { a: "x", e: 1, f: 2 }, { a: 1, e: 1 }]);
add("unevaluatedProperties schema", { properties: { a: true }, unevaluatedProperties: { type: "number" } }, [{ a: "x", b: 1, c: "y" }]);
add("unevaluatedProperties if", {
	if: { properties: { k: { const: "x" } }, required: ["k"] },
	then: { properties: { x: true } },
	else: { properties: { y: true } },
	unevaluatedProperties: false,
}, [{ k: "x", x: 1 }, { k: "x", y: 1 }, { y: 1 }]);
add("unevaluatedItems", { prefixItems: [{ type: "string" }], contains: { type: "number" }, unevaluatedItems: false }, [["a", 1], ["a", true, 1], ["a", 1, null]]);
add("unevaluatedItems schema", { allOf: [{ prefixItems: [true] }], unevaluatedItems: { type: "string" } }, [[1, "a", 2]]);
add("unevaluated in draft 7 ignored", { properties: { a: true }, unevaluatedProperties: false }, [{ b: 1 }], { draft: "7" });

// compile errors
add("malformed property", { properties: { a: 1 } }, []);
add("malformed items", { items: "x" }, []);
add("malformed additionalItems null", { items: [true], additionalItems: null }, []);
add("invalid pattern", { pattern: "(" }, []);
add("invalid pattern bracket", { pattern: "[a" }, []);
add("invalid pattern repeat", { pattern: "a**" }, []);
add("invalid pattern unmatched", { patternProperties: { "x)": true } }, []);
add("invalid pattern trailing backslash", { pattern: "a\\" }, []);
add("invalid pattern group", { pattern: "(?i)a" }, []);
add("invalid pattern range", { pattern: "[z-a]" }, []);
add("invalid pattern bounds", { pattern: "a{2,1}" }, []);

// a subschema the check never compiles is compiled by errors() and coerce()
coerceRows("malformed if compiled by errors", { if: { pattern: "(" }, minimum: 3 }, [1, 5]);
coerceRows("malformed if in a property", { type: "object", properties: { a: { type: "string", minLength: 5, if: { pattern: "(" } } } }, [{ a: "x" }, { a: "xxxxx" }]);
coerceRows("malformed draft 7 prefixItems met by coerce", { prefixItems: [{ patternProperties: { "(": true } }] }, [[{ a: 1 }], 5], { draft: "7" });

// cycles on one value: upstream's check overflows V8's stack where a value reaches them
const cycles = [
	["ref to root", { $ref: "#" }, [{}, 5]],
	["allOf ref to root", { type: "object", allOf: [{ $ref: "#" }] }, [{}, 5]],
	["ref and type", { $ref: "#", type: "object" }, [{}, 5]],
	["self ref def", { $defs: { A: { $ref: "#/$defs/A" } }, $ref: "#/$defs/A" }, [{}, 5]],
	["mutual ref defs", { $defs: { A: { $ref: "#/$defs/B" }, B: { allOf: [{ $ref: "#/$defs/A" }] } }, anyOf: [{ type: "string" }, { $ref: "#/$defs/A" }] }, ["s", 5]],
	["anyOf passes first", { anyOf: [{ type: "string" }, { $ref: "#" }] }, ["s", 5]],
	["recursion into properties", { type: "object", properties: { x: { $ref: "#" } } }, [{}, { x: {} }, { x: 5 }, { x: { x: { x: 1 } } }]],
	["cycle reached through a child first", { anyOf: [{ type: "object" }, { $ref: "#/$defs/Y" }], properties: { p: { $ref: "#/$defs/Y" } }, $defs: { Y: { $ref: "#" } } }, [{}, { p: 5 }, 5]],
	["cycle only the explainer meets", { type: "object", minProperties: 1, allOf: [{ $ref: "#" }] }, [{}, { a: 1 }, 5]],
	["unevaluated cycle", { unevaluatedProperties: false, allOf: [{ $ref: "#" }] }, [{}, { a: 1 }]],
	["unevaluated cycle the explainer meets", { minProperties: 2, allOf: [{ $ref: "#" }], unevaluatedProperties: false }, [{ a: 1 }]],
	["not cycle", { not: { $ref: "#" } }, [1]],
	["dependentSchemas cycle", { dependentSchemas: { a: { $ref: "#" } } }, [{}, { a: 1 }]],
	["if cycle", { if: { $ref: "#" }, then: true }, [1]],
	["if alone is no cycle", { if: { $ref: "#" } }, [1]],
	["items and allOf cycle", { items: { $ref: "#" }, minItems: 1, allOf: [{ $ref: "#" }] }, [[], [[]], 1]],
	["dynamicRef cycle", { $dynamicAnchor: "a", $dynamicRef: "#a" }, [1, {}]],
	["dynamicRef cycle in a def", { $defs: { x: { $dynamicAnchor: "a", $dynamicRef: "#a" } }, $ref: "#/$defs/x" }, [1]],
	["dynamicRef recursion into properties", { $dynamicAnchor: "a", type: "object", properties: { c: { $dynamicRef: "#a" } } }, [{ c: { c: 1 } }, { c: { c: {} } }]],
];
for (const [name, schema, values] of cycles) coerceRows(name, schema, values);
add("draft 7 ref cycle", { definitions: { A: { $ref: "#/definitions/A" } }, $ref: "#/definitions/A" }, [1], { draft: "7" });

// bytes
add("bytes bounds", { type: "object", properties: { data: { type: "bytes", minLength: 1, maxLength: 4 } } }, [
	{ data: new Uint8Array(4) },
	{ data: new Uint8Array(5) },
	{ data: new Uint8Array(0) },
	{ data: [1, 2] },
]);

// coercion: pass 1
coerceRows("optional nulls", {
	type: "object",
	properties: {
		path: { type: "string" },
		offset: { type: "number" },
		nullable: { anyOf: [{ type: "string" }, { type: "null" }] },
		ref: { $ref: "#/$defs/n" },
		req: { type: "number" },
		metadata: { type: "object", properties: { enabled: { type: "boolean" } }, additionalProperties: false },
	},
	required: ["path", "req"],
	$defs: { n: { type: "number" } },
}, [
	{ path: "f", offset: null, nullable: null, ref: null, req: null, metadata: { enabled: null } },
	{ path: "f", req: 1 },
]);
coerceRows("optional nulls in items and patterns", {
	type: "array",
	items: {
		type: "object",
		properties: { n: { type: "integer" } },
		patternProperties: { "^p": { type: "object", properties: { q: { type: "string" } } } },
		additionalProperties: { type: "object", properties: { r: { type: "string" } } },
	},
}, [[{ n: null, p1: { q: null }, other: { r: null }, extra: null }]]);
coerceRows("optional nulls in tuples", { type: "array", prefixItems: [{ type: "object", properties: { n: { type: "integer" } } }] }, [[{ n: null }]]);

// coercion: pass 2
const valueOf = (schema) => ({ type: "object", properties: { value: schema }, required: ["value"] });
coerceRows("convert string", valueOf({ type: "string" }), [42, -0, 1e21, true, null, "x", [1], 0.1 + 0.2].map((value) => ({ value })));
coerceRows("convert number", valueOf({ type: "number" }), ["42", "", " 12 ", "0x1F", "1e1000", "Infinity", "true", "FALSE", "12n", "9007199254740993", "99999999999999999999", "abc", null, true, 3].map((value) => ({ value })));
coerceRows("convert integer", valueOf({ type: "integer" }), ["3.5", 1.5, "-0.5", "42.1", "4x", "12n", "1e3", true, null, {}].map((value) => ({ value })));
coerceRows("convert boolean", valueOf({ type: "boolean" }), ["1", "0", "TRUE", "false", "2", 1, 0, 2, null, "yes"].map((value) => ({ value })));
coerceRows("convert null", valueOf({ type: "null" }), ["null", "NULL", "undefined", "", "0", 0, false, true, "x"].map((value) => ({ value })));
coerceRows("convert const and enum", {
	type: "object",
	properties: { c: { const: 1 }, e: { enum: [1, 2, "hello"] }, b: { const: true }, s: { const: "5" }, n: { const: null } },
}, [{ c: "1", e: "2", b: "true", s: 5, n: "null" }, { c: "2", e: "world", b: "x", s: 6 }]);
coerceRows("convert arrays", {
	type: "object",
	properties: {
		a: { type: "array", items: { type: "number" } },
		s: { type: "array", items: { type: "string" } },
		t: { type: "array", prefixItems: [{ type: "number" }, { type: "boolean" }], items: false },
		u: { type: "array" },
		w: { items: { type: "integer" } },
	},
}, [{ a: "1", s: null, t: ["1", "true"], u: 5, w: ["2.5"] }, { t: "1", a: [], s: ["x"] }]);
coerceRows("convert unions", {
	type: "object",
	properties: {
		n: { anyOf: [{ type: "number" }, { type: "null" }] },
		b: { oneOf: [{ type: "boolean" }, { type: "number" }] },
		x: { anyOf: [{ type: "array", items: { type: "number" } }, { type: "string" }] },
	},
}, [{ n: "42", b: "1", x: "1" }, { n: null, b: true, x: 7 }, { n: "null", b: "x" }]);
coerceRows("convert allOf", { allOf: [{ type: "object", properties: { a: { type: "number" } } }, { type: "object", properties: { b: { type: "boolean" } } }] }, [{ a: "1", b: "true" }]);
coerceRows("convert refs", {
	type: "object",
	properties: { item: { $ref: "#/$defs/item" } },
	$defs: { item: { type: "object", properties: { id: { type: "integer" } } } },
}, [{ item: { id: "7" } }]);
coerceRows("convert members", {
	type: "object",
	properties: { a: { type: "number" } },
	patternProperties: { "^n_": { type: "number" } },
	additionalProperties: { type: "boolean" },
}, [{ a: "1", n_x: "2", other: "true" }]);
coerceRows("convert union members", {
	type: "object",
	properties: { a: { type: "integer" }, b: { type: "integer" } },
	anyOf: [{ required: ["a"] }, { required: ["b"] }],
}, [{ a: "3.5" }]);
for (const keyword of ["anyOf", "oneOf"]) {
	coerceRows(`convert ${keyword} after members`, {
		type: "object",
		[keyword]: [
			{ type: "object", properties: { a: { type: "array", items: { type: "integer" } } }, required: ["a"] },
			{ type: "object", properties: { b: { type: "boolean" } }, required: ["b"] },
		],
		properties: { c: { type: "integer" } },
		required: ["c"],
	}, [{ a: "3", c: "2" }]);
}
coerceRows("recursive tree", {
	type: "object",
	properties: { t: { $ref: "#/$defs/Tree" } },
	$defs: { Tree: { anyOf: [{ type: "string" }, { type: "array", items: { $ref: "#/$defs/Tree" } }] } },
}, [{ t: ["a", ["b"]] }, { t: 5 }, { t: [5] }, { t: {} }, { t: { a: 1 } }]);
coerceRows("recursive json", {
	type: "object",
	properties: { data: { $ref: "#/$defs/Json" } },
	required: ["data"],
	$defs: {
		Json: {
			anyOf: [
				{ type: "string" },
				{ type: "number" },
				{ type: "boolean" },
				{ type: "array", items: { $ref: "#/$defs/Json" } },
				{ type: "object", additionalProperties: { $ref: "#/$defs/Json" } },
			],
		},
	},
}, [{ data: { a: [1, null] } }]);
coerceRows("recursive self array", {
	type: "object",
	properties: { value: { anyOf: [{ type: "number" }, { type: "array", items: { $ref: "#/properties/value" } }] } },
	required: ["value"],
}, [{ value: "42" }, { value: ["1", ["2"]] }, { value: {} }]);
coerceRows("recursive root", { anyOf: [{ type: "number" }, { type: "array", items: { $ref: "#" } }] }, [{}, "42", [1, "2", ["3"]]]);
{
	const S = {
		anyOf: [
			{ type: "object", additionalProperties: { $ref: "#/$defs/S" }, required: ["a"] },
			{ type: "object", additionalProperties: { $ref: "#/$defs/S" }, required: ["b"] },
		],
	};
	let value = { x: 1 };
	for (let i = 0; i < 12; i++) value = { c: value };
	coerceRows("deep recursive union", { type: "object", properties: { s: { $ref: "#/$defs/S" } }, $defs: { S } }, [{ s: value }]);
}

// coercion: pass 3
coerceRows("pass 3 primitives", {
	type: "object",
	properties: {
		a: { type: ["integer", "null"] },
		b: { type: ["boolean", "number"] },
		c: { type: ["number", "string"] },
		d: { type: ["array", "null"], items: { type: "string" } },
		e: { type: ["null", "string"] },
		f: { type: ["string", "integer"] },
		g: { type: ["integer", "string"] },
	},
}, [{ a: "5", b: "1", c: "1", d: null, e: "", f: 3, g: "4.5" }, { a: "x", b: "true", e: 0, g: true }]);
coerceRows("pass 3 patterns and additional", {
	type: "object",
	patternProperties: { "^n_": { type: ["integer", "null"] } },
	additionalProperties: { type: ["string", "null"] },
}, [{ n_a: "5", other: 7, n_b: "" }]);
coerceRows("pass 3 tuple", { type: "array", prefixItems: [{ type: ["integer", "null"] }, { type: ["boolean", "null"] }] }, [["5", "true"], ["x"]]);
coerceRows("pass 3 union branches", { anyOf: [{ type: ["integer", "null"] }, { type: ["boolean"] }] }, ["7", "true", "x"]);
coerceRows("unchanged value", {
	type: "object",
	properties: { a: { type: "object", properties: { b: { type: "array", items: { type: "number" } } } }, c: { type: "string" } },
}, [{ a: { b: [1, 2] }, c: "x" }, { a: { b: [1, "2"] }, c: "x" }]);
coerceRows("uncoercible", valueOf({ type: "integer" }), ["abc", "4x", {}, ["1", "2"]].map((value) => ({ value })));
coerceRows("coerce root scalars", { type: "number" }, ["7", null, true, "x"]);
coerceRows("coerce enum root", { enum: [1, true, "x"] }, ["1", "true", 1]);
coerceRows("coerce with index keys", {
	type: "object",
	properties: { 10: { type: "number" }, b: { type: "number" }, 2: { type: "number" } },
}, [JSON.parse('{"b":"1","10":"2","x":"3","2":"4"}')]);

// ---------------------------------------------------------------------------------------------------------------
// resolveUri
// ---------------------------------------------------------------------------------------------------------------

const DEFAULT = "https://json-schema.invalid/root.json";
const uriCases = [
	["#/$defs/a", DEFAULT],
	["#thing", DEFAULT],
	["other.json", "https://example.com/root.json"],
	["other.json#/a", "https://example.com/dir/root.json"],
	["../x.json", "https://example.com/a/b/c.json"],
	["../../../x", "http://a/b/c"],
	["//host/x", "https://a/b"],
	["?q", "http://a/b/c?d#e"],
	["", "http://a/b/c?d#e"],
	["HTTP://EX.com:80/a/./b/../c", DEFAULT],
	["https://ex.com:443/", DEFAULT],
	["http://ex.com:8080", DEFAULT],
	["#foo", "urn:uuid:deadbeef-1234-ffff-ffff-4321feebdaed"],
	["bar", "urn:uuid:deadbeef-1234-ffff-ffff-4321feebdaed"],
	["/x", "urn:a:b"],
	["urn:uuid:deadbeef-1234-0000-0000-4321feebdaed", DEFAULT],
	["urn:uuid:deadbeef-1234-0000-0000-4321feebdaed#/$defs/bar", DEFAULT],
	["tag:example.com,2014:foo", DEFAULT],
	["x", "tag:example.com,2014:foo"],
	["#/$defs/é b", DEFAULT],
	["a b/é?x y#z w", "http://localhost:1234/draft2020-12/"],
	["http://a/%2e/b/%2E%2e/c", DEFAULT],
	["x:/a/../b", DEFAULT],
	["x://H/a/../b", DEFAULT],
	["x:a/../b", DEFAULT],
	["file:///tmp/x", DEFAULT],
	["a", "file:///tmp/x"],
	["http://localhost:1234/draft2020-12/baseUriChange/", DEFAULT],
	["folderInteger.json", "http://localhost:1234/draft2020-12/baseUriChange/"],
	["http:foo", "http://a/b/c"],
	["schema-relative-uri-defs2.json", "http://example.com/schema-relative-uri-defs1.json"],
	["https://user:pw@Example.COM/a", DEFAULT],
	["http://[::1]:80/x", DEFAULT],
	["#", DEFAULT],
	["#%zz", DEFAULT],
	["https://", DEFAULT],
	["http://a b/", DEFAULT],
	["mailto:Joe@Example.COM", DEFAULT],
	["\t https://e.com/a\nb ", DEFAULT],
	["node", "https://example.com/tree"],
];
const uris = uriCases.map(([ref, base]) => [ref, base, resolveUri(ref, base)]);

// ---------------------------------------------------------------------------------------------------------------
// sharedRegex
// ---------------------------------------------------------------------------------------------------------------

const subjects = ["", "a", "abc", "ABC", "a1", "1", "\n", "\r", "\u2028", "\t", "\u000b", "\u00a0", "\ufeff", "\u3000", " ", "😀", "é", "π", "৪", "a-b", "a]b", "a{b", "{", "}", "x{1,2}", "\\", "/", "\u0003", "ab\ncd", "aa", "aaa", "0", "p{L}", "_", "$", "^",
	// UTF-16 code units, which a pattern without the u flag matches one by one
	"-😀", "-😀😀", "\ud83d", "\ude00", "-\ud83d", "😀x", "\ude00\ud83d", "-\ud83d\ud83d", "-\u{F0000}", "\u{F0000}\u{F07FF}"];
const sources = [
	"^abc$", "abc", "^\\d+$", "^\\D$", "^\\w+$", "^\\W$", "^\\s$", "^\\S$", "^.$", "^.*$", "^[^]$", "^[]$", "[]", "^[a-z]+$",
	"^[\\s]$", "^[\\S]$", "^[\\d-z]$", "^[\\w-]+$", "^[-a]$", "^[a-]$", "^[\\]]$", "^[\\-]$", "^[\\b]$", "^\\cC$", "^\\cc$", "\\c",
	"^[\\cC]$", "^\\x41$", "^\\u0041$", "^\\u{1F600}$", "^\\uD83D\\uDE00$", "^\\0$", "^\\t$", "^\\v$", "^\\/$", "^\\a$", "^\\-$",
	"a{", "a{1", "a{1,", "a{,2}", "x{1,2}", "^a{2}$", "^a{1,}$", "^a{1,2}?$", "a]", "a}", "^\\p{L}+$", "^\\P{L}+$",
	"^\\p{Letter}+$", "^\\p{digit}+$", "^\\p{Lu}$", "^\\p{Script=Greek}+$", "^\\p{sc=Grek}$", "^\\p{gc=Lu}$",
	"^\\p{LC}$", "^\\p{Any}$", "^\\p{ASCII}+$", "^\\p{Alphabetic}$", "^\\p{Greek}$", "^\\p{Nope}$", "^[\\p{L}\\d]+$", "^[\\P{L}]$",
	"(?<n>a)\\k<n>", "(a)\\1", "\\1", "\\8", "^\\12$", "(?=a)a", "(?!a).", "(?<=a)b", "(?<!a)b", "(?:ab)+", "(?<name>a)",
	"a|b|", "^(a|b)$", "^\\bab\\b", "\\Ba", "^\\$\\^\\.\\*\\+\\?\\(\\)\\[\\]\\{\\}\\|\\\\$", "a{1001}", "^\\u00e9$",
	"(", ")", "[", "a**", "*", "a\\", "(?i)a", "[z-a]", "a{2,1}", "a{2}{3}", "+a", "a|*", "^*", "(?<1a>x)",
	"^\\uD83D$", "^[😀-😂]$", "^\\00$", "^\\01$", "^[\\01]$", "\\k", "^\\k$",
	// duplicate group names (V8 12.4 predates duplicate named groups)
	"(?<a>x)(?<a>y)", "(?<a>x)|(?<a>y)", "(?<a>x)(?<b>y)", "(?<a>x)\\-(?<a>y)", "((?<a>x))(?<a>y)", "(?<a>(?<a>x))",
	// without the u flag (\- outside a class is invalid with it), code units
	"^\\-.$", "^\\-..$", "^\\-[^a]$", "^\\-[😀]$", "^\\-\\S$", "^\\-😀+$", "^\\-.{2}$", "^\\-\\W$", "^\\-\\s$",
	"^\\-(?:😀)+$", "^\\-[😀-😂]$", "^\\-[^😀]$", "^\\-[^😀]{2}$", "^\\-?\\ud83d", "\\-?\\ud83d$", "\\-?\\ude00",
	"^\\-?[\\ud800-\\udbff]", "^\\-?[\\ud800-\\udfff]+$", "\\-?[^\\ud800-\\udfff]", "^\\-?\\ud83d\\ude00$", "^\\-?\\u{1F600}$",
	"^\\-?[\\ud83d\\ude00]$", "^\\-?[\\ud83d\\ude00]{2}$", "^\\-?\\D{3}$", "^\\-?[\\s\\S]{3}$", "^\\-?[^]{2}$", "^\\-?.*$",
	"^\\-?[a-\\ud83d]{2}$", "^\\-?(😀|x)$", "^\\-?\\😀$", "^\\-?[\\😀]{2}$",
	"^\\-?\ud83d", "^\\-?[\ud800-\udfff]{2}$", "^\\-?[\u{F0000}]$", "^\\-?[\u{F0000}-\u{F07FF}]+$", "^\\-?(?<n😀>.)$", "^\\-?(?<𝑥>.)$", "^(?<𝑥>.)$", "^(?<é>.)$",
];
const regexes = sources.map((source) => {
	let re;
	try {
		re = sharedRegex(source);
	} catch (error) {
		return { source, error: error.message };
	}
	return { source, matches: subjects.map((s) => re.test(s)) };
});

// ---------------------------------------------------------------------------------------------------------------
// TypeBox's Value.Convert tests
// ---------------------------------------------------------------------------------------------------------------

const typebox = [];
const typeboxSkipped = [];
const names = [];
let current = "";
const vitestStub = pathToFileURL(path.join(import.meta.dirname, "vitest-stub.mjs")).href;
registerHooks({
	resolve(specifier, context, next) {
		if (specifier === "vitest") return { url: vitestStub, shortCircuit: true };
		if (specifier === "typebox") return next(pathToFileURL(path.join(typeboxDir, "build/index.mjs")).href, context);
		return next(specifier, context);
	},
});
globalThis.__piSchemaCapture = {
	describe(name, fn) {
		names.push(name);
		fn();
		names.pop();
	},
	it(name, fn) {
		current = [...names, name].join(" > ");
		fn();
	},
	skip(name) {
		typeboxSkipped.push([...names, name].join(" > "));
	},
};
const original = Coercer.prototype.convertOnly;
Coercer.prototype.convertOnly = function (value) {
	const result = original.call(this, value);
	typebox.push({ test: current, schema: enc(JSON.parse(JSON.stringify(this.schema))), value: enc(value), result: enc(result) });
	return result;
};
await import(pathToFileURL(path.join(tree, "packages/schema/test/coerce-typebox.test.ts")).href);
Coercer.prototype.convertOnly = original;

process.stdout.write(`${JSON.stringify({ sha: "6536f8012", rows, uris, regexes, subjects, typebox, typeboxSkipped }, null, 1)}\n`);
