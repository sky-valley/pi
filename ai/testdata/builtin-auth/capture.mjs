// Captures the api-key auth pi's builtinModels() gives each built-in provider:
// its display name and the prompts its api-key login asks — the oracle behind
// TestBuiltinAPIKeyAuthNamesMatchPi.
//
//   env -i PATH="$PATH" HOME="$(mktemp -d)" node capture.mjs <pi npm dir> <out.json>
//
//   env -i PATH="$PATH" HOME="$(mktemp -d)" node --experimental-strip-types \
//     capture.mjs --src <extraction> <sha> <pi npm dir> <out.json>
//
// When the pin is a release, read the PUBLISHED build and name the file by its
// version. Between releases, read the source at the pin and name the file by
// its sha: <extraction> holds packages/ai at <sha> (`git -C ~/.cache/pi-upstream
// archive <sha> packages/ai | tar -x -C <extraction>`), and its imports resolve
// through the npm build's node_modules, which the capture links in when the
// extraction has none. src generates its model data (src/providers/data) at
// build time, so the capture links in the build's: the catalog is the build's
// either way, as the port's is a release's.
//
// Each login answers its prompts "x1", "x2", ... A login that cannot go on
// with such an answer (a select whose options name its methods) records the
// error it throws. Run it with an empty HOME, so no ambient credential file
// is read.
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
let source;
let load;
if (src) {
	const ai = path.join(extraction, "packages/ai");
	const modules = path.join(ai, "node_modules");
	if (!fs.existsSync(modules)) fs.symlinkSync(path.join(npmDir, "node_modules"), modules);
	const data = path.join(ai, "src/providers/data");
	if (!fs.existsSync(data)) fs.symlinkSync(path.join(pkg, "dist/providers/data"), data);
	source = { sha, catalog: `pi-ai ${version}` };
	load = (file) => import(pathToFileURL(path.join(ai, "src", file.replace(/\.js$/, ".ts"))).href);
} else {
	source = { version };
	load = (file) => import(pathToFileURL(path.join(pkg, "dist", file)).href);
}
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
fs.writeFileSync(outFile, `${JSON.stringify({ ...source, providers }, null, "\t")}\n`);
console.log(`wrote ${providers.length} providers to ${outFile}`);
