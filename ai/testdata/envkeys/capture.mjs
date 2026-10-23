// Captures pi's API-key env var table — the oracle behind
// TestAPIKeyEnvVarsMatchPi in package ai.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/1.4.0 env-keys-1.4.0.json
//
// getApiKeyEnvVars is module-private, so the table is read through the
// exported findEnvKeys with an env that has every variable set: it then
// returns the provider's whole list, in precedence order, or undefined for a
// provider with no API-key variables (the ambient-only ones). The providers
// are the published catalog's keys plus the ids pi has renamed away, so a
// rename that the port misses shows up on both sides of it.
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
const { MODELS } = await load("models.generated.js");
const { findEnvKeys } = await load("env-api-keys.js");

// Renamed provider ids: azure-openai-responses became azure (a37306d43).
const renamed = ["azure-openai-responses"];
const everySet = new Proxy({}, { get: (_, name) => (typeof name === "string" ? "set" : undefined) });
const envKeys = {};
for (const provider of [...Object.keys(MODELS), ...renamed]) {
	envKeys[provider] = findEnvKeys(provider, everySet) ?? null;
}
fs.writeFileSync(outFile, `${JSON.stringify({ "pi-ai": version, envKeys }, null, "\t")}\n`);
console.log(`captured ${Object.keys(envKeys).length} providers from pi-ai ${version} -> ${outFile}`);
