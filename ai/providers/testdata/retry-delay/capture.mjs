// Captures how long real pi waits before retrying a 429, per Retry-After header
// shape — the oracle behind TestRetryDelayMatchesPi.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/0.99.2 retry-delay-0.99.2.json
//
// Each row drives the published build's retryProviderRequest with one failing
// attempt whose error carries the row's headers, and records the delay pi hands
// to setTimeout (or the error it throws instead of sleeping). The row runs
// twice, with Math.random pinned to 0 and to 0.5: a server-dictated delay is the
// same both times, the computed exponential backoff is not, which is what
// "backoff" records.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const { retryProviderRequest } = await import(pathToFileURL(path.join(pkg, "dist/utils/provider-retry.js")).href);

const cases = [
	[["retry-after", "5"]],
	[["retry-after-ms", "250"]],
	[["retry-after", "120"]],
	[["retry-after", "1e21"]],
	[["retry-after", "later"]],
	[["retry-after", "Infinity"]],
	[["retry-after", "1e306"]],
	[["retry-after-ms", "1e400"]],
	[["retry-after-ms", "Infinity"]],
	[["retry-after-ms", "-Infinity"]],
	[["retry-after-ms", "Infinity"], ["retry-after", "2"]],
	[["retry-after-ms", "abc"], ["retry-after", "3"]],
	[["retry-after-ms", "1e400"], ["retry-after", "later"]],
	// A date in the past is a finite, negative delay pi clamps to an immediate,
	// server-dictated retry. An ISO date never reaches Date.parse: parseFloat
	// reads its year as seconds first.
	[["retry-after", "Mon, 01 Jan 2001 00:00:00 GMT"]],
	[["retry-after", "2001-01-01T00:00:00Z"]],
	[["retry-after", "2001-01-01"]],
];

const realSetTimeout = globalThis.setTimeout;
const realRandom = Math.random;
async function run(headers, random) {
	let slept;
	Math.random = () => random;
	globalThis.setTimeout = (fn, ms) => {
		slept = ms;
		return realSetTimeout(fn, 0);
	};
	let attempt = 0;
	try {
		await retryProviderRequest(
			async () => {
				if (attempt++ > 0) return "ok";
				const error = new Error("429 slow down");
				error.status = 429;
				error.headers = new Headers(headers);
				throw error;
			},
			{ maxRetries: 1 },
		);
		return { ms: slept };
	} catch (error) {
		return { error: error.message };
	} finally {
		globalThis.setTimeout = realSetTimeout;
		Math.random = realRandom;
	}
}

const rows = [];
for (const headers of cases) {
	const a = await run(headers, 0);
	const b = await run(headers, 0.5);
	rows.push(a.error !== undefined ? { headers, error: a.error } : { headers, ms: a.ms, backoff: a.ms !== b.ms });
}
fs.writeFileSync(outFile, `${JSON.stringify({ piVersion: version, rows }, null, "\t")}\n`);
console.error(`${rows.length} rows from pi-ai ${version}`);
