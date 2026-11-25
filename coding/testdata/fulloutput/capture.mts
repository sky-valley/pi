// Captures what pi's OutputAccumulator makes of shell output — the display
// snapshot the model sees and readFullOutput, the up-to-1-MiB copy a script
// gets — the oracle behind TestOutputAccumulatorSnapshotMatchesPi and
// TestOutputAccumulatorReadFullOutputMatchesPi in package coding.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> fulloutput-8209929fb.json 8209929fb
//
// fulloutput-8209929fb.json is src at 8209929fb: three unsaved rows' `full`
// move (readFullOutput's fallback keeps the retained text within readMax and
// marks only missing bytes). The script at the parent 943a99e3c writes the
// earlier fulloutput-943a99e3c.json byte for byte, whose first 18 rows are
// fulloutput-3dd803d7e.json's but for `file`; at 713563870 the script writes
// those 18 rows byte for byte (and no unsaved rows).
//
// <extraction> holds packages/coding-agent at <sha> (`git archive <sha>
// packages/coding-agent/src/core/tools`); output-accumulator.ts imports only
// node builtins and truncate.ts, which imports nothing.
//
// Each row appends its chunks (hex), notes whether the accumulator has opened
// a temp file yet, finishes, takes the bash tool's snapshot
// (persistIfTruncated), closes the temp file and reads the full output with
// readMax, as bash.ts does with 1 MiB. The rows use small limits so a few
// bytes cross them, and cover a byte-order mark, characters cut by a chunk or
// by the head/tail split, invalid bytes, and odd limits.
//
// Rows marked `unsaved` point TMPDIR at a missing directory, so the temp file
// fails to open and readFullOutput falls back to the retained tail (upstream
// 943a99e3c). They are captured only from an accumulator that reports its
// file as `fullOutputFile` (943a99e3c and later); before that a failed open
// was an unhandled stream error. `file` is the file's kind after
// closeTempFile: "saved", "failed", or null for no file. The error text is
// Node's and is not recorded.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const { OutputAccumulator } = await import(
	pathToFileURL(path.join(extraction, "packages/coding-agent/src/core/tools/output-accumulator.ts")).href
);

type Row = { name: string; chunks: string[]; maxLines?: number; maxBytes?: number; readMax: number; unsaved?: boolean };
const MiB = 1024 * 1024;
const ascii = (s: string) => Buffer.from(s, "latin1").toString("hex");
const rows: Row[] = [
	{ name: "small output, no temp file", chunks: [ascii("hello\n"), ascii("world")], readMax: MiB },
	{ name: "no output", chunks: [], readMax: MiB },
	{ name: "a byte-order mark, no temp file", chunks: ["efbbbf" + ascii("a\n")], readMax: MiB },
	{ name: "invalid bytes, no temp file", chunks: ["61ff62e282"], readMax: MiB },
	{ name: "a character split across chunks", chunks: ["61e2", "82ac62"], readMax: MiB },
	{ name: "a temp file under readMax", chunks: [ascii("1\n2\n3\n")], maxLines: 2, maxBytes: 1000, readMax: 1000 },
	{ name: "over readMax", chunks: [ascii("0123456789"), ascii("ABCDEFGHIJ")], maxBytes: 8, readMax: 10 },
	{ name: "the head cuts a character", chunks: [ascii("abcd") + "c3a9" + ascii("PQRSTUVWXYZ"), "c3a9" + ascii("1234")], maxBytes: 5, readMax: 10 },
	{ name: "a byte-order mark leads the head", chunks: ["efbbbf" + ascii("hello world tail!")], maxBytes: 5, readMax: 10 },
	{ name: "a byte-order mark leads the tail", chunks: [ascii("AAAAAAAAAA") + "efbbbf" + ascii("zz")], maxBytes: 5, readMax: 10 },
	{ name: "the head ends in an invalid sequence", chunks: [ascii("abc") + "e080" + ascii("xxxxxxxxxx")], maxBytes: 5, readMax: 10 },
	{ name: "the tail ends in a cut character", chunks: [ascii("yyyyyyyyyy") + ascii("abc") + "e282"], maxBytes: 5, readMax: 10 },
	{ name: "an odd readMax", chunks: [ascii("abcdefghijklmnopqrstu")], maxBytes: 5, readMax: 11 },
	{ name: "the tail is all continuation bytes", chunks: [ascii("0123456789") + "8080808080"], maxBytes: 5, readMax: 10 },
	{ name: "invalid bytes and a cut character in the snapshot", chunks: ["61ff62", "e282"], readMax: MiB },
	{ name: "a byte-order mark in the snapshot", chunks: ["efbbbf" + ascii("hi\n")], readMax: MiB },
	{ name: "invalid bytes count decoded", chunks: ["ffffff"], maxBytes: 8, readMax: MiB },
	// 8 raw bytes, 5 decoded: only the raw count passes maxBytes.
	{ name: "a byte-order mark counts in the raw bytes", chunks: ["efbbbf" + ascii("abcde")], maxBytes: 5, readMax: MiB },
	{ name: "unsaved: all output still in memory", chunks: [ascii("1\n2\n3\n")], maxLines: 2, maxBytes: 1000, readMax: 1000, unsaved: true },
	{ name: "unsaved: output past the rolling tail", chunks: [ascii("line1\nline2\nline3\nline4\nline5\nline6\n")], maxBytes: 5, readMax: MiB, unsaved: true },
	{ name: "unsaved: one over-long line", chunks: [ascii(`${"x".repeat(40)}\n`)], maxBytes: 5, readMax: MiB, unsaved: true },
	{ name: "unsaved: readMax cuts a character", chunks: [ascii("0123456789\n") + "e282ac".repeat(3) + "0a"], maxBytes: 8, readMax: 5, unsaved: true },
	{ name: "unsaved: invalid bytes", chunks: ["ff".repeat(10)], maxBytes: 5, readMax: MiB, unsaved: true },
];

// The file a snapshot names: `fullOutputFile` since 943a99e3c, `fullOutputPath` before.
const fileOf = (snapshot: any): { kind: string; path?: string } | undefined =>
	"fullOutputFile" in snapshot
		? snapshot.fullOutputFile
		: snapshot.fullOutputPath === undefined
			? undefined
			: { kind: "saved", path: snapshot.fullOutputPath };
const reportsFailures = "fullOutputFile" in new OutputAccumulator().snapshot();
const tmpdir = process.env.TMPDIR;
const missing = path.join(fs.mkdtempSync(path.join(tmpdir ?? "/tmp", "pi-capture-")), "missing");

const out = [];
for (const row of rows) {
	if (row.unsaved && !reportsFailures) continue;
	if (row.unsaved) process.env.TMPDIR = missing;
	const acc = new OutputAccumulator({ maxLines: row.maxLines, maxBytes: row.maxBytes, tempFilePrefix: "pi-capture" });
	for (const chunk of row.chunks) acc.append(Buffer.from(chunk, "hex"));
	// snapshot() without persisting names a temp file only once the
	// accumulator has opened one, which it does as soon as the raw bytes, the
	// decoded bytes or the lines pass its limits (shouldUseTempFile).
	const tempFileBeforeFinish = fileOf(acc.snapshot()) !== undefined;
	acc.finish();
	const snapshot = acc.snapshot({ persistIfTruncated: true });
	const tempFile = fileOf(snapshot) !== undefined;
	await acc.closeTempFile();
	const file = fileOf(acc.snapshot())?.kind ?? null;
	const full = await acc.readFullOutput(row.readMax);
	if (row.unsaved) process.env.TMPDIR = tmpdir;
	const t = snapshot.truncation;
	out.push({
		...row,
		snapshot: {
			content: snapshot.content,
			truncated: t.truncated,
			truncatedBy: t.truncatedBy,
			totalLines: t.totalLines,
			totalBytes: t.totalBytes,
			outputLines: t.outputLines,
			outputBytes: t.outputBytes,
			lastLinePartial: t.lastLinePartial,
			tempFile,
			tempFileBeforeFinish,
		},
		...(reportsFailures ? { file } : {}),
		full,
	});
	const saved = fileOf(snapshot);
	if (saved?.kind === "saved") fs.rmSync(saved.path!, { force: true });
}
fs.rmSync(path.dirname(missing), { recursive: true });
fs.writeFileSync(outFile, `${JSON.stringify({ sha, node: process.version, rows: out }, null, "\t")}\n`);
console.log(`captured ${out.length} rows at ${sha} -> ${outFile}`);
