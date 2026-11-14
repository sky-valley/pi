// Captures the parameter and output schemas of pi's built-in tools as the
// model and programmatic callers receive them — the oracle behind
// TestBuiltinToolSchemasMatchPi.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs <npm>/1.3.0 toolschemas-1.3.0.json
//   node --experimental-strip-types capture.mjs <src extraction named by sha> <out.json>
//   e.g. ... capture.mjs <dir>/282b01c80 toolschemas-282b01c80.json
//
// toolschemas-282b01c80.json is a src capture over the 1.5.2 build's
// node_modules: 282b01c80 rewrites find's `pattern` and grep's `glob`
// descriptions. Control: src at 282b01c80^1 writes the previous capture's
// (toolschemas-6bf2ab9bb.json) rows byte for byte. Re-verify against the first
// build that ships 282b01c80.
//
// Each row is createToolDefinition(name, cwd) from the published build, with
// its parameters, and its outputSchema when it declares one, serialized by
// JSON.stringify (TypeBox's symbol-keyed metadata does not serialize, exactly
// as on the wire).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
// A src extraction (`git archive <sha> packages/coding-agent`, with the npm
// build's node_modules beside it) is read from its .ts files under
// `node --experimental-strip-types`, labelled "src <dir name>".
const src = fs.existsSync(path.join(npmDir, "packages/coding-agent/src/core/tools/index.ts"));
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-coding-agent");
const version = src
	? `src ${path.basename(npmDir)}`
	: JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const tools = src ? path.join(npmDir, "packages/coding-agent/src/core/tools/index.ts") : path.join(pkg, "dist/core/tools/index.js");
const { createToolDefinition } = await import(pathToFileURL(tools).href);

const names = ["read", "bash", "edit", "write", "grep", "find", "ls", "powershell"];
const rows = names.map((name) => {
	const def = createToolDefinition(name, "/tmp");
	const row = { name, parameters: JSON.parse(JSON.stringify(def.parameters)) };
	if (def.outputSchema !== undefined) row.outputSchema = JSON.parse(JSON.stringify(def.outputSchema));
	return row;
});
fs.writeFileSync(outFile, `${JSON.stringify({ piCodingAgentVersion: version, rows }, null, "\t")}\n`);
console.error(`${rows.length} tools from pi-coding-agent ${version}`);
