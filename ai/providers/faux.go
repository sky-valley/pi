// Package providers contains concrete ai.ApiProvider implementations (faux for
// testing, plus real LLM providers) registered with the ai registry.
package providers

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
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
	ID        string
	Name      string
	Reasoning bool
	Input     []string
	// InputLimits carries the model's image resize profile and provider input
	// limits (pi FauxModelDefinition.inputLimits, upstream f5c946480), so a test
	// fixture can express a narrowed profile the way the catalog does.
	InputLimits   *ai.ModelInputLimits
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
type FauxResponseStep func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, state *FauxState, model *ai.Model) *ai.AssistantMessage

// FauxStatic wraps a fixed assistant message as a response step.
func FauxStatic(msg *ai.AssistantMessage) FauxResponseStep {
	return func(ai.TranscriptContext, *ai.SimpleStreamOptions, *FauxState, *ai.Model) *ai.AssistantMessage {
		return msg
	}
}

// FauxState exposes mutable per-registration state.
type FauxState struct {
	CallCount int
	// DeferredFetchCount counts FetchDeferred calls, including the ones that
	// answered with the handle again.
	DeferredFetchCount int
	// CancelledDeferred records every handle passed to CancelDeferred, in order.
	CancelledDeferred []ai.DeferredHandle
}

// FauxDeferredOptions script the deferred-response behavior of a faux provider
// (pi RegisterFauxProviderOptions.deferred).
type FauxDeferredOptions struct {
	// PendingFetches is how many fetches answer with the handle again before the
	// scripted response becomes ready. Negative values count as zero.
	PendingFetches int
	// PollAfterMs is echoed on every handle the provider issues.
	PollAfterMs int64
}

// RegisterFauxProviderOptions configures a faux provider registration.
type RegisterFauxProviderOptions struct {
	Api      string
	Provider string
	Models   []FauxModelDefinition
	// Deferred enables deferred responses; nil still registers the deferred
	// entry points, it just leaves them at their defaults.
	Deferred        *FauxDeferredOptions
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
	promptCache  map[string][]string // per session: one prompt text per message
	provider     string
	deferredCfg  FauxDeferredOptions
	deferred     map[string]*fauxDeferredEntry
}

// fauxDeferredEntry is one accepted-but-unfinished submission, held until a
// fetch redeems it or a cancel drops it.
type fauxDeferredEntry struct {
	// mu guards the mutable fields below. It is per entry, not the
	// registration's lock, because redeeming runs the scripted step, which
	// takes the registration lock to estimate usage.
	mu             sync.Mutex
	handle         ai.DeferredHandle
	step           FauxResponseStep
	req            ai.TranscriptContext
	opts           *ai.SimpleStreamOptions
	model          *ai.Model
	pendingFetches int
	cancelled      bool
	final          *ai.AssistantMessage
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
			Reasoning: d.Reasoning, Input: input, InputLimits: d.InputLimits,
			Cost: cost, ContextWindow: cw, MaxTokens: mt,
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
		promptCache:  map[string][]string{},
		provider:     provider,
		deferred:     map[string]*fauxDeferredEntry{},
	}
	if options.Deferred != nil {
		reg.deferredCfg = *options.Deferred
		reg.deferredCfg.PendingFetches = max(0, reg.deferredCfg.PendingFetches)
	}

	streamSimple := func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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
			// pi awaits onResponse inside the stream's try: a throw lands in its
			// catch, which fails the stream (the step stays consumed).
			if opts != nil && opts.OnResponse != nil {
				if err := opts.OnResponse(ai.ProviderResponse{Status: 200, Headers: map[string]string{}}, model); err != nil {
					reg.failStream(outer, err, model)
					return
				}
			}
			if step == nil {
				msg := fauxErrorMessage(fmt.Errorf("No more faux responses queued"), api, provider, model.ID)
				msg = reg.withUsageEstimate(msg, req, opts)
				outer.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: ai.StopError, Error: msg})
				outer.End()
				return
			}
			if opts != nil && opts.Deferred != nil {
				handle := ai.DeferredHandle{
					Provider:    model.Provider,
					ModelID:     model.ID,
					Api:         model.Api,
					ID:          randomID("deferred"),
					PollAfterMs: reg.deferredCfg.PollAfterMs,
				}
				reg.mu.Lock()
				reg.deferred[handle.ID] = &fauxDeferredEntry{
					handle:         handle,
					step:           step,
					req:            req,
					opts:           opts,
					model:          model,
					pendingFetches: reg.deferredCfg.PendingFetches,
				}
				reg.mu.Unlock()
				reg.streamWithDeltas(ctx, outer, fauxDeferredMessage(model, handle))
				return
			}
			reg.streamWithDeltas(ctx, outer, reg.resolveResponse(step, req, opts, model))
		}()
		return outer
	}

	fetchDeferred := func(ctx context.Context, model *ai.Model, handle ai.DeferredHandle, opts *ai.DeferredFetchOptions) *ai.AssistantMessageEventStream {
		outer := ai.NewAssistantMessageEventStream()
		reg.mu.Lock()
		reg.State.DeferredFetchCount++
		reg.mu.Unlock()

		go func() {
			if opts != nil && opts.OnResponse != nil {
				if err := opts.OnResponse(ai.ProviderResponse{Status: 200, Headers: map[string]string{}}, model); err != nil {
					reg.failStream(outer, err, model)
					return
				}
			}
			message, err := reg.redeem(model, handle)
			if err != nil {
				reg.failStream(outer, err, model)
				return
			}
			reg.streamWithDeltas(ctx, outer, message)
		}()
		return outer
	}

	cancelDeferred := func(_ context.Context, model *ai.Model, handle ai.DeferredHandle, opts *ai.DeferredCancelOptions) error {
		reg.mu.Lock()
		reg.State.CancelledDeferred = append(reg.State.CancelledDeferred, handle)
		entry := reg.deferred[handle.ID]
		reg.mu.Unlock()
		if entry != nil {
			entry.mu.Lock()
			entry.cancelled = true
			entry.mu.Unlock()
		}
		if opts != nil && opts.OnResponse != nil {
			return opts.OnResponse(ai.ProviderResponse{Status: 200, Headers: map[string]string{}}, model)
		}
		return nil
	}

	ai.RegisterApiProvider(ai.ApiProvider{
		Api: api,
		Stream: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			var simple *ai.SimpleStreamOptions
			if opts != nil {
				simple = &ai.SimpleStreamOptions{StreamOptions: *opts}
			}
			return streamSimple(ctx, model, req, simple)
		},
		StreamSimple:   streamSimple,
		FetchDeferred:  fetchDeferred,
		CancelDeferred: cancelDeferred,
	}, reg.sourceID)

	return reg
}

// failStream ends a faux stream the way pi's catch does: an error event with
// reason "error" — even for an aborted request — carrying the thrown message,
// empty content and the default usage.
func (r *FauxProviderRegistration) failStream(stream *ai.AssistantMessageEventStream, err error, model *ai.Model) {
	msg := fauxErrorMessage(err, r.Api, r.provider, model.ID)
	stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: ai.StopError, Error: msg})
	stream.End()
}

// resolveResponse runs one scripted step and finishes the message the way a
// real provider would (identity fields plus a usage estimate).
func (r *FauxProviderRegistration) resolveResponse(
	step FauxResponseStep,
	req ai.TranscriptContext,
	opts *ai.SimpleStreamOptions,
	model *ai.Model,
) *ai.AssistantMessage {
	resolved := step(req, opts, r.State, model)
	msg := fauxClone(resolved, r.Api, r.provider, model.ID)
	return r.withUsageEstimate(msg, req, opts)
}

// redeem answers one FetchDeferred: the handle again while fetches are still
// scripted as pending, otherwise the scripted response, resolved once and
// cached so repeat fetches are idempotent.
func (r *FauxProviderRegistration) redeem(
	model *ai.Model,
	handle ai.DeferredHandle,
) (*ai.AssistantMessage, error) {
	r.mu.Lock()
	entry := r.deferred[handle.ID]
	r.mu.Unlock()
	if entry == nil ||
		entry.handle.Provider != handle.Provider ||
		entry.handle.ModelID != handle.ModelID ||
		entry.handle.Api != handle.Api {
		return nil, fmt.Errorf("Unknown faux deferred response: %s", handle.ID)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.cancelled {
		return nil, fmt.Errorf("Faux deferred response was cancelled: %s", handle.ID)
	}
	if entry.pendingFetches > 0 {
		entry.pendingFetches--
		return fauxDeferredMessage(model, entry.handle), nil
	}
	if entry.final == nil {
		entry.final = r.resolveResponse(entry.step, entry.req, fauxRedeemOptions(entry.opts), entry.model)
	}
	return entry.final, nil
}

// fauxRedeemOptions builds the options the scripted step sees when a fetch
// redeems a submission: the submission's own options, minus the deferral
// request and its own response hook. The fetch's options do not participate —
// upstream 686f193e5 stopped spreading them over the submission's, because a
// redeemed response was shaped when it was submitted and a fetch has no
// request body left to influence.
func fauxRedeemOptions(submission *ai.SimpleStreamOptions) *ai.SimpleStreamOptions {
	redeem := ai.SimpleStreamOptions{}
	if submission != nil {
		redeem = *submission
	}
	redeem.Deferred = nil
	redeem.OnResponse = nil
	return &redeem
}

// fauxDeferredMessage is the empty assistant message that carries a handle.
func fauxDeferredMessage(model *ai.Model, handle ai.DeferredHandle) *ai.AssistantMessage {
	return &ai.AssistantMessage{
		Content:    ai.ContentList{},
		Api:        model.Api,
		Provider:   model.Provider,
		Model:      model.ID,
		Usage:      fauxDefaultUsage,
		StopReason: ai.StopDeferred,
		Deferred:   &handle,
		Timestamp:  nowMillis(),
	}
}

func (r *FauxProviderRegistration) streamWithDeltas(ctx context.Context, stream *ai.AssistantMessageEventStream, message *ai.AssistantMessage) {
	partial := message.Clone()
	partial.Content = ai.ContentList{}
	// The in-flight partial carries no terminal reason yet (upstream f9a49869).
	partial.StopReason = ai.StopPending

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
			argsJSON, _ := jstext.Stringify(b.OrderedArguments())
			for _, chunk := range splitByTokenSize(argsJSON, r.minTokenSize, r.maxTokenSize) {
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

	// A faux response that never resolved its stop reason is a test-authoring
	// error; pi throws here (upstream f9a49869), which we surface as a stream error.
	if message.StopReason == ai.StopPending {
		errMsg := fauxErrorMessage(fmt.Errorf("Faux response ended without a stop reason"), message.Api, message.Provider, message.Model)
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: ai.StopError, Error: errMsg})
		stream.End()
		return
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

func (r *FauxProviderRegistration) withUsageEstimate(message *ai.AssistantMessage, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessage {
	// One text per message; the whole prompt joins them with blank lines.
	prompt := promptTexts(req)
	promptLength := joinedLength(prompt, len(prompt))
	promptTokens := estimateCodeUnitTokens(promptLength)
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
		if previous, ok := r.promptCache[sessionID]; ok {
			cachedChars := commonPromptPrefixLength(previous, prompt)
			cacheRead = estimateCodeUnitTokens(cachedChars)
			cacheWrite = estimateCodeUnitTokens(promptLength - cachedChars)
			input = max(0, promptTokens-cacheRead)
		} else {
			cacheWrite = promptTokens
		}
		r.promptCache[sessionID] = prompt
		r.mu.Unlock()
	}

	message.Usage = ai.Usage{
		Input: input, Output: outputTokens, CacheRead: cacheRead, CacheWrite: cacheWrite,
		TotalTokens: input + outputTokens + cacheRead + cacheWrite,
	}
	return message
}

// ---- helpers ----

// estimateTokens is pi's `Math.ceil(text.length / 4)`, over the JS length in
// UTF-16 code units.
func estimateTokens(text string) int {
	return estimateCodeUnitTokens(utf16Length(text))
}

func estimateCodeUnitTokens(codeUnits int) int {
	return int(math.Ceil(float64(codeUnits) / 4))
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

// joinedLength is the length, in UTF-16 code units as JS counts it, of the
// prompt text that joins the first count messages with blank lines.
func joinedLength(messages []string, count int) int {
	length := 0
	if count > 0 {
		length = (count - 1) * 2
	}
	for _, message := range messages[:count] {
		length += utf16Length(message)
	}
	return length
}

// commonPromptPrefixLength is the length of the common prefix of the two
// joined prompts, in UTF-16 code units. Equal messages are compared whole;
// characters are compared only from the first message that differs.
func commonPromptPrefixLength(previous, current []string) int {
	index := 0
	for index < len(previous) && index < len(current) && previous[index] == current[index] {
		index++
	}
	rest := func(messages []string) []uint16 {
		if index == len(messages) {
			return nil
		}
		text := strings.Join(messages[index:], "\n\n")
		if index > 0 {
			text = "\n\n" + text
		}
		return utf16.Encode([]rune(text))
	}
	return joinedLength(previous, index) + commonPrefixLength(rest(previous), rest(current))
}

func commonPrefixLength(a, b []uint16) int {
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
	return ai.NewErrorMessage(ai.Api(api), ai.ProviderId(provider), modelID, ai.StopError, err.Error())
}

func fauxAbortedMessage(partial *ai.AssistantMessage) *ai.AssistantMessage {
	c := partial.Clone()
	c.StopReason = ai.StopAborted
	c.ErrorMessage = errRequestWasAborted.Error()
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
			args, _ := jstext.Stringify(v.OrderedArguments())
			parts = append(parts, fmt.Sprintf("%s:%s", v.Name, args))
		}
	}
	return strings.Join(parts, "\n")
}

func messageToText(m ai.Message) string {
	if sm, ok := asSystemMsg(m); ok {
		// getSystemMessageText, then one `tool-:` line per removal and one `tool+:`
		// line per addition, empty parts dropped (faux.ts, upstream 9e05370b2).
		parts := []string{ai.GetSystemMessageText(sm)}
		for _, tool := range sm.ToolsRemoved {
			tj, _ := jstext.Stringify(tool)
			parts = append(parts, "tool-:"+tj)
		}
		for _, tool := range sm.ToolsAdded {
			tj, _ := jstext.Stringify(tool)
			parts = append(parts, "tool+:"+tj)
		}
		nonEmpty := parts[:0]
		for _, part := range parts {
			if part != "" {
				nonEmpty = append(nonEmpty, part)
			}
		}
		return strings.Join(nonEmpty, "\n")
	}
	if um, ok := asUserMsg(m); ok {
		return contentToText(um.Content)
	}
	if am, ok := asAssistantMsg(m); ok {
		return assistantContentToText(am.Content)
	}
	if tr, ok := asToolResultMsg(m); ok {
		// `[toolName, ...content.map(block => contentToText([block]))].join("\n")`:
		// no content leaves the tool name alone.
		parts := []string{tr.ToolName}
		for _, block := range tr.Content {
			parts = append(parts, contentToText(ai.ContentList{block}))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// promptTexts renders the transcript as the faux prompt, one `role:text` per
// message; the prompt text joins them with blank lines (the prompt and tools
// ride the system messages, upstream 9e05370b2).
func promptTexts(req ai.TranscriptContext) []string {
	texts := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		texts[i] = string(m.MessageRole()) + ":" + messageToText(m)
	}
	return texts
}
