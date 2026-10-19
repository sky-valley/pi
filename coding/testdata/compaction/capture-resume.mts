// Captures how real pi extends a compaction it finds in a resumed session file —
// the oracle behind TestResumedCompactionMatchesPiCapture in
// coding/compaction_resume_test.go.
//
//   node capture-resume.mts <root> <out.json> <version>
//   e.g. node capture-resume.mts <npm>/1.3.0 resume-1.3.0.json 1.3.0
//
// <root> is an npm root holding node_modules/@earendil-works/{pi-ai,
// pi-coding-agent} at <version>, or a src extraction holding
// packages/{ai,coding-agent}/src (and a node_modules resolving their
// dependencies) when no build ships the change yet: the summary requests
// stopped setting maxTokens in upstream 6fcb48670, after 1.3.0.
//
// Each scenario builds a session with pi's own SessionManager: messages, one
// compaction entry (summary, firstKeptEntryId, details, fromHook, and the
// system message appendCompaction stores), then more messages. The file is
// written out and opened again with SessionManager.open, as pi resumes one; the
// output keeps it as written, so the port resumes the same bytes. pi then
// compacts that branch again, as _runAutoCompaction does: prepareCompaction
// reads the previous summary, the boundary and the file lists from the entry,
// and compact() runs with a stub streamFn that records each request's system
// prompt, user text and maxTokens (null when it sets none) and answers
// "SUMMARY n". The new compaction is
// appended with compact()'s result, and the context pi sends next
// (buildSessionContext, then convertToLlm) is recorded one line per message.
// When compact() throws, as _runAutoCompaction catches it, its message is
// recorded as "error", nothing is appended, and the context recorded is the
// one pi keeps sending.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [root, outFile, version] = process.argv.slice(2);
if (!root || !outFile || !version) {
	console.error("usage: node capture-resume.mts <npm-root> <out.json> <version>");
	process.exit(2);
}
const srcPackages: Record<string, string> = { "pi-ai": "packages/ai", "pi-coding-agent": "packages/coding-agent" };
const fromSrc = fs.existsSync(path.join(root, "packages/coding-agent/src"));
const dist = (pkg: string, file: string) =>
	pathToFileURL(
		fromSrc
			? path.join(path.resolve(root), srcPackages[pkg], "src", file.replace(/\.js$/, ".ts"))
			: path.join(path.resolve(root), "node_modules/@earendil-works", pkg, "dist", file),
	).href;
const { SessionManager } = await import(dist("pi-coding-agent", "core/session-manager.js"));
const { compact, prepareCompaction } = await import(dist("pi-coding-agent", "core/compaction/index.js"));
const { convertToLlm } = await import(dist("pi-coding-agent", "core/messages.js"));
const { getSystemMessageText } = await import(dist("pi-ai", "utils/text.js"));

const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
let clock = 1_789_603_200_000;
const user = (text: string) => ({ role: "user", content: [{ type: "text", text }], timestamp: ++clock });
const assistant = (text: string, calls: Array<[string, string, Record<string, unknown>]> = []) => ({
	role: "assistant",
	content: [{ type: "text", text }, ...calls.map(([id, name, args]) => ({ type: "toolCall", id, name, arguments: args }))],
	api: "openai-completions",
	provider: "p",
	model: "m",
	usage,
	stopReason: calls.length > 0 ? "toolUse" : "stop",
	timestamp: ++clock,
});
const toolResult = (id: string, name: string, text: string) => ({
	role: "toolResult",
	toolCallId: id,
	toolName: name,
	content: [{ type: "text", text }],
	isError: false,
	timestamp: ++clock,
});
const system = (preamble: string) => ({ role: "system", content: "", sections: { preamble }, timestamp: ++clock });

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

type Scenario = {
	name: string;
	// Messages before the compaction; the one at keptIndex is its first kept
	// entry. Without one the compaction keeps nothing (appendCompaction then
	// stores its own id as firstKeptEntryId).
	before: any[];
	keptIndex?: number;
	summary: string;
	details?: unknown;
	// Written as given, null included: appendCompaction does not check its
	// type, and pi reads it by truthiness. Absent, the capture writes false;
	// present but undefined, the entry has no fromHook key.
	fromHook?: unknown;
	// JSON text no JavaScript value serializes to (a number past float64's
	// range): each key, a JSON string in the file, is replaced by its value
	// before pi opens the file.
	raw?: Record<string, string>;
	// A context edit that omits the compaction entry. SessionManager refuses
	// to write one (a compaction is not editable), so it is appended raw, as a
	// hand-edited or foreign file would hold it; the projection still honours
	// it, and prepareCompaction then finds no previous compaction.
	omitCompaction?: boolean;
	after: any[];
};

const scenarios: Scenario[] = [
	{
		// A model's empty reply is a summary: the entry holds "", and pi reads it
		// back as a defined previousSummary.
		name: "previous-summary-empty",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "",
		details: { readFiles: [], modifiedFiles: [] },
		after: [user("q3"), assistant("a3"), user("q4")],
	},
	{
		name: "previous-summary-with-files",
		before: [
			user("q1"),
			assistant("reading", [["r1", "read", { path: "/a/old.go" }]]),
			toolResult("r1", "read", "package old"),
			assistant("a1"),
			user("q2"),
			assistant("a2"),
		],
		keptIndex: 4,
		summary: "## Goal\nold work\n\n<read-files>\n/a/old.go\n</read-files>\n\n<modified-files>\n/a/changed.go\n</modified-files>",
		details: { readFiles: ["/a/old.go"], modifiedFiles: ["/a/changed.go"] },
		after: [
			user("q3"),
			assistant("editing", [["e1", "edit", { path: "/a/new.go", oldText: "a", newText: "b" }]]),
			toolResult("e1", "edit", "ok"),
			assistant("a3"),
			user("q4"),
		],
	},
	{
		// An extension's compaction: pi ignores its details.
		name: "previous-summary-from-hook",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nhook work",
		details: { readFiles: ["/a/hook.go"], modifiedFiles: [] },
		fromHook: true,
		after: [user("q3"), assistant("a3"), user("q4")],
	},
	{
		name: "previous-summary-from-hook-truthy",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nhook work",
		details: { readFiles: ["/a/hook.go"], modifiedFiles: [] },
		fromHook: 1,
		after: [user("q3"), assistant("a3"), user("q4")],
	},
	{
		name: "previous-summary-from-hook-falsy",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nown work",
		details: { readFiles: ["/a/own.go"], modifiedFiles: [] },
		fromHook: "",
		after: [user("q3"), assistant("a3"), user("q4")],
	},
	{
		// The entry stores the prompt state; a system message recorded right
		// after the compaction keeps its place and becomes the next replay.
		name: "previous-summary-system-messages",
		before: [system("v1"), user("q1"), assistant("a1"), system("v2"), user("q2"), assistant("a2")],
		keptIndex: 4,
		summary: "## Goal\nprompted work",
		details: { readFiles: [], modifiedFiles: [] },
		after: [system("v3"), user("q3"), assistant("a3"), user("q4")],
	},
	{
		// Ending on an assistant splits the last turn: the history goes to the
		// update prompt, the turn's prefix to its own request.
		name: "previous-summary-split-turn",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nold work",
		details: { readFiles: [], modifiedFiles: [] },
		after: [user("q3"), assistant("a3"), user("q4"), assistant("a4")],
	},
	{
		// A compaction that keeps nothing, then a split turn with no history
		// before it: pi keeps the previous summary as the history part, even an
		// empty one.
		name: "previous-summary-empty-retain-none-split-turn",
		before: [user("q1"), assistant("a1")],
		summary: "",
		details: { readFiles: [], modifiedFiles: [] },
		after: [user("q2"), assistant("a2")],
	},
	{
		name: "compaction-omitted-by-context-edit",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nhidden work",
		details: { readFiles: ["/a/hidden.go"], modifiedFiles: [] },
		omitCompaction: true,
		after: [user("q3"), assistant("a3"), user("q4")],
	},
	// Appended last, so earlier scenarios keep their message timestamps.
	//
	// fromHook by truthiness: null, 0, -0 and 1e-400 (0 once parsed) are
	// falsy and the details merge; [], {}, "0", "false" and 1e400 (Infinity)
	// are truthy and pi ignores the details.
	...[
		["null", null],
		["0", 0],
		["raw-negative-0", "raw:-0"],
		["empty-array", []],
		["empty-object", {}],
		["string-0", "0"],
		["string-false", "false"],
		["raw-1e400", "raw:1e400"],
		["raw-1e-400", "raw:1e-400"],
	].map(([label, fromHook]): Scenario => ({
		name: `previous-summary-from-hook-${label}`,
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nwork",
		details: { readFiles: [`/a/${label}.go`], modifiedFiles: [] },
		fromHook,
		...(typeof fromHook === "string" && fromHook.startsWith("raw:") ? { raw: { [fromHook]: fromHook.slice(4) } } : {}),
		after: [user("q3"), assistant("a3"), user("q4")],
	})),
	{
		// appendCompaction stores details unchecked, and extractFileOperations
		// adds every element of their lists to its Sets: a primitive by
		// SameValueZero (5.0 is 5, -0 is 0; 5 and "5" are two), each object
		// or array as its own element. The read list drops what the modified
		// set holds, both sort by String(value), ties keeping their order, and
		// join writes null as "".
		name: "previous-summary-details-odd-types",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nodd work",
		details: {
			readFiles: [
				5,
				"/a/r.go",
				null,
				{ x: 1 },
				["b", "c"],
				true,
				"/a/m.go",
				"5",
				"raw:5.0",
				"raw:-0",
				0,
				[null, "x", ["y", 2]],
				{},
				1e21,
				0.1,
				"raw:1e400",
				"Infinity",
				"/a/😀.go",
				"/a/｡.go",
			],
			modifiedFiles: ["/a/m.go", 7, 0],
		},
		raw: { "raw:5.0": "5.0", "raw:-0": "-0", "raw:1e400": "1e400" },
		after: [
			user("q3"),
			assistant("editing", [["e1", "edit", { path: "/a/r.go", oldText: "a", newText: "b" }]]),
			toolResult("e1", "edit", "ok"),
			assistant("a3"),
			user("q4"),
		],
	},
	// A value with no string form: an object with its own "toString" member
	// (JSON cannot make it callable, so String() falls to valueOf, which
	// returns the object, and throws a TypeError), directly or inside an
	// array. computeFileLists' sort and formatFileOperations' join take
	// String() of every element, so compact() throws after its summary
	// request; an own "valueOf" member alone is harmless.
	...[
		["no-string-form", { readFiles: [{ toString: 1 }, "/a/r.go"], modifiedFiles: [] }],
		["nested-no-string-form", { readFiles: ["/a/r.go"], modifiedFiles: [[{ toString: "x" }]] }],
		["value-of", { readFiles: [{ valueOf: 1 }, "/a/r.go"], modifiedFiles: [] }],
	].map(([label, details]): Scenario => ({
		name: `previous-summary-details-${label}`,
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nwork",
		details,
		after: [user("q3"), assistant("a3"), user("q4")],
	})),
	{
		// true, false and null each held once by the Sets (SameValueZero),
		// within a list and across the two: the read list drops the false and
		// null the modified set holds.
		name: "previous-summary-details-duplicate-primitives",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nwork",
		details: { readFiles: [true, true, null, null, false, "/a/r.go"], modifiedFiles: [false, null, null] },
		after: [user("q3"), assistant("a3"), user("q4")],
	},
	{
		// appendCompaction called without fromHook writes no fromHook key; pi
		// reads the absent value as undefined, which is falsy, and merges the
		// details.
		name: "previous-summary-from-hook-absent",
		before: [user("q1"), assistant("a1"), user("q2"), assistant("a2")],
		keptIndex: 2,
		summary: "## Goal\nwork",
		details: { readFiles: ["/a/absent.go"], modifiedFiles: [] },
		fromHook: undefined,
		after: [user("q3"), assistant("a3"), user("q4")],
	},
];

const textOf = (content: unknown): string =>
	typeof content === "string"
		? content
		: (content as Array<{ type: string; text?: string }>)
				.filter((block) => block.type === "text")
				.map((block) => block.text)
				.join("");

// One line per message the model is sent.
const describe = (message: any): string => {
	switch (message.role) {
		case "system":
			return `system:${getSystemMessageText(message)}`;
		case "assistant":
			return `assistant:${textOf(message.content)}${message.content
				.filter((block: any) => block.type === "toolCall")
				.map((block: any) => ` [${block.name} ${JSON.stringify(block.arguments)}]`)
				.join("")}`;
		default:
			return `${message.role}:${textOf(message.content)}`;
	}
};

const out = { version, reserveTokens, contextWindow: model.contextWindow, modelMaxTokens: model.maxTokens, scenarios: [] as unknown[] };
const dir = fs.mkdtempSync(path.join(os.tmpdir(), "capture-resume-"));
for (const scenario of scenarios) {
	const writer = SessionManager.inMemory("/tmp/capture-resume");
	let keptId: string | undefined;
	scenario.before.forEach((message, i) => {
		const id = writer.appendMessage(message);
		if (i === scenario.keptIndex) keptId = id;
	});
	const fromHook = "fromHook" in scenario ? scenario.fromHook : false;
	const compactionId = writer.appendCompaction(scenario.summary, keptId, 100, scenario.details, fromHook);
	if (scenario.omitCompaction) {
		writer._appendEntry({
			type: "context_edit",
			id: "0mit0000",
			parentId: writer.getLeafId(),
			timestamp: new Date().toISOString(),
			targetId: compactionId,
			replacement: null,
		});
	}
	for (const message of scenario.after) writer.appendMessage(message);
	// The file as written, then resumed the way pi resumes one.
	let file = `${[writer.getHeader(), ...writer.getEntries()].map((entry) => JSON.stringify(entry)).join("\n")}\n`;
	for (const [key, value] of Object.entries(scenario.raw ?? {})) {
		if (!file.includes(JSON.stringify(key))) throw new Error(`${scenario.name}: ${key} is not in the file`);
		file = file.replaceAll(JSON.stringify(key), value);
	}
	const sessionFile = path.join(dir, `${scenario.name}.jsonl`);
	fs.writeFileSync(sessionFile, file);
	const sm = SessionManager.open(sessionFile, dir);
	const resumed = convertToLlm(sm.buildSessionContext().messages).map(describe);

	const preparation = prepareCompaction(sm.getBranch(), { enabled: true, reserveTokens, keepRecentTokens: 1 });
	const requests: Array<{ systemPrompt: string; text: string; maxTokens: number | null }> = [];
	const streamFn = (_model: unknown, context: { messages: Array<{ role: string; content: unknown }> }, options: { maxTokens?: number }) => {
		const systemMessages = context.messages.filter((message) => message.role === "system");
		const users = context.messages.filter((message) => message.role === "user");
		if (systemMessages.length !== 1 || users.length !== 1 || context.messages.length !== 2) {
			throw new Error(`${scenario.name}: unexpected request shape ${JSON.stringify(context.messages.map((message) => message.role))}`);
		}
		requests.push({ systemPrompt: getSystemMessageText(systemMessages[0]), text: textOf(users[0].content), maxTokens: options.maxTokens ?? null });
		const response = { ...assistant(`SUMMARY ${requests.length}`), timestamp: 0 };
		return { result: async () => response };
	};
	let result: any;
	let error: string | undefined;
	try {
		result = await compact(preparation, model, "test-key", undefined, undefined, undefined, undefined, streamFn);
	} catch (e) {
		error = e instanceof Error ? e.message : String(e);
	}
	if (result) sm.appendCompaction(result.summary, result.firstKeptEntryId, result.tokensBefore, result.details, false);
	const compacted = convertToLlm(sm.buildSessionContext().messages).map(describe);

	out.scenarios.push({
		name: scenario.name,
		file,
		resumed,
		previousSummary: preparation.previousSummary,
		requests,
		...(result ? { summary: result.summary, details: result.details } : { error }),
		compacted,
	});
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
