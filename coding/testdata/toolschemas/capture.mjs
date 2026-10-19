// Captures the parameter and output schemas of pi's built-in tools as the
// model and programmatic callers receive them — the oracle behind
// TestBuiltinToolSchemasMatchPi.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs <npm>/1.3.0 toolschemas-1.3.0.json
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
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-coding-agent");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const { createToolDefinition } = await import(pathToFileURL(path.join(pkg, "dist/core/tools/index.js")).href);

const names = ["read", "bash", "edit", "write", "grep", "find", "ls", "powershell"];
const rows = names.map((name) => {
	const def = createToolDefinition(name, "/tmp");
	const row = { name, parameters: JSON.parse(JSON.stringify(def.parameters)) };
	if (def.outputSchema !== undefined) row.outputSchema = JSON.parse(JSON.stringify(def.outputSchema));
	return row;
});
fs.writeFileSync(outFile, `${JSON.stringify({ piCodingAgentVersion: version, rows }, null, "\t")}\n`);
console.error(`${rows.length} tools from pi-coding-agent ${version}`);
