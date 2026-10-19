// Captures what real pi's system-prompt.ts builds — the oracle behind
// coding/systemprompt_golden_test.go.
//
//   node capture.mts <pi-coding-agent package dir> <out.json> <version>
//   e.g. ... capture.mts <npm>/node_modules/@earendil-works/pi-coding-agent systemprompt-1.3.0.json 1.3.0
//
// The package dir is the published build (its dist/core/system-prompt.js and
// the @earendil-works/pi-ai it resolves). 1.3.0 is the first build that ships
// the codemode docs line (upstream 6f1072cc0) and hiddenTools (c30840c2e);
// the earlier goldens were src captures at e4c75a732/4259686d9, from before
// any build shipped the sectioned prompt.
//
// Each build case calls buildSystemPrompt, buildSystemPromptSections and
// buildSystemPromptState on its own, recording the value or the message it
// throws; a state keeps its `sections` key only when pi sets it.
//
// The inputs are the Go port's: toolSnippets is coding.ToolSnippets and
// toolGuidelines is what NewSession collects from the default tools'
// PromptGuidelines (the Go test checks the inputs against the Go values).
// PI_PACKAGE_DIR pins the documentation paths to /pkg, which the Go test
// passes as ReadmePath/DocsPath/ExamplesPath. JS objects whose key order
// matters (sections) are written as [name, value] pairs and built with
// Object.fromEntries.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile, version] = process.argv.slice(2);
if (!pkg || !outFile || !version) {
	console.error("usage: node capture.mts <pi-coding-agent package dir> <out.json> <version>");
	process.exit(2);
}
process.env.PI_PACKAGE_DIR = "/pkg";
const { buildSystemPrompt, buildSystemPromptSections, buildSystemPromptState, diffSystemPromptSections } = await import(
	pathToFileURL(path.join(pkg, "dist/core/system-prompt.js")).href
);

const toolSnippets = {
	read: "Read file contents",
	bash: "Execute bash commands (ls, grep, find, etc.)",
	powershell: "Execute PowerShell commands",
	edit: "Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
	write: "Create or overwrite files",
	grep: "Search file contents for patterns (respects .gitignore)",
	find: "Find files by glob pattern (respects .gitignore)",
	ls: "List directory contents",
	web_fetch: "Fetch a web URL and return readable text",
};
// The default tools' guidelines, keyed by name (agent-session.ts _rebuildSystemPrompt).
const defaultToolGuidelines = {
	read: ["Use read to examine files instead of cat or sed."],
	bash: ["You can inspect PI_* environment variables for current model and session details."],
	edit: [
		"Use edit for precise changes (edits[].oldText must match exactly)",
		"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
		"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
		"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
	],
	write: ["Use write only for new files or complete rewrites."],
};
const demoSkill = { name: "demo", description: "d", filePath: "/proj/.pi/skills/demo/SKILL.md", disableModelInvocation: false };
const hiddenSkill = { name: "hidden", description: "no", filePath: "/proj/.pi/skills/hidden/SKILL.md", disableModelInvocation: true };
const agentsFile = { path: "/proj/AGENTS.md", content: "follow the rules" };

type Pairs = Array<[string, string | null]>;
type Input = Record<string, unknown> & { sections?: Pairs };

const builds: Array<{ name: string; input: Input }> = [
	{
		name: "default-tools",
		input: {
			selectedTools: ["read", "bash", "edit", "write"],
			toolSnippets,
			toolGuidelines: defaultToolGuidelines,
			cwd: "/proj",
		},
	},
	{
		name: "custom-prompt-assembly",
		input: {
			customPrompt: "You are a custom agent.",
			appendSystemPrompt: "Appended instructions.",
			selectedTools: ["read", "bash"],
			toolSnippets,
			promptGuidelines: ["Use read to examine files instead of cat or sed."],
			cwd: "/proj",
			contextFiles: [agentsFile],
			skills: [demoSkill],
		},
	},
	{
		name: "bash-only-custom-prompt",
		input: {
			customPrompt: "You are a custom agent.",
			appendSystemPrompt: "Appended instructions.",
			selectedTools: ["bash"],
			toolSnippets,
			cwd: "/proj",
			contextFiles: [agentsFile],
			skills: [demoSkill],
		},
	},
	{
		name: "bash-only-default-prompt",
		input: {
			selectedTools: ["bash"],
			toolSnippets,
			appendSystemPrompt: "Appended instructions.",
			cwd: "/proj",
			contextFiles: [agentsFile],
			skills: [demoSkill],
		},
	},
	{
		name: "empty-tool-set",
		input: { selectedTools: [], toolSnippets, cwd: "/proj", skills: [demoSkill] },
	},
	{
		name: "rules-order-trim-dedupe",
		input: {
			selectedTools: ["edit", "read", "bash", "edit"],
			toolSnippets: { read: "Read file contents", edit: "Edit" },
			toolGuidelines: {
				read: ["  shared rule  ", "\ufeffbom rule\ufeff", ""],
				edit: ["edit rule", "shared rule"],
				bash: ["bash rule", "\u0085nel rule"],
				grep: ["unselected rule"],
			},
			promptGuidelines: ["prompt rule", "shared rule", "   ", "\u00a0nbsp rule\u2028", "Be concise in your responses"],
			cwd: "/proj",
		},
	},
	{
		name: "selected-tools-default",
		input: { toolSnippets, toolGuidelines: defaultToolGuidelines, cwd: "/proj", skills: [demoSkill, hiddenSkill] },
	},
	{
		name: "force-system-prompt",
		input: {
			forceSystemPrompt: "Exact prompt.",
			customPrompt: "ignored",
			selectedTools: ["read"],
			toolSnippets,
			sections: [["Bad Name", "never validated"]],
			cwd: "/tmp",
		},
	},
	{ name: "force-system-prompt-empty", input: { forceSystemPrompt: "", cwd: "/tmp" } },
	// system-prompt.test.ts 'preserves an exact forced prompt without sections'.
	{ name: "force-system-prompt-exact", input: { forceSystemPrompt: "exact", cwd: "/tmp" } },
	// A forced prompt is taken as given: never trimmed, and no other option adds to it.
	{
		name: "force-system-prompt-verbatim",
		input: {
			forceSystemPrompt: "  spaced\n\n",
			appendSystemPrompt: "Appended.",
			contextFiles: [agentsFile],
			skills: [demoSkill],
			sections: [["plan_mode", "Plan only."]],
			cwd: "/tmp",
		},
	},
	{
		name: "custom-sections-default-prompt",
		input: {
			selectedTools: ["read"],
			toolSnippets: { read: "Read file contents" },
			appendSystemPrompt: "Append.",
			contextFiles: [agentsFile],
			skills: [demoSkill],
			cwd: "/proj",
			sections: [
				["plan_mode", "Plan only."],
				["tools", "Overridden tools."],
				["empty", ""],
				["cwd", "custom cwd"],
				["skills", "Custom skills."],
				["z-9_x", "last"],
			],
		},
	},
	{
		name: "custom-sections-custom-prompt",
		input: {
			customPrompt: "You are Exact.",
			selectedTools: [],
			cwd: "/tmp",
			sections: [
				["tools", "T"],
				["rules", "R"],
				["docs", ""],
			],
		},
	},
	{ name: "invalid-section-uppercase", input: { cwd: "/tmp", sections: [["plan", "x"], ["Plan", "y"]] } },
	{ name: "invalid-section-preamble", input: { cwd: "/tmp", sections: [["preamble", "x"]] } },
	{ name: "invalid-section-js-key-order", input: { cwd: "/tmp", sections: [["bad name", "x"], ["7", "y"]] } },
	{ name: "invalid-section-empty-name", input: { cwd: "/tmp", sections: [["", "x"]] } },
	{ name: "invalid-section-empty-content", input: { cwd: "/tmp", sections: [["Bad", ""]] } },
	{ name: "skills-hidden-only", input: { selectedTools: ["read"], toolSnippets, cwd: "/proj", skills: [hiddenSkill] } },
	{ name: "empty-custom-prompt", input: { customPrompt: "", selectedTools: [], cwd: "/proj" } },
	{
		name: "windows-cwd-and-context-files",
		input: {
			selectedTools: ["bash", "powershell"],
			toolSnippets,
			cwd: "C:\\Users\\me\\proj",
			contextFiles: [agentsFile, { path: "C:\\Users\\me\\proj\\sub\\AGENTS.md", content: "nested\n" }],
		},
	},
];

const objectPrototypeNames = Object.getOwnPropertyNames(Object.prototype);
const diffs: Array<{ name: string; previous: Pairs; current: Pairs }> = [
	{ name: "patch-in-js-key-order", previous: [["b", "1"], ["10", "x"], ["a", "2"]], current: [["a", "2"], ["c", "3"]] },
	{ name: "unchanged", previous: [["a", "1"]], current: [["a", "1"]] },
	{ name: "previous-null-values", previous: [["a", null], ["b", null]], current: [["a", "x"]] },
	{ name: "current-null-value", previous: [["a", "1"]], current: [["a", null], ["b", null]] },
	{
		name: "object-prototype-names-are-never-removed",
		previous: [["constructor", "c"], ["toString", "t"], ["__proto__", "p"], ["hasOwnProperty", "h"], ["kept", "k"]],
		current: [],
	},
	{
		// Every name Object.prototype carries, as this node reports them.
		name: "every-object-prototype-name-is-never-removed",
		previous: [...objectPrototypeNames.map((name): [string, string] => [name, name]), ["plain", "x"]],
		current: [],
	},
	{ name: "object-prototype-name-added", previous: [], current: [["constructor", "c"], ["valueOf", "v"]] },
	{ name: "proto-key-never-patched", previous: [], current: [["__proto__", "p"], ["a", "1"]] },
];

const entries = (object: Record<string, string | null> | undefined) => (object ? Object.entries(object) : null);
const toOptions = (input: Input) => {
	const { sections, ...rest } = input;
	return sections ? { ...rest, sections: Object.fromEntries(sections) } : rest;
};

// attempt returns { [key]: value } or { [key + "Error"]: message }.
const attempt = (key: string, build: () => unknown) => {
	try {
		return { [key]: build() };
	} catch (error) {
		return { [`${key}Error`]: (error as Error).message };
	}
};

const out = {
	version,
	objectPrototypeNames,
	builds: builds.map(({ name, input }) => ({
		name,
		input,
		...attempt("prompt", () => buildSystemPrompt(toOptions(input))),
		...attempt("sections", () => entries(buildSystemPromptSections(toOptions(input)))),
		...attempt("state", () => {
			const { content, sections, ...rest } = buildSystemPromptState(toOptions(input));
			if (Object.keys(rest).length > 0) throw new Error(`unexpected state keys ${Object.keys(rest)}`);
			return { content, ...(sections === undefined ? {} : { sections: entries(sections) }) };
		}),
	})),
	diffs: diffs.map(({ name, previous, current }) => ({
		name,
		previous,
		current,
		patch: entries(diffSystemPromptSections(Object.fromEntries(previous), Object.fromEntries(current))),
	})),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
