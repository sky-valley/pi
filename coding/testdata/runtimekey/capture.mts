// Captures where real pi's coding agent sends a Cloudflare request, and with
// which auth, when the key comes from the environment, a stored credential or
// a runtime API key (ModelRuntime.setRuntimeApiKey) — the oracle behind
// TestSessionRequestsResolveAuthAsPi in coding/runtime_key_test.go.
//
//   node capture.mts <npm-root> <out.json>
//   e.g. node capture.mts .../npm-1.3.0 runtimekey-1.3.0.json
//
// <npm-root> holds node_modules/@earendil-works/pi-coding-agent. Each
// scenario sets the CLOUDFLARE_* environment, builds a ModelRuntime over an
// in-memory credential store holding the scenario's stored credentials, sets
// its runtime API keys, and drives a real AgentSession (createAgentSession):
// one prompt per model in `models` (setModel switches to each model after the
// first), then, when `compact` is set, a manual compaction. pi's session
// streams every one of those requests through modelRuntime.streamSimple
// (sdk.ts), and a summarization passes the auth _getSummarizationRequestAuth
// resolved from the runtime. globalThis.fetch answers each request with one
// chat-completions stream and records its URL and the headers that carry
// Cloudflare auth; no request leaves the process (PI_OFFLINE is set, so the
// runtime fetches no catalog either).
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [root, outFile] = process.argv.slice(2);
if (!root || !outFile) {
	console.error("usage: node capture.mts <npm-root> <out.json>");
	process.exit(2);
}
process.env.PI_OFFLINE = "1";
const agentRoot = path.join(path.resolve(root), "node_modules/@earendil-works/pi-coding-agent");
const version = JSON.parse(fs.readFileSync(path.join(agentRoot, "package.json"), "utf8")).version;
const load = (file: string) => import(pathToFileURL(path.join(agentRoot, file)).href);
const { SessionManager, SettingsManager, ModelRuntime, createAgentSession } = await load("dist/index.js");
const { AuthStorage } = await load("dist/core/auth-storage.js");

const AUTH_HEADERS = ["authorization", "x-api-key", "cf-aig-authorization"];
let phase = "";
let requests: Array<{ phase: string; method: string; url: string; headers: Record<string, string> }> = [];
const chunk = (body: object) => `data: ${JSON.stringify({ id: "c", object: "chat.completion.chunk", created: 0, model: "m", ...body })}\n\n`;
globalThis.fetch = (async (input: any, init?: any) => {
	const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
	const headers = new Headers(init?.headers ?? (input instanceof Request ? input.headers : undefined));
	const auth: Record<string, string> = {};
	for (const name of AUTH_HEADERS) {
		if (headers.has(name)) auth[name] = headers.get(name)!;
	}
	requests.push({ phase, method: init?.method ?? "GET", url, headers: auth });
	const text = phase === "compaction" ? "## Goal\nsummary" : "ok";
	const body =
		chunk({ choices: [{ index: 0, delta: { role: "assistant", content: text }, finish_reason: null }] }) +
		chunk({
			choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
			usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 },
		}) +
		"data: [DONE]\n\n";
	return new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
}) as typeof fetch;

const workersAI = ["cloudflare-workers-ai", "@cf/meta/llama-3.3-70b-instruct-fp8-fast"];
const gateway = ["cloudflare-ai-gateway", "workers-ai/@cf/meta/llama-3.3-70b-instruct-fp8-fast"];
const ambient = { CLOUDFLARE_API_KEY: "env-key", CLOUDFLARE_ACCOUNT_ID: "env-account", CLOUDFLARE_GATEWAY_ID: "env-gateway" };
const storedAccount = { type: "api_key", key: "stored-key", env: { CLOUDFLARE_ACCOUNT_ID: "stored-account" } };

type Scenario = {
	name: string;
	env: Record<string, string>;
	stored?: Record<string, object>;
	runtimeKeys?: Record<string, string>;
	models: string[][];
	compact?: boolean;
};
const scenarios: Scenario[] = [
	// The reviewer's probe: key and account from the environment.
	{ name: "env", env: ambient, models: [workersAI], compact: true },
	{ name: "runtime-key", env: ambient, runtimeKeys: { "cloudflare-workers-ai": "runtime-key" }, models: [workersAI], compact: true },
	// A stored credential's key and account.
	{ name: "stored", env: ambient, stored: { "cloudflare-workers-ai": storedAccount }, models: [workersAI], compact: true },
	// A runtime key masks the whole stored credential, its account included.
	{
		name: "runtime-key-masks-stored",
		env: ambient,
		stored: { "cloudflare-workers-ai": storedAccount },
		runtimeKeys: { "cloudflare-workers-ai": "runtime-key" },
		models: [workersAI],
		compact: true,
	},
	// Runtime keys are per provider: setModel to the gateway uses its own.
	{
		name: "set-model",
		env: ambient,
		runtimeKeys: { "cloudflare-workers-ai": "workers-runtime-key", "cloudflare-ai-gateway": "gateway-runtime-key" },
		models: [workersAI, gateway],
		compact: true,
	},
];

const out = { version, scenarios: [] as unknown[] };
for (const scenario of scenarios) {
	for (const name of Object.keys(process.env)) {
		if (name.startsWith("CLOUDFLARE_")) delete process.env[name];
	}
	Object.assign(process.env, scenario.env);
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "capture-runtimekey-"));
	const credentials = AuthStorage.inMemory(scenario.stored ?? {});
	const runtime = await ModelRuntime.create({ credentials, modelsPath: null, allowModelNetwork: false });
	for (const [provider, key] of Object.entries(scenario.runtimeKeys ?? {})) {
		await runtime.setRuntimeApiKey(provider, key);
	}
	const [first, ...rest] = scenario.models.map(([provider, id]) => runtime.getModel(provider, id));
	requests = [];
	const { session } = await createAgentSession({
		cwd: dir,
		agentDir: dir,
		modelRuntime: runtime,
		model: first,
		sessionManager: SessionManager.inMemory(dir),
		settingsManager: SettingsManager.inMemory({ compaction: { enabled: false, reserveTokens: 100, keepRecentTokens: 1 } }),
		noTools: "all",
	});
	const errors: string[] = [];
	const prompt = async (text: string) => {
		phase = "prompt";
		await session.prompt(text);
		const last = session.messages[session.messages.length - 1];
		if (last?.role === "assistant" && last.errorMessage) errors.push(last.errorMessage);
	};
	await prompt("q1");
	for (const model of rest) {
		await session.setModel(model);
		await prompt("q2");
	}
	if (scenario.compact) {
		await prompt("q3");
		phase = "compaction";
		try {
			await session.compact();
		} catch (error) {
			errors.push(`compact: ${(error as Error).message}`);
		}
	}
	session.dispose?.();
	out.scenarios.push({ ...scenario, requests, errors });
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
