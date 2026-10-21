// Captures Node's answers for nodepath_test.go:
//
//	node testdata/capture.mjs > testdata/node.json
//
// Run with the Node version nodepath follows (v26.4.0). The process runs in a
// fixed working directory so path.resolve's fallback to process.cwd() is
// reproducible; the test gives the Go functions the same one.
import { posix, win32 } from "node:path";
import { fileURLToPath } from "node:url";

const cwd = "/usr/bin";
process.chdir(cwd);

const atoms = [
	"", ".", "..", "...", "/", "//", "///", "a", "a/", "a//b", "/a/b/../c", "a/./b", "../a", "../../a/..", "/..",
	"/../a", "a/..", "a/../..", "./", ".a", "..a", "a..", "a/.../b", "a/b/c/../../..", "~", "~/x", "é/ü", "/é/../ü",
	"C:", "C:\\", "C:/", "c:\\a\\..\\b", "C:a", "C:a\\..\\..", "C:.", "C:..\\x", "D:\\x\\y", "\\", "\\\\", "\\\\server",
	"\\\\server\\", "\\\\server\\share", "\\\\server\\share\\", "\\\\server\\share\\a\\..\\b", "//server/share/x",
	"\\\\server\\\\share", "\\\\.\\PHYSICALDRIVE0", "\\\\?\\C:\\x", "\\\\?\\COM1:", "\\\\.\\COM1:\\x", "CON", "CON:",
	"con:x", "NUL.txt", "COM1:\\x", "COM\u00b9:", "a:b", "a:\\b", "ab:", "x\\y:z", "x/y:/z", "\\a\\b", "/a\\b",
	"a\\b/c", "C:\\..\\..", "\\\\server\\share\\..", "C:\\a\\", "a\\", ".\\a", "a\\.\\b\\..", "\\\\\\a", "///a/b",
	"CONx", "lpt1.txt:", "C:\\COM1:", "a/b:c/d",
];

// Extra inputs for toNamespacedPath (Node's fs bindings namespace every
// path on Windows): long, device and UNC forms, and shell paths.
const namespaceAtoms = [
	"\\\\?\\UNC\\server\\share\\x", "\\\\?\\c:\\a\\..\\b", "\\\\.\\pipe\\x", "//server/share/a/../b",
	"C:\\Program Files\\Git\\bin\\bash.exe", "c:/Windows/System32/bash.exe", "C:\\a\\..\\..\\x",
	"D:\\Users\\test\\AppData\\Local\\Microsoft\\WindowsApps\\pwsh.exe", "\\\\server\\share\\dir\\bash.exe",
	"C:x\\y", "\\x\\y", "x\\y", "C:\\", "\\\\?\\", "\\\\.\\", "\\\\a", "ab",
];

const posixBases =["/home/u", "/", "rel/dir", "", "/a/b/"];
const win32Bases = ["C:\\work", "c:\\", "\\\\srv\\share\\dir", "D:", "rel", "", "\\top"];

const cases = [];
const record = (fn, args, run) => {
	try {
		cases.push({ fn, args, result: run() });
	} catch (error) {
		cases.push({ fn, args, error: String(error?.message ?? error) });
	}
};

for (const [name, flavor, bases] of [
	["posix", posix, posixBases],
	["win32", win32, win32Bases],
]) {
	for (const a of atoms) {
		record(`${name}.normalize`, [a], () => flavor.normalize(a));
		record(`${name}.isAbsolute`, [a], () => flavor.isAbsolute(a));
		record(`${name}.basename`, [a], () => flavor.basename(a));
		record(`${name}.resolve`, [a], () => flavor.resolve(a));
		for (const base of bases) record(`${name}.resolve`, [base, a], () => flavor.resolve(base, a));
		for (const b of atoms) record(`${name}.join`, [a, b], () => flavor.join(a, b));
	}
	for (const a of [...atoms, ...namespaceAtoms]) {
		record(`${name}.toNamespacedPath`, [a], () => flavor.toNamespacedPath(a));
	}
	record(`${name}.resolve`, [], () => flavor.resolve());
	record(`${name}.join`, [], () => flavor.join());
	record(`${name}.join`, ["a", "", "b", "..", "c"], () => flavor.join("a", "", "b", "..", "c"));
}

const urls = [
	"file:///a/b", "file:///a%20b", "file:///a b", "file:///a%2Fb", "file:///a%2fb", "file:///a%5Cb", "file:///a%5cb",
	"file:///C:/x", "file:///c|/x", "file:///C:", "file:///C:/", "file:///C:/../x", "file:///C:/a/..", "file:///C:/..",
	"file:///../C:/x", "file:///C:\\x", "file:///a/../../b", "file:///a/./b/%2e/c/%2E%2e/d", "file:///%2e%2e/x",
	"file:///x/%2e%2E", "file:///x/.%2e/y", "file:///x/%2e./y", "file:///x/..", "file:///x/.", "file:///x/./",
	"file://localhost/etc", "file://LOCALHOST/etc", "file://local%68ost/x", "file://localhost", "file://server/share/x",
	"file://SERVER/share", "file://ex%41mple/x", "file://1.2.3/x", "file://0x7f.1/x", "file://010.0.0.1/x",
	"file://256.1.1.1/x", "file://1.2.3.4.5/x", "file://1.2.3.4./x", "file://4294967295/x", "file://4294967296/x",
	"file://0x/x", "file://a.0x/x", "file://a.b.1/x", "file://[::1]/x", "file://a b/x", "file://a%20b/x",
	"file://a%zzb/x", "file://a^b/x", "file://a|b/x", "file://a_b/x", "file://C:/x", "file://C|/x", "file://C:",
	"file:////server/share", "file://", "file:///", "file:///a?b#c", "file:///a#b", "file:///a?b/c", "file:///é",
	"file:///%C3%A9", "file:///%E9", "file:///100%", "file:///%zz", "file:///%", "file:///a\tb", " file:///a ",
	"file:///a\nb\r", "file://\\x", "file:///x\\y", "file://host", "file://h/C:/x", "file://h/C|/x",
	"file://xn--nxasmq6b/x", "file://é/x", "file:///a%00b", "file:///%F0%9F%98%80", "file:///%ED%A0%80",
	"file:///%C0%80", "file:///%E2%82", "file:///a/b/", "file:///a//b", "file://h/%2fx", "file:///a`b{c}d^e\"f<g>h",
	"file:///\u0001x", "file:///\u007fx", "file:///C%3A/x", "file:///c:/x", "file://h", "file://h?x", "file://h#x",
];
for (const url of urls) {
	for (const windows of [false, true]) {
		record(`fileURLToPath`, [url, windows], () => fileURLToPath(url, { windows }));
	}
}

// One case per line keeps the file diffable.
const lines = cases.map((entry) => JSON.stringify(entry));
process.stdout.write(`{"node":${JSON.stringify(process.version)},"cwd":${JSON.stringify(cwd)},"cases":[\n${lines.join(",\n")}\n]}\n`);
