// Captures the text pi's read tool puts with an image, by the model of the
// request — the oracle behind TestReadImageNoteFollowsTheModel in
// coding/toolresultimages_test.go.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs <npm>/1.3.0 readimagenote-1.3.0.json
//
// createReadToolDefinition(cwd).execute runs with an execution context whose
// model accepts images, accepts only text, or is absent, over a decodable PNG
// and a PNG header with no image data. Each row records the text block, the
// content block types and the structured content without its image data
// (the encoder's bytes are not what this pins).
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-coding-agent");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const { createReadToolDefinition } = await import(pathToFileURL(path.join(pkg, "dist/core/tools/read.js")).href);

// coding/imagedetect_test.go minimalPNG, and its first 33 bytes.
const minimalPNG = Buffer.from([
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00,
	0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00, 0x0a, 0x49,
	0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
	0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
]);
const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "pi-readimagenote-"));
fs.writeFileSync(path.join(cwd, "pixel.png"), minimalPNG);
fs.writeFileSync(path.join(cwd, "broken.png"), minimalPNG.subarray(0, 33));

const models = {
	vision: { id: "vision", provider: "p", api: "a", input: ["text", "image"] },
	"text-only": { id: "text-only", provider: "p", api: "a", input: ["text"] },
	none: undefined,
};
const tool = createReadToolDefinition(cwd);
const rows = [];
for (const file of ["pixel.png", "broken.png"]) {
	for (const [modelName, model] of Object.entries(models)) {
		const result = await tool.execute("id", { path: file }, undefined, undefined, model ? { cwd, model } : undefined);
		const structured = result.structuredContent;
		rows.push({
			file,
			model: modelName,
			modelInput: model?.input ?? null,
			text: result.content.find((block) => block.type === "text")?.text ?? null,
			blocks: result.content.map((block) => block.type),
			structured: typeof structured === "string" ? structured : { ...structured, data: typeof structured.data },
		});
	}
}
fs.rmSync(cwd, { recursive: true, force: true });
fs.writeFileSync(outFile, `${JSON.stringify({ piCodingAgentVersion: version, rows }, null, "\t")}\n`);
for (const row of rows) console.error(row.file, row.model, JSON.stringify(row.text));
