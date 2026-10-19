// Captures what pi sends for azure models on openai-completions (upstream
// a37306d43: providers/azure.ts's azureStreams resolves the endpoint with
// api/azure-openai-config.ts — AZURE_OPENAI_BASE_URL, then
// AZURE_OPENAI_RESOURCE_NAME, then model.baseUrl, normalized to /openai/v1 on
// Azure hosts — and swaps the request's model for the deployment name in
// AZURE_OPENAI_DEPLOYMENT_NAME_MAP) — the oracle behind
// TestAzureChatCompletionsMatchesPi.
//
//   node capture.mjs <pi-ai package dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai azure-completions-1.3.0.json
//
// The package is the published npm build. Rows go through compat.js's
// stream()/streamSimple(), as a caller of the package does: the catalog's
// azure/deepseek-v4-pro and an azure model registered with a baseUrl (pi's
// docs/providers.md "Azure OpenAI"). The environment is passed as
// options.env (the process environment's AZURE_OPENAI_* are cleared). The
// request is read where the openai SDK fetches it (globalThis.fetch), which
// answers 400 so nothing streams. Each row records the request's URL,
// Authorization and body, what a caller's onPayload saw as the model, and
// the result's model, stopReason and errorMessage.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
for (const name of Object.keys(process.env)) if (name.startsWith("AZURE_")) delete process.env[name];
let request;
globalThis.fetch = async (url, init) => {
	request ??= { url: String(url), authorization: new Headers(init?.headers).get("authorization"), body: JSON.parse(String(init?.body)) };
	return new Response('{"error":{"message":"stop here"}}', { status: 400, headers: { "content-type": "application/json" } });
};
const pi = await import(pathToFileURL(path.join(pkg, "dist/compat.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const catalog = pi.getModel("azure", "deepseek-v4-pro");
// pi's docs/providers.md example, as models.json's provider-level baseUrl
// lands on the model.
const registered = {
	id: "your-deployment",
	name: "your-deployment",
	api: "openai-completions",
	provider: "azure",
	baseUrl: "https://your-resource.services.ai.azure.com",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 128000,
	maxTokens: 16384,
};

const rows = [
	{ name: "catalog model, no endpoint configured", model: "catalog", env: {} },
	{ name: "catalog model, no endpoint and no key", model: "catalog", env: {}, noKey: true },
	{ name: "catalog model, no key", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com" }, noKey: true },
	{ name: "base URL: resource root", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com" } },
	{ name: "base URL: simple stream at high", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com" }, simple: true, reasoning: "high" },
	{ name: "base URL: /openai", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://r.openai.azure.com/openai/" } },
	{ name: "base URL: /openai/v1/responses with a query", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://r.cognitiveservices.azure.com/openai/v1/responses?api-version=preview" } },
	{ name: "base URL: another Azure path is kept", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://r.openai.azure.com/openai/deployments/x" } },
	{ name: "base URL: padded, upper case, trailing slashes", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "  https://R.OpenAI.Azure.com//  " } },
	{ name: "base URL: not an Azure host", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://proxy.example.com/v1/" } },
	{ name: "base URL: not a URL", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "not a url" } },
	{ name: "resource name", model: "catalog", env: { AZURE_OPENAI_RESOURCE_NAME: "my-res" } },
	{ name: "base URL wins over resource name", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://b.ai.azure.com", AZURE_OPENAI_RESOURCE_NAME: "my-res" } },
	{ name: "blank base URL falls to resource name", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "   ", AZURE_OPENAI_RESOURCE_NAME: "my-res" } },
	{ name: "deployment map", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com", AZURE_OPENAI_DEPLOYMENT_NAME_MAP: "deepseek-v4-pro=my-deepseek" } },
	{ name: "deployment map: simple stream", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com", AZURE_OPENAI_DEPLOYMENT_NAME_MAP: "deepseek-v4-pro=my-deepseek" }, simple: true },
	{
		name: "deployment map: split at most twice, trimmed, the last entry wins",
		model: "catalog",
		env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com", AZURE_OPENAI_DEPLOYMENT_NAME_MAP: " a=b ,, =z, w=, deepseek-v4-pro=first, deepseek-v4-pro = dep-x=y " },
	},
	{ name: "deployment map: a blank deployment keeps the id", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com", AZURE_OPENAI_DEPLOYMENT_NAME_MAP: "deepseek-v4-pro= " } },
	{ name: "deployment map: through a caller's onPayload", model: "catalog", env: { AZURE_OPENAI_BASE_URL: "https://my-resource.services.ai.azure.com", AZURE_OPENAI_DEPLOYMENT_NAME_MAP: "deepseek-v4-pro=my-deepseek" }, onPayload: true },
	{ name: "registered model with a baseUrl", model: "registered", env: {} },
	{ name: "registered model: the environment wins", model: "registered", env: { AZURE_OPENAI_RESOURCE_NAME: "my-res" } },
	{ name: "registered model: simple stream", model: "registered", env: {}, simple: true },
];

const out = [];
for (const row of rows) {
	request = undefined;
	let seenModel = null;
	const model = row.model === "catalog" ? catalog : registered;
	const options = {
		...(row.noKey ? {} : { apiKey: "azure-key" }),
		env: row.env,
		maxRetries: 0,
		...(row.reasoning ? { reasoning: row.reasoning } : {}),
		...(row.onPayload
			? {
					onPayload: (payload) => {
						seenModel = payload.model;
						return { ...payload, temperature: 0.1 };
					},
				}
			: {}),
	};
	const context = { systemPrompt: "sys", messages: [{ role: "user", content: "hi", timestamp: 0 }] };
	const result = await (row.simple ? pi.streamSimple : pi.stream)(model, context, options).result();
	out.push({
		...row,
		request: request ?? null,
		seenModel,
		result: { model: result.model, stopReason: result.stopReason, errorMessage: result.errorMessage ?? null },
	});
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, registered, rows: out }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${out.length} rows`);
