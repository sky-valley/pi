// Captures how real pi's AgentSession declares its system prompt into the
// transcript across prompts and continued turns — the oracle behind
// coding/session_next_turn_test.go.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> sessionprompt-16292398a.json 16292398a
//
// <extraction> holds packages/ai, packages/agent and packages/coding-agent at
// <sha> (`git archive <sha> packages/ai packages/agent packages/coding-agent`
// from the upstream clone), a node_modules resolving their dependencies (the
// npm build's), packages/ai/src/providers/data (the generated model catalog,
// needed only so the modules import), and
// packages/{agent,coding-agent}/node_modules/@earendil-works/{pi-ai,pi-agent-core}
// resolving to packages/{ai,agent}/src. The npm build 0.85.1 predates upstream
// 9e05370b2 and e4c75a732, so these are src captures: re-verify them against
// the first build that ships them (the BUILD wins).
//
// AgentSession itself does not import under node without the host's modules
// (theme, export-html, tools/index), so the script runs the REAL upstream Agent
// (packages/agent/src/agent.ts) under the AgentSession members that decide the
// declaration, taken verbatim from core/agent-session.ts at <sha> and
// type-stripped: _installAgentNextTurnRefresh,
// _installAgentForcedPromptProjection, _preparePromptAndToolLoadout,
// _normalizePromptGuidelines, getActiveToolNames and the systemPrompt getter.
// Transcribed around them (each marked below): sdk.ts's Agent with no prompt and
// no tools, _buildRuntime's loadout for the default tools, prompt()'s declaration
// with no extension handlers (or, for a step with `force`, with the options a
// before_agent_start handler returning that systemPrompt leaves:
// forceSystemPrompt set on the normalized base options — runner.ts
// emitBeforeAgentStart), _runAgentPrompt's reset of the run options,
// _refreshToolRegistry's guidelines map, and _compactBeforeNextAssistantResponse
// returning the context unchanged (the port compacts in a per-request
// TransformContext instead — docs/UPSTREAM.md D4).
//
// The tool inputs are the Go port's, as in ../systemprompt/capture.mts: the
// default tools' snippets and guidelines, which the Go test does not need to
// repeat because it asserts structure (roles, section names, tool names) and
// checks the replayed prompt against Session.SystemPrompt.
//
// A message's projection is its role, and for a system message its content
// text when non-empty, its section names and its declared tool names.
//
// Since upstream 16292398a a forced prompt never reaches the transcript: it is
// projected onto the REQUEST by _installAgentForcedPromptProjection, which the
// script installs as the constructor does. So a forced step's requests show one
// collapsed system head holding the forced text, while the transcript keeps
// recording only the structured sections.
//
// Per request the script records the messages' projection and the state the
// refresh hands the loop: the model, the reasoning level and the tools the
// request's transcript declares. A reply's `then` changes the agent's model,
// thinking level or tools while that request is served, as a mid-run /model
// or loadout change would. Changing the tools re-derives the prompt's tool
// sections in pi (selectedTools = getActiveToolNames()), which is
// setActiveTools' half of the refresh; such a scenario is marked
// `loadoutChange` and the Go test compares only its request state.
import fs from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
process.env.PI_PACKAGE_DIR = "/pkg";
const url = (file: string) => pathToFileURL(path.join(extraction, file)).href;
const { Agent } = await import(url("packages/agent/src/agent.ts"));
const { createAssistantMessageEventStream } = await import(url("packages/ai/src/utils/event-stream.ts"));
const { fauxAssistantMessage, fauxToolCall } = await import(url("packages/ai/src/providers/faux.ts"));
const { getCurrentSystemMessage, getCurrentTools } = await import(url("packages/ai/src/utils/transcript.ts"));
const { contentText, getSystemMessageText } = await import(url("packages/ai/src/utils/text.ts"));
const systemPrompt = await import(url("packages/coding-agent/src/core/system-prompt.ts"));

// --- AgentSession members, verbatim from core/agent-session.ts at <sha> -----
const agentSessionSource = fs.readFileSync(
	path.join(extraction, "packages/coding-agent/src/core/agent-session.ts"),
	"utf8",
);

/** Returns the class member starting at `signature`, through its matching brace. */
function member(signature: string): string {
	const start = agentSessionSource.indexOf(signature);
	if (start < 0 || agentSessionSource.indexOf(signature, start + 1) >= 0) {
		throw new Error(`agent-session.ts must contain exactly one ${JSON.stringify(signature)}`);
	}
	let depth = 0;
	let quote = "";
	for (let i = agentSessionSource.indexOf("{", start); i < agentSessionSource.length; i++) {
		const ch = agentSessionSource[i];
		if (quote) {
			if (ch === "\\") i++;
			else if (ch === quote) quote = "";
			continue;
		}
		if (ch === '"' || ch === "'" || ch === "`") quote = ch;
		else if (ch === "/" && agentSessionSource[i + 1] === "/") i = agentSessionSource.indexOf("\n", i);
		else if (ch === "{") depth++;
		else if (ch === "}" && --depth === 0) return agentSessionSource.slice(start, i + 1);
	}
	throw new Error(`unterminated member ${signature}`);
}

const members = [
	"private _installAgentNextTurnRefresh(): void {",
	"private _installAgentForcedPromptProjection(): void {",
	"private _preparePromptAndToolLoadout(",
	"private _normalizePromptGuidelines(",
	"getActiveToolNames(): string[] {",
	"get systemPrompt(): string {",
].map(member);
const AgentSessionMembers = new Function(
	"normalizeBuildSystemPromptOptions",
	"buildSystemPrompt",
	"buildSystemPromptSections",
	"buildSystemPromptState",
	"diffSystemPromptSections",
	"getCurrentSystemMessage",
	"contentText",
	`return (${stripTypeScriptTypes(`class AgentSessionMembers {\n${members.join("\n")}\n}`)});`,
)(
	systemPrompt.normalizeBuildSystemPromptOptions,
	systemPrompt.buildSystemPrompt,
	systemPrompt.buildSystemPromptSections,
	// Added by upstream e4c75a732; undefined at earlier shas, whose members do not use it.
	// 16292398a's _preparePromptAndToolLoadout went back to buildSystemPromptSections;
	// the export stays (buildSystemPrompt renders through it).
	systemPrompt.buildSystemPromptState,
	systemPrompt.diffSystemPromptSections,
	getCurrentSystemMessage,
	contentText,
);

// --- Session construction (transcribed) --------------------------------------
const toolSnippets = {
	read: "Read file contents",
	bash: "Execute bash commands (ls, grep, find, etc.)",
	edit: "Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
	write: "Create or overwrite files",
};
const toolGuidelines = {
	read: ["Use read to examine files instead of cat or sed."],
	bash: ["You can inspect PI_* environment variables for current model and session details."],
	edit: [
		"Use edit for targeted changes. Batch changes to one file in a single call.",
		"Each edits[].oldText must exactly and uniquely match the original file. Keep matches short and non-overlapping; merge nearby changes without including large unchanged regions.",
	],
	write: ["Use write only for new files or complete rewrites."],
};

type Then = { model?: string; thinkingLevel?: string; tools?: string[] };
type Reply = ({ text: string } | { toolCall: { id: string; name: string; arguments: Record<string, unknown> } }) & {
	then?: Then;
};
type Step =
	| { kind: "load"; messages: unknown[]; append?: boolean }
	| { kind: "prompt"; text: string; force?: string; replies: Reply[] }
	| { kind: "continue"; replies: Reply[] };

function project(message: any) {
	if (message.role !== "system") return { role: message.role };
	const content = contentText(message.content);
	return {
		role: "system",
		...(content ? { content } : {}),
		sections: Object.keys(message.sections ?? {}),
		...(message.toolsAdded ? { toolsAdded: message.toolsAdded.map((tool: any) => tool.name) } : {}),
		...(message.toolsRemoved ? { toolsRemoved: message.toolsRemoved.map((tool: any) => tool.name) } : {}),
	};
}

const fauxModel = (id: string) => ({ id, api: "faux", provider: "faux", reasoning: true });

function createSession(requests: unknown[][], requestState: unknown[], replies: Reply[]) {
	let session: any;
	const streamFn = (model: any, context: any, options: any) => {
		requests.push(context.messages.map(project));
		requestState.push({
			model: model.id,
			reasoning: options?.reasoning ?? null,
			tools: getCurrentTools(context.messages).map((tool: any) => tool.name),
		});
		const reply = replies.shift();
		if (!reply) throw new Error("no scripted reply left");
		if (reply.then?.model) session.agent.state.model = fauxModel(reply.then.model);
		if (reply.then?.thinkingLevel) session.agent.state.thinkingLevel = reply.then.thinkingLevel;
		if (reply.then?.tools) {
			const names = reply.then.tools;
			session.agent.state.tools = session.agent.state.tools.filter((tool: any) => names.includes(tool.name));
		}
		const message =
			"text" in reply
				? fauxAssistantMessage(reply.text, { timestamp: 0 })
				: fauxAssistantMessage([fauxToolCall(reply.toolCall.name, reply.toolCall.arguments, { id: reply.toolCall.id })], {
						stopReason: "toolUse",
						timestamp: 0,
					});
		const stream = createAssistantMessageEventStream();
		queueMicrotask(() => stream.push({ type: "done", reason: message.stopReason, message }));
		return stream;
	};
	// sdk.ts: the Agent gets the model and the (clamped) default thinking level,
	// and no systemPrompt and no tools.
	const agent = new Agent({ initialState: { model: fauxModel("faux-1"), thinkingLevel: "medium" }, streamFn });
	const tools = Object.keys(toolSnippets).map((name) => ({
		name,
		label: name,
		description: `${name} tool`,
		parameters: { type: "object", properties: {} },
		execute: async () => ({ content: [{ type: "text", text: "ok" }], details: {} }),
	}));
	session = Object.assign(Object.create(AgentSessionMembers.prototype), {
		agent,
		_toolRegistry: new Map(tools.map((tool) => [tool.name, tool])),
		_runSystemPromptOptions: undefined,
		// _compactBeforeNextAssistantResponse: below the threshold it returns the context.
		_compactBeforeNextAssistantResponse: async (context: unknown) => context,
	});
	// _buildRuntime → _rebuildSystemPrompt for the default active tools.
	agent.state.tools = tools;
	session._baseSystemPromptOptions = systemPrompt.normalizeBuildSystemPromptOptions({
		cwd: "/proj",
		skills: [],
		contextFiles: [],
		customPrompt: undefined,
		appendSystemPrompt: "",
		selectedTools: session.getActiveToolNames(),
		toolSnippets,
		toolGuidelines,
	});
	// The constructor installs the next-turn refresh, then the forced-prompt
	// projection over it (upstream 16292398a).
	session._installAgentNextTurnRefresh();
	session._installAgentForcedPromptProjection();
	return session;
}

async function run(steps: Step[]) {
	const requests: unknown[][] = [];
	const requestState: unknown[] = [];
	const replies: Reply[] = [];
	const session = createSession(requests, requestState, replies);
	for (const step of steps) {
		switch (step.kind) {
			case "load":
				session.agent.state.messages = step.append ? [...session.agent.state.messages, ...step.messages] : step.messages;
				break;
			case "prompt": {
				replies.push(...step.replies);
				// prompt() with no extension handlers: emitBeforeAgentStart returns the
				// normalized base options and selectedTools becomes the live loadout.
				const options = systemPrompt.normalizeBuildSystemPromptOptions(session._baseSystemPromptOptions);
				if (step.force !== undefined) options.forceSystemPrompt = step.force;
				options.selectedTools = session.getActiveToolNames();
				const messages: unknown[] = [{ role: "user", content: [{ type: "text", text: step.text }], timestamp: 0 }];
				const updateMessage = session._preparePromptAndToolLoadout(options);
				session._runSystemPromptOptions = options;
				if (updateMessage) messages.unshift(updateMessage);
				try {
					await session.agent.prompt(messages);
				} finally {
					// _runAgentPrompt's finally.
					session._runSystemPromptOptions = undefined;
				}
				break;
			}
			case "continue":
				replies.push(...step.replies);
				await session.agent.continue();
				break;
		}
		if (session.agent.state.errorMessage) throw new Error(session.agent.state.errorMessage);
	}
	if (replies.length > 0) throw new Error(`${replies.length} scripted replies unused`);
	const current = getCurrentSystemMessage(session.agent.state.messages);
	return {
		requests,
		requestState,
		transcript: session.agent.state.messages.map(project),
		replayedPromptIsSystemPrompt: current ? getSystemMessageText(current) === session.systemPrompt : false,
	};
}

const user = (text: string, timestamp: number) => ({ role: "user", content: [{ type: "text", text }], timestamp });
const assistant = (text: string, timestamp: number) => fauxAssistantMessage(text, { timestamp });
const readCall: Reply = { toolCall: { id: "call-1", name: "read", arguments: { path: "missing.txt" } } };

const scenarios: Array<{ name: string; loadoutChange?: boolean; steps: Step[] }> = [
	{
		// A transcript written before transcript-owned prompts: continue's first
		// request declares nothing; the next-turn refresh declares from turn 2.
		name: "continue-without-system-message",
		steps: [
			{ kind: "load", messages: [user("list files", 1)] },
			{ kind: "continue", replies: [readCall, { text: "done" }] },
		],
	},
	{
		// A declared transcript: no continued turn re-declares.
		name: "continue-with-declared-prompt",
		steps: [
			{ kind: "prompt", text: "one", replies: [{ text: "first" }] },
			{ kind: "load", messages: [user("two", 2)], append: true },
			{ kind: "continue", replies: [readCall, { text: "done" }] },
		],
	},
	{
		// The refresh hands the next request the agent's live model, thinking
		// level and tools.
		name: "continue-follows-live-agent-state",
		loadoutChange: true,
		steps: [
			{ kind: "prompt", text: "one", replies: [{ text: "first" }] },
			{ kind: "load", messages: [user("two", 2)], append: true },
			{
				kind: "continue",
				replies: [{ ...readCall, then: { model: "faux-2", thinkingLevel: "high", tools: ["read"] } }, { text: "done" }],
			},
		],
	},
	{
		// The prompt declares up front; the refresh before the tool turn's
		// follow-up request finds nothing to add.
		name: "prompt-tool-turn",
		steps: [{ kind: "prompt", text: "one", replies: [readCall, { text: "done" }] }],
	},
	{
		// A transcript without a system message is declared by the first prompt
		// only: the second diffs against the transcript's current system message.
		name: "prompt-twice-without-system-message",
		steps: [
			{ kind: "load", messages: [user("existing", 1), assistant("old", 2)] },
			{ kind: "prompt", text: "one", replies: [{ text: "first" }] },
			{ kind: "prompt", text: "two", replies: [{ text: "second" }] },
		],
	},
	{
		// system-prompt-updates.test.ts 'a forced prompt is sent as the leading
		// prompt for the run and never recorded' (upstream 16292398a): prompts two
		// and three are forced. Each forced request collapses to one system head
		// holding the forced text and the transcript's current tools; the
		// transcript records nothing for it, so leaving the forced prompt needs no
		// restoring declaration.
		name: "forced-prompt-projects-and-is-not-recorded",
		steps: [
			{ kind: "prompt", text: "one", replies: [{ text: "one" }] },
			{ kind: "prompt", text: "two", force: "Exact prompt.", replies: [{ text: "two" }] },
			{ kind: "prompt", text: "three", force: "Exact prompt.", replies: [{ text: "three" }] },
			{ kind: "prompt", text: "four", replies: [{ text: "four" }] },
		],
	},
	{
		// A forced prompt changing to another forced prompt projects the new text;
		// within a forced run every request of the turn carries the same head.
		name: "forced-prompt-changes-and-refreshes",
		steps: [
			{ kind: "prompt", text: "one", force: "Prompt A.", replies: [readCall, { text: "done" }] },
			{ kind: "prompt", text: "two", force: "Prompt B.", replies: [{ text: "second" }] },
		],
	},
	{
		// A transcript whose replayed prompt has no sections (an Agent seeded with
		// a plain systemPrompt): the first prompt patches the full section set onto
		// it, and the content it already carries stays in the replay.
		name: "prompt-over-an-opaque-system-message",
		steps: [
			{
				kind: "load",
				messages: [
					{ role: "system", content: "Legacy prompt.", timestamp: 0 },
					user("existing", 1),
					assistant("old", 2),
				],
			},
			{ kind: "prompt", text: "one", replies: [{ text: "first" }] },
			{ kind: "prompt", text: "two", replies: [{ text: "second" }] },
		],
	},
];

// _refreshToolRegistry's _toolPromptGuidelines, over _normalizePromptGuidelines
// (transcribed map construction; the normalizer is verbatim).
const guidelineTools: Array<{ name: string; promptGuidelines?: string[] }> = [
	{
		name: "custom",
		promptGuidelines: [
			"nel rule",
			"nel tail",
			"  shared rule  ",
			"shared rule",
			"﻿bom rule﻿",
			" nbsp rule ",
			"᠎mvs rule᠎",
			"​zwsp rule​",
			"　ideographic rule　",
			"vt rule",
			"",
			"   ",
		],
	},
	{ name: "blank", promptGuidelines: ["", " \t\n "] },
	{ name: "none" },
	{ name: "empty", promptGuidelines: [] },
];
const normalizer = Object.create(AgentSessionMembers.prototype);
const promptGuidelines = {
	tools: guidelineTools,
	normalized: Array.from(
		new Map(
			guidelineTools
				.map((definition) => {
					const guidelines = normalizer._normalizePromptGuidelines(definition.promptGuidelines);
					return guidelines.length > 0 ? ([definition.name, guidelines] as const) : undefined;
				})
				.filter((entry): entry is readonly [string, string[]] => entry !== undefined),
		).entries(),
	),
};

const out = {
	sha,
	sessions: [] as unknown[],
	promptGuidelines,
};
for (const { name, loadoutChange, steps } of scenarios) {
	out.sessions.push({ name, loadoutChange: loadoutChange ?? false, steps, ...(await run(steps)) });
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
