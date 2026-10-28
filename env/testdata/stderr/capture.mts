// Captures, for stderr_test.go, what upstream's pi-env Connection makes of a
// daemon's stderr when a start fails (479d6e5e2, #10716): node's
// util.stripVTControlCharacters, and connection.ts's withStderr over the last
// STDERR_TAIL_CHARS UTF-16 code units of the stderr text, for a seeded fuzz
// corpus heavy in escape introducers, their parameter and final bytes, C0 and
// C1 controls, line ends, whitespace JS's trimEnd removes, and astral
// characters (a cut through a surrogate pair included).
//
// withStderr is not exported, so this reads it out of connection.ts: run
//
//	git -C <pi> show 479d6e5e2:packages/env/src/connection.ts > /tmp/connection.ts
//	node capture.mts /tmp/connection.ts > 479d6e5e2.json
//
// The node that runs it is upstream's stripVTControlCharacters; its version is
// recorded in the output.
import { readFileSync } from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import { stripVTControlCharacters } from "node:util";

const source = readFileSync(process.argv[2]!, "utf8");
const pick = (pattern: RegExp): string => {
	const match = source.match(pattern);
	if (!match) throw new Error(`connection.ts has no ${pattern}`);
	return match[0];
};
const constants = [pick(/^const STDERR_TAIL_CHARS = .*$/m), pick(/^const STDERR_TAIL_LINES = .*$/m)];
const fn = pick(/^function withStderr\([^]*?\n}\n/m);
const withStderr = new Function(
	"stripVTControlCharacters",
	`${stripTypeScriptTypes(`${constants.join("\n")}\n${fn}`)}\nreturn { withStderr, STDERR_TAIL_CHARS, STDERR_TAIL_LINES };`,
)(stripVTControlCharacters) as {
	withStderr(message: string, stderr: string): string;
	STDERR_TAIL_CHARS: number;
	STDERR_TAIL_LINES: number;
};

// mulberry32: the corpus is the same on every run.
let seed = 0x479d6e5e;
function random(): number {
	seed = (seed + 0x6d2b79f5) | 0;
	let t = seed;
	t = Math.imul(t ^ (t >>> 15), t | 1);
	t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
	return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}
const pieces = [
	"\u001b", "\u001b", "\u001b", "\u009b", "\u009b", "\u0007", "\u009c", "\u001b\\", "\u001b[", "\u001b]", "\u001b(",
	"[", "]", "(", ")", "#", ";", ";", "?", "0", "1", "2", "4", "9", "12", "123", "1234", "12345", "1;31", ";;",
	"m", "A", "B", "H", "J", "K", "P", "Q", "R", "S", "T", "U", "Z", "c", "f", "g", "h", "l", "n", "o", "p", "q",
	"r", "s", "t", "u", "v", "w", "x", "y", "z", "=", ">", "<", "~", "-", "/", "&", ".", ":", "%", "@", "_", "\\",
	"8;;http://x/", "0;title", " ", " ", "\t", "\r", "\n", "\n", "\r\n", "\u0000", "\u0001", "\u000b", "\u000c",
	"\u001f", "\u007f", "\u0080", "\u0085", "\u009d", "\u009f", "\u00a0", "\u00e9", "\u2717", "\u{1f600}", "\u2028",
	"\u2029", "\u3000", "\ufeff", "\u200b", "\u1680", "word", "a", "b", "line",
];
const corpus: string[] = [
	"",
	"ssh: Permission denied (publickey).\n",
	" \n\u0007\u001b[0m\n",
	"\u001b[31mred\u001b[0m\r\nbell\u0007 del\u007f\tc1:\u0085\u009d end\n\u00dcberpr\u00fcfung fehlgeschlagen \u2717\n\n",
	"\u001b]8;;http://example.com\u0007link\u001b]8;;\u0007\n",
	"\u001b]0;title\u001b\\after\n",
	"\u009b31mred\u009b0m\n",
	"\u001b[?25l\u001b[2J\u001b[H\u001b(B\u001b#8\n",
	"\u001b[1234m\u001b[12345m",
	"\u001b[1;1234m\u001b[1;12345m",
	"\u001b[1;;12m\u001b[;5H",
];
for (let i = 0; i < 1200; i++) {
	let text = "";
	const length = Math.floor(random() * 20);
	for (let j = 0; j < length; j++) text += pieces[Math.floor(random() * pieces.length)];
	corpus.push(text);
}
// Long stderr, as [piece, repeat] runs: only its last STDERR_TAIL_CHARS code
// units reach withStderr.
const long: [string, number][][] = [
	[["first\n", 1], ["x", 5000], ["\n", 1]],
	[["line\n", 30]],
	[["\u{1f600}", 1500]],
	[["\u{1f600}", 1500], ["b", 1]],
	[["\u{1f600}", 1500], ["\n", 1]],
	[["a", 1], ["\u{1f600}", 1500]],
	[["\u001b[31m", 100], ["\u{1f600}x", 700], ["\r\n", 1]],
	[["y", 1999], ["\u001b[0m", 1]],
	[["z\n", 1000], ["\u001b[", 1], ["1;2", 400]],
];
const expand = (runs: [string, number][]): string => runs.map(([piece, n]) => piece.repeat(n)).join("");

const message = "pi-env exited with code 1 before it was ready";
const header = {
	node: process.version,
	message,
	tailChars: withStderr.STDERR_TAIL_CHARS,
	tailLines: withStderr.STDERR_TAIL_LINES,
};
const cases = corpus.map((input) => ({
	input,
	strip: stripVTControlCharacters(input),
	withStderr: withStderr.withStderr(message, input),
}));
// JSON.stringify writes half a surrogate pair, which a cut can leave, as a
// \u escape.
const longCases = long.map((runs) => ({
	runs,
	withStderr: withStderr.withStderr(message, expand(runs).slice(-withStderr.STDERR_TAIL_CHARS)),
}));
// One case per line.
const rows = (list: unknown[]): string => list.map((row) => `\t\t${JSON.stringify(row)}`).join(",\n");
process.stdout.write(
	`${JSON.stringify(header).slice(0, -1)},\n\t"cases": [\n${rows(cases)}\n\t],\n\t"long": [\n${rows(longCases)}\n\t]\n}\n`,
);
