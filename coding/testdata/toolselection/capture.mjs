// Captures how pi selects a session's tools from createAgentSession's tools,
// excludeTools and noTools options — the oracle behind
// coding/toolselection_test.go.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs <npm>/1.3.0 toolselection-1.3.0.json
//
// From the published build (pi-coding-agent 1.3.0, the first that ships both
// upstream 04b97ef00 and ddaa0a034) it records:
//   - matcher: createToolNameMatcher(entries)(name) over a matrix of entries
//     (exact names and `*` patterns) and names, including regex metacharacters
//     and JS line terminators, which `.` does not match;
//   - lists: getToolListError(entries) and applyToolModifiers(DEFAULT_TOOL_NAMES,
//     entries) for +name/-name lists;
//   - sessions: createAgentSession with an in-memory session and settings and
//     no extensions, the scenario's options and its custom tools (each a
//     no-op tool active by default); the active and registered tool names in
//     pi's order, or the message createAgentSession throws.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-coding-agent");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const dist = (file) => import(pathToFileURL(path.join(pkg, "dist", file)).href);
const sdk = await dist("index.js");
const { createToolNameMatcher } = await dist("core/mcp-servers.js");
const { applyToolModifiers, DEFAULT_TOOL_NAMES, getToolListError } = await dist("core/settings-manager.js");

const matcherEntries = [
	[],
	["read"],
	["*"],
	["re*"],
	["*_tool", "ask_*"],
	["a.b*"],
	["a*b*c"],
	["mcp__docs__s*"],
	["(x)|[y]*"],
	["a+b?*", "^$\\{}"],
	["**"],
	["a*b"],
	["read", "*sh*"],
];
const matcherNames = [
	"read",
	"bash",
	"powershell",
	"write",
	"",
	"dynamic_tool",
	"ask_question",
	"a.bc",
	"aXbc",
	"abc",
	"aXbYc",
	"mcp__docs__search",
	"mcp__docs__fail",
	"(x)|[y]",
	"(x)|[y]z",
	"x",
	"a+b?",
	"a+b?!",
	"aab",
	"^$\\{}",
	"a\nb",
	"a\rb",
	"a b",
	"a b",
	"a\tb",
	"a\u0085b",
];
const matcher = matcherEntries.map((entries) => {
	const matches = createToolNameMatcher(entries);
	return { entries, matched: matcherNames.filter((name) => matches(name)) };
});

const toolLists = [
	["read", "bash"],
	["+grep"],
	["+grep", "-write"],
	["-read", "-bash", "-edit", "-write"],
	["+read"],
	["+grep", "+grep", "-grep", "+grep"],
	["+", "-"],
	["-missing"],
	["read", "+grep"],
	["", "+grep"],
	["-gr*"],
	["+grep", "+mcp__radius__*"],
	["*"],
];
const lists = toolLists.map((entries) => ({
	entries,
	error: getToolListError(entries) ?? null,
	applied: applyToolModifiers(DEFAULT_TOOL_NAMES, entries),
}));

const sessions = [
	{ name: "default", customTools: ["ask_question", "dynamic_tool"] },
	{ name: "allow-star", tools: ["*"] },
	// regression #5109 "matches allowlist and denylist patterns".
	{
		name: "patterns-5109",
		tools: ["*_tool", "ask_*", "re*"],
		excludeTools: ["ask*"],
		customTools: ["ask_question", "dynamic_tool"],
	},
	{ name: "exclude-pattern", excludeTools: ["*sh*"], customTools: ["fish_tool", "plain"] },
	{ name: "pattern-metachars", tools: ["read", "a.b*"], customTools: ["a.bc", "aXbc", "a.b"] },
	{ name: "exclude-star", tools: ["*"], excludeTools: ["*"], customTools: ["dynamic_tool"] },
	{ name: "allow-empty", tools: [], customTools: ["dynamic_tool"] },
	{ name: "modifiers", tools: ["+grep", "-write"], customTools: ["dynamic_tool"] },
	{ name: "modifiers-add-custom", tools: ["+dynamic_tool", "-bash"], customTools: ["dynamic_tool", "other_tool"] },
	{ name: "modifiers-no-tools-all", tools: ["+grep", "-write"], noTools: "all", customTools: ["dynamic_tool"] },
	{ name: "modifiers-no-tools-builtin", tools: ["+grep"], noTools: "builtin", customTools: ["dynamic_tool"] },
	// Modifier lists that add nothing to an empty selection: the allowlist is
	// empty, not absent.
	{ name: "modifiers-no-tools-all-remove-only", tools: ["-write"], noTools: "all", customTools: ["dynamic_tool"] },
	{ name: "modifiers-no-tools-all-remove-missing", tools: ["-x"], noTools: "all", customTools: ["dynamic_tool"] },
	{ name: "modifiers-no-tools-all-empty-names", tools: ["+", "-"], noTools: "all", customTools: ["dynamic_tool"] },
	{ name: "modifiers-no-tools-builtin-remove-only", tools: ["-write"], noTools: "builtin", customTools: ["dynamic_tool"] },
	{ name: "modifiers-remove-all", tools: ["-read", "-bash", "-edit", "-write"], customTools: ["dynamic_tool"] },
	{ name: "modifiers-excluded", tools: ["+grep", "+ls"], excludeTools: ["g*"] },
	{ name: "modifiers-empty-names", tools: ["+", "-"] },
	{ name: "error-mixed", tools: ["read", "+grep"] },
	{ name: "error-mixed-empty-entry", tools: ["", "+grep"] },
	{ name: "error-pattern-modifier", tools: ["-gr*"] },
	{ name: "error-mixed-with-no-tools", tools: ["read", "-bash"], noTools: "all" },
];

const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "pi-toolselection-agent-"));
const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "pi-toolselection-cwd-"));
const customTool = (name) => ({
	name,
	label: name,
	description: `${name} tool`,
	parameters: { type: "object", properties: {} },
	execute: async () => ({ content: [{ type: "text", text: "ok" }], details: {} }),
});
const sessionRows = [];
for (const { name, customTools = [], ...options } of sessions) {
	const settingsManager = sdk.SettingsManager.inMemory();
	const resourceLoader = new sdk.DefaultResourceLoader({ cwd, agentDir, settingsManager, noExtensions: true });
	await resourceLoader.reload();
	try {
		const { session } = await sdk.createAgentSession({
			cwd,
			agentDir,
			settingsManager,
			sessionManager: sdk.SessionManager.inMemory(cwd),
			resourceLoader,
			customTools: customTools.map(customTool),
			...options,
		});
		sessionRows.push({
			name,
			options,
			customTools,
			active: session.getActiveToolNames(),
			registered: session.getAllTools().map((tool) => tool.name),
		});
		session.dispose();
	} catch (error) {
		sessionRows.push({ name, options, customTools, error: error.message });
	}
}
fs.rmSync(agentDir, { recursive: true, force: true });
fs.rmSync(cwd, { recursive: true, force: true });

fs.writeFileSync(outFile, `${JSON.stringify({ piCodingAgentVersion: version, matcherNames, matcher, lists, sessions: sessionRows }, null, "\t")}\n`);
console.error(`${matcher.length} matcher rows, ${lists.length} lists, ${sessionRows.length} sessions from pi-coding-agent ${version}`);
