// Captures, for connection_test.go, how upstream's TypeScript Connection
// fails to start (misbehaving daemons of fakedaemon.sh, a missing program, no
// command, a failing command function, a closed connection), and how its
// RemoteExecutionEnv resolves paths against a POSIX and a Windows daemon's
// hello.
//
// Copy this file into a checkout of upstream's packages/env at d2e7d8b7c (with
// node_modules) and run, from there,
//
//	node capture.mts <path of testdata/connection> > <path of testdata/connection>/starts.json
import { join } from "node:path";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { Connection, type ConnectionOptions, RemoteError } from "./src/connection.ts";
import { RemoteExecutionEnv } from "./src/remote-env.ts";

const testdata = process.argv[2]!;
const script = join(testdata, "fakedaemon.sh");

const cases: [string, ConnectionOptions["command"], boolean?][] = [
	...["exit", "protocol", "noprotocol", "corrupt", "short", "array", "refuse", "noise", "stderr"].map(
		(mode): [string, ConnectionOptions["command"]] => [mode, ["sh", script, mode]],
	),
	["missing program", ["/nonexistent/pi-env-missing"]],
	["no command", []],
	[
		"failing command function",
		async () => {
			throw new Error("ssh gpu-box failed: Network is unreachable");
		},
	],
	["closed", ["sh", script, "noise"], true],
];

const results: unknown[] = [];
for (const [name, command, close] of cases) {
	const logs: string[] = [];
	const connection = new Connection({ command, onLog: (text) => logs.push(text) });
	if (close) connection.close();
	let outcome: unknown;
	try {
		const info = await connection.info();
		outcome = { info };
	} catch (error) {
		if (!(error instanceof RemoteError)) throw error;
		outcome = { code: error.code, message: error.message, lost: error.fields.lost === true };
	}
	await new Promise((resolve) => setTimeout(resolve, 300));
	connection.close();
	results.push({ name, outcome, logs: name === "noise" || name === "stderr" ? logs : null });
}
const inputs = [
	"", ".", "..", "a/b", "a\\b", "/abs/x/../y", "~", "~/a", "~\\a", "~a", "C:\\x", "C:/x", "c:x", "C:", "D:x", "E:x",
	"e:x", "F:x", "Z:x", "\\x", "\\\\srv\\share\\x", "//srv/share/x", "file:///etc/x", "file:///C:/x/../y", "file://host/share/x",
	"file:///a%20b", "file:///a%2Fb", "file:///100%", "file:", "FILE:///x", "file://localhost/x", "file:///D:x",
];
const paths: unknown[] = [];
for (const [mode, cwd] of [
	["posix", "/work/dir"],
	["windows", "C:\\work"],
] as const) {
	const connection = new Connection({ command: ["sh", script, mode] });
	const env = new RemoteExecutionEnv({ connection, id: "pi-env:paths", cwd });
	for (const input of inputs) {
		const resolved = await env.absolutePath(input, BACKGROUND_CONTEXT);
		paths.push({ mode, cwd, input, result: resolved.ok ? resolved.value : { code: resolved.error.code } });
	}
	const joined = await env.joinPath(["a", "..", "b", "c/d"], BACKGROUND_CONTEXT);
	paths.push({ mode, cwd, input: "join a .. b c/d", result: joined.ok ? joined.value : { code: joined.error.code } });
	connection.close();
}
process.stdout.write(`${JSON.stringify({ starts: results, paths }, null, "\t")}\n`);
process.exit(0);
