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
	"regexp"
	"strings"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// OpenAIOptions are provider-native options for the OpenAI completions stream.
type OpenAIOptions struct {
	ai.StreamOptions
	ReasoningEffort string
	// ToolChoice mirrors pi's OpenAICompletionsOptions.toolChoice: a string
	// ("auto"|"none"|"required") or an object {type:"function",function:{name}}.
	ToolChoice any
	// ThinkingBudgets are token budgets per thinking level. Used when
	// compat.ThinkingTokenBudgetField or compat.SupportsThinkingTokenBudget is
	// set, or by a {"$var": "thinking.budget"} chat-template value
	// (pi b23741269).
	ThinkingBudgets *ai.ThinkingBudgets
}

// StreamSimpleOpenAICompletions maps unified reasoning to OpenAI options.
func StreamSimpleOpenAICompletions(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	o := &OpenAIOptions{}
	if opts != nil {
		o.StreamOptions = opts.StreamOptions
		o.ThinkingBudgets = opts.ThinkingBudgets
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
	// The request's samplingParams pass through as they are; buildOpenAIParams
	// merges them over the model's (upstream c01f687e5).
	mt := ai.ClampMaxTokensToContext(model, req, ai.SimpleMaxTokensDefault(model, opts))
	o.MaxTokens = &mt
	return StreamOpenAICompletions(ctx, model, req, o)
}

// StreamOpenAICompletions streams from an OpenAI-compatible /chat/completions API.
// The transcript is resolved for the model first: a model with
// compat.supportsMidConvoSystemMessages keeps later system messages in place,
// any other has them folded into the leading one. The request, the grammar
// tools and the Copilot headers all read the resolved transcript.
func StreamOpenAICompletions(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *OpenAIOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &OpenAIOptions{}
	}
	normalized := ai.ResolveTranscript(req, getOpenAICompat(model).SupportsMidConvoSystemMessages)

	go func() {
		output := &ai.AssistantMessage{
			Content: ai.ContentList{}, Api: model.Api, Provider: model.Provider, Model: model.ID,
			StopReason: ai.StopPending, Timestamp: nowMillis(),
		}
		// The blocks accumulating out of the SSE deltas, and the two closures that
		// publish them. They live above fail because fail reports output, the same
		// reason pi hoists streamedReasoningDetails / applyStreamedReasoningDetails
		// above its try (upstream 7aab6c26e). The rest of the streaming state is
		// declared with the loop that fills it.
		var thinkBuilder *blockBuilder
		// thinkingDetails is the reasoning_details sequence bound for the thinking
		// block's signature. One stream opens at most one thinking block, so this
		// is the whole of it. It starts empty and is never re-read out of the
		// signature slot: pi dropped its seed-from-the-existing-signature step in
		// 7aab6c26e, so a signature already in the slot (the reasoning field name)
		// is replaced rather than extended.
		var thinkingDetails []json.RawMessage
		var order []*blockBuilder
		materialize := func() {
			content := make(ai.ContentList, len(order))
			for i, b := range order {
				content[i] = b.toContent()
			}
			output.Content = content
		}
		// applyStreamedReasoningDetails serializes the sequence into the thinking
		// block's signature. reasoning_details are replay metadata, not a visible
		// stream delta, so pi does this exactly twice — once where the block is
		// finalized and once on the failure path — instead of on every arrival
		// (upstream 7aab6c26e).
		applyStreamedReasoningDetails := func() {
			if thinkBuilder == nil || len(thinkingDetails) == 0 {
				return
			}
			thinkBuilder.thinkingSig = marshalOpenAIReasoningDetails(thinkingDetails)
			// pi assigns straight onto the block inside its live output object;
			// Go's output.Content is a snapshot of the builders, so republish.
			materialize()
		}
		fail := func(err error) {
			// pi's catch block signs any thinking block still in output.content
			// before reporting it, which is what keeps an aborted stream's replay
			// data intact now that nothing writes it per delta (upstream 7aab6c26e).
			applyStreamedReasoningDetails()
			if ctx != nil && ctx.Err() != nil {
				output.StopReason = ai.StopAborted
			} else {
				output.StopReason = ai.StopError
			}
			output.ErrorMessage = err.Error()
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: output.StopReason, Error: output})
			stream.End()
		}

		apiKey, keyErr := clientAPIKey(model.Provider, opts.APIKey, opts.Headers)
		if keyErr != nil {
			fail(keyErr)
			return
		}

		// Cloudflare providers carry {VAR} placeholders in baseUrl. pi resolves
		// them in its cloudflareStreams wrapper (providers/cloudflare-stream.ts),
		// not in createClient, and it never fails: an absent env var is left in
		// the URL as the literal {CLOUDFLARE_ACCOUNT_ID}. We fail fast instead —
		// a deliberate divergence (docs/UPSTREAM.md, 2026-06-24 ruling items 2-3).
		// With the vars set, the emitted request matches pi byte-for-byte.
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
		grammarProps, err := grammarToolInputProperties(ai.GetDeclaredTools(normalized.Messages), getOpenAICompat(model).SupportsOpenAIGrammarTools)
		if err != nil {
			fail(err)
			return
		}
		// pi createClient (openai-completions.ts, upstream 87af49dec)
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
		// pi createClient header precedence (openai-completions.ts:458-477):
		// model.headers first, then copilot dynamic headers, then session
		// affinity (overrides model headers), with options.headers merged last.
		o.merge(model.Headers)
		if model.Provider == "github-copilot" {
			o.mergeStrings(buildCopilotDynamicHeaders(normalized.Messages, hasCopilotVisionInput(normalized.Messages)))
		}
		// Session-affinity headers for cache-routing providers (e.g. Fireworks).
		// Format selects the header shape (pi openai-completions.ts:519-530).
		if compat := getOpenAICompat(model); opts.SessionID != "" &&
			resolveCacheRetention(opts.CacheRetention, opts.Env) != ai.CacheNone &&
			compat.SendSessionAffinityHeaders {
			if compat.SessionAffinityFormat == sessionAffinityOpenRouter {
				o.set("x-session-id", opts.SessionID)
			} else {
				if compat.SessionAffinityFormat == sessionAffinityOpenAI {
					o.set("session_id", opts.SessionID)
				}
				o.set("x-client-request-id", opts.SessionID)
				o.set("x-session-affinity", opts.SessionID)
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
		params, err := buildOpenAIParams(model, normalized, opts)
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
		url, err := requestURL(sdkJoinURL(baseURL, "/chat/completions"))
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
			fail(formatCompletionsHTTPError(resp.StatusCode, data))
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

		var textBuilder *blockBuilder
		// pi ensureToolCallBlock keeps BOTH maps (openai-completions.ts:229-265):
		// lookup by stream index when the delta carries one, falling back to id,
		// and registers blocks under both keys. The maps are keyed by the JS
		// values themselves: an index is any number (SameValueZero, which a
		// float64 key matches for every number JSON can write), and an id any
		// truthy value, where 5 and "5" are two keys and an object or array
		// matches nothing (jsMapKey).
		toolBuildersByIndex := map[float64]*blockBuilder{}
		toolBuildersByID := map[any]*blockBuilder{}
		setToolBuilderByID := func(id any, b *blockBuilder) {
			if key, ok := jsMapKey(id); ok {
				toolBuildersByID[key] = b
			}
		}
		builderHasIndex := map[*blockBuilder]bool{}
		indexOf := func(b *blockBuilder) int {
			for i, x := range order {
				if x == b {
					return i
				}
			}
			return -1
		}
		// startGrammarBuffer switches a block to the custom-tool (grammar) shape.
		// The "input" fallback should never be taken; it only gives a made-up tool
		// we know nothing about somewhere to stash its input.
		//
		// The arguments start at {property: ""}, not empty (pi 8b5899dce, which
		// reversed 5c6655e76): consumers read a partial tool call's arguments by
		// property, so the property is present — holding the empty string — from
		// the toolcall_start event onwards rather than materializing with the
		// first appendGrammarInput.
		startGrammarBuffer := func(b *blockBuilder) {
			property, ok := grammarProps[b.toolName]
			if !ok {
				property = "input"
			}
			b.grammar = newGrammarInputBuffer(property)
			b.args = map[string]any{property: ""}
			b.partialJSON.Reset()
		}
		// grammarInput is the raw input accumulated on a custom tool call so far.
		grammarInput := func(b *blockBuilder) string {
			if b.grammar == nil {
				return ""
			}
			s, _ := b.args[b.grammar.property].(string)
			return s
		}
		// appendGrammarInput advances a custom tool call to nextInput and returns
		// the synthesized JSON delta (empty when there is nothing to emit).
		appendGrammarInput := func(b *blockBuilder, nextInput string, final bool) (string, error) {
			if b.grammar == nil {
				return "", nil
			}
			delta, _, err := b.grammar.append(nextInput, final)
			if err != nil {
				return "", err
			}
			b.args = map[string]any{b.grammar.property: nextInput}
			return delta, nil
		}
		// toolCallName is pi's `toolCall.function?.name ?? toolCall.custom?.name`,
		// undefined when neither is set.
		toolCallName := func(toolCall any) any {
			if name := jsGet(jsGet(toolCall, "function"), "name"); name != nil && name != jsUndefined {
				return name
			}
			return jsGet(jsGet(toolCall, "custom"), "name")
		}
		// isGrammarCall is pi's `toolCall.custom && !toolCall.function` (pi
		// 34239180): a delta carrying both is an ordinary function call — some
		// providers attach an empty `custom: {}` to one, and treating that as a
		// grammar call discards the streamed arguments.
		isGrammarCall := func(toolCall any) bool {
			return jsTruthy(jsGet(toolCall, "custom")) && !jsTruthy(jsGet(toolCall, "function"))
		}
		// ensureToolCallBlock is pi's, over a tool_calls entry of any type: it
		// reads each member with JS semantics, so an entry that is not an
		// object opens a block with no id, name or index (a string's
		// characters each open one), and a null entry throws reading its index.
		ensureToolCallBlock := func(toolCall any) (*blockBuilder, error) {
			if toolCall == nil {
				return nil, jsReadError(nil, "index")
			}
			streamIndex, hasIndex := jsGet(toolCall, "index").(float64)
			id := jsGet(toolCall, "id")
			name := toolCallName(toolCall)
			if name == nil || name == jsUndefined {
				name = ""
			}
			var b *blockBuilder
			if hasIndex {
				b = toolBuildersByIndex[streamIndex]
			}
			if b == nil && jsTruthy(id) {
				if key, ok := jsMapKey(id); ok {
					b = toolBuildersByID[key]
				}
			}
			if b == nil {
				// pi: `id: toolCall.id || ""`.
				b = &blockBuilder{kind: "toolCall", toolName: jsStringField(name), args: map[string]any{}}
				if jsTruthy(id) {
					b.toolID = jsStringField(id)
				}
				if isGrammarCall(toolCall) {
					startGrammarBuffer(b)
				}
				if hasIndex {
					toolBuildersByIndex[streamIndex] = b
					builderHasIndex[b] = true
				}
				if jsTruthy(id) {
					setToolBuilderByID(id, b)
				}
				order = append(order, b)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallStart, ContentIndex: indexOf(b), Partial: output.Clone()})
			}
			if hasIndex && !builderHasIndex[b] {
				builderHasIndex[b] = true
				toolBuildersByIndex[streamIndex] = b
			}
			if jsTruthy(id) {
				setToolBuilderByID(id, b)
			}
			if b.toolName == "" && jsTruthy(name) {
				b.toolName = jsStringField(name)
			}
			if isGrammarCall(toolCall) && b.grammar == nil {
				startGrammarBuffer(b)
			}
			return b, nil
		}

		hasFinishReason := false
		// responseID is pi's output.responseId, the value `||=` tests: the
		// message stores it as a string (jsStringField), whose emptiness is not
		// the value's falsiness (0 is "0").
		var responseID any = jsUndefined
		var onEvent func(any) error
		if opts.OnProviderStreamEvent != nil {
			onEvent = func(data any) error { return opts.OnProviderStreamEvent(data, model) }
		}
		// Each chunk is read as pi's loop reads it: member by member off the
		// parsed value, by exact key, each of whatever type it holds, with pi's
		// own guards (typeof, Array.isArray, truthiness) and JS's coercions where
		// pi concatenates or computes.
		err = iterateOpenAISSE(respBody, ctx, onEvent, func(chunk any) error {
			// OpenAI documents ChatCompletionChunk.id as the unique chat completion
			// identifier shared by every chunk in a streamed completion.
			// pi: `output.responseId ||= chunk.id`.
			if !jsTruthy(responseID) {
				responseID = jsGet(chunk, "id")
				output.ResponseID = jsStringField(responseID)
			}
			if m, isString := jsGet(chunk, "model").(string); isString && m != "" && m != model.ID && output.ResponseModel == "" {
				output.ResponseModel = m
			}
			usage := jsGet(chunk, "usage")
			if jsTruthy(usage) {
				parsed, err := parseChunkUsage(usage, model)
				if err != nil {
					return err
				}
				output.Usage = parsed
			}
			// pi: `Array.isArray(chunk.choices) ? chunk.choices[0] : undefined`.
			var choice any = jsUndefined
			if choices, isArray := jsGet(chunk, "choices").([]any); isArray && len(choices) > 0 {
				choice = choices[0]
			}
			if !jsTruthy(choice) {
				return nil
			}

			// Fallback: some providers (e.g. Moonshot) return usage in choice.usage
			// instead of the top-level chunk.usage.
			if choiceUsage := jsGet(choice, "usage"); !jsTruthy(usage) && jsTruthy(choiceUsage) {
				parsed, err := parseChunkUsage(choiceUsage, model)
				if err != nil {
					return err
				}
				output.Usage = parsed
			}

			if finishReason := jsGet(choice, "finish_reason"); jsTruthy(finishReason) {
				output.RawStopReason = jsStringField(finishReason)
				stopReason, errMsg, err := mapOpenAIFinishReasonValue(finishReason)
				if err != nil {
					return err
				}
				output.StopReason = stopReason
				if errMsg != "" {
					output.ErrorMessage = errMsg
				}
				hasFinishReason = true
			}

			d := jsGet(choice, "delta")
			if !jsTruthy(d) {
				return nil
			}
			// pi processes delta fields in order: content first, then reasoning,
			// then tool_calls, then reasoning_details (openai-completions.ts:299-385).
			// pi: `content !== null && content !== undefined && content.length > 0`,
			// then `block.text += content`: an array's string is its elements
			// joined, and an object is read by its own "length".
			if content := jsGet(d, "content"); content != nil && content != jsUndefined {
				positive, err := jsLengthPositive(content)
				if err != nil {
					return err
				}
				if positive {
					if textBuilder == nil {
						textBuilder = &blockBuilder{kind: "text"}
						order = append(order, textBuilder)
						materialize()
						stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextStart, ContentIndex: indexOf(textBuilder), Partial: output.Clone()})
					}
					text, err := jsToString(content)
					if err != nil {
						return err
					}
					textBuilder.text.WriteString(text)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: indexOf(textBuilder), Delta: text, Partial: output.Clone()})
				}
			}

			// Reasoning may arrive in reasoning_content (llama.cpp), reasoning, or
			// reasoning_text. Use the first non-empty string field to avoid
			// duplication (e.g. chutes.ai returns both with the same content),
			// and record the field name as the thinking signature.
			var reasoningDelta, reasoningSig string
			for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
				if value, isString := jsGet(d, field).(string); isString && value != "" {
					reasoningSig, reasoningDelta = field, value
					break
				}
			}
			if reasoningDelta != "" {
				if model.Provider == "opencode-go" && reasoningSig == "reasoning" {
					reasoningSig = "reasoning_content"
				}
				if thinkBuilder == nil {
					thinkBuilder = &blockBuilder{kind: "thinking", thinkingSig: reasoningSig}
					order = append(order, thinkBuilder)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingStart, ContentIndex: indexOf(thinkBuilder), Partial: output.Clone()})
				}
				thinkBuilder.thinking.WriteString(reasoningDelta)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: indexOf(thinkBuilder), Delta: reasoningDelta, Partial: output.Clone()})
			}

			// pi: `for (const toolCall of choice.delta.tool_calls)` when it is
			// truthy. A string iterates its characters; anything else that is
			// not an array throws.
			if toolCalls := jsGet(d, "tool_calls"); jsTruthy(toolCalls) {
				var entries []any
				switch v := toolCalls.(type) {
				case []any:
					entries = v
				case string:
					for _, r := range v {
						entries = append(entries, string(r))
					}
				default:
					return errToolCallsNotIterable
				}
				for _, toolCall := range entries {
					b, err := ensureToolCallBlock(toolCall)
					if err != nil {
						return err
					}
					// id and name are first-wins, never overwritten (pi :350-356).
					if id := jsGet(toolCall, "id"); b.toolID == "" && jsTruthy(id) {
						b.toolID = jsStringField(id)
						setToolBuilderByID(id, b)
					}
					if name := toolCallName(toolCall); b.toolName == "" && jsTruthy(name) {
						b.toolName = jsStringField(name)
					}

					// pi pushes a toolcall_delta for EVERY delta entry, with an empty
					// delta string when no arguments arrived (pi :358-369).
					delta := ""
					if arguments := jsGet(jsGet(toolCall, "function"), "arguments"); jsTruthy(arguments) {
						text, err := jsToString(arguments)
						if err != nil {
							return err
						}
						delta = text
						b.partialJSON.WriteString(delta)
						b.args, b.argsOrder = parseStreamingJSON(b.partialJSON.String())
					} else if input := jsGet(jsGet(toolCall, "custom"), "input"); jsTruthy(input) {
						text, err := jsToString(input)
						if err != nil {
							return err
						}
						jsonDelta, gerr := appendGrammarInput(b, grammarInput(b)+text, false)
						if gerr != nil {
							return gerr
						}
						delta = jsonDelta
					}
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: indexOf(b), Delta: delta, Partial: output.Clone()})
				}
			}

			// reasoning_details: OpenRouter's structured reasoning replay channel
			// (pi :620-632, upstream b7bb00b93). Every valid detail is appended to
			// thinkingDetails, which becomes the THINKING block's signature —
			// OpenRouter requires the complete sequence back unmodified and in
			// order, so the slot holds the sequence rather than one detail per tool
			// call. OpenRouter streams them as DELTAS, so appending is a merge —
			// see appendOpenAIReasoningDetail. Nothing is written into the
			// signature here: applyStreamedReasoningDetails does that once, at the
			// end of the block (upstream 7aab6c26e). pi guards the field with
			// `Array.isArray`, so anything else in it is ignored.
			details, _ := jsGet(d, "reasoning_details").([]any)
			for _, detail := range details {
				if !isOpenAIReasoningDetailValue(detail) {
					continue
				}
				// pi ensureThinkingBlock(""): the details alone are enough to open
				// a thinking block, whose visible thinking stays empty.
				if thinkBuilder == nil {
					thinkBuilder = &blockBuilder{kind: "thinking"}
					order = append(order, thinkBuilder)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingStart, ContentIndex: indexOf(thinkBuilder), Partial: output.Clone()})
				}
				text, err := jstext.Stringify(detail)
				if err != nil {
					return fmt.Errorf("writing a reasoning detail as JSON.stringify does: %w; this is a port bug, report it with the event", err)
				}
				// Deltas, not finished entries: consecutive text/summary details
				// fold into the entry they extend (upstream c5ad7c1b0).
				thinkingDetails = appendOpenAIReasoningDetail(thinkingDetails, json.RawMessage(text))
			}
			return nil
		})

		if err != nil {
			// A mid-stream read/handler failure throws past pi's finalization
			// loop straight into the catch block; do the same here. The catch
			// block also appends an error chunk's error.metadata.raw.
			fail(withOpenAIErrorMetadataRaw(err))
			return
		}

		// pi finalizes every block ONCE, in content order, after the entire SSE
		// loop (openai-completions.ts:389-391) — even when the stream ended
		// without a finish_reason — so consumers always see *_end events (with
		// final usage in the Partial snapshots) before any error below.
		materialize()
		for _, b := range order {
			switch b.kind {
			case "text":
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: indexOf(b), Content: b.text.String(), Partial: output.Clone()})
			case "thinking":
				// pi signs the block immediately before pushing thinking_end, so
				// the sequence first becomes visible in THIS event's partial and
				// not in any earlier one (upstream 7aab6c26e).
				applyStreamedReasoningDetails()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: indexOf(b), Content: b.thinking.String(), Partial: output.Clone()})
			case "toolCall":
				if b.grammar != nil {
					delta, gerr := appendGrammarInput(b, grammarInput(b), true)
					if gerr != nil {
						fail(gerr)
						return
					}
					if delta != "" {
						materialize()
						stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: indexOf(b), Delta: delta, Partial: output.Clone()})
					}
				} else {
					b.args, b.argsOrder = parseStreamingJSON(b.partialJSON.String())
				}
				materialize()
				tc := b.toContent().(ai.ToolCall)
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: indexOf(b), ToolCall: &tc, Partial: output.Clone()})
			}
		}

		if ctx != nil && ctx.Err() != nil {
			fail(errRequestWasAborted)
			return
		}
		if output.StopReason == ai.StopAborted {
			fail(errRequestWasAborted)
			return
		}
		// Some OpenAI-compatible providers never emit finish_reason. When compat
		// says so, infer the stop reason from the content instead of failing
		// (upstream 2c3041242). pi places this after the aborted guard and before
		// the error guard; in Go only the ctx.Err() check above can actually
		// preempt it, since a StopAborted/StopError stop reason implies
		// hasFinishReason (only mapOpenAIFinishReason sets them) and is therefore
		// mutually exclusive with the !hasFinishReason gate. The placement is
		// shape parity, not a live ordering.
		supportsFinishReason := getOpenAICompat(model).SupportsFinishReason
		if !hasFinishReason && !supportsFinishReason {
			output.StopReason = ai.StopStop
			for _, c := range output.Content {
				if _, ok := c.(ai.ToolCall); ok {
					output.StopReason = ai.StopToolUse
					break
				}
			}
		}
		if output.StopReason == ai.StopError {
			msg := output.ErrorMessage
			if msg == "" {
				msg = "Provider returned an error stop reason"
			}
			fail(fmt.Errorf("%s", msg))
			return
		}
		// pi throws when no finish_reason arrived (:402-404) — including for
		// zero-choice streams that only carried [DONE] — unless compat waived
		// finish_reason above; and also when a finish_reason arrived but left the
		// stop reason unresolved.
		if (supportsFinishReason && !hasFinishReason) || output.StopReason == ai.StopPending {
			fail(fmt.Errorf("Stream ended without finish_reason"))
			return
		}
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: output.StopReason, Message: output})
		stream.End()
	}()

	return stream
}

func buildOpenAIParams(model *ai.Model, req ai.TranscriptContext, opts *OpenAIOptions) (map[string]any, error) {
	compat := getOpenAICompat(model)
	grammarProps, err := grammarToolInputProperties(ai.GetDeclaredTools(req.Messages), compat.SupportsOpenAIGrammarTools)
	if err != nil {
		return nil, err
	}
	// Tool additions anchor in the transcript only where the model takes both
	// later system messages and the tools they add; otherwise the request
	// declares the complete current tool set.
	supportsToolAdditions := compat.SupportsMidConvoSystemMessages && compat.SupportsMidConvoToolAdditions
	transcriptTools := ai.ResolveTranscriptTools(req.Messages, supportsToolAdditions)

	// pi's convertMessages resolves the transcript, and its tools, again itself
	// (a no-op on a transcript the stream already resolved), so a builder handed
	// an unresolved transcript still folds it for a model without
	// mid-conversation system messages.
	var messages []map[string]any
	resolved := ai.ResolveTranscript(req, compat.SupportsMidConvoSystemMessages)
	transformed := transformMessages(resolved.Messages, model, func(id string) string {
		return normalizeOpenAIToolCallID(model, id)
	})
	anchorsAdditions := ai.ResolveTranscriptTools(resolved.Messages, supportsToolAdditions).AnchorsAdditions
	instructionRole := "system"
	if model.Reasoning && compat.SupportsDeveloperRole {
		instructionRole = "developer"
	}
	modelHasImageInput := false
	for _, in := range model.Input {
		if in == "image" {
			modelHasImageInput = true
			break
		}
	}
	lastRole := ""
	for i := 0; i < len(transformed); i++ {
		m := transformed[i]
		// Some providers don't allow user messages directly after tool results;
		// bridge with a synthetic assistant message.
		if compat.RequiresAssistantAfterToolResult && lastRole == "toolResult" {
			if _, ok := asUserMsg(m); ok {
				messages = append(messages, map[string]any{
					"role": "assistant", "content": "I have processed the tool results.",
				})
			}
		}

		if sm, ok := asSystemMsg(m); ok {
			// i counts the transformed transcript, as pi's loop does: the message
			// at index 0 is the prompt, any later one an update.
			if i > 0 && anchorsAdditions && len(sm.ToolsAdded) > 0 {
				tools, err := convertOpenAITools(sm.ToolsAdded, compat)
				if err != nil {
					return nil, err
				}
				// Kimi accepts a system message with tools but omits the standard
				// content field; it declares the additions ahead of the update.
				messages = append(messages, map[string]any{"role": "system", "tools": tools})
			}
			var text string
			if i == 0 {
				text = ai.GetSystemMessageText(sm)
			} else {
				text = ai.RenderSystemMessageUpdate(sm)
			}
			if text != "" {
				messages = append(messages, map[string]any{"role": instructionRole, "content": sanitizeSurrogates(text)})
			}
			// Empty text sends nothing. Either way the message sets lastRole, as
			// pi's system branch falls through to `lastRole = msg.role`: a system
			// message between tool results and a user message means no bridge.
			lastRole = "system"
		} else if um, ok := asUserMsg(m); ok {
			// pi (openai-completions.ts:789-816): string-form content is sent as
			// a plain string; array content maps to an array of parts — even a
			// single text block — and an empty array skips the message entirely.
			if s, isString := um.StringContent(); isString {
				messages = append(messages, map[string]any{"role": "user", "content": sanitizeSurrogates(s)})
				lastRole = "user"
				continue
			}
			content := openAIUserContent(um.Content)
			if len(content) == 0 {
				continue // skipped without updating lastRole, like pi's `continue`
			}
			messages = append(messages, map[string]any{"role": "user", "content": content})
			lastRole = "user"
		} else if am, ok := asAssistantMsg(m); ok {
			msg := map[string]any{"role": "assistant"}
			// Some providers don't accept null content; use an empty string instead.
			if compat.RequiresAssistantAfterToolResult {
				msg["content"] = ""
			} else {
				msg["content"] = nil
			}

			var assistantTextParts []string
			var toolCalls []map[string]any
			// thinkingBlocks holds EVERY thinking block, not only the ones with
			// visible thinking: a reasoning_details-only turn parks its replay
			// sequence on a block whose thinking is empty.
			var thinkingBlocks []ai.ThinkingContent
			var nonEmptyThinkingBlocks []ai.ThinkingContent
			var toolCallBlocks []ai.ToolCall
			for _, c := range am.Content {
				switch v := c.(type) {
				case ai.TextContent:
					if jstext.Trim(v.Text) != "" {
						assistantTextParts = append(assistantTextParts, sanitizeSurrogates(v.Text))
					}
				case ai.ThinkingContent:
					thinkingBlocks = append(thinkingBlocks, v)
					if jstext.Trim(v.Thinking) != "" {
						nonEmptyThinkingBlocks = append(nonEmptyThinkingBlocks, v)
					}
				case ai.ToolCall:
					toolCallBlocks = append(toolCallBlocks, v)
					if property, isGrammar := grammarProps[v.Name]; isGrammar {
						input, gerr := grammarToolInput(v.Name, v.Arguments, property)
						if gerr != nil {
							return nil, gerr
						}
						toolCalls = append(toolCalls, map[string]any{
							"id": v.ID, "type": "custom",
							"custom": map[string]any{"name": v.Name, "input": sanitizeSurrogates(input)},
						})
						break
					}
					args, _ := jstext.Stringify(v.OrderedArguments())
					toolCalls = append(toolCalls, map[string]any{
						"id": v.ID, "type": "function",
						"function": map[string]any{"name": v.Name, "arguments": args},
					})
				}
			}
			assistantText := strings.Join(assistantTextParts, "")

			// The reasoning to replay, preferring the sequence a thinking block
			// carries over the single encrypted detail older sessions stored on a
			// tool call (pi :1233-1242, upstream b7bb00b93). The first thinking
			// block whose signature parses as a sequence wins; the legacy details
			// are only reached when no block carries one.
			var preservedReasoningDetails []json.RawMessage
			for _, b := range thinkingBlocks {
				if details := parseOpenAIReasoningDetails(b.ThinkingSignature); details != nil {
					preservedReasoningDetails = details
					break
				}
			}
			if len(preservedReasoningDetails) == 0 {
				for _, tc := range toolCallBlocks {
					if detail, ok := parseLegacyEncryptedReasoningDetail(tc.ThoughtSignature); ok {
						preservedReasoningDetails = append(preservedReasoningDetails, detail)
					}
				}
			}

			if len(nonEmptyThinkingBlocks) > 0 {
				if compat.RequiresThinkingAsText {
					// Convert thinking blocks to plain text (no tags) prepended to text parts.
					var tparts []string
					for _, b := range nonEmptyThinkingBlocks {
						tparts = append(tparts, sanitizeSurrogates(b.Thinking))
					}
					thinkingText := strings.Join(tparts, "\n\n")
					contentBlocks := []any{map[string]any{"type": "text", "text": thinkingText}}
					for _, p := range assistantTextParts {
						contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": p})
					}
					msg["content"] = contentBlocks
				} else {
					if assistantText != "" {
						msg["content"] = assistantText
					}
					// reasoning_details is the structured alternative to a raw
					// reasoning field: when there is a sequence to replay, the raw
					// field is not written at all.
					if len(preservedReasoningDetails) == 0 {
						// Use the signature from the first thinking block (llama.cpp + gpt-oss).
						signature := nonEmptyThinkingBlocks[0].ThinkingSignature
						if model.Provider == "opencode-go" && signature == "reasoning" {
							signature = "reasoning_content"
						}
						// Only a signature that names one of the three reasoning
						// fields becomes a request key; any other signature (a
						// Responses item id, an anthropic blob) is replay data that
						// would otherwise be sent as a bogus field name.
						if isOpenAICompletionsReasoningField(signature) {
							var thoughts []string
							for _, b := range nonEmptyThinkingBlocks {
								thoughts = append(thoughts, b.Thinking)
							}
							msg[signature] = strings.Join(thoughts, "\n")
						}
					}
				}
			} else if assistantText != "" {
				msg["content"] = assistantText
			}

			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			if len(preservedReasoningDetails) > 0 {
				msg["reasoning_details"] = preservedReasoningDetails
			}

			// DeepSeek-style providers reject replayed assistant turns that omit
			// reasoning_content when reasoning is enabled.
			if compat.RequiresReasoningContentOnAssistantMessages && model.Reasoning {
				if _, ok := msg["reasoning_content"]; !ok {
					msg["reasoning_content"] = ""
				}
			}

			// Skip assistant messages with neither content nor tool calls.
			content := msg["content"]
			hasContent := false
			switch cv := content.(type) {
			case string:
				hasContent = len(cv) > 0
			case []any:
				hasContent = len(cv) > 0
			}
			_, hasToolCalls := msg["tool_calls"]
			if !hasContent && !hasToolCalls {
				continue
			}
			messages = append(messages, msg)
			lastRole = "assistant"
		} else if _, ok := asToolResultMsg(m); ok {
			// Group consecutive tool-result messages, collecting images.
			var imageBlocks []any
			j := i
			for ; j < len(transformed); j++ {
				tr, ok := asToolResultMsg(transformed[j])
				if !ok {
					break
				}
				var text []string
				hasImages := false
				for _, c := range tr.Content {
					switch cv := c.(type) {
					case ai.TextContent:
						text = append(text, cv.Text)
					case ai.ImageContent:
						hasImages = true
					}
				}
				textResult := strings.Join(text, "\n")
				content := textResult
				if content == "" {
					// Only claim an attached image when one is actually present;
					// empty results with no image get a distinct placeholder so the
					// model doesn't hallucinate an attachment (pi #6290).
					if hasImages {
						content = "(see attached image)"
					} else {
						content = "(no tool output)"
					}
				}
				toolMsg := map[string]any{
					"role":         "tool",
					"content":      sanitizeSurrogates(content),
					"tool_call_id": tr.ToolCallID,
				}
				if compat.RequiresToolResultName && tr.ToolName != "" {
					toolMsg["name"] = tr.ToolName
				}
				messages = append(messages, toolMsg)

				if hasImages && modelHasImageInput {
					for _, c := range tr.Content {
						if img, ok := c.(ai.ImageContent); ok {
							imageBlocks = append(imageBlocks, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s", img.MimeType, img.Data)},
							})
						}
					}
				}
			}
			i = j - 1

			if len(imageBlocks) > 0 {
				if compat.RequiresAssistantAfterToolResult {
					messages = append(messages, map[string]any{
						"role": "assistant", "content": "I have processed the tool results.",
					})
				}
				content := []any{map[string]any{"type": "text", "text": "Attached image(s) from tool result:"}}
				content = append(content, imageBlocks...)
				messages = append(messages, map[string]any{"role": "user", "content": content})
				lastRole = "user"
			} else {
				lastRole = "toolResult"
			}
			continue
		}
	}

	params := map[string]any{
		"model":    model.ID,
		"messages": messages,
		"stream":   true,
	}
	if compat.SupportsUsageInStreaming {
		params["stream_options"] = map[string]any{"include_usage": true}
	}
	if compat.SupportsStore {
		params["store"] = false
	}

	// Prompt caching (OpenAI native, and long-retention compatible providers).
	// pi (openai-completions.ts:510-515): prompt_cache_key needs a sessionId,
	// but prompt_cache_retention is sent independently of any sessionId.
	retention := resolveCacheRetention(opts.CacheRetention, opts.Env)
	if opts.SessionID != "" &&
		((strings.Contains(model.BaseURL, "api.openai.com") && retention != ai.CacheNone) ||
			(retention == ai.CacheLong && compat.SupportsLongCacheRetention)) {
		params["prompt_cache_key"] = clampPromptCacheKey(opts.SessionID)
	}
	if retention == ai.CacheLong && compat.SupportsLongCacheRetention {
		params["prompt_cache_retention"] = "24h"
	}

	// Match pi: only send a max-token field when the caller explicitly sets one;
	// otherwise let the model use its own default (do NOT send model.MaxTokens).
	if opts.MaxTokens != nil && *opts.MaxTokens > 0 {
		params[compat.MaxTokensField] = *opts.MaxTokens
	}
	if opts.Temperature != nil {
		params["temperature"] = *opts.Temperature
	}

	if len(transcriptTools.RequestTools) > 0 {
		converted, cerr := convertOpenAITools(transcriptTools.RequestTools, compat)
		if cerr != nil {
			return nil, cerr
		}
		params["tools"] = converted
		if compat.ZaiToolStream {
			params["tool_stream"] = true
		}
	} else if hasToolHistory(req.Messages) {
		// Anthropic (via LiteLLM/proxy) requires a tools param when the conversation
		// already contains tool_calls / tool results — send an empty array.
		params["tools"] = []map[string]any{}
	}

	// Anthropic-style cache_control markers (e.g. OpenRouter routing an anthropic/ model).
	if cc := compatCacheControl(compat, resolveCacheRetention(opts.CacheRetention, opts.Env)); cc != nil {
		applyAnthropicCacheControl(messages, params["tools"], cc)
	}

	// An explicitly requested choice is forwarded whatever `tools` ended up as —
	// absent, the empty array above, or real tools. fe37e9f9b had briefly gated
	// this on `params.tools?.length` so gateways would not see a choice with
	// nothing to choose from; 6b36eb592 reverted that, because the gate also
	// swallowed a choice the caller had asked for on purpose.
	if opts.ToolChoice != nil {
		params["tool_choice"] = opts.ToolChoice
	}

	// vLLM scheduler priority (pi 256f63024). Sent whenever compat carried the
	// key, 0 included: pi's guard is `!== undefined`, and 0 is vLLM's own
	// default rather than an "unset" sentinel. An explicit null is carried too,
	// as `"priority":null` — see HasVLLMPriority.
	if compat.HasVLLMPriority {
		var priority any
		if compat.VLLMPriority != nil {
			priority = *compat.VLLMPriority
		}
		params["priority"] = priority
	}

	// pi computes both once here, before the thinking-format dispatch: the
	// chat-template/baseten branches read the budget for {"$var":"thinking.budget"}
	// and the top-level field write below reads both (upstream b23741269).
	budgetField := resolveThinkingTokenBudgetField(compat)
	thinkingBudget := resolveClampedThinkingBudget(model, opts, params)

	applyReasoningFormat(params, model, compat, opts.ReasoningEffort, thinkingBudget)

	// Cap reasoning with a top-level budget field. Independent of thinkingFormat:
	// the same server can serve zai, qwen or chat-template models. Reasoning and
	// the answer share max_tokens here, so an uncapped reasoning phase can consume
	// the whole response and leave no answer and no tool call (pi b23741269).
	if budgetField != "" && thinkingBudget != nil {
		params[budgetField] = *thinkingBudget
	}

	// OpenRouter provider routing preferences. pi checks
	// model.compat?.openRouterRouting for truthiness (:613), so an explicit
	// empty object {} is still sent (JS: {} is truthy); only absent/null is not.
	if compat.HasOpenRouterRouting {
		params["provider"] = compat.OpenRouterRouting
	}

	// Vercel AI Gateway provider routing preferences. pi 129eb460 dropped the
	// baseUrl gate — routing is sent whenever model.compat.vercelGatewayRouting
	// carries only/order (byte-identical for catalog models: all carry the
	// vercel baseUrl and none set routing).
	{
		routing := compat.VercelGatewayRouting
		if len(routing.Only) > 0 || len(routing.Order) > 0 {
			gatewayOptions := map[string]any{}
			if len(routing.Only) > 0 {
				gatewayOptions["only"] = routing.Only
			}
			if len(routing.Order) > 0 {
				gatewayOptions["order"] = routing.Order
			}
			params["providerOptions"] = map[string]any{"gateway": gatewayOptions}
		}
	}

	// Last so custom keys override the named request fields (upstream 25a2c8dc).
	// Per-request keys override model defaults (upstream c01f687e5).
	maps.Copy(params, model.SamplingParams)
	maps.Copy(params, opts.SamplingParams)

	return params, nil
}

// clientAPIKey ports pi's getClientApiKey (129eb460): when the request carries
// no api key but its options headers supply an authorization or
// cf-aig-authorization value, the OpenAI client uses an "unused" placeholder
// (later overwritten by the real header) instead of failing. Absent both, the
// stream fails with pi's exact message.
// A deletion marker is not a credential: pi's hasHeader requires `value !==
// null` before it counts a header as supplying auth. It matches the name by its
// JavaScript toLowerCase (jstext.ToLower), so "Author\u0130zation" is not
// "authorization" there, where strings.ToLower would make it one.
func clientAPIKey(provider ai.ProviderId, apiKey string, headers ai.ProviderHeaders) (string, error) {
	if apiKey != "" {
		return apiKey, nil
	}
	for k, v := range headers {
		lk := jstext.ToLower(k)
		if (lk == "authorization" || lk == "cf-aig-authorization") && v != nil && jstext.Trim(*v) != "" {
			return "unused", nil
		}
	}
	return "", fmt.Errorf("No API key for provider: %s", provider)
}

// convertOpenAITools serializes tools into the chat-completions tools shape
// (port of pi's convertTools).
func convertOpenAITools(tools []ai.Tool, compat openAICompletionsCompat) ([]map[string]any, error) {
	var out []map[string]any
	for _, t := range tools {
		grammar, err := resolveGrammarSampling(t, compat.SupportsOpenAIGrammarTools)
		if err != nil {
			return nil, err
		}
		if grammar != nil {
			out = append(out, map[string]any{
				"type": "custom",
				"custom": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"format": map[string]any{
						"type":    "grammar",
						"grammar": map[string]any{"syntax": grammar.format, "definition": grammar.definition},
					},
				},
			})
			continue
		}
		// Resolved unconditionally: a tool that REQUIRES strict sampling must fail
		// the request on a provider that cannot do it, even though the key itself
		// is only emitted where the provider supports it (some reject unknown fields).
		strict, err := resolveJSONSchemaStrictSampling(t, compat.SupportsStrictMode, nil)
		if err != nil {
			return nil, err
		}
		parameters, err := jsonSchemaToolParameters(t, strict)
		if err != nil {
			return nil, err
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if parameters != nil {
			if raw, err := json.Marshal(parameters); err == nil {
				var p any
				_ = json.Unmarshal(raw, &p)
				schema = p
			}
		}
		fn := map[string]any{"name": t.Name, "description": t.Description, "parameters": schema}
		if compat.SupportsStrictMode {
			fn["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, nil
}

// hasToolHistory reports whether the conversation already contains tool calls or
// tool results (port of pi's hasToolHistory). Anthropic via proxy requires the
// `tools` param to be present in that case.
func hasToolHistory(messages []ai.Message) bool {
	for _, m := range messages {
		if _, ok := asToolResultMsg(m); ok {
			return true
		}
		if am, ok := asAssistantMsg(m); ok {
			for _, c := range am.Content {
				if _, ok := c.(ai.ToolCall); ok {
					return true
				}
			}
		}
	}
	return false
}

// compatCacheControl returns an Anthropic-style cache_control marker when the
// provider uses that format (port of getCompatCacheControl).
func compatCacheControl(compat openAICompletionsCompat, retention ai.CacheRetention) map[string]any {
	if compat.CacheControlFormat != "anthropic" || retention == ai.CacheNone {
		return nil
	}
	cc := map[string]any{"type": "ephemeral"}
	if retention == ai.CacheLong && compat.SupportsLongCacheRetention {
		cc["ttl"] = "1h"
	}
	return cc
}

// applyAnthropicCacheControl marks the system prompt, the last tool, and the
// last user/assistant text block with cache_control (port of applyAnthropicCacheControl).
func applyAnthropicCacheControl(messages []map[string]any, tools any, cc map[string]any) {
	// System prompt: first system/developer message.
	for _, m := range messages {
		if r, _ := m["role"].(string); r == "system" || r == "developer" {
			addCacheControlToTextContent(m, cc)
			break
		}
	}
	// Last tool.
	if ts, ok := tools.([]map[string]any); ok && len(ts) > 0 {
		ts[len(ts)-1]["cache_control"] = cc
	}
	// Last user/assistant/tool message with text. Tool results are cacheable
	// too (pi bc41f612, #6940) — an OpenRouter turn that ends on a tool result
	// would otherwise leave the marker on an earlier message.
	for i := len(messages) - 1; i >= 0; i-- {
		if r, _ := messages[i]["role"].(string); r == "user" || r == "assistant" || r == "tool" {
			if addCacheControlToTextContent(messages[i], cc) {
				break
			}
		}
	}
}

// addCacheControlToTextContent stamps cache_control onto a message's text,
// converting string content to the block-array form (port of addCacheControlToTextContent).
func addCacheControlToTextContent(m map[string]any, cc map[string]any) bool {
	switch content := m["content"].(type) {
	case string:
		if content == "" {
			return false
		}
		m["content"] = []any{map[string]any{"type": "text", "text": content, "cache_control": cc}}
		return true
	case []any:
		for i := len(content) - 1; i >= 0; i-- {
			if part, ok := content[i].(map[string]any); ok {
				if t, _ := part["type"].(string); t == "text" {
					part["cache_control"] = cc
					return true
				}
			}
		}
	}
	return false
}

// resolveThinkingTokenBudgetField ports pi's resolveThinkingTokenBudgetField
// (openai-completions.ts:891, upstream b23741269). "" is pi's undefined. The
// boolean alias only fires when no explicit field name is set — and an explicit
// empty field name is falsy in JS, so it falls through here the same way.
func resolveThinkingTokenBudgetField(compat openAICompletionsCompat) string {
	if compat.ThinkingTokenBudgetField != "" {
		return compat.ThinkingTokenBudgetField
	}
	if compat.SupportsThinkingTokenBudget {
		return "thinking_token_budget"
	}
	return ""
}

// resolveClampedThinkingBudget ports pi's resolveClampedThinkingBudget
// (openai-completions.ts:899, upstream b23741269). nil is pi's undefined: no
// requested effort, a non-reasoning model, or nothing left after reserving
// answer room. The ceiling reads whichever max-token field buildOpenAIParams
// already set, so this must run after that assignment and before the
// samplingParams merge.
func resolveClampedThinkingBudget(model *ai.Model, opts *OpenAIOptions, params map[string]any) *int {
	if opts.ReasoningEffort == "" || !model.Reasoning {
		return nil
	}
	// pi: `params.max_tokens ?? params.max_completion_tokens ?? model.maxTokens`
	// — presence in params is pi's non-undefined (sampling params merge later
	// there too, so only the field set above can be present here).
	ceiling := model.MaxTokens
	if v, ok := params["max_tokens"].(int); ok {
		ceiling = v
	} else if v, ok := params["max_completion_tokens"].(int); ok {
		ceiling = v
	}
	// Always leave room for the answer, otherwise the budget recreates the bug it prevents.
	budget := clampThinkingBudgetToAnswerRoom(
		thinkingBudgetForLevel(ai.ThinkingLevel(opts.ReasoningEffort), opts.ThinkingBudgets), ceiling)
	if budget <= 0 {
		return nil
	}
	return &budget
}

// applyReasoningFormat sets reasoning fields per the provider's thinking format,
// mirroring pi's openai-completions reasoning dispatch (:556-610). The switch
// replicates pi's else-if chain exactly: note that "ant-ling" only matches when
// an effort was requested, so ant-ling with no effort falls through to the
// generic reasoning_effort branches at the bottom.
func applyReasoningFormat(params map[string]any, model *ai.Model, compat openAICompletionsCompat, level string, thinkingBudget *int) {
	enabled := level != ""
	switch {
	case compat.ThinkingFormat == "zai" && model.Reasoning:
		// pi (since 64b51efb): zai uses thinking: {type: "enabled"|"disabled"}
		// driven by !!options.reasoningEffort, not enable_thinking: bool.
		// pi (b91bdd5a / #6083): the enabled payload also carries
		// clear_thinking:false to preserve Z.AI thinking content; the disabled
		// payload stays bare {type:"disabled"}.
		if enabled {
			params["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
		} else {
			params["thinking"] = map[string]any{"type": "disabled"}
		}
		// pi (75b0d723): GLM-5.2 also accepts a native reasoning_effort. When an
		// effort was requested and the model opts in via supportsReasoningEffort,
		// send the thinkingLevelMap-mapped effort (raw level if unmapped, omitted
		// if mapped to null — e.g. GLM-5.2's minimal:null).
		if enabled && compat.SupportsReasoningEffort {
			if effort, ok := mappedEffortOrRaw(model, level); ok {
				params["reasoning_effort"] = effort
			}
		}
	case compat.ThinkingFormat == "qwen" && model.Reasoning:
		params["enable_thinking"] = enabled
		// pi (4c1a0b92): Qwen Token Plan reasoning models also take a native
		// reasoning_effort. Unlike the zai branch above, pi uses `??` here, so a
		// present-null mapping falls back to the raw level exactly like an absent
		// one — that is effortValue, not mappedEffortOrRaw. pi's follow-on
		// `typeof effort === "string"` guard cannot fail after the `??` —
		// effortValue returns string — so it has no Go counterpart.
		if enabled && compat.SupportsReasoningEffort {
			params["reasoning_effort"] = effortValue(model, level)
		}
	case compat.ThinkingFormat == "qwen-chat-template" && model.Reasoning:
		params["chat_template_kwargs"] = map[string]any{"enable_thinking": enabled, "preserve_thinking": true}
	case compat.ThinkingFormat == "chat-template" && model.Reasoning:
		// pi (8b97e75c): configurable chat_template_kwargs resolved from the
		// model's compat.chatTemplateKwargs ($var/omitWhenOff/scalar). Emitted
		// only when at least one kwarg survives resolution.
		if kw := buildChatTemplateValues(model, compat.ChatTemplateKwargs, level, thinkingBudget); kw != nil {
			params["chat_template_kwargs"] = kw
		}
	case compat.ThinkingFormat == "baseten" && model.Reasoning:
		// pi (c1019d92): Baseten takes configurable chat_template_args plus, when
		// the model opts in, a native reasoning_effort.
		if args := buildChatTemplateValues(model, compat.ChatTemplateArgs, level, thinkingBudget); args != nil {
			params["chat_template_args"] = args
		}
		if compat.SupportsReasoningEffort {
			// pi looks up thinkingLevelMap[requestedEffort] when an effort was
			// requested and thinkingLevelMap.off otherwise, falls back to the raw
			// requested effort when the lookup is undefined, and sends only string
			// results. Enabled, that is mappedEffortOrRaw (absent → raw level,
			// present-null → omit). Disabled, the fallback is the *absent* request
			// effort, so only a present-string off mapping is ever sent — pi's
			// `typeof effort === "string"` guard, which Go's *string map values
			// express directly.
			if enabled {
				if effort, ok := mappedEffortOrRaw(model, level); ok {
					params["reasoning_effort"] = effort
				}
			} else if off, ok := offEffortValue(model); ok {
				params["reasoning_effort"] = off
			}
		}
	case compat.ThinkingFormat == "deepseek" && model.Reasoning:
		// pi (0369bdb8 / #5760): when no effort, only send thinking:{disabled}
		// if the model's thinkingLevelMap.off is not present-null. Kimi K2.7 Code
		// is always-thinking (off:null) and rejects a disabled payload, so the
		// thinking key is omitted entirely. offEffortOrDefault's send flag is
		// exactly pi's `thinkingLevelMap?.off !== null`.
		if enabled {
			params["thinking"] = map[string]any{"type": "enabled"}
		} else if _, send := offEffortOrDefault(model, ""); send {
			params["thinking"] = map[string]any{"type": "disabled"}
		}
		if enabled && compat.SupportsReasoningEffort {
			params["reasoning_effort"] = effortValue(model, level)
		}
	case compat.ThinkingFormat == "openrouter" && model.Reasoning:
		if enabled {
			params["reasoning"] = map[string]any{"effort": effortValue(model, level)}
		} else if off, send := offEffortOrDefault(model, "none"); send {
			params["reasoning"] = map[string]any{"effort": off}
		}
	case compat.ThinkingFormat == "ant-ling" && model.Reasoning && enabled:
		if v, ok := offOrMapped(model, level); ok {
			params["reasoning"] = map[string]any{"effort": v}
		}
	case compat.ThinkingFormat == "together" && model.Reasoning:
		params["reasoning"] = map[string]any{"enabled": enabled}
		if enabled && compat.SupportsReasoningEffort {
			params["reasoning_effort"] = effortValue(model, level)
		}
	case compat.ThinkingFormat == "string-thinking" && model.Reasoning:
		if enabled {
			params["thinking"] = effortValue(model, level)
		} else if off, send := offEffortOrDefault(model, "none"); send {
			params["thinking"] = off
		}
	case enabled && model.Reasoning && compat.SupportsReasoningEffort:
		// OpenAI-style reasoning_effort.
		params["reasoning_effort"] = effortValue(model, level)
	case !enabled && model.Reasoning && compat.SupportsReasoningEffort:
		if off, ok := offEffortValue(model); ok {
			params["reasoning_effort"] = off
		}
	}
}

// offOrMapped returns the mapped effort value only when the model defines one
// (ant-ling sends reasoning only for non-null mapped efforts).
func offOrMapped(model *ai.Model, level string) (string, bool) {
	if model.ThinkingLevelMap != nil {
		if v, ok := model.ThinkingLevelMap[ai.ModelThinkingLevel(level)]; ok && v != nil {
			return *v, true
		}
	}
	return "", false
}

// mappedEffortOrRaw ports pi's zai reasoning_effort lookup (75b0d723):
//
//	const mapped = thinkingLevelMap?.[effort];
//	const value = mapped === undefined ? effort : mapped;
//	if (typeof value === "string") send value;
//
// so a level ABSENT from the map (undefined) falls back to the raw level, a
// present-null mapping omits the field (ok=false), and a present string uses the
// mapped value. This differs from effortValue, which returns the raw level for a
// present-null mapping rather than omitting.
func mappedEffortOrRaw(model *ai.Model, level string) (string, bool) {
	if model.ThinkingLevelMap != nil {
		if v, ok := model.ThinkingLevelMap[ai.ModelThinkingLevel(level)]; ok {
			if v == nil {
				return "", false
			}
			return *v, true
		}
	}
	return level, true
}

// openAIUserContent maps user content to OpenAI parts. pi always emits
// array-of-parts for array content — never joins multi-text with "\n"
// (openai-completions.ts:796-810) — and drops empty text parts before mapping,
// since some compatible providers reject them beside an image (pi #9797).
func openAIUserContent(content ai.ContentList) []any {
	var parts []any
	for _, c := range content {
		switch v := c.(type) {
		case ai.TextContent:
			if v.Text == "" {
				continue
			}
			parts = append(parts, map[string]any{"type": "text", "text": sanitizeSurrogates(v.Text)})
		case ai.ImageContent:
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s", v.MimeType, v.Data)},
			})
		}
	}
	return parts
}

// openAIToolCallIDSanitizeRe matches pi's /[^a-zA-Z0-9_-]/g.
var openAIToolCallIDSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// normalizeOpenAIToolCallID ports pi's normalizeToolCallId
// (openai-completions.ts convertMessages): pipe-separated ids from the Responses
// API ({call_id}|{item_id}, e.g. github-copilot / openai-codex / opencode).
// Multiple tool calls in the same turn can share call_id but differ by item_id,
// so item-level uniqueness is preserved when replaying into Chat Completions
// (which requires distinct tool call ids): both halves are sanitized and joined
// as {call_id}_{item_id}, falling back to {call_id}_{hash} when that exceeds the
// 40-char limit. Non-pipe ids longer than 40 chars are truncated only for
// provider "openai".
func normalizeOpenAIToolCallID(model *ai.Model, id string) string {
	if sep := strings.Index(id, "|"); sep >= 0 {
		callID := openAIToolCallIDSanitizeRe.ReplaceAllString(id[:sep], "_")
		itemID := openAIToolCallIDSanitizeRe.ReplaceAllString(id[sep+1:], "_")
		combined := callID
		if len(itemID) > 0 {
			combined = callID + "_" + itemID
		}
		// Sanitized halves are ASCII, so byte length == pi's UTF-16 .length here.
		if len(combined) <= 40 {
			return combined
		}
		hash := shortHash(id)
		if len(hash) > 8 {
			hash = hash[:8]
		}
		prefixLen := 40 - len(hash) - 1
		if prefixLen < 1 {
			prefixLen = 1
		}
		if prefixLen > len(callID) {
			prefixLen = len(callID)
		}
		return callID[:prefixLen] + "_" + hash
	}
	if model.Provider == "openai" {
		if r := []rune(id); len(r) > 40 {
			return string(r[:40])
		}
	}
	return id
}

// parseChunkUsage is pi's parseChunkUsage over a usage value of any type,
// each member read with JS semantics:
//
//	promptTokens = prompt_tokens || 0
//	cacheRead    = prompt_tokens_details?.cached_tokens ?? prompt_cache_hit_tokens ?? cached_tokens ?? 0
//	cacheWrite   = prompt_tokens_details?.cache_write_tokens || 0
//	input        = Math.max(0, promptTokens - cacheRead - cacheWrite)
//	output       = completion_tokens || 0
//	total        = input + output + cacheRead + cacheWrite
//
// Input excludes cache-read and cache-write tokens, and total is the sum of all
// four buckets. The `??` arms are nullish (upstream d3ab2af96: OpenAI and
// OpenRouter use prompt_tokens_details.cached_tokens, DeepSeek
// prompt_cache_hit_tokens, Kimi a top-level cached_tokens), so an explicit 0
// at any arm stops the chain. `-` and `+` convert as JS does and throw V8's
// TypeError where JS does. ai.Usage holds integers, so a member pi keeps as
// a string, or a fraction, is held as the integer its number truncates to.
func parseChunkUsage(raw any, model *ai.Model) (ai.Usage, error) {
	details := jsGet(raw, "prompt_tokens_details")
	promptTokens := jsOr(jsGet(raw, "prompt_tokens"), 0.0)
	cacheReadTokens := jsNullish(jsGet(details, "cached_tokens"), jsGet(raw, "prompt_cache_hit_tokens"), jsGet(raw, "cached_tokens"), 0.0)
	cacheWriteTokens := jsOr(jsGet(details, "cache_write_tokens"), 0.0)
	var operands [3]float64
	for i, v := range []any{promptTokens, cacheReadTokens, cacheWriteTokens} {
		n, err := jsToNumber(v)
		if err != nil {
			return ai.Usage{}, err
		}
		operands[i] = n
	}
	input := math.Max(0, operands[0]-operands[1]-operands[2])
	outputTokens := jsOr(jsGet(raw, "completion_tokens"), 0.0)
	var total any = input
	for _, v := range []any{outputTokens, cacheReadTokens, cacheWriteTokens} {
		sum, err := jsAdd(total, v)
		if err != nil {
			return ai.Usage{}, err
		}
		total = sum
	}
	usage := ai.Usage{
		Input:      jsTokenCount(input),
		Output:     jsTokenCount(outputTokens),
		CacheRead:  jsTokenCount(cacheReadTokens),
		CacheWrite: jsTokenCount(cacheWriteTokens),
		// pi: `reasoning: completion_tokens_details?.reasoning_tokens || 0` —
		// always set (0 when absent) for the completions path.
		Reasoning:   jsTokenCount(jsOr(jsGet(jsGet(raw, "completion_tokens_details"), "reasoning_tokens"), 0.0)),
		TotalTokens: jsTokenCount(total),
	}
	ai.CalculateCost(model, &usage)
	return usage, nil
}

// jsOr is `value || fallback`.
func jsOr(value, fallback any) any {
	if jsTruthy(value) {
		return value
	}
	return fallback
}

// jsNullish is `a ?? b ?? …`: the first value that is neither null nor
// undefined, else the last.
func jsNullish(values ...any) any {
	for _, v := range values[:len(values)-1] {
		if v != nil && v != jsUndefined {
			return v
		}
	}
	return values[len(values)-1]
}

// jsMapKey is value as a key of a JS Map, whose keys compare by
// SameValueZero: a string, number, boolean, null or undefined is its own key,
// and an object or array — which two parsed values never share — matches
// nothing (ok false).
func jsMapKey(value any) (any, bool) {
	switch value.(type) {
	case string, float64, bool, nil, jsUndefinedValue:
		return value, true
	}
	return nil, false
}

// jsLengthPositive is `value.length > 0` for a value that is neither null nor
// undefined: a string or array by its length, an object by its own "length"
// member compared as JS does (Number() of it, which throws V8's TypeError
// where String() does), and a number or boolean, which has no length, never.
func jsLengthPositive(value any) (bool, error) {
	switch v := value.(type) {
	case string:
		return v != "", nil
	case []any:
		return len(v) > 0, nil
	case ai.OrderedObject:
		n, err := jsToNumber(jsGet(v, "length"))
		return n > 0, err
	}
	return false, nil
}

// errToolCallsNotIterable is V8's TypeError for pi's
// `for (const toolCall of choice.delta.tool_calls)` over a truthy value that is
// neither an array nor a string.
var errToolCallsNotIterable = errors.New("choice.delta.tool_calls is not iterable")

// mapOpenAIFinishReason ports pi's mapStopReason: returns the stop reason plus
// an optional error message for filter/error finish reasons.
func mapOpenAIFinishReason(reason string) (ai.StopReason, string) {
	switch reason {
	case "stop", "end":
		return ai.StopStop, ""
	case "length":
		return ai.StopLength, ""
	case "tool_calls", "function_call":
		return ai.StopToolUse, ""
	case "content_filter":
		return ai.StopError, "Provider finish_reason: content_filter"
	case "network_error":
		return ai.StopError, "Provider finish_reason: network_error"
	default:
		return ai.StopError, fmt.Sprintf("Provider finish_reason: %s", reason)
	}
}

// mapOpenAIFinishReasonValue is mapStopReason over a truthy finish_reason of
// any type: a string maps as mapOpenAIFinishReason does, and any other value
// matches no case and falls to `Provider finish_reason: ${reason}`, whose
// template literal throws V8's TypeError where String() does.
func mapOpenAIFinishReasonValue(reason any) (ai.StopReason, string, error) {
	if text, isString := reason.(string); isString {
		stop, message := mapOpenAIFinishReason(text)
		return stop, message, nil
	}
	text, err := jsToString(reason)
	if err != nil {
		return ai.StopPending, "", err
	}
	return ai.StopError, "Provider finish_reason: " + text, nil
}

// iterateOpenAISSE reads a /chat/completions stream the way pi iterates the
// openai SDK's Stream (iterateOpenAIStream). Each item goes to onEvent (when
// set) first, as pi's loop opens with onProviderStreamEvent — whatever the
// item is — and then to handle, unless pi's `!chunk || typeof chunk !==
// "object"` skips it: null and every scalar. An array is an object to typeof,
// so it reaches handle, which reads no member off it.
func iterateOpenAISSE(body io.Reader, ctx context.Context, onEvent func(any) error, handle func(chunk any) error) error {
	return iterateOpenAIStream(body, ctx, func(item openaiStreamItem) error {
		if onEvent != nil {
			if err := onEvent(item.value); err != nil {
				return err
			}
		}
		switch item.value.(type) {
		case ai.OrderedObject, []any:
			return handle(item.value)
		}
		return nil
	})
}

// RegisterOpenAICompletions registers the openai-completions api provider.
//
// Note: the registry Stream entry point takes the unified ai.StreamOptions, so
// provider-native options (ToolChoice, ReasoningEffort) are not reachable
// through it. This is the documented Go API shape (callers needing native
// options use StreamOpenAICompletions directly) and intentionally diverges
// from pi's structurally-typed options object.
func RegisterOpenAICompletions() {
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIOpenAICompletions,
		Stream: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			o := &OpenAIOptions{}
			if opts != nil {
				o.StreamOptions = *opts
			}
			return StreamOpenAICompletions(ctx, model, req, o)
		},
		StreamSimple: StreamSimpleOpenAICompletions,
	})
}
