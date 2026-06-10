package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/sky-valley/pi/ai"
)

// StreamFn streams an assistant response. Defaults to ai.StreamSimple. Per the
// stream contract it must encode failures in the returned stream, not panic.
type StreamFn func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream

// emitPanic is the value used to unwind the loop when an event listener returns
// an error. pi rejects the run when a subscriber throws (agent.ts:553-555); the
// rejection propagates out of runAgentLoop and is caught by runWithLifecycle.
// Go has no exceptions, so we mirror that unwind with a typed panic that is
// recovered at the run boundary (in agent.go) or in the low-level goroutine.
type emitPanic struct{ err error }

// mustEmit emits an event and unwinds the loop (via panic) if the sink returns
// an error, matching pi's "await emit(...)" throwing on a rejected listener.
func mustEmit(emit EventSink, e AgentEvent) {
	if err := emit(e); err != nil {
		panic(emitPanic{err: err})
	}
}

// AgentLoop starts an agent loop with new prompt messages, returning a stream of
// AgentEvents whose final result is the list of new messages produced.
func AgentLoop(ctx context.Context, prompts []AgentMessage, agentCtx AgentContext, config AgentLoopConfig, streamFn StreamFn) *ai.EventStream[AgentEvent, []AgentMessage] {
	stream := newAgentStream()
	go func() {
		// The low-level loop has no failure-turn synthesis (that lives in the
		// Agent wrapper, matching pi). Recover so a listener error / panic ends
		// the stream instead of crashing the goroutine; pi rejects the returned
		// promise in the equivalent case.
		defer func() { _ = recover() }()
		messages := runAgentLoop(ctx, prompts, agentCtx, config, func(e AgentEvent) error {
			stream.Push(e)
			return nil
		}, streamFn)
		stream.End(messages)
	}()
	return stream
}

// AgentLoopContinue continues from the current context without a new message.
// The last message must convert to a user or tool-result message.
func AgentLoopContinue(ctx context.Context, agentCtx AgentContext, config AgentLoopConfig, streamFn StreamFn) (*ai.EventStream[AgentEvent, []AgentMessage], error) {
	if len(agentCtx.Messages) == 0 {
		return nil, errors.New("Cannot continue: no messages in context")
	}
	if agentCtx.Messages[len(agentCtx.Messages)-1].MessageRole() == ai.RoleAssistant {
		return nil, errors.New("Cannot continue from message role: assistant")
	}
	stream := newAgentStream()
	go func() {
		defer func() { _ = recover() }()
		messages := runAgentLoopContinue(ctx, agentCtx, config, func(e AgentEvent) error {
			stream.Push(e)
			return nil
		}, streamFn)
		stream.End(messages)
	}()
	return stream, nil
}

func newAgentStream() *ai.EventStream[AgentEvent, []AgentMessage] {
	return ai.NewEventStream(
		func(e AgentEvent) bool { return e.Type == EvAgentEnd },
		func(e AgentEvent) []AgentMessage {
			if e.Type == EvAgentEnd {
				return e.Messages
			}
			return nil
		},
	)
}

func runAgentLoop(ctx context.Context, prompts []AgentMessage, agentCtx AgentContext, config AgentLoopConfig, emit EventSink, streamFn StreamFn) []AgentMessage {
	newMessages := append([]AgentMessage(nil), prompts...)
	current := agentCtx
	current.Messages = append(append([]AgentMessage(nil), agentCtx.Messages...), prompts...)

	mustEmit(emit, AgentEvent{Type: EvAgentStart})
	mustEmit(emit, AgentEvent{Type: EvTurnStart})
	for _, p := range prompts {
		mustEmit(emit, AgentEvent{Type: EvMessageStart, Message: p})
		mustEmit(emit, AgentEvent{Type: EvMessageEnd, Message: p})
	}

	runLoop(ctx, &current, &newMessages, config, emit, streamFn)
	return newMessages
}

func runAgentLoopContinue(ctx context.Context, agentCtx AgentContext, config AgentLoopConfig, emit EventSink, streamFn StreamFn) []AgentMessage {
	newMessages := []AgentMessage{}
	current := agentCtx

	mustEmit(emit, AgentEvent{Type: EvAgentStart})
	mustEmit(emit, AgentEvent{Type: EvTurnStart})

	runLoop(ctx, &current, &newMessages, config, emit, streamFn)
	return newMessages
}

func aborted(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

func runLoop(ctx context.Context, current *AgentContext, newMessages *[]AgentMessage, config AgentLoopConfig, emit EventSink, streamFn StreamFn) {
	firstTurn := true
	var pending []AgentMessage
	if config.GetSteeringMessages != nil {
		pending = config.GetSteeringMessages()
	}

	for { // outer loop: follow-up messages
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pending) > 0 {
			if !firstTurn {
				mustEmit(emit, AgentEvent{Type: EvTurnStart})
			} else {
				firstTurn = false
			}

			if len(pending) > 0 {
				for _, m := range pending {
					mustEmit(emit, AgentEvent{Type: EvMessageStart, Message: m})
					mustEmit(emit, AgentEvent{Type: EvMessageEnd, Message: m})
					current.Messages = append(current.Messages, m)
					*newMessages = append(*newMessages, m)
				}
				pending = nil
			}

			message := streamAssistantResponse(ctx, current, config, emit, streamFn)
			*newMessages = append(*newMessages, message)

			if message.StopReason == ai.StopError || message.StopReason == ai.StopAborted {
				// pi emits toolResults: [] here (agent-loop.ts:197), never null.
				mustEmit(emit, AgentEvent{Type: EvTurnEnd, Message: message, ToolResults: []ai.ToolResultMessage{}})
				mustEmit(emit, AgentEvent{Type: EvAgentEnd, Messages: *newMessages})
				return
			}

			toolCalls := filterToolCalls(message)
			// Non-nil so a no-tool turn_end carries [] like pi (agent-loop.ts:205).
			toolResults := []ai.ToolResultMessage{}
			hasMoreToolCalls = false
			if len(toolCalls) > 0 {
				batch := executeToolCalls(ctx, current, message, config, emit)
				toolResults = append(toolResults, batch.messages...)
				hasMoreToolCalls = !batch.terminate
				for _, r := range toolResults {
					current.Messages = append(current.Messages, r)
					*newMessages = append(*newMessages, r)
				}
			}

			mustEmit(emit, AgentEvent{Type: EvTurnEnd, Message: message, ToolResults: toolResults})

			nextTurnCtx := ShouldStopAfterTurnContext{
				Message:     message,
				ToolResults: toolResults,
				Context:     current,
				NewMessages: *newMessages,
			}
			if config.PrepareNextTurn != nil {
				if snap := config.PrepareNextTurn(nextTurnCtx); snap != nil {
					if snap.Context != nil {
						*current = *snap.Context
					}
					if snap.Model != nil {
						config.Model = snap.Model
					}
					if snap.ThinkingLevel != nil {
						if *snap.ThinkingLevel == "off" {
							config.Reasoning = ""
						} else {
							config.Reasoning = ThinkingLevel(*snap.ThinkingLevel)
						}
					}
				}
			}

			if config.ShouldStopAfterTurn != nil && config.ShouldStopAfterTurn(nextTurnCtx) {
				mustEmit(emit, AgentEvent{Type: EvAgentEnd, Messages: *newMessages})
				return
			}

			if config.GetSteeringMessages != nil {
				pending = config.GetSteeringMessages()
			} else {
				pending = nil
			}
		}

		var followUps []AgentMessage
		if config.GetFollowUpMessages != nil {
			followUps = config.GetFollowUpMessages()
		}
		if len(followUps) > 0 {
			pending = followUps
			continue
		}
		break
	}

	mustEmit(emit, AgentEvent{Type: EvAgentEnd, Messages: *newMessages})
}

func filterToolCalls(m *ai.AssistantMessage) []ai.ToolCall {
	var out []ai.ToolCall
	for _, c := range m.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			out = append(out, tc)
		}
	}
	return out
}

func streamAssistantResponse(ctx context.Context, agentCtx *AgentContext, config AgentLoopConfig, emit EventSink, streamFn StreamFn) *ai.AssistantMessage {
	messages := agentCtx.Messages
	if config.TransformContext != nil {
		messages = config.TransformContext(ctx, messages)
	}

	var llmMessages []ai.Message
	if config.ConvertToLlm != nil {
		llmMessages = config.ConvertToLlm(messages)
	} else {
		llmMessages = defaultConvertToLlm(messages)
	}

	llmCtx := ai.Context{
		SystemPrompt: agentCtx.SystemPrompt,
		Messages:     llmMessages,
		Tools:        toAITools(agentCtx.Tools),
	}

	fn := streamFn
	if fn == nil {
		fn = ai.StreamSimple
	}

	apiKey := config.APIKey
	if config.GetApiKey != nil {
		if k := config.GetApiKey(config.Model.Provider); k != "" {
			apiKey = k
		}
	}

	// pi spreads the whole config into the stream options (agent-loop.ts:304-308;
	// AgentLoopConfig extends SimpleStreamOptions), so every StreamOptions field
	// must be forwarded here.
	opts := &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{
			APIKey:                    apiKey,
			Transport:                 config.Transport,
			SessionID:                 config.SessionID,
			OnPayload:                 config.OnPayload,
			OnResponse:                config.OnResponse,
			MaxRetryDelayMs:           config.MaxRetryDelayMs,
			MaxRetries:                config.MaxRetries,
			TimeoutMs:                 config.TimeoutMs,
			WebSocketConnectTimeoutMs: config.WebSocketConnectTimeoutMs,
			Temperature:               config.Temperature,
			MaxTokens:                 config.MaxTokens,
			CacheRetention:            config.CacheRetention,
			Headers:                   config.Headers,
			Metadata:                  config.Metadata,
		},
		ThinkingBudgets: config.ThinkingBudgets,
	}
	if config.Reasoning != "" && config.Reasoning != "off" {
		opts.Reasoning = ai.ThinkingLevel(config.Reasoning)
	}

	response := fn(ctx, config.Model, llmCtx, opts)

	var partial *ai.AssistantMessage
	addedPartial := false

	for event := range response.Events() {
		switch event.Type {
		case ai.EventStart:
			partial = event.Partial
			agentCtx.Messages = append(agentCtx.Messages, partial)
			addedPartial = true
			mustEmit(emit, AgentEvent{Type: EvMessageStart, Message: partial.Clone()})

		case ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd,
			ai.EventThinkingStart, ai.EventThinkingDelta, ai.EventThinkingEnd,
			ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd:
			if partial != nil {
				partial = event.Partial
				agentCtx.Messages[len(agentCtx.Messages)-1] = partial
				ev := event
				mustEmit(emit, AgentEvent{Type: EvMessageUpdate, AssistantMessageEvent: &ev, Message: partial.Clone()})
			}

		case ai.EventDone, ai.EventError:
			final := response.Result()
			if addedPartial {
				agentCtx.Messages[len(agentCtx.Messages)-1] = final
			} else {
				agentCtx.Messages = append(agentCtx.Messages, final)
				mustEmit(emit, AgentEvent{Type: EvMessageStart, Message: final.Clone()})
			}
			mustEmit(emit, AgentEvent{Type: EvMessageEnd, Message: final})
			return final
		}
	}

	final := response.Result()
	if addedPartial {
		agentCtx.Messages[len(agentCtx.Messages)-1] = final
	} else {
		agentCtx.Messages = append(agentCtx.Messages, final)
		mustEmit(emit, AgentEvent{Type: EvMessageStart, Message: final.Clone()})
	}
	mustEmit(emit, AgentEvent{Type: EvMessageEnd, Message: final})
	return final
}

func toAITools(tools []AgentTool) []ai.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ai.Tool, len(tools))
	for i, t := range tools {
		out[i] = t.asAITool()
	}
	return out
}

func defaultConvertToLlm(messages []AgentMessage) []ai.Message {
	var out []ai.Message
	for _, m := range messages {
		switch m.MessageRole() {
		case ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

type executedBatch struct {
	messages  []ai.ToolResultMessage
	terminate bool
}

func findTool(tools []AgentTool, name string) (AgentTool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return AgentTool{}, false
}

func executeToolCalls(ctx context.Context, current *AgentContext, msg *ai.AssistantMessage, config AgentLoopConfig, emit EventSink) executedBatch {
	toolCalls := filterToolCalls(msg)
	hasSequential := false
	for _, tc := range toolCalls {
		if t, ok := findTool(current.Tools, tc.Name); ok && t.ExecutionMode == ToolSequential {
			hasSequential = true
			break
		}
	}
	if config.ToolExecution == ToolSequential || hasSequential {
		return executeToolCallsSequential(ctx, current, msg, toolCalls, config, emit)
	}
	return executeToolCallsParallel(ctx, current, msg, toolCalls, config, emit)
}

type finalizedOutcome struct {
	toolCall ai.ToolCall
	result   AgentToolResult
	isError  bool
}

func shouldTerminateBatch(calls []finalizedOutcome) bool {
	if len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if !c.result.Terminate {
			return false
		}
	}
	return true
}

func executeToolCallsSequential(ctx context.Context, current *AgentContext, msg *ai.AssistantMessage, toolCalls []ai.ToolCall, config AgentLoopConfig, emit EventSink) executedBatch {
	var finalized []finalizedOutcome
	var messages []ai.ToolResultMessage

	for _, tc := range toolCalls {
		mustEmit(emit, AgentEvent{Type: EvToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments})

		prep := prepareToolCall(ctx, current, msg, tc, config)
		var fo finalizedOutcome
		if prep.immediate != nil {
			fo = finalizedOutcome{toolCall: tc, result: prep.immediate.result, isError: prep.immediate.isError}
		} else {
			executed := executePreparedToolCall(ctx, *prep.prepared, emit)
			fo = finalizeExecutedToolCall(ctx, current, msg, *prep.prepared, executed, config)
		}

		emitToolExecutionEnd(fo, emit)
		trm := createToolResultMessage(fo)
		emitToolResultMessage(trm, emit)
		finalized = append(finalized, fo)
		messages = append(messages, trm)

		if aborted(ctx) {
			break
		}
	}

	return executedBatch{messages: messages, terminate: shouldTerminateBatch(finalized)}
}

func executeToolCallsParallel(ctx context.Context, current *AgentContext, msg *ai.AssistantMessage, toolCalls []ai.ToolCall, config AgentLoopConfig, emit EventSink) executedBatch {
	// pi runs the parallel tool batch as `Promise.all`: tool `execute` bodies
	// are concurrently *scheduled*, but JS never interleaves the synchronous
	// bodies of hooks (afterToolCall) or the shared-context mutations between
	// them. We preserve real parallelism for tool execution while serializing
	// everything that touches shared state — event emission, hook invocation,
	// and result/context mutation — under a single mutex. This keeps the emit
	// order and tool-result ordering identical to pi and is race-free.
	var serialMu sync.Mutex
	safeEmit := func(e AgentEvent) error {
		serialMu.Lock()
		defer serialMu.Unlock()
		return emit(e)
	}

	type slot struct {
		immediate *finalizedOutcome
		thunk     func() finalizedOutcome
	}
	slots := make([]slot, 0, len(toolCalls))

	for _, tc := range toolCalls {
		mustEmit(safeEmit, AgentEvent{Type: EvToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments})

		// prepareToolCall runs the BeforeToolCall hook; the prepare loop is
		// already sequential (matches pi), so Before hooks never interleave.
		prep := prepareToolCall(ctx, current, msg, tc, config)
		if prep.immediate != nil {
			fo := finalizedOutcome{toolCall: tc, result: prep.immediate.result, isError: prep.immediate.isError}
			emitToolExecutionEnd(fo, safeEmit)
			slots = append(slots, slot{immediate: &fo})
			if aborted(ctx) {
				break
			}
			continue
		}
		prepared := *prep.prepared
		slots = append(slots, slot{thunk: func() finalizedOutcome {
			// Tool execution runs in parallel, OUTSIDE the lock (pi's Promise.all).
			executed := executePreparedToolCall(ctx, prepared, safeEmit)
			// Finalization runs the AfterToolCall hook and reads/writes shared
			// context; serialize it so hook bodies cannot interleave (pi
			// single-thread). The critical section is func-scoped with a deferred
			// unlock so a PANICKING listener (or hook) cannot leak the mutex and
			// deadlock the other tool goroutines at wg.Wait. A listener ERROR
			// (non-panic) still propagates as emitPanic AFTER the unlock.
			var fo finalizedOutcome
			var emitErr error
			func() {
				serialMu.Lock()
				defer serialMu.Unlock()
				fo = finalizeExecutedToolCall(ctx, current, msg, prepared, executed, config)
				emitErr = emit(AgentEvent{
					Type:       EvToolExecutionEnd,
					ToolCallID: fo.toolCall.ID,
					ToolName:   fo.toolCall.Name,
					Result:     fo.result,
					IsError:    fo.isError,
				})
			}()
			if emitErr != nil {
				panic(emitPanic{err: emitErr})
			}
			return fo
		}})
		if aborted(ctx) {
			break
		}
	}

	ordered := make([]finalizedOutcome, len(slots))
	var wg sync.WaitGroup
	var panicOnce sync.Once
	var panicVal any
	for i, s := range slots {
		if s.immediate != nil {
			ordered[i] = *s.immediate
			continue
		}
		wg.Add(1)
		go func(i int, thunk func() finalizedOutcome) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicOnce.Do(func() { panicVal = r })
				}
			}()
			ordered[i] = thunk()
		}(i, s.thunk)
	}
	wg.Wait()
	// Re-raise any listener-error/panic from a tool goroutine on the loop
	// goroutine so it unwinds to the run boundary (matches pi rejecting the run).
	if panicVal != nil {
		panic(panicVal)
	}

	var messages []ai.ToolResultMessage
	for _, fo := range ordered {
		trm := createToolResultMessage(fo)
		emitToolResultMessage(trm, emit)
		messages = append(messages, trm)
	}

	return executedBatch{messages: messages, terminate: shouldTerminateBatch(ordered)}
}

type immediateOutcome struct {
	result  AgentToolResult
	isError bool
}

type preparedToolCall struct {
	toolCall ai.ToolCall
	tool     AgentTool
	args     map[string]any
}

type prepareResult struct {
	immediate *immediateOutcome
	prepared  *preparedToolCall
}

func prepareToolCall(ctx context.Context, current *AgentContext, msg *ai.AssistantMessage, tc ai.ToolCall, config AgentLoopConfig) (res prepareResult) {
	tool, ok := findTool(current.Tools, tc.Name)
	if !ok {
		return prepareResult{immediate: &immediateOutcome{result: errorToolResult("Tool " + tc.Name + " not found"), isError: true}}
	}

	// pi wraps prepareArguments/validate/beforeToolCall in try/catch; a throw
	// (panic) becomes an immediate error tool result, not a run failure
	// (agent-loop.ts:578-625). An emitPanic must still unwind to the run boundary.
	defer func() {
		if r := recover(); r != nil {
			if ep, ok := r.(emitPanic); ok {
				panic(ep)
			}
			res = prepareResult{immediate: &immediateOutcome{result: errorToolResult(panicMessage(r)), isError: true}}
		}
	}()

	prepared := tc
	if tool.PrepareArguments != nil {
		if newArgs := tool.PrepareArguments(tc.Arguments); newArgs != nil {
			prepared = ai.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: newArgs, ThoughtSignature: tc.ThoughtSignature}
		}
	}

	validated, err := ai.ValidateToolArguments(tool.asAITool(), prepared)
	if err != nil {
		return prepareResult{immediate: &immediateOutcome{result: errorToolResult(err.Error()), isError: true}}
	}

	if config.BeforeToolCall != nil {
		before := config.BeforeToolCall(ctx, BeforeToolCallContext{
			AssistantMessage: msg,
			ToolCall:         tc,
			Args:             validated,
			Context:          current,
		})
		if aborted(ctx) {
			return prepareResult{immediate: &immediateOutcome{result: errorToolResult("Operation aborted"), isError: true}}
		}
		if before != nil && before.Block {
			reason := before.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			return prepareResult{immediate: &immediateOutcome{result: errorToolResult(reason), isError: true}}
		}
	}
	if aborted(ctx) {
		return prepareResult{immediate: &immediateOutcome{result: errorToolResult("Operation aborted"), isError: true}}
	}
	return prepareResult{prepared: &preparedToolCall{toolCall: tc, tool: tool, args: validated}}
}

func executePreparedToolCall(ctx context.Context, prepared preparedToolCall, emit EventSink) immediateOutcome {
	// pi buffers tool_execution_update emit promises and awaits them only after
	// execute settles (agent-loop.ts:633-654): a listener error never interrupts
	// the tool mid-flight; it surfaces afterwards and rejects the run. We mirror
	// that by recording the first onUpdate emit error and re-raising it (as
	// emitPanic) once Execute has finished.
	var updateMu sync.Mutex
	var updateEmitErr error
	onUpdate := func(partial AgentToolResult) {
		err := emit(AgentEvent{
			Type:          EvToolExecutionUpdate,
			ToolCallID:    prepared.toolCall.ID,
			ToolName:      prepared.toolCall.Name,
			Args:          prepared.toolCall.Arguments,
			PartialResult: partial,
		})
		if err != nil {
			updateMu.Lock()
			if updateEmitErr == nil {
				updateEmitErr = err
			}
			updateMu.Unlock()
		}
	}

	// pi wraps execute in try/catch (agent-loop.ts:635-663): a throwing tool
	// yields an error tool result (text = the thrown error's message, matching
	// createErrorToolResult) and the loop CONTINUES. An emitPanic (listener
	// error) must still unwind to the run boundary, like the recovers in
	// prepareToolCall/finalizeExecutedToolCall.
	outcome := func() (out immediateOutcome) {
		defer func() {
			if r := recover(); r != nil {
				if ep, ok := r.(emitPanic); ok {
					panic(ep)
				}
				out = immediateOutcome{result: errorToolResult(panicMessage(r)), isError: true}
			}
		}()
		result, err := prepared.tool.Execute(ctx, prepared.toolCall.ID, prepared.args, onUpdate)
		if err != nil {
			return immediateOutcome{result: errorToolResult(err.Error()), isError: true}
		}
		return immediateOutcome{result: result, isError: false}
	}()

	// pi's `await Promise.all(updateEvents)` runs in both the try and catch
	// paths, so a listener rejection wins over the tool outcome either way.
	updateMu.Lock()
	emitErr := updateEmitErr
	updateMu.Unlock()
	if emitErr != nil {
		panic(emitPanic{err: emitErr})
	}
	return outcome
}

func finalizeExecutedToolCall(ctx context.Context, current *AgentContext, msg *ai.AssistantMessage, prepared preparedToolCall, executed immediateOutcome, config AgentLoopConfig) finalizedOutcome {
	result := executed.result
	isError := executed.isError

	if config.AfterToolCall != nil {
		// pi wraps afterToolCall in try/catch; a throw (panic) becomes an error
		// tool result, not a run failure (agent-loop.ts:676-701).
		func() {
			defer func() {
				if r := recover(); r != nil {
					if ep, ok := r.(emitPanic); ok {
						panic(ep)
					}
					result = errorToolResult(panicMessage(r))
					isError = true
				}
			}()
			after := config.AfterToolCall(ctx, AfterToolCallContext{
				AssistantMessage: msg,
				ToolCall:         prepared.toolCall,
				Args:             prepared.args,
				Result:           result,
				IsError:          isError,
				Context:          current,
			})
			if after != nil {
				if after.HasContent {
					result.Content = after.Content
				}
				if after.HasDetails {
					result.Details = after.Details
				}
				if after.Terminate != nil {
					result.Terminate = *after.Terminate
				}
				if after.IsError != nil {
					isError = *after.IsError
				}
			}
		}()
	}

	return finalizedOutcome{toolCall: prepared.toolCall, result: result, isError: isError}
}

func errorToolResult(message string) AgentToolResult {
	return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: message}}, Details: map[string]any{}}
}

func emitToolExecutionEnd(fo finalizedOutcome, emit EventSink) {
	mustEmit(emit, AgentEvent{
		Type:       EvToolExecutionEnd,
		ToolCallID: fo.toolCall.ID,
		ToolName:   fo.toolCall.Name,
		Result:     fo.result,
		IsError:    fo.isError,
	})
}

func createToolResultMessage(fo finalizedOutcome) ai.ToolResultMessage {
	return ai.ToolResultMessage{
		ToolCallID: fo.toolCall.ID,
		ToolName:   fo.toolCall.Name,
		Content:    fo.result.Content,
		Details:    fo.result.Details,
		IsError:    fo.isError,
		Timestamp:  nowMillis(),
	}
}

func emitToolResultMessage(trm ai.ToolResultMessage, emit EventSink) {
	mustEmit(emit, AgentEvent{Type: EvMessageStart, Message: trm})
	mustEmit(emit, AgentEvent{Type: EvMessageEnd, Message: trm})
}
