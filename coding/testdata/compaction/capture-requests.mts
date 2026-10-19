// Captures the summarization requests real pi's compact() sends — the oracle
// behind TestSummarizationRequestsMatchPiCapture in
// coding/compaction_request_test.go.
//
//   node capture-requests.mts <compaction-module> <out.json> <version>
//   e.g. node capture-requests.mts <npm>/1.3.0/node_modules/@earendil-works/pi-coding-agent/dist/core/compaction/index.js requests-1.3.0.json 1.3.0
//
// <compaction-module> exports compact, createFileOps and
// extractFileOpsFromMessage: the npm build's dist/core/compaction/index.js, or
// packages/coding-agent/src/core/compaction/index.ts of a src extraction when
// no build ships the change yet (1.3.0 omits thinking from the serialized
// conversation, upstream 15e7bdc14).
//
// Each scenario is a transcript whose first historyCount messages are the
// history to summarize and whose next prefixCount messages are the split
// turn's prefix; the rest is kept. The preparation carries those lists, the
// optional previous summary and the file operations of both lists, as
// prepareCompaction builds them. compact() runs with a stub streamFn that
// records each request's system prompt, user text and maxTokens (null when
// the request sets none), and answers
// request n with the scenario's replies[n-1], or "SUMMARY n" when it has none.
// The output keeps the messages as pi reads them, the requests in order with
// the reply each got, and the summary compact() returns.
//
// An empty reply is a summary, not a failure: compact() returns "" plus the
// file lists, and AgentSession appends that as the compaction. prepareCompaction
// then reads the empty summary back as a DEFINED previousSummary "", so
// `previousSummary ?? "No prior history."` keeps "" while
// `previousSummary ? UPDATE : INITIAL` picks the initial prompt; the
// *-previous-summary-empty scenarios pin both.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [entry, outFile, version] = process.argv.slice(2);
if (!entry || !outFile || !version) {
	console.error("usage: node capture-requests.mts <compaction-module> <out.json> <version>");
	process.exit(2);
}
const { compact, createFileOps, extractFileOpsFromMessage } = await import(pathToFileURL(path.resolve(entry)).href);

const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
let clock = 0;
const user = (content: string | Array<{ type: "text"; text: string }>) => ({ role: "user", content, timestamp: ++clock });
const assistant = (text: string | null, calls: Array<[string, string, Record<string, unknown>]> = [], stopReason = "stop") => ({
	role: "assistant",
	content: [
		...(text === null ? [] : [{ type: "text", text }]),
		...calls.map(([id, name, args]) => ({ type: "toolCall", id, name, arguments: args })),
	],
	api: "openai-completions",
	provider: "p",
	model: "m",
	usage,
	stopReason,
	timestamp: ++clock,
});
const thinkingAssistant = (thinking: string[], text: string | null, calls: Array<[string, string, Record<string, unknown>]> = []) => {
	const message = assistant(text, calls, calls.length > 0 ? "toolUse" : "stop");
	return { ...message, content: [...thinking.map((block) => ({ type: "thinking", thinking: block })), ...message.content] };
};
const toolResult = (id: string, name: string, text: string) => ({
	role: "toolResult",
	toolCallId: id,
	toolName: name,
	content: [{ type: "text", text }],
	isError: false,
	timestamp: ++clock,
});

const reserveTokens = 2000;
const model = {
	id: "m",
	name: "m",
	api: "openai-completions",
	provider: "p",
	baseUrl: "http://localhost",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: reserveTokens,
	maxTokens: 8192,
};

const turnWithTools = () => [
	user([{ type: "text", text: "please refactor the parser" }]),
	assistant("reading it first", [["r1", "read", { path: "/a/parser.go" }]], "toolUse"),
	toolResult("r1", "read", "package parser"),
	assistant("refactored the parser"),
	user([{ type: "text", text: "now fix the tests" }]),
	assistant(null, [["e1", "edit", { path: "/a/parser_test.go", oldText: "a", newText: "b" }]], "toolUse"),
	toolResult("e1", "edit", "ok"),
	assistant("tests fixed"),
];

const scenarios: Array<{
	name: string;
	messages: any[];
	historyCount: number;
	prefixCount: number;
	previousSummary?: string;
	replies?: string[];
}> = [
	{
		// agent-session-compaction.test.ts seedCompactableSession under keepRecentTokens 1.
		name: "split-turn-prefix-only",
		messages: [user([{ type: "text", text: "message to compact" }]), assistant("assistant response to compact")],
		historyCount: 0,
		prefixCount: 1,
	},
	{
		// compaction-summary-reasoning.test.ts "preserves the previous summary without
		// an empty history request for a split turn".
		name: "split-turn-previous-summary",
		messages: [user("Summarize this."), assistant("kept")],
		historyCount: 0,
		prefixCount: 1,
		previousSummary: "previous checkpoint",
	},
	{ name: "split-turn-with-history", messages: turnWithTools(), historyCount: 4, prefixCount: 3 },
	{
		name: "split-turn-with-history-and-previous-summary",
		messages: turnWithTools(),
		historyCount: 4,
		prefixCount: 3,
		previousSummary: "## Goal\nearlier work",
	},
	{
		name: "history-only",
		messages: [user("first question"), assistant("first answer"), user("second question")],
		historyCount: 2,
		prefixCount: 0,
	},
	{
		name: "history-only-previous-summary",
		messages: [user("first question"), assistant("first answer"), user("second question")],
		historyCount: 2,
		prefixCount: 0,
		previousSummary: "## Goal\nearlier work",
	},
	{
		name: "history-only-empty-reply",
		messages: [user("first question"), assistant("first answer"), user("second question")],
		historyCount: 2,
		prefixCount: 0,
		replies: [""],
	},
	{
		name: "history-only-empty-reply-with-file-ops",
		messages: [
			user("please read the parser"),
			assistant("reading it", [["r1", "read", { path: "/a/parser.go" }]], "toolUse"),
			toolResult("r1", "read", "package parser"),
			assistant("read it"),
			user("next question"),
		],
		historyCount: 4,
		prefixCount: 0,
		replies: [""],
	},
	{ name: "split-turn-empty-replies", messages: turnWithTools(), historyCount: 4, prefixCount: 3, replies: ["", ""] },
	{
		// The turn after an empty compaction: its summary is the previous one.
		name: "split-turn-previous-summary-empty",
		messages: [user("Summarize this."), assistant("kept")],
		historyCount: 0,
		prefixCount: 1,
		previousSummary: "",
	},
	{
		name: "split-turn-with-history-and-previous-summary-empty",
		messages: turnWithTools(),
		historyCount: 4,
		prefixCount: 3,
		previousSummary: "",
	},
	{
		name: "history-only-previous-summary-empty",
		messages: [user("first question"), assistant("first answer"), user("second question")],
		historyCount: 2,
		prefixCount: 0,
		previousSummary: "",
	},
	{
		// Thinking is left out of the serialized conversation (upstream 15e7bdc14):
		// text, tool calls and tool results record what happened.
		name: "history-only-thinking",
		messages: [
			user("why is the build red?"),
			thinkingAssistant(["the linker fails", "check the flags"], "checking the linker", [["r1", "read", { path: "/a/Makefile" }]]),
			toolResult("r1", "read", "LDFLAGS=-x"),
			thinkingAssistant(["only thinking, no text"], null),
			thinkingAssistant(["fix it"], "the -x flag is wrong"),
			user("next question"),
		],
		historyCount: 5,
		prefixCount: 0,
	},
	{
		name: "split-turn-thinking",
		messages: [
			user("first question"),
			thinkingAssistant(["first thought"], "first answer"),
			user("now edit it"),
			thinkingAssistant(["plan the edit"], null, [["e1", "edit", { path: "/a/x.go", oldText: "a", newText: "b" }]]),
			toolResult("e1", "edit", "ok"),
			thinkingAssistant(["done thinking"], "edited"),
		],
		historyCount: 2,
		prefixCount: 3,
	},
	{
		// computeFileLists sorts with Array.prototype.sort: by UTF-16 code unit,
		// which puts an astral character (a surrogate) before U+E000..U+FFFF,
		// where UTF-8 byte order puts it after.
		name: "history-only-file-ops-utf16-order",
		messages: [
			user("please read and edit these"),
			assistant(
				"on it",
				[
					["r1", "read", { path: "/a/｡.go" }],
					["r2", "read", { path: "/a/😀.go" }],
					["r3", "read", { path: "/a/z.go" }],
					["e1", "edit", { path: "/a/ﬀ.go", oldText: "a", newText: "b" }],
					["e2", "edit", { path: "/a/𝔞.go", oldText: "a", newText: "b" }],
				],
				"toolUse",
			),
			toolResult("r1", "read", "one"),
			toolResult("r2", "read", "two"),
			toolResult("r3", "read", "three"),
			toolResult("e1", "edit", "ok"),
			toolResult("e2", "edit", "ok"),
			assistant("done"),
			user("next question"),
		],
		historyCount: 8,
		prefixCount: 0,
	},
];

const textOf = (content: unknown): string =>
	typeof content === "string"
		? content
		: (content as Array<{ type: string; text?: string }>)
				.filter((block) => block.type === "text")
				.map((block) => block.text)
				.join("");

const out = { version, reserveTokens, modelMaxTokens: model.maxTokens, scenarios: [] as unknown[] };
for (const { name, messages, historyCount, prefixCount, previousSummary, replies } of scenarios) {
	const messagesToSummarize = messages.slice(0, historyCount);
	const turnPrefixMessages = messages.slice(historyCount, historyCount + prefixCount);
	const fileOps = createFileOps();
	for (const message of [...messagesToSummarize, ...turnPrefixMessages]) extractFileOpsFromMessage(message, fileOps);

	const requests: Array<{ systemPrompt: string; text: string; maxTokens: number | null; reply: string }> = [];
	const streamFn = (_model: unknown, context: { messages: Array<{ role: string; content: unknown }> }, options: { maxTokens?: number }) => {
		const system = context.messages.filter((message) => message.role === "system");
		const users = context.messages.filter((message) => message.role === "user");
		if (system.length !== 1 || users.length !== 1 || context.messages.length !== 2) {
			throw new Error(`${name}: unexpected request shape ${JSON.stringify(context.messages.map((message) => message.role))}`);
		}
		const reply = replies?.[requests.length] ?? `SUMMARY ${requests.length + 1}`;
		requests.push({ systemPrompt: textOf(system[0].content), text: textOf(users[0].content), maxTokens: options.maxTokens ?? null, reply });
		const response = { ...assistant(reply), timestamp: 0 };
		return { result: async () => response };
	};

	const result = await compact(
		{
			firstKeptEntryId: "kept",
			messagesToSummarize,
			turnPrefixMessages,
			isSplitTurn: prefixCount > 0,
			tokensBefore: 0,
			previousSummary,
			fileOps,
			settings: { enabled: true, reserveTokens, keepRecentTokens: 1 },
		},
		model,
		"test-key",
		undefined,
		undefined,
		undefined,
		undefined,
		streamFn,
	);
	out.scenarios.push({
		name,
		messages,
		historyCount,
		prefixCount,
		...(previousSummary === undefined ? {} : { previousSummary }),
		requests,
		summary: result.summary,
	});
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
