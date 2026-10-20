// Builds pisrc/<sha>/node_modules for the "src" backend.
//
//   node pi/srclink.mjs <srcDir> <npmNodeModules> <upstreamDir> <sha>
//
// <srcDir> holds upstream packages/ai at <sha> (run.sh extracts it). Its bare
// imports resolve through <srcDir>/node_modules, which this script makes a
// real directory: every package of the published build's node_modules is
// symlinked in, EXCEPT the @earendil-works workspace packages that pi-ai
// depends on at <sha> (packages/ai/package.json `dependencies`). Those come
// from the same sha, because src imports what the workspace has at that sha,
// not what the last release shipped — upstream's packages/ai/src/telemetry.ts
// imported NOOP_TELEMETRY_METRICS from @earendil-works/pi-telemetry before any
// release carried it, and the 1.3.0 build's copy made every src scenario fail
// at import.
//
// Each such package is extracted read-only (`git archive`) into
// <srcDir>/.workspace/<dir>/ and wrapped in a shim package whose `exports`
// map every `./dist/*.js` target of the real package.json to the matching
// `./src/*.ts`, so Node runs its TypeScript directly, like pi-ai's own src.
// Nothing here is cached: the directory is rebuilt every run (it is links and
// one small archive per package).

import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import path from "node:path";

const [srcDir, npmNodeModules, upstreamDir, sha] = process.argv.slice(2);
if (!srcDir || !npmNodeModules || !upstreamDir || !sha) {
	console.error("usage: node pi/srclink.mjs <srcDir> <npmNodeModules> <upstreamDir> <sha>");
	process.exit(2);
}
const SCOPE = "@earendil-works";

const git = (...args) => execFileSync("git", ["-C", upstreamDir, ...args], { maxBuffer: 1 << 28 });

// The workspace package names at <sha>, by directory.
const workspace = new Map();
for (const file of git("ls-tree", "--name-only", sha, "packages/").toString().split("\n").filter(Boolean)) {
	let manifest;
	try {
		manifest = JSON.parse(git("show", `${sha}:${file}/package.json`).toString());
	} catch {
		continue; // not a package
	}
	if (manifest.name) workspace.set(manifest.name, { dir: path.basename(file), manifest });
}

const aiManifest = JSON.parse(readFileSync(path.join(srcDir, "package.json"), "utf8"));
const fromWorkspace = Object.keys(aiManifest.dependencies ?? {}).filter((name) => workspace.has(name));

// Rewrite an exports target from the build to the source.
function toSource(target) {
	if (typeof target === "string") {
		return target.replace(/^\.\/dist\//, "./src/").replace(/\.js$/, ".ts").replace(/\.d\.ts$/, ".ts");
	}
	if (target && typeof target === "object") {
		// A conditional entry: run the source, whichever condition Node picks.
		const source = target.source ?? toSource(target.import ?? target.default);
		return source;
	}
	return target;
}

const nodeModules = path.join(srcDir, "node_modules");
rmSync(nodeModules, { recursive: true, force: true }); // a symlink in older runs: removes the link only
mkdirSync(path.join(nodeModules, SCOPE), { recursive: true });

for (const entry of readdirSync(npmNodeModules)) {
	if (entry === SCOPE) continue;
	symlinkSync(path.join(npmNodeModules, entry), path.join(nodeModules, entry));
}
const published = path.join(npmNodeModules, SCOPE);
for (const entry of existsSync(published) ? readdirSync(published) : []) {
	if (fromWorkspace.includes(`${SCOPE}/${entry}`)) continue;
	symlinkSync(path.join(published, entry), path.join(nodeModules, SCOPE, entry));
}

for (const name of fromWorkspace) {
	const { dir, manifest } = workspace.get(name);
	const extracted = path.join(srcDir, ".workspace", dir);
	rmSync(extracted, { recursive: true, force: true });
	mkdirSync(extracted, { recursive: true });
	execFileSync("tar", ["-x", "-C", extracted, "--strip-components=2"], {
		input: git("archive", sha, `packages/${dir}`),
	});
	const shim = path.join(nodeModules, name);
	mkdirSync(shim, { recursive: true });
	symlinkSync(path.join(extracted, "src"), path.join(shim, "src"));
	const exports = Object.fromEntries(Object.entries(manifest.exports ?? { ".": "./dist/index.js" }).map(([key, target]) => [key, toSource(target)]));
	writeFileSync(path.join(shim, "package.json"), `${JSON.stringify({ name, type: "module", exports }, null, "\t")}\n`);
	console.log(`src backend: ${name} from packages/${dir} @ ${sha}`);
}
