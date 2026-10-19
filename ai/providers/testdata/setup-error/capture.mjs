// Captures how pi's compat stream entry points end a request that has no
// credentials — the oracle behind TestSetupErrorMatchesPi in
// ai/providers/setup_error_test.go.
//
//   env -i PATH="$PATH" HOME="$(mktemp -d)" node capture.mjs <pi-ai package dir> <out.json>
//   e.g. ... capture.mjs <npm>/1.3.0/node_modules/@earendil-works/pi-ai setup-error-1.3.0.json
//
// Run it with an empty environment, so no provider finds a key. For each
// provider whose api the port implements, it calls the compat
// streamSimple and stream with no options key and records the events the
// stream delivered and its result: the message's keys in order (pi's
// JSON.stringify order), its stopReason and errorMessage. streamSimple's
// eager auth assertion throws before a stream exists, so lazyApi's
// createSetupErrorMessage builds that result (errorMessage before timestamp);
// stream fails inside the adapter's own output object.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [dir, outFile] = process.argv.slice(2);
if (!dir || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const version = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version;
const compat = await import(pathToFileURL(path.join(dir, "dist/compat.js")).href);
const { MODELS } = await import(pathToFileURL(path.join(dir, "dist/models.generated.js")).href);
const context = { messages: [{ role: "user", content: "hi", timestamp: 1 }] };
const rows = [];
for (const [provider, id] of [
	["anthropic", "claude-opus-4-8"],
	["google", "gemini-2.5-pro"],
	["openai", "gpt-5"],
	["deepseek", "deepseek-v4-pro"],
]) {
	const model = MODELS[provider][id];
	if (!model) throw new Error(`no ${provider}/${id} in the catalog`);
	for (const entry of ["streamSimple", "stream"]) {
		const stream = compat.getApiProvider(model.api)[entry](model, context, entry === "streamSimple" ? { reasoning: "high" } : {});
		const events = [];
		for await (const event of stream) events.push(event.type);
		const result = await stream.result();
		rows.push({
			name: `${provider}/${entry}`,
			entry,
			model,
			events,
			keys: Object.keys(JSON.parse(JSON.stringify(result))),
			stopReason: result.stopReason,
			errorMessage: result.errorMessage,
		});
	}
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAiVersion: version, rows }, null, "\t")}\n`);
for (const row of rows) console.error(row.name, row.events.join(","), row.keys.join(","));
