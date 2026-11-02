// Generates the two table files of package formats:
//
//   tables_v8.go   — what Node's RegExp engine (V8 12.4, ICU 78.3, Unicode 17.0
//                    in Node v22.23.4) says about each code point for the
//                    Unicode property classes the upstream format checks use,
//                    plus the property names and group-name characters V8
//                    accepts in a /u pattern (the `regex` format).
//   tables_ada.go  — the UTS #46 mapping, bidi-class, combining-mark, virama
//                    and joining-type tables of Ada 2.9.2 (the WHATWG URL
//                    parser behind Node v22's URL.canParse), read from its
//                    single-header source.
//
//   node --no-opt --no-maglev gen.mjs <ada.cpp> <formats dir>
//
// <ada.cpp> is singleheader/ada.cpp from
// https://github.com/ada-url/ada/releases/download/v2.9.2/singleheader.zip
// (zip sha256 b2cce630590b490d79ea4f4460ba77efd5fb29c5a87a4e8cb7ebc4859bc4b564,
// ada.cpp sha256 30f1265b340b928e5e105787bc7af207396903c6c234cc1dca9279a90cec327b).
// It is only read as text; nothing in it runs. <formats dir> is schema/formats.
//
// The outputs are gofmt'd (gofmt must be on PATH).
//
// Run it with the Node the golden was captured with (process.versions is
// checked): the tables ARE that runtime's answers, so regenerating under
// another Node changes them, and the golden would then have to be recaptured.
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

const [adaPath, outDir] = process.argv.slice(2);
if (!adaPath || !outDir) {
	console.error("usage: node --no-opt --no-maglev gen.mjs <ada.cpp> <formats dir>");
	process.exit(2);
}
const want = { node: "22.23.4", v8: "12.4.254.21-node.57", icu: "78.3", unicode: "17.0", ada: "2.9.2" };
for (const [k, v] of Object.entries(want)) {
	if (process.versions[k] !== v) {
		console.error(`process.versions.${k} is ${process.versions[k]}, want ${v}`);
		process.exit(1);
	}
}
const adaSrc = fs.readFileSync(adaPath, "latin1");
const adaSha = crypto.createHash("sha256").update(fs.readFileSync(adaPath)).digest("hex");
if (adaSha !== "30f1265b340b928e5e105787bc7af207396903c6c234cc1dca9279a90cec327b") {
	console.error(`ada.cpp sha256 ${adaSha} is not Ada 2.9.2's`);
	process.exit(1);
}

const MAX = 0x10ffff;
const hex = (n) => `0x${n.toString(16).toUpperCase().padStart(4, "0")}`;

// ---- RangeTable emission ------------------------------------------------------------------------------------------

function codePointsMatching(test) {
	const out = [];
	for (let cp = 0; cp <= MAX; cp++) if (test(cp)) out.push(cp);
	return out;
}

function rangeTable(cps) {
	// Greedy stride ranges, split at the R16/R32 boundary.
	const r16 = [];
	const r32 = [];
	let i = 0;
	while (i < cps.length) {
		const lo = cps[i];
		const wide = lo > 0xffff;
		let hi = lo;
		let stride = 1;
		let j = i + 1;
		if (j < cps.length && cps[j] > 0xffff === wide) {
			stride = cps[j] - lo;
			hi = cps[j];
			j++;
			while (j < cps.length && cps[j] - hi === stride && cps[j] > 0xffff === wide) {
				hi = cps[j];
				j++;
			}
			// A two-element strided run is no better than two singletons; keep stride-1 runs maximal.
			if (stride !== 1 && j - i === 2) {
				hi = lo;
				stride = 1;
				j = i + 1;
			}
		}
		(wide ? r32 : r16).push([lo, hi, stride]);
		i = j;
	}
	const latin = r16.filter((r) => r[1] <= 0xff).length;
	let s = "&unicode.RangeTable{\n";
	if (r16.length) {
		s += "\tR16: []unicode.Range16{\n";
		for (const [lo, hi, st] of r16) s += `\t\t{${hex(lo)}, ${hex(hi)}, ${st}},\n`;
		s += "\t},\n";
	}
	if (r32.length) {
		s += "\tR32: []unicode.Range32{\n";
		for (const [lo, hi, st] of r32) s += `\t\t{${hex(lo)}, ${hex(hi)}, ${st}},\n`;
		s += "\t},\n";
	}
	if (latin) s += `\tLatinOffset: ${latin},\n`;
	return `${s}}`;
}

function classTable(source, flags = "u") {
	const re = new RegExp(`^(?:${source})$`, flags);
	return rangeTable(codePointsMatching((cp) => re.test(String.fromCodePoint(cp))));
}

function compiles(source) {
	try {
		new RegExp(source, "u");
		return true;
	} catch {
		return false;
	}
}

// ---- tables_v8.go -------------------------------------------------------------------------------------------------

const classes = [
	["jsLetter", "\\p{L}", "/\\p{L}/u (RE_LETTER)"],
	["jsDecimalNumber", "\\p{Nd}", "/\\p{Nd}/u (RE_NUMBER_DECIMAL)"],
	["jsNonspacingMark", "\\p{Mn}", "/\\p{Mn}/u (RE_MARK_NONSPACING)"],
	["jsSpacingMark", "\\p{Mc}", "/\\p{Mc}/u (RE_MARK_SPACING_COMBINING)"],
	["jsEnclosingMark", "\\p{Me}", "/\\p{Me}/u (RE_MARK_ENCLOSING)"],
	["jsGreek", "\\p{Script=Greek}", "/\\p{Script=Greek}/u (RE_SCRIPT_GREEK)"],
	["jsHebrew", "\\p{Script=Hebrew}", "/\\p{Script=Hebrew}/u (RE_SCRIPT_HEBREW)"],
	[
		"jsJapanese",
		"[\\p{Script=Hiragana}\\p{Script=Katakana}\\p{Script=Han}]",
		"/[\\p{Script=Hiragana}\\p{Script=Katakana}\\p{Script=Han}]/u (RE_SCRIPT_JAPANESE)",
	],
	[
		"jsArabicLetter",
		"[\\p{Script=Arabic}\\p{Script=Syriac}\\p{Script=Thaana}\\p{Script=Mandaic}]",
		"/[\\p{Script=Arabic}\\p{Script=Syriac}\\p{Script=Thaana}\\p{Script=Mandaic}]/u (RE_SCRIPT_ARABIC_LETTER)",
	],
];

let js = `// Code generated by testdata/tables/gen.mjs; DO NOT EDIT.
//
// Node v${process.versions.node} (V8 ${process.versions.v8}, ICU ${process.versions.icu}, Unicode ${process.versions.unicode}).
// Each table is the set of code points the quoted JavaScript regular expression
// matches, read from V8 code point by code point, so the checks classify
// exactly as upstream does on that runtime (Go's own unicode tables are an
// older Unicode version).

package formats

import "unicode"

`;
for (const [name, source, doc] of classes) {
	js += `// ${name} is ${doc}.\nvar ${name} = ${classTable(source)}\n\n`;
}
js += `// jsIdnEmailDomain is /[\\p{L}\\p{N}]/iu, the domain-label class of format idn-email: case-insensitive
// /u matching also admits code points that only case-fold into it (U+0345, for one).
var jsIdnEmailDomain = ${classTable("[\\p{L}\\p{N}]", "iu")}

`;

// Group names: what V8 accepts as the first and as a later code point of a literal (?<name>) group name.
const startCPs = codePointsMatching((cp) => compiles(`(?<${String.fromCodePoint(cp)}>)`));
const continueCPs = codePointsMatching((cp) => compiles(`(?<a${String.fromCodePoint(cp)}>)`));
js += `// jsGroupNameStart is the code points V8 accepts first in a /u group name: ID_Start, $ and _.
var jsGroupNameStart = ${rangeTable(startCPs)}

// jsGroupNameContinue is the code points V8 accepts after the first in a /u group name: ID_Continue,
// $, ZWNJ and ZWJ.
var jsGroupNameContinue = ${rangeTable(continueCPs)}

`;

// Property names: every identifier-like token in the node binary (ICU's pnames data carries all
// property and value aliases), kept when V8 accepts it in the position at hand.
const bin = fs.readFileSync(process.execPath);
const tokens = new Set();
{
	let start = -1;
	for (let i = 0; i <= bin.length; i++) {
		const c = i < bin.length ? bin[i] : 0;
		const word = (c >= 0x30 && c <= 0x39) || (c >= 0x41 && c <= 0x5a) || (c >= 0x61 && c <= 0x7a) || c === 0x5f;
		if (word) {
			if (start < 0) start = i;
		} else if (start >= 0) {
			if (i - start <= 64) tokens.add(bin.toString("latin1", start, i));
			start = -1;
		}
	}
}
const sorted = [...tokens].sort();
const lone = sorted.filter((t) => compiles(`\\p{${t}}`));
const gcKeys = sorted.filter((t) => compiles(`\\p{${t}=Lu}`));
const gcValues = sorted.filter((t) => compiles(`\\p{General_Category=${t}}`));
const scKeys = sorted.filter((t) => compiles(`\\p{${t}=Latin}`));
const scValues = sorted.filter((t) => compiles(`\\p{Script=${t}}`));
const scxValues = sorted.filter((t) => compiles(`\\p{Script_Extensions=${t}}`));
if (JSON.stringify(scValues) !== JSON.stringify(scxValues)) throw new Error("Script and Script_Extensions values differ");
for (const k of gcKeys) for (const v of gcValues) if (!compiles(`\\p{${k}=${v}}`)) throw new Error(`gc ${k}=${v}`);
for (const k of scKeys) for (const v of scValues) if (!compiles(`\\p{${k}=${v}}`)) throw new Error(`sc ${k}=${v}`);
const strList = (xs) => `map[string]bool{\n${xs.map((x) => `\t${JSON.stringify(x)}: true,\n`).join("")}}`;
js += `// jsLoneProperties is the names V8 accepts alone in a /u \\p{…}: General_Category values and binary
// properties.
var jsLoneProperties = ${strList(lone)}

// jsGeneralCategoryKeys is the names V8 accepts before "=" for a General_Category value.
var jsGeneralCategoryKeys = ${strList(gcKeys)}

// jsGeneralCategoryValues is the values V8 accepts after General_Category=.
var jsGeneralCategoryValues = ${strList(gcValues)}

// jsScriptKeys is the names V8 accepts before "=" for a Script or Script_Extensions value.
var jsScriptKeys = ${strList(scKeys)}

// jsScriptValues is the values V8 accepts after Script= (and Script_Extensions=).
var jsScriptValues = ${strList(scValues)}
`;
fs.writeFileSync(path.join(outDir, "tables_v8.go"), js);

// ---- tables_ada.go ------------------------------------------------------------------------------------------------

function cArray(name) {
	const re = new RegExp(`${name}(?:\\[\\d*\\])+\\s*=\\s*\\{`, "g");
	const m = re.exec(adaSrc);
	if (!m) throw new Error(`no ${name}`);
	let depth = 1;
	let i = m.index + m[0].length;
	const start = i;
	while (depth > 0) {
		if (adaSrc[i] === "{") depth++;
		else if (adaSrc[i] === "}") depth--;
		i++;
	}
	return adaSrc.slice(start, i - 1);
}
const nums = (body) => [...body.matchAll(/0x[0-9a-fA-F]+|\d+/g)].map((m) => Number(m[0]));

const mappings = nums(cArray("const uint32_t mappings"));
if (mappings.length !== 5164) throw new Error("mappings length");
const tableNums = nums(cArray("const uint32_t table"));
if (tableNums.length !== 16000) throw new Error("table length");
const dirBody = cArray("static directions dir_table");
const dirs = [...dirBody.matchAll(/\{(0x[0-9a-f]+), (0x[0-9a-f]+), direction::(\w+)\}/g)].map((m) => [
	Number(m[1]),
	Number(m[2]),
	m[3],
]);
const list = (name) => nums(cArray(`constexpr static uint32_t ${name}`));
const combining = list("combining");
const virama = list("virama");
const joinR = list("R");
const joinL = list("L");
const joinD = list("D");
for (const [n, xs] of Object.entries({ combining, virama, joinR, joinL, joinD })) {
	for (let i = 1; i < xs.length; i++) if (xs[i - 1] >= xs[i]) throw new Error(`${n} is not strictly sorted`);
}
for (let i = 1; i < dirs.length; i++) if (dirs[i - 1][1] >= dirs[i][0]) throw new Error("dir_table overlaps");

let ada = `// Code generated by testdata/tables/gen.mjs; DO NOT EDIT.
//
// From Ada 2.9.2 (singleheader/ada.cpp, sha256 ${adaSha}), the URL parser of
// Node v${process.versions.node}. Ada is dual-licensed Apache-2.0 / MIT; see LICENSES/ada-LICENSE-MIT.
// Its IDNA data is Unicode IDNA 15.0.0.

package formats

`;
ada += "// uts46Table is Ada's UTS #46 mapping table: each entry covers code points from start up to the next\n";
ada += "// entry's start.\nvar uts46Table = [...]uts46Entry{\n";
for (let i = 0; i < tableNums.length; i += 2) {
	const start = tableNums[i];
	const d = tableNums[i + 1];
	const code = d & 0xff;
	if (code < 3) {
		ada += `\t{${hex(start)}, ${["uts46Ignored", "uts46Valid", "uts46Disallowed"][code]}, 0, 0},\n`;
	} else {
		ada += `\t{${hex(start)}, uts46Mapped, ${(d >>> 8) & 0xffff}, ${d >>> 24}},\n`;
	}
}
ada += "}\n\n// uts46Mappings holds the replacement code points uts46Table entries index.\nvar uts46Mappings = [...]rune{";
mappings.forEach((m, i) => {
	ada += `${i % 12 === 0 ? "\n\t" : " "}${hex(m)},`;
});
ada += "\n}\n\n// adaBidiTable is Ada's dir_table: code points outside every range have no direction.\nvar adaBidiTable = [...]bidiRange{\n";
for (const [lo, hi, d] of dirs) ada += `\t{${hex(lo)}, ${hex(hi)}, bidi${d}},\n`;
ada += "}\n";
const runeList = (name, doc, xs) => {
	let s = `\n// ${name} is ${doc}\nvar ${name} = [...]rune{`;
	xs.forEach((x, i) => {
		s += `${i % 12 === 0 ? "\n\t" : " "}${hex(x)},`;
	});
	return `${s}\n}\n`;
};
ada += runeList("adaCombining", "Ada's sorted list of combining marks (General_Category=Mark).", combining);
ada += runeList("adaVirama", "Ada's sorted list of Virama (Canonical_Combining_Class=9) code points.", virama);
ada += runeList("adaJoiningR", "Ada's sorted list of Joining_Type=R code points.", joinR);
ada += runeList("adaJoiningL", "Ada's sorted list of Joining_Type=L code points.", joinL);
ada += runeList("adaJoiningD", "Ada's sorted list of Joining_Type=D code points.", joinD);
fs.writeFileSync(path.join(outDir, "tables_ada.go"), ada);
execFileSync("gofmt", ["-w", path.join(outDir, "tables_v8.go"), path.join(outDir, "tables_ada.go")]);
console.error(
	`wrote tables: ${lone.length} lone properties, ${gcValues.length} gc values, ${scValues.length} script values, keys gc=${gcKeys} sc=${scKeys}`,
);
