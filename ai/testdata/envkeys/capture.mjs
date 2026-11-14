// Captures pi's API-key env var table — the oracle behind
// TestAPIKeyEnvVarsMatchPi in package ai.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/1.4.0 env-keys-1.4.0.json
//
//   node --experimental-strip-types capture.mjs --src <extraction> <sha> <pi npm dir> <out.json>
//   e.g. ... capture.mjs --src <dir> 139707002 ~/.cache/pi-npm/1.5.2 env-keys-139707002.json
//
// When the pin is a release, read the PUBLISHED build and name the file by its
// version. Between releases, read the source at the pin and name the file by
// its sha: <extraction> holds packages/ai at <sha> (`git -C ~/.cache/pi-upstream
// archive <sha> packages/ai | tar -x -C <extraction>`), and its imports resolve
// through the npm build's node_modules, which the capture links in when the
// extraction has none. The catalog is the build's either way: src generates
// its model data at build time, and the port's catalog is a release's.
//
// getApiKeyEnvVars is module-private, so the table is read through the
// exported findEnvKeys with an env that has every variable set: it then
// returns the provider's whole list, in precedence order, or undefined for a
// provider with no API-key variables (the ambient-only ones). The providers
// are the build's catalog keys plus the ids pi has renamed away, so a rename
// that the port misses shows up on both sides of it.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const args = process.argv.slice(2);
const src = args[0] === "--src";
const [extraction, sha, npmDir, outFile] = src ? args.slice(1) : [undefined, undefined, ...args];
if (!npmDir || !outFile || (src && (!extraction || !sha))) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	console.error("       node --experimental-strip-types capture.mjs --src <extraction> <sha> <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const { MODELS } = await import(pathToFileURL(path.join(pkg, "dist/models.generated.js")).href);
let source;
let envApiKeys;
if (src) {
	const ai = path.join(extraction, "packages/ai");
	const modules = path.join(ai, "node_modules");
	if (!fs.existsSync(modules)) fs.symlinkSync(path.join(npmDir, "node_modules"), modules);
	source = { sha, catalog: `pi-ai ${version}` };
	envApiKeys = path.join(ai, "src/env-api-keys.ts");
} else {
	source = { "pi-ai": version };
	envApiKeys = path.join(pkg, "dist/env-api-keys.js");
}
const { findEnvKeys } = await import(pathToFileURL(envApiKeys).href);

// Renamed provider ids: azure-openai-responses became azure (a37306d43).
const renamed = ["azure-openai-responses"];
const everySet = new Proxy({}, { get: (_, name) => (typeof name === "string" ? "set" : undefined) });
const envKeys = {};
for (const provider of [...Object.keys(MODELS), ...renamed]) {
	envKeys[provider] = findEnvKeys(provider, everySet) ?? null;
}
fs.writeFileSync(outFile, `${JSON.stringify({ ...source, envKeys }, null, "\t")}\n`);
console.log(`captured ${Object.keys(envKeys).length} providers from ${src ? `src at ${sha}` : `pi-ai ${version}`} -> ${outFile}`);
