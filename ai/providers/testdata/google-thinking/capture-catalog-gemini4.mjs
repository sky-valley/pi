// Captures the generationConfig.thinkingConfig pi sends for the shipped
// catalog's Gemini 4 models on google-generative-ai at every reasoning level —
// the oracle behind TestCatalogGemini4ThinkingConfigMatchesPi (upstream
// 1692bba63, #10890: Gemini 4 and later send thinkingLevel).
//
//   node capture-catalog-gemini4.mjs <pi-ai package dir> <out.json>
//   e.g. node capture-catalog-gemini4.mjs .../npm-1.3.0/node_modules/@earendil-works/pi-ai catalog-gemini4-1.3.0.json
//
// The package is the published npm build; its catalog supplies the models
// (getModel), and the body is read where @google/genai POSTs it
// (globalThis.fetch). Every google-generative-ai model whose id is Gemini 4 or
// later is captured. The catalog's google-vertex gemini-4-argon is left out:
// the port has no google-vertex adapter (Scope queue entry 5).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pkg, outFile] = process.argv.slice(2);
if (!pkg || !outFile) {
	console.error("usage: node capture-catalog-gemini4.mjs <pi-ai package dir> <out.json>");
	process.exit(2);
}
let body;
globalThis.fetch = async (_url, init) => {
	body ??= String(init?.body);
	throw new Error("body captured");
};
const pi = await import(pathToFileURL(path.join(pkg, "dist/compat.js")).href);
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;

const rows = [];
for (const provider of pi.getProviders()) {
	for (const model of pi.getModels(provider)) {
		if (model.api !== "google-generative-ai" || !/gemini-(?:[4-9]|[1-9]\d)/.test(model.id.toLowerCase())) continue;
		for (const reasoning of [undefined, "off", "minimal", "low", "medium", "high", "xhigh", "max"]) {
			body = undefined;
			const final = await pi
				.streamSimple(model, { messages: [{ role: "user", content: "Hello", timestamp: 0 }] }, { apiKey: "test", maxRetries: 0, reasoning })
				.result();
			if (body === undefined) throw new Error(`${provider}/${model.id} ${reasoning}: ${final.errorMessage}`);
			rows.push({ provider, id: model.id, reasoning: reasoning ?? null, thinkingConfig: JSON.parse(body).generationConfig?.thinkingConfig ?? null });
		}
	}
}
fs.writeFileSync(outFile, `${JSON.stringify({ piAi: version, rows }, null, "\t")}\n`);
console.log(`wrote ${outFile}: ${rows.length} rows`);
