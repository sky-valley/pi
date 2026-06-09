package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/sky-valley/pi/ai"
)

var defaultModel = &ai.Model{
	ID: "unknown", Name: "unknown", Api: "unknown", Provider: "unknown",
	Input: []string{}, ContextWindow: 0, MaxTokens: 0,
}

// AgentState is the public, mutable state of an Agent.
type AgentState struct {
	SystemPrompt  string
	Model         *ai.Model
	ThinkingLevel ThinkingLevel
	Tools         []AgentTool
	Messages      []AgentMessage

	IsStreaming      bool
	StreamingMessage AgentMessage
	PendingToolCalls map[string]bool
	ErrorMessage     string
}

// Listener receives agent events with the active run's cancellation context.
type Listener func(ctx context.Context, event AgentEvent) error

type pendingQueue struct {
	mode     QueueMode
	messages []AgentMessage
}

func (q *pendingQueue) enqueue(m AgentMessage) { q.messages = append(q.messages, m) }
func (q *pendingQueue) hasItems() bool         { return len(q.messages) > 0 }
func (q *pendingQueue) clear()                 { q.messages = nil }
func (q *pendingQueue) drain() []AgentMessage {
	if q.mode == QueueAll {
		drained := q.messages
		q.messages = nil
		return drained
	}
	if len(q.messages) == 0 {
		return nil
	}
	first := q.messages[0]
	q.messages = q.messages[1:]
	return []AgentMessage{first}
}

// AgentOptions configures a new Agent.
type AgentOptions struct {
	InitialState     *AgentState
	ConvertToLlm     func(messages []AgentMessage) []ai.Message
	TransformContext func(ctx context.Context, messages []AgentMessage) []AgentMessage
	StreamFn         StreamFn
	GetApiKey        func(provider string) string
	OnPayload        func(payload any, model *ai.Model) (any, error)
	OnResponse       func(resp ai.ProviderResponse, model *ai.Model) error
	BeforeToolCall   func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall    func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
	PrepareNextTurn  func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
	SteeringMode     QueueMode
	FollowUpMode     QueueMode
	SessionID        string
	ThinkingBudgets  *ai.ThinkingBudgets
	Transport        ai.Transport
	MaxRetryDelayMs  *int
	MaxRetries       int
	TimeoutMs        int
	Temperature      *float64
	MaxTokens        *int
	CacheRetention   ai.CacheRetention
	Headers          map[string]string
	ToolExecution    ToolExecutionMode
}

type activeRun struct {
	cancel context.CancelFunc
	ctx    context.Context
	done   chan struct{}
}

// Agent is a stateful wrapper around the low-level agent loop. It owns the
// transcript, emits lifecycle events, executes tools, and exposes steering/
// follow-up queueing.
type Agent struct {
	mu        sync.Mutex
	state     AgentState
	listeners []Listener

	steeringQueue pendingQueue
	followUpQueue pendingQueue

	ConvertToLlm     func(messages []AgentMessage) []ai.Message
	TransformContext func(ctx context.Context, messages []AgentMessage) []AgentMessage
	StreamFn         StreamFn
	GetApiKey        func(provider string) string
	OnPayload        func(payload any, model *ai.Model) (any, error)
	OnResponse       func(resp ai.ProviderResponse, model *ai.Model) error
	BeforeToolCall   func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall    func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
	PrepareNextTurn  func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate

	SessionID       string
	ThinkingBudgets *ai.ThinkingBudgets
	Transport       ai.Transport
	MaxRetryDelayMs *int
	MaxRetries      int
	TimeoutMs       int
	Temperature     *float64
	MaxTokens       *int
	CacheRetention  ai.CacheRetention
	Headers         map[string]string
	ToolExecution   ToolExecutionMode

	active *activeRun
}

// NewAgent constructs an Agent from options.
func NewAgent(opts AgentOptions) *Agent {
	st := AgentState{
		Model:            defaultModel,
		ThinkingLevel:    ThinkOff,
		PendingToolCalls: map[string]bool{},
	}
	if opts.InitialState != nil {
		in := opts.InitialState
		if in.SystemPrompt != "" {
			st.SystemPrompt = in.SystemPrompt
		}
		if in.Model != nil {
			st.Model = in.Model
		}
		if in.ThinkingLevel != "" {
			st.ThinkingLevel = in.ThinkingLevel
		}
		st.Tools = append([]AgentTool(nil), in.Tools...)
		st.Messages = append([]AgentMessage(nil), in.Messages...)
	}
	a := &Agent{
		state:            st,
		ConvertToLlm:     opts.ConvertToLlm,
		TransformContext: opts.TransformContext,
		StreamFn:         opts.StreamFn,
		GetApiKey:        opts.GetApiKey,
		OnPayload:        opts.OnPayload,
		OnResponse:       opts.OnResponse,
		BeforeToolCall:   opts.BeforeToolCall,
		AfterToolCall:    opts.AfterToolCall,
		PrepareNextTurn:  opts.PrepareNextTurn,
		SessionID:        opts.SessionID,
		ThinkingBudgets:  opts.ThinkingBudgets,
		Transport:        opts.Transport,
		MaxRetryDelayMs:  opts.MaxRetryDelayMs,
		MaxRetries:       opts.MaxRetries,
		TimeoutMs:        opts.TimeoutMs,
		Temperature:      opts.Temperature,
		MaxTokens:        opts.MaxTokens,
		CacheRetention:   opts.CacheRetention,
		Headers:          opts.Headers,
		ToolExecution:    opts.ToolExecution,
	}
	if a.ConvertToLlm == nil {
		a.ConvertToLlm = defaultConvertToLlm
	}
	if a.StreamFn == nil {
		a.StreamFn = ai.StreamSimple
	}
	if a.Transport == "" {
		a.Transport = ai.TransportAuto
	}
	if a.ToolExecution == "" {
		a.ToolExecution = ToolParallel
	}
	a.steeringQueue.mode = orMode(opts.SteeringMode, QueueOneAtATime)
	a.followUpQueue.mode = orMode(opts.FollowUpMode, QueueOneAtATime)
	return a
}

func orMode(m, fallback QueueMode) QueueMode {
	if m == "" {
		return fallback
	}
	return m
}

// Subscribe registers an event listener; the returned function unsubscribes.
func (a *Agent) Subscribe(l Listener) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listeners = append(a.listeners, l)
	idx := len(a.listeners) - 1
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if idx < len(a.listeners) {
			a.listeners[idx] = nil
		}
	}
}

// State returns a snapshot view of the agent state. The returned struct is a
// shallow copy; slices/maps share backing storage and should be treated read-only.
func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// SetSystemPrompt sets the system prompt used for future turns.
func (a *Agent) SetSystemPrompt(p string) { a.mu.Lock(); a.state.SystemPrompt = p; a.mu.Unlock() }

// SetModel sets the active model for future turns.
func (a *Agent) SetModel(m *ai.Model) { a.mu.Lock(); a.state.Model = m; a.mu.Unlock() }

// SetThinkingLevel sets the reasoning level for future turns.
func (a *Agent) SetThinkingLevel(l ThinkingLevel) {
	a.mu.Lock()
	a.state.ThinkingLevel = l
	a.mu.Unlock()
}

// SetTools replaces the available tools (copied).
func (a *Agent) SetTools(tools []AgentTool) {
	a.mu.Lock()
	a.state.Tools = append([]AgentTool(nil), tools...)
	a.mu.Unlock()
}

// SetMessages replaces the transcript (copied).
func (a *Agent) SetMessages(messages []AgentMessage) {
	a.mu.Lock()
	a.state.Messages = append([]AgentMessage(nil), messages...)
	a.mu.Unlock()
}

// Steer queues a message to inject after the current assistant turn finishes.
func (a *Agent) Steer(m AgentMessage) { a.mu.Lock(); a.steeringQueue.enqueue(m); a.mu.Unlock() }

// FollowUp queues a message to run after the agent would otherwise stop.
func (a *Agent) FollowUp(m AgentMessage) { a.mu.Lock(); a.followUpQueue.enqueue(m); a.mu.Unlock() }

// ClearSteeringQueue removes all queued steering messages.
func (a *Agent) ClearSteeringQueue() { a.mu.Lock(); a.steeringQueue.clear(); a.mu.Unlock() }

// ClearFollowUpQueue removes all queued follow-up messages.
func (a *Agent) ClearFollowUpQueue() { a.mu.Lock(); a.followUpQueue.clear(); a.mu.Unlock() }

// ClearAllQueues removes all queued messages.
func (a *Agent) ClearAllQueues() {
	a.mu.Lock()
	a.steeringQueue.clear()
	a.followUpQueue.clear()
	a.mu.Unlock()
}

// HasQueuedMessages reports whether either queue has pending messages.
func (a *Agent) HasQueuedMessages() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steeringQueue.hasItems() || a.followUpQueue.hasItems()
}

// Abort cancels the current run, if any.
func (a *Agent) Abort() {
	a.mu.Lock()
	run := a.active
	a.mu.Unlock()
	if run != nil {
		run.cancel()
	}
}

// WaitForIdle blocks until the current run and its listeners finish.
func (a *Agent) WaitForIdle() {
	a.mu.Lock()
	run := a.active
	a.mu.Unlock()
	if run != nil {
		<-run.done
	}
}

// Reset clears transcript, runtime state, and queued messages.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]bool{}
	a.state.ErrorMessage = ""
	a.steeringQueue.clear()
	a.followUpQueue.clear()
}

// Prompt starts a new run from text. Blocks until the run completes.
func (a *Agent) Prompt(ctx context.Context, text string, images ...ai.ImageContent) error {
	content := ai.ContentList{ai.TextContent{Text: text}}
	for _, img := range images {
		content = append(content, img)
	}
	msg := ai.UserMessage{Content: content, Timestamp: nowMillis()}
	return a.PromptMessages(ctx, []AgentMessage{msg})
}

// PromptMessages starts a new run from explicit messages.
func (a *Agent) PromptMessages(ctx context.Context, messages []AgentMessage) error {
	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return errors.New("Agent is already processing a prompt. Use Steer() or FollowUp() to queue messages, or wait for completion.")
	}
	a.mu.Unlock()
	return a.runPromptMessages(ctx, messages, false)
}

// Continue continues from the current transcript. The last message must be a
// user or tool-result message (or queued messages must exist).
func (a *Agent) Continue(ctx context.Context) error {
	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return errors.New("Agent is already processing. Wait for completion before continuing.")
	}
	n := len(a.state.Messages)
	if n == 0 {
		a.mu.Unlock()
		return errors.New("No messages to continue from")
	}
	last := a.state.Messages[n-1]
	a.mu.Unlock()

	if last.MessageRole() == ai.RoleAssistant {
		a.mu.Lock()
		steering := a.steeringQueue.drain()
		a.mu.Unlock()
		if len(steering) > 0 {
			return a.runPromptMessages(ctx, steering, true)
		}
		a.mu.Lock()
		followUps := a.followUpQueue.drain()
		a.mu.Unlock()
		if len(followUps) > 0 {
			return a.runPromptMessages(ctx, followUps, false)
		}
		return errors.New("Cannot continue from message role: assistant")
	}
	return a.runContinuation(ctx)
}

func (a *Agent) runPromptMessages(parent context.Context, messages []AgentMessage, skipInitialSteeringPoll bool) error {
	return a.runWithLifecycle(parent, func(ctx context.Context) {
		runAgentLoop(ctx, messages, a.contextSnapshot(), a.loopConfig(skipInitialSteeringPoll), a.processEvent(ctx), a.StreamFn)
	})
}

func (a *Agent) runContinuation(parent context.Context) error {
	return a.runWithLifecycle(parent, func(ctx context.Context) {
		runAgentLoopContinue(ctx, a.contextSnapshot(), a.loopConfig(false), a.processEvent(ctx), a.StreamFn)
	})
}

var emptyUsage = ai.Usage{
	Cost: ai.CostBreakdown{},
}

// handleRunFailure mirrors pi agent.ts:476-492: when the executor throws (a Go
// panic from streamFn/convertToLlm/transformContext, or a listener error that
// unwound the loop), synthesize a terminal assistant message and emit the full
// failure sequence (message_start → message_end → turn_end → agent_end) so the
// lifecycle is always complete and state.errorMessage is set.
func (a *Agent) handleRunFailure(ctx context.Context, msg string, aborted bool) {
	a.mu.Lock()
	model := a.state.Model
	a.mu.Unlock()

	stop := ai.StopError
	if aborted {
		stop = ai.StopAborted
	}
	failure := &ai.AssistantMessage{
		Content:      ai.ContentList{ai.TextContent{Text: ""}},
		Api:          ai.Api(model.Api),
		Provider:     ai.Provider(model.Provider),
		Model:        model.ID,
		Usage:        emptyUsage,
		StopReason:   stop,
		ErrorMessage: msg,
		Timestamp:    nowMillis(),
	}
	// During failure handling the run has already failed; a listener error here
	// cannot fail it further, so ignore emit errors (pi lets the finally run).
	emit := a.processEvent(ctx)
	_ = emit(AgentEvent{Type: EvMessageStart, Message: failure})
	_ = emit(AgentEvent{Type: EvMessageEnd, Message: failure})
	_ = emit(AgentEvent{Type: EvTurnEnd, Message: failure, ToolResults: []ai.ToolResultMessage{}})
	_ = emit(AgentEvent{Type: EvAgentEnd, Messages: []AgentMessage{failure}})
}

func (a *Agent) contextSnapshot() AgentContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AgentContext{
		SystemPrompt: a.state.SystemPrompt,
		Messages:     append([]AgentMessage(nil), a.state.Messages...),
		Tools:        append([]AgentTool(nil), a.state.Tools...),
	}
}

func (a *Agent) loopConfig(skipInitialSteeringPoll bool) AgentLoopConfig {
	a.mu.Lock()
	model := a.state.Model
	reasoning := a.state.ThinkingLevel
	a.mu.Unlock()

	skip := skipInitialSteeringPoll
	cfg := AgentLoopConfig{
		Model:            model,
		Reasoning:        reasoning,
		SessionID:        a.SessionID,
		Transport:        a.Transport,
		ThinkingBudgets:  a.ThinkingBudgets,
		MaxRetryDelayMs:  a.MaxRetryDelayMs,
		MaxRetries:       a.MaxRetries,
		TimeoutMs:        a.TimeoutMs,
		Temperature:      a.Temperature,
		MaxTokens:        a.MaxTokens,
		CacheRetention:   a.CacheRetention,
		Headers:          a.Headers,
		ToolExecution:    a.ToolExecution,
		OnPayload:        a.OnPayload,
		OnResponse:       a.OnResponse,
		ConvertToLlm:     a.ConvertToLlm,
		TransformContext: a.TransformContext,
		GetApiKey:        a.GetApiKey,
		BeforeToolCall:   a.BeforeToolCall,
		AfterToolCall:    a.AfterToolCall,
		PrepareNextTurn:  a.PrepareNextTurn,
		GetSteeringMessages: func() []AgentMessage {
			a.mu.Lock()
			defer a.mu.Unlock()
			if skip {
				skip = false
				return nil
			}
			return a.steeringQueue.drain()
		},
		GetFollowUpMessages: func() []AgentMessage {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.followUpQueue.drain()
		},
	}
	if reasoning == ThinkOff {
		cfg.Reasoning = ""
	}
	return cfg
}

func (a *Agent) runWithLifecycle(parent context.Context, executor func(ctx context.Context)) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	run := &activeRun{cancel: cancel, ctx: ctx, done: make(chan struct{})}

	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		cancel()
		return errors.New("Agent is already processing.")
	}
	a.active = run
	a.state.IsStreaming = true
	a.state.StreamingMessage = nil
	a.state.ErrorMessage = ""
	a.mu.Unlock()

	// pi wraps the executor in try/catch → handleRunFailure (agent.ts:467-492).
	// In Go the failure surfaces as a panic: an emitPanic (a listener returned
	// an error and unwound the loop) or any other panic from streamFn /
	// convertToLlm / transformContext. Either way we synthesize the failure turn.
	func() {
		defer func() {
			if r := recover(); r != nil {
				var msg string
				if ep, ok := r.(emitPanic); ok {
					msg = ep.err.Error()
				} else {
					msg = panicMessage(r)
				}
				a.handleRunFailure(ctx, msg, ctx.Err() != nil)
			}
		}()
		executor(ctx)
	}()

	a.mu.Lock()
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]bool{}
	a.active = nil
	a.mu.Unlock()
	cancel()
	close(run.done)
	return nil
}

// processEvent reduces internal state for a loop event, then notifies listeners.
func (a *Agent) processEvent(ctx context.Context) EventSink {
	return func(event AgentEvent) error {
		a.mu.Lock()
		switch event.Type {
		case EvMessageStart, EvMessageUpdate:
			a.state.StreamingMessage = event.Message
		case EvMessageEnd:
			a.state.StreamingMessage = nil
			a.state.Messages = append(a.state.Messages, event.Message)
		case EvToolExecutionStart:
			a.state.PendingToolCalls[event.ToolCallID] = true
		case EvToolExecutionEnd:
			delete(a.state.PendingToolCalls, event.ToolCallID)
		case EvTurnEnd:
			if am, ok := asAssistant(event.Message); ok && am.ErrorMessage != "" {
				a.state.ErrorMessage = am.ErrorMessage
			}
		case EvAgentEnd:
			a.state.StreamingMessage = nil
		}
		listeners := append([]Listener(nil), a.listeners...)
		a.mu.Unlock()

		for _, l := range listeners {
			if l == nil {
				continue
			}
			if err := l(ctx, event); err != nil {
				return err
			}
		}
		return nil
	}
}

func asAssistant(m AgentMessage) (*ai.AssistantMessage, bool) {
	switch v := m.(type) {
	case *ai.AssistantMessage:
		return v, true
	case ai.AssistantMessage:
		return &v, true
	default:
		return nil, false
	}
}
