// Captures String.prototype.toUpperCase for every BMP character it changes —
// the oracle behind TestJSToUpperMatchesNode in package ai, which pins
// jsToUpper (the first letter of NVIDIA's invoke origin, pi identity.ts).
//
//   node capture-to-upper.mjs <out.json>
//   e.g. node capture-to-upper.mjs to-upper-node-v26.4.0.json
//
// ai/identity_upper.go's jsUpperDiffs is the subset unicode.ToUpper gets
// wrong; when the test fails after a Node or Go upgrade, the rows it names
// are the entries to add to or drop from that table.
import fs from "node:fs";

const [outFile] = process.argv.slice(2);
if (!outFile) {
	console.error("usage: node capture-to-upper.mjs <out.json>");
	process.exit(2);
}
const rows = [];
for (let c = 0; c <= 0xffff; c++) {
	if (c >= 0xd800 && c <= 0xdfff) continue;
	const s = String.fromCharCode(c);
	const upper = s.toUpperCase();
	if (upper !== s) rows.push([c, upper]);
}
fs.writeFileSync(outFile, `${JSON.stringify({ node: process.version, rows })}\n`);
console.log(`captured ${rows.length} characters under ${process.version} -> ${outFile}`);
