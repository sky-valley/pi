// Captures what pi sends for Anthropic models with native mid-conversation tool
// changes once tools are defined inline (upstream b271b0a52, beta
// inline-tools-2026-09-15): later tools ride in `tool_addition` blocks as
// `{type: "tool_definition", definition}`, converted like the top-level tools
// (OAuth names, eager streaming, strict), the top-level list stays the initial
// tools plus the deferred placeholder, and a removal is skipped for a name the
// same update re-adds. It is the oracle behind TestAnthropicInlineToolsMatchPi.
//
//   node capture.mjs <pi-ai package dir | src extraction> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai inline-tools-1.3.0.json
//
// A package dir (with dist/) runs the published build; a directory holding
// packages/ai/src (`git archive <sha> packages/ai`, node_modules resolving
// pi-ai's dependencies) runs that sha's src under --experimental-strip-types.
// Each row streams pi's streamSimple through options.fetch and records the
// model, the messages and key it was given, the params onPayload saw (`betas`
// included), and the request on the wire: its anthropic-beta header and body.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [dir, outFile] = process.argv.slice(2);
if (!dir || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir | src extraction> <out.json>");
	process.exit(2);
}
const dist = fs.existsSync(path.join(dir, "dist/api/anthropic-messages.js"));
const load = (file) =>
	import(pathToFileURL(dist ? path.join(dir, "dist", `${file}.js`) : path.join(dir, "packages/ai/src", `${file}.ts`)).href);
const anthropic = await load("api/anthropic-messages");
const { normalizeContext } = await load("utils/transcript");
const source = dist
	? `pi-ai ${JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version}`
	: `src ${path.basename(dir)}`;

const objectSchema = (properties = {}) => ({ type: "object", properties, required: Object.keys(properties) });
const tool = (name, extra = {}) => ({ name, description: `${name} tool`, parameters: objectSchema(), ...extra });
const nativeCompat = { supportsMidConvoSystemMessages: true, supportsMidConvoToolChanges: true };
const model = (compat = nativeCompat) => ({
	id: "claude-opus-5",
	name: "Claude Opus 5",
	api: "anthropic-messages",
	provider: "anthropic",
	baseUrl: "https://api.anthropic.com",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 100000,
	maxTokens: 1000,
	compat,
});
const usage = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 0,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};
const system = (content, timestamp, changes = {}) => ({ role: "system", content, ...changes, timestamp });
const user = (content, timestamp) => ({ role: "user", content, timestamp });

const sse = [
	{ type: "message_start", message: { id: "msg_1", model: "claude-opus-5", usage: { input_tokens: 1, output_tokens: 0 } } },
	{ type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 1 } },
	{ type: "message_stop" },
]
	.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`)
	.join("");

const baseTool = tool("base_tool");
const lateTool = tool("late_tool");
const redefinedTool = { ...baseTool, description: "changed" };
const rows = [];
async function row(name, m, messages, apiKey = "test-key") {
	let payload;
	let request;
	const fetch = async (url, init) => {
		request = {
			url: String(url),
			anthropicBeta: new Headers(init?.headers).get("anthropic-beta"),
			body: JSON.parse(String(init?.body)),
		};
		return new Response(sse, { status: 200, headers: { "content-type": "text/event-stream" } });
	};
	const onPayload = (params) => {
		payload = JSON.parse(JSON.stringify(params));
		return undefined;
	};
	const message = await anthropic.streamSimple(m, normalizeContext({ messages }), { apiKey, fetch, onPayload }).result();
	if (message.stopReason === "error" || !request) throw new Error(`${name}: ${message.errorMessage}`);
	rows.push({ name, model: m, messages, apiKey, payload, request });
}

// transcript-tool-changes.test.ts at b271b0a52: its `context`, the inline
// redefinition and the history without an initial tool.
await row("upstream/native", model(), [
	system("base prompt", 0, {
		sections: { rules: "<rules>\nold rules\n</rules>", docs: "<docs>\nread docs\n</docs>" },
		toolsAdded: [baseTool],
	}),
	user("before", 1),
	system("updated guidance", 2, {
		sections: { rules: "<rules>\nnew rules\n</rules>", docs: null },
		toolsRemoved: [{ name: "base_tool" }],
		toolsAdded: [lateTool],
	}),
]);
await row("upstream/redefinition", model(), [
	system("base prompt", 0, { toolsAdded: [baseTool] }),
	user("before", 1),
	system("", 2, { toolsRemoved: [{ name: "base_tool" }], toolsAdded: [redefinedTool] }),
]);
await row("upstream/no initial tool", model(), [
	system("base prompt", 0),
	user("before", 1),
	system("updated guidance", 2, { toolsAdded: [lateTool] }),
]);

// OAuth: Claude Code names in the removal reference and in the inline
// definition, as in the top-level list.
await row(
	"oauth names",
	model(),
	[
		system("base prompt", 0, { toolsAdded: [tool("read"), baseTool] }),
		user("before", 1),
		system("updated guidance", 2, { toolsRemoved: [{ name: "read" }], toolsAdded: [tool("bash"), tool("grep")] }),
	],
	"sk-ant-oat-test",
);

// Strict tools: an inline definition is resolved like a top-level one — a
// schema the strict subset takes is strict, one with an unsupported keyword
// falls back for "prefer".
const pathSchema = objectSchema({ path: { type: "string" } });
await row("strict definitions", model({ ...nativeCompat, supportsStrictTools: true }), [
	system("base prompt", 0, {
		toolsAdded: [tool("base_tool", { parameters: pathSchema, constrainedSampling: { type: "json_schema", strict: "require" } })],
	}),
	user("before", 1),
	system("updated guidance", 2, {
		toolsAdded: [
			tool("strict_tool", { parameters: pathSchema, constrainedSampling: { type: "json_schema", strict: "prefer" } }),
			tool("loose_tool", {
				parameters: objectSchema({ count: { type: "integer", minimum: 1 } }),
				constrainedSampling: { type: "json_schema", strict: "prefer" },
			}),
			tool("plain_tool", { parameters: pathSchema }),
		],
	}),
]);

// Without eager input streaming the definitions carry no
// eager_input_streaming, and the fine-grained streaming beta rides along.
await row("no eager streaming", model({ ...nativeCompat, supportsEagerToolInputStreaming: false }), [
	system("base prompt", 0, { toolsAdded: [baseTool] }),
	user("before", 1),
	system("updated guidance", 2, { toolsAdded: [lateTool] }),
]);

// A removal is skipped only for a name the SAME update re-adds: removed in one
// update and re-added in a later one, both blocks are sent.
await row("remove then re-add", model(), [
	system("base prompt", 0, { toolsAdded: [baseTool, lateTool] }),
	user("first", 1),
	system("drop it", 2, { toolsRemoved: [{ name: "base_tool" }] }),
	{
		role: "assistant",
		content: [{ type: "text", text: "ok" }],
		api: "anthropic-messages",
		provider: "anthropic",
		model: "claude-opus-5",
		usage,
		stopReason: "stop",
		timestamp: 3,
	},
	user("second", 4),
	system("bring it back", 5, { toolsAdded: [redefinedTool] }),
]);

// One update removing two tools, redefining one of them and adding another:
// only the other removal is sent, then the additions in order.
await row("mixed update", model(), [
	system("base prompt", 0, { toolsAdded: [tool("a"), tool("b"), baseTool] }),
	user("before", 1),
	system("", 2, {
		toolsRemoved: [{ name: "a" }, { name: "b" }],
		toolsAdded: [{ ...tool("b"), description: "b again" }, tool("c")],
	}),
]);

fs.writeFileSync(outFile, `${JSON.stringify({ source, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
