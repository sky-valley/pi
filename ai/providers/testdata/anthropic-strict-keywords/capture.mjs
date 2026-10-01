// Captures whether real pi sends an Anthropic tool strict, per schema keyword —
// the oracle behind TestAnthropicStrictUnsupportedKeywordsMatchPi.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/0.99.2 strict-keywords-0.99.2.json
//
// Each schema is sent as a json_schema-constrained tool, once with strict
// "prefer" and once with "require", to the published build's anthropic-messages
// stream on a model with compat.supportsStrictTools. onPayload stops the request
// before any network I/O; the row records the tool's strict flag, or the error
// pi fails the request with when a "require" tool cannot go strict.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const load = (file) => import(pathToFileURL(path.join(pkg, "dist", file)).href);
const { stream } = await load("api/anthropic-messages.js");
const { normalizeContext } = await load("utils/transcript.js");

const model = {
	id: "claude-x",
	name: "x",
	api: "anthropic-messages",
	provider: "test-anthropic",
	baseUrl: "http://127.0.0.1:9",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 200000,
	maxTokens: 100,
	compat: { supportsStrictTools: true },
};
const obj = (properties, extra = {}) => ({ type: "object", properties, ...extra });
const strArray = (extra) => ({ type: "array", items: { type: "string" }, ...extra });
const schemas = {
	// The three shapes from upstream's anthropic-strict-tool-schema.test.ts.
	"minimum and maximum": obj({ timeoutMs: { type: "integer", minimum: 1, maximum: 300000 } }),
	"nested minItems 2": obj({ options: obj({ tags: strArray({ minItems: 2 }) }) }),
	"format regex": obj({ expression: { type: "string", format: "regex" } }),
	"supported keywords": obj({
		code: { type: "string", minLength: 1, maxLength: 1000, pattern: "^[a-z]+$" },
		url: { type: "string", format: "uri" },
		tags: strArray({ minItems: 1 }),
	}),
	"minItems 0": obj({ tags: strArray({ minItems: 0 }) }),
	"maxItems": obj({ tags: strArray({ maxItems: 3 }) }),
	"uniqueItems": obj({ tags: strArray({ uniqueItems: true }) }),
	"multipleOf fraction": obj({ n: { type: "number", multipleOf: 0.5 } }),
	"exclusiveMaximum": obj({ n: { type: "number", exclusiveMaximum: 10 } }),
	"root maxProperties": obj({ n: { type: "number" } }, { maxProperties: 3 }),
	"anyOf variant exclusiveMinimum": obj({ n: { anyOf: [{ type: "number", exclusiveMinimum: 0 }, { type: "string" }] } }),
	"items minContains": obj({ tags: strArray({ minContains: 1 }) }),
	"every allowed format": obj(
		Object.fromEntries(
			["date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid"].map((f) => [
				f,
				{ type: "string", format: f },
			]),
		),
	),
	"format email-like but not allowed": obj({ e: { type: "string", format: "idn-email" } }),
	// The first offending keyword in SOURCE order names the error.
	"maximum before minimum": obj({ n: { type: "integer", maximum: 10, minimum: 1 } }),
	"uniqueItems before maxItems": obj({ tags: { type: "array", uniqueItems: true, items: { type: "string" }, maxItems: 3 } }),
	"maxContains before minimum": obj({ n: { maxContains: 2, type: "integer", minimum: 1 } }),
	// Values a typed field cannot hold still count.
	"minItems fraction": obj({ tags: strArray({ minItems: 1.5 }) }),
	"minItems string": obj({ tags: strArray({ minItems: "1" }) }),
	"minItems null": obj({ tags: strArray({ minItems: null }) }),
	"maxItems null": obj({ tags: strArray({ maxItems: null }) }),
	"format empty": obj({ e: { type: "string", format: "" } }),
	"format number": obj({ e: { type: "string", format: 5 } }),
	"format null": obj({ e: { type: "string", format: null } }),
	"minimum string": obj({ n: { type: "number", minimum: "5" } }),
	"minimum null": obj({ n: { type: "number", minimum: null } }),
	"exclusiveMinimum boolean": obj({ n: { type: "number", minimum: 0, exclusiveMinimum: true } }),
};

const rows = [];
for (const strict of ["prefer", "require"]) {
	for (const [name, parameters] of Object.entries(schemas)) {
		let payload;
		const s = stream(
			model,
			normalizeContext({
				messages: [{ role: "user", content: "hi", timestamp: 1 }],
				tools: [{ name: "lookup", description: "d", parameters, constrainedSampling: { type: "json_schema", strict } }],
			}),
			{
				apiKey: "k",
				cacheRetention: "none",
				onPayload: (p) => {
					payload = p;
					throw new Error("captured");
				},
			},
		);
		const result = await s.result();
		rows.push(
			payload
				? { name, strict, parameters, sentStrict: payload.tools[0].strict === true }
				: { name, strict, parameters, error: result.errorMessage },
		);
	}
}
fs.writeFileSync(outFile, `${JSON.stringify({ piVersion: version, rows }, null, "\t")}\n`);
console.error(`${rows.length} rows from pi-ai ${version}`);
