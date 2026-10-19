// Captures pi's defaultModelPerProvider table — the oracle behind
// TestDefaultModelPerProviderMatchesPi in coding/resolve_test.go.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/0.87.1 default-models-0.87.1.json
//
//   node --experimental-strip-types capture.mjs --src <extraction> <sha> <pi npm dir> <out.json>
//   e.g. ... capture.mjs --src <dir> b313731b8 ~/.cache/pi-npm/0.87.1 default-models-b313731b8.json
//
// Re-capture it at every re-pin: upstream re-points entries in commits whose
// model-resolver.ts hunk sits among host-only changes, and a missed one only
// shows as a custom model id cloned from the wrong template. When the pin is a
// release, read the PUBLISHED build (dist/core/model-resolver.js) and name the
// file by its version. Between releases, read the source at the pin and name
// the file by its sha: <extraction> holds packages/coding-agent at <sha>
// (`git -C ~/.cache/pi-upstream archive <sha> packages/coding-agent | tar -x
// -C <extraction>`), and its imports resolve through the npm build's
// node_modules, which the capture links in when the extraction has none. The
// table is a literal, so those dependencies only have to load; one the pin
// needs and the build lacks fails the import rather than the table.
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
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-coding-agent");
let source;
let modulePath;
if (src) {
	const agent = path.join(extraction, "packages/coding-agent");
	const modules = path.join(agent, "node_modules");
	// 1.x installs hoist the package's dependencies into the build's own
	// node_modules (no shrinkwrap since 581e7ba78); older ones nest them.
	const nested = path.join(pkg, "node_modules");
	if (!fs.existsSync(modules)) fs.symlinkSync(fs.existsSync(nested) ? nested : path.join(npmDir, "node_modules"), modules);
	source = { sha };
	modulePath = path.join(agent, "src/core/model-resolver.ts");
} else {
	source = { "pi-coding-agent": JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version };
	modulePath = path.join(pkg, "dist/core/model-resolver.js");
}
const { defaultModelPerProvider } = await import(pathToFileURL(modulePath).href);
fs.writeFileSync(outFile, `${JSON.stringify({ ...source, defaultModelPerProvider }, null, "\t")}\n`);
console.log(`captured ${Object.keys(defaultModelPerProvider).length} defaults from ${modulePath} -> ${outFile}`);
