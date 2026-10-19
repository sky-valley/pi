// Captures the error texts pi's validateToolCall / validateToolArguments
// (utils/validation.ts) throw — text the agent loop hands the model as the
// tool result — the oracle behind TestValidationErrorTextMatchesPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai validation-errors-1.3.0.json
//
// The package is the published npm build. Rows pin the parts of the text
// the port builds itself: the tool name as written between quotes (no
// escaping), and the received arguments as JSON.stringify(arguments, null, 2)
// writes them — <, > and & as themselves, U+2028 unescaped, numbers the
// JavaScript way, -0 as 0, and keys in the object's own order. Each row's
// arguments are recorded as JSON text, so the test decodes them in that
// order.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const { validateToolArguments, validateToolCall } = await import(pathToFileURL(path.join(pkg, "dist/utils/validation.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const parameters = { type: "object", properties: { value: { type: "string" } }, required: ["value"] };
// Arguments are written as JSON text: what a model's arguments decode to.
const rows = [
	{ name: "echo", args: '{"note":"<a & b>"}' },
	{ name: 'we"ird\\name', args: '{"note":1}' },
	{ name: "echo", args: `{"n":1e21,"z":-0,"f":0.1,"big":123456789012345680000,"tiny":1e-7,"line":"a${String.fromCharCode(0x2028)}b"}` },
	{ name: "echo", args: '{"nested":{"b":1,"2":[1,{"y":"<>"}],"a":[],"o":{}}}' },
	{ name: "mi\"ssé", args: "{}", tools: ["echo"] },
].map((row) => {
	const toolCall = { type: "toolCall", id: "1", name: row.name, arguments: JSON.parse(row.args) };
	try {
		if (row.tools) validateToolCall(row.tools.map((name) => ({ name, description: "d", parameters })), toolCall);
		else validateToolArguments({ name: row.name, description: "d", parameters }, toolCall);
	} catch (error) {
		return { ...row, error: error.message };
	}
	throw new Error(`${row.name} ${row.args} validated`);
});
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, parameters, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
