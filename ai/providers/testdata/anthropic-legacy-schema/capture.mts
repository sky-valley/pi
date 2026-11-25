// Captures the input_schema pi's anthropic-messages adapter sends for a tool,
// per root schema shape — the oracle behind
// TestAnthropicToolInputSchemaMatchesPi. Since upstream 4245b07b4 (#12212) a
// non-strict tool keeps a declared root $schema beside type, properties,
// required, additionalProperties, $defs and definitions, so nested keywords
// (Draft 7 tuple items) are read in the schema's own dialect.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> legacy-schema-4245b07b4.json 4245b07b4
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai`)
// and a node_modules resolving its dependencies (the npm 1.6.0 build's, with
// the generated src/providers/data catalogs copied from its dist). 4245b07b4
// is after the 1.6.0 release, so this is a src capture: re-verify it against
// the first build that ships it (the BUILD wins).
//
// Each row records the input_schema as sent (JSON.stringify keeps pi's key
// order) and whether the tool went strict. The first row is upstream's
// anthropic-strict-tool-schema.test.ts "preserves the declared dialect and
// tuple syntax in non-strict schemas"; the rest probe values JavaScript's
// `=== undefined` test keeps (null, a non-string) and the strict path.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = (file: string) => import(pathToFileURL(path.join(extraction, "packages/ai/src", file)).href);
const { stream } = await src("api/anthropic-messages.ts");
const { normalizeContext } = await src("utils/transcript.ts");

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
const draft7 = "http://json-schema.org/draft-07/schema#";
const pair = { type: "array", items: [{ type: "string" }, { type: "string" }], minItems: 2, maxItems: 2 };
const rows: Array<{ name: string; parameters: unknown; strict?: string }> = [
	{
		name: "draft 7 tuple",
		parameters: { $schema: draft7, type: "object", properties: { pair }, required: ["pair"] },
	},
	{
		name: "$schema last, with every kept keyword",
		parameters: {
			definitions: { D: {} },
			$defs: { A: {} },
			additionalProperties: false,
			required: ["a"],
			properties: { a: { type: "string" } },
			type: "object",
			title: "T",
			$schema: "https://json-schema.org/draft/2020-12/schema",
		},
	},
	{ name: "$schema null", parameters: { $schema: null, type: "object", properties: {} } },
	{ name: "$schema number", parameters: { $schema: 7, type: "object", properties: {} } },
	{ name: "$schema empty", parameters: { $schema: "", type: "object", properties: {} } },
	{ name: "$schema absent", parameters: { type: "object", properties: { a: { type: "string" } } } },
	{ name: "$SCHEMA cased", parameters: { $SCHEMA: draft7, type: "object", properties: {} } },
	{
		name: "strict prefer with $schema",
		parameters: { $schema: draft7, type: "object", properties: { a: { type: "string" } }, required: ["a"] },
		strict: "prefer",
	},
];

const out = [];
for (const { name, parameters, strict } of rows) {
	let payload: { tools: Array<{ strict?: boolean; input_schema: unknown }> } | undefined;
	const s = stream(
		model,
		normalizeContext({
			messages: [{ role: "user", content: "hi", timestamp: 1 }],
			tools: [
				{
					name: "lookup",
					description: "d",
					parameters,
					...(strict ? { constrainedSampling: { type: "json_schema", strict } } : {}),
				},
			],
		}),
		{
			apiKey: "k",
			cacheRetention: "none",
			onPayload: (p: typeof payload) => {
				payload = p;
				throw new Error("captured");
			},
		},
	);
	const result = await s.result();
	if (!payload) throw new Error(`${name}: no payload: ${result.errorMessage}`);
	const tool = payload.tools[0];
	out.push({
		name,
		...(strict ? { strict } : {}),
		parameters,
		sentStrict: tool.strict === true,
		inputSchema: JSON.stringify(tool.input_schema),
	});
}
fs.writeFileSync(outFile, `${JSON.stringify({ source: `pi src ${sha}`, rows: out }, null, "\t")}\n`);
console.error(`${out.length} rows from pi src ${sha}`);
