// Derives a cause-detail overlay for one of this package's captures: the
// errorMessage pi gives each row once upstream 4c992154d (withCauseDetail,
// #11783) appends the deepest cause of a connection error to it.
//
//   node --experimental-strip-types cause-detail.mts <base.json> <recapture.json> <out.json>
//
// <base.json> is the capture the replay test reads; <recapture.json> is the
// same capture.mts rerun against an extraction of 4c992154d. The two may
// differ elsewhere — a recapture on another node reads and decodes the wire
// differently, and upstream moved on between the shas — so only an
// errorMessage the recapture extends with ": <cause>" is taken; any other
// errorMessage change is listed on stderr and left out. The rows must line
// up: the same arrays, of the same lengths, naming the same rows. The overlay
// maps each row's JSON pointer (e.g. "/runs/12") to pi's errorMessage there.
import fs from "node:fs";
import path from "node:path";

const [baseFile, recaptureFile, outFile] = process.argv.slice(2);
if (!baseFile || !recaptureFile || !outFile) {
	console.error("usage: node --experimental-strip-types cause-detail.mts <base.json> <recapture.json> <out.json>");
	process.exit(2);
}
const base = JSON.parse(fs.readFileSync(baseFile, "utf8"));
const recapture = JSON.parse(fs.readFileSync(recaptureFile, "utf8"));

const identity = (row: Record<string, unknown>) => ["api", "mode", "name", "label", "baseUrl"].map((k) => row[k]).join("|");
const errorMessages: Record<string, string> = {};
const walk = (a: unknown, b: unknown, pointer: string) => {
	if (Array.isArray(a)) {
		// Only an array of rows has to line up; a row's own lists (its events,
		// say) are no rows.
		if (!a.some((row) => row && typeof row === "object" && !Array.isArray(row))) return;
		if (!Array.isArray(b) || a.length !== b.length) throw new Error(`${pointer}: the arrays differ in length`);
		a.forEach((row, i) => {
			if (row && typeof row === "object" && !Array.isArray(row) && identity(row) !== identity(b[i])) {
				throw new Error(`${pointer}/${i}: ${identity(row)} is ${identity(b[i])} in the recapture`);
			}
			walk(row, b[i], `${pointer}/${i}`);
		});
		return;
	}
	if (!a || typeof a !== "object" || !b || typeof b !== "object") return;
	for (const [key, value] of Object.entries(a)) {
		const other = (b as Record<string, unknown>)[key];
		if (key === "errorMessage" && typeof value === "string" && typeof other === "string" && value !== other) {
			if (other.startsWith(`${value}: `)) errorMessages[pointer] = other;
			else console.error(`${pointer}: ${JSON.stringify(value)} became ${JSON.stringify(other)}; left out`);
			continue;
		}
		walk(value, other, `${pointer}/${key}`);
	}
};
walk(base, recapture, "");

fs.writeFileSync(
	outFile,
	`${JSON.stringify(
		{
			source: `upstream 4c992154d: ${path.basename(recaptureFile)} (${recapture.source}) over ${path.basename(baseFile)}`,
			errorMessages,
		},
		null,
		"\t",
	)}\n`,
);
console.log(`${Object.keys(errorMessages).length} errorMessages -> ${outFile}`);
