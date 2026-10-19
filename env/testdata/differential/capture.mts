// Captures NodeExecutionEnv's results for differential_test.go: the random
// file operation sequences and the device/FIFO/directory reads of upstream's
// packages/env/test/differential.test.ts (d2e7d8b7c), run against
// @earendil-works/pi-durable 1.3.0's NodeExecutionEnv on this machine.
//
// Copy this file next to a checkout of upstream's packages/env whose
// node_modules has @earendil-works/pi-durable and chord 1.3.0, then
//
//	node capture.mts > <GOOS>.json
//
// (Node 22.18+ runs TypeScript directly.) Results are normalized as upstream
// normalizes them: the root as <root>, failures as their code, mtimes elided.
// compact then keeps the file small: bytes (Buffer or Uint8Array) become hex,
// and a string longer than 256 characters its SHA-256 and UTF-8 length;
// differential_test.go compacts its own results the same way.
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import type { ExecutionEnv, FileError, Result } from "@earendil-works/pi-durable/env";
import { NodeExecutionEnv } from "@earendil-works/pi-durable/env/node";

const context = BACKGROUND_CONTEXT;
const dirs: string[] = [];

function random(seed: number): () => number {
	let state = seed >>> 0;
	return () => {
		state = (state + 0x6d2b79f5) >>> 0;
		let t = state;
		t = Math.imul(t ^ (t >>> 15), t | 1);
		t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	};
}

function normalize(value: unknown, root: string): unknown {
	const json = JSON.stringify(value, (key, field: unknown) => {
		if (field instanceof Uint8Array) return { bytes: Buffer.from(field).toString("hex") };
		if (key === "mtimeMs") return "mtime";
		if (field && typeof field === "object" && "ok" in field && (field as Result<unknown, FileError>).ok === false) {
			return { ok: false, code: (field as unknown as { error: FileError }).error.code };
		}
		return field;
	});
	if (json === undefined) return undefined;
	const escaped = (path: string) => JSON.stringify(path).slice(1, -1);
	return JSON.parse(json.replaceAll(escaped(realpathSync.native(root)), "<root>").replaceAll(escaped(root), "<root>"));
}

function compact(value: unknown): unknown {
	if (typeof value === "string") {
		if (value.length <= 256) return value;
		const bytes = Buffer.from(value, "utf8");
		return { sha256: createHash("sha256").update(bytes).digest("hex"), length: bytes.length };
	}
	if (Array.isArray(value)) return value.map(compact);
	if (value && typeof value === "object") {
		const record = value as Record<string, unknown>;
		if (record.type === "Buffer" && Array.isArray(record.data)) {
			return { bytes: compact(Buffer.from(record.data as number[]).toString("hex")) };
		}
		return Object.fromEntries(Object.entries(record).map(([key, field]) => [key, compact(field)]));
	}
	return value;
}

const NAMES = ["a.txt", "b", "dir", "dir/c.txt", "dir/sub", "dir/sub/d.bin", "missing", "a.txt/x"];
const CONTENTS: (string | Uint8Array)[] = [
	"",
	"hello\n",
	"﻿bom\r\nline",
	Uint8Array.from([0xe2, 0x82, 0x0a, 0xff, 0xef, 0xbb, 0xbf]),
	"x".repeat(70_000),
	"é".repeat(300_001),
];

type Operation = (env: ExecutionEnv) => Promise<unknown>;

function randomOperation(next: () => number): { label: string; run: Operation } {
	const args: unknown[] = [];
	const pick = <T>(value: T): T => {
		args.push(
			value instanceof Uint8Array
				? "<bytes>"
				: typeof value === "string" && value.length > 20
					? `<${value.length} chars>`
					: value,
		);
		return value;
	};
	const nameValue = () => pick(NAMES[Math.floor(next() * NAMES.length)]!);
	const contentValue = () => pick(CONTENTS[Math.floor(next() * CONTENTS.length)]!);
	const flagValue = () => pick(next() < 0.5);
	const small = (limit: number) => pick(Math.floor(next() * limit));
	const choices: [string, () => Operation][] = [
		["writeFile", () => { const n = nameValue(); const c = contentValue(); return (env) => env.writeFile(n, c, context); }],
		["appendFile", () => { const n = nameValue(); const c = contentValue(); return (env) => env.appendFile(n, c, context); }],
		["readTextFile", () => { const n = nameValue(); return (env) => env.readTextFile(n, context); }],
		["readBinaryFile", () => { const n = nameValue(); return (env) => env.readBinaryFile(n, context); }],
		["readTextLines", () => { const n = nameValue(); const m = small(4); return (env) => env.readTextLines(n, { maxLines: m }, context); }],
		["fileInfo", () => { const n = nameValue(); return (env) => env.fileInfo(n, context); }],
		["exists", () => { const n = nameValue(); return (env) => env.exists(n, context); }],
		["listDir", () => { const n = nameValue(); return (env) => env.listDir(n, context); }],
		["createDir", () => { const n = nameValue(); const r = flagValue(); return (env) => env.createDir(n, { recursive: r }, context); }],
		["remove", () => { const n = nameValue(); const r = flagValue(); const f = flagValue(); return (env) => env.remove(n, { recursive: r, force: f }, context); }],
		["renameFile", () => { const a = nameValue(); const b = nameValue(); return (env) => env.renameFile(a, b, context); }],
		["truncateFile", () => { const n = nameValue(); const size = small(10); return (env) => env.truncateFile(n, size, context); }],
		["flushFile", () => { const n = nameValue(); return (env) => env.flushFile(n, context); }],
		["canonicalPath", () => { const n = nameValue(); return (env) => env.canonicalPath(n, context); }],
		[
			"openBinaryReader",
			() => {
				const n = nameValue();
				const noFollow = flagValue();
				const offset = small(8);
				const length = small(8);
				return async (env) => {
					const reader = await env.openBinaryReader(n, { noFollow }, context);
					if (!reader.ok) return reader;
					try {
						return [await reader.value.read(offset, length, context), await reader.value.scanLines({ startLine: 0 }, context)];
					} finally {
						await reader.value.close(context);
					}
				};
			},
		],
	];
	const [label, make] = choices[Math.floor(next() * choices.length)]!;
	const run = make();
	return { label: `${label}(${args.map((arg) => JSON.stringify(arg)).join(", ")})`, run };
}

const sequences: unknown[] = [];
for (let seed = 1; seed <= 60; seed++) {
	const next = random(seed);
	const root = mkdtempSync(join(tmpdir(), "pi-env-local-"));
	dirs.push(root);
	const local = new NodeExecutionEnv({ cwd: root });
	const steps: unknown[] = [];
	for (let step = 0; step < 25; step++) {
		const operation = randomOperation(next);
		steps.push({ label: operation.label, result: compact(normalize(await operation.run(local), root) ?? null) });
	}
	sequences.push({ seed, steps });
}

let devices: unknown = null;
if (process.platform !== "win32") {
	const root = mkdtempSync(join(tmpdir(), "pi-env-local-"));
	dirs.push(root);
	const env = new NodeExecutionEnv({ cwd: root });
	const fifo = join(root, "fifo");
	const made = await env.exec(["mkfifo", fifo], undefined, context);
	if (!made.ok || made.value.exitCode !== 0) throw new Error("mkfifo failed");
	await env.createDir("dir", undefined, context);
	const write = (text: string) => spawn("sh", ["-c", `printf '%s' "$1" > "$2"`, "sh", text, fifo]);
	write("fifo text\n");
	const fifoText = await env.readTextFile("fifo", context);
	write("fifo bytes");
	const fifoBytes = await env.readBinaryFile("fifo", context);
	write("x");
	const fifoLines = await env.readTextLines("fifo", undefined, context);
	const dirReader = await env.openTextLineReader("dir", context);
	const dirLine = dirReader.ok ? await dirReader.value.readLine(context) : undefined;
	if (dirReader.ok) await dirReader.value.close(context);
	devices = compact(normalize(
		{
			nullText: await env.readTextFile("/dev/null", context),
			nullBytes: await env.readBinaryFile("/dev/null", context),
			nullLines: await env.readTextLines("/dev/null", undefined, context),
			fifoText,
			fifoBytes,
			fifoLines,
			dirReaderOpened: dirReader.ok,
			dirLine,
			dirText: await env.readTextFile("dir", context),
			dirBytes: await env.readBinaryFile("dir", context),
		},
		root,
	));
}

// macOS file systems refuse names that are not UTF-8.
let names: unknown = null;
if (process.platform === "linux" || process.platform === "android") {
	const root = mkdtempSync(join(tmpdir(), "pi-env-local-"));
	dirs.push(root);
	for (const name of [[0x62], [0x61, 0xff], [0xc3, 0xa9], [0x61, 0xfe, 0x7a], [0x7a]]) {
		writeFileSync(Buffer.concat([Buffer.from(`${root}/`), Buffer.from(name)]), "x");
	}
	const env = new NodeExecutionEnv({ cwd: root });
	const reader = await env.openDirReader(".", context);
	const pages: unknown[] = [];
	if (reader.ok) {
		for (let done = false; !done; ) {
			const page = await reader.value.next(2, context);
			pages.push(page);
			done = !page.ok || page.value.done;
		}
		await reader.value.close(context);
	}
	names = compact(normalize({ list: await env.listDir(".", context), pages }, root));
}

for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
const lines = sequences.map((sequence) => JSON.stringify(sequence));
process.stdout.write(
	`{"node":${JSON.stringify(process.version)},"platform":${JSON.stringify(process.platform)},"devices":${JSON.stringify(devices)},"names":${JSON.stringify(names)},"sequences":[\n${lines.join(",\n")}\n]}\n`,
);
process.exit(0);
