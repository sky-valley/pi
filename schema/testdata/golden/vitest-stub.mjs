// Stand-ins for vitest's describe/it/expect, for capture.mjs to run
// upstream's test/coerce-typebox.test.ts outside vitest. describe and it run
// their bodies at once through the hooks capture.mjs installs; expect reports
// a failed assertion on stderr (upstream's own tests, so none is expected).
import { isDeepStrictEqual } from "node:util";

const hooks = () => globalThis.__piSchemaCapture;

export function describe(name, fn) {
	hooks().describe(name, fn);
}

export function it(name, fn) {
	hooks().it(name, fn);
}
it.skip = (name) => hooks().skip(name);

// toEqual ignores properties whose value is undefined, as vitest's does.
function normalize(v) {
	if (Array.isArray(v)) return v.map(normalize);
	if (v !== null && typeof v === "object" && Object.getPrototypeOf(v) === Object.prototype) {
		return Object.fromEntries(Object.entries(v).filter(([, x]) => x !== undefined).map(([k, x]) => [k, normalize(x)]));
	}
	return v;
}

export function expect(actual) {
	const fail = (what, expected) =>
		console.error(`assertion failed (${what}): ${String(actual)} vs ${String(expected)}`);
	return {
		toEqual(expected) {
			if (!isDeepStrictEqual(normalize(actual), normalize(expected))) fail("toEqual", expected);
		},
		toBe(expected) {
			if (!Object.is(actual, expected)) fail("toBe", expected);
		},
	};
}
