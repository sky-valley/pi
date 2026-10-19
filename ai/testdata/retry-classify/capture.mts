// Captures how real pi classifies provider error text — the oracle behind
// TestIsRetryableAssistantErrorMatchesPi and TestProviderErrorPatternSourcesMatchPi
// in package ai.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> classify-d2e7d8b7c.json d2e7d8b7c
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies (the npm
// build's). It is a src capture, checked against the build when a release
// ships the source it read (the BUILD wins): classify-d2e7d8b7c.json was
// captured from src at d2e7d8b7c with the 1.3.0 build's node_modules. v1.3.0
// (71023aec1) ships every alternative but the five 0fbdbd3f8 and 698a2d23b
// added after it: its dist/utils/retry.js builds the limit pattern with exactly
// this source and the retryable one with this source less those five, both "i".
//
// Every row records three answers: isRetryableAssistantError on the message
// retry.test.ts would build (fauxAssistantMessage), and RegExp.prototype.test of
// the two module-private patterns it consults. Those constants are read by
// evaluating retry.ts's own text with an export appended, not a copy.
//
// The rows are pi's own retry.test.ts literals, the stop-reason guard, one row
// per pattern alternative that only that alternative matches (checked below:
// the capture fails if deleting any alternative would leave every row's answer
// unchanged), and the inputs where a JavaScript RegExp without the "u" flag
// and Go's RE2 disagree: "." never matches \r, U+2028 or U+2029, and an astral
// character is two UTF-16 code units a single "." cannot span; "i" folds ASCII
// letters only, so U+212A KELVIN SIGN is not "k" and U+017F LONG S is not "s";
// \S excludes \v and the Unicode spaces, where RE2's excludes ASCII space only.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { fauxAssistantMessage } = await load("providers/faux.ts");
const { isRetryableAssistantError } = await load("utils/retry.ts");

const retryTs = path.join(src, "utils/retry.ts");
const patternsModule = path.join(src, "utils/.retry-patterns.tmp.mts");
fs.writeFileSync(
	patternsModule,
	fs.readFileSync(retryTs, "utf8") +
		"\nexport { NON_RETRYABLE_PROVIDER_LIMIT_ERROR_PATTERN, RETRYABLE_PROVIDER_ERROR_PATTERN };\n",
);
let limitPattern: RegExp;
let retryablePattern: RegExp;
try {
	({
		NON_RETRYABLE_PROVIDER_LIMIT_ERROR_PATTERN: limitPattern,
		RETRYABLE_PROVIDER_ERROR_PATTERN: retryablePattern,
	} = await import(pathToFileURL(patternsModule).href));
} finally {
	fs.rmSync(patternsModule);
}

type Row = { covers: string; stopReason?: string; errorMessage?: string; content?: string };
const error = (covers: string, errorMessage: string): Row => ({ covers, stopReason: "error", errorMessage });

// retry.test.ts constants at the sha.
const openAIExplicitRetryMessage =
	"An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. Please include the request ID req_******** in your message.";
const bedrockExplicitRetryMessage =
	'{"message":"The system encountered an unexpected error during processing. Try your request again."}';
const nvidiaNIMResourceExhaustedMessage = "ResourceExhausted: Worker local total request limit reached (288/48)";
const bunFetchSocketClosedMessage =
	"The socket connection was closed unexpectedly. For more information, pass `verbose: true` in the second argument to fetch()";
const openAIResponsesEarlyEofMessage = "OpenAI Responses stream ended before a terminal response event";
const wrappedDnsLookupError =
	"The pending stream has been canceled (caused by: getaddrinfo ENOTFOUND bedrock-runtime.us-east-1.amazonaws.com)";
const chatGPTUsageLimitMessage =
	'OpenAI API error (429): {"code":"subscription_sharing_usage_limit_exceeded","message":"Usage limit reached."}';
const azurePeakLoadError =
	"The system is currently experiencing high demand and cannot process your request. Your request exceeds the maximum usage size allowed during peak load. For improved capacity reliability, consider switching to Provisioned Throughput.";

const rows: Row[] = [
	// retry.test.ts "provider retry classification" and retryAssistantCall literals.
	error("pi: OpenAI explicit retry guidance", openAIExplicitRetryMessage),
	error("pi: Bedrock explicit retry guidance", bedrockExplicitRetryMessage),
	error("pi: NVIDIA NIM ResourceExhausted", nvidiaNIMResourceExhaustedMessage),
	error("pi: Bun fetch socket drop", bunFetchSocketClosedMessage),
	error("pi: upstream request buffer exhaustion", "Error: exceeded request buffer limit while retrying upstream"),
	error("pi: wrapped DNS lookup failure", wrappedDnsLookupError),
	error("pi: DNS ENOTFOUND", "connect ENOTFOUND api.example.com"),
	error("pi: DNS EAI_AGAIN", "EAI_AGAIN api.example.com"),
	error("pi: DNS getaddrinfo", "getaddrinfo failed for api.example.com"),
	error("pi: OpenAI Responses early EOF", openAIResponsesEarlyEofMessage),
	error("pi: limit wins over a retryable status", "429 quota exceeded"),
	error("pi: overloaded_error", "overloaded_error"),
	error("pi: Cloudflare 520 (#9627)", "520 status code (no body)"),
	error("pi: Azure peak load (#9669)", azurePeakLoadError),
	error("pi: Cloudflare 524", "524 status code (no body)"),
	{ covers: "pi: not an error", content: "not an error" },
	error("pi: retryAssistantCall non-retryable", "insufficient_quota"),
	error("pi: retryAssistantCall transient", "terminated"),
	error("pi: ChatGPT subscription usage limit", chatGPTUsageLimitMessage),
	error("pi: temporary ChatGPT usage error", "subscription_sharing_usage_unavailable: Usage cannot be checked."),
	error("pi: temporary ChatGPT user error", "subscription_sharing_user_unavailable: User cannot be loaded."),
	error("pi: HTTP/2 pending stream cancellation (#10379)", "The pending stream has been canceled"),
	error("pi: HTTP/2 pending stream cancellation (#10379)", "The pending stream has been canceled (caused by: socket closed)"),
	error("pi: service temporarily unavailable (#10965)", "The service is temporarily unavailable. Please retry later."),
	error("pi: Bedrock ServiceUnavailableException", "Service unavailable: The service is temporarily unavailable."),
	error("pi: node-fetch request failure (#10996)", "request to https://oauth2.googleapis.com/token failed, reason:"),
	error(
		"pi: node-fetch request failure (#10996)",
		"request to https://oauth2.googleapis.com/token failed, reason: connect ECONNREFUSED 127.0.0.1:9",
	),
	error("pi: gateway upstream failure (#10920)", "unknown: Unable to forward upstream stream"),
	error("pi: gateway upstream failure (#10920)", "upstream_error: Upstream request failed"),
	error("pi: gateway upstream failure (#10920)", "upstream_stream_read_error: Upstream response stream was interrupted"),
	error("pi: gateway upstream failure (#10920)", "Upstream HTTP/2 stream failed"),
	// CHANGELOG wording of the alternatives whose commits added no test.
	error("pi: model at capacity (#10278)", "Selected model is at capacity"),
	error("pi: server busy (#10543)", "server_busy"),
	error("pi: server busy (#10543)", "servers are currently busy"),
	error("pi: upstream provider returned an error (#10842)", "The upstream provider returned an error"),

	// isRetryableAssistantError's guard: only stopReason "error" with error text.
	{ covers: "guard: stop", stopReason: "stop", errorMessage: "overloaded" },
	{ covers: "guard: length", stopReason: "length", errorMessage: "overloaded" },
	{ covers: "guard: toolUse", stopReason: "toolUse", errorMessage: "overloaded" },
	{ covers: "guard: aborted", stopReason: "aborted", errorMessage: "overloaded" },
	error("guard: empty error text", ""),
	{ covers: "guard: no error text", stopReason: "error" },

	// One row per retryable alternative that no other alternative matches.
	error("overloaded", "Overloaded"),
	error("server_busy", "Server_Busy"),
	error("servers are currently busy", "The Servers Are Currently Busy"),
	error("currently experiencing high demand", "Currently Experiencing High Demand"),
	error("model is at capacity", "Model Is At Capacity"),
	error("rate.?limit", "rate_limit_error"),
	error("too many requests", "Too Many Requests"),
	error("429", "HTTP 429"),
	error("500", "status 500"),
	error("502", "status 502"),
	error("503", "status 503"),
	error("504", "status 504"),
	error("520", "status 520"),
	error("524", "status 524"),
	error("service.?(?:is.?)?(?:temporarily.?)?unavailable", "Service Unavailable"),
	error("service.?(?:is.?)?(?:temporarily.?)?unavailable", "service is unavailable"),
	error("service.?(?:is.?)?(?:temporarily.?)?unavailable", "Service-Temporarily-Unavailable"),
	error("service.?(?:is.?)?(?:temporarily.?)?unavailable", "serviceistemporarilyunavailable"),
	error("server.?error", "server_error"),
	error("internal.?error", "InternalError"),
	error("provider.?returned.?(?:an.?)?error", "Provider returned error"),
	error("provider.?returned.?(?:an.?)?error", "provider_returned_an_error"),
	error("provider.?returned.?(?:an.?)?error", "provider returned anerror"),
	error("unable to forward upstream", "UNABLE TO FORWARD UPSTREAM"),
	error("upstream.?request.?failed", "upstream_request_failed"),
	error("upstream_stream_read_error", "Upstream_Stream_Read_Error"),
	error("stream was interrupted", "Stream Was Interrupted"),
	error("http/?2 stream failed", "http2 stream failed"),
	error("http/?2 stream failed", "HTTP/2 Stream Failed"),
	error("network.?error", "NetworkError when attempting to fetch resource."),
	error("connection.?error", "Connection error."),
	error("connection.?refused", "connection refused"),
	error("connection.?lost", "Connection lost"),
	error("other side closed", "SocketError: other side closed"),
	error("fetch failed", "TypeError: fetch failed"),
	error("request to \\S+ failed, reason:", "FetchError: Request to https://x failed, reason: socket hang"),
	error("request to \\S+ failed, reason:", "request to x failed, reason:"),
	error("getaddrinfo", "GETADDRINFO"),
	error("ENOTFOUND", "enotfound"),
	error("EAI_AGAIN", "eai_again"),
	error("upstream.?connect", "upstream connect error"),
	error("reset before headers", "reset before headers"),
	error("socket hang up", "Socket Hang Up"),
	error("socket connection was closed", "socket connection was closed"),
	error("timed? out", "Request timed out."),
	error("timed? out", "time out"),
	error("timeout", "Request timeout"),
	error("terminated", "Terminated"),
	error("websocket.?closed", "WebSocket closed"),
	error("websocket.?error", "WebSocket error"),
	error("ended without", "stream ended without message_stop"),
	error("stream ended before message_stop", "Anthropic stream ended before message_stop"),
	error("http2 request did not get a response", "http2 request did not get a response"),
	error("pending stream has been canceled", "PENDING STREAM HAS BEEN CANCELED"),
	error("retry delay", "Retry delay 120000ms exceeds maxRetryDelayMs"),
	error("you can retry your request", "the model is busy; you can retry your request"),
	error("try your request again", "please try your request again shortly"),
	error("please retry your request", "transient failure, please retry your request"),
	error("ResourceExhausted", "resourceexhausted"),
	error("subscription_sharing_usage_unavailable", "SUBSCRIPTION_SHARING_USAGE_UNAVAILABLE"),
	error("subscription_sharing_user_unavailable", "Subscription_Sharing_User_Unavailable"),

	// One row per limit alternative, each beside a retryable status it must veto.
	error("GoUsageLimitError", "GoUsageLimitError: 429"),
	error("FreeUsageLimitError", "freeusagelimiterror 503"),
	error("Monthly usage limit reached", "Monthly usage limit reached (429)"),
	error("available balance", "Enable Available Balance usage: 429"),
	error("insufficient_quota", "insufficient_quota (429)"),
	error("out of budget", "out of budget 500"),
	error("quota exceeded", "Quota Exceeded: 429"),
	error("billing", "BILLING 502"),
	error("subscription_sharing_usage_limit_exceeded", "Subscription_Sharing_Usage_Limit_Exceeded (429)"),

	// Limit near-misses: the retryable status stands.
	error("near-miss limit", "GoUsageLimit 429"),
	error("near-miss limit", "Monthly usage limit 503"),
	error("near-miss limit", "available-balance 429"),
	error("near-miss limit", "insufficient quota 429"),
	error("near-miss limit", "out-of-budget 500"),
	error("near-miss limit", "quota exceed 429"),
	error("near-miss limit", "bill 502"),
	error("near-miss limit", "subscription sharing usage limit exceeded 429"),

	// Retryable near-misses.
	error("near-miss", "model refused to answer"),
	error("near-miss", "status 501"),
	error("near-miss", "status 505"),
	error("near-miss", "status 521"),
	error("near-miss", "42 9"),
	error("near-miss", "status 5200"),
	error("near-miss", "too many request"),
	error("near-miss", "service not available"),
	error("near-miss", "service is temporarily  unavailable"),
	error("near-miss", "service was temporarily unavailable"),
	error("near-miss", "service temporarily is unavailable"),
	error("near-miss", "server busy"),
	error("near-miss", "server is currently busy"),
	error("near-miss", "model at capacity"),
	error("near-miss", "unable to forward downstream"),
	error("near-miss", "upstream read error"),
	error("near-miss", "stream interrupted"),
	error("near-miss", "http/1.1 stream failed"),
	error("near-miss", "http//2 stream failed"),
	error("near-miss", "pending stream has been cancelled"),
	error("near-miss", "request to  failed, reason:"),
	error("near-miss", "request to a b failed, reason:"),
	error("near-miss", "request to https://x failed reason:"),
	error("near-miss", "request to https://x failed, reason"),
	error("near-miss", "subscription_sharing_usage unavailable"),
	error("near-miss", "server-side error"),
	error("near-miss", "provider returned a error"),
	error("near-miss", "provider returned  an error"),
	error("near-miss", "ECONNREFUSED 127.0.0.1:443"),
	error("near-miss", "RESOURCE_EXHAUSTED"),
	error("near-miss", "resource exhausted"),
	error("near-miss", "timd out"),
	error("near-miss", "timedout"),
	error("near-miss", "ETIMEDOUT"),
	error("near-miss", "line1\n520 status code (no body)"),

	// "." is one UTF-16 code unit other than a line terminator.
	error("dot", "rate-limit"),
	error("dot", "ratelimit"),
	error("dot", "rate  limit"),
	error("dot", "rate\tlimit"),
	error("dot", "rate\u0000limit"),
	error("dot", "rate\u0085limit"),
	error("dot", "rate\u00e9limit"),
	error("dot", "rate\uFEFFlimit"),
	error("dot", "rate\uFFFDlimit"),
	error("dot", "rate\nlimit"),
	error("dot: JS excludes CR", "rate\rlimit"),
	error("dot: JS excludes U+2028", "rate\u2028limit"),
	error("dot: JS excludes U+2029", "rate\u2029limit"),
	error("dot: astral is two code units", "rate\u{1F600}limit"),
	error("dot: astral is two code units", "rate\u{10000}limit"),
	error("dot: JS excludes CR", "websocket\rclosed"),
	error("dot: JS excludes U+2028", "provider\u2028returned error"),
	error("dot: astral is two code units", "Service\u{1F600}Unavailable"),
	error("dot: astral is two code units", "service is\u{1F600}unavailable"),
	error("dot: JS excludes U+2028", "provider returned an\u2028error"),
	error("dot: JS excludes CR", "service\ris temporarily unavailable"),
	error("dot: in a group", "service\u00e9is\u00e9temporarily\u00e9unavailable"),
	error("dot: in a group", "provider returned an\uFEFFerror"),

	// \S is one UTF-16 code unit that is not JavaScript whitespace: an astral
	// character is two such units, and \v, U+00A0, U+1680, U+2000-U+200A, U+202F,
	// U+205F, U+3000 and U+FEFF are whitespace, which RE2's ASCII \S matches.
	error("\\S: astral", "request to https://\u{1F600}.example failed, reason:"),
	error("\\S: non-ASCII", "request to https://b\u00fccher.example failed, reason:"),
	error("\\S: U+FFFD", "request to \uFFFD failed, reason:"),
	error("\\S: JS excludes \\t", "request to a\tb failed, reason:"),
	error("\\S: JS excludes \\v", "request to a\vb failed, reason:"),
	error("\\S: JS excludes \\f", "request to a\fb failed, reason:"),
	error("\\S: JS excludes U+00A0", "request to a\u00a0b failed, reason:"),
	error("\\S: JS excludes U+1680", "request to a\u1680b failed, reason:"),
	error("\\S: JS excludes U+2000", "request to a\u2000b failed, reason:"),
	error("\\S: JS excludes U+200A", "request to a\u200ab failed, reason:"),
	error("\\S: JS excludes U+2028", "request to a\u2028b failed, reason:"),
	error("\\S: JS excludes U+2029", "request to a\u2029b failed, reason:"),
	error("\\S: JS excludes U+202F", "request to a\u202fb failed, reason:"),
	error("\\S: JS excludes U+205F", "request to a\u205fb failed, reason:"),
	error("\\S: JS excludes U+3000", "request to a\u3000b failed, reason:"),
	error("\\S: JS excludes U+FEFF", "request to a\ufeffb failed, reason:"),
	error("\\S: U+180E is not a space", "request to a\u180eb failed, reason:"),
	error("\\S: U+200B is not a space", "request to a\u200bb failed, reason:"),
	error("\\S: U+0085 is not a space", "request to a\u0085b failed, reason:"),

	// "i" folds ASCII letters only.
	error("fold: U+212A is not k", "soc\u212Aet hang up"),
	error("fold: U+017F is not s", "\u017Focket hang up"),
	error("fold: U+212A is not k", "networ\u212A error"),
	error("fold: U+017F is not s", "too many reque\u017Fts"),
	error("fold: U+017F is not s in a limit", "in\u017Fufficient_quota 429"),
	error("fold: U+0131 is not i", "t\u0131meout"),
	error("fold: U+0130 is not I", "T\u0130MEOUT"),
	error("fold: U+017F is not s", "up\u017Ftream_stream_read_error"),
	error("fold: U+017F is not s", "\u017Ferver_busy"),
	error("fold: U+017F is not s in a group", "service i\u017F temporarily unavailable"),
	error("fold: U+017F is not s before \\S", "Reque\u017Ft to https://x failed, reason:"),
];

// Every alternative must decide at least one row on its own: for a retryable
// alternative, a row it alone matches that no limit alternative vetoes; for a
// limit alternative, a row it alone matches that a retryable alternative would
// otherwise retry.
function checkCoverage(pattern: RegExp, other: RegExp, wantOther: boolean) {
	const alternatives = pattern.source.split("|");
	for (const alternative of alternatives) {
		const decides = rows.some((row) => {
			if (row.stopReason !== "error" || !row.errorMessage) return false;
			const matching = alternatives.filter((a) => new RegExp(a, pattern.flags).test(row.errorMessage!));
			return matching.length === 1 && matching[0] === alternative && other.test(row.errorMessage) === wantOther;
		});
		if (!decides) throw new Error(`no row is decided by ${JSON.stringify(alternative)} alone; add one`);
	}
}
checkCoverage(retryablePattern, limitPattern, false);
checkCoverage(limitPattern, retryablePattern, true);

const out = {
	sha,
	patterns: {
		NON_RETRYABLE_PROVIDER_LIMIT_ERROR_PATTERN: { source: limitPattern.source, flags: limitPattern.flags },
		RETRYABLE_PROVIDER_ERROR_PATTERN: { source: retryablePattern.source, flags: retryablePattern.flags },
	},
	rows: rows.map((row) => {
		const options: Record<string, string> = {};
		if (row.stopReason !== undefined) options.stopReason = row.stopReason;
		if (row.errorMessage !== undefined) options.errorMessage = row.errorMessage;
		const message = fauxAssistantMessage(row.content ?? "", options);
		return {
			covers: row.covers,
			stopReason: message.stopReason,
			...(message.errorMessage === undefined
				? {}
				: {
						errorMessage: message.errorMessage,
						limitPattern: limitPattern.test(message.errorMessage),
						retryablePattern: retryablePattern.test(message.errorMessage),
					}),
			retryable: isRetryableAssistantError(message),
		};
	}),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`captured ${out.rows.length} rows at ${sha} -> ${outFile}`);
