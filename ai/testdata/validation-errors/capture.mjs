// Captures what pi's validateToolCall / validateToolArguments
// (utils/validation.ts) return and throw — the error text is the tool result
// the agent loop hands the model — the oracle behind
// TestValidationErrorTextMatchesPi and TestValidationCoercionMatchesPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   node capture.mjs --src <upstream tree> <out.json> <sha>
//
// The first form loads the published build (dist/utils/validation.js). The
// second loads packages/ai/src/utils/validation.ts from an extracted upstream
// tree (`git archive <sha> packages/ai/src packages/schema | tar -x -C
// <tree>`), mapping the bare @earendil-works/pi-schema imports onto
// packages/schema/src with a resolve hook, so nothing is written into the
// tree. Node v22 strips the types.
//
// validation-errors-6536f8012.json is a src capture: upstream 6536f8012
// moved validation onto pi-schema (compile(schema, {formats, coerce: true,
// unresolvedRefs: "reject"}).parse) after 1.4.0 shipped, which changed every
// message ("must have required properties value" became "missing") and the
// coercion rules. It replaces validation-errors-1.3.0.json, the 1.3.0
// build's capture. Re-verify against the first build that ships 6536f8012
// (the BUILD wins).
//
// Rows pin the parts of the text the port builds itself: the tool name as
// written between quotes (no escaping), each error as "  - <path>: <message>"
// with pi-schema's JSON Pointer turned into a dotted path, and the received
// arguments as JSON.stringify(arguments, null, 2) writes them — <, > and & as
// themselves, U+2028 unescaped, numbers the JavaScript way, -0 as 0, and keys
// in the object's own order. Arguments and schemas are recorded as JSON text,
// so the test decodes them in that order. A row that validates records the
// value validateToolArguments returns (the coerced arguments).
import fs from "node:fs";
import { register } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const argv = process.argv.slice(2);
let validationURL;
let label;
let outFile;
if (argv[0] === "--src") {
	const [, tree, out, sha] = argv;
	if (!tree || !out || !sha) usage();
	const schemaSrc = path.resolve(tree, "packages/schema/src");
	const hook = `
		const map = ${JSON.stringify({
			"@earendil-works/pi-schema": pathToFileURL(path.join(schemaSrc, "index.ts")).href,
			"@earendil-works/pi-schema/formats": pathToFileURL(path.join(schemaSrc, "formats/index.ts")).href,
		})};
		export async function resolve(specifier, context, next) {
			if (specifier in map) return { url: map[specifier], shortCircuit: true };
			return next(specifier, context);
		}`;
	register(`data:text/javascript,${encodeURIComponent(hook)}`);
	validationURL = pathToFileURL(path.resolve(tree, "packages/ai/src/utils/validation.ts")).href;
	label = sha;
	outFile = out;
} else {
	const [pkg, out] = argv;
	if (!pkg || !out) usage();
	validationURL = pathToFileURL(path.join(pkg, "dist/utils/validation.js")).href;
	label = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
	outFile = out;
}
const { validateToolArguments, validateToolCall } = await import(validationURL);

function usage() {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>\n       node capture.mjs --src <upstream tree> <out.json> <sha>");
	process.exit(2);
}

// The default schema; a row may bring its own (as JSON text).
const parameters = '{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}';
const rich = JSON.stringify({
	type: "object",
	properties: {
		path: { type: "string", minLength: 1 },
		count: { type: "integer", minimum: 0, maximum: 10 },
		ratio: { type: "number", exclusiveMinimum: 0 },
		mode: { type: "string", enum: ["fast", "slow"] },
		flag: { type: "boolean" },
		tags: { type: "array", items: { type: "string" }, maxItems: 2 },
		email: { type: "string", format: "email" },
		when: { type: "string", format: "date-time" },
		note: { type: "string" },
		edits: {
			type: "array",
			items: {
				type: "object",
				properties: { oldText: { type: "string" }, newText: { type: "string" } },
				required: ["oldText", "newText"],
				additionalProperties: false,
			},
		},
		"a/b~c": { type: "number" },
	},
	required: ["path"],
	additionalProperties: false,
});
const union = JSON.stringify({
	type: "object",
	properties: {
		target: { anyOf: [{ type: "number" }, { type: "boolean" }] },
		pick: { oneOf: [{ type: "string", const: "a" }, { type: "string", const: "b" }] },
		maybe: { type: ["string", "null"] },
	},
	required: ["target"],
});
const dangling = JSON.stringify({
	type: "object",
	properties: { ok: { type: "string" }, bad: { $ref: "#/$defs/missing" } },
});

// Arguments are written as JSON text: what a model's arguments decode to.
const rows = [
	{ name: "echo", args: '{"note":"<a & b>"}' },
	{ name: 'we"ird\\name', args: '{"note":1}' },
	{ name: "echo", args: `{"n":1e21,"z":-0,"f":0.1,"big":123456789012345680000,"tiny":1e-7,"line":"a${String.fromCharCode(0x2028)}b"}` },
	{ name: "echo", args: '{"nested":{"b":1,"2":[1,{"y":"<>"}],"a":[],"o":{}}}' },
	{ name: "mi\"ssé", args: "{}", tools: ["echo"] },
	{ name: "echo", args: '{"value":"ok"}' },
	{ name: "echo", args: '{"value":42}' },
	{ name: "echo", args: '{"value":true}' },
	{ name: "echo", args: '{"value":null}' },
	{ name: "echo", args: '{"value":["x"]}' },
	{ name: "echo", args: '{"value":{"x":1}}' },
	{ name: "edit", parameters: rich, args: '{"path":"a.txt","count":"3","ratio":"0.5","flag":"true","tags":"one","note":null}' },
	{ name: "edit", parameters: rich, args: '{"path":"","count":11,"ratio":0,"mode":"medium","flag":"yes","tags":["a","b","c"],"extra":1}' },
	{ name: "edit", parameters: rich, args: '{"count":1.5,"email":"not-an-email","when":"yesterday"}' },
	{ name: "edit", parameters: rich, args: '{"path":"p","email":"me@example.com","when":"2026-11-02T10:00:00Z"}' },
	{ name: "edit", parameters: rich, args: '{"path":"p","edits":[{"oldText":"a","newText":"b"},{"oldText":"c"},{"oldText":1,"newText":"d","x":true}]}' },
	{ name: "edit", parameters: rich, args: '{"path":"p","edits":{"oldText":"a","newText":"b"}}' },
	{ name: "edit", parameters: rich, args: '{"path":"p","a/b~c":"x"}' },
	{ name: "edit", parameters: rich, args: '{"path":"p","count":"-1","ratio":"abc"}' },
	{ name: "edit", parameters: rich, args: '{"path":7}' },
	{ name: "pick", parameters: union, args: '{"target":"12","pick":"a","maybe":null}' },
	{ name: "pick", parameters: union, args: '{"target":"false"}' },
	{ name: "pick", parameters: union, args: '{"target":"x","pick":"c","maybe":3}' },
	{ name: "pick", parameters: union, args: "{}" },
	{ name: "ref", parameters: dangling, args: '{"ok":"x"}' },
	{ name: "ref", parameters: dangling, args: '{"ok":"x","bad":1}' },
].map((row) => {
	const schema = JSON.parse(row.parameters ?? parameters);
	const toolCall = { type: "toolCall", id: "1", name: row.name, arguments: JSON.parse(row.args) };
	const out = { ...row };
	try {
		const value = row.tools
			? validateToolCall(row.tools.map((name) => ({ name, description: "d", parameters: schema })), toolCall)
			: validateToolArguments({ name: row.name, description: "d", parameters: schema }, toolCall);
		out.value = JSON.stringify(value);
	} catch (error) {
		out.error = error.message;
	}
	return out;
});
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: label, parameters: JSON.parse(parameters), rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
