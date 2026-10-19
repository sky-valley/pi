// Captures real pi's session context reconstruction — the oracle behind
// coding/session_tree_parity_test.go. For every scenario given, it runs
// buildSessionContext followed by convertToLlm over the scenario's entries and
// writes <scenario>.golden.json beside it. The golden also records
// buildSessionProjection's provenance: each selected entry's id and how many
// model messages it contributes after convertToLlm.
//
//   node capture.mts <entry> <scenario.json>...
//   e.g. node capture.mts ~/.cache/pi-npm/0.87.0/node_modules/@earendil-works/pi-coding-agent/dist/index.js *_*.json
//   (skip the .golden.json files)
//
// <entry> is a module exporting buildSessionContext, buildSessionProjection and
// convertToLlm: the npm
// build's dist/index.js, or packages/coding-agent/src/index.ts in a src
// extraction (see ../sessionprompt/capture.mts for its layout; run it with
// --experimental-strip-types). The BUILD wins over a src capture.
//
// Every golden was captured from the npm build of pi-coding-agent 0.87.0, the
// first build carrying context_edit entries and the newest-compaction-only
// checkpoint (upstream 466db0fec), except 22_compaction_retained_tail, captured
// from 1.3.0; 13 and 19 re-captured under 1.3.0 as controls came out
// byte-identical.
//
// Projection: role and the text of the content (a string as is; blocks joined
// with no separator, an image as <image>, any other block as <other>), plus
// stringContent when the content is a plain string rather than blocks. A system
// message also carries the whole message as pi returns it, so its key order,
// sections and tool fields are pinned too.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [entry, ...scenarios] = process.argv.slice(2);
if (!entry || scenarios.length === 0) {
	console.error("usage: node capture.mts <entry> <scenario.json>...");
	process.exit(2);
}
const { buildSessionContext, buildSessionProjection, convertToLlm } = await import(pathToFileURL(path.resolve(entry)).href);

function text(content: unknown): string {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return content
		.map((block) => (block.type === "text" ? block.text : block.type === "image" ? "<image>" : "<other>"))
		.join("");
}

for (const file of scenarios) {
	const scenario = JSON.parse(fs.readFileSync(file, "utf8"));
	const context = buildSessionContext(scenario.entries, scenario.leafId);
	const projection = buildSessionProjection(scenario.entries, scenario.leafId);
	const out = {
		thinkingLevel: context.thinkingLevel,
		model: context.model,
		messages: convertToLlm(context.messages).map((message: any) => ({
			role: message.role,
			text: text(message.content),
			...(typeof message.content === "string" ? { stringContent: true } : {}),
			...(message.role === "system" ? { message } : {}),
		})),
		entries: projection.entries.map((entry: any) => ({
			id: entry.sourceEntry.id,
			messages: convertToLlm(entry.messages).length,
		})),
	};
	fs.writeFileSync(file.replace(/\.json$/, ".golden.json"), JSON.stringify(out, null, 2));
}
