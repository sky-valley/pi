// Captures what real pi sends, and fails with, when the anthropic provider
// authenticates by workload identity federation (upstream a9424cd43) — the
// oracle behind TestAnthropicFederationMatchesPi.
//
//   node capture.mjs <pi npm dir> <out.json>
//   e.g. node capture.mjs ~/.cache/pi-npm/0.99.2 anthropic-federation-0.99.2.json
//
// pi hands the federation config to @anthropic-ai/sdk (0.124.0, what upstream's
// package-lock.json locks at a9424cd43 and what the 0.99.2 build ships), and it
// is the SDK that exchanges the identity token, caches the access token and
// sends it. So no fake client here: each row streams the published build's
// anthropic-messages adapter, with its real SDK client, against a loopback
// server that answers POST /v1/oauth/token with the row's exchange responses
// (one per exchange, the last repeated) and POST /v1/messages with an SSE body
// (or the row's error statuses, one per request). A row runs `requests`
// streams back to back, `waitMs` apart, under the row's env; the identity token
// file holds the row's tokenFile text (no file when it is null; {repeat, count}
// for a long one, which recorded strings then write as <tokenFile>).
//
// Each row records every exchange (method, path, the headers the SDK sets, and
// the body as sent), every messages request's auth headers, and each stream's
// stopReason and errorMessage. The loopback base URL and the token file path
// are written as <base> and <token-file>. undici's own request headers
// (accept, accept-language, sec-fetch-mode, accept-encoding, ...) are left
// out, as the port leaves them out everywhere.
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [npmDir, outFile] = process.argv.slice(2);
if (!npmDir || !outFile) {
	console.error("usage: node capture.mjs <pi npm dir> <out.json>");
	process.exit(2);
}
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const sdkVersion = JSON.parse(
	fs.readFileSync(path.join(npmDir, "node_modules/@anthropic-ai/sdk/package.json"), "utf8"),
).version;
const { stream } = await import(pathToFileURL(path.join(pkg, "dist/api/anthropic-messages.js")).href);

// pi reads the federation variables from options.env, then process.env; keep
// the ambient environment out of every row.
for (const name of Object.keys(process.env)) {
	if (name.startsWith("ANTHROPIC_")) delete process.env[name];
}

// The model upstream's anthropic-federation tests stream.
const model = {
	id: "claude-test",
	name: "Claude Test",
	api: "anthropic-messages",
	provider: "anthropic",
	baseUrl: "https://api.anthropic.com",
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 100000,
	maxTokens: 4096,
};

const sse = [
	`event: message_start\ndata: ${JSON.stringify({
		type: "message_start",
		message: { id: "msg_test", usage: { input_tokens: 1, output_tokens: 0 } },
	})}\n`,
	`event: message_delta\ndata: ${JSON.stringify({
		type: "message_delta",
		delta: { stop_reason: "end_turn" },
		usage: { output_tokens: 1 },
	})}\n`,
	`event: message_stop\ndata: ${JSON.stringify({ type: "message_stop" })}\n`,
].join("\n");

const ids = {
	ANTHROPIC_FEDERATION_RULE_ID: "fdrl_test",
	ANTHROPIC_ORGANIZATION_ID: "org-test",
	ANTHROPIC_IDENTITY_TOKEN_FILE: "<token-file>",
};
const json = (body, extra = {}) => ({ status: 200, headers: { "content-type": "application/json", ...extra }, body: JSON.stringify(body) });
const granted = (token, expiresIn = 3600) => json({ access_token: token, expires_in: expiresIn });

// Rows: name, env, tokenFile, exchanges (recorded as responses), and optionally
// requests, waitMs, statuses (of the messages responses), base ("closed" for a
// port nothing listens on, or a literal base URL), baseSuffix, provider,
// apiKey, headers, payloadBetas (an onPayload that sets params.betas).
const cases = [
	{ name: "exchangesOnceAcrossRequests", env: ids, tokenFile: "  header.payload.signature\n", exchanges: [granted("federated-token")], requests: 3 },
	{
		name: "optionalIdsJoinTheExchangeBody",
		env: { ...ids, ANTHROPIC_SERVICE_ACCOUNT_ID: "svac_test", ANTHROPIC_WORKSPACE_ID: "wrkspc_test" },
		tokenFile: "header.payload.signature",
		exchanges: [json({ access_token: "federated-token", expires_in: 3600, token_type: "bearer" })],
	},
	{ name: "trailingSlashesLeaveTheBase", env: ids, tokenFile: "jwt", exchanges: [granted("t")], baseSuffix: "//" },
	{ name: "consumerBetaHeaderIsReplacedByBetas", env: ids, tokenFile: "jwt", exchanges: [granted("t")], headers: { "anthropic-beta": "custom-beta" } },
	{ name: "emptyBetasGetTheOAuthBetaAppended", env: ids, tokenFile: "jwt", exchanges: [granted("t")], payloadBetas: [] },
	{ name: "betasGetTheOAuthBetaAppended", env: ids, tokenFile: "jwt", exchanges: [granted("t")], payloadBetas: ["a-beta", "b-beta"] },
	{ name: "presentOAuthBetaIsNotRepeated", env: ids, tokenFile: "jwt", exchanges: [granted("t")], payloadBetas: ["a-beta", " oauth-2025-04-20 "] },
	{ name: "expiringTokenIsExchangedPerRequest", env: ids, tokenFile: "jwt", exchanges: [granted("t1", 10), granted("t2", 10)], requests: 2 },
	{
		name: "advisoryWindowServesTheCachedTokenAndRefreshes",
		env: ids,
		tokenFile: "jwt",
		exchanges: [granted("t1", 60), granted("t2")],
		requests: 3,
		waitMs: 300,
	},
	{ name: "stringExpiresInIsANumber", env: ids, tokenFile: "jwt", exchanges: [json({ access_token: "t", expires_in: "3600" })], requests: 2 },
	{ name: "messages401InvalidatesTheToken", env: ids, tokenFile: "jwt", exchanges: [granted("t1"), granted("t2")], statuses: [401], requests: 3 },
	{
		name: "exchange401HintsAtTheWorkspace",
		env: ids,
		tokenFile: "jwt",
		exchanges: [{ status: 401, headers: { "request-id": "req_x" }, body: JSON.stringify({ error: "invalid_grant", assertion: "secret", error_description: "no match" }) }],
	},
	{
		name: "exchange401WithAWorkspace",
		env: { ...ids, ANTHROPIC_WORKSPACE_ID: "wrkspc_test" },
		tokenFile: "jwt",
		exchanges: [{ status: 401, headers: {}, body: JSON.stringify({ error: "invalid_grant" }) }],
	},
	{ name: "exchange500Text", env: ids, tokenFile: "jwt", exchanges: [{ status: 500, headers: {}, body: "upstream down" }] },
	{ name: "exchange400Array", env: ids, tokenFile: "jwt", exchanges: [{ status: 400, headers: {}, body: "[1,2]" }] },
	{ name: "nonJSONTokenResponse", env: ids, tokenFile: "jwt", exchanges: [{ status: 200, headers: {}, body: "not json" }] },
	{ name: "missingAccessToken", env: ids, tokenFile: "jwt", exchanges: [json({ expires_in: 3600, refresh_token: "r", error: "e" })] },
	{ name: "missingExpiresIn", env: ids, tokenFile: "jwt", exchanges: [json({ access_token: "t", error_uri: "u" })] },
	{ name: "unsupportedTokenType", env: ids, tokenFile: "jwt", exchanges: [json({ access_token: "t", expires_in: 3600, token_type: "MAC" })] },
	{ name: "insecureBaseURL", env: ids, tokenFile: "jwt", exchanges: [], base: "http://example.invalid/" },
	{ name: "unreachableTokenEndpoint", env: ids, tokenFile: "jwt", exchanges: [], base: "closed" },
	{ name: "identityTokenFileMissing", env: ids, tokenFile: null, exchanges: [] },
	{ name: "identityTokenFileBlank", env: ids, tokenFile: " \n\t", exchanges: [] },
	{ name: "identityTokenTooLarge", env: ids, tokenFile: { repeat: "a", count: 16 * 1024 + 1 }, exchanges: [] },
	{ name: "identityTokenAtTheLimit", env: ids, tokenFile: { repeat: "a", count: 16 * 1024 }, exchanges: [granted("t")] },
	{ name: "missingRuleIdIsNotFederated", env: { ANTHROPIC_ORGANIZATION_ID: "org-test", ANTHROPIC_IDENTITY_TOKEN_FILE: "<token-file>" }, tokenFile: "jwt", exchanges: [] },
	{ name: "otherProvidersAreNotFederated", env: ids, tokenFile: "jwt", exchanges: [], provider: "kimi-coding" },
	{ name: "explicitApiKeyWins", env: ids, tokenFile: "jwt", exchanges: [granted("t")], apiKey: "sk-explicit" },
	{ name: "authorizationHeaderWins", env: ids, tokenFile: "jwt", exchanges: [granted("t")], headers: { Authorization: "Bearer auth-token" } },
];

const pick = (headers, names) => Object.fromEntries(names.map((n) => [n, headers[n] ?? null]));

async function run(row, tmp) {
	const tokenFile = path.join(tmp, `${row.name}.jwt`);
	const repeated = row.tokenFile && typeof row.tokenFile === "object" ? row.tokenFile.repeat.repeat(row.tokenFile.count) : null;
	if (row.tokenFile !== null) fs.writeFileSync(tokenFile, repeated ?? row.tokenFile);
	const exchanges = [];
	const messages = [];
	let exchangeIndex = 0;
	let messageIndex = 0;
	const srv = http.createServer((req, res) => {
		const chunks = [];
		req.on("data", (c) => chunks.push(c));
		req.on("end", () => {
			const body = Buffer.concat(chunks).toString("utf8");
			if (req.url.endsWith("/v1/oauth/token")) {
				exchanges.push({
					method: req.method,
					path: req.url,
					headers: pick(req.headers, ["content-type", "anthropic-beta", "user-agent", "authorization", "x-api-key"]),
					body,
				});
				const r = row.exchanges[Math.min(exchangeIndex++, row.exchanges.length - 1)];
				res.writeHead(r.status, r.headers);
				res.end(r.body);
				return;
			}
			messages.push({ method: req.method, path: req.url, headers: pick(req.headers, ["authorization", "x-api-key", "anthropic-beta"]) });
			const status = row.statuses?.[messageIndex++] ?? 200;
			if (status !== 200) {
				res.writeHead(status, { "content-type": "application/json" });
				res.end(JSON.stringify({ type: "error", error: { type: "authentication_error", message: "token rejected" } }));
				return;
			}
			res.writeHead(200, { "content-type": "text/event-stream" });
			res.end(sse);
		});
	});
	await new Promise((r) => srv.listen(0, "127.0.0.1", r));
	let base = `http://127.0.0.1:${srv.address().port}`;
	if (row.base === "closed") {
		srv.close();
	} else if (row.base) {
		base = row.base;
	}
	const env = Object.fromEntries(Object.entries(row.env).map(([k, v]) => [k, v === "<token-file>" ? tokenFile : v]));
	const results = [];
	try {
		for (let i = 0; i < (row.requests ?? 1); i++) {
			if (i > 0 && row.waitMs) await new Promise((r) => setTimeout(r, row.waitMs));
			const options = { env, maxRetries: 0 };
			if (row.apiKey) options.apiKey = row.apiKey;
			if (row.headers) options.headers = row.headers;
			if (row.payloadBetas) options.onPayload = (params) => ({ ...params, betas: row.payloadBetas });
			const message = await stream(
				{ ...model, provider: row.provider ?? model.provider, baseUrl: base + (row.baseSuffix ?? "") },
				{ messages: [{ role: "user", content: "Hello", timestamp: 1 }] },
				options,
			).result();
			results.push({ stopReason: message.stopReason, errorMessage: message.errorMessage ?? null });
		}
		// A background refresh may still be in flight.
		await new Promise((r) => setTimeout(r, 100));
	} finally {
		srv.close();
	}
	const scrub = (s) => {
		if (typeof s !== "string") return s;
		s = s.split(base).join("<base>").split(tokenFile).join("<token-file>");
		return repeated ? s.split(repeated).join("<tokenFile>") : s;
	};
	const scrubDeep = (v) =>
		typeof v === "string" ? scrub(v)
		: v && typeof v === "object" ? Object.fromEntries(Object.entries(v).map(([k, x]) => [k, scrubDeep(x)]))
		: v;
	const { exchanges: responses, ...spec } = row;
	return { ...spec, responses, exchanges: exchanges.map(scrubDeep), messages, results: results.map(scrubDeep) };
}

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "pi-anthropic-federation-"));
const rows = [];
try {
	for (const row of cases) rows.push(await run(row, tmp));
} finally {
	fs.rmSync(tmp, { recursive: true, force: true });
}
// One row per line, so a re-capture diffs row by row.
const text = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(
	outFile,
	`{"pi-ai":${JSON.stringify(version)},"@anthropic-ai/sdk":${JSON.stringify(sdkVersion)},"model":${JSON.stringify(model)},"rows":[\n${text}\n]}\n`,
);
console.log(`captured ${rows.length} rows from @earendil-works/pi-ai ${version} -> ${outFile}`);
