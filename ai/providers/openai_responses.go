package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf16"

	"github.com/sky-valley/pi/ai"
)

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
	SupportsDeveloperRole      bool
	SendSessionIDHeader        bool
	SupportsLongCacheRetention bool
}

func getResponsesCompat(model *ai.Model) responsesCompat {
	c := responsesCompat{
		SupportsDeveloperRole:      true,
		SendSessionIDHeader:        true,
		SupportsLongCacheRetention: true,
	}
	if len(model.Compat) == 0 {
		return c
	}
	var raw struct {
		SupportsDeveloperRole      *bool `json:"supportsDeveloperRole"`
		SendSessionIDHeader        *bool `json:"sendSessionIdHeader"`
		SupportsLongCacheRetention *bool `json:"supportsLongCacheRetention"`
	}
	if json.Unmarshal(model.Compat, &raw) != nil {
		return c
	}
	if raw.SupportsDeveloperRole != nil {
		c.SupportsDeveloperRole = *raw.SupportsDeveloperRole
	}
	if raw.SendSessionIDHeader != nil {
		c.SendSessionIDHeader = *raw.SendSessionIDHeader
	}
	if raw.SupportsLongCacheRetention != nil {
		c.SupportsLongCacheRetention = *raw.SupportsLongCacheRetention
	}
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
	// "flex", "priority"); it also scales cost (flex ×0.5, priority ×2).
	ServiceTier string
}

// StreamSimpleOpenAIResponses maps unified reasoning to Responses options.
func StreamSimpleOpenAIResponses(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	o := &OpenAIResponsesOptions{}
	if opts != nil {
		o.StreamOptions = opts.StreamOptions
		if opts.Reasoning != "" {
			clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(opts.Reasoning))
			if clamped != "off" {
				o.ReasoningEffort = string(clamped)
			}
		}
	}
	return StreamOpenAIResponses(ctx, model, req, o)
}

// StreamOpenAIResponses streams from an OpenAI Responses API (/responses).
func StreamOpenAIResponses(ctx context.Context, model *ai.Model, req ai.Context, opts *OpenAIResponsesOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &OpenAIResponsesOptions{}
	}

	go func() {
		output := &ai.AssistantMessage{
			Content: ai.ContentList{}, Api: model.Api, Provider: model.Provider, Model: model.ID,
			StopReason: ai.StopStop, Timestamp: nowMillis(),
		}
		fail := func(err error) {
			if ctx != nil && ctx.Err() != nil {
				output.StopReason = ai.StopAborted
			} else {
				output.StopReason = ai.StopError
			}
			output.ErrorMessage = err.Error()
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: output.StopReason, Error: output})
			stream.End()
		}

		if opts.APIKey == "" {
			fail(fmt.Errorf("No API key for provider: %s", model.Provider))
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
			resolved, cfErr := resolveCloudflareBaseURL(model)
			if cfErr != nil {
				fail(cfErr)
				return
			}
			baseURL = resolved
		}

		params, err := buildResponsesParams(model, req, opts)
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

		url := strings.TrimRight(baseURL, "/") + "/responses"
		build := func() (*http.Request, error) {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			r.Header.Set("content-type", "application/json")
			r.Header.Set("accept", "text/event-stream")
			// pi cloudflare-ai-gateway: Authorization defaults to null (the SDK
			// auth header is suppressed) unless headers explicitly provide one;
			// the API key rides in cf-aig-authorization instead.
			if model.Provider != "cloudflare-ai-gateway" {
				r.Header.Set("authorization", "Bearer "+opts.APIKey)
			}
			// pi createClient header precedence (openai-responses.ts:189-219):
			// model.headers, copilot dynamic headers, session cache headers,
			// then options.headers merged last so they can override defaults.
			for k, v := range model.Headers {
				r.Header.Set(k, v)
			}
			if model.Provider == "github-copilot" {
				for k, v := range buildCopilotDynamicHeaders(req.Messages, hasCopilotVisionInput(req.Messages)) {
					r.Header.Set(k, v)
				}
			}
			// Session cache headers (pi openai-responses.ts:200-205); the
			// sessionId is zeroed when cacheRetention is "none" (:115).
			if opts.SessionID != "" && resolveCacheRetention(opts.CacheRetention) != ai.CacheNone {
				if getResponsesCompat(model).SendSessionIDHeader {
					r.Header.Set("session_id", opts.SessionID)
				}
				r.Header.Set("x-client-request-id", opts.SessionID)
			}
			for k, v := range opts.Headers {
				r.Header.Set(k, v)
			}
			if model.Provider == "cloudflare-ai-gateway" {
				r.Header.Set("cf-aig-authorization", "Bearer "+opts.APIKey)
			}
			return r, nil
		}
		resp, err := sendWithRetry(ctx, build, retryFromOptions(opts.StreamOptions))
		if err != nil {
			fail(err)
			return
		}
		defer resp.Body.Close()
		if opts.OnResponse != nil {
			// pi awaits onResponse; a throw propagates and fails the stream.
			if err := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: flattenHeaders(resp.Header)}, model); err != nil {
				fail(err)
				return
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(resp.Body)
			fail(formatResponsesHTTPError(resp.StatusCode, data))
			return
		}

		stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: output.Clone()})

		var builders []*blockBuilder
		var current *blockBuilder
		// Per-item streaming state (mirrors pi's currentItem tracking).
		// hasMsgContentPart / msgLastPartType track the message item's content
		// parts; hasSummaryPart tracks whether the reasoning item has an open
		// summary part to append deltas to.
		var hasMsgContentPart bool
		var msgLastPartType string
		var hasSummaryPart bool
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
		idx := func() int { return len(builders) - 1 }

		err = iterateOpenAISSE2(resp.Body, ctx, func(ev responsesEvent) error {
			switch ev.Type {
			case "response.created":
				if ev.Response != nil {
					output.ResponseID = ev.Response.ID
				}
			case "response.output_item.added":
				if ev.Item == nil {
					return nil
				}
				switch ev.Item.Type {
				case "reasoning":
					current = &blockBuilder{kind: "thinking"}
					hasSummaryPart = false
					builders = append(builders, current)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingStart, ContentIndex: idx(), Partial: output.Clone()})
				case "message":
					current = &blockBuilder{kind: "text"}
					hasMsgContentPart = false
					msgLastPartType = ""
					builders = append(builders, current)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextStart, ContentIndex: idx(), Partial: output.Clone()})
				case "function_call":
					current = &blockBuilder{kind: "toolCall", toolID: ev.Item.CallID + "|" + ev.Item.ID, toolName: ev.Item.Name, args: map[string]any{}}
					current.partialJSON.WriteString(ev.Item.Arguments)
					builders = append(builders, current)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallStart, ContentIndex: idx(), Partial: output.Clone()})
				}
			case "response.reasoning_summary_part.added":
				if current != nil && current.kind == "thinking" {
					hasSummaryPart = true
				}
			case "response.reasoning_summary_text.delta":
				if current != nil && current.kind == "thinking" && hasSummaryPart {
					current.thinking.WriteString(ev.Delta)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: idx(), Delta: ev.Delta, Partial: output.Clone()})
				}
			case "response.reasoning_summary_part.done":
				if current != nil && current.kind == "thinking" && hasSummaryPart {
					current.thinking.WriteString("\n\n")
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: idx(), Delta: "\n\n", Partial: output.Clone()})
				}
			case "response.reasoning_text.delta":
				if current != nil && current.kind == "thinking" {
					current.thinking.WriteString(ev.Delta)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: idx(), Delta: ev.Delta, Partial: output.Clone()})
				}
			case "response.content_part.added":
				if current != nil && current.kind == "text" && ev.Part != nil {
					if ev.Part.Type == "output_text" || ev.Part.Type == "refusal" {
						hasMsgContentPart = true
						msgLastPartType = ev.Part.Type
					}
				}
			case "response.output_text.delta":
				if current != nil && current.kind == "text" && hasMsgContentPart && msgLastPartType == "output_text" {
					current.text.WriteString(ev.Delta)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: idx(), Delta: ev.Delta, Partial: output.Clone()})
				}
			case "response.refusal.delta":
				if current != nil && current.kind == "text" && hasMsgContentPart && msgLastPartType == "refusal" {
					current.text.WriteString(ev.Delta)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: idx(), Delta: ev.Delta, Partial: output.Clone()})
				}
			case "response.function_call_arguments.delta":
				if current != nil && current.kind == "toolCall" {
					current.partialJSON.WriteString(ev.Delta)
					current.args = parseStreamingJSON(current.partialJSON.String())
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: idx(), Delta: ev.Delta, Partial: output.Clone()})
				}
			case "response.function_call_arguments.done":
				if current != nil && current.kind == "toolCall" {
					previous := current.partialJSON.String()
					current.partialJSON.Reset()
					current.partialJSON.WriteString(ev.Arguments)
					current.args = parseStreamingJSON(ev.Arguments)
					materialize()
					// Emit the trailing delta so a provider that only sends
					// .done (no incremental deltas) still yields full args.
					if strings.HasPrefix(ev.Arguments, previous) {
						delta := ev.Arguments[len(previous):]
						if delta != "" {
							stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: idx(), Delta: delta, Partial: output.Clone()})
						}
					}
				}
			case "response.output_item.done":
				if ev.Item == nil {
					return nil
				}
				// pi branches on the ITEM type (shared :443-492); the
				// function_call branch runs even without a matching current
				// block, and currentBlock is cleared only in matched branches.
				switch {
				case ev.Item.Type == "reasoning" && current != nil && current.kind == "thinking":
					summaryText := joinPartsText(ev.Item.Summary, "\n\n")
					contentText := joinPartsText(ev.Item.Content, "\n\n")
					rebuilt := summaryText
					if rebuilt == "" {
						rebuilt = contentText
					}
					if rebuilt == "" {
						rebuilt = current.thinking.String()
					}
					current.thinking.Reset()
					current.thinking.WriteString(rebuilt)
					if len(ev.RawItem) > 0 {
						current.thinkingSig = string(ev.RawItem)
					}
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: idx(), Content: current.thinking.String(), Partial: output.Clone()})
					current = nil
				case ev.Item.Type == "message" && current != nil && current.kind == "text":
					// Rebuild final text from item.content (output_text or refusal).
					var sb strings.Builder
					for _, p := range ev.Item.Content {
						if p.Type == "refusal" {
							sb.WriteString(p.Refusal)
						} else {
							sb.WriteString(p.Text)
						}
					}
					current.text.Reset()
					current.text.WriteString(sb.String())
					textSigs[idx()] = encodeTextSignatureV1(ev.Item.ID, ev.Item.Phase)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: idx(), Content: current.text.String(), Partial: output.Clone()})
					current = nil
				case ev.Item.Type == "function_call":
					var tc ai.ToolCall
					if current != nil && current.kind == "toolCall" {
						if current.partialJSON.Len() > 0 {
							current.args = parseStreamingJSON(current.partialJSON.String())
						} else {
							current.args = parseStreamingJSON(orEmptyJSON(ev.Item.Arguments))
						}
						materialize()
						tc = current.toContent().(ai.ToolCall)
					} else {
						// pi shared :481-491: a done without a prior added still
						// constructs the toolCall and emits toolcall_end (the
						// block is NOT appended to content — replicated as-is).
						tc = ai.ToolCall{
							ID:        ev.Item.CallID + "|" + ev.Item.ID,
							Name:      ev.Item.Name,
							Arguments: parseStreamingJSON(orEmptyJSON(ev.Item.Arguments)),
						}
					}
					current = nil
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: idx(), ToolCall: &tc, Partial: output.Clone()})
				}
			case "response.completed":
				// pi runs the usage/stop-reason block even when response is
				// null (shared :493-521): cost calc, service-tier pricing,
				// mapStopReason(undefined), and toolUse promotion all apply.
				if ev.Response != nil {
					if ev.Response.ID != "" {
						output.ResponseID = ev.Response.ID
					}
					if ev.Response.Usage != nil {
						cached := ev.Response.Usage.InputTokensDetails.CachedTokens
						output.Usage = ai.Usage{
							Input:       ev.Response.Usage.InputTokens - cached,
							Output:      ev.Response.Usage.OutputTokens,
							CacheRead:   cached,
							TotalTokens: ev.Response.Usage.TotalTokens,
						}
					}
				}
				ai.CalculateCost(model, &output.Usage)
				// Service-tier pricing: the response-reported tier wins over the
				// requested one (shared :511-516 `response?.service_tier ?? options.serviceTier`).
				serviceTier := opts.ServiceTier
				if ev.Response != nil && ev.Response.ServiceTier != "" {
					serviceTier = ev.Response.ServiceTier
				}
				applyResponsesServiceTierPricing(&output.Usage, serviceTier, model)
				status := ""
				if ev.Response != nil {
					status = ev.Response.Status
				}
				reason, statusErr := mapResponsesStatus(status)
				if statusErr != nil {
					return statusErr
				}
				output.StopReason = reason
				for _, b := range builders {
					if b.kind == "toolCall" && output.StopReason == ai.StopStop {
						output.StopReason = ai.StopToolUse
					}
				}
			case "error":
				return fmt.Errorf("Error Code %s: %s", ev.Code, ev.Message)
			case "response.failed":
				return fmt.Errorf("%s", responsesFailedMessage(ev))
			}
			return nil
		})

		if err != nil {
			fail(err)
			return
		}
		if ctx != nil && ctx.Err() != nil {
			fail(fmt.Errorf("Request was aborted"))
			return
		}
		// pi openai-responses.ts:140-142: a stream that ended with an error or
		// aborted stop reason (e.g. response.completed status "cancelled")
		// must fail, never emit done.
		if output.StopReason == ai.StopAborted || output.StopReason == ai.StopError {
			fail(fmt.Errorf("An unknown error occurred"))
			return
		}
		materialize()
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: output.StopReason, Message: output})
		stream.End()
	}()

	return stream
}

func buildResponsesParams(model *ai.Model, req ai.Context, opts *OpenAIResponsesOptions) (map[string]any, error) {
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
	retention := resolveCacheRetention(opts.CacheRetention)
	// Prompt caching: route same-session requests to a stable cache key so OpenAI
	// can reuse the cached system-prompt + tool prefix (latency/cost win).
	if retention != ai.CacheNone && opts.SessionID != "" {
		params["prompt_cache_key"] = clampPromptCacheKey(opts.SessionID)
	}
	// pi sets prompt_cache_retention independent of sessionId (openai-responses.ts:239).
	if retention == ai.CacheLong && getResponsesCompat(model).SupportsLongCacheRetention {
		params["prompt_cache_retention"] = "24h"
	}
	// pi `if (options?.maxTokens)` — JS truthiness, so 0 is omitted.
	if opts.MaxTokens != nil && *opts.MaxTokens != 0 {
		params["max_output_tokens"] = *opts.MaxTokens
	}
	if opts.Temperature != nil {
		params["temperature"] = *opts.Temperature
	}
	if opts.ServiceTier != "" {
		params["service_tier"] = opts.ServiceTier
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			var p any = map[string]any{"type": "object", "properties": map[string]any{}}
			if t.Parameters != nil {
				if raw, err := json.Marshal(t.Parameters); err == nil {
					var decoded any
					_ = json.Unmarshal(raw, &decoded)
					p = decoded
				}
			}
			tools = append(tools, map[string]any{
				// strict defaults to false (port of convertResponsesTools).
				"type": "function", "name": t.Name, "description": t.Description, "parameters": p, "strict": false,
			})
		}
		params["tools"] = tools
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
	}
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

// responsesInput converts unified messages into Responses API input items
// (port of convertResponsesMessages). It errors when an assistant thinking
// block carries an unparseable thinkingSignature (pi's JSON.parse throws and
// fails the stream).
func responsesInput(model *ai.Model, req ai.Context) ([]any, error) {
	var items []any

	compat := getResponsesCompat(model)
	if req.SystemPrompt != "" {
		role := "system"
		if model.Reasoning && compat.SupportsDeveloperRole {
			role = "developer"
		}
		items = append(items, map[string]any{"role": role, "content": sanitizeSurrogates(req.SystemPrompt)})
	}

	// Tool-call id normalization happens inside transformMessages (gated on
	// !isSameModel there), so the toolCallId map also rewrites tool results
	// and synthetic orphan results, exactly like pi.
	transformed := transformMessages(req.Messages, model, buildResponsesToolCallIDNormalizer(model, req.Messages))
	imageInput := modelSupportsImages(model)

	msgIndex := 0
	for _, m := range transformed {
		if um, ok := asUserMsg(m); ok {
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
			isDifferentModel := am.Model != model.ID && am.Provider == model.Provider && am.Api == model.Api
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
					// For different-model messages, drop the fc_ item id to
					// avoid pairing validation against reasoning items.
					if isDifferentModel && strings.HasPrefix(itemID, "fc_") {
						itemID = ""
					}
					args, _ := json.Marshal(orEmptyMap(v.Arguments))
					fc := map[string]any{"type": "function_call", "call_id": callID, "name": v.Name, "arguments": string(args)}
					if itemID != "" {
						fc["id"] = itemID
					}
					output = append(output, fc)
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
			} else {
				outputVal = sanitizeSurrogates("(see attached image)")
			}

			items = append(items, map[string]any{
				"type": "function_call_output", "call_id": callID, "output": outputVal,
			})
		}
		msgIndex++
	}
	return items, nil
}

// serviceTierCostMultiplier ports getServiceTierCostMultiplier
// (openai-responses.ts:279-291): flex halves cost, priority doubles it
// (×2.5 for the exact model id "gpt-5.5").
func serviceTierCostMultiplier(model *ai.Model, serviceTier string) float64 {
	switch serviceTier {
	case "flex":
		return 0.5
	case "priority":
		if model.ID == "gpt-5.5" {
			return 2.5
		}
		return 2
	default:
		return 1
	}
}

// applyResponsesServiceTierPricing ports applyServiceTierPricing
// (openai-responses.ts:293-306).
func applyResponsesServiceTierPricing(usage *ai.Usage, serviceTier string, model *ai.Model) {
	multiplier := serviceTierCostMultiplier(model, serviceTier)
	if multiplier == 1 {
		return
	}
	usage.Cost.Input *= multiplier
	usage.Cost.Output *= multiplier
	usage.Cost.CacheRead *= multiplier
	usage.Cost.CacheWrite *= multiplier
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

// mapResponsesStatus ports pi's mapStopReason: unknown statuses are an error
// (pi throws), surfaced here as a returned error that fails the stream.
func mapResponsesStatus(status string) (ai.StopReason, error) {
	switch status {
	case "":
		return ai.StopStop, nil
	case "completed":
		return ai.StopStop, nil
	case "incomplete":
		return ai.StopLength, nil
	case "failed", "cancelled":
		return ai.StopError, nil
	case "in_progress", "queued":
		return ai.StopStop, nil
	default:
		return ai.StopStop, fmt.Errorf("Unhandled stop reason: %s", status)
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

// responsesFailedMessage surfaces error.code/message or incomplete_details.reason
// from a response.failed event (port of pi's response.failed handling).
func responsesFailedMessage(ev responsesEvent) string {
	if ev.Response != nil {
		if ev.Response.Error != nil {
			code := ev.Response.Error.Code
			if code == "" {
				code = "unknown"
			}
			msg := ev.Response.Error.Message
			if msg == "" {
				msg = "no message"
			}
			return fmt.Sprintf("%s: %s", code, msg)
		}
		if ev.Response.IncompleteDetails != nil && ev.Response.IncompleteDetails.Reason != "" {
			return fmt.Sprintf("incomplete: %s", ev.Response.IncompleteDetails.Reason)
		}
	}
	return "Unknown error (no error details in response)"
}

// ---- SSE event types ----

type responsesContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesEvent struct {
	Type      string                `json:"type"`
	Delta     string                `json:"delta"`
	Arguments string                `json:"arguments"`
	Code      string                `json:"code"`
	Message   string                `json:"message"`
	Part      *responsesContentPart `json:"part"`
	Item      *struct {
		Type      string                 `json:"type"`
		ID        string                 `json:"id"`
		CallID    string                 `json:"call_id"`
		Name      string                 `json:"name"`
		Arguments string                 `json:"arguments"`
		Phase     string                 `json:"phase"`
		Summary   []responsesContentPart `json:"summary"`
		Content   []responsesContentPart `json:"content"`
	} `json:"item"`
	RawItem  json.RawMessage `json:"-"`
	Response *struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		ServiceTier string `json:"service_tier"`
		Usage       *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

func iterateOpenAISSE2(body io.Reader, ctx context.Context, handle func(responsesEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if ctx != nil && ctx.Err() != nil {
			return fmt.Errorf("Request was aborted")
		}
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		// Capture the raw item for reasoning-signature round-tripping.
		var probe struct {
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal([]byte(data), &probe) == nil {
			ev.RawItem = probe.Item
		}
		if err := handle(ev); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// RegisterOpenAIResponses registers the openai-responses api provider.
func RegisterOpenAIResponses() {
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIOpenAIResponses,
		Stream: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			o := &OpenAIResponsesOptions{}
			if opts != nil {
				o.StreamOptions = *opts
			}
			return StreamOpenAIResponses(ctx, model, req, o)
		},
		StreamSimple: StreamSimpleOpenAIResponses,
	})
}
