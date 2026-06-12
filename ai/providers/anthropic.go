package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/sky-valley/pi/ai"
)

const (
	anthropicVersion          = "2023-06-01"
	fineGrainedToolStreamBeta = "fine-grained-tool-streaming-2025-05-14"
	interleavedThinkingBeta   = "interleaved-thinking-2025-05-14"
	claudeCodeVersion         = "2.1.75"
	anthropicDefaultBaseURL   = "https://api.anthropic.com"
)

// claudeCodeTools is the canonical Claude Code 2.x tool-name casing used in
// OAuth "stealth" mode.
var claudeCodeTools = []string{
	"Read", "Write", "Edit", "Bash", "Grep", "Glob", "AskUserQuestion",
	"EnterPlanMode", "ExitPlanMode", "KillShell", "NotebookEdit", "Skill",
	"Task", "TaskOutput", "TodoWrite", "WebFetch", "WebSearch",
}

var ccToolLookup = func() map[string]string {
	m := map[string]string{}
	for _, t := range claudeCodeTools {
		m[strings.ToLower(t)] = t
	}
	return m
}()

func toClaudeCodeName(name string) string {
	if c, ok := ccToolLookup[strings.ToLower(name)]; ok {
		return c
	}
	return name
}

func fromClaudeCodeName(name string, tools []ai.Tool) string {
	lower := strings.ToLower(name)
	for _, t := range tools {
		if strings.ToLower(t.Name) == lower {
			return t.Name
		}
	}
	return name
}

// AnthropicOptions are the provider-native options for streamAnthropic.
type AnthropicOptions struct {
	ai.StreamOptions
	// ThinkingProvided mirrors pi's tri-state `thinkingEnabled?: boolean`
	// (anthropic.ts:951,975-977): when false (the zero value, pi `undefined`)
	// the `thinking` key is omitted entirely; when true, ThinkingEnabled
	// selects enabled/adaptive vs an explicit {type:"disabled"}.
	ThinkingProvided     bool
	ThinkingEnabled      bool
	ThinkingBudgetTokens int
	Effort               string // low|medium|high|xhigh|max
	ThinkingDisplay      string // summarized|omitted
	InterleavedThinking  *bool
	ToolChoice           any
}

// AnthropicCompat holds resolved Anthropic-messages compatibility flags.
type anthropicCompat struct {
	supportsEagerToolInputStreaming bool
	supportsLongCacheRetention      bool
	sendSessionAffinityHeaders      bool
	supportsCacheControlOnTools     bool
	supportsTemperature             bool
	allowEmptySignature             bool
	forceAdaptiveThinking           bool
}

func getAnthropicCompat(model *ai.Model) anthropicCompat {
	c := anthropicCompat{
		supportsEagerToolInputStreaming: true,
		supportsLongCacheRetention:      true,
		supportsCacheControlOnTools:     true,
		supportsTemperature:             true,
	}
	isFireworks := model.Provider == "fireworks"
	isCFAnthropic := model.Provider == "cloudflare-ai-gateway" && strings.Contains(model.BaseURL, "anthropic")
	if isFireworks {
		c.supportsEagerToolInputStreaming = false
		c.supportsLongCacheRetention = false
		c.supportsCacheControlOnTools = false
	}
	c.sendSessionAffinityHeaders = isFireworks || isCFAnthropic

	// Apply explicit model.compat overrides.
	if len(model.Compat) > 0 {
		var raw struct {
			SupportsEagerToolInputStreaming *bool `json:"supportsEagerToolInputStreaming"`
			SupportsLongCacheRetention      *bool `json:"supportsLongCacheRetention"`
			SendSessionAffinityHeaders      *bool `json:"sendSessionAffinityHeaders"`
			SupportsCacheControlOnTools     *bool `json:"supportsCacheControlOnTools"`
			SupportsTemperature             *bool `json:"supportsTemperature"`
			AllowEmptySignature             *bool `json:"allowEmptySignature"`
			ForceAdaptiveThinking           *bool `json:"forceAdaptiveThinking"`
		}
		if json.Unmarshal(model.Compat, &raw) == nil {
			setBool(&c.supportsEagerToolInputStreaming, raw.SupportsEagerToolInputStreaming)
			setBool(&c.supportsLongCacheRetention, raw.SupportsLongCacheRetention)
			setBool(&c.sendSessionAffinityHeaders, raw.SendSessionAffinityHeaders)
			setBool(&c.supportsCacheControlOnTools, raw.SupportsCacheControlOnTools)
			setBool(&c.supportsTemperature, raw.SupportsTemperature)
			setBool(&c.allowEmptySignature, raw.AllowEmptySignature)
			setBool(&c.forceAdaptiveThinking, raw.ForceAdaptiveThinking)
		}
	}
	return c
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

func resolveCacheRetention(r ai.CacheRetention) ai.CacheRetention {
	if r != "" {
		return r
	}
	// Match pi: PI_CACHE_RETENTION=long opts the default into long retention.
	if os.Getenv("PI_CACHE_RETENTION") == "long" {
		return ai.CacheLong
	}
	return ai.CacheShort
}

type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

func getCacheControl(model *ai.Model, retention ai.CacheRetention) (ai.CacheRetention, *cacheControl) {
	r := resolveCacheRetention(retention)
	if r == ai.CacheNone {
		return r, nil
	}
	cc := &cacheControl{Type: "ephemeral"}
	if r == ai.CacheLong && getAnthropicCompat(model).supportsLongCacheRetention {
		cc.TTL = "1h"
	}
	return r, cc
}

func isOAuthToken(apiKey string) bool { return strings.Contains(apiKey, "sk-ant-oat") }

// StreamSimpleAnthropic maps unified reasoning to AnthropicOptions then streams.
func StreamSimpleAnthropic(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	var base ai.StreamOptions
	if opts != nil {
		base = opts.StreamOptions
	}
	aopts := AnthropicOptions{StreamOptions: base}

	reasoning := ai.ThinkingLevel("")
	if opts != nil {
		reasoning = opts.Reasoning
	}
	// pi streamSimpleAnthropic always passes thinkingEnabled explicitly
	// (false for no reasoning, true otherwise) — so Provided is always set here.
	aopts.ThinkingProvided = true
	if reasoning == "" {
		aopts.ThinkingEnabled = false
		return StreamAnthropic(ctx, model, req, &aopts)
	}

	compat := getAnthropicCompat(model)
	if compat.forceAdaptiveThinking {
		aopts.ThinkingEnabled = true
		aopts.Effort = mapThinkingLevelToEffort(model, reasoning)
		return StreamAnthropic(ctx, model, req, &aopts)
	}

	var budgets *ai.ThinkingBudgets
	if opts != nil {
		budgets = opts.ThinkingBudgets
	}
	maxTokens, thinkingBudget := adjustMaxTokensForThinking(base.MaxTokens, model.MaxTokens, reasoning, budgets)
	mt := maxTokens
	aopts.MaxTokens = &mt
	aopts.ThinkingEnabled = true
	aopts.ThinkingBudgetTokens = thinkingBudget
	return StreamAnthropic(ctx, model, req, &aopts)
}

func mapThinkingLevelToEffort(model *ai.Model, level ai.ThinkingLevel) string {
	if model.ThinkingLevelMap != nil {
		if mapped, ok := model.ThinkingLevelMap[ai.ModelThinkingLevel(level)]; ok && mapped != nil {
			return *mapped
		}
	}
	switch level {
	case ai.ThinkingMinimal, ai.ThinkingLow:
		return "low"
	case ai.ThinkingMedium:
		return "medium"
	case ai.ThinkingHigh:
		return "high"
	default:
		return "high"
	}
}

func adjustMaxTokensForThinking(baseMaxTokens *int, modelMaxTokens int, level ai.ThinkingLevel, custom *ai.ThinkingBudgets) (int, int) {
	budgets := map[ai.ThinkingLevel]int{
		ai.ThinkingMinimal: 1024, ai.ThinkingLow: 2048, ai.ThinkingMedium: 8192, ai.ThinkingHigh: 16384,
	}
	if custom != nil {
		if custom.Minimal != nil {
			budgets[ai.ThinkingMinimal] = *custom.Minimal
		}
		if custom.Low != nil {
			budgets[ai.ThinkingLow] = *custom.Low
		}
		if custom.Medium != nil {
			budgets[ai.ThinkingMedium] = *custom.Medium
		}
		if custom.High != nil {
			budgets[ai.ThinkingHigh] = *custom.High
		}
	}
	clamped := level
	if level == ai.ThinkingXHigh {
		clamped = ai.ThinkingHigh
	}
	thinkingBudget := budgets[clamped]
	const minOutput = 1024
	var maxTokens int
	if baseMaxTokens == nil {
		maxTokens = modelMaxTokens
	} else {
		maxTokens = *baseMaxTokens + thinkingBudget
		if maxTokens > modelMaxTokens {
			maxTokens = modelMaxTokens
		}
	}
	if maxTokens <= thinkingBudget {
		thinkingBudget = maxTokens - minOutput
		if thinkingBudget < 0 {
			thinkingBudget = 0
		}
	}
	return maxTokens, thinkingBudget
}

var toolIDCleaner = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func normalizeToolCallID(id string) string {
	cleaned := toolIDCleaner.ReplaceAllString(id, "_")
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

// StreamAnthropic streams an assistant response from the Anthropic Messages API.
func StreamAnthropic(ctx context.Context, model *ai.Model, req ai.Context, opts *AnthropicOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &AnthropicOptions{}
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

		apiKey := opts.APIKey
		if apiKey == "" {
			fail(fmt.Errorf("No API key for provider: %s", model.Provider))
			return
		}
		// pi createClient checks the cloudflare-ai-gateway and github-copilot
		// provider branches BEFORE sniffing the key for an OAuth token
		// (anthropic.ts:802,826,848) — those branches always report
		// isOAuthToken=false even for sk-ant-oat keys.
		oauth := model.Provider != "cloudflare-ai-gateway" &&
			model.Provider != "github-copilot" &&
			isOAuthToken(apiKey)

		body := buildAnthropicParams(model, req, oauth, opts)
		if opts.OnPayload != nil {
			next, perr := opts.OnPayload(body, model)
			if perr != nil {
				// pi: a throw from onPayload propagates and fails the stream.
				fail(perr)
				return
			}
			// pi: any `!== undefined` return replaces the params wholesale.
			if next != nil {
				if m, ok := next.(map[string]any); ok {
					body = m
				}
			}
		}
		payload, err := json.Marshal(body)
		if err != nil {
			fail(err)
			return
		}

		baseURL := model.BaseURL
		if model.Provider == "cloudflare-ai-gateway" {
			// pi: resolveCloudflareBaseUrl(model) throws on a missing env var,
			// which surfaces as a failed stream.
			resolved, rerr := resolveCloudflareBaseURL(model)
			if rerr != nil {
				fail(rerr)
				return
			}
			baseURL = resolved
		}
		if baseURL == "" {
			baseURL = anthropicDefaultBaseURL
		}
		url := strings.TrimRight(baseURL, "/") + "/v1/messages"
		build := func() (*http.Request, error) {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			applyAnthropicHeaders(r, model, opts, oauth, apiKey, len(req.Tools) > 0, req.Messages)
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
			fail(formatProviderError("Anthropic", resp.StatusCode, data))
			return
		}

		stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: output.Clone()})

		builders := []*blockBuilder{}
		indexMap := map[int]int{}
		materialize := func() {
			content := make(ai.ContentList, len(builders))
			for i, b := range builders {
				content[i] = b.toContent()
			}
			output.Content = content
		}

		sawStart, sawStop := false, false
		err = iterateAnthropicSSE(resp.Body, ctx, func(ev anthropicStreamEvent) error {
			switch ev.Type {
			case "message_start":
				sawStart = true
				if ev.Message != nil {
					output.ResponseID = ev.Message.ID
					applyUsage(&output.Usage, ev.Message.Usage, true)
					ai.CalculateCost(model, &output.Usage)
				}
			case "content_block_start":
				if ev.ContentBlock == nil {
					return nil
				}
				var b *blockBuilder
				var evType ai.EventType
				switch ev.ContentBlock.Type {
				case "text":
					b = &blockBuilder{kind: "text"}
					evType = ai.EventTextStart
				case "thinking":
					b = &blockBuilder{kind: "thinking"}
					evType = ai.EventThinkingStart
				case "redacted_thinking":
					b = &blockBuilder{kind: "thinking", redacted: true, thinkingSig: ev.ContentBlock.Data}
					b.thinking.WriteString("[Reasoning redacted]")
					evType = ai.EventThinkingStart
				case "tool_use":
					name := ev.ContentBlock.Name
					if oauth {
						name = fromClaudeCodeName(name, req.Tools)
					}
					b = &blockBuilder{kind: "toolCall", toolID: ev.ContentBlock.ID, toolName: name, args: map[string]any{}}
					evType = ai.EventToolCallStart
				default:
					return nil
				}
				builders = append(builders, b)
				indexMap[ev.Index] = len(builders) - 1
				materialize()
				stream.Push(ai.AssistantMessageEvent{Type: evType, ContentIndex: len(builders) - 1, Partial: output.Clone()})
			case "content_block_delta":
				idx, ok := indexMap[ev.Index]
				if !ok || ev.Delta == nil {
					return nil
				}
				b := builders[idx]
				// pi only applies a delta when the indexed block has the matching
				// type (anthropic.ts:586-627); mismatches are dropped silently.
				switch ev.Delta.Type {
				case "text_delta":
					if b.kind != "text" {
						return nil
					}
					b.text.WriteString(ev.Delta.Text)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: idx, Delta: ev.Delta.Text, Partial: output.Clone()})
				case "thinking_delta":
					if b.kind != "thinking" {
						return nil
					}
					b.thinking.WriteString(ev.Delta.Thinking)
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: idx, Delta: ev.Delta.Thinking, Partial: output.Clone()})
				case "input_json_delta":
					if b.kind != "toolCall" {
						return nil
					}
					b.partialJSON.WriteString(ev.Delta.PartialJSON)
					b.args = parseStreamingJSON(b.partialJSON.String())
					materialize()
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: idx, Delta: ev.Delta.PartialJSON, Partial: output.Clone()})
				case "signature_delta":
					if b.kind != "thinking" {
						return nil
					}
					b.thinkingSig += ev.Delta.Signature
				}
			case "content_block_stop":
				idx, ok := indexMap[ev.Index]
				if !ok {
					return nil
				}
				b := builders[idx]
				materialize()
				switch b.kind {
				case "text":
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: idx, Content: b.text.String(), Partial: output.Clone()})
				case "thinking":
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: idx, Content: b.thinking.String(), Partial: output.Clone()})
				case "toolCall":
					b.args = parseStreamingJSON(b.partialJSON.String())
					materialize()
					tc := b.toContent().(ai.ToolCall)
					stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: idx, ToolCall: &tc, Partial: output.Clone()})
				}
			case "message_delta":
				if ev.Delta != nil && ev.Delta.StopReason != "" {
					sr, err := mapAnthropicStopReason(ev.Delta.StopReason)
					if err != nil {
						return err
					}
					output.StopReason = sr
				}
				if ev.Usage != nil {
					applyUsage(&output.Usage, *ev.Usage, false)
					ai.CalculateCost(model, &output.Usage)
				}
			case "message_stop":
				sawStop = true
			}
			return nil
		})

		if err != nil {
			fail(err)
			return
		}
		if sawStart && !sawStop {
			fail(fmt.Errorf("Anthropic stream ended before message_stop"))
			return
		}
		if ctx != nil && ctx.Err() != nil {
			fail(fmt.Errorf("Request was aborted"))
			return
		}
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

type blockBuilder struct {
	kind        string
	text        strings.Builder
	thinking    strings.Builder
	thinkingSig string
	redacted    bool
	toolID      string
	toolName    string
	partialJSON strings.Builder
	args        map[string]any
}

func (b *blockBuilder) toContent() ai.Content {
	switch b.kind {
	case "text":
		return ai.TextContent{Text: b.text.String()}
	case "thinking":
		return ai.ThinkingContent{Thinking: b.thinking.String(), ThinkingSignature: b.thinkingSig, Redacted: b.redacted}
	case "toolCall":
		args := b.args
		if args == nil {
			args = map[string]any{}
		}
		return ai.ToolCall{ID: b.toolID, Name: b.toolName, Arguments: args}
	}
	return ai.TextContent{}
}

// ---- request building ----

func buildAnthropicParams(model *ai.Model, req ai.Context, oauth bool, opts *AnthropicOptions) map[string]any {
	retention := ai.CacheRetention("")
	if opts != nil {
		retention = opts.CacheRetention
	}
	_, cc := getCacheControl(model, retention)
	compat := getAnthropicCompat(model)

	maxTokens := model.MaxTokens
	if opts != nil && opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}

	params := map[string]any{
		"model":      model.ID,
		"messages":   convertAnthropicMessages(req.Messages, model, oauth, cc, compat.allowEmptySignature),
		"max_tokens": maxTokens,
		"stream":     true,
	}

	textBlock := func(text string) map[string]any {
		blk := map[string]any{"type": "text", "text": sanitizeSurrogates(text)}
		if cc != nil {
			blk["cache_control"] = cc
		}
		return blk
	}
	if oauth {
		system := []any{textBlock("You are Claude Code, Anthropic's official CLI for Claude.")}
		if req.SystemPrompt != "" {
			system = append(system, textBlock(req.SystemPrompt))
		}
		params["system"] = system
	} else if req.SystemPrompt != "" {
		params["system"] = []any{textBlock(req.SystemPrompt)}
	}

	// pi: `!options?.thinkingEnabled` — only an explicit thinkingEnabled:true
	// suppresses temperature; unset (not Provided) behaves like false.
	thinkingOn := opts != nil && opts.ThinkingProvided && opts.ThinkingEnabled
	if opts != nil && opts.Temperature != nil && !thinkingOn && compat.supportsTemperature {
		params["temperature"] = *opts.Temperature
	}

	if len(req.Tools) > 0 {
		var toolCC *cacheControl
		if compat.supportsCacheControlOnTools {
			toolCC = cc
		}
		params["tools"] = convertAnthropicTools(req.Tools, oauth, compat.supportsEagerToolInputStreaming, toolCC)
	}

	// pi tri-state (anthropic.ts:950-978): thinkingEnabled undefined omits the
	// thinking key entirely; explicit true enables; explicit false sends
	// {type:"disabled"} — unless the model's thinkingLevelMap carries an
	// explicit off:null (present-nil), which marks "disabled" as unsupported
	// and omits the key too (pi 9ccfcd7c: `thinkingLevelMap?.off !== null`).
	if model.Reasoning && opts != nil && opts.ThinkingProvided {
		if opts.ThinkingEnabled {
			display := opts.ThinkingDisplay
			if display == "" {
				display = "summarized"
			}
			if compat.forceAdaptiveThinking {
				thinking := map[string]any{"type": "adaptive", "display": display}
				params["thinking"] = thinking
				if opts.Effort != "" {
					params["output_config"] = map[string]any{"effort": opts.Effort}
				}
			} else {
				budget := opts.ThinkingBudgetTokens
				if budget == 0 {
					budget = 1024
				}
				params["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget, "display": display}
			}
		} else if off, present := model.ThinkingLevelMap["off"]; !present || off != nil {
			params["thinking"] = map[string]any{"type": "disabled"}
		}
	}

	if opts != nil && opts.Metadata != nil {
		if uid, ok := opts.Metadata["user_id"].(string); ok {
			params["metadata"] = map[string]any{"user_id": uid}
		}
	}

	if opts != nil && opts.ToolChoice != nil {
		switch tc := opts.ToolChoice.(type) {
		case string:
			params["tool_choice"] = map[string]any{"type": tc}
		default:
			params["tool_choice"] = tc
		}
	}

	return params
}

func convertAnthropicTools(tools []ai.Tool, oauth, eager bool, cc *cacheControl) []map[string]any {
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		name := t.Name
		if oauth {
			name = toClaudeCodeName(name)
		}
		props := map[string]any{}
		var required []string
		if t.Parameters != nil {
			if raw, err := json.Marshal(t.Parameters); err == nil {
				var sch struct {
					Properties json.RawMessage `json:"properties"`
					Required   []string        `json:"required"`
				}
				_ = json.Unmarshal(raw, &sch)
				if len(sch.Properties) > 0 {
					_ = json.Unmarshal(sch.Properties, &props)
				}
				required = sch.Required
			}
		}
		if required == nil {
			required = []string{}
		}
		tool := map[string]any{
			"name":        name,
			"description": t.Description,
			"input_schema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		}
		if eager {
			tool["eager_input_streaming"] = true
		}
		if cc != nil && i == len(tools)-1 {
			tool["cache_control"] = cc
		}
		out[i] = tool
	}
	return out
}

func convertAnthropicMessages(messages []ai.Message, model *ai.Model, oauth bool, cc *cacheControl, allowEmptySig bool) []map[string]any {
	transformed := transformMessages(messages, model, normalizeToolCallID)
	var params []map[string]any

	for i := 0; i < len(transformed); i++ {
		m := transformed[i]
		if um, ok := asUserMsg(m); ok {
			blocks := convertUserBlocks(um.Content)
			if len(blocks) == 0 {
				continue
			}
			params = append(params, map[string]any{"role": "user", "content": blocks})
		} else if am, ok := asAssistantMsg(m); ok {
			blocks := convertAssistantBlocks(am, oauth, allowEmptySig)
			if len(blocks) == 0 {
				continue
			}
			params = append(params, map[string]any{"role": "assistant", "content": blocks})
		} else if tr, ok := asToolResultMsg(m); ok {
			toolResults := []any{toolResultBlock(tr)}
			j := i + 1
			for j < len(transformed) {
				next, ok := asToolResultMsg(transformed[j])
				if !ok {
					break
				}
				toolResults = append(toolResults, toolResultBlock(next))
				j++
			}
			i = j - 1
			params = append(params, map[string]any{"role": "user", "content": toolResults})
		}
	}

	// Cache the conversation history by marking the last user block.
	if cc != nil && len(params) > 0 {
		last := params[len(params)-1]
		if last["role"] == "user" {
			if content, ok := last["content"].([]any); ok && len(content) > 0 {
				if blk, ok := content[len(content)-1].(map[string]any); ok {
					t, _ := blk["type"].(string)
					if t == "text" || t == "image" || t == "tool_result" {
						blk["cache_control"] = cc
					}
				}
			}
		}
	}
	return params
}

func convertUserBlocks(content ai.ContentList) []any {
	var blocks []any
	for _, b := range content {
		switch v := b.(type) {
		case ai.TextContent:
			if strings.TrimSpace(v.Text) == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(v.Text)})
		case ai.ImageContent:
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": v.MimeType, "data": v.Data,
				},
			})
		}
	}
	return blocks
}

func convertAssistantBlocks(am *ai.AssistantMessage, oauth, allowEmptySig bool) []any {
	var blocks []any
	for _, b := range am.Content {
		switch v := b.(type) {
		case ai.TextContent:
			if strings.TrimSpace(v.Text) == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(v.Text)})
		case ai.ThinkingContent:
			if v.Redacted {
				blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": v.ThinkingSignature})
				continue
			}
			if strings.TrimSpace(v.Thinking) == "" {
				continue
			}
			if strings.TrimSpace(v.ThinkingSignature) == "" {
				if allowEmptySig {
					blocks = append(blocks, map[string]any{"type": "thinking", "thinking": sanitizeSurrogates(v.Thinking), "signature": ""})
				} else {
					blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(v.Thinking)})
				}
			} else {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": sanitizeSurrogates(v.Thinking), "signature": v.ThinkingSignature})
			}
		case ai.ToolCall:
			name := v.Name
			if oauth {
				name = toClaudeCodeName(name)
			}
			args := v.Arguments
			if args == nil {
				args = map[string]any{}
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": v.ID, "name": name, "input": args})
		}
	}
	return blocks
}

func toolResultBlock(tr ai.ToolResultMessage) map[string]any {
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": tr.ToolCallID,
		"content":     convertContentBlocks(tr.Content),
		"is_error":    tr.IsError,
	}
}

// convertContentBlocks returns either a concatenated string (text-only) or a
// content-block array (with images).
func convertContentBlocks(content ai.ContentList) any {
	hasImages := false
	for _, c := range content {
		if _, ok := c.(ai.ImageContent); ok {
			hasImages = true
			break
		}
	}
	if !hasImages {
		var texts []string
		for _, c := range content {
			if tc, ok := c.(ai.TextContent); ok {
				texts = append(texts, tc.Text)
			}
		}
		return sanitizeSurrogates(strings.Join(texts, "\n"))
	}
	var blocks []any
	hasText := false
	for _, c := range content {
		switch v := c.(type) {
		case ai.TextContent:
			hasText = true
			blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(v.Text)})
		case ai.ImageContent:
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": v.MimeType, "data": v.Data}})
		}
	}
	if !hasText {
		blocks = append([]any{map[string]any{"type": "text", "text": "(see attached image)"}}, blocks...)
	}
	return blocks
}

func applyAnthropicHeaders(r *http.Request, model *ai.Model, opts *AnthropicOptions, oauth bool, apiKey string, hasTools bool, messages []ai.Message) {
	r.Header.Set("content-type", "application/json")
	r.Header.Set("accept", "application/json")
	r.Header.Set("anthropic-version", anthropicVersion)
	r.Header.Set("anthropic-dangerous-direct-browser-access", "true")

	compat := getAnthropicCompat(model)
	var betas []string
	if hasTools && !compat.supportsEagerToolInputStreaming {
		betas = append(betas, fineGrainedToolStreamBeta)
	}
	interleaved := true
	if opts.InterleavedThinking != nil {
		interleaved = *opts.InterleavedThinking
	}
	if interleaved && !compat.forceAdaptiveThinking {
		betas = append(betas, interleavedThinkingBeta)
	}

	// Branch order mirrors pi createClient (anthropic.ts:802,826,848,870):
	// cloudflare-ai-gateway, then github-copilot, then the OAuth sniff, then
	// plain api-key auth.
	switch {
	case model.Provider == "cloudflare-ai-gateway":
		// pi: cf-aig-authorization carries the key; x-api-key and
		// Authorization are explicitly nulled (anthropic.ts:812-814).
		r.Header.Set("cf-aig-authorization", "Bearer "+apiKey)
		if len(betas) > 0 {
			r.Header.Set("anthropic-beta", strings.Join(betas, ","))
		}
	case model.Provider == "github-copilot":
		r.Header.Set("authorization", "Bearer "+apiKey)
		if len(betas) > 0 {
			r.Header.Set("anthropic-beta", strings.Join(betas, ","))
		}
	case oauth:
		r.Header.Set("authorization", "Bearer "+apiKey)
		r.Header.Set("user-agent", "claude-cli/"+claudeCodeVersion)
		r.Header.Set("x-app", "cli")
		oauthBetas := append([]string{"claude-code-20250219", "oauth-2025-04-20"}, betas...)
		r.Header.Set("anthropic-beta", strings.Join(oauthBetas, ","))
	default:
		r.Header.Set("x-api-key", apiKey)
		if len(betas) > 0 {
			r.Header.Set("anthropic-beta", strings.Join(betas, ","))
		}
		// pi anthropic.ts:496-497: cacheSessionId is dropped when the effective
		// cacheRetention is "none", so no session-affinity header is sent.
		if opts.SessionID != "" && compat.sendSessionAffinityHeaders &&
			resolveCacheRetention(opts.CacheRetention) != ai.CacheNone {
			r.Header.Set("x-session-affinity", opts.SessionID)
		}
	}

	for k, v := range model.Headers {
		r.Header.Set(k, v)
	}
	// pi merges copilotDynamicHeaders after model.headers, before options
	// headers (anthropic.ts:832-841).
	if model.Provider == "github-copilot" {
		for k, v := range buildCopilotDynamicHeaders(messages, hasCopilotVisionInput(messages)) {
			r.Header.Set(k, v)
		}
	}
	for k, v := range opts.Headers {
		r.Header.Set(k, v)
	}
}

func mapAnthropicStopReason(reason string) (ai.StopReason, error) {
	switch reason {
	case "end_turn":
		return ai.StopStop, nil
	case "max_tokens":
		return ai.StopLength, nil
	case "tool_use":
		return ai.StopToolUse, nil
	case "refusal", "sensitive":
		return ai.StopError, nil
	case "pause_turn", "stop_sequence":
		return ai.StopStop, nil
	default:
		return "", fmt.Errorf("Unhandled stop reason: %s", reason)
	}
}

// ---- SSE parsing ----

type anthropicUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string         `json:"id"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		Data  string          `json:"data"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
}

var anthropicMessageEvents = map[string]bool{
	"message_start": true, "message_delta": true, "message_stop": true,
	"content_block_start": true, "content_block_delta": true, "content_block_stop": true,
}

// scanSSELines is a bufio.SplitFunc that treats \r, \n, and \r\n all as line
// breaks, mirroring pi's SSE decoder (anthropic.ts consumeLine/nextLineBreakIndex).
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			// A trailing \r might be half of a \r\n pair; wait for more data.
			return 0, nil, nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// iterateAnthropicSSE parses the SSE body and invokes handle for each known event.
func iterateAnthropicSSE(body io.Reader, ctx context.Context, handle func(anthropicStreamEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	scanner.Split(scanSSELines)

	var eventName string
	var dataLines []string

	flush := func() error {
		if eventName == "" && len(dataLines) == 0 {
			return nil
		}
		name := eventName
		data := strings.Join(dataLines, "\n")
		eventName = ""
		dataLines = nil

		if name == "error" {
			return fmt.Errorf("%s", data)
		}
		if !anthropicMessageEvents[name] {
			return nil
		}
		var ev anthropicStreamEvent
		if err := parseJSONWithRepair(data, &ev); err != nil {
			return fmt.Errorf("Could not parse Anthropic SSE event %s: %v; data=%s", name, err, data)
		}
		return handle(ev)
	}

	for scanner.Scan() {
		if ctx != nil && ctx.Err() != nil {
			return fmt.Errorf("Request was aborted")
		}
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		var field, value string
		if idx == -1 {
			field = line
		} else {
			field = line[:idx]
			value = line[idx+1:]
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventName = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func applyUsage(usage *ai.Usage, u anthropicUsage, isStart bool) {
	if isStart {
		usage.Input = derefOr(u.InputTokens, 0)
		usage.Output = derefOr(u.OutputTokens, 0)
		usage.CacheRead = derefOr(u.CacheReadInputTokens, 0)
		usage.CacheWrite = derefOr(u.CacheCreationInputTokens, 0)
	} else {
		if u.InputTokens != nil {
			usage.Input = *u.InputTokens
		}
		if u.OutputTokens != nil {
			usage.Output = *u.OutputTokens
		}
		if u.CacheReadInputTokens != nil {
			usage.CacheRead = *u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens != nil {
			usage.CacheWrite = *u.CacheCreationInputTokens
		}
	}
	usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

func derefOr(p *int, d int) int {
	if p != nil {
		return *p
	}
	return d
}

func flattenHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

// RegisterAnthropic registers the anthropic-messages api provider.
func RegisterAnthropic() {
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIAnthropicMessages,
		Stream: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			aopts := &AnthropicOptions{}
			if opts != nil {
				aopts.StreamOptions = *opts
			}
			return StreamAnthropic(ctx, model, req, aopts)
		},
		StreamSimple: StreamSimpleAnthropic,
	})
}
