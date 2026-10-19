// Captures the frames upstream's TypeScript client sends for wire_test.go:
// the operations of wire_test.go's scenario, in the same order, against a
// daemon whose input is recorded by a tee in front of it.
//
// Copy this file into a checkout of upstream's packages/env at d2e7d8b7c
// (with node_modules) and run, from there,
//
//	node capture.mts <daemon> <record file> > <path of testdata/wire>/frames.json
//
// with any daemon of protocol 1 (the recorded frames are the client's).
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { getOrThrow } from "@earendil-works/pi-durable/env";
import { Connection } from "./src/connection.ts";
import { RemoteExecutionEnv } from "./src/remote-env.ts";
import { openTerminal } from "./src/terminal.ts";

const [daemon, record] = process.argv.slice(2) as [string, string];
const context = BACKGROUND_CONTEXT;
const root = realpathSync(mkdtempSync(join(tmpdir(), "pi-env-wire-")));
const connection = new Connection({ command: ["sh", "-c", 'r=$0 d=$1; shift; tee "$r" | exec "$d" "$@"', record, daemon] });
const env = new RemoteExecutionEnv({ connection, id: "pi-env:wire", cwd: root, shellEnv: { Z: "z" } });
const shellEnv = new RemoteExecutionEnv({
	connection,
	id: "pi-env:wire",
	cwd: root,
	shellPath: "/bin/sh",
	watch: { mode: "polling", pollIntervalMs: 100, maxDirectories: 50 },
});

const { tmpdir: remoteTmpdir } = await connection.info();
getOrThrow(await env.writeFile("a.txt", "hello", context));
getOrThrow(await env.appendFile("a.txt", " world", context));
const big = new Uint8Array(600 * 1024);
for (let index = 0; index < big.length; index++) big[index] = index % 253;
getOrThrow(await env.writeFile("big.bin", big, context));
getOrThrow(await env.readTextFile("a.txt", context));
getOrThrow(await env.readBinaryFile("big.bin", context));
getOrThrow(await env.fileInfo("a.txt", context));
getOrThrow(await env.listDir(".", context));
const dirReader = getOrThrow(await env.openDirReader(".", context));
getOrThrow(await dirReader.next(10, context));
await dirReader.close(context);
const reader = getOrThrow(await env.openBinaryReader("a.txt", { noFollow: true }, context));
getOrThrow(await reader.read(0, 5, context));
getOrThrow(await reader.scanLines({ startLine: 0, endLine: 1 }, context));
getOrThrow(await reader.info(context));
await reader.close(context);
const lines = getOrThrow(await env.openTextLineReader("a.txt", context));
getOrThrow(await lines.readLine(context));
await lines.close(context);
getOrThrow(await env.readTextLines("a.txt", { maxLines: 1 }, context));
getOrThrow(await env.truncateFile("a.txt", 3, context));
getOrThrow(await env.flushFile("a.txt", context));
getOrThrow(await env.renameFile("a.txt", "b.txt", context));
getOrThrow(await env.canonicalPath("b.txt", context));
getOrThrow(await env.exists("b.txt", context));
getOrThrow(await env.createDir("d/e", undefined, context));
getOrThrow(await env.createDir("f", { recursive: false }, context));
getOrThrow(await env.remove("d", { recursive: true, force: true }, context));
getOrThrow(await env.createTempDir("x-", context));
getOrThrow(await env.createTempFile({ prefix: "p-", suffix: ".s" }, context));
getOrThrow(
	await env.exec(
		["sh", "-c", "exit 3"],
		{
			cwd: "f",
			env: { A: "1" },
			inheritEnv: false,
			timeout: 5,
			spill: { afterBytes: 10, afterLines: 2 },
			window: { maxBytes: 100, maxLines: 5, minIntervalMs: 10, bytesPerSecond: 1000 },
		},
		context,
	),
);
getOrThrow(await shellEnv.exec("printf hi", { env: { B: "2" } }, context));
getOrThrow(
	await env.listFiles(
		".",
		{ hidden: false, gitignore: false, exclude: ["node_modules"], maxEntries: 10, maxBytes: 1000, timeoutMs: 5000 },
		context,
	),
);
getOrThrow(await env.listFiles(".", undefined, context));
const watcher = getOrThrow(
	await shellEnv.watch([{ path: "w", recursive: true, exclude: { hidden: true, names: ["n"] } }, { path: "b.txt" }], () => {}, context),
);
await watcher.close(context);
const controller = new AbortController();
const aborted = env.exec(["sleep", "5"], undefined, withAbortSignal(controller.signal, context));
setTimeout(() => controller.abort(), 100);
await aborted;
const killed = env.exec(["sleep", "5"], undefined, context);
await new Promise((resolve) => setTimeout(resolve, 100));
await env.cleanup(context);
await killed;
const terminal = await openTerminal(connection, {
	cwd: root,
	argv: ["sh", "-c", "read x; exit 0"],
	env: { TERM: "xterm" },
	inheritEnv: true,
	cols: 80,
	rows: 24,
	onData: () => {},
});
terminal.resize(100, 30);
await terminal.foreground();
terminal.write("ok\n");
await terminal.exited;
connection.close();

// The recorded stream: frames after nothing (the client writes no sync line).
const stream = readFileSync(record);
const frames: unknown[] = [];
for (let offset = 0; offset + 4 <= stream.length; ) {
	const length = stream.readUInt32BE(offset);
	const type = stream.readUInt8(offset + 4);
	const id = stream.readUInt32BE(offset + 5);
	const jsonLength = stream.readUInt32BE(offset + 9);
	const json = stream.subarray(offset + 13, offset + 13 + jsonLength).toString("utf8");
	const payload = stream.subarray(offset + 13 + jsonLength, offset + 4 + length);
	offset += 4 + length;
	if (type === 6) continue;
	frames.push({ type, id, json: mask(json), payload: createHash("sha256").update(payload).digest("hex") });
}

/** The scenario's root, the daemon's temporary directory, and its random names, which differ between runs. */
function mask(json: string): string {
	return json
		.replaceAll(JSON.stringify(root).slice(1, -1), "<root>")
		.replaceAll(`"${JSON.stringify(remoteTmpdir).slice(1, -1)}`, '"<tmpdir>')
		.replace(/x-[A-Za-z0-9]{6}/g, "x-<random>")
		.replace(/tmp-[A-Za-z0-9]{6}/g, "tmp-<random>")
		.replace(/p-[0-9a-f-]{36}\.s/g, "p-<uuid>.s");
}

rmSync(root, { recursive: true, force: true });
process.stdout.write(`${JSON.stringify({ frames }, null, "\t")}\n`);
process.exit(0);
