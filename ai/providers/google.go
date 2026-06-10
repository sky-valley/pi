package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/sky-valley/pi/ai"
)

const googleDefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// googleToolCallCounter is a monotonic counter for synthesizing unique tool-call
// IDs (pi's module-level toolCallCounter). Guarded by googleToolCallCounterMu so
// concurrent streams don't collide.
var (
	googleToolCallCounter   int
	googleToolCallCounterMu sync.Mutex
)

func nextGoogleToolCallID(name string) string {
	googleToolCallCounterMu.Lock()
	googleToolCallCounter++
	c := googleToolCallCounter
	googleToolCallCounterMu.Unlock()
	return fmt.Sprintf("%s_%d_%d", name, nowMillis(), c)
}

// GoogleOptions are provider-native options for the Gemini stream.
type GoogleOptions struct {
	ai.StreamOptions
	// ThinkingProvided mirrors pi's optional `thinking` object being present at all.
	// When false, no thinkingConfig (enabled or disabled) is emitted.
	ThinkingProvided bool
	ThinkingEnabled  bool
	ThinkingBudget   *int   // -1 dynamic, 0 disable
	ThinkingLevel    string // Gemini 3: MINIMAL|LOW|MEDIUM|HIGH
	ToolChoice       string // auto|none|any
}

// StreamSimpleGoogle maps unified reasoning to GoogleOptions then streams.
func StreamSimpleGoogle(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	g := &GoogleOptions{}
	if opts != nil {
		g.StreamOptions = opts.StreamOptions
	}
	reasoning := ai.ThinkingLevel("")
	if opts != nil {
		reasoning = opts.Reasoning
	}
	if reasoning == "" {
		g.ThinkingProvided = true
		g.ThinkingEnabled = false
		return StreamGoogle(ctx, model, req, g)
	}
	clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(reasoning))
	// pi google.ts:296 only coerces "off" → "high". An "xhigh" that survives
	// clamping falls through both getThinkingLevel and getGoogleBudget's
	// per-family tables, yielding thinkingConfig:{includeThoughts:true} with
	// neither thinkingLevel nor thinkingBudget.
	effort := string(clamped)
	if effort == "off" {
		effort = "high"
	}
	g.ThinkingProvided = true
	g.ThinkingEnabled = true
	if isGemini3(model.ID) || isGemma4(model.ID) {
		g.ThinkingLevel = googleThinkingLevel(effort, model.ID)
	} else {
		var custom *ai.ThinkingBudgets
		if opts != nil {
			custom = opts.ThinkingBudgets
		}
		g.ThinkingBudget = googleBudget(model.ID, effort, custom)
	}
	return StreamGoogle(ctx, model, req, g)
}

var (
	gemini3ProRe   = regexp.MustCompile(`gemini-3(?:\.\d+)?-pro`)
	gemini3FlashRe = regexp.MustCompile(`gemini-3(?:\.\d+)?-flash`)
	gemma4Re       = regexp.MustCompile(`gemma-?4`)
)

func isGemini3Pro(id string) bool   { return gemini3ProRe.MatchString(strings.ToLower(id)) }
func isGemini3Flash(id string) bool { return gemini3FlashRe.MatchString(strings.ToLower(id)) }
func isGemini3(id string) bool {
	return isGemini3Pro(id) || isGemini3Flash(id)
}
func isGemma4(id string) bool { return gemma4Re.MatchString(strings.ToLower(id)) }

// getDisabledThinkingConfig mirrors pi google.ts getDisabledThinkingConfig.
// Gemini 3 Pro cannot fully disable thinking (lowest is LOW); Gemini 3 Flash and
// Gemma 4 use MINIMAL; everything else (Gemini 2.x) disables via thinkingBudget:0.
func getDisabledThinkingConfig(modelID string) map[string]any {
	switch {
	case isGemini3Pro(modelID):
		return map[string]any{"thinkingLevel": "LOW"}
	case isGemini3Flash(modelID):
		return map[string]any{"thinkingLevel": "MINIMAL"}
	case isGemma4(modelID):
		return map[string]any{"thinkingLevel": "MINIMAL"}
	default:
		return map[string]any{"thinkingBudget": 0}
	}
}

// googleThinkingLevel mirrors pi getThinkingLevel (google.ts:430-461). The empty
// string means "no thinkingLevel" (pi returns undefined for an unmatched effort
// such as xhigh).
func googleThinkingLevel(effort, id string) string {
	if isGemini3Pro(id) {
		switch effort {
		case "minimal", "low":
			return "LOW"
		case "medium", "high":
			return "HIGH"
		}
	}
	if isGemma4(id) {
		switch effort {
		case "minimal", "low":
			return "MINIMAL"
		case "medium", "high":
			return "HIGH"
		}
	}
	switch effort {
	case "minimal":
		return "MINIMAL"
	case "low":
		return "LOW"
	case "medium":
		return "MEDIUM"
	case "high":
		return "HIGH"
	}
	return ""
}

// googleBudget mirrors pi getGoogleBudget (google.ts:463-503). A nil return means
// "no thinkingBudget" (pi's per-family tables have no xhigh key, so budgets[effort]
// is undefined); the final default of -1 applies to unmatched model families for
// ANY effort, xhigh included.
func googleBudget(id, effort string, custom *ai.ThinkingBudgets) *int {
	if custom != nil {
		if b := budgetForEffort(custom, effort); b != nil {
			return b
		}
	}
	switch {
	case strings.Contains(id, "2.5-pro"):
		return pick(effort, 128, 2048, 8192, 32768)
	case strings.Contains(id, "2.5-flash-lite"):
		return pick(effort, 512, 2048, 8192, 24576)
	case strings.Contains(id, "2.5-flash"):
		return pick(effort, 128, 2048, 8192, 24576)
	default:
		v := -1
		return &v
	}
}

func budgetForEffort(b *ai.ThinkingBudgets, effort string) *int {
	switch effort {
	case "minimal":
		return b.Minimal
	case "low":
		return b.Low
	case "medium":
		return b.Medium
	case "high":
		return b.High
	}
	return nil
}

func pick(effort string, minimal, low, medium, high int) *int {
	switch effort {
	case "minimal":
		return &minimal
	case "low":
		return &low
	case "medium":
		return &medium
	case "high":
		return &high
	}
	return nil
}

func requiresToolCallID(modelID string) bool {
	return strings.HasPrefix(modelID, "claude-") || strings.HasPrefix(modelID, "gpt-oss-")
}

// base64SignaturePattern matches the base64 alphabet pi requires for thought
// signatures (TYPE_BYTES). Signatures must also be a multiple of 4 in length.
var base64SignaturePattern = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

func isValidThoughtSignature(sig string) bool {
	if sig == "" {
		return false
	}
	if len(sig)%4 != 0 {
		return false
	}
	return base64SignaturePattern.MatchString(sig)
}

// resolveThoughtSignature only keeps a signature from the same provider/model with
// valid base64 (pi google-shared resolveThoughtSignature).
func resolveThoughtSignature(isSameProviderAndModel bool, sig string) string {
	if isSameProviderAndModel && isValidThoughtSignature(sig) {
		return sig
	}
	return ""
}

// getGeminiMajorVersion extracts the leading Gemini major version (pi google-shared).
func getGeminiMajorVersion(modelID string) (int, bool) {
	m := geminiMajorRe.FindStringSubmatch(strings.ToLower(modelID))
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n, true
}

// supportsMultimodalFunctionResponse reports whether the model nests tool-result
// images inside functionResponse.parts (Gemini ≥ 3); others need a separate user
// image turn. Non-Gemini models default to true (pi google-shared).
func supportsMultimodalFunctionResponse(modelID string) bool {
	if v, ok := getGeminiMajorVersion(modelID); ok {
		return v >= 3
	}
	return true
}

var geminiMajorRe = regexp.MustCompile(`^gemini(?:-live)?-(\d+)`)

func modelSupportsImageInput(model *ai.Model) bool {
	for _, in := range model.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// StreamGoogle streams from the Gemini generateContent SSE endpoint.
func StreamGoogle(ctx context.Context, model *ai.Model, req ai.Context, opts *GoogleOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &GoogleOptions{}
	}

	go func() {
		output := &ai.AssistantMessage{
			Content: ai.ContentList{}, Api: ai.APIGoogleGenerativeAI, Provider: model.Provider, Model: model.ID,
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

		body := buildGoogleParams(model, req, opts)
		if opts.OnPayload != nil {
			next, perr := opts.OnPayload(body, model)
			if perr != nil {
				// pi: a throw from onPayload propagates and fails the stream.
				fail(perr)
				return
			}
			if m, ok := next.(map[string]any); ok && m != nil {
				body = m
			}
		}
		payload, _ := json.Marshal(body)

		baseURL := model.BaseURL
		if baseURL == "" {
			baseURL = googleDefaultBaseURL
		}
		url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", strings.TrimRight(baseURL, "/"), model.ID)
		build := func() (*http.Request, error) {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			r.Header.Set("content-type", "application/json")
			r.Header.Set("x-goog-api-key", opts.APIKey)
			for k, v := range model.Headers {
				r.Header.Set(k, v)
			}
			for k, v := range opts.Headers {
				r.Header.Set(k, v)
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
			fail(formatProviderError("Google", resp.StatusCode, data))
			return
		}

		stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: output.Clone()})

		var builders []*blockBuilder
		var current *blockBuilder // text or thinking
		// textSigs / toolCallSigs carry per-block thoughtSignatures that the shared
		// blockBuilder.toContent() does not model (text textSignature, toolCall
		// thoughtSignature), keyed by builder index.
		textSigs := map[int]string{}
		toolCallSigs := map[int]string{}
		materialize := func() {
			content := make(ai.ContentList, len(builders))
			for i, b := range builders {
				c := b.toContent()
				if sig, ok := textSigs[i]; ok && sig != "" {
					if tc, ok := c.(ai.TextContent); ok {
						tc.TextSignature = sig
						c = tc
					}
				}
				if sig, ok := toolCallSigs[i]; ok && sig != "" {
					if tc, ok := c.(ai.ToolCall); ok {
						tc.ThoughtSignature = sig
						c = tc
					}
				}
				content[i] = c
			}
			output.Content = content
		}
		endCurrent := func() {
			if current == nil {
				return
			}
			idx := len(builders) - 1
			if current.kind == "text" {
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: idx, Content: current.text.String(), Partial: output.Clone()})
			} else {
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: idx, Content: current.thinking.String(), Partial: output.Clone()})
			}
			current = nil
		}

		err = iterateGoogleSSE(resp.Body, ctx, func(chunk googleChunk) error {
			if chunk.ResponseID != "" && output.ResponseID == "" {
				output.ResponseID = chunk.ResponseID
			}
			if len(chunk.Candidates) > 0 {
				cand := chunk.Candidates[0]
				for _, part := range cand.Content.Parts {
					// pi runs INDEPENDENT checks (google.ts:97,158): `text !== undefined`
					// first, then `functionCall` — a part carrying both processes both;
					// a part with neither (signature-only, inlineData-only) produces
					// nothing at all.
					if part.Text != nil {
						text := *part.Text
						isThinking := part.Thought
						want := "text"
						if isThinking {
							want = "thinking"
						}
						if current == nil || current.kind != want {
							endCurrent()
							current = &blockBuilder{kind: want}
							builders = append(builders, current)
							idx := len(builders) - 1
							materialize()
							if isThinking {
								stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingStart, ContentIndex: idx, Partial: output.Clone()})
							} else {
								stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextStart, ContentIndex: idx, Partial: output.Clone()})
							}
						}
						idx := len(builders) - 1
						if isThinking {
							current.thinking.WriteString(text)
							if part.ThoughtSignature != "" {
								current.thinkingSig = part.ThoughtSignature
							}
							materialize()
							stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: idx, Delta: text, Partial: output.Clone()})
						} else {
							current.text.WriteString(text)
							// A thoughtSignature can appear on a text part (pi: textSignature via
							// retainThoughtSignature — keep last non-empty for the block).
							if part.ThoughtSignature != "" {
								textSigs[idx] = part.ThoughtSignature
							}
							materialize()
							stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: idx, Delta: text, Partial: output.Clone()})
						}
					}
					if part.FunctionCall != nil {
						endCurrent()
						// Regenerate the ID when it is empty OR a duplicate of one already
						// seen in this response (pi google.ts: needsNewId).
						providedID := part.FunctionCall.ID
						needsNewID := providedID == ""
						if !needsNewID {
							for _, b := range builders {
								if b.kind == "toolCall" && b.toolID == providedID {
									needsNewID = true
									break
								}
							}
						}
						id := providedID
						if needsNewID {
							id = nextGoogleToolCallID(part.FunctionCall.Name)
						}
						args := part.FunctionCall.Args
						if args == nil {
							args = map[string]any{}
						}
						b := &blockBuilder{kind: "toolCall", toolID: id, toolName: part.FunctionCall.Name, args: args}
						builders = append(builders, b)
						idx := len(builders) - 1
						// pi sets thoughtSignature on the ToolCall object BEFORE pushing
						// toolcall_start (google.ts:186-195), so partials already carry it.
						if part.ThoughtSignature != "" {
							toolCallSigs[idx] = part.ThoughtSignature
						}
						materialize()
						stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallStart, ContentIndex: idx, Partial: output.Clone()})
						argsJSON, _ := json.Marshal(args)
						stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: idx, Delta: string(argsJSON), Partial: output.Clone()})
						tc := b.toContent().(ai.ToolCall)
						if part.ThoughtSignature != "" {
							tc.ThoughtSignature = part.ThoughtSignature
						}
						stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: idx, ToolCall: &tc, Partial: output.Clone()})
					}
				}
				if cand.FinishReason != "" {
					reason, rerr := mapGoogleStopReason(cand.FinishReason)
					if rerr != nil {
						return rerr
					}
					output.StopReason = reason
					for _, b := range builders {
						if b.kind == "toolCall" {
							output.StopReason = ai.StopToolUse
							break
						}
					}
				}
			}
			if chunk.UsageMetadata != nil {
				u := chunk.UsageMetadata
				output.Usage = ai.Usage{
					Input:       u.PromptTokenCount - u.CachedContentTokenCount,
					Output:      u.CandidatesTokenCount + u.ThoughtsTokenCount,
					CacheRead:   u.CachedContentTokenCount,
					CacheWrite:  0,
					TotalTokens: u.TotalTokenCount,
				}
				ai.CalculateCost(model, &output.Usage)
			}
			return nil
		})

		endCurrent()
		if err != nil {
			fail(err)
			return
		}
		if ctx != nil && ctx.Err() != nil {
			fail(fmt.Errorf("Request was aborted"))
			return
		}
		// pi: a terminal error/aborted stopReason surfaces as a thrown error.
		if output.StopReason == ai.StopError || output.StopReason == ai.StopAborted {
			fail(fmt.Errorf("An unknown error occurred"))
			return
		}
		materialize()
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: output.StopReason, Message: output})
		stream.End()
	}()

	return stream
}

// buildGoogleParams builds the REST body the @google/genai SDK actually sends to
// generativelanguage.googleapis.com. The SDK lifts systemInstruction / tools /
// toolConfig to the top level (alongside contents) and keeps generation params
// (temperature, maxOutputTokens, thinkingConfig) under generationConfig.
func buildGoogleParams(model *ai.Model, req ai.Context, opts *GoogleOptions) map[string]any {
	params := map[string]any{
		"contents": googleContents(model, req),
	}

	// generationConfig holds only generation params (temperature, maxOutputTokens,
	// thinkingConfig). SDK: generateContentConfigToMldev writes these to toObject,
	// which becomes generationConfig.
	gen := map[string]any{}
	if opts.Temperature != nil {
		gen["temperature"] = *opts.Temperature
	}
	if opts.MaxTokens != nil {
		gen["maxOutputTokens"] = *opts.MaxTokens
	}

	// thinkingConfig lives under generationConfig per the SDK.
	if model.Reasoning && opts.ThinkingProvided && opts.ThinkingEnabled {
		tc := map[string]any{"includeThoughts": true}
		if opts.ThinkingLevel != "" {
			tc["thinkingLevel"] = opts.ThinkingLevel
		} else if opts.ThinkingBudget != nil {
			tc["thinkingBudget"] = *opts.ThinkingBudget
		}
		gen["thinkingConfig"] = tc
	} else if model.Reasoning && opts.ThinkingProvided && !opts.ThinkingEnabled {
		gen["thinkingConfig"] = getDisabledThinkingConfig(model.ID)
	}

	// The genai SDK always sends generationConfig (unconditional setValueByPath),
	// even as an empty {} when no generation params are set.
	params["generationConfig"] = gen

	// systemInstruction / tools / toolConfig are lifted to the top level by the SDK.
	if req.SystemPrompt != "" {
		params["systemInstruction"] = map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": sanitizeSurrogates(req.SystemPrompt)}},
		}
	}
	if len(req.Tools) > 0 {
		params["tools"] = googleTools(req.Tools, useParameters(model.ID))
		if opts.ToolChoice != "" {
			params["toolConfig"] = map[string]any{
				"functionCallingConfig": map[string]any{"mode": mapToolChoice(opts.ToolChoice)},
			}
		}
	}
	return params
}

// mapToolChoice mirrors pi google-shared mapToolChoice (auto/none/any → upper).
func mapToolChoice(choice string) string {
	switch choice {
	case "auto":
		return "AUTO"
	case "none":
		return "NONE"
	case "any":
		return "ANY"
	default:
		return "AUTO"
	}
}

// useParameters selects the legacy OpenAPI `parameters` field (vs full-JSON-Schema
// `parametersJsonSchema`). pi's convertTools (google-shared.ts) defaults
// useParameters=false, and BOTH runtime callers — google.ts:356 and
// google-vertex.ts:445 — invoke `convertTools(context.tools)` with no second
// argument. So the google-generative-ai / google-vertex providers ALWAYS emit
// `parametersJsonSchema`; the `parameters` branch is a library affordance for
// out-of-tree callers (Cloud Code Assist) that pi never exercises here. Returning
// false unconditionally pins pi's actual runtime field choice for all models,
// including Claude-via-Google.
func useParameters(modelID string) bool {
	return false
}

func googleContents(model *ai.Model, req ai.Context) []any {
	normalizeID := func(id string) string {
		if !requiresToolCallID(model.ID) {
			return id
		}
		return normalizeToolCallID(id)
	}
	transformed := transformMessages(req.Messages, model, normalizeID)
	var contents []any
	for _, m := range transformed {
		if um, ok := asUserMsg(m); ok {
			var parts []any
			for _, c := range um.Content {
				switch v := c.(type) {
				case ai.TextContent:
					parts = append(parts, map[string]any{"text": sanitizeSurrogates(v.Text)})
				case ai.ImageContent:
					parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": v.MimeType, "data": v.Data}})
				}
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "user", "parts": parts})
		} else if am, ok := asAssistantMsg(m); ok {
			isSame := am.Provider == model.Provider && am.Model == model.ID
			var parts []any
			for _, c := range am.Content {
				switch v := c.(type) {
				case ai.TextContent:
					if strings.TrimSpace(v.Text) == "" {
						continue
					}
					p := map[string]any{"text": sanitizeSurrogates(v.Text)}
					// thoughtSignature can ride on a text part for context replay
					// (pi: textSignature). Only keep same-model + valid base64.
					if sig := resolveThoughtSignature(isSame, v.TextSignature); sig != "" {
						p["thoughtSignature"] = sig
					}
					parts = append(parts, p)
				case ai.ThinkingContent:
					if strings.TrimSpace(v.Thinking) == "" {
						continue
					}
					if isSame {
						p := map[string]any{"thought": true, "text": sanitizeSurrogates(v.Thinking)}
						if sig := resolveThoughtSignature(isSame, v.ThinkingSignature); sig != "" {
							p["thoughtSignature"] = sig
						}
						parts = append(parts, p)
					} else {
						parts = append(parts, map[string]any{"text": sanitizeSurrogates(v.Thinking)})
					}
				case ai.ToolCall:
					fc := map[string]any{"name": v.Name, "args": orEmptyMap(v.Arguments)}
					if requiresToolCallID(model.ID) {
						fc["id"] = v.ID
					}
					part := map[string]any{"functionCall": fc}
					if sig := resolveThoughtSignature(isSame, v.ThoughtSignature); sig != "" {
						part["thoughtSignature"] = sig
					}
					parts = append(parts, part)
				}
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
		} else if tr, ok := asToolResultMsg(m); ok {
			var texts []string
			var imageParts []any
			modelTakesImages := modelSupportsImageInput(model)
			for _, c := range tr.Content {
				switch cv := c.(type) {
				case ai.TextContent:
					texts = append(texts, cv.Text)
				case ai.ImageContent:
					if modelTakesImages {
						imageParts = append(imageParts, map[string]any{
							"inlineData": map[string]any{"mimeType": cv.MimeType, "data": cv.Data},
						})
					}
				}
			}
			textResult := strings.Join(texts, "\n")
			hasText := len(textResult) > 0
			hasImages := len(imageParts) > 0

			// responseValue: text if present, else placeholder for image-only, else "".
			responseValue := ""
			if hasText {
				responseValue = sanitizeSurrogates(textResult)
			} else if hasImages {
				responseValue = "(see attached image)"
			}

			respKey := "output"
			if tr.IsError {
				respKey = "error"
			}
			nested := supportsMultimodalFunctionResponse(model.ID)
			fr := map[string]any{"name": tr.ToolName, "response": map[string]any{respKey: responseValue}}
			// Gemini ≥ 3 nests images inside functionResponse.parts.
			if hasImages && nested {
				fr["parts"] = imageParts
			}
			if requiresToolCallID(model.ID) {
				fr["id"] = tr.ToolCallID
			}
			part := map[string]any{"functionResponse": fr}
			// Merge consecutive function responses into the last user turn
			// (Cloud Code Assist requires a single user turn).
			merged := false
			if n := len(contents); n > 0 {
				if last, ok := contents[n-1].(map[string]any); ok && last["role"] == "user" {
					if parts, ok := last["parts"].([]any); ok && hasFunctionResponse(parts) {
						last["parts"] = append(parts, part)
						merged = true
					}
				}
			}
			if !merged {
				contents = append(contents, map[string]any{"role": "user", "parts": []any{part}})
			}

			// Gemini < 3 / Claude-via-Google: images go in a separate user turn.
			if hasImages && !nested {
				imgTurnParts := append([]any{map[string]any{"text": "Tool result image:"}}, imageParts...)
				contents = append(contents, map[string]any{"role": "user", "parts": imgTurnParts})
			}
		}
	}
	return contents
}

func hasFunctionResponse(parts []any) bool {
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if _, has := m["functionResponse"]; has {
				return true
			}
		}
	}
	return false
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// jsonSchemaMetaDeclarations mirrors pi google-shared JSON_SCHEMA_META_DECLARATIONS.
var jsonSchemaMetaDeclarations = map[string]bool{
	"$schema":        true,
	"$id":            true,
	"$anchor":        true,
	"$dynamicAnchor": true,
	"$vocabulary":    true,
	"$comment":       true,
	"$defs":          true,
	"definitions":    true, // pre-draft-2019-09 equivalent of $defs
}

// sanitizeForOpenApi recursively strips JSON Schema meta-declarations from a schema
// so it can be sent as an OpenAPI 3.0.3 schema (pi google-shared sanitizeForOpenApi).
func sanitizeForOpenApi(schema any) any {
	switch v := schema.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, value := range v {
			if jsonSchemaMetaDeclarations[key] {
				continue
			}
			result[key] = sanitizeForOpenApi(value)
		}
		return result
	default:
		// Arrays and scalars pass through unchanged. pi only recurses into plain
		// objects; arrays are returned as-is (Array.isArray short-circuit).
		return schema
	}
}

// schemaToGeneric marshals a Schema to its JSON-Schema map form so it can be
// sanitized key-by-key like pi (which operates on plain objects).
func schemaToGeneric(s *ai.Schema) any {
	raw, err := json.Marshal(s)
	if err != nil {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// googleTools converts tools to Gemini functionDeclarations. When useParameters is
// true it emits the legacy OpenAPI `parameters` field (sanitized of meta-keys),
// otherwise the full-JSON-Schema `parametersJsonSchema`. (pi convertTools.)
func googleTools(tools []ai.Tool, useParameters bool) []any {
	if len(tools) == 0 {
		return nil
	}
	var decls []any
	for _, t := range tools {
		decl := map[string]any{"name": t.Name, "description": t.Description}
		if useParameters {
			if t.Parameters != nil {
				decl["parameters"] = sanitizeForOpenApi(schemaToGeneric(t.Parameters))
			}
		} else if t.Parameters != nil {
			decl["parametersJsonSchema"] = t.Parameters
		}
		decls = append(decls, decl)
	}
	return []any{map[string]any{"functionDeclarations": decls}}
}

// mapGoogleStopReason maps a Gemini FinishReason to our StopReason, returning a
// non-nil error only for a truly-unknown reason (pi mapStopReason throws via the
// exhaustive-never check). Known safety/recitation/malformed reasons map to error
// without throwing — pi later surfaces them as "An unknown error occurred".
func mapGoogleStopReason(reason string) (ai.StopReason, error) {
	switch reason {
	case "STOP":
		return ai.StopStop, nil
	case "MAX_TOKENS":
		return ai.StopLength, nil
	case "BLOCKLIST",
		"PROHIBITED_CONTENT",
		"SPII",
		"SAFETY",
		"IMAGE_SAFETY",
		"IMAGE_PROHIBITED_CONTENT",
		"IMAGE_RECITATION",
		"IMAGE_OTHER",
		"RECITATION",
		"FINISH_REASON_UNSPECIFIED",
		"OTHER",
		"LANGUAGE",
		"MALFORMED_FUNCTION_CALL",
		"UNEXPECTED_TOOL_CALL",
		"NO_IMAGE":
		return ai.StopError, nil
	default:
		return ai.StopError, fmt.Errorf("Unhandled stop reason: %s", reason)
	}
}

// ---- SSE chunk types ----

type googleChunkError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

type googleChunk struct {
	ResponseID string `json:"responseId"`
	Candidates []struct {
		Content struct {
			Parts []googlePart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error *googleChunkError `json:"error"`
}

type googlePart struct {
	// Text is a pointer so presence ("" included) is distinguishable from
	// absence, mirroring pi's `part.text !== undefined` check (google.ts:97).
	Text             *string `json:"text"`
	Thought          bool    `json:"thought"`
	ThoughtSignature string  `json:"thoughtSignature"`
	FunctionCall     *struct {
		ID   string         `json:"id"`
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"functionCall"`
}

// googleAPIError formats a {code,message,status} error payload the way the
// @google/genai SDK does (api_client processStreamResponse → ApiError:
// "got status: ${status}. ${JSON.stringify(chunkJson)}"). It returns nil when
// the code is outside the SDK's 400..599 throw range.
func googleAPIError(e *googleChunkError, rawChunk string) error {
	if e == nil || e.Code < 400 || e.Code >= 600 {
		return nil
	}
	return fmt.Errorf("got status: %s. %s", e.Status, rawChunk)
}

// iterateGoogleSSE consumes the alt=sse stream the way the @google/genai SDK
// does (processStreamResponse): events are split on \n\n, \r\r, or \r\n\r\n;
// only "data:"-prefixed events are decoded; a chunk carrying an error payload
// fails the stream like the SDK's ApiError; and a trailing unconsumed segment
// fails with the SDK's "Incomplete JSON segment at the end".
func iterateGoogleSSE(body io.Reader, ctx context.Context, handle func(googleChunk) error) error {
	delimiters := []string{"\n\n", "\r\r", "\r\n\r\n"}
	buf := make([]byte, 32*1024)
	var pending string

	processEvent := func(event string) error {
		trimmed := strings.TrimSpace(event)
		if !strings.HasPrefix(trimmed, "data:") {
			// The SDK detects bare (non-SSE) JSON error chunks before buffering;
			// a 4xx/5xx error payload fails the stream as an ApiError.
			if trimmed != "" {
				var chunk googleChunk
				if json.Unmarshal([]byte(trimmed), &chunk) == nil {
					if err := googleAPIError(chunk.Error, trimmed); err != nil {
						return err
					}
				}
			}
			return nil
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			return nil
		}
		var chunk googleChunk
		if err := parseJSONWithRepair(data, &chunk); err != nil {
			return nil
		}
		if err := googleAPIError(chunk.Error, data); err != nil {
			return err
		}
		return handle(chunk)
	}

	for {
		if ctx != nil && ctx.Err() != nil {
			return fmt.Errorf("Request was aborted")
		}
		n, readErr := body.Read(buf)
		if n > 0 {
			pending += string(buf[:n])
			for {
				// Earliest delimiter wins (SDK keeps the smallest index).
				delimIdx, delimLen := -1, 0
				for _, d := range delimiters {
					if i := strings.Index(pending, d); i != -1 && (delimIdx == -1 || i < delimIdx) {
						delimIdx, delimLen = i, len(d)
					}
				}
				if delimIdx == -1 {
					break
				}
				event := pending[:delimIdx]
				pending = pending[delimIdx+delimLen:]
				if err := processEvent(event); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	if strings.TrimSpace(pending) != "" {
		trimmed := strings.TrimSpace(pending)
		// A bare JSON error payload arriving without a trailing delimiter still
		// fails as an ApiError (the SDK checks chunks before buffering them).
		var chunk googleChunk
		if json.Unmarshal([]byte(trimmed), &chunk) == nil {
			if err := googleAPIError(chunk.Error, trimmed); err != nil {
				return err
			}
		}
		return fmt.Errorf("Incomplete JSON segment at the end")
	}
	return nil
}

// RegisterGoogle registers the google-generative-ai api provider.
func RegisterGoogle() {
	ai.RegisterApiProvider(ai.ApiProvider{
		Api: ai.APIGoogleGenerativeAI,
		Stream: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			g := &GoogleOptions{}
			if opts != nil {
				g.StreamOptions = *opts
			}
			return StreamGoogle(ctx, model, req, g)
		},
		StreamSimple: StreamSimpleGoogle,
	})
}
