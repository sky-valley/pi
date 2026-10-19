// Captures what pi's pi-messages adapter keeps of a toolcall_end's toolCall
// (Object.assign(block, event.toolCall)): every key, argumentsError
// (upstream 87f9a3641) included, so a gateway that reports malformed final
// arguments fails tool validation on the client as it would locally. It is the
// oracle behind TestPiMessagesToolCallKeepsArgumentsErrorLikePi.
//
//   node capture-pi-messages-arguments-error.mjs <pi-ai package dir> <out.json>
//   e.g. node capture-pi-messages-arguments-error.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai pi-messages-arguments-error-1.3.0.json
//
// Each row streams one SSE body through the published build's pi-messages
// stream (served through options.fetch) and records the finished tool call and
// what validateToolArguments makes of it against {path: string}.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture-pi-messages-arguments-error.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
const pi = await import(pathToFileURL(path.join(pkg, "dist/api/pi-messages.js")).href);
const { validateToolArguments } = await import(pathToFileURL(path.join(pkg, "dist/utils/validation.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const model = {
	id: "auto",
	name: "a",
	api: "pi-messages",
	provider: "radius",
	baseUrl: "http://127.0.0.1:9/v1",
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 1000,
	maxTokens: 100,
};
const tool = { name: "read", description: "read", parameters: { type: "object", properties: { path: { type: "string" } } } };
const malformed = 'Unexpected end of JSON input\n\nReceived arguments:\n{"path":';

const rows = [];
for (const [name, extra] of [
	["string argumentsError", { argumentsError: malformed }],
	["no argumentsError", {}],
	["null argumentsError", { argumentsError: null }],
	["empty argumentsError", { argumentsError: "" }],
	["number argumentsError", { argumentsError: 5 }],
]) {
	const frames = [
		{ type: "start" },
		{ type: "toolcall_start", contentIndex: 0, id: "call_1", toolName: "read" },
		{ type: "toolcall_delta", contentIndex: 0, delta: '{"path":' },
		{ type: "toolcall_end", contentIndex: 0, toolCall: { type: "toolCall", id: "call_1", name: "read", arguments: {}, ...extra } },
		{ type: "done", reason: "toolUse", usage },
	];
	const body = frames.map((f) => `data: ${JSON.stringify(f)}\n\n`).join("");
	const fetch = async () => new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
	const message = await pi.stream(model, { messages: [{ role: "user", content: "hi", timestamp: 0 }] }, { apiKey: "k", fetch }).result();
	const toolCall = message.content[0];
	let validation = null;
	try {
		validateToolArguments(tool, toolCall);
	} catch (error) {
		validation = error.message;
	}
	rows.push({ name, body, stopReason: message.stopReason, toolCall, validation });
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
