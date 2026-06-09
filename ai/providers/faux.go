// Package providers contains concrete ai.ApiProvider implementations (faux for
// testing, plus real LLM providers) registered with the ai registry.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/ai"
)

const (
	fauxDefaultAPI       = "faux"
	fauxDefaultProvider  = "faux"
	fauxDefaultModelID   = "faux-1"
	fauxDefaultModelName = "Faux Model"
	fauxDefaultBaseURL   = "http://localhost:0"
	fauxDefaultMinToken  = 3
	fauxDefaultMaxToken  = 5
)

var fauxDefaultUsage = ai.Usage{}

// FauxModelDefinition describes a model exposed by a faux provider.
type FauxModelDefinition struct {
	ID            string
	Name          string
	Reasoning     bool
	Input         []string
	Cost          *ai.ModelCost
	ContextWindow int
	MaxTokens     int
}

// FauxText builds a text content block.
func FauxText(text string) ai.TextContent { return ai.TextContent{Text: text} }

// FauxThinking builds a thinking content block.
func FauxThinking(thinking string) ai.ThinkingContent { return ai.ThinkingContent{Thinking: thinking} }

// FauxToolCall builds a tool-call content block.
func FauxToolCall(name string, args map[string]any, id ...string) ai.ToolCall {
	tcID := ""
	if len(id) > 0 {
		tcID = id[0]
	}
	if tcID == "" {
		tcID = randomID("tool")
	}
	return ai.ToolCall{ID: tcID, Name: name, Arguments: args}
}

// FauxAssistantMessage builds an assistant message from content blocks.
func FauxAssistantMessage(content ai.ContentList, stopReason ai.StopReason) *ai.AssistantMessage {
	if stopReason == "" {
		stopReason = ai.StopStop
	}
	return &ai.AssistantMessage{
		Content:    content,
		Api:        fauxDefaultAPI,
		Provider:   fauxDefaultProvider,
		Model:      fauxDefaultModelID,
		Usage:      fauxDefaultUsage,
		StopReason: stopReason,
		Timestamp:  nowMillis(),
	}
}

// FauxResponseStep produces the assistant message for one stream call.
type FauxResponseStep func(req ai.Context, opts *ai.SimpleStreamOptions, state *FauxState, model *ai.Model) *ai.AssistantMessage

// FauxStatic wraps a fixed assistant message as a response step.
func FauxStatic(msg *ai.AssistantMessage) FauxResponseStep {
	return func(ai.Context, *ai.SimpleStreamOptions, *FauxState, *ai.Model) *ai.AssistantMessage {
		return msg
	}
}

// FauxState exposes mutable per-registration state.
type FauxState struct {
	CallCount int
}

// RegisterFauxProviderOptions configures a faux provider registration.
type RegisterFauxProviderOptions struct {
	Api             string
	Provider        string
	Models          []FauxModelDefinition
	TokensPerSecond float64
	MinTokenSize    int
	MaxTokenSize    int
}

// FauxProviderRegistration is the handle returned by RegisterFauxProvider.
type FauxProviderRegistration struct {
	Api    string
	Models []*ai.Model
	State  *FauxState

	mu           sync.Mutex
	pending      []FauxResponseStep
	sourceID     string
	minTokenSize int
	maxTokenSize int
	tps          float64
	promptCache  map[string]string
	provider     string
}

// GetModel returns the model with the given id, or the first model if id is empty.
func (r *FauxProviderRegistration) GetModel(id ...string) *ai.Model {
	if len(id) == 0 || id[0] == "" {
		return r.Models[0]
	}
	for _, m := range r.Models {
		if m.ID == id[0] {
			return m
		}
	}
	return nil
}

// SetResponses replaces the queued responses.
func (r *FauxProviderRegistration) SetResponses(responses []FauxResponseStep) {
	r.mu.Lock()
	r.pending = append([]FauxResponseStep(nil), responses...)
	r.mu.Unlock()
}

// AppendResponses appends to the queued responses.
func (r *FauxProviderRegistration) AppendResponses(responses []FauxResponseStep) {
	r.mu.Lock()
	r.pending = append(r.pending, responses...)
	r.mu.Unlock()
}

// PendingResponseCount returns the number of queued responses.
func (r *FauxProviderRegistration) PendingResponseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// Unregister removes the faux provider from the registry.
func (r *FauxProviderRegistration) Unregister() {
	ai.UnregisterApiProviders(r.sourceID)
}

// RegisterFauxProvider registers a deterministic-protocol faux provider used for
// tests, returning a handle to script responses.
func RegisterFauxProvider(options RegisterFauxProviderOptions) *FauxProviderRegistration {
	api := options.Api
	if api == "" {
		api = randomID(fauxDefaultAPI)
	}
	provider := options.Provider
	if provider == "" {
		provider = fauxDefaultProvider
	}
	minT := options.MinTokenSize
	if minT == 0 {
		minT = fauxDefaultMinToken
	}
	maxT := options.MaxTokenSize
	if maxT == 0 {
		maxT = fauxDefaultMaxToken
	}
	if minT < 1 {
		minT = 1
	}
	if maxT < minT {
		maxT = minT
	}

	defs := options.Models
	if len(defs) == 0 {
		defs = []FauxModelDefinition{{
			ID: fauxDefaultModelID, Name: fauxDefaultModelName,
			Input: []string{"text", "image"}, ContextWindow: 128000, MaxTokens: 16384,
		}}
	}
	models := make([]*ai.Model, len(defs))
	for i, d := range defs {
		name := d.Name
		if name == "" {
			name = d.ID
		}
		input := d.Input
		if input == nil {
			input = []string{"text", "image"}
		}
		cost := ai.ModelCost{}
		if d.Cost != nil {
			cost = *d.Cost
		}
		cw := d.ContextWindow
		if cw == 0 {
			cw = 128000
		}
		mt := d.MaxTokens
		if mt == 0 {
			mt = 16384
		}
		models[i] = &ai.Model{
			ID: d.ID, Name: name, Api: api, Provider: provider, BaseURL: fauxDefaultBaseURL,
			Reasoning: d.Reasoning, Input: input, Cost: cost, ContextWindow: cw, MaxTokens: mt,
		}
	}

	reg := &FauxProviderRegistration{
		Api:          api,
		Models:       models,
		State:        &FauxState{},
		sourceID:     randomID("faux-provider"),
		minTokenSize: minT,
		maxTokenSize: maxT,
		tps:          options.TokensPerSecond,
		promptCache:  map[string]string{},
		provider:     provider,
	}

	streamSimple := func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		outer := ai.NewAssistantMessageEventStream()
		reg.mu.Lock()
		var step FauxResponseStep
		if len(reg.pending) > 0 {
			step = reg.pending[0]
			reg.pending = reg.pending[1:]
		}
		reg.State.CallCount++
		reg.mu.Unlock()

		go func() {
			if opts != nil && opts.OnResponse != nil {
				_ = opts.OnResponse(ai.ProviderResponse{Status: 200, Headers: map[string]string{}}, model)
			}
			if step == nil {
				msg := fauxErrorMessage(fmt.Errorf("No more faux responses queued"), api, provider, model.ID)
				msg = reg.withUsageEstimate(msg, req, opts)
				outer.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: ai.StopError, Error: msg})
				outer.End()
				return
			}
			resolved := step(req, opts, reg.State, model)
			msg := fauxClone(resolved, api, provider, model.ID)
			msg = reg.withUsageEstimate(msg, req, opts)
			reg.streamWithDeltas(ctx, outer, msg)
		}()
		return outer
	}

	ai.RegisterApiProvider(ai.ApiProvider{
		Api: api,
		Stream: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			var simple *ai.SimpleStreamOptions
			if opts != nil {
				simple = &ai.SimpleStreamOptions{StreamOptions: *opts}
			}
			return streamSimple(ctx, model, req, simple)
		},
		StreamSimple: streamSimple,
	}, reg.sourceID)

	return reg
}

func (r *FauxProviderRegistration) streamWithDeltas(ctx context.Context, stream *ai.AssistantMessageEventStream, message *ai.AssistantMessage) {
	partial := message.Clone()
	partial.Content = ai.ContentList{}

	abortNow := func() bool {
		if ctx != nil && ctx.Err() != nil {
			a := fauxAbortedMessage(partial)
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: ai.StopAborted, Error: a})
			stream.End()
			return true
		}
		return false
	}

	if abortNow() {
		return
	}
	stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: partial.Clone()})

	for index, block := range message.Content {
		if abortNow() {
			return
		}
		switch b := block.(type) {
		case ai.ThinkingContent:
			partial.Content = append(partial.Content, ai.ThinkingContent{})
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingStart, ContentIndex: index, Partial: partial.Clone()})
			acc := ""
			for _, chunk := range splitByTokenSize(b.Thinking, r.minTokenSize, r.maxTokenSize) {
				r.scheduleChunk(chunk)
				if abortNow() {
					return
				}
				acc += chunk
				partial.Content[index] = ai.ThinkingContent{Thinking: acc}
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingDelta, ContentIndex: index, Delta: chunk, Partial: partial.Clone()})
			}
			partial.Content[index] = b
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: index, Content: b.Thinking, Partial: partial.Clone()})
		case ai.TextContent:
			partial.Content = append(partial.Content, ai.TextContent{})
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextStart, ContentIndex: index, Partial: partial.Clone()})
			acc := ""
			for _, chunk := range splitByTokenSize(b.Text, r.minTokenSize, r.maxTokenSize) {
				r.scheduleChunk(chunk)
				if abortNow() {
					return
				}
				acc += chunk
				partial.Content[index] = ai.TextContent{Text: acc}
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, ContentIndex: index, Delta: chunk, Partial: partial.Clone()})
			}
			partial.Content[index] = b
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: index, Content: b.Text, Partial: partial.Clone()})
		case ai.ToolCall:
			partial.Content = append(partial.Content, ai.ToolCall{ID: b.ID, Name: b.Name, Arguments: map[string]any{}})
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallStart, ContentIndex: index, Partial: partial.Clone()})
			argsJSON, _ := json.Marshal(b.Arguments)
			for _, chunk := range splitByTokenSize(string(argsJSON), r.minTokenSize, r.maxTokenSize) {
				r.scheduleChunk(chunk)
				if abortNow() {
					return
				}
				stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: index, Delta: chunk, Partial: partial.Clone()})
			}
			partial.Content[index] = b
			tc := b
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: index, ToolCall: &tc, Partial: partial.Clone()})
		}
	}

	if message.StopReason == ai.StopError || message.StopReason == ai.StopAborted {
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: message.StopReason, Error: message})
		stream.End()
		return
	}
	stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: message.StopReason, Message: message})
	stream.End()
}

func (r *FauxProviderRegistration) scheduleChunk(chunk string) {
	if r.tps <= 0 {
		return
	}
	delay := time.Duration(float64(estimateTokens(chunk)) / r.tps * float64(time.Second))
	time.Sleep(delay)
}

func (r *FauxProviderRegistration) withUsageEstimate(message *ai.AssistantMessage, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessage {
	promptText := serializeContext(req)
	promptTokens := estimateTokens(promptText)
	outputTokens := estimateTokens(assistantContentToText(message.Content))
	input := promptTokens
	cacheRead := 0
	cacheWrite := 0

	var sessionID string
	var retention ai.CacheRetention
	if opts != nil {
		sessionID = opts.SessionID
		retention = opts.CacheRetention
	}
	if sessionID != "" && retention != ai.CacheNone {
		r.mu.Lock()
		prev := r.promptCache[sessionID]
		if prev != "" {
			cachedChars := commonPrefixLength(prev, promptText)
			cacheRead = estimateTokens(prev[:cachedChars])
			cacheWrite = estimateTokens(promptText[cachedChars:])
			input = promptTokens - cacheRead
			if input < 0 {
				input = 0
			}
		} else {
			cacheWrite = promptTokens
		}
		r.promptCache[sessionID] = promptText
		r.mu.Unlock()
	}

	message.Usage = ai.Usage{
		Input: input, Output: outputTokens, CacheRead: cacheRead, CacheWrite: cacheWrite,
		TotalTokens: input + outputTokens + cacheRead + cacheWrite,
	}
	return message
}

// ---- helpers ----

func estimateTokens(text string) int {
	return int(math.Ceil(float64(len(text)) / 4))
}

func randomID(prefix string) string {
	return fmt.Sprintf("%s:%d:%s", prefix, nowMillis(), strings.TrimPrefix(fmt.Sprintf("%x", rand.Int63()), "0x"))
}

func nowMillis() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

func splitByTokenSize(text string, minT, maxT int) []string {
	var chunks []string
	i := 0
	for i < len(text) {
		tokenSize := minT + rand.Intn(maxT-minT+1)
		charSize := tokenSize * 4
		if charSize < 1 {
			charSize = 1
		}
		end := i + charSize
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[i:end])
		i = end
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

func commonPrefixLength(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func fauxClone(message *ai.AssistantMessage, api, provider, modelID string) *ai.AssistantMessage {
	c := message.Clone()
	c.Api = api
	c.Provider = provider
	c.Model = modelID
	if c.Timestamp == 0 {
		c.Timestamp = nowMillis()
	}
	return c
}

func fauxErrorMessage(err error, api, provider, modelID string) *ai.AssistantMessage {
	return &ai.AssistantMessage{
		Content: ai.ContentList{}, Api: api, Provider: provider, Model: modelID,
		StopReason: ai.StopError, ErrorMessage: err.Error(), Timestamp: nowMillis(),
	}
}

func fauxAbortedMessage(partial *ai.AssistantMessage) *ai.AssistantMessage {
	c := partial.Clone()
	c.StopReason = ai.StopAborted
	c.ErrorMessage = "Request was aborted"
	c.Timestamp = nowMillis()
	return c
}

func contentToText(content ai.ContentList) string {
	var parts []string
	for _, b := range content {
		switch v := b.(type) {
		case ai.TextContent:
			parts = append(parts, v.Text)
		case ai.ImageContent:
			parts = append(parts, fmt.Sprintf("[image:%s:%d]", v.MimeType, len(v.Data)))
		}
	}
	return strings.Join(parts, "\n")
}

func assistantContentToText(content ai.ContentList) string {
	var parts []string
	for _, b := range content {
		switch v := b.(type) {
		case ai.TextContent:
			parts = append(parts, v.Text)
		case ai.ThinkingContent:
			parts = append(parts, v.Thinking)
		case ai.ToolCall:
			args, _ := json.Marshal(v.Arguments)
			parts = append(parts, fmt.Sprintf("%s:%s", v.Name, string(args)))
		}
	}
	return strings.Join(parts, "\n")
}

func messageToText(m ai.Message) string {
	switch v := m.(type) {
	case ai.UserMessage:
		return contentToText(v.Content)
	case *ai.AssistantMessage:
		return assistantContentToText(v.Content)
	case ai.AssistantMessage:
		return assistantContentToText(v.Content)
	case ai.ToolResultMessage:
		return strings.Join(append([]string{v.ToolName}, contentToText(v.Content)), "\n")
	}
	return ""
}

func serializeContext(req ai.Context) string {
	var parts []string
	if req.SystemPrompt != "" {
		parts = append(parts, "system:"+req.SystemPrompt)
	}
	for _, m := range req.Messages {
		parts = append(parts, string(m.MessageRole())+":"+messageToText(m))
	}
	if len(req.Tools) > 0 {
		tj, _ := json.Marshal(req.Tools)
		parts = append(parts, "tools:"+string(tj))
	}
	return strings.Join(parts, "\n\n")
}
