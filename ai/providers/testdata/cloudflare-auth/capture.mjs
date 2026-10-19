// Captures how real pi resolves Cloudflare auth (providers/cloudflare-auth.ts:
// cloudflareWorkersAIAuth / cloudflareAIGatewayAuth, which builtinModels()
// gives the two cloudflare-* providers) and what that does to a request — the
// oracle behind TestCloudflareAuthMatchesPi.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0 cloudflare-auth-1.3.0.json
//
// Every row runs under one ambient environment (CLOUDFLARE_* in process.env,
// which pi's default auth context reads) and, for the Models rows, one stored
// credential. It records checkAuth and getAuth (bare, and with request
// overrides) for both providers, the api-key login's prompts and credential,
// and for one model per Cloudflare api: what Models.stream and compat's
// stream() do with each request option set — the request's URL and auth
// headers, or the setup error when nothing is sent. Requests never leave the
// process: globalThis.fetch answers 401, which no SDK retries.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const load = (file) => import(pathToFileURL(path.join(pkg, "dist", file)).href);
const { builtinModels } = await load("providers/all.js");
const { InMemoryCredentialStore } = await load("auth/credential-store.js");
const compat = await load("compat.js");

const CF = ["CLOUDFLARE_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"];
for (const name of Object.keys(process.env)) {
	if (name.startsWith("CLOUDFLARE_")) delete process.env[name];
}
function setEnv(env) {
	for (const name of CF) delete process.env[name];
	Object.assign(process.env, env);
}

// The only headers that carry Cloudflare auth.
const AUTH_HEADERS = ["authorization", "x-api-key", "cf-aig-authorization"];
let requests = [];
globalThis.fetch = async (input, init) => {
	const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
	const headers = new Headers(init?.headers ?? (input instanceof Request ? input.headers : undefined));
	const auth = {};
	for (const name of AUTH_HEADERS) {
		if (headers.has(name)) auth[name] = headers.get(name);
	}
	requests.push({ method: init?.method ?? "GET", url, headers: auth });
	return new Response(JSON.stringify({ error: { message: "captured", type: "authentication_error" } }), {
		status: 401,
		headers: { "content-type": "application/json" },
	});
};

const envs = {
	none: {},
	key: { CLOUDFLARE_API_KEY: "env-key" },
	"blank-key": { CLOUDFLARE_API_KEY: "  ", CLOUDFLARE_ACCOUNT_ID: "env-account", CLOUDFLARE_GATEWAY_ID: "env-gateway" },
	"key-account": { CLOUDFLARE_API_KEY: "env-key", CLOUDFLARE_ACCOUNT_ID: "env-account" },
	"key-account-gateway": {
		CLOUDFLARE_API_KEY: "env-key",
		CLOUDFLARE_ACCOUNT_ID: "env-account",
		CLOUDFLARE_GATEWAY_ID: "env-gateway",
	},
	"account-gateway": { CLOUDFLARE_ACCOUNT_ID: "env-account", CLOUDFLARE_GATEWAY_ID: "env-gateway" },
};

const credentials = {
	none: undefined,
	"key-only": { type: "api_key", key: "stored-key" },
	"key-account": { type: "api_key", key: "stored-key", env: { CLOUDFLARE_ACCOUNT_ID: "stored-account" } },
	"key-account-gateway": {
		type: "api_key",
		key: "stored-key",
		env: { CLOUDFLARE_ACCOUNT_ID: "stored-account", CLOUDFLARE_GATEWAY_ID: "stored-gateway" },
	},
	"empty-key": {
		type: "api_key",
		key: "",
		env: { CLOUDFLARE_ACCOUNT_ID: "stored-account", CLOUDFLARE_GATEWAY_ID: "stored-gateway" },
	},
	"empty-account": { type: "api_key", key: "stored-key", env: { CLOUDFLARE_ACCOUNT_ID: "" } },
};

const overrideSets = {
	none: undefined,
	apiKey: { apiKey: "request-key" },
	env: { env: { CLOUDFLARE_ACCOUNT_ID: "request-account", CLOUDFLARE_GATEWAY_ID: "request-gateway" } },
	"apiKey-env": {
		apiKey: "request-key",
		env: { CLOUDFLARE_ACCOUNT_ID: "request-account", CLOUDFLARE_GATEWAY_ID: "request-gateway" },
	},
};

const optionSets = {
	...overrideSets,
	"gateway-header": { headers: { "cf-aig-authorization": "Bearer request-header" } },
};

const models = [
	["cloudflare-workers-ai", "@cf/deepseek-ai/deepseek-v4-flash-0731"],
	["cloudflare-ai-gateway", "workers-ai/@cf/deepseek-ai/deepseek-v4-flash-0731"],
	["cloudflare-ai-gateway", "claude-haiku-4-5"],
	["cloudflare-ai-gateway", "gpt-4.1"],
];

function storeWith(credential) {
	const store = new InMemoryCredentialStore();
	return { store, ready: credential ? Promise.all(["cloudflare-workers-ai", "cloudflare-ai-gateway"].map((id) => store.modify(id, async () => structuredClone(credential)))) : Promise.resolve() };
}

async function outcome(stream) {
	requests = [];
	let result;
	try {
		for await (const _ of stream);
		result = await stream.result();
	} catch (error) {
		return { threw: String(error?.message ?? error) };
	}
	return {
		requests,
		stopReason: result.stopReason,
		// The 401's text is each SDK's own; only a setup error is pi's.
		errorMessage: requests.length === 0 ? (result.errorMessage ?? null) : null,
	};
}

function plain(value) {
	return value === undefined ? null : JSON.parse(JSON.stringify(value));
}

const out = { version, auth: [], login: [], stream: [] };
for (const [envName, env] of Object.entries(envs)) {
	setEnv(env);
	for (const [credName, credential] of Object.entries(credentials)) {
		const { store, ready } = storeWith(credential);
		await ready;
		const runtime = builtinModels({ credentials: store });
		for (const provider of ["cloudflare-workers-ai", "cloudflare-ai-gateway"]) {
			const row = { env: envName, credential: credName, provider };
			row.checkAuth = plain(await runtime.checkAuth(provider));
			row.getAuth = {};
			for (const [overrideName, overrides] of Object.entries(overrideSets)) {
				row.getAuth[overrideName] = plain(await runtime.getAuth(provider, overrides));
			}
			out.auth.push(row);
		}
		for (const [provider, modelId] of models) {
			const model = runtime.getModel(provider, modelId);
			if (!model) throw new Error(`${provider}/${modelId} is not in the ${version} catalog`);
			for (const [optionName, options] of Object.entries(optionSets)) {
				const row = { env: envName, credential: credName, provider, modelId, api: model.api, options: optionName, entry: "models" };
				out.stream.push({ ...row, ...(await outcome(runtime.stream(model, { messages: [] }, options))) });
				if (credName === "none") {
					// compat has no credential store: its Cloudflare branch is compatModels.
					out.stream.push({ ...row, entry: "compat", ...(await outcome(compat.stream(model, { messages: [] }, options))) });
				}
			}
		}
	}
}
setEnv({});

for (const provider of ["cloudflare-workers-ai", "cloudflare-ai-gateway"]) {
	const { store } = storeWith(undefined);
	const runtime = builtinModels({ credentials: store });
	const prompts = [];
	const answers = ["login-key", "login-account", "login-gateway"];
	const credential = await runtime.login(provider, "api_key", {
		prompt: async (prompt) => {
			prompts.push(prompt);
			return answers[prompts.length - 1];
		},
		notify: () => {},
	});
	out.login.push({
		provider,
		name: runtime.getProvider(provider).auth.apiKey.name,
		prompts,
		credential,
		stored: await store.read(provider),
	});
}

fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${out.auth.length} auth, ${out.login.length} login and ${out.stream.length} stream rows to ${outFile}`);
