package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/sky-valley/pi/ai"
)

// OpenAIOptions are provider-native options for the OpenAI completions stream.
type OpenAIOptions struct {
	ai.StreamOptions
	ReasoningEffort string
	// ToolChoice mirrors pi's OpenAICompletionsOptions.toolChoice: a string
	// ("auto"|"none"|"required") or an object {type:"function",function:{name}}.
	ToolChoice any
}

// StreamSimpleOpenAICompletions maps unified reasoning to OpenAI options.
func StreamSimpleOpenAICompletions(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	o := &OpenAIOptions{}
	if opts != nil {
		o.StreamOptions = opts.StreamOptions
		if opts.Reasoning != "" {
			clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(opts.Reasoning))
			if clamped != "off" {
				o.ReasoningEffort = string(clamped)
			}
		}
	}
	// pi buildBaseOptions: maxTokens = clamp(options?.maxTokens ?? model.maxTokens).
	mt := ai.ClampMaxTokensToContext(model, req, ai.SimpleMaxTokensDefault(model, opts))
	o.MaxTokens = &mt
	return StreamOpenAICompletions(ctx, model, req, o)
}

// StreamOpenAICompletions streams from an OpenAI-compatible /chat/completions API.
func StreamOpenAICompletions(ctx context.Context, model *ai.Model, req ai.Context, opts *OpenAIOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &OpenAIOptions{}
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

		apiKey, keyErr := clientAPIKey(model.Provider, opts.APIKey, opts.Headers)
		if keyErr != nil {
			fail(keyErr)
			return
		}

		// pi createClient runs before onPayload: Cloudflare providers resolve
		// {VAR} placeholders in baseUrl from the environment, failing the stream
		// when a variable is unset (openai-completions.ts:490).
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

		var body any = buildOpenAIParams(model, req, opts)
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

		url := strings.TrimRight(baseURL, "/") + "/chat/completions"
		build := func() (*http.Request, error) {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			r.Header.Set("content-type", "application/json")
			r.Header.Set("accept", "text/event-stream")
			if model.Provider != "cloudflare-ai-gateway" {
				r.Header.Set("authorization", "Bearer "+apiKey)
			}
			// pi mergeProviderAttributionHeaders (sdk.ts) puts the attribution
			// bundle at the bottom of the precedence stack: emit session +
			// default attribution first so model.headers and options.headers
			// override them.
			applyAttributionDefaults(r.Header.Set, model, opts.SessionID)
			// pi createClient header precedence (openai-completions.ts:458-477):
			// model.headers first, then copilot dynamic headers, then session
			// affinity (overrides model headers), with options.headers merged last.
			for k, v := range model.Headers {
				r.Header.Set(k, v)
			}
			if model.Provider == "github-copilot" {
				for k, v := range buildCopilotDynamicHeaders(req.Messages, hasCopilotVisionInput(req.Messages)) {
					r.Header.Set(k, v)
				}
			}
			// Session-affinity headers for cache-routing providers (e.g. Fireworks).
			if opts.SessionID != "" && resolveCacheRetention(opts.CacheRetention, opts.Env) != ai.CacheNone &&
				getOpenAICompat(model).SendSessionAffinityHeaders {
				r.Header.Set("session_id", opts.SessionID)
				r.Header.Set("x-client-request-id", opts.SessionID)
				r.Header.Set("x-session-affinity", opts.SessionID)
			}
			// pi options.headers (consumer) are spread last and win over
			// everything above, including model.headers and the attribution
			// defaults.
			for k, v := range opts.Headers {
				r.Header.Set(k, v)
			}
			// Cloudflare AI Gateway carries the API key in cf-aig-authorization
			// (set after all merges, like pi's defaultHeaders construction) and
			// leaves the upstream Authorization to model.headers.
			if model.Provider == "cloudflare-ai-gateway" {
				r.Header.Set("cf-aig-authorization", "Bearer "+apiKey)
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
			_ = opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: flattenHeaders(resp.Header)}, model)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(resp.Body)
			err := formatProviderError("OpenAI", resp.StatusCode, data)
			// Some providers via OpenRouter give additional information in
			// error.metadata.raw; pi appends it to the error message
			// (openai-completions.ts:417-419).
			if raw := openRouterErrorRaw(data); raw != "" {
				err = fmt.Errorf("%s\n%s", err.Error(), raw)
			}
			fail(err)
			return
		}

		stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: output.Clone()})

		var textBuilder *blockBuilder
		var thinkBuilder *blockBuilder
		// pi ensureToolCallBlock keeps BOTH maps (openai-completions.ts:229-265):
		// lookup by stream index when the delta carries one, falling back to id,
		// and registers blocks under both keys.
		toolBuildersByIndex := map[int]*blockBuilder{}
		toolBuildersByID := map[string]*blockBuilder{}
		builderHasIndex := map[*blockBuilder]bool{}
		// thoughtSignature captured from streamed reasoning_details, keyed by
		// builder (blockBuilder itself has no thoughtSignature field).
		toolThoughtSigs := map[*blockBuilder]string{}
		// pi pendingReasoningDetailsByToolCallId (openai-completions.ts:200): an
		// encrypted reasoning_detail can arrive in a delta BEFORE the tool-call
		// block carrying its id exists. Buffer it by id and attach it when the
		// block is registered, instead of dropping it.
		pendingReasoningDetails := map[string]string{}
		var order []*blockBuilder
		materialize := func() {
			content := make(ai.ContentList, len(order))
			for i, b := range order {
				c := b.toContent()
				if tc, ok := c.(ai.ToolCall); ok {
					if sig := toolThoughtSigs[b]; sig != "" {
						tc.ThoughtSignature = sig
						c = tc
					}
				}
				content[i] = c
			}
			output.Content = content
		}
		indexOf := func(b *blockBuilder) int {
			for i, x := range order {
				if x == b {
					return i
				}
			}
			return -1
		}
		// pi applyPendingReasoningDetail (openai-completions.ts:256): once a
		// block's id is known, drain any reasoning_detail buffered under it. Keyed
		// on the block's id exactly like pi (an index-only block whose id has not
		// yet arrived is skipped, matching pi).
		applyPendingReasoningDetail := func(b *blockBuilder) {
			if b.toolID == "" {
				return
			}
			if sig, ok := pendingReasoningDetails[b.toolID]; ok {
				toolThoughtSigs[b] = sig
				delete(pendingReasoningDetails, b.toolID)
			}
		}
		ensureToolCallBlock := func(tcDelta openAIToolCallDelta) *blockBuilder {
			var b *blockBuilder
			if tcDelta.Index != nil {
				b = toolBuildersByIndex[*tcDelta.Index]
			}
			if b == nil && tcDelta.ID != "" {
				b = toolBuildersByID[tcDelta.ID]
			}
			if b == nil {
				b = &blockBuilder{kind: "toolCall", toolID: tcDelta.ID, args: map[string]any{}}
				if tcDelta.Function != nil {
					b.toolName = tcDelta.Function.Name
				}
				if tcDelta.Index != nil {
					toolBuildersByIndex[*tcDelta.Index] = b
					builderHasIndex[b] = true
				}
				if tcDelta.ID != "" {
					toolBuildersByID[tcDelta.ID] = b
				}
				order = append(order, b)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallStart, ContentIndex: indexOf(b), Partial: output.Clone()})
			}
			if tcDelta.Index != nil && !builderHasIndex[b] {
				builderHasIndex[b] = true
				toolBuildersByIndex[*tcDelta.Index] = b
			}
			if tcDelta.ID != "" {
				toolBuildersByID[tcDelta.ID] = b
			}
			applyPendingReasoningDetail(b)
			return b
		}

		hasFinishReason := false
		err = iterateOpenAISSE(resp.Body, ctx, func(chunk openAIChunk) error {
			// OpenAI documents ChatCompletionChunk.id as the unique chat completion
			// identifier shared by every chunk in a streamed completion.
			if output.ResponseID == "" && chunk.ID != "" {
				output.ResponseID = chunk.ID
			}
			if output.ResponseModel == "" && chunk.Model != "" && chunk.Model != model.ID {
				output.ResponseModel = chunk.Model
			}
			if chunk.Usage != nil {
				output.Usage = parseChunkUsage(chunk.Usage, model)
			}
			if len(chunk.Choices) == 0 {
				return nil
			}
			choice := chunk.Choices[0]
			d := choice.Delta

			// Fallback: some providers (e.g. Moonshot) return usage in choice.usage
			// instead of the top-level chunk.usage.
			if chunk.Usage == nil && choice.Usage != nil {
				output.Usage = parseChunkUsage(choice.Usage, model)
			}

			if choice.FinishReason != "" {
				stopReason, errMsg := mapOpenAIFinishReason(choice.FinishReason)
				output.StopReason = stopReason
				if errMsg != "" {
					output.ErrorMessage = errMsg
				}
				hasFinishReason = true
			}

			// pi processes delta fields in order: content first, then reasoning,
			// then tool_calls, then reasoning_details (openai-completions.ts:299-385).
			if d.Content != "" {
				if textBuilder == nil {
					textBuilder = &blockBuilder{kind: "text"}
					order = append(order, textBuilder)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextStart, ContentIndex: indexOf(textBuilder), Partial: output.Clone()})
				}
				textBuilder.text.WriteString(d.Content)
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: indexOf(textBuilder), Delta: d.Content, Partial: output.Clone()})
			}

			// Reasoning may arrive in reasoning_content (llama.cpp), reasoning, or
			// reasoning_text. Use the first non-empty field to avoid duplication
			// (e.g. chutes.ai returns both with the same content), and record the
			// field name as the thinking signature.
			reasoningFields := []struct {
				name  string
				value string
			}{
				{"reasoning_content", d.ReasoningContent},
				{"reasoning", d.Reasoning},
				{"reasoning_text", d.ReasoningText},
			}
			var reasoningDelta, reasoningSig string
			for _, f := range reasoningFields {
				if f.value != "" {
					reasoningSig = f.name
					reasoningDelta = f.value
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

			for _, tcDelta := range d.ToolCalls {
				b := ensureToolCallBlock(tcDelta)
				// id and name are first-wins, never overwritten (pi :350-356).
				if b.toolID == "" && tcDelta.ID != "" {
					b.toolID = tcDelta.ID
					toolBuildersByID[tcDelta.ID] = b
				}
				if b.toolName == "" && tcDelta.Function != nil && tcDelta.Function.Name != "" {
					b.toolName = tcDelta.Function.Name
				}

				// pi pushes a toolcall_delta for EVERY delta entry, with an empty
				// delta string when no arguments arrived (pi :358-369).
				delta := ""
				if tcDelta.Function != nil && tcDelta.Function.Arguments != "" {
					delta = tcDelta.Function.Arguments
					b.partialJSON.WriteString(delta)
					b.args = parseStreamingJSON(b.partialJSON.String())
				}
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: indexOf(b), Delta: delta, Partial: output.Clone()})
			}

			// reasoning_details: OpenRouter-style encrypted reasoning attached to a
			// tool call by id (pi :373-385). The matching tool call's
			// thoughtSignature becomes the serialized detail object.
			for _, rawDetail := range d.ReasoningDetails {
				var detail struct {
					Type string          `json:"type"`
					ID   string          `json:"id"`
					Data json.RawMessage `json:"data"`
				}
				if json.Unmarshal(rawDetail, &detail) != nil {
					continue
				}
				// pi isEncryptedReasoningDetail (7d0497fd): type must be
				// "reasoning.encrypted" and both id and data non-empty strings.
				// A non-string id fails the typed unmarshal above; data is checked
				// here (a number/object/null detail.data is rejected, matching
				// typeof data === "string").
				if detail.Type != "reasoning.encrypted" || detail.ID == "" {
					continue
				}
				var data string
				if json.Unmarshal(detail.Data, &data) != nil || data == "" {
					continue
				}
				// pi stores JSON.stringify(detail); compacting the raw entry
				// preserves field order and unknown fields.
				var buf bytes.Buffer
				if json.Compact(&buf, []byte(rawDetail)) != nil {
					continue
				}
				serialized := buf.String()
				// pi looks the tool call up by id (toolCallBlocksById), not by
				// scanning content order. When the matching block has not been
				// created yet, buffer the detail and attach it on block creation
				// instead of dropping it (upstream 7d0497fd).
				if b := toolBuildersByID[detail.ID]; b != nil {
					toolThoughtSigs[b] = serialized
					materialize()
				} else {
					pendingReasoningDetails[detail.ID] = serialized
				}
			}
			return nil
		})

		if err != nil {
			// A mid-stream read/handler failure throws past pi's finalization
			// loop straight into the catch block; do the same here.
			fail(err)
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
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: indexOf(b), Content: b.thinking.String(), Partial: output.Clone()})
			case "toolCall":
				b.args = parseStreamingJSON(b.partialJSON.String())
				materialize()
				tc := b.toContent().(ai.ToolCall)
				if sig := toolThoughtSigs[b]; sig != "" {
					tc.ThoughtSignature = sig
				}
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: indexOf(b), ToolCall: &tc, Partial: output.Clone()})
			}
		}

		if ctx != nil && ctx.Err() != nil {
			fail(fmt.Errorf("Request was aborted"))
			return
		}
		if output.StopReason == ai.StopAborted {
			fail(fmt.Errorf("Request was aborted"))
			return
		}
		if output.StopReason == ai.StopError {
			msg := output.ErrorMessage
			if msg == "" {
				msg = "Provider returned an error stop reason"
			}
			fail(fmt.Errorf("%s", msg))
			return
		}
		// pi throws unconditionally when no finish_reason arrived (:402-404),
		// including for zero-choice streams that only carried [DONE].
		if !hasFinishReason {
			fail(fmt.Errorf("Stream ended without finish_reason"))
			return
		}
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: output.StopReason, Message: output})
		stream.End()
	}()

	return stream
}

// openRouterErrorRaw extracts error.metadata.raw from a provider error body
// (pi appends it to errorMessage; some OpenRouter upstreams put detail there).
func openRouterErrorRaw(body []byte) string {
	var parsed struct {
		Error struct {
			Metadata struct {
				Raw string `json:"raw"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Error.Metadata.Raw
}

func buildOpenAIParams(model *ai.Model, req ai.Context, opts *OpenAIOptions) map[string]any {
	compat := getOpenAICompat(model)

	var messages []map[string]any
	if req.SystemPrompt != "" {
		role := "system"
		if model.Reasoning && compat.SupportsDeveloperRole {
			role = "developer"
		}
		messages = append(messages, map[string]any{"role": role, "content": sanitizeSurrogates(req.SystemPrompt)})
	}
	transformed := transformMessages(req.Messages, model, func(id string) string {
		return normalizeOpenAIToolCallID(model, id)
	})
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

		if um, ok := asUserMsg(m); ok {
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
			var thinkingBlocks []ai.ThinkingContent
			for _, c := range am.Content {
				switch v := c.(type) {
				case ai.TextContent:
					if strings.TrimSpace(v.Text) != "" {
						assistantTextParts = append(assistantTextParts, sanitizeSurrogates(v.Text))
					}
				case ai.ThinkingContent:
					if strings.TrimSpace(v.Thinking) != "" {
						thinkingBlocks = append(thinkingBlocks, v)
					}
				case ai.ToolCall:
					args, _ := json.Marshal(v.Arguments)
					toolCalls = append(toolCalls, map[string]any{
						"id": v.ID, "type": "function",
						"function": map[string]any{"name": v.Name, "arguments": string(args)},
					})
				}
			}
			assistantText := strings.Join(assistantTextParts, "")

			if len(thinkingBlocks) > 0 {
				if compat.RequiresThinkingAsText {
					// Convert thinking blocks to plain text (no tags) prepended to text parts.
					var tparts []string
					for _, b := range thinkingBlocks {
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
					// Use the signature from the first thinking block (llama.cpp + gpt-oss).
					signature := thinkingBlocks[0].ThinkingSignature
					if model.Provider == "opencode-go" && signature == "reasoning" {
						signature = "reasoning_content"
					}
					if signature != "" {
						var thoughts []string
						for _, b := range thinkingBlocks {
							thoughts = append(thoughts, b.Thinking)
						}
						msg[signature] = strings.Join(thoughts, "\n")
					}
				}
			} else if assistantText != "" {
				msg["content"] = assistantText
			}

			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
				// reasoning_details round-trip: parse each tool call's stored signature.
				var reasoningDetails []any
				for _, c := range am.Content {
					tc, ok := c.(ai.ToolCall)
					if !ok || tc.ThoughtSignature == "" {
						continue
					}
					var detail any
					if json.Unmarshal([]byte(tc.ThoughtSignature), &detail) == nil && detail != nil {
						reasoningDetails = append(reasoningDetails, detail)
					}
				}
				if len(reasoningDetails) > 0 {
					msg["reasoning_details"] = reasoningDetails
				}
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
					content = "(see attached image)"
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

	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
			if t.Parameters != nil {
				if raw, err := json.Marshal(t.Parameters); err == nil {
					var p any
					_ = json.Unmarshal(raw, &p)
					schema = p
				}
			}
			fn := map[string]any{"name": t.Name, "description": t.Description, "parameters": schema}
			if compat.SupportsStrictMode {
				fn["strict"] = false
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		params["tools"] = tools
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

	if opts.ToolChoice != nil {
		params["tool_choice"] = opts.ToolChoice
	}

	applyReasoningFormat(params, model, compat, opts.ReasoningEffort)

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

	return params
}

// clientAPIKey ports pi's getClientApiKey (129eb460): when the request carries
// no api key but its options headers supply an authorization or
// cf-aig-authorization value, the OpenAI client uses an "unused" placeholder
// (later overwritten by the real header) instead of failing. Absent both, the
// stream fails with pi's exact message.
func clientAPIKey(provider ai.ProviderId, apiKey string, headers map[string]string) (string, error) {
	if apiKey != "" {
		return apiKey, nil
	}
	for k, v := range headers {
		lk := strings.ToLower(k)
		if (lk == "authorization" || lk == "cf-aig-authorization") && strings.TrimSpace(v) != "" {
			return "unused", nil
		}
	}
	return "", fmt.Errorf("No API key for provider: %s", provider)
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
	// Last user/assistant message with text.
	for i := len(messages) - 1; i >= 0; i-- {
		if r, _ := messages[i]["role"].(string); r == "user" || r == "assistant" {
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

// applyReasoningFormat sets reasoning fields per the provider's thinking format,
// mirroring pi's openai-completions reasoning dispatch (:556-610). The switch
// replicates pi's else-if chain exactly: note that "ant-ling" only matches when
// an effort was requested, so ant-ling with no effort falls through to the
// generic reasoning_effort branches at the bottom.
func applyReasoningFormat(params map[string]any, model *ai.Model, compat openAICompletionsCompat, level string) {
	enabled := level != ""
	switch {
	case compat.ThinkingFormat == "zai" && model.Reasoning:
		// pi (since 64b51efb): zai uses thinking: {type: "enabled"|"disabled"}
		// driven by !!options.reasoningEffort, not enable_thinking: bool.
		t := "disabled"
		if enabled {
			t = "enabled"
		}
		params["thinking"] = map[string]any{"type": t}
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
	case compat.ThinkingFormat == "qwen-chat-template" && model.Reasoning:
		params["chat_template_kwargs"] = map[string]any{"enable_thinking": enabled, "preserve_thinking": true}
	case compat.ThinkingFormat == "chat-template" && model.Reasoning:
		// pi (8b97e75c): configurable chat_template_kwargs resolved from the
		// model's compat.chatTemplateKwargs ($var/omitWhenOff/scalar). Emitted
		// only when at least one kwarg survives resolution.
		if kw := buildChatTemplateKwargs(model, compat, level); kw != nil {
			params["chat_template_kwargs"] = kw
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
// (openai-completions.ts:796-810).
func openAIUserContent(content ai.ContentList) []any {
	var parts []any
	for _, c := range content {
		switch v := c.(type) {
		case ai.TextContent:
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
// (openai-completions.ts:753-766): pipe-separated ids from the Responses API
// ({call_id}|{encrypted}, e.g. github-copilot / openai-codex / opencode) keep
// only the call_id, sanitized and clamped to 40 chars. Non-pipe ids longer
// than 40 chars are truncated only for provider "openai".
func normalizeOpenAIToolCallID(model *ai.Model, id string) string {
	if strings.Contains(id, "|") {
		callID := strings.SplitN(id, "|", 2)[0]
		callID = openAIToolCallIDSanitizeRe.ReplaceAllString(callID, "_")
		if r := []rune(callID); len(r) > 40 {
			return string(r[:40])
		}
		return callID
	}
	if model.Provider == "openai" {
		if r := []rune(id); len(r) > 40 {
			return string(r[:40])
		}
	}
	return id
}

// parseChunkUsage converts raw chunk usage into our Usage, matching pi's
// parseChunkUsage: input excludes cache-read and cache-write tokens, and total
// is the sum of all four buckets.
func parseChunkUsage(raw *openAIChunkUsage, model *ai.Model) ai.Usage {
	promptTokens := raw.PromptTokens
	cacheWriteTokens := 0
	if raw.PromptTokensDetails != nil {
		cacheWriteTokens = raw.PromptTokensDetails.CacheWriteTokens
	}
	// pi: `cached_tokens ?? prompt_cache_hit_tokens ?? 0` — an explicit
	// cached_tokens of 0 must NOT fall back to prompt_cache_hit_tokens.
	cacheReadTokens := raw.PromptCacheHitTokens
	if raw.PromptTokensDetails != nil && raw.PromptTokensDetails.CachedTokens != nil {
		cacheReadTokens = *raw.PromptTokensDetails.CachedTokens
	}
	input := promptTokens - cacheReadTokens - cacheWriteTokens
	if input < 0 {
		input = 0
	}
	// pi: `reasoning: completion_tokens_details?.reasoning_tokens || 0` — always
	// set (0 when absent) for the completions path.
	reasoningTokens := 0
	if raw.CompletionTokensDetails != nil {
		reasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}
	usage := ai.Usage{
		Input:       input,
		Output:      raw.CompletionTokens,
		CacheRead:   cacheReadTokens,
		CacheWrite:  cacheWriteTokens,
		Reasoning:   reasoningTokens,
		TotalTokens: input + raw.CompletionTokens + cacheReadTokens + cacheWriteTokens,
	}
	ai.CalculateCost(model, &usage)
	return usage
}

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

// ---- SSE chunk types ----

type openAIChunkUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		// CachedTokens is a pointer so an explicit 0 beats the
		// prompt_cache_hit_tokens fallback (pi `??` nullish semantics).
		CachedTokens     *int `json:"cached_tokens"`
		CacheWriteTokens int  `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// openAIToolCallDelta is one entry of choice.delta.tool_calls. Index is a
// pointer so an absent index (id-keyed streams) is distinguishable from 0.
type openAIToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string                `json:"content"`
			ReasoningContent string                `json:"reasoning_content"`
			Reasoning        string                `json:"reasoning"`
			ReasoningText    string                `json:"reasoning_text"`
			ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
			// ReasoningDetails entries stay raw so the stored thoughtSignature
			// preserves the provider's field order and unknown fields.
			ReasoningDetails []json.RawMessage `json:"reasoning_details"`
		} `json:"delta"`
		FinishReason string            `json:"finish_reason"`
		Usage        *openAIChunkUsage `json:"usage"`
	} `json:"choices"`
	Usage *openAIChunkUsage `json:"usage"`
}

func iterateOpenAISSE(body io.Reader, ctx context.Context, handle func(openAIChunk) error) error {
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
		var chunk openAIChunk
		if err := parseJSONWithRepair(data, &chunk); err != nil {
			// Deliberate leniency: unparseable SSE data lines are skipped rather
			// than failing the stream (some providers interleave junk/keepalives).
			continue
		}
		if err := handle(chunk); err != nil {
			return err
		}
	}
	return scanner.Err()
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
		Stream: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			o := &OpenAIOptions{}
			if opts != nil {
				o.StreamOptions = *opts
			}
			return StreamOpenAICompletions(ctx, model, req, o)
		},
		StreamSimple: StreamSimpleOpenAICompletions,
	})
}
