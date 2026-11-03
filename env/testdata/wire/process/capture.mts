// Captures the frames upstream's TypeScript client sends for
// TestProcessSendsUpstreamsFrames (process_test.go): the scenario's
// openProcess calls, in the same order, against a daemon whose input is
// recorded by a tee in front of it.
//
// Copy this file into upstream's packages/env at 9c9ce8547 (it needs no
// node_modules) and run, from there,
//
//	node --experimental-strip-types capture.mts <daemon> <record file> > <path of testdata/wire/process>/frames.json
//
// with a daemon that runs processes (the recorded frames are the client's).
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Connection } from "./src/connection.ts";
import { openProcess } from "./src/process.ts";

const [daemon, record] = process.argv.slice(2) as [string, string];
const root = realpathSync(mkdtempSync(join(tmpdir(), "pi-env-wire-")));
const connection = new Connection({ command: ["sh", "-c", 'r=$0 d=$1; shift; tee "$r" | exec "$d" "$@"', record, daemon] });

await connection.info();
const piped = await openProcess(connection, {
	cwd: root,
	argv: ["sh", "-c", "cat > /dev/null; exit 4"],
	env: { A: "1" },
	inheritEnv: false,
	onStdout: () => {},
});
piped.write("hi");
const big = new Uint8Array(1536 * 1024);
for (let index = 0; index < big.length; index++) big[index] = index % 251;
piped.write(big);
piped.write(new Uint8Array(0));
piped.closeStdin();
await piped.exited;
const closed = await openProcess(connection, { cwd: root, argv: ["sleep", "5"], onStdout: () => {} });
closed.close();
await closed.exited.catch(() => {});
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
	frames.push({
		type,
		id,
		json: json.replaceAll(JSON.stringify(root).slice(1, -1), "<root>"),
		payload: createHash("sha256").update(payload).digest("hex"),
	});
}

rmSync(root, { recursive: true, force: true });
process.stdout.write(`${JSON.stringify({ frames }, null, "\t")}\n`);
process.exit(0);
