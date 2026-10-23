// Captures the identity headers real pi's adapters send — the oracle behind
// TestIdentityHeadersMatchPi in package providers.
//
//   node --experimental-strip-types capture-identity.mts <extraction> <out.json> <sha>
//   e.g. ... capture-identity.mts <dir> identity-9ffd27cfa.json 9ffd27cfa
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies (the
// npm build's). It is a src capture: the 1.4.0 build ships adc5fa9f8 and
// 04a93bad8, so re-verify it against that build when it can run these rows.
//
// Each row is a request, described as data so the Go test replays the same
// one: the api, the model's provider, base URL and headers, the request's api
// key, identity, headers and session id, and how it is sent — the api's own
// stream ("adapter"), or streamSimple through createModels, optionally with a
// Models identity and a withClientIdentity wrapper ("models"). globalThis.fetch
// records the headers each request carries and answers 400, which no adapter
// retries. Only the names identity and attribution touch are recorded, with
// the headers a row's overrides name; a detailed User-Agent's runtime slot
// is node's here, which the Go test swaps for the port's.
import path from "node:path";
import fs from "node:fs";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-identity.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { normalizeContext } = await load("utils/transcript.ts");
const { createModels, createProvider } = await load("models.ts");
const {
	withClientIdentity,
	PI_ATTRIBUTION,
	getAttributionHeaders,
	getOpenCodeSessionHeaders,
	mergeClientIdentity,
	formatUserAgent,
} = await load("utils/identity.ts");
const apis: Record<string, { stream: Function; streamSimple: Function }> = {
	"openai-completions": await load("api/openai-completions.ts"),
	"openai-responses": await load("api/openai-responses.ts"),
	"anthropic-messages": await load("api/anthropic-messages.ts"),
	"google-generative-ai": await load("api/google-generative-ai.ts"),
	"pi-messages": await load("api/pi-messages.ts"),
};

const RECORDED = [
	"user-agent",
	"http-referer",
	"x-openrouter-title",
	"x-openrouter-categories",
	"x-billing-invoke-origin",
	"x-opencode-client",
	"x-opencode-session",
	"x-app",
];

type Identity = Record<string, unknown> | undefined;
type Row = {
	name: string;
	api: string;
	provider: string;
	baseUrl: string;
	modelHeaders?: Record<string, string>;
	apiKey?: string;
	identity?: Identity;
	headers?: Record<string, string | null>;
	sessionId?: string;
	via?: "adapter" | "models";
	modelsIdentity?: Identity;
	wrapperIdentity?: Identity;
	env?: Record<string, string>;
};

const PI = { name: "pi", url: "https://pi.dev", categories: ["cli-agent"] };
const ACME = { name: "acme", url: "https://acme.example" };
const OPENROUTER = "https://openrouter.ai/api/v1";
const NVIDIA = "https://integrate.api.nvidia.com/v1";
const OPENCODE = "https://opencode.ai/zen/v1";
const OTHER = "https://api.example.test/v1";
const LOCAL = "http://127.0.0.1:9"; // a base URL no host test matches (google's rows)

// Every adapter: the defaults, each User-Agent mode, attribution on each target
// kind, null attribution, and the sources that outrank the identity.
const perAdapter = (api: string, baseUrls: { openrouter: string; nvidia: string; opencode: string; other: string }, apiKey = "key") => {
	const r = (name: string, row: Omit<Row, "name" | "api" | "apiKey"> & { apiKey?: string }): Row => ({
		name: `${api}: ${name}`,
		api,
		apiKey,
		...row,
	});
	return [
		r("no identity", { provider: "plain", baseUrl: baseUrls.other }),
		r("empty identity", { provider: "plain", baseUrl: baseUrls.other, identity: {} }),
		r("short", { provider: "plain", baseUrl: baseUrls.other, identity: { userAgent: "short", version: "9.9.9" } }),
		r("custom", { provider: "plain", baseUrl: baseUrls.other, identity: { userAgent: { custom: "acme/1.0" } } }),
		r("detailed version", { provider: "plain", baseUrl: baseUrls.other, identity: { userAgent: "detailed", version: "9.9.9" } }),
		r("no attribution by default", { provider: "openrouter", baseUrl: baseUrls.openrouter }),
		r("pi on openrouter", { provider: "openrouter", baseUrl: baseUrls.openrouter, identity: { attribution: PI } }),
		r("acme on openrouter, no categories", { provider: "openrouter", baseUrl: baseUrls.openrouter, identity: { attribution: ACME } }),
		r("acme on openrouter, empty categories", {
			provider: "openrouter",
			baseUrl: baseUrls.openrouter,
			identity: { attribution: { ...ACME, categories: [] } },
		}),
		r("acme on openrouter, two categories", {
			provider: "openrouter",
			baseUrl: baseUrls.openrouter,
			identity: { attribution: { ...ACME, categories: ["cli-agent", "cloud-agent"] } },
		}),
		r("null attribution", { provider: "openrouter", baseUrl: baseUrls.openrouter, identity: { attribution: null } }),
		r("pi on nvidia", { provider: "nvidia", baseUrl: baseUrls.nvidia, identity: { attribution: PI } }),
		r("nvidia capitalizes ß", { provider: "nvidia", baseUrl: baseUrls.nvidia, identity: { attribution: { name: "ßeta", url: "x" } } }),
		r("nvidia keeps a capital", { provider: "nvidia", baseUrl: baseUrls.nvidia, identity: { attribution: { name: "Über", url: "x" } } }),
		r("pi on opencode", { provider: "opencode", baseUrl: baseUrls.opencode, identity: { attribution: PI } }),
		r("pi on another provider", { provider: "plain", baseUrl: baseUrls.other, identity: { attribution: PI } }),
		r("pi on cloudflare", { provider: "cloudflare-workers-ai", baseUrl: baseUrls.other, identity: { attribution: PI } }),
		r("model User-Agent outranks the identity", {
			provider: "plain",
			baseUrl: baseUrls.other,
			modelHeaders: { "User-Agent": "model-agent/1.0" },
			identity: { userAgent: { custom: "acme" } },
		}),
		r("model attribution header outranks the identity", {
			provider: "openrouter",
			baseUrl: baseUrls.openrouter,
			modelHeaders: { "X-OpenRouter-Title": "model-title" },
			identity: { attribution: PI },
		}),
		r("request marker removes the User-Agent", { provider: "plain", baseUrl: baseUrls.other, headers: { "user-agent": null } }),
		r("request marker removes an attribution header", {
			provider: "openrouter",
			baseUrl: baseUrls.openrouter,
			identity: { attribution: PI, userAgent: "short" },
			headers: { "HTTP-Referer": null },
		}),
		r("request header outranks the identity", {
			provider: "openrouter",
			baseUrl: baseUrls.openrouter,
			identity: { attribution: PI },
			headers: { "X-OpenRouter-Title": "request-title", "User-Agent": "request-agent" },
		}),
	];
};

const hosted = { openrouter: OPENROUTER, nvidia: NVIDIA, opencode: OPENCODE, other: OTHER };
const local = { openrouter: LOCAL, nvidia: LOCAL, opencode: LOCAL, other: LOCAL };
const rows: Row[] = [
	...perAdapter("openai-completions", hosted),
	...perAdapter("openai-responses", hosted),
	...perAdapter("anthropic-messages", hosted, "sk-ant-api-test"),
	...perAdapter("pi-messages", hosted),
	...perAdapter("google-generative-ai", local),
	// Host-matched targets under custom provider names.
	{ name: "custom provider on openrouter's host", api: "openai-completions", provider: "my-router", baseUrl: OPENROUTER, apiKey: "key", identity: { attribution: PI } },
	{ name: "custom provider on nvidia's host", api: "openai-completions", provider: "custom-nim", baseUrl: NVIDIA, apiKey: "key", identity: { attribution: ACME } },
	{ name: "custom provider on opencode's host", api: "openai-completions", provider: "custom", baseUrl: OPENCODE, apiKey: "key", identity: { attribution: ACME } },
	{ name: "a subdomain is not nvidia's host", api: "openai-completions", provider: "custom", baseUrl: "https://x.integrate.api.nvidia.com/v1", apiKey: "key", identity: { attribution: PI } },
	{ name: "vercel gateway is not attributed", api: "openai-completions", provider: "vercel-ai-gateway", baseUrl: "https://ai-gateway.vercel.sh/v1", apiKey: "key", identity: { attribution: PI } },
	// Anthropic's other two client branches.
	{ name: "anthropic OAuth sends Claude Code's identity, not the client's", api: "anthropic-messages", provider: "anthropic", baseUrl: OTHER, apiKey: "sk-ant-oat-test", identity: { userAgent: { custom: "acme" }, attribution: PI } },
	{ name: "anthropic OAuth on openrouter is not attributed", api: "anthropic-messages", provider: "openrouter", baseUrl: OPENROUTER, apiKey: "sk-ant-oat-test", identity: { attribution: PI } },
	{ name: "anthropic copilot keeps the identity", api: "anthropic-messages", provider: "github-copilot", baseUrl: OTHER, apiKey: "sk-ant-oat-test", identity: { userAgent: "short" } },
	{ name: "copilot model User-Agent outranks the identity", api: "openai-completions", provider: "github-copilot", baseUrl: OTHER, apiKey: "key", modelHeaders: { "User-Agent": "GitHubCopilotChat/0.35.0" }, identity: { userAgent: { custom: "acme" } } },
	// The OAuth branch has no client User-Agent slot any more, so any spelling of
	// a request's User-Agent lands after Claude Code's and wins.
	...[
		["User-Agent", "custom-client"],
		["user-agent", "custom-client"],
		["USER-AGENT", "custom-client"],
		["User-Agent", null],
		["user-agent", null],
	].map(([name, value]): Row => ({
		name: `anthropic OAuth: request ${name} ${value === null ? "marker" : "value"}`,
		api: "anthropic-messages",
		provider: "anthropic",
		baseUrl: OTHER,
		apiKey: "sk-ant-oat-test",
		headers: { [name as string]: value },
	})),
	// ANTHROPIC_CUSTOM_HEADERS goes under the client's object, where the SDK
	// constructor spreads it.
	...[
		["sk-ant-api-test", "user-agent: env"],
		["sk-ant-api-test", "User-Agent: env"],
		["sk-ant-oat-test", "user-agent: env"],
		["sk-ant-oat-test", "User-Agent: env"],
		["sk-ant-api-test", "HTTP-Referer: env"],
	].map(([apiKey, env]): Row => ({
		name: `anthropic ${apiKey.includes("oat") ? "OAuth" : "api key"}: ANTHROPIC_CUSTOM_HEADERS ${env}`,
		api: "anthropic-messages",
		provider: "openrouter",
		baseUrl: OPENROUTER,
		apiKey,
		identity: { attribution: PI },
		env: { ANTHROPIC_CUSTOM_HEADERS: env },
	})),
	// pi-messages' fixed headers are part of its record now.
	{ name: "pi-messages request marker removes accept", api: "pi-messages", provider: "plain", baseUrl: OTHER, apiKey: "key", headers: { accept: null } },
	// Session headers: no adapter adds x-opencode-session itself.
	{ name: "adapter: opencode session is not the adapter's", api: "openai-completions", provider: "opencode", baseUrl: OPENCODE, apiKey: "key", sessionId: "session-1", identity: { attribution: PI } },
	// The Models layer: its identity, a wrapper's, the request's, and the session header.
	...["openai-completions", "anthropic-messages", "google-generative-ai", "pi-messages"].flatMap((api): Row[] => {
		const base = api === "google-generative-ai" ? LOCAL : OPENROUTER;
		const opencode = api === "google-generative-ai" ? LOCAL : OPENCODE;
		const key = api === "anthropic-messages" ? "sk-ant-api-test" : "key";
		return [
			{ name: `${api} models: default identity`, api, provider: "openrouter", baseUrl: base, apiKey: key, via: "models" },
			{ name: `${api} models: Models identity`, api, provider: "openrouter", baseUrl: base, apiKey: key, via: "models", modelsIdentity: { attribution: { name: "models", url: "https://acme.example" }, userAgent: "short" } },
			{ name: `${api} models: wrapper over Models`, api, provider: "openrouter", baseUrl: base, apiKey: key, via: "models", modelsIdentity: { attribution: { name: "models", url: "https://acme.example" }, userAgent: "short" }, wrapperIdentity: { attribution: { name: "wrapper", url: "https://acme.example" } } },
			{ name: `${api} models: request over wrapper`, api, provider: "openrouter", baseUrl: base, apiKey: key, via: "models", modelsIdentity: { userAgent: "short" }, wrapperIdentity: { attribution: PI }, identity: { attribution: null, version: "2.0.0" } },
			{ name: `${api} models: request version over Models`, api, provider: "plain", baseUrl: base, apiKey: key, via: "models", modelsIdentity: { version: "1.0.0" }, identity: { userAgent: "detailed", version: "2.0.0" } },
			{ name: `${api} models: opencode session`, api, provider: "custom-opencode", baseUrl: opencode, apiKey: key, via: "models", sessionId: "session-1" },
			{ name: `${api} models: opencode session by provider name`, api, provider: "opencode", baseUrl: base, apiKey: key, via: "models", sessionId: "session-1" },
			{ name: `${api} models: model header outranks the session`, api, provider: "opencode", baseUrl: opencode, apiKey: key, via: "models", sessionId: "session-1", modelHeaders: { "X-OpenCode-Session": "model" } },
			{ name: `${api} models: request header outranks the session`, api, provider: "opencode", baseUrl: opencode, apiKey: key, via: "models", sessionId: "session-1", headers: { "X-OpenCode-Session": "request" } },
			{ name: `${api} models: request marker removes the session`, api, provider: "opencode", baseUrl: opencode, apiKey: key, via: "models", sessionId: "session-1", headers: { "x-opencode-session": null } },
			{ name: `${api} models: no session on another host`, api, provider: "plain", baseUrl: base, apiKey: key, via: "models", sessionId: "session-1" },
			{ name: `${api} models: opencode attribution with session`, api, provider: "opencode", baseUrl: opencode, apiKey: key, via: "models", sessionId: "session-1", identity: { attribution: PI } },
		];
	}),
];

const model = (row: Row) => ({
	id: "test-model",
	name: "Test Model",
	api: row.api,
	provider: row.provider,
	baseUrl: row.baseUrl,
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 128000,
	maxTokens: 4096,
	...(row.modelHeaders ? { headers: row.modelHeaders } : {}),
});

const context = { messages: [{ role: "user", content: "hello", timestamp: 1 }] };

async function send(row: Row): Promise<Record<string, string>> {
	let captured: Headers | undefined;
	const realFetch = globalThis.fetch;
	const savedEnv: Record<string, string | undefined> = {};
	for (const [name, value] of Object.entries(row.env ?? {})) {
		savedEnv[name] = process.env[name];
		process.env[name] = value;
	}
	globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
		captured = new Request(input, init).headers;
		return new Response(JSON.stringify({ error: { message: "captured" } }), {
			status: 400,
			headers: { "content-type": "application/json" },
		});
	}) as typeof fetch;
	try {
		const m = model(row);
		const options: Record<string, unknown> = {};
		if (row.identity !== undefined) options.identity = row.identity;
		if (row.headers) options.headers = row.headers;
		if (row.sessionId) options.sessionId = row.sessionId;
		const api = apis[row.api];
		if (row.via === "models") {
			const models = createModels(row.modelsIdentity ? { identity: row.modelsIdentity } : undefined);
			models.setProvider(
				createProvider({
					id: row.provider,
					auth: { apiKey: { name: "Test", resolve: async () => ({ auth: { apiKey: row.apiKey } }) } },
					models: [m],
					api: { stream: api.stream, streamSimple: api.streamSimple },
				}),
			);
			const target = row.wrapperIdentity ? withClientIdentity(models, row.wrapperIdentity) : models;
			await target.streamSimple(m, context, options).result();
		} else {
			await api.stream(m, normalizeContext(context), { apiKey: row.apiKey, ...options }).result();
		}
	} finally {
		globalThis.fetch = realFetch;
		for (const [name, value] of Object.entries(savedEnv)) {
			if (value === undefined) delete process.env[name];
			else process.env[name] = value;
		}
	}
	if (!captured) throw new Error(`${row.name}: no request was sent`);
	const names = new Set([...RECORDED, ...Object.keys(row.headers ?? {}).map((n) => n.toLowerCase())]);
	const out: Record<string, string> = {};
	for (const name of [...names].sort()) {
		const value = captured.get(name);
		if (value !== null) out[name] = value;
	}
	return out;
}

// The functions themselves, on targets no request can reach (a base URL that
// is not a URL) and on the merge's edge cases.
const targets = [
	{ provider: "custom", baseUrl: "not-a-url-openrouter.ai" },
	{ provider: "custom", baseUrl: "not a url opencode.ai" },
	{ provider: "custom", baseUrl: "https://OPENCODE.AI/zen" },
	{ provider: "custom", baseUrl: "https://opencode.ai:8443/zen" },
	{ provider: "custom", baseUrl: "https://user@integrate.api.nvidia.com/v1" },
	{ provider: "custom", baseUrl: "HTTPS://INTEGRATE.API.NVIDIA.COM/v1" },
	{ provider: "custom", baseUrl: "foo://OPENCODE.AI/x" },
	{ provider: "custom", baseUrl: "foo://opencode.ai/x" },
	{ provider: "custom", baseUrl: "https://opencode.ai./zen" },
	{ provider: "custom", baseUrl: "integrate.api.nvidia.com/v1" },
	{ provider: "opencode-go", baseUrl: "" },
	{ provider: "", baseUrl: "" },
];
const identities: Identity[] = [
	undefined,
	{ attribution: PI },
	{ attribution: { name: "", url: "" } },
	{ attribution: { name: "a", url: "u", categories: [""] } },
];
const merges: Identity[][] = [
	[],
	[undefined, undefined],
	[{}],
	[{ attribution: PI, userAgent: "short" }, { attribution: undefined, version: "1.0.0" }],
	[{ attribution: PI }, { attribution: null }],
	[{ attribution: null }, { attribution: PI }],
	[{ userAgent: { custom: "a" } }, { userAgent: "detailed" }, undefined],
	[{ version: "1.0.0" }, { version: "" }],
];
const functions = {
	attribution: targets.flatMap((target) =>
		identities.map((identity) => ({ target, identity, headers: getAttributionHeaders(target, identity) })),
	),
	session: targets.map((target) => ({ target, headers: getOpenCodeSessionHeaders(target, "s") ?? null })),
	merge: merges.map((sources) => {
		const merged = mergeClientIdentity(...sources);
		return { sources, merged: merged === undefined ? "undefined" : merged };
	}),
	userAgent: [
		{ identity: { userAgent: { custom: "" } }, value: formatUserAgent({ userAgent: { custom: "" } }) },
		{ identity: { version: "" }, value: formatUserAgent({ version: "" }) },
	],
};

const out = {
	sha,
	runtime: `node/${process.version}`,
	piAttribution: PI_ATTRIBUTION,
	functions,
	rows: [] as (Row & { sent: Record<string, string> })[],
};
for (const row of rows) out.rows.push({ ...row, sent: await send(row) });
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`captured ${out.rows.length} requests at ${sha} -> ${outFile}`);
