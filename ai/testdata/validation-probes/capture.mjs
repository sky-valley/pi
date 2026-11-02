// Captures every validateToolArguments call of upstream's
// packages/ai/test/validation-probes.test.ts (the schema switch's reviewer
// probes: TypeBox-built and MCP-shaped plain JSON tool schemas) — the oracle
// behind TestValidationProbesMatchPi.
//
//   node capture.mjs <upstream tree> <typebox package dir> <out.json> <sha>
//
// <upstream tree> is `git archive <sha> packages/ai/src packages/ai/test
// packages/schema | tar -x -C <tree>`; nothing is written into it.
// <typebox package dir> is TypeBox 1.3.27, upstream's dev dependency (`npm
// install typebox@1.3.27` in a scratch directory; pass its
// node_modules/typebox). Node v22 strips the types.
//
// The test file runs as written. A resolve hook hands it stand-ins for vitest
// (it runs each body at once; a failed expect is reported on stderr, and none
// is expected) and, for its "../src/utils/validation.ts" import, a wrapper
// that calls the real function and records each call: the parameters as
// JSON.stringify writes them at the call, the arguments, and the returned
// value or thrown message. TypeBox's own runtime parts (a Refine callback, a
// Codec, symbol-keyed kinds) do not survive JSON, so a row whose parameters
// object was seen before with different JSON (a schema mutated after its
// validator was cached) is marked "mutated", and the Go test reads each row's
// JSON as an ai.Schema like any tool schema decoded from JSON.
import fs from "node:fs";
import { register } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [tree, typebox, outFile, sha] = process.argv.slice(2);
if (!tree || !typebox || !outFile || !sha) {
	console.error("usage: node capture.mjs <upstream tree> <typebox package dir> <out.json> <sha>");
	process.exit(2);
}
const root = path.resolve(tree);
const url = (p) => pathToFileURL(p).href;
const realValidation = url(path.join(root, "packages/ai/src/utils/validation.ts"));
const testFile = url(path.join(root, "packages/ai/test/validation-probes.test.ts"));

const vitest = `
export function describe(name, fn) { fn(); }
export function it(name, fn) { fn(); }
export function expect(actual) {
	return { toBe(expected) { if (!Object.is(actual, expected)) console.error("assertion failed: " + actual + " vs " + expected); } };
}`;
const wrapper = `
import * as real from ${JSON.stringify(realValidation)};
const seen = new WeakMap();
export function validateToolArguments(tool, toolCall) {
	const parameters = JSON.stringify(tool.parameters);
	const before = seen.get(tool.parameters);
	if (before === undefined) seen.set(tool.parameters, parameters);
	const row = { name: tool.name, parameters, args: JSON.stringify(toolCall.arguments) };
	if (before !== undefined && before !== parameters) row.mutated = true;
	globalThis.__probeRows.push(row);
	try {
		const value = real.validateToolArguments(tool, toolCall);
		row.value = JSON.stringify(value, (_k, v) => (typeof v === "bigint" ? v + "n" : v));
		return value;
	} catch (error) {
		row.error = error.message;
		throw error;
	}
}
export const validateToolCall = real.validateToolCall;`;
const schemaSrc = path.join(root, "packages/schema/src");
const map = {
	vitest: `data:text/javascript,${encodeURIComponent(vitest)}`,
	typebox: url(path.join(path.resolve(typebox), "build/index.mjs")),
	"@earendil-works/pi-schema": url(path.join(schemaSrc, "index.ts")),
	"@earendil-works/pi-schema/formats": url(path.join(schemaSrc, "formats/index.ts")),
};
const hook = `
const map = ${JSON.stringify(map)};
const wrapper = ${JSON.stringify(`data:text/javascript,${encodeURIComponent(wrapper)}`)};
export async function resolve(specifier, context, next) {
	if (specifier === "../src/utils/validation.ts" && context.parentURL === ${JSON.stringify(testFile)}) {
		return { url: wrapper, shortCircuit: true };
	}
	if (specifier in map) return { url: map[specifier], shortCircuit: true };
	return next(specifier, context);
}`;
register(`data:text/javascript,${encodeURIComponent(hook)}`);

globalThis.__probeRows = [];
await import(testFile);
const rows = globalThis.__probeRows;
fs.writeFileSync(outFile, `${JSON.stringify({ sha, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
