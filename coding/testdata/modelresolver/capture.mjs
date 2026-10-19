// Captures pi's model reference resolution — the oracle behind
// coding/resolve_reference_test.go.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs <npm>/1.3.0 modelresolver-1.3.0.json
//
// From the published build (pi-coding-agent 1.3.0, the first that ships
// upstream d7f7e0dfe) it records:
//   - references: resolveModelReference over model-resolver.test.ts's
//     reference catalog and cases (plus edge cases: targets of not-found
//     results, trimming, empty suffixes), each run over the catalog in order
//     and reversed, since catalog order must never decide;
//   - cli: resolveCliModel with a stub model runtime (getModels, and
//     hasConfiguredAuth from the case's configured providers) over the test
//     file's models and cases plus edge cases: the model or error, thinking
//     level and warning it returns.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-coding-agent");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const { resolveCliModel, resolveModelReference } = await import(
	pathToFileURL(path.join(pkg, "dist/core/model-resolver.js")).href
);

const model = (provider, id, extra = {}) => ({
	id,
	name: id,
	api: "openai-completions",
	provider,
	baseUrl: `https://${provider}.invalid/${id}`,
	reasoning: true,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 128000,
	maxTokens: 8192,
	...extra,
});

const referenceCatalog = [
	model("anthropic", "claude-sonnet-4-5"),
	model("anthropic", "claude-sonnet-4-5-20250929"),
	model("anthropic", "claude-opus-4-8"),
	model("anthropic", "exact"),
	model("anthropic", "shared-pro"),
	model("openai", "gpt-4o"),
	model("openrouter", "qwen/qwen3-coder:exacto"),
	model("openrouter", "openai/gpt-4o:extended"),
	model("gw-a", "shared"),
	model("gw-a", "anthropic/future"),
	model("gw-a", "anthropic/exact"),
	model("gw-a", "anthropic/claude-opus-4.8"),
	model("gw-a", "anthropic/claude-opus"),
	model("gw-b", "shared"),
	model("gw-b", "anthropic/future"),
	model("gw-b", "claude-opus-4-8"),
	model("gw-b", "gw-b/free"),
	model("gw-b", "zz:free"),
];

const referenceCases = [
	// model-resolver.test.ts referenceCases at d2e7d8b7c.
	{ name: "exact id", text: "claude-sonnet-4-5" },
	{ name: "exact id is case-insensitive", text: "GPT-4O" },
	{ name: "fuzzy prefers aliases over dated ids", text: "sonnet" },
	{ name: "thinking suffix", text: "sonnet:high" },
	{ name: "colon in a literal id is not a thinking suffix", text: "qwen/qwen3-coder:exacto" },
	{ name: "provider prefix, colon id, and thinking suffix", text: "openrouter/qwen/qwen3-coder:exacto:high" },
	{ name: "raw id wins when the provider prefix names no matching model", text: "openai/gpt-4o:extended" },
	{ name: "fuzzy searches the prefixed provider first", text: "openrouter/qwen" },
	{ name: "strict keeps an invalid suffix in the id", text: "gpt-4o:random" },
	{ name: "lenient drops an invalid suffix with a warning", text: "gpt-4o:random", thinkingSuffix: "lenient" },
	{
		name: "lenient invalid outer suffix discards the inner thinking level",
		text: "qwen/qwen3-coder:exacto:high:random",
		thinkingSuffix: "lenient",
	},
	{ name: "none keeps the suffix in the id", text: "gpt-4o:high", thinkingSuffix: "none" },
	{ name: "exact-only skips fuzzy matches", text: "sonnet", fuzzy: false },
	{ name: "empty text", text: "  " },
	{ name: "sole usable provider wins", text: "shared", usable: ["gw-b"] },
	{ name: "no usable provider is ambiguous instead of falling through to fuzzy", text: "shared" },
	{ name: "several usable providers are ambiguous", text: "shared", usable: ["gw-a", "gw-b"] },
	{ name: "thinking suffix does not change provider selection", text: "shared:high", usable: ["gw-b"] },
	{ name: "thinking suffix does not bypass ambiguity", text: "shared:high", usable: ["gw-a", "gw-b"] },
	{ name: "provider prefix without a direct model selects the usable raw id", text: "anthropic/future", usable: ["gw-b"] },
	{ name: "usable qualified match beats a usable raw id", text: "anthropic/exact", usable: ["anthropic", "gw-a"] },
	{ name: "usable raw id beats an unusable qualified match", text: "anthropic/exact", usable: ["gw-a"] },
	{ name: "unusable qualified match is kept when nothing is usable", text: "anthropic/exact" },
	{ name: "named provider's usable fuzzy match beats a raw id elsewhere", text: "anthropic/claude-opus", usable: ["anthropic"] },
	{ name: "usable raw id beats the named provider's unusable fuzzy match", text: "anthropic/claude-opus", usable: ["gw-a"] },
	{ name: "named provider's fuzzy match is kept when nothing is usable", text: "anthropic/claude-opus" },
	{ name: "named provider's literal id beats its fuzzy matches", text: "gw-b/free" },
	{ name: "fuzzy prefers usable providers", text: "opus", usable: ["gw-a"] },
	{ name: "fuzzy id tie across usable providers is ambiguous", text: "opus", usable: ["anthropic", "gw-a", "gw-b"] },
	{ name: "fuzzy id tie resolves to the sole usable provider", text: "opus", usable: ["anthropic"] },
	{ name: "explicit provider ignores usable providers elsewhere", text: "shared", provider: "gw-a", usable: ["gw-b"] },
	{ name: "explicit provider accepts its own prefix", text: "GW-B/shared", provider: "gw-b" },
	{ name: "explicit provider allows fuzzy matches", text: "4o", provider: "openai" },
	{ name: "explicit provider never leaves the provider", text: "gpt-4o", provider: "gw-a" },
	// Edge cases.
	{ name: "not-found target under a provider prefix", text: "anthropic/ghost" },
	{ name: "not-found target keeps a thinking suffix apart", text: "anthropic/ghost:high" },
	{ name: "not-found target keeps an invalid suffix in the id", text: "anthropic/ghost:banana" },
	{ name: "not-found target with suffix none", text: "anthropic/ghost:high", thinkingSuffix: "none" },
	{ name: "not-found target under an explicit provider", text: "ghost:low", provider: "OPENAI" },
	{ name: "not-found target under an explicit provider with its prefix", text: "openai/ghost", provider: "openai" },
	{ name: "not-found without a known prefix has no target", text: "nobody/ghost" },
	{ name: "an unknown explicit provider has no target", text: "ghost", provider: "nobody" },
	{ name: "provider prefix with no model text", text: "anthropic/" },
	{ name: "surrounding space is trimmed", text: "  anthropic/exact  " },
	{ name: "space around the prefix", text: "anthropic / exact" },
	{ name: "upper-case prefix", text: "ANTHROPIC/claude-opus-4-8" },
	{ name: "empty suffix", text: "gpt-4o:" },
	{ name: "lenient empty suffix", text: "gpt-4o:", thinkingSuffix: "lenient" },
	{ name: "colon id under its provider", text: "gw-b/zz:free" },
	{ name: "colon id with suffix", text: "zz:free:max" },
	{ name: "lenient double invalid suffix", text: "gpt-4o:a:b", thinkingSuffix: "lenient" },
	{ name: "lenient invalid suffix not found", text: "ghost:banana", thinkingSuffix: "lenient" },
	{ name: "fuzzy via name substring", text: "SONNET-4-5-2025" },
	{ name: "ambiguous exact with a usable named provider", text: "gw-a/shared", usable: ["gw-b"] },
	{ name: "fuzzy across providers with none usable", text: "anthropic/", fuzzy: true },
	{ name: "fuzzy match on several providers none usable", text: "future" },
	{ name: "fuzzy match on several providers one usable", text: "future", usable: ["gw-a"] },
	{ name: "thinking level suffix on an ambiguous lenient match", text: "shared:high", thinkingSuffix: "lenient" },
	{ name: "off suffix", text: "exact:off" },
];

const ref = (m) => `${m.provider}/${m.id}`;
const describeReference = (result) => {
	const out = { status: result.status };
	if (result.status === "resolved") out.model = ref(result.model);
	if (result.status === "ambiguous") {
		out.candidates = result.candidates.map(ref).sort();
		out.usableCount = result.usableCount;
	}
	if (result.thinkingLevel !== undefined) out.thinkingLevel = result.thinkingLevel;
	if (result.warning !== undefined) out.warning = result.warning;
	if (result.target) {
		out.target = {
			provider: result.target.provider,
			providerModels: result.target.providerModels.map(ref).sort(),
			modelText: result.target.modelText,
			modelId: result.target.modelId,
			...(result.target.thinkingLevel === undefined ? {} : { thinkingLevel: result.target.thinkingLevel }),
		};
	}
	return out;
};
const references = referenceCases.map((c) => {
	const usable = new Set(c.usable ?? []);
	const request = {
		text: c.text,
		provider: c.provider,
		thinkingSuffix: c.thinkingSuffix ?? "strict",
		fuzzy: c.fuzzy ?? true,
	};
	const results = [referenceCatalog, [...referenceCatalog].reverse()].map((models) =>
		describeReference(resolveModelReference(request, models, (provider) => usable.has(provider))),
	);
	if (JSON.stringify(results[0]) !== JSON.stringify(results[1])) {
		throw new Error(`${c.name}: catalog order decided ${JSON.stringify(results)}`);
	}
	return { ...c, request, result: results[0] };
});

const mockModels = [
	model("anthropic", "claude-sonnet-4-5", { name: "Claude Sonnet 4.5" }),
	model("openai", "gpt-4o", { name: "GPT-4o", reasoning: false }),
];
const openRouterModels = [
	model("openrouter", "qwen/qwen3-coder:exacto", { name: "Qwen3 Coder Exacto" }),
	model("openrouter", "openai/gpt-4o:extended", { name: "GPT-4o Extended", reasoning: false }),
];
const neuralwatt = model("neuralwatt", "some-base-model", { name: "Some Base Model", reasoning: false });
const catalogs = {
	all: [...mockModels, ...openRouterModels, neuralwatt],
	gateways: [
		model("anthropic", "claude-sonnet-4-5"),
		model("openrouter", "anthropic/claude-opus-5.5"),
		model("vercel-ai-gateway", "anthropic/claude-opus-5.5"),
	],
	defaults: [
		model("xai", "grok-4.6", { reasoning: false, contextWindow: 460000 }),
		model("xai", "grok-4.7", { reasoning: false, contextWindow: 470000 }),
		model("mistral", "codestral-latest", { reasoning: false }),
	],
	reference: referenceCatalog,
	empty: [],
};
const cliCases = [
	{ name: "no model", catalog: "all" },
	{ name: "invalid suffix under an explicit provider stays in the id", catalog: "all", cliProvider: "openai", cliModel: "gpt-4o:extended" },
	{ name: "explicit provider custom id without double prefixing", catalog: "all", cliProvider: "openrouter", cliModel: "openrouter/openai/ghost-model" },
	{ name: "no models", catalog: "empty", cliProvider: "openai", cliModel: "gpt-4o" },
	{ name: "authenticated gateway", catalog: "gateways", cliModel: "anthropic/claude-opus-5.5", configured: ["vercel-ai-gateway"] },
	{ name: "authenticated gateway with a thinking suffix", catalog: "gateways", cliModel: "anthropic/claude-opus-5.5:high", configured: ["vercel-ai-gateway"] },
	{ name: "ambiguous with no authenticated provider", catalog: "gateways", cliModel: "anthropic/claude-opus-5.5" },
	{ name: "ambiguous with several authenticated providers", catalog: "gateways", cliModel: "anthropic/claude-opus-5.5", configured: ["openrouter", "vercel-ai-gateway"] },
	{ name: "api key does not choose a provider", catalog: "gateways", cliModel: "anthropic/claude-opus-5.5", inlineApiKey: true, configured: ["vercel-ai-gateway"] },
	{ name: "custom id strips a thinking suffix", catalog: "all", cliModel: "neuralwatt/zai-org/GLM-5.1-FP8:high" },
	{ name: "custom id without a suffix", catalog: "all", cliModel: "neuralwatt/zai-org/GLM-5.1-FP8" },
	...["off", "minimal", "low", "medium", "high", "xhigh", "max"].map((level) => ({
		name: `custom id level ${level}`,
		catalog: "all",
		cliModel: `neuralwatt/zai-org/GLM-5.1-FP8:${level}`,
	})),
	{ name: "custom id keeps an invalid suffix", catalog: "all", cliModel: "neuralwatt/zai-org/GLM-5.1-FP8:banana" },
	{ name: "explicit provider custom id strips the suffix", catalog: "all", cliProvider: "neuralwatt", cliModel: "zai-org/GLM-5.1-FP8:high" },
	{ name: "explicit thinking keeps the suffix in the custom id", catalog: "all", cliModel: "neuralwatt/zai-org/GLM-5.1-FP8:high", cliThinking: "medium" },
	{ name: "explicit thinking off on a custom id", catalog: "all", cliModel: "neuralwatt/ghost", cliThinking: "off" },
	{ name: "explicit thinking high on a custom id", catalog: "all", cliModel: "neuralwatt/ghost", cliThinking: "high" },
	{ name: "unknown provider", catalog: "all", cliProvider: "nobody", cliModel: "gpt-4o" },
	{ name: "explicit provider is case-insensitive", catalog: "all", cliProvider: "OpenAI", cliModel: "gpt-4o" },
	{ name: "not found", catalog: "all", cliModel: "definitely-not-a-model" },
	{ name: "resolved with a thinking suffix", catalog: "all", cliModel: "sonnet:high" },
	{ name: "resolved colon id", catalog: "all", cliModel: "openai/gpt-4o:extended" },
	{ name: "fallback templated from the provider default", catalog: "defaults", cliModel: "xai/my-custom-grok" },
	{ name: "fallback with no default uses the provider's first model", catalog: "defaults", cliModel: "mistral/my-custom" },
	{ name: "fallback reasoning for a requested level", catalog: "defaults", cliModel: "mistral/my-custom:high" },
	{ name: "fallback keeps reasoning off for :off", catalog: "defaults", cliModel: "mistral/my-custom:off" },
	{ name: "reference catalog ambiguity", catalog: "reference", cliModel: "shared" },
	{ name: "reference catalog fuzzy tie", catalog: "reference", cliModel: "opus", configured: ["anthropic", "gw-b"] },
	{ name: "reference catalog fuzzy tie with api key", catalog: "reference", cliModel: "opus", inlineApiKey: true, configured: ["anthropic", "gw-b"] },
];
const describeModel = (m) =>
	m && { provider: m.provider, id: m.id, name: m.name, reasoning: m.reasoning, baseUrl: m.baseUrl, contextWindow: m.contextWindow };
const cli = cliCases.map(({ catalog, configured = [], ...c }) => {
	const runtime = { getModels: () => catalogs[catalog], hasConfiguredAuth: (provider) => configured.includes(provider) };
	const result = resolveCliModel({ ...c, modelRuntime: runtime });
	return {
		...c,
		catalog,
		configured,
		result: {
			model: describeModel(result.model) ?? null,
			...(result.thinkingLevel === undefined ? {} : { thinkingLevel: result.thinkingLevel }),
			...(result.warning === undefined ? {} : { warning: result.warning }),
			...(result.error === undefined ? {} : { error: result.error }),
		},
	};
});

fs.writeFileSync(
	outFile,
	`${JSON.stringify({ piCodingAgentVersion: version, catalogs, references, cli }, null, "\t")}\n`,
);
console.error(`${references.length} references, ${cli.length} cli cases from pi-coding-agent ${version}`);
