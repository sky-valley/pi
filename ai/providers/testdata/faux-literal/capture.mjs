// Captures the key order of pi's fauxAssistantMessage literals — the oracle
// behind TestFauxAssistantMessageLiteralKeyOrderMatchesPi in
// ai/providers/faux_test.go.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs <npm>/1.3.0/node_modules/@earendil-works/pi-ai faux-literal-1.3.0.json
//
// fauxAssistantMessage writes its deferred, errorMessage and responseId
// options into the object literal ahead of timestamp; the faux provider
// spreads the scripted message into its result, so the order survives the
// stream, which appends durationMs. Each row records the options, the keys of
// the literal, and the keys of the result a faux stream returns for it.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [dir, outFile] = process.argv.slice(2);
if (!dir || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const version = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version;
const ai = await import(pathToFileURL(path.join(dir, "dist/index.js")).href);
const compat = await import(pathToFileURL(path.join(dir, "dist/compat.js")).href);
const keys = (value) => Object.keys(JSON.parse(JSON.stringify(value)));
const rows = [];
for (const [name, options] of [
	["no options", {}],
	["error message", { stopReason: "error", errorMessage: "boom" }],
	["response id", { responseId: "resp_1" }],
	["error message and response id", { stopReason: "error", errorMessage: "boom", responseId: "resp_1", timestamp: 5 }],
]) {
	const message = ai.fauxAssistantMessage("hi", options);
	const registration = compat.registerFauxProvider();
	registration.setResponses([message]);
	const stream = compat.streamSimple(registration.getModel(), { messages: [{ role: "user", content: "hi", timestamp: 1 }] });
	for await (const _ of stream);
	const result = await stream.result();
	registration.unregister();
	rows.push({ name, options, literal: keys(message), result: keys(result) });
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAiVersion: version, rows }, null, "\t")}\n`);
for (const row of rows) console.error(row.name, "|", row.literal.join(","), "|", row.result.join(","));
