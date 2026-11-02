// Captures what upstream pi-schema's format checks (packages/schema/src/formats,
// adapted from TypeBox 1.3.27) return — the oracle behind TestGolden.
//
//   node --no-opt --no-maglev capture.mjs <upstream tree> <out.json> <sha>
//
// <upstream tree> is an extracted upstream checkout (`git archive <sha>
// packages/schema | tar -x -C <tree>`); the script imports
// packages/schema/src/formats/index.ts from it (Node v22 strips the types)
// and reads the JSON-Schema-Test-Suite fixtures under
// packages/schema/test/fixtures, so nothing is written into the tree.
//
// --no-opt --no-maglev are required: Node v22's optimized URL.canParse goes
// through a V8 fast-API call that hands Ada a Latin-1 string as if it were
// UTF-8, so `url`, `iri` and `iri-reference` flip for hosts like "é.com"
// once the call is hot. The unoptimized answer is the one the WHATWG URL
// standard (and the Go port) gives; the script refuses to run without the
// flags.
//
// The corpus: every string in the suite's optional/format fixtures (draft
// 2020-12 and draft 7) fed to EVERY format, the edge cases below fed to every
// format, and targeted families (code point samples, URL and regex fuzz,
// Unicode property names) fed to the formats they exercise. A row records
// check(value) per format. Strings holding a lone surrogate are recorded with
// both the raw result and the result on value.toWellFormed(): a JSON decoder
// (Go's, and so the Go port's input) turns the \udXXX escape into U+FFFD.
//
// capture-6536f8012.json is a src capture of upstream 6536f8012, under Node
// v22.23.4 (V8 12.4, ICU 78.3, Unicode 17.0, Ada 2.9.2).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [tree, outFile, sha] = process.argv.slice(2);
if (!tree || !outFile || !sha) {
	console.error("usage: node --no-opt --no-maglev capture.mjs <upstream tree> <out.json> <sha>");
	process.exit(2);
}
if (!process.execArgv.includes("--no-opt") || !process.execArgv.includes("--no-maglev")) {
	console.error("run with node --no-opt --no-maglev (see the header)");
	process.exit(2);
}
const { formats } = await import(pathToFileURL(path.resolve(tree, "packages/schema/src/formats/index.ts")).href);
const names = Object.keys(formats);

// ---- corpus -------------------------------------------------------------------------------------------------------

const suite = path.resolve(tree, "packages/schema/test/fixtures/json-schema-test-suite/tests");
const fixtureStrings = new Set();
for (const draft of ["draft2020-12", "draft7"]) {
	const dir = path.join(suite, draft, "optional/format");
	for (const file of fs.readdirSync(dir).sort()) {
		for (const group of JSON.parse(fs.readFileSync(path.join(dir, file), "utf8"))) {
			for (const test of group.tests) if (typeof test.data === "string") fixtureStrings.add(test.data);
		}
	}
}

const edge = [
	// generic
	"", " ", "\t", "\n", "a\n", "\na", "a b", "\u00a0", "\u2028", "\u2029", "\ufeff", "x", "X", "0", "-", ".",
	"\ud800", "a\udc00b", "\ud83d\ude00", "\ud83d", "\ude00\ud83d", "\u{1f600}", "\u{10ffff}", "\u0000", "\u007f",
	// date / time / date-time / duration
	"2020-02-29", "2021-02-29", "1900-02-29", "2000-02-29", "2020-13-01", "2020-00-10", "2020-01-00", "2020-04-31",
	"2020-12-31", "0000-01-01", "9999-12-31", "2020-1-01", "20200101", "2020-01-01\n", "\uff12020-01-01",
	"2020-01-01T00:00:00Z", "2020-01-01t00:00:00z", "2020-01-01 00:00:00Z", "2020-01-01T00:00:00", "2020-01-01TT00:00:00Z",
	"2020-01-01T00:00:00.123456789+01:30", "2020-01-01T24:00:00Z", "2020-01-01T23:59:60Z", "2020-01-01T23:59:60+00:00",
	"2020-01-01T00:59:60+01:00", "2020-01-01T23:59:60-00:01", "2020-01-01T22:59:60-01:00", "2020-01-01T15:59:60-08:00",
	"2020-01-01T23:59:60.5Z", "2020-01-01T23:58:60Z", "1998-12-31T23:59:60Z", "2020-01-01T00:00:00+24:00",
	"2020-01-01T00:00:00+23:60", "2020-01-01T00:00:00+2300", "2020-01-01T00:00:00.Z", "2020-01-01T00:00:00,5Z",
	"23:59:60Z", "00:00:00Z", "00:00:00z", "00:00:00", "00:00:00+00:00", "00:00:00-23:59", "12:00:00.0000Z", "1:00:00Z",
	"12:60:00Z", "12:00:61Z", "23:59:60+00:00\n", "00:29:60+00:30", "00:00:60-23:59", "23:59:60-00:00",
	"P1Y", "P1Y2M3DT4H5M6S", "PT1H", "PT", "P", "P1W", "P1W1D", "P1.5Y", "PT36H", "P1D T1H", "p1y", "P1M1Y", "PT1S1M",
	"P0D", "-P1D", "P1DT", "P1Y2D", "PT1H2S", "P1Y\n",
	// email / idn-email
	"a@b.c", "A@B.C", "a.b@c", "a..b@c", ".a@b", "a.@b", "\"a b\"@c", "\"a\\\"b\"@c", "\"a\\\nb\"@c", "\"a\nb\"@c",
	"a@[1.2.3.4]", "a@[256.1.1.1]", "a@[IPv6:::1]", "a@[ipv6:::1]", "a@[IPV6:ABCD::]", "a@-b.c", "a@b-.c", "a@b..c",
	"a@" + "b".repeat(63) + ".c", "a@" + "b".repeat(64) + ".c", "a@b", "@b", "a@", "a@@b", "a@b@c", "a\u212a@b",
	"\u212a@b", "a@\u212a", "a@\u017f", "\u00e9@\u00e9", "\u00e9@b.c", "a@\u00e9.c", "a@\u0345", "a@b\u0345", "a@\u0661",
	"a@\u00b2", "a@\u2160", "a@\u{1d7ce}", "a@\u{16fe4}", "a@\u{1e5d1}", "a@a-", "a@a-b", "a@-", "a@a.-b", "\u00e9\u0301@x",
	"e\u0301@x", "a@e\u0301", "a@\u00e9", "\"\u00e9\"@x", "\"\\\u2028\"@x", "\"\\\u00e9\"@x", "a@b\n", "a@" + "\u00e9".repeat(63),
	"a@" + "\u00e9".repeat(64), "a@\u{1f600}", "a@\ud83d", "\ud83d@a", "a@" + "\u{10400}".repeat(40),
	// hostname / idn-hostname
	"example.com", "Example.COM", "example.com.", ".example.com", "example..com", "-a.com", "a-.com", "a--b.com",
	"ab--c.com", "xn--ls8h", "XN--LS8H", "xn--ls8h.", "xn--", "xn---", "xn--a", "xn--a-", "xn---a", "xn--zca", "xn--Zca",
	"xn--4ca", "xn--9ca", "xn--bcher-kva", "xn--bcher-kva.example", "xn--BCHER-KVA", "xn--a-ecp.ru", "xn--l8j",
	"xn--ihqwcrb4cv8a8dqg056pqjye", "xn--zz", "xn--zzzzzzzzz", "xn--99999999999", "xn--" + "9".repeat(55),
	"xn--a".padEnd(63, "a"), "a".repeat(63), "a".repeat(64), `${"a".repeat(63)}.${"b".repeat(63)}.${"c".repeat(63)}.${"d".repeat(61)}`,
	`${"a".repeat(63)}.${"b".repeat(63)}.${"c".repeat(63)}.${"d".repeat(62)}`, "a_b", "a b", "1.2.3.4", "123", "a.1",
	"\u212an--ls8h", "xn--\u212a", "x\u212a", "XN--\u0130", "xn--\u0131", "\u00e9", "\u00e9.com", "e\u0301", "\u0301a",
	"\u00df", "\u03c2", "a\u00b7l", "l\u00b7l", "l\u00b7", "\u0375\u03b1", "\u0375a", "\u03b1\u0375", "\u05d0\u05f3",
	"a\u05f3", "\u05f3", "a\u200cb", "\u094d\u200cb", "\u0915\u094d\u200c", "\u0915\u094d\u200d", "a\u200d", "\u200d",
	"\u30fb", "\u30fb\u3042", "a\u30fb", "\u3042\u30fb", "\u0640", "a\u0640", "\u07fa", "\u3005", "\u3007", "\u302e",
	"\u05d0", "\u05d0a", "a\u05d0", "\u05d01", "\u05d0\u0661", "\u05d01\u0661", "\u0627\u0661", "\u0627\u06f1",
	"\u0627\u0661\u06f1", "1\u05d0", "\u05d0.a", "a.\u05d0", "a.b.\u05d0", "\u05d0.1a", "\u05d0.a1", "xn--4db.a",
	"xn--4db.1a", "\u0627.com", "\u0627\u0300", "\u05d0\u0300", "\u0300", "\uff41", "\uff41\uff42\uff43", "a\uff0eb",
	"a\u3002b", "a\uff61b", "\uff0e", "\uff21", "a\u00adb", "\u00ad", "a\u034fb", "a\u200bb", "a\ufe0fb", "a\u{e0100}b",
	"\u3002", "a\u3002", "\u3002a", "\uff10", "a\u0020b", "\u00e9".repeat(60), "\u00e9".repeat(63), "\u00e9".repeat(64),
	`${"\u{10400}".repeat(31)}`, `${"\u{10400}".repeat(32)}`, "\u{1f600}", "a\u{1f600}", "\u{10400}", "\u{e0001}",
	"\u0660", "a\u0660", "\u06f0", "\u05d0\u06f0", "\u{1e900}", "\u{1e5d0}", "\u{16d40}", "\u{11380}", "\u{10d40}",
	"\u0d3b", "\u0dca", "\u0915\u200c", "\u0644\u200c\u0644", "\u0628\u200c", "\u200c\u0628", "\ua872\u200c\u0628",
	"\u0627\u200c\u0628", "\u0628\u064b\u200c\u0628", "\u0628\u200c\u064b", "\u0915\u094d\u200c\u05d0",
	// ipv4 / ipv6
	"0.0.0.0", "255.255.255.255", "256.0.0.0", "01.2.3.4", "1.2.3", "1.2.3.4.", "1.2.3.04", "1.2.3.4\n", "\u0661.2.3.4",
	"::", "::1", "1::", "1:2:3:4:5:6:7:8", "1:2:3:4:5:6:7:8:9", "1:2:3:4:5:6:7::", "::1:2:3:4:5:6:7", "1::2::3",
	":::", "1:2:3:4:5:6:1.2.3.4", "::ffff:1.2.3.4", "::FFFF:1.2.3.4", "::1.2.3.4", "1::1.2.3.4", "::1.2.3.256",
	"::01.2.3.4", "1:2:3:4:5:6:7:1.2.3.4", "fe80::1%eth0", "ABCD::EF", "12345::", "::1\n", "[::1]", "1:2:3:4:5::1.2.3.4",
	// uri / uri-reference / iri / iri-reference / url / uri-template
	"http://example.com", "HTTP://EXAMPLE.COM", "http://example.com/path?q=1#frag", "http:", "http:/", "http://",
	"http:///a", "http://a:80", "http://a:", "http://a:65535", "http://a:65536", "http://a:-1", "http://a:0x50",
	"http://a:080", "http://u:p@a", "http://u@", "http://@a", "http://a@b@c", "http://[::1]", "http://[::1]:8080/",
	"http://[::1", "http://[v1.fe]", "http://[V1.FE]", "http://[vz.a]", "http://[::ffff:1.2.3.4]",
	"http://1.2.3.4", "http://1.2.3.256", "http://0x7f.1", "http://0x7f.0.0.1", "http://127.1", "http://4294967295",
	"http://4294967296", "http://08.1.1.1", "http://1.2.3.4.5", "http://a.1", "http://a.0x", "http://a.09",
	"http://1.2.3.4.", "http://1.2.3.4..", "http://%41", "http://%zz", "http://%25", "http://a%2Eb", "http://%c3%a9",
	"http://%ff", "http://%e2%80%8b", "http://\u00e9.com/", "http://\u00c9.com/", "http://xn--ls8h/", "http://xn--a/",
	"http://xn--a-/", "http://\u00e9.xn--a-/", "http://xn--zca.\u00e9", "http://a.xn--/", "http://\u00ad/",
	"http://a\u00adb/", "http://\u2488/", "http://\uff11\uff12\uff17.\uff10.\uff10.\uff11/", "http://a\uff05b/",
	"http://a_b/", "http://-a/", "http://a b/", "http://a<b/", "http://a^b/", "http://a|b/", "http://a\\b",
	"http://a/b\\c", "http:\\\\a", "http:\\a", "http:a", "file:///c:/x", "file://c:/x", "file://host/x",
	"file://host:80/x", "file://[::1]/", "file://\u00e9/", "file://%zz/", "file:", "file:x", "FILE://A/B",
	"foo://", "foo://a:b", "foo://a:1", "foo://a b", "foo://a%zz", "foo://\u00e9", "foo://[::1]", "foo://[x]",
	"foo://a^b", "foo:///x", "foo:", "foo:bar", "foo:/bar", "mailto:a@b.c", "urn:isbn:0451450523", "a+b-c.d:x",
	"1a:x", "a\u00e9:x", "//a/b", "//a:99999/b", "//[::1", "/a/b", "a/b", "?q", "#f", "", "./a", "../a", "a:b:c",
	"httpx//example.com", "http//example.com", "http://exa mple.com", "http://example.com/a b", "http://example.com/\u00e9",
	"http://example.com/%", "http://example.com/%4", "http://example.com/%41", "http://example.com/%zz",
	"http://example.com/<>", "http://example.com/{}", "http://example.com/|", "http://example.com/^", "http://ex`am",
	"http://example.com/\u007f", "http://example.com\t/", "\thttp://example.com", " http://example.com ",
	"http://ex\nample.com", "ws://a", "wss://a:443", "ftp://a", "https://a:0", "javascript:alert(1)", "data:,x",
	"http://[1:2:3:4:5:6:7:8:9]", "http://[::1.2.3.4]", "http://[1::2::3]", "http://[::1]x", "http://[v1.x]",
	"{a}", "{+a}", "{a,b}", "{a:3}", "{a*}", "{a:0}", "{a:10000}", "{.a.b}", "{}", "{a", "a}", "{a b}", "{%41}", "{%4}",
	"%", "%41", "%zz", "x{=a}", "x{!a}", "x{a}{b}", "http://example.com/{id}?x={y}", "\u00e9{a}", "<", "{a-b}",
	// json-pointer / relative-json-pointer / json-pointer-uri-fragment
	"/", "/a", "/a/b", "a", "/~", "/~0", "/~1", "/~2", "/~01", "//", "/\u00e9", "#", "#/", "#/a", "#/%41", "#/%4",
	"#/~0", "#/~2", "#/a b", "#a", "#/\u00e9", "0", "1", "01", "0#", "1/a", "0/~2", "-1", "1#/", "10#", "0/",
	// uuid
	"123e4567-e89b-12d3-a456-426614174000", "123E4567-E89B-12D3-A456-426614174000", "123e4567e89b12d3a456426614174000",
	"123e4567-e89b-12d3-a456-42661417400", "123e4567-e89b-12d3-a456-4266141740000", "g23e4567-e89b-12d3-a456-426614174000",
	"{123e4567-e89b-12d3-a456-426614174000}", "00000000-0000-0000-0000-000000000000",
	// regex
	"a", "a|b", "(a)", "(?:a)", "(?=a)", "(?!a)", "(?<=a)", "(?<!a)", "(?<n>a)\\k<n>", "\\k<n>", "(?<n>a)(?<n>b)",
	"(?<n>a)|(?<n>b)", "(?<1>a)", "(?<$_>a)", "(?<\\u0061>a)", "(?<\\u{61}>a)", "(?<a\\u200c>a)", "(?<\u0301>a)",
	"(?<a\u0301>a)", "(?<\u{1d49c}>a)", "(?<\\ud835\\udc9c>a)", "(?<\\ud835>a)", "(?<a", "(?<>a)", "(?<a>", "(", ")",
	"(a", "a)", "[", "]", "[a", "a]", "{", "}", "a{", "a{1", "a{1,", "a{1,2", "a{1}", "a{1,}", "a{1,2}", "a{2,1}",
	"a{,1}", "a{1}?", "a{1}{2}", "a**", "a*?", "a*??", "a+*", "*", "+", "?", "a|*", "(*)", "^*", "$*", "\\b*", "\\B+",
	"(?=a)*", "(?!a)?", "(?<=a)*", "(?<!a){2}", "\\", "a\\", "\\a", "\\d", "\\D", "\\s", "\\S", "\\w", "\\W", "\\b",
	"\\B", "\\f\\n\\r\\t\\v", "\\0", "\\00", "\\01", "\\1", "(a)\\1", "(a)\\2", "\\1(a)", "\\8", "\\9", "(a)\\10",
	"\\x4", "\\x41", "\\xg1", "\\u004", "\\u0041", "\\u{41}", "\\u{110000}", "\\u{}", "\\u{10FFFF}", "\\u{0000000041}",
	"\\ud83d\\ude00", "\\ud83d", "\\ude00", "\\cA", "\\ca", "\\c1", "\\c", "\\c_", "\\-", "\\/", "\\.", "\\^", "\\$",
	"\\|", "\\(", "\\)", "\\[", "\\]", "\\{", "\\}", "\\*", "\\+", "\\?", "\\\\", "\\e", "\\z", "\\_", "\\ ", "\\k",
	"\\p", "\\p{", "\\p{L", "\\p{L}", "\\P{L}", "\\p{Lu}", "\\p{Letter}", "\\p{letter}", "\\p{gc=L}", "\\p{gc=Letter}",
	"\\p{General_Category=Lu}", "\\p{general_category=Lu}", "\\p{Script=Latin}", "\\p{sc=Latn}", "\\p{scx=Grek}",
	"\\p{Script_Extensions=Greek}", "\\p{Script=Garay}", "\\p{Script=Sidetic}", "\\p{Script=Unknown}",
	"\\p{Script=Zzzz}", "\\p{Script=L}", "\\p{gc=Latin}", "\\p{ASCII}", "\\p{Any}", "\\p{Assigned}", "\\p{Emoji}",
	"\\p{RGI_Emoji}", "\\p{Basic_Emoji}", "\\p{L=Lu}", "\\p{ L}", "\\p{L }", "\\p{=L}", "\\p{gc=}", "\\p{gc==L}",
	"\\p{Script=Latin=}", "[\\p{L}]", "[\\p{L}-z]", "[a-\\p{L}]", "[\\P{L}]", "[a]", "[^a]", "[]", "[^]", "[a-z]",
	"[z-a]", "[a-a]", "[-]", "[a-]", "[-a]", "[a-b-c]", "[--a]", "[a--]", "[\\w-]", "[\\w-a]", "[a-\\w]", "[\\d-\\w]",
	"[\\b]", "[\\B]", "[\\-]", "[\\0]", "[\\00]", "[\\1]", "[\\8]", "[\\k]", "[\\c1]", "[\\c_]", "[\\cA]", "[\\x41-\\x5a]",
	"[\\u{1f600}-\\u{1f601}]", "[\\u{1f601}-\\u{1f600}]", "[\\ud83d\\ude00-\\ud83d\\ude01]", "[\u{1f600}-\u{1f601}]",
	"[\u{1f601}-\u{1f600}]", "[\\]]", "[[]", "[a]]", "[\\", "[a-\\", ".", "^$", "a$b", "(?:", "(?", "(?a)", "(?i:a)",
	"(?i)a", "(?-i:a)", "(?#c)", "a{99999999999999999999}", "a{1,99999999999999999999}", "x{2147483647}",
	"x{2147483648}", "x{4294967296,1}", "\u00e9", "[\u00e9-\u00ea]", "\u{1f600}", "\ud800", "[\ud800-\udbff]",
	"[\udc00-\ud800]", "\\/\\/", "/", "a/b", "\\u{1F600}", "\\u{1f600", "\\uD83D\\uDE00", "[\\ud83d-\\ude00]",
];

// ---- targeted families --------------------------------------------------------------------------------------------

// A seeded LCG keeps the fuzz reproducible.
let seed = 0x5eed;
const rand = (n) => {
	seed = (seed * 1103515245 + 12345) % 2147483648;
	return seed % n;
};
const pick = (xs) => xs[rand(xs.length)];

const targeted = []; // [value, formats[]]
const add = (value, fmts) => targeted.push([value, fmts]);

// Code points: a stride through all of Unicode plus the ones the checks single out.
const special = [
	0x80, 0x9f, 0xa0, 0xad, 0xb2, 0xb7, 0xc9, 0xdf, 0xe9, 0x130, 0x131, 0x17f, 0x300, 0x301, 0x345, 0x34f, 0x375,
	0x3c2, 0x5d0, 0x5f3, 0x5f4, 0x610, 0x627, 0x640, 0x660, 0x661, 0x6f0, 0x6f1, 0x6fd, 0x710, 0x7fa, 0x94d, 0xf0b,
	0x180b, 0x200b, 0x200c, 0x200d, 0x200e, 0x2028, 0x212a, 0x2160, 0x2488, 0x24b6, 0x3002, 0x3005, 0x3007, 0x302e,
	0x3031, 0x303b, 0x3042, 0x30a2, 0x30fb, 0x4e00, 0xa872, 0xd7ff, 0xd800, 0xdbff, 0xdc00, 0xdfff, 0xe000, 0xfe00,
	0xfe0f, 0xfeff, 0xff01, 0xff05, 0xff0e, 0xff10, 0xff21, 0xff41, 0xff5e, 0xff61, 0xfffd, 0xffff, 0x10000, 0x10400,
	0x10d40, 0x11380, 0x16d40, 0x16fe4, 0x1d49c, 0x1d7ce, 0x1e5d0, 0x1e900, 0x1f1e6, 0x1f600, 0x20000, 0x2ebf0,
	0x31350, 0x323b0, 0xe0001, 0xe0100, 0xe01ef, 0xf0000, 0x10fffd, 0x10ffff,
];
const cps = new Set(special);
for (let cp = 0x80; cp <= 0x10ffff; cp += 509) cps.add(cp);
for (const cp of [...cps].sort((a, b) => a - b)) {
	const c = String.fromCodePoint(cp);
	add(`a${c}b`, ["hostname", "idn-hostname"]);
	add(c, ["idn-hostname", "regex", "json-pointer", "uri-template"]);
	add(`x@${c}`, ["idn-email"]);
	add(`${c}@x`, ["email", "idn-email"]);
	add(`http://a${c}b/`, ["url", "iri", "iri-reference", "uri"]);
	add(`http://x/${c}`, ["url", "iri", "iri-reference", "uri", "uri-reference"]);
	add(`foo://a${c}b/`, ["url", "iri"]);
	add(`\\${c}`, ["regex"]);
	add(`(?<${c}>)`, ["regex"]);
	add(`(?<a${c}>)`, ["regex"]);
	add(`[${c}-\u{10ffff}]`, ["regex"]);
}

// URLs: the host column crossed with three schemes, then a seeded sample of whole URLs.
const hosts = [
	"", "a", "A", "a.b", "1.2.3.4", "0x7f.1", "0X7F.1", "0x.0x.0", "1.2.3.4.5", "999.1.1.1", "1.256", "1.2.65536",
	"4294967295", "4294967296", "0", "00", "09", "0x", "0xg", "a.0x10", "x.08", "1e1", "[::1]", "[::1", "::1]",
	"[1:2:3:4:5:6:7:8]", "[::ffff:1.2.3.4]", "[::1.2.3]", "[v1.x]", "[]", "xn--ls8h", "xn--a", "xn--a-", "XN--LS8H",
	"xn--", "a%41", "%41", "%zz", "%", "%2e", "a b", "\u00e9", "\u00c9", "\u00df", "\u03c2", "a_b", "-a", "a-", "a..b",
	".", "..", "\uff11\uff12\uff17.\uff10.\uff10.\uff11", "\u00ad", "a\u200db", "\u0915\u094d\u200d", "\u2488", "a|b",
	"a^b", "a<b", "a\u0000b", "\u05d0", "\u05d0a", "a\u05d0", "\u05d01", "\u0627\u0661\u06f1", "\u0300a", "a\u0300",
	"\u{1f600}", "\ufffd", "\u{e0100}", "xn--zca", "xn--4ca", "xn--a-ecp", "xn--ihqwcrb4cv8a8dqg056pqjye",
	"\u00e9.xn--a-", "xn--zz", "a\u3002b", "a\uff0eb", "\u212a", "u@a", "u:p@a", "@a", "a@", "@", "a:1", "a:",
	"a:99999", "a:-1", "a:0x1",
];
for (const h of hosts) {
	for (const scheme of ["http://", "foo://", "file://"]) add(`${scheme}${h}/p`, ["url", "iri", "iri-reference"]);
	add(`//${h}/p`, ["iri-reference", "uri-reference"]);
}
const schemes = ["http", "HTTP", "https", "ws", "wss", "ftp", "file", "foo", "a+b", "mailto", "", "1x"];
const seps = [":", "://", ":/", ":///", ":\\\\", ":/\\", "://\\"];
const users = ["", "u@", "u:p@", "@", "u@@", "%@", "\u00e9@"];
const ports = ["", ":", ":80", ":65535", ":65536", ":-1", ":a", ":0080", ":1:2"];
const tails = ["", "/", "/p?q#f", "?", "#", "\\x", "/a b", "/%zz", "/\u00e9", "#\u0000"];
for (let i = 0; i < 4000; i++) {
	add(`${pick(schemes)}${pick(seps)}${pick(users)}${pick(hosts)}${pick(ports)}${pick(tails)}`, [
		"url",
		"iri",
		"iri-reference",
		"uri",
		"uri-reference",
	]);
}

// Regex: every pattern of up to two characters over a syntax-heavy alphabet, up to three over a smaller one,
// and seeded longer ones.
const alpha2 = [..."\\()[]{}*+?|^$.-,=!:<>0129abcdkpuxBDSWP/_ \u00e9"];
const alpha3 = [..."\\()[]{}*+?|^$-=!<>01akpux"];
for (const a of alpha2) {
	add(a, ["regex"]);
	for (const b of alpha2) add(a + b, ["regex"]);
}
for (const a of alpha3) for (const b of alpha3) for (const c of alpha3) add(a + b + c, ["regex"]);
const pieces = [
	..."\\()[]{}*+?|^$.-,=!:<>0129abkpuxcd", "\\p{L}", "\\p{", "(?<a>", "(?<b>", "\\k<a>", "\\k<", "(?:", "(?=", "(?!",
	"(?<=", "(?<!", "{1}", "{1,2}", "{2,1}", "{1,}", "\\u{41}", "\\u0041", "\\x41", "\\cA", "\\1", "\\2", "\\0", "[^",
	"\\d", "\\w", "\\b", "\\B", "\\-",
];
for (let i = 0; i < 6000; i++) {
	let s = "";
	const n = 2 + rand(6);
	for (let j = 0; j < n; j++) s += pick(pieces);
	add(s, ["regex"]);
}

// Punycode: random A-label bodies, long ones included (upstream decodes in doubles without overflow checks, Ada in
// checked int32).
const punyAlpha = [..."abcdefghijklmnopqrstuvwxyz0123456789-"];
for (let i = 0; i < 3000; i++) {
	let body = "";
	const n = 1 + rand(i % 3 === 0 ? 59 : 12);
	for (let j = 0; j < n; j++) body += pick(punyAlpha);
	add(`xn--${body}`, ["hostname", "idn-hostname"]);
	add(`a.xn--${body}`, ["idn-hostname"]);
	add(`א.xn--${body}`, ["idn-hostname"]);
	add(`http://xn--${body}/`, ["url"]);
	add(`http://é.xn--${body}/`, ["url"]);
}

// IDN labels: random strings over the code points the label rules single out.
const idnAlpha = [
	..."ab1-.", "l", "·", "͵", "α", "א", "׳", "ا", "ب", "ل", "١", "۱",
	"̀", "ً", "ः", "क", "्", "‌", "‍", "・", "あ", "ア", "一", "ß",
	"ς", "ـ", "­", "​", "️", "ａ", "．", "。", "｡", "é", "é",
	"K", "\u{10400}", "\u{1f600}", "ꡲ", "ܐ", "ހ", "ࡀ", "٠",
];
for (let i = 0; i < 6000; i++) {
	let s = "";
	const n = 1 + rand(7);
	for (let j = 0; j < n; j++) s += pick(idnAlpha);
	add(s, ["hostname", "idn-hostname", "idn-email"]);
	add(`x@${s}`, ["idn-email"]);
	add(`http://${s}/`, ["url", "iri"]);
}

// IP addresses: random sequences of address pieces.
const ipPieces = ["1", "0", "00", "ffff", "FFFF", "12345", "g", ":", "::", ".", "1.2.3.4", "255", "256", "01", "0x1"];
for (let i = 0; i < 3000; i++) {
	let s = "";
	const n = 1 + rand(9);
	for (let j = 0; j < n; j++) s += pick(ipPieces);
	add(s, ["ipv4", "ipv6"]);
	add(`http://[${s}]/`, ["url", "uri", "iri"]);
	add(`http://${s}/`, ["url", "uri"]);
	add(`foo://[${s}]/`, ["url"]);
}

// Dates and times: digit templates with random values.
const two = () => String(rand(100)).padStart(2, "0");
for (let i = 0; i < 3000; i++) {
	const date = `${String(rand(10000)).padStart(4, "0")}-${two()}-${two()}`;
	const offset = pick(["Z", "z", "", `+${two()}:${two()}`, `-${two()}:${two()}`, "+00:00"]);
	const time = `${two()}:${two()}:${pick(["59", "60", "61", two()])}${pick(["", ".5", ".", ".123"])}${offset}`;
	add(date, ["date"]);
	add(time, ["time"]);
	add(`${date}${pick(["T", "t", " "])}${time}`, ["date-time"]);
}
for (const y of [1600, 1700, 1900, 2000, 2004, 2100, 2400, 0, 4, 100]) {
	for (let m = 0; m <= 13; m++) {
		for (const d of [0, 28, 29, 30, 31, 32]) {
			add(`${String(y).padStart(4, "0")}-${String(m).padStart(2, "0")}-${String(d).padStart(2, "0")}`, ["date"]);
		}
	}
}
for (let h = 0; h <= 24; h++) {
	for (const m of [0, 29, 30, 58, 59, 60]) {
		for (const off of ["Z", "+00:00", "-00:00", "+01:00", "-01:00", "+00:30", "-23:59", "+23:59", "+05:45", "-09:30"]) {
			add(`${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}:60${off}`, ["time"]);
		}
	}
}

// Unicode property names: every name V8 accepts somewhere, in every position, plus near misses.
{
	const bin = fs.readFileSync(process.execPath);
	const tokens = new Set();
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
	const ok = (s) => {
		try {
			new RegExp(s, "u");
			return true;
		} catch {
			return false;
		}
	};
	const valid = [...tokens]
		.sort()
		.filter((t) => ok(`\\p{${t}}`) || ok(`\\p{Script=${t}}`) || ok(`\\p{gc=${t}}`));
	for (const t of valid) {
		for (const form of [`\\p{${t}}`, `\\P{${t}}`, `\\p{Script=${t}}`, `\\p{sc=${t}}`, `\\p{scx=${t}}`, `\\p{gc=${t}}`,
			`\\p{General_Category=${t}}`, `\\p{Script_Extensions=${t}}`, `[\\p{${t}}]`, `\\p{${t.toLowerCase()}}`,
			`\\p{${t}_}`, `\\p{${t}=${t}}`]) {
			add(form, ["regex"]);
		}
	}
}

// V8's limits: 32767 capturing groups, quantifier bounds clamped at 2^31-1.
for (const n of [32767, 32768]) add("()".repeat(n), ["regex"]);
for (const q of ["x{2147483647}", "x{2147483648}", "x{2147483648,2147483647}", "x{2147483647,2147483646}",
	"x{0,2147483648}", "(?<a\\u003e)", "(?<a\\u{3e})", "(?<\\u003e>)", "(?<a\\u003e>)"]) {
	add(q, ["regex"]);
}

// ---- capture ------------------------------------------------------------------------------------------------------

// A row is [value, {format: result}] — or, for a value with a lone surrogate, [value, results, results on
// value.toWellFormed()]. The edge cases and fixtures carry every format; the families the formats they exercise.
const rows = [];
const run = (value, fmts) => {
	const check = (v) => Object.fromEntries(fmts.map((f) => [f, formats[f](v)]));
	rows.push(value.isWellFormed() ? [value, check(value)] : [value, check(value), check(value.toWellFormed())]);
};
const seen = new Set();
for (const v of [...fixtureStrings, ...edge]) {
	if (seen.has(v)) continue;
	seen.add(v);
	run(v, names);
}
const seenTargeted = new Map();
for (const [v, fmts] of targeted) {
	if (seen.has(v)) continue;
	const got = seenTargeted.get(v) ?? new Set();
	for (const f of fmts) got.add(f);
	seenTargeted.set(v, got);
}
for (const [v, fmts] of seenTargeted) run(v, [...fmts].sort());

// Sanity: the fast-API hazard the header describes did not leak in.
for (let i = 0; i < 200000; i++) {
	if (!URL.canParse("http://é.com/")) throw new Error("URL.canParse got optimized; run with --no-opt --no-maglev");
}

let checks = 0;
for (const r of rows) checks += Object.keys(r[1]).length;
const head = {
	upstream: sha,
	node: process.version,
	versions: { v8: process.versions.v8, icu: process.versions.icu, unicode: process.versions.unicode, ada: process.versions.ada },
	formats: names,
	checks,
};
// One row per line, so a recapture diffs by value.
const body = rows.map((r) => JSON.stringify(r)).join(",\n");
fs.writeFileSync(outFile, `${JSON.stringify(head).slice(0, -1)},"rows":[\n${body}\n]}\n`);
console.error(`${rows.length} values, ${checks} checks -> ${outFile}`);
