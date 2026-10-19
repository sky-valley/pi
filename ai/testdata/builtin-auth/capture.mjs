// Captures the api-key auth pi's builtinModels() gives each built-in provider:
// its display name and the prompts its api-key login asks — the oracle behind
// TestBuiltinAPIKeyAuthNamesMatchPi.
//
//   env -i PATH="$PATH" HOME="$(mktemp -d)" node capture.mjs <pi npm dir> <out.json>
//
// Each login answers its prompts "x1", "x2", ... A login that cannot go on
// with such an answer (a select whose options name its methods) records the
// error it throws. Run it with an empty HOME, so no ambient credential file
// is read.
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

const providers = [];
for (const { id } of builtinModels().getProviders()) {
	const runtime = builtinModels({ credentials: new InMemoryCredentialStore() });
	const apiKey = runtime.getProvider(id).auth.apiKey;
	const row = { id, apiKeyName: apiKey?.name ?? null, prompts: null, loginError: null };
	if (apiKey?.login) {
		row.prompts = [];
		try {
			await runtime.login(id, "api_key", {
				prompt: async ({ type, message }) => {
					row.prompts.push({ type, message });
					return `x${row.prompts.length}`;
				},
				notify: () => {},
			});
		} catch (error) {
			row.loginError = error.message;
		}
	}
	providers.push(row);
}
fs.writeFileSync(outFile, `${JSON.stringify({ version, providers }, null, "\t")}\n`);
console.log(`wrote ${providers.length} providers to ${outFile}`);
