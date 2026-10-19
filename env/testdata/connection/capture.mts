// Captures, for connection_test.go, how upstream's TypeScript Connection
// fails to start (misbehaving daemons of fakedaemon.sh, a missing program, no
// command, a failing command function, a closed connection), how its
// RemoteExecutionEnv resolves paths against a POSIX and a Windows daemon's
// hello (also with an aborted context, before the daemon started), and how
// requests fail when the connection is lost: a request larger than a frame,
// and a daemon that stops reading and pinging (this one takes 35 s).
//
// Copy this file into a checkout of upstream's packages/env at d2e7d8b7c (with
// node_modules) and run, from there,
//
//	node capture.mts <path of testdata/connection> <pi-env daemon> > <path of testdata/connection>/starts.json
//
// with the daemon of @earendil-works/pi-env 1.3.0 (bin/pi-env-<platform>-<arch>/pi-env).
import { join } from "node:path";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { Connection, type ConnectionOptions, isConnectionLost, RemoteError } from "./src/connection.ts";
import { RemoteExecutionEnv } from "./src/remote-env.ts";

const testdata = process.argv[2]!;
const daemon = process.argv[3]!;
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

// absolutePath and joinPath do not look at the context: aborted, they still
// wait for the daemon to start and resolve.
const aborted = withAbortSignal(AbortSignal.abort(), BACKGROUND_CONTEXT);
const abortedPaths: unknown[] = [];
for (const operation of ["absolutePath ~/a", "joinPath a b"]) {
	const connection = new Connection({ command: ["sh", script, "posix"] });
	const env = new RemoteExecutionEnv({ connection, id: "pi-env:paths", cwd: "/work/dir" });
	const result =
		operation === "joinPath a b" ? await env.joinPath(["a", "b"], aborted) : await env.absolutePath("~/a", aborted);
	abortedPaths.push({ operation, result: result.ok ? result.value : { code: result.error.code } });
	connection.close();
}

function failure(error: unknown): unknown {
	if (!(error instanceof RemoteError)) throw error;
	return { code: error.code, message: error.message, lost: isConnectionLost(error) };
}

const lost: unknown[] = [];
{
	// The daemon refuses a frame above 16 MiB and exits; the request and the
	// connection are lost, and the next request starts the daemon again.
	const connection = new Connection({ command: [daemon] });
	const env = new RemoteExecutionEnv({ connection, id: "pi-env:lost", cwd: "/" });
	const { pid } = await connection.info();
	let outcome: unknown;
	try {
		await connection.request("lstat", { path: "/" }, { payload: new Uint8Array(17 * 1024 * 1024) });
		outcome = "answered";
	} catch (error) {
		outcome = failure(error);
	}
	const restarted = (await connection.info()).pid !== pid;
	lost.push({ name: "request above a frame", outcome, restarted });
	const { pid: second } = await connection.info();
	const result = await env.exec("x".repeat(17 * 1024 * 1024), undefined, BACKGROUND_CONTEXT);
	lost.push({
		name: "command above a frame",
		outcome: result.ok ? "ran" : { code: result.error.code, message: result.error.message },
		restarted: (await connection.info()).pid !== second,
	});
	connection.close();
}
{
	// A daemon that stops reading and pinging: writes queue up behind the
	// request, aborting it only queues a cancel, and the silence limit (30 s,
	// checked every 5 s) loses the connection.
	const connection = new Connection({ command: ["sh", script, "silent"] });
	await connection.info();
	const started = Date.now();
	const controller = new AbortController();
	setTimeout(() => controller.abort(), 1000);
	let outcome: unknown;
	try {
		await connection.request(
			"write",
			{ path: "/nonexistent/x", append: false, parents: false },
			{ payload: new Uint8Array(4 * 1024 * 1024), signal: controller.signal },
		);
		outcome = "answered";
	} catch (error) {
		outcome = failure(error);
	}
	lost.push({ name: "daemon stops reading", outcome, afterSilenceLimit: Date.now() - started >= 30_000 });
	connection.close();
}
process.stdout.write(`${JSON.stringify({ starts: results, paths, abortedPaths, lost }, null, "\t")}\n`);
process.exit(0);
