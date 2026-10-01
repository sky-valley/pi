package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// openaiResponsesMinOutputTokens is the floor OpenAI Responses enforces on
// max_output_tokens — the API rejects values below 16 (#6265).
const openaiResponsesMinOutputTokens = 16

// chatGPTUsageURL is where a Sign in with ChatGPT user checks the
// subscription's usage (pi CHATGPT_USAGE_URL).
const chatGPTUsageURL = "https://chatgpt.com/settings/usage"

// isChatGPTSignIn reports whether a request to OpenAI itself authenticates with
// a Sign in with ChatGPT access token (pi isChatGPTSignIn, upstream 02eed88fd):
// OpenAI API keys start with "sk-", so any other credential sent there is such
// a token. apiKey is the caller's option, not one resolved from the
// environment, and the base URL is the model's own, compared exactly. An empty
// apiKey is pi's undefined, no key; pi reads an explicit "" as a key, which a
// Go caller cannot express.
func isChatGPTSignIn(model *ai.Model, apiKey string) bool {
	return model.Provider == "openai" && model.BaseURL == "https://api.openai.com/v1" &&
		apiKey != "" && !strings.HasPrefix(apiKey, "sk-")
}

// withChatGPTUsageHint points at the usage page when the subscription's usage
// limit, which Sign in with ChatGPT shares with other apps, failed the stream.
// pi applies it to every error its stream catches.
func withChatGPTUsageHint(message string) string {
	if strings.Contains(message, "subscription_sharing_usage_limit_exceeded") {
		return message + "\nCheck your ChatGPT usage: " + chatGPTUsageURL
	}
	return message
}

// openaiToolCallProviders are the providers whose tool-call ids carry the
// Responses-specific `callId|itemId` shape (port of OPENAI_TOOL_CALL_PROVIDERS).
var openaiToolCallProviders = map[string]bool{
	"openai":       true,
	"openai-codex": true,
	"opencode":     true,
}

// responsesCompat is the resolved Responses-API compatibility profile
// (port of OpenAIResponsesCompat, defaults true).
type responsesCompat struct {
	SupportsDeveloperRole bool
	// SupportsMidConvoSystemMessages reports whether later system messages are
	// sent in place as instruction-role messages; otherwise they are folded into
	// the leading one. Default: false.
	SupportsMidConvoSystemMessages bool
	// SessionAffinityFormat selects the session-affinity header shape (pi
	// SessionAffinityFormat). Auto-detected from provider/baseURL.
	SessionAffinityFormat string
	// SupportsLongCacheRetention reports whether the provider supports long
	// prompt cache retention. Upstream 17de82d7b: this is spelled
	// `prompt_cache_options.ttl: "30m"` on GPT-5.6+ and
	// `prompt_cache_retention: "24h"` on earlier models. Default: true.
	SupportsLongCacheRetention bool
	// SupportsStrictMode reports whether the provider supports strict
	// JSON-schema function tools. Default: false; the generated OpenAI models
	// enable it explicitly.
	SupportsStrictMode bool
	// SupportsOpenAIGrammarTools reports whether to emit OpenAI custom tools with
	// Lark/regex grammar formats. When false, grammar-constrained tools fall back
	// to normal function tools. Default: false.
	SupportsOpenAIGrammarTools bool
	// SupportsAdditionalTools reports whether the model accepts message-anchored
	// `additional_tools` input items. It outranks SupportsToolSearch. Default:
	// false.
	SupportsAdditionalTools bool
	// SupportsToolSearch reports whether the model supports client-executed tool
	// search for transcript-anchored additions. Default: false.
	SupportsToolSearch bool
	// SupportsExplicitPromptCacheMode reports whether the model accepts
	// `prompt_cache_options` (OpenAI GPT-5.6+ prompt caching). Older
	// OpenAI models reject the parameter. Default: false.
	SupportsExplicitPromptCacheMode bool
	// SupportsMaxOutputTokens reports whether the provider accepts the
	// `max_output_tokens` parameter. Some Codex-protocol gateways reject it.
	// Default: true.
	SupportsMaxOutputTokens bool
}

// detectResponsesSessionAffinityFormat is pi's detectSessionAffinityFormat:
// openrouter when the provider is openrouter or the baseURL points at it.
func detectResponsesSessionAffinityFormat(model *ai.Model) string {
	return sessionAffinityFormatFor(model.Provider == "openrouter" || strings.Contains(model.BaseURL, "openrouter.ai"))
}

func getResponsesCompat(model *ai.Model) responsesCompat {
	c := responsesCompat{
		SupportsDeveloperRole:      true,
		SessionAffinityFormat:      detectResponsesSessionAffinityFormat(model),
		SupportsLongCacheRetention: true,
		SupportsToolSearch:         false,
		SupportsMaxOutputTokens:    true,
	}
	// Each key is resolved on its own, as pi's `model.compat?.<key> ?? default`
	// does — see compatOverrides.
	o := newCompatOverrides(model.Compat)
	applyCompat(o, "supportsDeveloperRole", &c.SupportsDeveloperRole)
	applyCompat(o, "supportsMidConvoSystemMessages", &c.SupportsMidConvoSystemMessages)
	applyCompat(o, "sessionAffinityFormat", &c.SessionAffinityFormat)
	applyCompat(o, "supportsLongCacheRetention", &c.SupportsLongCacheRetention)
	applyCompat(o, "supportsStrictMode", &c.SupportsStrictMode)
	applyCompat(o, "supportsOpenAIGrammarTools", &c.SupportsOpenAIGrammarTools)
	applyCompat(o, "supportsAdditionalTools", &c.SupportsAdditionalTools)
	applyCompat(o, "supportsToolSearch", &c.SupportsToolSearch)
	applyCompat(o, "supportsExplicitPromptCacheMode", &c.SupportsExplicitPromptCacheMode)
	applyCompat(o, "supportsMaxOutputTokens", &c.SupportsMaxOutputTokens)
	return c
}

// textSignatureV1 is the encoded provider metadata carried on assistant text
// blocks for Responses replay (port of TextSignatureV1).
type textSignatureV1 struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Phase string `json:"phase,omitempty"`
}

func encodeTextSignatureV1(id, phase string) string {
	payload := textSignatureV1{V: 1, ID: id}
	if phase != "" {
		payload.Phase = phase
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// parseTextSignature decodes a textSignature, falling back to legacy plain-string
// id handling (port of parseTextSignature).
func parseTextSignature(signature string) (id, phase string, ok bool) {
	if signature == "" {
		return "", "", false
	}
	if strings.HasPrefix(signature, "{") {
		var parsed textSignatureV1
		if json.Unmarshal([]byte(signature), &parsed) == nil {
			if parsed.V == 1 && parsed.ID != "" {
				if parsed.Phase == "commentary" || parsed.Phase == "final_answer" {
					return parsed.ID, parsed.Phase, true
				}
				return parsed.ID, "", true
			}
		}
		// Fall through to legacy plain-string handling.
	}
	return signature, "", true
}

// normalizeResponsesIDPart ports pi's normalizeIdPart (shared :98-102). The JS
// regex [^a-zA-Z0-9_-] (no /u flag) operates per UTF-16 code unit, so an astral
// character (a surrogate pair) becomes TWO underscores, and .length/.slice are
// UTF-16-unit based — the sanitized string is pure ASCII, so byte ops match.
func normalizeResponsesIDPart(part string) string {
	units := utf16.Encode([]rune(part))
	sanitized := make([]byte, len(units))
	for i, u := range units {
		switch {
		case u >= 'a' && u <= 'z', u >= 'A' && u <= 'Z', u >= '0' && u <= '9', u == '_', u == '-':
			sanitized[i] = byte(u)
		default:
			sanitized[i] = '_'
		}
	}
	s := string(sanitized)
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.TrimRight(s, "_")
}

// shortHash is a fast deterministic hash to shorten long strings (port of
// shortHash). JS charCodeAt iterates UTF-16 code units, so astral characters
// feed two surrogate halves into the hash — utf16.Encode matches that.
func shortHash(str string) string {
	var h1 uint32 = 0xdeadbeef
	var h2 uint32 = 0x41c6ce57
	for _, u := range utf16.Encode([]rune(str)) {
		ch := uint32(u)
		h1 = imul(h1^ch, 2654435761)
		h2 = imul(h2^ch, 1597334677)
	}
	h1 = imul(h1^(h1>>16), 2246822507) ^ imul(h2^(h2>>13), 3266489909)
	h2 = imul(h2^(h2>>16), 2246822507) ^ imul(h1^(h1>>13), 3266489909)
	return base36(h2) + base36(h1)
}

// utf16Length is JS String.prototype.length (UTF-16 code units).
func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func imul(a, b uint32) uint32 { return a * b }

func base36(n uint32) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [7]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}

func buildForeignResponsesItemID(itemID string) string {
	normalized := "fc_" + shortHash(itemID)
	if len(normalized) > 64 {
		normalized = normalized[:64]
	}
	return normalized
}

// OpenAIResponsesOptions are provider-native options for the Responses API.
type OpenAIResponsesOptions struct {
	ai.StreamOptions
	ReasoningEffort  string
	ReasoningSummary string
	// ServiceTier is OpenAI's service_tier request param ("auto", "default",
	// "fast", "flex", "priority"); it also scales cost (flex ×0.5, priority
	// and fast ×2).
	ServiceTier string
	// ToolChoice mirrors pi's OpenAIResponsesOptions.toolChoice: the Responses
	// API tool_choice param ("auto"|"none"|"required" or an object). Sent
	// verbatim when set; nil leaves the param off (the API defaults to "auto").
	ToolChoice any
}

// StreamSimpleOpenAIResponses maps unified reasoning to Responses options.
func StreamSimpleOpenAIResponses(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	o := &OpenAIResponsesOptions{}
	if opts != nil {
		o.StreamOptions = opts.StreamOptions
		if opts.ToolChoice != "" {
			o.ToolChoice = string(opts.ToolChoice)
		}
		if opts.Reasoning != "" {
			clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(opts.Reasoning))
			if clamped != "off" {
				o.ReasoningEffort = string(clamped)
			}
		}
	}
	// pi buildBaseOptions: maxTokens = clamp(options?.maxTokens ?? model.maxTokens).
	// The request's samplingParams pass through as they are; buildResponsesParams
	// merges them over the model's (upstream c01f687e5).
	mt := ai.ClampMaxTokensToContext(model, req, ai.SimpleMaxTokensDefault(model, opts))
	o.MaxTokens = &mt
	return StreamOpenAIResponses(ctx, model, req, o)
}

// StreamOpenAIResponses streams from an OpenAI Responses API (/responses).
// The transcript is resolved first: later system messages stay in place for a
// model with compat.supportsMidConvoSystemMessages and are folded into the
// leading one otherwise, so the request, the grammar tool properties and the
// Copilot headers all read the resolved transcript.
func StreamOpenAIResponses(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *OpenAIResponsesOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &OpenAIResponsesOptions{}
	}
	normalized := ai.ResolveTranscript(req, getResponsesCompat(model).SupportsMidConvoSystemMessages)

	go func() {
		output := &ai.AssistantMessage{
			Content: ai.ContentList{}, Api: model.Api, Provider: model.Provider, Model: model.ID,
			StopReason: ai.StopPending, Timestamp: nowMillis(),
		}
		fail := func(err error) {
			if ctx != nil && ctx.Err() != nil {
				output.StopReason = ai.StopAborted
			} else {
				output.StopReason = ai.StopError
			}
			output.ErrorMessage = withChatGPTUsageHint(err.Error())
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: output.StopReason, Error: output})
			stream.End()
		}

		apiKey, keyErr := clientAPIKey(model.Provider, opts.APIKey, opts.Headers)
		if keyErr != nil {
			fail(keyErr)
			return
		}

		// pi createClient runs before onPayload: Cloudflare providers resolve
		// {VAR} placeholders in baseUrl from the environment, failing the
		// stream when a variable is unset (openai-responses.ts:212-223).
		baseURL := model.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		if isCloudflareProvider(model.Provider) {
			resolved, cfErr := resolveCloudflareBaseURL(model, opts.Env)
			if cfErr != nil {
				fail(cfErr)
				return
			}
			baseURL = resolved
		}

		// Resolved before the request body, matching pi: a bad grammar tool must
		// fail the stream with its own message rather than a downstream one.
		grammarProps, err := grammarToolInputProperties(ai.GetDeclaredTools(normalized.Messages), getResponsesCompat(model).SupportsOpenAIGrammarTools)
		if err != nil {
			fail(err)
			return
		}
		// pi createClient (openai-responses.ts, upstream 87af49dec)
		// builds ONE header object and hands it to the SDK as
		// `defaultHeaders`; headerObject is that object, slots and all.
		// It opens with pi's runtime user agent —
		// `{"User-Agent": getPiUserAgent(), ...model.headers}` — so it is a
		// default every later source outranks, xai included.
		o := &headerObject{}
		o.merge(piUserAgentHeaders())
		// pi mergeProviderAttributionHeaders (sdk.ts) puts the attribution
		// bundle at the bottom of the precedence stack: emit session +
		// default attribution first so model.headers and options.headers
		// override them.
		applyAttributionDefaults(o.set, model, opts.SessionID)
		// Header-owned provider auth (pi resolves it in the auth layer and
		// delivers it as options.headers, above attribution and below
		// model/consumer headers).
		if model.Provider == "cloudflare-ai-gateway" {
			o.merge(cloudflareAIGatewayAuthHeaders(apiKey))
		}
		// pi createClient header precedence (openai-responses.ts:189-219):
		// model.headers, copilot dynamic headers, session cache headers,
		// then options.headers merged last so they can override defaults.
		o.merge(model.Headers)
		if model.Provider == "github-copilot" {
			o.mergeStrings(buildCopilotDynamicHeaders(normalized.Messages, hasCopilotVisionInput(normalized.Messages)))
		}
		// Session cache headers (pi openai-responses.ts:207-217); the
		// sessionId is zeroed when cacheRetention is "none" (:115). Format
		// selects the header shape.
		if compat := getResponsesCompat(model); opts.SessionID != "" &&
			resolveCacheRetention(opts.CacheRetention, opts.Env) != ai.CacheNone {
			if compat.SessionAffinityFormat == sessionAffinityOpenRouter {
				o.set("x-session-id", opts.SessionID)
			} else {
				if compat.SessionAffinityFormat == sessionAffinityOpenAI {
					o.set("session_id", opts.SessionID)
				}
				o.set("x-client-request-id", opts.SessionID)
			}
		}
		// pi options.headers (consumer) are spread last and win over
		// everything above, including model.headers and the attribution
		// defaults — a deletion marker here suppresses any of them.
		o.merge(opts.Headers)

		// The client is built here, in pi's createClient, before the params and
		// onPayload, and its constructor reads the environment then (see
		// openAIClientHeaders). The SDK sends its own Accept and the api key as
		// its auth header in bundles below pi's object, so a deletion marker
		// there can suppress either, and content-type in a bundle above it.
		headers, err := openAIClientHeaders(o, apiKey)
		if err != nil {
			fail(err)
			return
		}
		params, err := buildResponsesParams(model, normalized, opts)
		if err != nil {
			fail(err)
			return
		}
		var body any = params
		if opts.OnPayload != nil {
			next, err := opts.OnPayload(body, model)
			if err != nil {
				// pi: a throw from onPayload propagates and fails the stream.
				fail(err)
				return
			}
			// pi: any `!== undefined` return replaces the params wholesale.
			if next != nil {
				body = next
			}
		}
		payload, _ := json.Marshal(body)

		// The SDK's buildURL makes the request URL with `new URL(...)` before
		// anything else about the request, and fetch sends what that makes of
		// it (requestURL).
		url, err := requestURL(sdkJoinURL(baseURL, "/responses"))
		if err != nil {
			fail(err)
			return
		}
		build := func() (*http.Request, error) {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			if err := headers.apply(r.Header); err != nil {
				return nil, err
			}
			return r, nil
		}
		resp, err := sendWithRetry(ctx, build, retryFromOptions(opts.StreamOptions, openaiSDKErrorMessage))
		if err != nil {
			fail(openaiFetchError(err))
			return
		}
		nullBody := nullResponseBody(resp)
		defer resp.Body.Close()
		respBody := sdkResponseBody(ctx, resp, opts.HTTPClient)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, err := readSDKErrorBody(ctx, respBody)
			if err != nil {
				fail(err)
				return
			}
			fail(formatResponsesHTTPError(model.Provider, resp.StatusCode, data))
			return
		}
		// pi awaits onResponse once the SDK has a 2xx response — for any other
		// status the SDK throws before withResponse resolves, so the hook never
		// runs — and a throw from it fails the stream.
		if opts.OnResponse != nil {
			if err := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: flattenHeaders(resp.Header)}, model); err != nil {
				fail(err)
				return
			}
		}

		stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: output.Clone()})
		if nullBody {
			fail(errOpenAIStreamNoBody)
			return
		}

		var builders []*blockBuilder
		// outputSlots maps an event's output_index to the in-flight block for
		// that output item. Tracking by output_index (instead of a single
		// "current" pointer) preserves routing when items interleave or arrive
		// out of order: a delta for an earlier item still targets its original
		// contentIndex even after a later item's block was appended (port of
		// upstream 8c9dbffa's ResponsesOutputSlot map, closes #6009).
		type responsesOutputSlot struct {
			block        *blockBuilder
			contentIndex int
		}
		// Keyed by slotKey: event.output_index as pi's Map compares it.
		outputSlots := map[any]*responsesOutputSlot{}
		// reasoningBlocksByID indexes reasoning blocks by their item id so a
		// terminal response.completed can backfill a missing encrypted_content
		// onto the persisted signature (port of upstream 1f0dbc00). Azure OpenAI
		// can omit reasoning.encrypted_content from response.output_item.done and
		// supply it only in response.completed.response.output; backfilling keeps
		// store:false multi-turn replay stateless. See
		// https://github.com/earendil-works/pi/issues/6409. Like pi's Map, it is
		// keyed by the item's id as parsed (jsMapKey), so an absent id is a key
		// of its own, undefined.
		reasoningBlocksByID := map[any]*blockBuilder{}
		// textSigs carries the per-text-block textSignature (blockBuilder, shared
		// with anthropic, has no textSignature field) keyed by builder index.
		textSigs := map[int]string{}
		materialize := func() {
			content := make(ai.ContentList, len(builders))
			for i, b := range builders {
				c := b.toContent()
				if sig, ok := textSigs[i]; ok && sig != "" {
					if tc, isText := c.(ai.TextContent); isText {
						tc.TextSignature = sig
						c = tc
					}
				}
				content[i] = c
			}
			output.Content = content
		}
		// getSlot returns the slot for outputIndex only if it exists AND its
		// block kind matches the requested kind (port of pi's getSlot<TType>).
		getSlot := func(key any, kind string) *responsesOutputSlot {
			slot := outputSlots[key]
			if slot != nil && slot.block.kind == kind {
				return slot
			}
			return nil
		}
		// grammarInput is the raw input accumulated on a custom tool call so far.
		grammarInput := func(b *blockBuilder) string {
			if b.grammar == nil {
				return ""
			}
			s, _ := b.args[b.grammar.property].(string)
			return s
		}
		// appendGrammarInput advances a custom tool call to nextInput and pushes
		// the synthesized JSON delta, if any.
		appendGrammarInput := func(slot *responsesOutputSlot, nextInput string, final bool) error {
			b := slot.block
			if b.grammar == nil {
				return nil
			}
			delta, ok, err := b.grammar.append(nextInput, final)
			if err != nil {
				return err
			}
			b.args = map[string]any{b.grammar.property: nextInput}
			materialize()
			if ok {
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: slot.contentIndex, Delta: delta, Partial: output.Clone()})
			}
			return nil
		}
		// applyMessagePhaseStopReason mirrors pi's openai-responses-shared helper
		// (upstream f9a49869): a message item whose phase is "final_answer"
		// resolves the still-pending stop reason to "stop". Called both when a
		// message slot is first created and again on response.output_item.done,
		// since the terminal item may be the first place the phase is seen.
		applyMessagePhaseStopReason := func(item responsesItem) {
			if item.Type == "message" && item.Phase == "final_answer" {
				output.StopReason = ai.StopStop
			}
		}
		// createSlot appends a new block for item and records the slot with its
		// stable contentIndex, emitting the matching *_start event. Returns nil
		// for item types that have no streaming block.
		createSlot := func(key any, item responsesItem) *responsesOutputSlot {
			var b *blockBuilder
			var startEvent ai.EventType
			switch item.Type {
			case "reasoning":
				b = &blockBuilder{kind: "thinking"}
				startEvent = ai.EventThinkingStart
			case "message":
				applyMessagePhaseStopReason(item)
				b = &blockBuilder{kind: "text"}
				startEvent = ai.EventTextStart
			case "function_call":
				b = &blockBuilder{
					kind: "toolCall", toolID: item.CallID + "|" + item.ID, toolName: item.Name,
					toolNamespace: item.Namespace, args: map[string]any{},
				}
				b.partialJSON.WriteString(item.Arguments)
				startEvent = ai.EventToolCallStart
			case "custom_tool_call":
				// The "input" fallback should never be taken; it only gives a
				// made-up tool we know nothing about somewhere to stash its input.
				property, ok := grammarProps[item.Name]
				if !ok {
					property = "input"
				}
				// The input the item already carries is present on the block the
				// start event announces (pi 8b5899dce, reverting 5c6655e76's
				// replay-as-a-delta). pi writes `item.input || ""`, so the property
				// is there even for an item that carries no input at all. Only the
				// JSON delta buffer stays empty: the first delta synthesized after
				// this opens the object and carries the initial input with it.
				input := ""
				if item.Input != nil {
					input = *item.Input
				}
				b = &blockBuilder{
					kind: "toolCall", toolID: item.CallID + "|" + item.ID, toolName: item.Name,
					toolNamespace: item.Namespace,
					args:          map[string]any{property: input}, grammar: newGrammarInputBuffer(property),
				}
				startEvent = ai.EventToolCallStart
			default:
				return nil
			}
			if b.kind == "toolCall" {
				b.unfinished = true
			}
			builders = append(builders, b)
			slot := &responsesOutputSlot{block: b, contentIndex: len(builders) - 1}
			outputSlots[key] = slot
			materialize()
			stream.Push(ai.AssistantMessageEvent{Type: startEvent, ContentIndex: slot.contentIndex, Partial: output.Clone()})
			return slot
		}
		// getOrCreateSlot returns the existing slot for outputIndex or creates one.
		getOrCreateSlot := func(key any, item responsesItem) *responsesOutputSlot {
			if slot := outputSlots[key]; slot != nil {
				return slot
			}
			return createSlot(key, item)
		}

		// sawTerminalResponseEvent tracks whether a terminal response event
		// (response.completed | response.incomplete | response.failed) arrived
		// before the stream ended (port of upstream cd95c274).
		sawTerminalResponseEvent := false
		// backfillReasoningSignatures re-derives a reasoning block's persisted
		// signature from the terminal response's output when the earlier
		// output_item.done omitted encrypted_content (port of upstream 1f0dbc00).
		// It reads `response.output ?? []` as pi does: a for-of over it, so an
		// output that is neither nullish, an array nor a string throws V8's
		// TypeError, as does a null item (item.type). The rebuilt signature is
		// pi's `JSON.stringify({...storedItem, encrypted_content})`: the stored
		// item's keys in their order, encrypted_content in its place or after
		// them, written as JSON.stringify writes it. The signature persists in
		// the session and replays to the provider.
		backfillReasoningSignatures := func(responseOutput any) error {
			var items []any
			switch v := responseOutput.(type) {
			case nil, jsUndefinedValue, string:
				return nil // `?? []`; a string's characters are no reasoning items
			case []any:
				items = v
			default:
				return errors.New("responseOutput is not iterable")
			}
			for _, item := range items {
				if item == nil {
					return jsReadError(item, "type")
				}
				encrypted := jsGet(item, "encrypted_content")
				if jsGet(item, "type") != "reasoning" || !jsTruthy(encrypted) {
					continue
				}
				var block *blockBuilder
				if key, ok := jsMapKey(jsGet(item, "id")); ok {
					block = reasoningBlocksByID[key]
				}
				if block == nil || block.thinkingSig == "" {
					continue
				}
				// Written by JSON.stringify at output_item.done, so it parses.
				stored, err := ai.DecodeOrderedValue([]byte(block.thinkingSig))
				if err != nil {
					return fmt.Errorf("re-reading a reasoning signature the stream wrote: %w; this is a port bug, report it with the event", err)
				}
				if jsTruthy(jsGet(stored, "encrypted_content")) {
					continue
				}
				storedItem, _ := stored.(ai.OrderedObject)
				rebuilt := slices.Clone(storedItem)
				if i := slices.IndexFunc(rebuilt, func(f ai.OrderedField) bool { return f.Key == "encrypted_content" }); i >= 0 {
					rebuilt[i].Value = encrypted
				} else {
					rebuilt = append(rebuilt, ai.OrderedField{Key: "encrypted_content", Value: encrypted})
				}
				signature, err := jstext.Stringify(rebuilt)
				if err != nil {
					return fmt.Errorf("writing a reasoning signature as JSON.stringify does: %w; this is a port bug, report it with the event", err)
				}
				block.thinkingSig = signature
			}
			return nil
		}
		// finalizeResponse runs pi's finalizeResponse for response.completed
		// and response.incomplete, reading event.response with JS semantics:
		// every member exact-key and of whatever type it holds. Its first read,
		// `response.output`, throws on a null or absent response.
		finalizeResponse := func(event any) error {
			sawTerminalResponseEvent = true
			response := jsGet(event, "response")
			if response == nil || response == jsUndefined {
				return jsReadError(response, "output")
			}
			if err := backfillReasoningSignatures(jsGet(response, "output")); err != nil {
				return err
			}
			// pi rewrites the block in place, so whatever ends the stream —
			// done, or an error such as a content-filtered incomplete — carries
			// the backfilled signature.
			materialize()
			if id := jsGet(response, "id"); jsTruthy(id) {
				output.ResponseID = jsStringField(id)
			}
			if usage := jsGet(response, "usage"); jsTruthy(usage) {
				details := jsGet(usage, "input_tokens_details")
				cached := jsTokenCount(jsGet(details, "cached_tokens"))
				cacheWrite := jsTokenCount(jsGet(details, "cache_write_tokens"))
				output.Usage = ai.Usage{
					// OpenAI includes cached and cache-write tokens in input_tokens,
					// so subtract both (clamped at 0) to get non-cached input.
					Input:      max(0, jsTokenCount(jsGet(usage, "input_tokens"))-cached-cacheWrite),
					Output:     jsTokenCount(jsGet(usage, "output_tokens")),
					CacheRead:  cached,
					CacheWrite: cacheWrite,
					// pi: `reasoning: output_tokens_details?.reasoning_tokens || 0`.
					Reasoning:   jsTokenCount(jsGet(jsGet(usage, "output_tokens_details"), "reasoning_tokens")),
					TotalTokens: jsTokenCount(jsGet(usage, "total_tokens")),
				}
			}
			ai.CalculateCost(model, &output.Usage)
			// Service-tier pricing: `response?.service_tier ?? options.serviceTier`,
			// so the response's tier wins unless it is null or absent. One that is
			// not a string ("" included) matches no tier and prices at ×1.
			serviceTier := opts.ServiceTier
			if tier := jsGet(response, "service_tier"); tier != nil && tier != jsUndefined {
				serviceTier, _ = tier.(string)
			}
			applyResponsesServiceTierPricing(&output.Usage, serviceTier, model)
			// Upstream 32850ef7: the provider's incomplete_details.reason is
			// retained so max-output truncation and content filtering stay
			// distinct — it qualifies rawStopReason as "<status>.<reason>" and
			// decides whether "incomplete" is a length stop or an error. pi
			// assigns rawStopReason before mapping the status, and a template
			// literal that throws leaves it unassigned.
			status := jsGet(response, "status")
			incompleteReason, _ := jsGet(jsGet(response, "incomplete_details"), "reason").(string)
			if incompleteReason != "" {
				statusText, err := jsToString(status)
				if err != nil {
					return err
				}
				output.RawStopReason = statusText + "." + incompleteReason
			} else {
				output.RawStopReason = jsStringField(status)
			}
			reason, errorMessage, statusErr := mapResponsesStatusValue(status, incompleteReason)
			if statusErr != nil {
				return statusErr
			}
			output.StopReason = reason
			output.ErrorMessage = errorMessage
			for _, b := range builders {
				if b.kind == "toolCall" && output.StopReason == ai.StopStop {
					output.StopReason = ai.StopToolUse
				}
			}
			return nil
		}

		var onEvent func(any) error
		if opts.OnProviderStreamEvent != nil {
			onEvent = func(data any) error { return opts.OnProviderStreamEvent(data, model) }
		}
		err = iterateOpenAISSE2(respBody, ctx, onEvent, func(ev responsesEvent) error {
			switch ev.Type {
			case "response.created":
				// pi: `output.responseId = event.response.id`, which throws on a
				// null or absent response and assigns whatever id holds.
				response := jsGet(ev.JS, "response")
				if response == nil || response == jsUndefined {
					return jsReadError(response, "id")
				}
				output.ResponseID = jsStringField(jsGet(response, "id"))
			case "response.output_item.added":
				if ev.Item == nil {
					return nil
				}
				createSlot(ev.slotKey(), *ev.Item)
			case "response.reasoning_summary_text.delta":
				slot := getSlot(ev.slotKey(), "thinking")
				if slot == nil {
					return nil
				}
				slot.block.thinking.WriteString(ev.Delta)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: slot.contentIndex, Delta: ev.Delta, Partial: output.Clone()})
			case "response.reasoning_summary_part.done":
				slot := getSlot(ev.slotKey(), "thinking")
				if slot == nil {
					return nil
				}
				slot.block.thinking.WriteString("\n\n")
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: slot.contentIndex, Delta: "\n\n", Partial: output.Clone()})
			case "response.reasoning_text.delta":
				slot := getSlot(ev.slotKey(), "thinking")
				if slot == nil {
					return nil
				}
				slot.block.thinking.WriteString(ev.Delta)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: slot.contentIndex, Delta: ev.Delta, Partial: output.Clone()})
			case "response.output_text.delta":
				slot := getSlot(ev.slotKey(), "text")
				if slot == nil {
					return nil
				}
				slot.block.text.WriteString(ev.Delta)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: slot.contentIndex, Delta: ev.Delta, Partial: output.Clone()})
			case "response.refusal.delta":
				slot := getSlot(ev.slotKey(), "text")
				if slot == nil {
					return nil
				}
				slot.block.text.WriteString(ev.Delta)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: slot.contentIndex, Delta: ev.Delta, Partial: output.Clone()})
			case "response.function_call_arguments.delta":
				slot := getSlot(ev.slotKey(), "toolCall")
				if slot == nil || slot.block.grammar != nil {
					return nil
				}
				slot.block.partialJSON.WriteString(ev.Delta)
				slot.block.args, slot.block.argsOrder = parseStreamingJSON(slot.block.partialJSON.String())
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: slot.contentIndex, Delta: ev.Delta, Partial: output.Clone()})
			case "response.function_call_arguments.done":
				slot := getSlot(ev.slotKey(), "toolCall")
				if slot == nil || slot.block.grammar != nil {
					return nil
				}
				previous := slot.block.partialJSON.String()
				slot.block.partialJSON.Reset()
				slot.block.partialJSON.WriteString(ev.Arguments)
				slot.block.args, slot.block.argsOrder = parseStreamingJSON(ev.Arguments)
				materialize()
				// Emit the trailing delta so a provider that only sends
				// .done (no incremental deltas) still yields full args.
				if strings.HasPrefix(ev.Arguments, previous) {
					delta := ev.Arguments[len(previous):]
					if delta != "" {
						stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: slot.contentIndex, Delta: delta, Partial: output.Clone()})
					}
				}
			case "response.custom_tool_call_input.delta":
				slot := getSlot(ev.slotKey(), "toolCall")
				if slot == nil || slot.block.grammar == nil {
					return nil
				}
				return appendGrammarInput(slot, grammarInput(slot.block)+ev.Delta, false)
			case "response.custom_tool_call_input.done":
				slot := getSlot(ev.slotKey(), "toolCall")
				if slot == nil || slot.block.grammar == nil {
					return nil
				}
				return appendGrammarInput(slot, ev.Input, true)
			case "response.output_item.done":
				if ev.Item == nil {
					return nil
				}
				applyMessagePhaseStopReason(*ev.Item)
				// getOrCreateSlot mirrors pi's new design: a done without a
				// prior added still materializes the block (and its *_start
				// event) before finalizing (port of 8c9dbffa).
				slot := getOrCreateSlot(ev.slotKey(), *ev.Item)
				switch {
				case ev.Item.Type == "reasoning" && slot != nil && slot.block.kind == "thinking":
					summaryText := joinPartsText(ev.Item.Summary, "\n\n")
					contentText := joinPartsText(ev.Item.Content, "\n\n")
					rebuilt := summaryText
					if rebuilt == "" {
						rebuilt = contentText
					}
					if rebuilt == "" {
						rebuilt = slot.block.thinking.String()
					}
					slot.block.thinking.Reset()
					slot.block.thinking.WriteString(rebuilt)
					// pi: `thinkingSignature = JSON.stringify(item)`, the item as
					// JSON.parse read it, not the provider's bytes.
					item := jsGet(ev.JS, "item")
					signature, err := jstext.Stringify(item)
					if err != nil {
						return fmt.Errorf("writing a reasoning item as JSON.stringify does: %w; this is a port bug, report it with the event", err)
					}
					slot.block.thinkingSig = signature
					// Index by item id so a terminal response.completed can backfill
					// a late-arriving encrypted_content (port of upstream 1f0dbc00).
					if key, ok := jsMapKey(jsGet(item, "id")); ok {
						reasoningBlocksByID[key] = slot.block
					}
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: slot.contentIndex, Content: slot.block.thinking.String(), Partial: output.Clone()})
					delete(outputSlots, ev.slotKey())
				case ev.Item.Type == "message" && slot != nil && slot.block.kind == "text":
					// Rebuild final text from item.content (output_text or refusal).
					var sb strings.Builder
					for _, p := range ev.Item.Content {
						if p.Type == "refusal" {
							sb.WriteString(p.Refusal)
						} else {
							sb.WriteString(p.Text)
						}
					}
					slot.block.text.Reset()
					slot.block.text.WriteString(sb.String())
					textSigs[slot.contentIndex] = encodeTextSignatureV1(ev.Item.ID, ev.Item.Phase)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: slot.contentIndex, Content: slot.block.text.String(), Partial: output.Clone()})
					delete(outputSlots, ev.slotKey())
				case ev.Item.Type == "function_call" && slot != nil && slot.block.kind == "toolCall" && slot.block.grammar == nil:
					// pi 8c9dbffa: parseStreamingJson(item.arguments || partialJson || "{}")
					// — the done event's item.arguments wins over the scratch buffer.
					argsJSON := ev.Item.Arguments
					if argsJSON == "" {
						argsJSON = slot.block.partialJSON.String()
					}
					slot.block.args, slot.block.argsOrder = parseStreamingJSON(orEmptyJSON(argsJSON))
					// Guarded so an absent namespace on the done item cannot clobber
					// the one captured at output_item.added.
					if ev.Item.Namespace != "" {
						slot.block.toolNamespace = ev.Item.Namespace
					}
					slot.block.unfinished = false
					materialize()
					tc := slot.block.toContent().(ai.ToolCall)
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: slot.contentIndex, ToolCall: &tc, Partial: output.Clone()})
					delete(outputSlots, ev.slotKey())
				case ev.Item.Type == "custom_tool_call" && slot != nil && slot.block.grammar != nil:
					// pi: item.input ?? the input accumulated so far.
					finalInput := grammarInput(slot.block)
					if ev.Item.Input != nil {
						finalInput = *ev.Item.Input
					}
					if gerr := appendGrammarInput(slot, finalInput, true); gerr != nil {
						return gerr
					}
					slot.block.grammar = nil
					// Guarded so an absent namespace on the done item cannot clobber
					// the one captured at output_item.added.
					if ev.Item.Namespace != "" {
						slot.block.toolNamespace = ev.Item.Namespace
					}
					slot.block.unfinished = false
					materialize()
					tc := slot.block.toContent().(ai.ToolCall)
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: slot.contentIndex, ToolCall: &tc, Partial: output.Clone()})
					delete(outputSlots, ev.slotKey())
				}
			case "response.completed", "response.incomplete":
				// Upstream cd95c274: response.incomplete finalizes usage/cost/
				// stopReason identically to response.completed.
				if finalizeErr := finalizeResponse(ev.JS); finalizeErr != nil {
					return finalizeErr
				}
			case "error":
				return errorEventMessage(ev.JS)
			case "response.failed":
				// Upstream cd95c274: response.failed is a terminal event too,
				// recorded before its error is thrown.
				sawTerminalResponseEvent = true
				// pi assigns `event.response?.status` unconditionally, so an event
				// without a response clears any earlier raw stop reason.
				response := jsGet(ev.JS, "response")
				output.RawStopReason = jsStringField(jsGet(response, "status"))
				return responsesFailedMessage(response)
			}
			return nil
		})

		if err != nil {
			fail(err)
			return
		}
		// Upstream cd95c274: a stream that ended without any terminal response
		// event must fail with this exact message. pi checks it at the end of
		// processResponsesStream, before its adapter's abort guard, so a request
		// cancelled before any terminal event ends with this message (and the
		// aborted stop reason fail derives from ctx), not "Request was aborted".
		if !sawTerminalResponseEvent {
			fail(fmt.Errorf("OpenAI Responses stream ended before a terminal response event"))
			return
		}
		// Upstream 1b2aa0ca0: the agent runs every tool call in the final
		// message, so refuse to hand over one whose output_item.done never
		// arrived — its arguments may be cut off or mixed up, e.g. when a
		// non-compliant server omits output_index. pi checks it last in
		// processResponsesStream, so it precedes the abort guard too.
		if output.StopReason == ai.StopToolUse {
			for _, b := range builders {
				if b.unfinished {
					fail(fmt.Errorf("OpenAI Responses stream completed with an unfinished tool call: %s (%s)", b.toolName, b.toolID))
					return
				}
			}
		}
		if ctx != nil && ctx.Err() != nil {
			fail(errRequestWasAborted)
			return
		}
		// pi openai-responses.ts (upstream f9a49869): a stream that ended without
		// resolving the pending stop reason must fail with this exact message.
		// Unreachable today — the sawTerminalResponseEvent guard above only passes
		// once finalizeResponse has assigned a concrete reason — but kept as
		// defensive parity with pi in case a future terminal path sets the flag
		// without resolving the reason.
		if output.StopReason == ai.StopPending {
			fail(fmt.Errorf("OpenAI Responses stream ended without a stop reason"))
			return
		}
		// pi openai-responses.ts:140-142: a stream that ended with an error or
		// aborted stop reason (e.g. response.completed status "cancelled")
		// must fail, never emit done. Upstream 32850ef7 prefers the message the
		// status mapping produced (`output.errorMessage || "An unknown error
		// occurred"`), so a content-filtered incomplete says why.
		if output.StopReason == ai.StopAborted || output.StopReason == ai.StopError {
			message := output.ErrorMessage
			if message == "" {
				message = "An unknown error occurred"
			}
			fail(errors.New(message))
			return
		}
		materialize()
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: output.StopReason, Message: output})
		stream.End()
	}()

	return stream
}

func buildResponsesParams(model *ai.Model, req ai.TranscriptContext, opts *OpenAIResponsesOptions) (map[string]any, error) {
	compat := getResponsesCompat(model)
	// body.tools holds the initial tools when additions can be anchored at their
	// system messages (responsesInput emits the later ones there), and the
	// complete current tool set otherwise.
	transcriptTools := ai.ResolveTranscriptTools(req.Messages, compat.SupportsAdditionalTools || compat.SupportsToolSearch)
	input, err := responsesInput(model, req)
	if err != nil {
		return nil, err
	}
	params := map[string]any{
		"model":  model.ID,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	retention := resolveCacheRetention(opts.CacheRetention, opts.Env)
	// Sign in with ChatGPT rejects the cache retention settings, the output
	// token limit and the temperature; prompt_cache_key still goes.
	omitUnsupported := isChatGPTSignIn(model, opts.APIKey)
	// Prompt caching: route same-session requests to a stable cache key so OpenAI
	// can reuse the cached system-prompt + tool prefix (latency/cost win).
	if retention != ai.CacheNone && opts.SessionID != "" {
		params["prompt_cache_key"] = clampPromptCacheKey(opts.SessionID)
	}
	// pi getPromptCacheRetention (openai-responses.ts) sets prompt_cache_retention
	// independent of sessionId. Upstream 17de82d7b added the explicit-mode
	// exclusion: a GPT-5.6+ model expresses long retention through
	// prompt_cache_options instead, and must not send both.
	if !omitUnsupported && retention == ai.CacheLong && compat.SupportsLongCacheRetention && !compat.SupportsExplicitPromptCacheMode {
		params["prompt_cache_retention"] = "24h"
	}
	// pi getPromptCacheOptions (upstream 17de82d7b), for models that accept
	// prompt_cache_options at all. "none" tells a model with implicit caching to
	// stop (upstream 241431c6 — write compaction and branch summaries must not
	// poison the session cache); "long" is how those models spell 24h retention.
	if !omitUnsupported && compat.SupportsExplicitPromptCacheMode {
		switch {
		case retention == ai.CacheNone:
			params["prompt_cache_options"] = map[string]any{"mode": "explicit"}
		case retention == ai.CacheLong && compat.SupportsLongCacheRetention:
			params["prompt_cache_options"] = map[string]any{"ttl": "30m"}
		}
	}
	// pi `if (options?.maxTokens && compat.supportsMaxOutputTokens)` — JS
	// truthiness, so 0 is omitted. pi then floors the value at 16 (Math.max)
	// since the Responses API rejects lower.
	if opts.MaxTokens != nil && *opts.MaxTokens != 0 && compat.SupportsMaxOutputTokens && !omitUnsupported {
		mo := *opts.MaxTokens
		if mo < openaiResponsesMinOutputTokens {
			mo = openaiResponsesMinOutputTokens
		}
		params["max_output_tokens"] = mo
	}
	if opts.Temperature != nil && !omitUnsupported {
		params["temperature"] = *opts.Temperature
	}
	if opts.ServiceTier != "" {
		params["service_tier"] = opts.ServiceTier
	}
	if len(transcriptTools.RequestTools) > 0 {
		tools, err := convertResponsesTools(transcriptTools.RequestTools, compat, false)
		if err != nil {
			return nil, err
		}
		params["tools"] = tools
	}
	if opts.ToolChoice != nil {
		params["tool_choice"] = opts.ToolChoice
	}
	if model.Reasoning {
		if opts.ReasoningEffort != "" || opts.ReasoningSummary != "" {
			effort := "medium"
			if opts.ReasoningEffort != "" {
				effort = effortValue(model, opts.ReasoningEffort)
			}
			summary := opts.ReasoningSummary
			if summary == "" {
				summary = "auto"
			}
			params["reasoning"] = map[string]any{"effort": effort, "summary": summary}
			// Required for store:false: have the API return encrypted reasoning so
			// the reasoning item can be replayed inline on the next turn (otherwise
			// replaying its id 404s — items aren't persisted when store is false).
			params["include"] = []any{"reasoning.encrypted_content"}
		} else if model.Provider != "github-copilot" {
			// pi: else if provider !== "github-copilot" && thinkingLevelMap?.off !== null
			if off, send := offEffortOrDefault(model, "none"); send {
				params["reasoning"] = map[string]any{"effort": off}
			}
		}
		// xAI returns encrypted reasoning only when asked; request it for every
		// reasoning-capable xai model regardless of which branch fired, so
		// store:false multi-turn replay keeps the reasoning items (pi 5220aba6).
		if model.Provider == "xai" {
			params["include"] = []any{"reasoning.encrypted_content"}
		}
	}

	// Last so custom keys override the named request fields (upstream 25a2c8dc).
	// Per-request keys override model defaults (upstream c01f687e5).
	maps.Copy(params, model.SamplingParams)
	maps.Copy(params, opts.SamplingParams)

	return params, nil
}

// normalizeResponsesToolCallID ports pi's normalizeToolCallId closure
// (openai-responses.ts:109-121): non-allowed providers sanitize the WHOLE raw
// id (pipes become underscores, so the later split yields no item id); allowed
// providers normalize the callId|itemId halves, hashing foreign item ids and
// enforcing the fc_ prefix.
func normalizeResponsesToolCallID(model *ai.Model, id string, isForeign bool) string {
	if !openaiToolCallProviders[model.Provider] {
		return normalizeResponsesIDPart(id)
	}
	if !strings.Contains(id, "|") {
		return normalizeResponsesIDPart(id)
	}
	callID, itemID := splitToolCallID(id)
	normalizedCallID := normalizeResponsesIDPart(callID)
	var normalizedItemID string
	if isForeign {
		normalizedItemID = buildForeignResponsesItemID(itemID)
	} else {
		normalizedItemID = normalizeResponsesIDPart(itemID)
	}
	// OpenAI Responses API requires item id to start with "fc"
	if !strings.HasPrefix(normalizedItemID, "fc_") {
		normalizedItemID = normalizeResponsesIDPart("fc_" + normalizedItemID)
	}
	return normalizedCallID + "|" + normalizedItemID
}

// buildResponsesToolCallIDNormalizer pre-computes normalized tool-call ids per
// source assistant message. pi's normalizeToolCallId receives the SOURCE
// message (needed for the foreign/cross-provider distinction), which the Go
// transformMessages callback signature lacks — so results are keyed by raw id
// here. transformMessages only consults the normalizer for !isSameModel
// messages (transform-messages.ts:133), so same-model raw ids (450+ chars,
// raw callId|itemId) replay verbatim; the map mirrors that gating.
func buildResponsesToolCallIDNormalizer(model *ai.Model, messages []ai.Message) func(string) string {
	normalized := map[string]string{}
	for _, m := range messages {
		am, ok := asAssistantMsg(m)
		if !ok {
			continue
		}
		if am.Provider == model.Provider && am.Api == model.Api && am.Model == model.ID {
			continue // same model: ids are never normalized
		}
		isForeign := am.Provider != model.Provider || am.Api != model.Api
		for _, c := range am.Content {
			if tc, ok := c.(ai.ToolCall); ok {
				normalized[tc.ID] = normalizeResponsesToolCallID(model, tc.ID, isForeign)
			}
		}
	}
	return func(id string) string {
		if n, ok := normalized[id]; ok {
			return n
		}
		return id
	}
}

// convertResponsesTools maps unified tools to Responses API tools. `strict` is
// emitted only where the provider supports it (upstream 24bace27 — it used to be
// unconditional); grammar-constrained tools become custom tools;
// toolSearchResult marks definitions returned by a client tool_search_output
// item as `defer_loading`. Port of convertResponsesTools.
func convertResponsesTools(tools []ai.Tool, compat responsesCompat, toolSearchResult bool) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		grammar, err := resolveGrammarSampling(t, compat.SupportsOpenAIGrammarTools)
		if err != nil {
			return nil, err
		}
		if grammar != nil {
			tool := map[string]any{
				"type": "custom", "name": t.Name, "description": t.Description,
				"format": map[string]any{"type": "grammar", "syntax": grammar.format, "definition": grammar.definition},
			}
			if toolSearchResult {
				tool["defer_loading"] = true
			}
			out = append(out, tool)
			continue
		}
		// pi computes `strict = constrainedStrict ?? defaultStrict`; the only
		// ported caller (openai-responses.ts) never sets a defaultStrict, so the
		// resolver's answer is the whole story here (openai-codex-responses.ts,
		// which passes strict:null, is deliberately unported).
		strict, err := resolveJSONSchemaStrictSampling(t, compat.SupportsStrictMode, nil)
		if err != nil {
			return nil, err
		}
		parameters, err := jsonSchemaToolParameters(t, strict)
		if err != nil {
			return nil, err
		}
		var p any = map[string]any{"type": "object", "properties": map[string]any{}}
		if parameters != nil {
			if raw, err := json.Marshal(parameters); err == nil {
				var decoded any
				_ = json.Unmarshal(raw, &decoded)
				p = decoded
			}
		}
		tool := map[string]any{
			"type": "function", "name": t.Name, "description": t.Description, "parameters": p,
		}
		if toolSearchResult {
			tool["defer_loading"] = true
		}
		if compat.SupportsStrictMode {
			tool["strict"] = strict
		}
		out = append(out, tool)
	}
	return out, nil
}

// responsesInput converts unified messages into Responses API input items
// (port of convertResponsesMessages). It errors when an assistant thinking
// block carries an unparseable thinkingSignature (pi's JSON.parse throws and
// fails the stream).
func responsesInput(model *ai.Model, req ai.TranscriptContext) ([]any, error) {
	var items []any

	compat := getResponsesCompat(model)
	grammarProps, err := grammarToolInputProperties(ai.GetDeclaredTools(req.Messages), compat.SupportsOpenAIGrammarTools)
	if err != nil {
		return nil, err
	}
	instructionRole := "system"
	if model.Reasoning && compat.SupportsDeveloperRole {
		instructionRole = "developer"
	}

	// pi's convertResponsesMessages resolves the transcript itself (a no-op on
	// one the stream already resolved). Tool-call id normalization happens
	// inside transformMessages (gated on !isSameModel there), so the toolCallId
	// map also rewrites tool results and synthetic orphan results, exactly like
	// pi.
	normalized := ai.ResolveTranscript(req, compat.SupportsMidConvoSystemMessages)
	transformed := transformMessages(normalized.Messages, model, buildResponsesToolCallIDNormalizer(model, normalized.Messages))
	transcriptTools := ai.ResolveTranscriptTools(normalized.Messages, compat.SupportsAdditionalTools || compat.SupportsToolSearch)
	imageInput := modelSupportsImages(model)

	msgIndex := 0
	for sourceIndex, m := range transformed {
		sm, isSystem := asSystemMsg(m)
		// The leading system message is the prompt; it does not count toward
		// the msg_pi_<index> fallback ids or the tool_search call_id seeds.
		isLeadingSystemMessage := sourceIndex == 0 && isSystem
		if isSystem {
			var text string
			if isLeadingSystemMessage {
				text = ai.GetSystemMessageText(sm)
			} else {
				if transcriptTools.AnchorsAdditions {
					items, err = appendSystemToolAdditions(items, sm.ToolsAdded, fmt.Sprintf("system:%d", msgIndex), compat)
					if err != nil {
						return nil, err
					}
				}
				text = ai.RenderSystemMessageUpdate(sm)
			}
			if text != "" {
				items = append(items, map[string]any{"role": instructionRole, "content": sanitizeSurrogates(text)})
			}
		} else if um, ok := asUserMsg(m); ok {
			var content []any
			for _, c := range um.Content {
				switch v := c.(type) {
				case ai.TextContent:
					content = append(content, map[string]any{"type": "input_text", "text": sanitizeSurrogates(v.Text)})
				case ai.ImageContent:
					content = append(content, map[string]any{"type": "input_image", "detail": "auto", "image_url": fmt.Sprintf("data:%s;base64,%s", v.MimeType, v.Data)})
				}
			}
			if len(content) == 0 {
				continue
			}
			items = append(items, map[string]any{"role": "user", "content": content})
		} else if am, ok := asAssistantMsg(m); ok {
			var output []any
			sameProviderAndAPI := am.Provider == model.Provider && am.Api == model.Api
			isSameModel := sameProviderAndAPI && am.Model == model.ID
			isDifferentModel := sameProviderAndAPI && am.Model != model.ID
			textBlockIndex := 0
			for _, c := range am.Content {
				switch v := c.(type) {
				case ai.ThinkingContent:
					if v.ThinkingSignature != "" {
						var item any
						// pi: JSON.parse throws on an invalid signature and the
						// throw fails the stream with the bare parse error —
						// propagate, don't drop (Go's json error stands in for
						// the V8 SyntaxError message).
						if err := json.Unmarshal([]byte(v.ThinkingSignature), &item); err != nil {
							return nil, err
						}
						output = append(output, item)
					}
				case ai.TextContent:
					id, phase, _ := parseTextSignature(v.TextSignature)
					var fallback string
					if textBlockIndex == 0 {
						fallback = fmt.Sprintf("msg_pi_%d", msgIndex)
					} else {
						fallback = fmt.Sprintf("msg_pi_%d_%d", msgIndex, textBlockIndex)
					}
					textBlockIndex++
					// OpenAI requires id to be max 64 characters (UTF-16 units,
					// matching JS .length).
					msgID := id
					if msgID == "" {
						msgID = fallback
					} else if utf16Length(msgID) > 64 {
						msgID = "msg_" + shortHash(msgID)
					}
					msgItem := map[string]any{
						"type": "message", "role": "assistant", "status": "completed",
						"content": []any{map[string]any{"type": "output_text", "text": sanitizeSurrogates(v.Text), "annotations": []any{}}},
						"id":      msgID,
					}
					if phase != "" {
						msgItem["phase"] = phase
					}
					output = append(output, msgItem)
				case ai.ToolCall:
					// Ids were already normalized inside transformMessages for
					// !isSameModel messages; pi splits the (raw or normalized)
					// id here without further touching it (shared :201-217).
					callID, itemID := splitToolCallID(v.ID)
					property, isGrammar := grammarProps[v.Name]
					// For different-model messages, drop the fc_ item id to avoid
					// pairing validation against reasoning items. Replaying a
					// custom-tool call as a function_call also drops any non-fc_*
					// id (e.g. a ctc_* custom-tool id), which function_call items
					// cannot carry (upstream 24bace27).
					if (isDifferentModel && strings.HasPrefix(itemID, "fc_")) ||
						(!isGrammar && !strings.HasPrefix(itemID, "fc_")) {
						itemID = ""
					}
					var item map[string]any
					if isGrammar {
						input, gerr := grammarToolInput(v.Name, v.Arguments, property)
						if gerr != nil {
							return nil, gerr
						}
						item = map[string]any{
							"type": "custom_tool_call", "call_id": callID, "name": v.Name,
							"input": sanitizeSurrogates(input),
						}
					} else {
						args, _ := jstext.Stringify(orEmptyArguments(v))
						item = map[string]any{
							"type": "function_call", "call_id": callID, "name": v.Name,
							"arguments": args,
						}
					}
					if v.Namespace != "" && isSameModel {
						// A namespace only replays for a call the current model made
						// itself (upstream 02bd2d1c6, narrowed back to the same model
						// by 9e05370b2).
						item["namespace"] = v.Namespace
					}
					if itemID != "" {
						item["id"] = itemID
					}
					output = append(output, item)
				}
			}
			if len(output) == 0 {
				continue
			}
			items = append(items, output...)
		} else if tr, ok := asToolResultMsg(m); ok {
			// pi takes the raw first split segment (shared :229); any
			// normalization already flowed through the toolCallId map.
			callID, _ := splitToolCallID(tr.ToolCallID)
			var texts []string
			hasImages := false
			for _, c := range tr.Content {
				switch tc := c.(type) {
				case ai.TextContent:
					texts = append(texts, tc.Text)
				case ai.ImageContent:
					hasImages = true
				}
			}
			textResult := strings.Join(texts, "\n")
			hasText := len(textResult) > 0

			var outputVal any
			if hasImages && imageInput {
				var parts []any
				if hasText {
					parts = append(parts, map[string]any{"type": "input_text", "text": sanitizeSurrogates(textResult)})
				}
				for _, c := range tr.Content {
					if img, ok := c.(ai.ImageContent); ok {
						parts = append(parts, map[string]any{"type": "input_image", "detail": "auto", "image_url": fmt.Sprintf("data:%s;base64,%s", img.MimeType, img.Data)})
					}
				}
				outputVal = parts
			} else if hasText {
				outputVal = sanitizeSurrogates(textResult)
			} else if hasImages {
				outputVal = sanitizeSurrogates("(see attached image)")
			} else {
				// No text and no image: distinct placeholder so the model doesn't
				// hallucinate an attachment for empty output (pi #6290).
				outputVal = sanitizeSurrogates("(no tool output)")
			}

			outputType := "function_call_output"
			if _, isGrammar := grammarProps[tr.ToolName]; isGrammar {
				outputType = "custom_tool_call_output"
			}
			items = append(items, map[string]any{
				"type": outputType, "call_id": callID, "output": outputVal,
			})
		}
		if !isLeadingSystemMessage {
			msgIndex++
		}
	}
	return items, nil
}

// appendSystemToolAdditions appends the tools a later system message adds to
// items, at that message; callers invoke it only when the request anchors
// additions (port of the convertResponsesMessages closure of the same name). A
// model that accepts additional_tools gets them inline in one developer item;
// otherwise a model with client tool search gets a completed tool_search_call
// and its tool_search_output, whose call_id hashes seed ("system:<msgIndex>")
// with the tool names.
func appendSystemToolAdditions(items []any, tools []ai.Tool, seed string, compat responsesCompat) ([]any, error) {
	if len(tools) == 0 {
		return items, nil
	}
	if compat.SupportsAdditionalTools {
		additional, err := convertResponsesTools(tools, compat, false)
		if err != nil {
			return nil, err
		}
		return append(items, map[string]any{"type": "additional_tools", "role": "developer", "tools": additional}), nil
	}
	if !compat.SupportsToolSearch {
		return items, nil
	}
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	callID := "pi_tool_load_" + shortHash(seed+":"+strings.Join(names, ","))
	loaded, err := convertResponsesTools(tools, compat, true)
	if err != nil {
		return nil, err
	}
	return append(items,
		map[string]any{
			"type": "tool_search_call", "call_id": callID, "execution": "client", "status": "completed",
			"arguments": map[string]any{"query": strings.Join(names, " "), "limit": len(names)},
		},
		map[string]any{
			"type": "tool_search_output", "call_id": callID, "execution": "client", "status": "completed",
			"tools": loaded,
		},
	), nil
}

// serviceTierCostMultiplier ports getServiceTierCostMultiplier
// (openai-responses.ts:367-380): flex halves cost, priority doubles it (×2.5
// for the exact model id "gpt-5.5"), and so does "fast", Fast mode's name for
// priority processing that GPT-6 models report (upstream a6ca86102).
func serviceTierCostMultiplier(model *ai.Model, serviceTier string) float64 {
	switch serviceTier {
	case "flex":
		return 0.5
	case "priority", "fast":
		if model.ID == "gpt-5.5" {
			return 2.5
		}
		return 2
	default:
		return 1
	}
}

// applyResponsesServiceTierPricing ports applyServiceTierPricing
// (openai-responses.ts:382-395).
func applyResponsesServiceTierPricing(usage *ai.Usage, serviceTier string, model *ai.Model) {
	multiplier := serviceTierCostMultiplier(model, serviceTier)
	if multiplier == 1 {
		return
	}
	// Explicit conversions keep the compiler from fusing each product into the
	// total's adds (an FMA rounds once where V8 rounds twice); see
	// ai.CalculateCost.
	usage.Cost.Input = float64(usage.Cost.Input * multiplier)
	usage.Cost.Output = float64(usage.Cost.Output * multiplier)
	usage.Cost.CacheRead = float64(usage.Cost.CacheRead * multiplier)
	usage.Cost.CacheWrite = float64(usage.Cost.CacheWrite * multiplier)
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
}

// clampPromptCacheKey keeps the cache key within OpenAI's accepted length,
// clamping by Unicode code points (port of clampOpenAIPromptCacheKey).
func clampPromptCacheKey(key string) string {
	runes := []rune(key)
	if len(runes) > 64 {
		return string(runes[:64])
	}
	return key
}

// splitToolCallID mirrors JS `const [callId, itemId] = id.split("|")`: the
// item id is the SECOND segment only (later pipes are discarded), and it is
// empty when the id has no pipe.
func splitToolCallID(id string) (callID, itemID string) {
	parts := strings.SplitN(id, "|", 3)
	if len(parts) > 1 {
		return parts[0], parts[1]
	}
	return id, ""
}

// mapResponsesStatus ports pi's mapStopReason: it maps a response status (with
// the provider's incomplete_details.reason, empty when absent) to a stop reason
// and, for a non-retryable incomplete, the message the stream fails with.
// Unknown statuses are an error (pi throws), surfaced here as a returned error
// that fails the stream.
func mapResponsesStatus(status, incompleteReason string) (ai.StopReason, string, error) {
	switch status {
	case "":
		return ai.StopStop, "", nil
	case "completed":
		return ai.StopStop, "", nil
	case "incomplete":
		// Only max-output truncation is a resumable length stop (upstream
		// 32850ef7); every other reason — content filtering, provider-specific
		// limits, none at all — is a non-retryable error.
		if incompleteReason == "max_output_tokens" {
			return ai.StopLength, "", nil
		}
		if incompleteReason != "" {
			return ai.StopError, "Response incomplete: " + incompleteReason, nil
		}
		return ai.StopError, "Response incomplete without a provider reason", nil
	case "failed", "cancelled":
		return ai.StopError, "", nil
	case "in_progress", "queued":
		return ai.StopStop, "", nil
	default:
		return ai.StopStop, "", fmt.Errorf("Unhandled stop reason: %s", status)
	}
}

// joinPartsText joins the text of output_text/refusal parts with sep.
func joinPartsText(parts []responsesContentPart, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	texts := make([]string, len(parts))
	for i, p := range parts {
		if p.Type == "refusal" {
			texts[i] = p.Refusal
		} else {
			texts[i] = p.Text
		}
	}
	return strings.Join(texts, sep)
}

func orEmptyJSON(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// errorEventMessage is the error pi throws for an `error` event:
// `Error Code ${event.code}: ${event.message}`. A template literal writes any
// value — "undefined" for an absent member, "null", a number as JS formats it,
// an array joined with ",", "[object Object]" — and throws V8's TypeError on
// an object it cannot convert to a primitive, which pi's catch block surfaces.
func errorEventMessage(event any) error {
	code, err := jsToString(jsGet(event, "code"))
	if err != nil {
		return err
	}
	message, err := jsToString(jsGet(event, "message"))
	if err != nil {
		return err
	}
	return errors.New("Error Code " + code + ": " + message)
}

// responsesFailedMessage is the error pi throws for a response.failed event:
//
//	error ? `${error.code || "unknown"}: ${error.message || "no message"}`
//	  : details?.reason ? `incomplete: ${details.reason}`
//	  : "Unknown error (no error details in response)"
//
// over event.response's error and incomplete_details, with JS truthiness and
// template-literal writing throughout (jsToString): a response or an error
// that is not an object reads its members as undefined, and a member no
// template literal can write throws V8's TypeError, which pi's catch block
// surfaces.
func responsesFailedMessage(response any) error {
	errorValue, details := jsGet(response, "error"), jsGet(response, "incomplete_details")
	orDefault := func(value any, fallback string) (string, error) {
		if !jsTruthy(value) {
			return fallback, nil
		}
		return jsToString(value)
	}
	if jsTruthy(errorValue) {
		code, err := orDefault(jsGet(errorValue, "code"), "unknown")
		if err != nil {
			return err
		}
		message, err := orDefault(jsGet(errorValue, "message"), "no message")
		if err != nil {
			return err
		}
		return errors.New(code + ": " + message)
	}
	if reason := jsGet(details, "reason"); jsTruthy(reason) {
		text, err := jsToString(reason)
		if err != nil {
			return err
		}
		return errors.New("incomplete: " + text)
	}
	return errors.New("Unknown error (no error details in response)")
}

// mapResponsesStatusValue is pi's mapStopReason over a status of any type:
// `!status` is a stop, a string maps as mapResponsesStatus does, and any
// other value matches no case and throws `Unhandled stop reason: ${status}`.
func mapResponsesStatusValue(status any, incompleteReason string) (ai.StopReason, string, error) {
	if !jsTruthy(status) {
		return ai.StopStop, "", nil
	}
	if text, isString := status.(string); isString {
		return mapResponsesStatus(text, incompleteReason)
	}
	text, err := jsToString(status)
	if err != nil {
		return ai.StopStop, "", err
	}
	return ai.StopStop, "", errors.New("Unhandled stop reason: " + text)
}

// ---- SSE event types ----

type responsesContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesEvent struct {
	// Type is the event's `type` when it is a string, read by exact key as
	// JS reads event.type; "" matches no handler.
	Type      string `json:"-"`
	Delta     string `json:"delta"`
	Arguments string `json:"arguments"`
	// Input carries response.custom_tool_call_input.done's final raw input.
	Input string                `json:"input"`
	Part  *responsesContentPart `json:"part"`
	Item  *responsesItem        `json:"item"`
	// JS is the event as JSON.parse made it (ai.DecodeOrderedValue's
	// shapes). The handlers for the events that end the stream —
	// response.created's id, response.completed/incomplete/failed and error —
	// read it with jsvalue.go's JS semantics, as pi does: exact keys, and a
	// member of any type read as whatever it holds, so no member can make the
	// port drop the event. The typed fields above stay encoding/json's
	// (case-insensitive keys, and a mistyped member drops the event); only
	// the other events use them.
	JS any `json:"-"`
}

// slotKey is event.output_index as pi's outputSlots Map reads it, by
// SameValueZero: an absent index (undefined), a null, a 0 and a "0" are four
// keys, as they are four slots in pi. It is read from the event as JSON.parse
// made it — a typed int would read absent and null as 0, and drop the whole
// event over a string. An index no JSON primitive can be (an object or an
// array) is a key of its own that no later event matches, as a fresh object's
// identity would be.
func (ev responsesEvent) slotKey() any {
	if key, ok := jsMapKey(jsGet(ev.JS, "output_index")); ok {
		return key
	}
	return new(struct{ unmatched bool })
}

type responsesItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Input is a pointer so an absent field is distinguishable from an
	// empty string, which pi's `item.input ?? …` depends on.
	Input *string `json:"input"`
	// Namespace rides on function_call / custom_tool_call items for
	// dynamically loaded or namespaced tools (upstream 02bd2d1c6).
	Namespace        string                 `json:"namespace"`
	Phase            string                 `json:"phase"`
	EncryptedContent string                 `json:"encrypted_content"`
	Summary          []responsesContentPart `json:"summary"`
	Content          []responsesContentPart `json:"content"`
}

// jsReadError is V8's TypeError for reading key off of, a value that is null
// or undefined, which pi's catch block surfaces as the message.
func jsReadError(of any, key string) error {
	name := "undefined"
	if of == nil {
		name = "null"
	}
	return fmt.Errorf("Cannot read properties of %s (reading '%s')", name, key)
}

// jsStringField is a value pi assigns to one of the message's string fields
// (responseId, rawStopReason) as the port stores it: a string as is, null and
// undefined as "", and anything else — which pi stores as the value itself —
// as String() writes it ("" where String() would throw).
func jsStringField(value any) string {
	if value == nil || value == jsUndefined {
		return ""
	}
	text, _ := jsToString(value)
	return text
}

// jsTokenCount is a usage value as ai.Usage holds it: the number pi's
// arithmetic reads it as (Number(): a numeric string is its number, null and
// false 0), truncated to an integer. A value with no number (NaN, undefined,
// an object) counts 0. pi keeps a member it does not compute with as the value
// itself, a string included, and does not truncate a fraction.
func jsTokenCount(value any) int {
	f, err := jsToNumber(value)
	switch {
	case err != nil, math.IsNaN(f):
		return 0
	case f >= math.MaxInt:
		return math.MaxInt
	case f <= math.MinInt:
		return math.MinInt
	}
	return int(f)
}

// iterateOpenAISSE2 reads a /responses stream the way pi iterates the openai
// SDK's Stream (iterateOpenAIStream). Each item goes to onEvent (when set)
// first, as processResponsesStream's loop opens with onProviderStreamEvent,
// so an event the loop ignores or fails on is observed too; then it is handed
// to handle.
func iterateOpenAISSE2(body io.Reader, ctx context.Context, onEvent func(any) error, handle func(responsesEvent) error) error {
	return iterateOpenAIStream(body, ctx, func(item openaiStreamItem) error {
		if onEvent != nil {
			if err := onEvent(item.value); err != nil {
				return err
			}
		}
		// The SDK yields `data: null` as null, and pi's processResponsesStream
		// reads event.type off every event it iterates, so that read throws and
		// the stream fails with V8's TypeError text (openai-responses-shared.ts).
		// A scalar or an array reads as undefined and matches no branch.
		if item.value == nil {
			return jsReadError(nil, "type")
		}
		if _, isObject := item.value.(ai.OrderedObject); !isObject {
			return nil
		}
		ev := responsesEvent{JS: item.value}
		ev.Type, _ = jsGet(item.value, "type").(string)
		switch ev.Type {
		case "response.created", "response.completed", "response.incomplete", "response.failed", "error":
			return handle(ev) // these read ev.JS alone
		}
		if json.Unmarshal(item.text, &ev) != nil {
			return nil
		}
		return handle(ev)
	})
}

// RegisterOpenAIResponses registers the openai-responses api provider.
func RegisterOpenAIResponses() {
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIOpenAIResponses,
		Stream: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			o := &OpenAIResponsesOptions{}
			if opts != nil {
				o.StreamOptions = *opts
			}
			return StreamOpenAIResponses(ctx, model, req, o)
		},
		StreamSimple: StreamSimpleOpenAIResponses,
	})
}
