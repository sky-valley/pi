// Captures what upstream's TypeScript client does over ssh for
// ssh_oracle_test.go: for each scenario of scenarios.json, the ssh calls it
// makes (arguments and stdin, recorded by fakessh.sh) and where it ends; and
// sshArguments for the targets of scenarios.json.
//
// Copy this file into upstream's packages/env at 479d6e5e2 and run, from
// there,
//
//	node --conditions=source capture.mts <path of testdata/ssh> > <path of testdata/ssh>/oracle.json
//
// (a source extraction as testdata/connection/capture.mts describes). Against
// 479d6e5e2^1's connection.ts it gives the capture of d2e7d8b7c byte for
// byte; 479d6e5e2 adds the daemon's stderr to the start that fails.
import { createHash } from "node:crypto";
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connectSsh, detectPlatform, scanHostKey, sshArguments, sshConnection } from "./src/ssh.ts";

const testdata = process.argv[2]!;
const spec = JSON.parse(readFileSync(join(testdata, "scenarios.json"), "utf8"));
const fake = join(testdata, "fakessh.sh");

const binary = new Uint8Array(spec.binaryLength);
for (let index = 0; index < binary.length; index++) binary[index] = (index * 7919) % 251;

function outcomeOf(error: unknown): unknown {
	const name = (error as Error).constructor.name;
	return { error: { class: name, message: (error as Error).message } };
}

/** The recorded calls, with the random token and scan directory masked. */
function calls(dir: string): unknown[] {
	const recorded: unknown[] = [];
	for (let n = 0; readdirSync(dir).includes(`${n}.argv`); n++) {
		const raw = readFileSync(join(dir, `${n}.argv`), "utf8").split("\0").slice(0, -1);
		const argv = raw.map((argument, index) => (raw[index - 1] === "--token" ? "<token>" : mask(argument)));
		let stdin: string | null = null;
		try {
			stdin = readFileSync(join(dir, `${n}.stdin`)).toString("base64");
		} catch {}
		recorded.push({ argv, stdin });
	}
	return recorded;
}

function mask(argument: string): string {
	return argument.replace(/"[^"]*pi-env-hostkey-[^/"]*\/known_hosts"/, '"<scan>/known_hosts"');
}

const results: unknown[] = [];
for (const scenario of spec.scenarios) {
	const dir = mkdtempSync(join(tmpdir(), "pi-env-fakessh-"));
	scenario.responses.forEach((response: { code: number; out?: string; err?: string; nostdin?: boolean }, n: number) => {
		writeFileSync(join(dir, `${n}.code`), String(response.code));
		if (response.out !== undefined) writeFileSync(join(dir, `${n}.out`), response.out);
		if (response.err !== undefined) writeFileSync(join(dir, `${n}.err`), response.err);
		if (response.nostdin) writeFileSync(join(dir, `${n}.nostdin`), "");
	});
	const binaryFile = join(dir, "daemon.bin");
	writeFileSync(binaryFile, binary);
	process.env.FAKE_SSH_DIR = dir;
	const logs: string[] = [];
	const target = { ...scenario.target, ssh: fake };
	const options = { ...target, binary: binaryFile, loginShell: scenario.loginShell === true, onLog: (text: string) => logs.push(text) };
	let outcome: unknown;
	try {
		if (scenario.api === "connectSsh") {
			const { connection, remote } = await connectSsh(options);
			const start = await connection.info().then(
				() => "started",
				(error: Error) => error.message,
			);
			connection.close();
			outcome = { remote, start };
		} else if (scenario.api === "detectPlatform") {
			outcome = { remote: await detectPlatform(target) };
		} else if (scenario.api === "scanHostKey") {
			outcome = { scan: await scanHostKey(target) };
		} else if (scenario.api === "sshConnection") {
			const { connection, remote } = sshConnection(options);
			const starts: unknown[] = [];
			for (let attempt = 0; attempt < 3; attempt++) {
				starts.push(
					await connection.info().then(
						() => "started",
						(error: Error & { code?: string }) => `${error.code}: ${error.message}`,
					),
				);
				starts.push(remote() ?? null);
			}
			connection.close();
			outcome = { starts };
		}
	} catch (error) {
		outcome = outcomeOf(error);
	}
	results.push({ name: scenario.name, outcome, logs, calls: calls(dir) });
	rmSync(dir, { recursive: true, force: true });
}

const argumentsResults = spec.arguments.map((target: Parameters<typeof sshArguments>[0]) => {
	try {
		return { argv: sshArguments(target) };
	} catch (error) {
		return outcomeOf(error);
	}
});

const sha = createHash("sha256").update(binary).digest("hex");
process.stdout.write(
	`${JSON.stringify({ binarySha256: sha, scenarios: results, arguments: argumentsResults }, null, "\t")}\n`,
);
process.exit(0);
