// Captures what real pi says when a built-in model's api has no
// implementation to stream through — the oracle behind
// TestUnportedApisFailAsPisDispatch.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs .../npm-1.3.0 unported-apis-1.3.0.json
//
// pi implements all ten built-in apis (compat.ts BUILTIN_APIS) and registers
// them when compat loads, so neither text below is reachable there for a
// built-in model as shipped; each row records the two states the port can be
// in, for one catalog model per api:
//
//   dispatch     — the model's built-in provider, rebuilt by createProvider
//                  with the same id, models and auth but without that api's
//                  implementation, streams the model: createProvider's
//                  dispatch error. compat reaches it when the api has neither
//                  a registry entry nor a recorded built-in (undefined ===
//                  undefined in getBuiltinProviderForModel), which is the
//                  port's state for an api it has not ported.
//   unregistered — compat streams the model after a host registered an
//                  implementation of the api under a source id and then
//                  unregistered that source, leaving no registry entry while
//                  the recorded built-in remains: resolveApiProvider's throw.
//
// No request leaves the process: neither path reaches an adapter.
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
const { createProvider } = await load("models.js");
const { builtinProviders } = await load("providers/all.js");
const compat = await load("compat.js");

const apis = [
	"anthropic-messages",
	"openai-completions",
	"openai-responses",
	"openai-codex-responses",
	"azure-openai-responses",
	"google-generative-ai",
	"google-vertex",
	"mistral-conversations",
	"bedrock-converse-stream",
	"pi-messages",
];
const unreached = () => {
	throw new Error("unreached");
};
const errorOf = async (run) => {
	try {
		return (await run().result()).errorMessage;
	} catch (error) {
		return error.message;
	}
};

const rows = [];
for (const api of apis) {
	const builtin = builtinProviders().find((provider) => provider.getModels().some((model) => model.api === api));
	const model = builtin.getModels().find((candidate) => candidate.api === api);
	const otherApi = apis.find((candidate) => candidate !== api);
	const lacking = createProvider({
		id: builtin.id,
		models: builtin.getModels(),
		auth: builtin.auth,
		api: { [otherApi]: { stream: unreached, streamSimple: unreached } },
	});
	const dispatch = await errorOf(() => lacking.streamSimple(model, { messages: [] }));
	compat.registerApiProvider({ api, stream: unreached, streamSimple: unreached }, "capture");
	compat.unregisterApiProviders("capture");
	const unregistered = await errorOf(() => compat.streamSimple(model, { messages: [] }));
	compat.resetApiProviders();
	rows.push({ api, provider: builtin.id, model: model.id, dispatch, unregistered });
}
fs.writeFileSync(outFile, `${JSON.stringify({ version, rows }, null, "\t")}\n`);
