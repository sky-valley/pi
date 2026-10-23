// pi side of the differential: build each scenario's request body with REAL pi
// and write it out canonically. No API key and no network — `onPayload` grabs
// the body and throws, which aborts the stream before the first byte is sent.
//
// Two backends, because the port is ahead of the last npm release:
//   "dist" -> the published @earendil-works/pi-ai build (strongest ground truth)
//   "src"  -> upstream TypeScript at the synced sha, run directly under Node's
//             type stripping (the ONLY ground truth for changes ported before
//             they shipped; the published build does not contain them)

import { readdir, readFile, mkdir, writeFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { canonicalize } from "./canon.mjs";

const HERE = path.dirname(new URL(import.meta.url).pathname);
const ROOT = path.dirname(HERE);
const SCENARIOS = path.join(ROOT, "scenarios");
const OUT = path.join(ROOT, "out", "pi");

const NPM_DIR = process.env.PI_NPM_DIR;
const SRC_DIR = path.join(ROOT, "pisrc", process.env.PI_UPSTREAM_SHA ?? "");

function moduleFor(backend, api) {
	if (backend === "dist") {
		return path.join(NPM_DIR, "node_modules/@earendil-works/pi-ai/dist/api", `${api}.js`);
	}
	if (backend === "src") return path.join(SRC_DIR, "src/api", `${api}.ts`);
	throw new Error(`scenario backend must be "dist" or "src", got ${JSON.stringify(backend)}`);
}

/**
 * The context an adapter entry point takes. From upstream 9e05370b2 on, api
 * `stream`/`streamSimple` take a TranscriptContext that only normalizeContext
 * produces — the public entry points normalize before dispatching — and an
 * adapter handed the raw context instead silently drops the system prompt and
 * the tools.
 *
 * This is decided by PROBING the backend, not by assuming which one is ahead.
 * It used to early-return for "dist" on the grounds that the published build
 * predated the change; 0.86.1 ships it, and that assumption turned 46 of 58
 * scenarios red in a single npm bump — every one of them reading as a port bug
 * (pi "missing" the system message and the tools) when the harness was the
 * thing feeding pi the wrong shape. 0.85.1 has no dist/utils/transcript.js at
 * all, so the probe is exact in both directions.
 */
async function adapterContext(scenario) {
	const transcriptPath =
		scenario.backend === "src"
			? path.join(SRC_DIR, "src/utils/transcript.ts")
			: path.join(NPM_DIR, "node_modules/@earendil-works/pi-ai/dist/utils/transcript.js");
	if (!existsSync(transcriptPath)) return scenario.context;
	const { normalizeContext } = await import(pathToFileURL(transcriptPath).href);
	return normalizeContext(scenario.context);
}

/**
 * A scenario that sets `catalogModel: "<provider>/<id>"` claims its `model` is a
 * verbatim copy of that entry in the published catalog. Hold it to that against
 * the build under test. A regen can move fields the scenario's request never
 * reads, and then the stale copy still passes: baseten-catalog-kimi (now -glm) missed the
 * 0.87.0 regen's inputLimits that way, through two regens.
 */
async function checkCatalogCopy(scenario) {
	const key = scenario.catalogModel;
	if (key === undefined) return;
	if (scenario.backend !== "dist") {
		throw new Error(`catalogModel is checked against the published catalog, so the scenario's backend must be "dist"`);
	}
	const slash = key.indexOf("/");
	const [provider, id] = [key.slice(0, slash), key.slice(slash + 1)];
	const generated = path.join(NPM_DIR, "node_modules/@earendil-works/pi-ai/dist/models.generated.js");
	const { MODELS } = await import(pathToFileURL(generated).href);
	const entry = slash > 0 ? MODELS[provider]?.[id] : undefined;
	if (entry === undefined) {
		throw new Error(`catalogModel ${JSON.stringify(key)} is not in ${generated}; name a "<provider>/<id>" the catalog has, or drop catalogModel`);
	}
	const want = JSON.stringify(entry);
	if (JSON.stringify(scenario.model) !== want) {
		throw new Error(
			`model is a stale copy of catalog ${key}. Replace it with this build's entry, ` +
				`JSON.stringify(MODELS[${JSON.stringify(provider)}][${JSON.stringify(id)}]) from ${generated} ` +
				`(key order included), and update the note's "refreshed at" version:\n${want}`,
		);
	}
}

/** Map scenario options (pi-shaped, camelCase) onto the pi options object. */
function buildOptions(scenario, capture) {
	const o = { ...(scenario.options ?? {}) };
	o.onPayload = capture;
	// Never let a real request escape even if onPayload were to be bypassed.
	o.maxRetries = 0;
	return o;
}

async function captureScenario(scenario) {
	await checkCatalogCopy(scenario);
	const modPath = moduleFor(scenario.backend, scenario.api);
	if (!existsSync(modPath)) {
		throw new Error(
			`pi backend module missing: ${modPath}\n` +
				(scenario.backend === "dist"
					? `Install it: (cd ${NPM_DIR} && npm i)`
					: `Extract it: run.sh does this from $PI_UPSTREAM_DIR at $PI_UPSTREAM_SHA`),
		);
	}
	const mod = await import(pathToFileURL(modPath).href);
	const entry = scenario.entry === "streamSimple" ? mod.streamSimple : mod.stream;
	if (typeof entry !== "function") {
		throw new Error(`module ${modPath} has no ${scenario.entry ?? "stream"} export`);
	}

	let captured;
	const halt = Symbol("halt");
	const options = buildOptions(scenario, (payload) => {
		captured = payload;
		throw halt;
	});

	const final = await entry(scenario.model, await adapterContext(scenario), options).result();
	if (captured === undefined) {
		throw new Error(
			`payload was never built (stream ended ${final?.stopReason}: ${final?.errorMessage ?? ""})`,
		);
	}
	return captured;
}

/**
 * Restrict a payload to the declared top-level paths ("$.contents", ...).
 *
 * This exists for ONE reason: google-generative-ai's onPayload observes a
 * different layer on each side. pi hands the @google/genai SDK a call-params
 * object ({model, contents, config}) and the SDK builds the REST body inside
 * itself — pi explicitly refuses a custom fetch, so the real REST body is not
 * observable from pi at all. The Go port has no such SDK and builds the REST
 * body directly. Only `contents` is the same artifact on both sides, so the
 * google scenarios compare that and NOTHING ELSE. Everything outside it
 * (generationConfig / tools / systemInstruction) is out of the differential's
 * reach — see README "Known limits".
 */
function project(payload, comparePaths) {
	if (!comparePaths || comparePaths.length === 0) return payload;
	const out = {};
	for (const p of comparePaths) {
		const m = /^\$\.([A-Za-z0-9_]+)$/.exec(p);
		if (!m) throw new Error(`comparePaths only supports "$.<topLevelKey>", got ${p}`);
		if (!(m[1] in payload)) throw new Error(`comparePaths ${p}: key absent from payload`);
		out[m[1]] = payload[m[1]];
	}
	return out;
}

async function main() {
	await mkdir(OUT, { recursive: true });
	const only = process.env.DIFF_ONLY;
	const files = (await readdir(SCENARIOS)).filter((f) => f.endsWith(".json")).sort();
	let failures = 0;

	for (const file of files) {
		const scenario = JSON.parse(await readFile(path.join(SCENARIOS, file), "utf8"));
		if (only && scenario.name !== only) continue;
		try {
			const payload = project(await captureScenario(scenario), scenario.comparePaths);
			// The wire bytes: exactly what pi would have POSTed.
			const wire = JSON.stringify(payload);
			const { body, order } = canonicalize(wire);
			await writeFile(path.join(OUT, `${scenario.name}.raw.json`), wire + "\n");
			await writeFile(path.join(OUT, `${scenario.name}.body.json`), body);
			await writeFile(path.join(OUT, `${scenario.name}.order.txt`), order);
			console.log(`pi  ok    ${scenario.name} (${scenario.backend})`);
		} catch (err) {
			failures++;
			console.error(`pi  ERROR ${scenario.name}: ${err?.stack ?? err}`);
		}
	}
	if (failures) process.exit(1);
}

await main();
