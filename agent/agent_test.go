package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

var testModel = &ai.Model{ID: "faux", Name: "faux", Api: "faux", Provider: "faux"}

// scriptedStream returns a StreamFn that emits one scripted assistant message
// per invocation, in order. Each message streams a start event then a terminal
// done/error event.
func scriptedStream(messages ...*ai.AssistantMessage) StreamFn {
	var mu sync.Mutex
	idx := 0
	return func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		mu.Lock()
		var msg *ai.AssistantMessage
		if idx < len(messages) {
			msg = messages[idx]
			idx++
		} else {
			msg = &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop}
		}
		mu.Unlock()

		s := ai.NewAssistantMessageEventStream()
		go func() {
			s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: &ai.AssistantMessage{}})
			if msg.StopReason == ai.StopError || msg.StopReason == ai.StopAborted {
				s.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: msg.StopReason, Error: msg})
			} else {
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: msg.StopReason, Message: msg})
			}
			s.End()
		}()
		return s
	}
}

func assistantWithToolCall(id, name string, args map[string]any) *ai.AssistantMessage {
	return &ai.AssistantMessage{
		Content:    ai.ContentList{ai.ToolCall{ID: id, Name: name, Arguments: args}},
		StopReason: ai.StopToolUse,
		Model:      "faux",
	}
}

func TestAgentRunsToolCallThenFinishes(t *testing.T) {
	var executed []string
	tool := AgentTool{
		Name:        "echo",
		Description: "echo input",
		Parameters:  ai.Object(ai.Prop("text", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			executed = append(executed, params["text"].(string))
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echoed: " + params["text"].(string)}}}, nil
		},
	}

	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: scriptedStream(
			assistantWithToolCall("c1", "echo", map[string]any{"text": "hi"}),
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "all done"}}, StopReason: ai.StopStop},
		),
	})

	var events []EventType
	var turnEndResults [][]ai.ToolResultMessage
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		events = append(events, e.Type)
		if e.Type == EvTurnEnd {
			turnEndResults = append(turnEndResults, e.ToolResults)
		}
		return nil
	})

	if err := a.Prompt(context.Background(), "please echo"); err != nil {
		t.Fatal(err)
	}

	if len(executed) != 1 || executed[0] != "hi" {
		t.Fatalf("tool not executed correctly: %v", executed)
	}

	st := a.State()
	// user, assistant(toolcall), toolResult, assistant(final)
	if len(st.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %#v", len(st.Messages), st.Messages)
	}
	if st.Messages[2].MessageRole() != ai.RoleToolResult {
		t.Fatalf("expected message[2] to be toolResult, got %s", st.Messages[2].MessageRole())
	}
	final, ok := asAssistant(st.Messages[3])
	if !ok || textOf(final) != "all done" {
		t.Fatalf("unexpected final message: %#v", st.Messages[3])
	}

	// Lifecycle events must include agent_start ... agent_end.
	if events[0] != EvAgentStart || events[len(events)-1] != EvAgentEnd {
		t.Fatalf("unexpected event boundaries: %v", events)
	}
	assertContains(t, events, EvToolExecutionStart)
	assertContains(t, events, EvToolExecutionEnd)

	// B7: tool turn carries 1 result; the final no-tool turn carries a non-nil
	// empty slice (pi initializes toolResults = [] every turn).
	if len(turnEndResults) != 2 {
		t.Fatalf("expected 2 turn_end events, got %d", len(turnEndResults))
	}
	if len(turnEndResults[0]) != 1 {
		t.Fatalf("expected 1 tool result on first turn_end, got %#v", turnEndResults[0])
	}
	if turnEndResults[1] == nil || len(turnEndResults[1]) != 0 {
		t.Fatalf("expected non-nil empty ToolResults on no-tool turn_end, got %#v", turnEndResults[1])
	}
}

func TestAgentErrorStopsLoop(t *testing.T) {
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: scriptedStream(
			&ai.AssistantMessage{StopReason: ai.StopError, ErrorMessage: "boom"},
		),
	})
	var turnEndResults [][]ai.ToolResultMessage
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvTurnEnd {
			turnEndResults = append(turnEndResults, e.ToolResults)
		}
		return nil
	})
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	st := a.State()
	if st.ErrorMessage != "boom" {
		t.Fatalf("expected error message 'boom', got %q", st.ErrorMessage)
	}
	// B7: the error-turn turn_end must carry [] (pi agent-loop.ts:197), not nil.
	if len(turnEndResults) != 1 {
		t.Fatalf("expected 1 turn_end, got %d", len(turnEndResults))
	}
	if turnEndResults[0] == nil || len(turnEndResults[0]) != 0 {
		t.Fatalf("expected non-nil empty ToolResults on error turn_end, got %#v", turnEndResults[0])
	}
}

func TestAgentBlockedToolViaBeforeHook(t *testing.T) {
	var ran bool
	tool := AgentTool{
		Name:       "danger",
		Parameters: ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			ran = true
			return AgentToolResult{}, nil
		},
	}
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: scriptedStream(
			assistantWithToolCall("c1", "danger", map[string]any{}),
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop},
		),
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			return &BeforeToolCallResult{Block: true, Reason: "nope"}
		},
	})
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("blocked tool should not have executed")
	}
	st := a.State()
	tr, _ := st.Messages[2].(ai.ToolResultMessage)
	if !tr.IsError {
		t.Fatal("expected blocked tool result to be an error")
	}
	if got := textOf(&ai.AssistantMessage{Content: tr.Content}); got != "nope" {
		t.Fatalf("expected block reason 'nope', got %q", got)
	}
}

func TestAgentParallelToolsTerminate(t *testing.T) {
	mkTool := func(name string) AgentTool {
		return AgentTool{
			Name:       name,
			Parameters: ai.Object(),
			Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
				return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: name}}, Terminate: true}, nil
			},
		}
	}
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{mkTool("a"), mkTool("b")}},
		StreamFn: scriptedStream(&ai.AssistantMessage{
			Content: ai.ContentList{
				ai.ToolCall{ID: "1", Name: "a", Arguments: map[string]any{}},
				ai.ToolCall{ID: "2", Name: "b", Arguments: map[string]any{}},
			},
			StopReason: ai.StopToolUse,
		}),
	})
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	st := a.State()
	// user, assistant(2 toolcalls), 2 toolResults — then terminate stops the loop.
	if len(st.Messages) != 4 {
		t.Fatalf("expected 4 messages after terminate, got %d", len(st.Messages))
	}
}

func TestAgentRejectsConcurrentPrompt(t *testing.T) {
	block := make(chan struct{})
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			s := ai.NewAssistantMessageEventStream()
			go func() {
				<-block
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Message: &ai.AssistantMessage{StopReason: ai.StopStop}})
				s.End()
			}()
			return s
		},
	})
	go a.Prompt(context.Background(), "first")
	// Wait until the run is active.
	for !a.State().IsStreaming {
	}
	err := a.Prompt(context.Background(), "second")
	if err == nil {
		t.Fatal("expected concurrent prompt to be rejected")
	}
	close(block)
	a.WaitForIdle()
}

// ---------------------------------------------------------------------------
// Task 1: throw-to-failure-turn
// ---------------------------------------------------------------------------

// TestAgentPanickingStreamFnEmitsFailureLifecycle mirrors pi agent.test.ts
// "emits full lifecycle events for thrown run failures": a streamFn that throws
// must yield the complete terminal sequence plus a synthetic assistant message
// with stopReason "error" and a non-empty errorMessage.
func TestAgentPanickingStreamFnEmitsFailureLifecycle(t *testing.T) {
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			panic(errors.New("provider exploded"))
		},
	})

	var events []EventType
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		events = append(events, e.Type)
		return nil
	})

	if err := a.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	want := []EventType{
		EvAgentStart, EvTurnStart,
		EvMessageStart, EvMessageEnd, // prompt
		EvMessageStart, EvMessageEnd, // synthetic failure message
		EvTurnEnd, EvAgentEnd,
	}
	if len(events) != len(want) {
		t.Fatalf("event count mismatch:\n got %v\nwant %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s (full: %v)", i, events[i], want[i], events)
		}
	}

	st := a.State()
	last, ok := asAssistant(st.Messages[len(st.Messages)-1])
	if !ok {
		t.Fatalf("expected last message to be assistant, got %#v", st.Messages[len(st.Messages)-1])
	}
	if last.StopReason != ai.StopError {
		t.Fatalf("expected stopReason error, got %q", last.StopReason)
	}
	if last.ErrorMessage != "provider exploded" {
		t.Fatalf("expected errorMessage 'provider exploded', got %q", last.ErrorMessage)
	}
	if st.ErrorMessage != "provider exploded" {
		t.Fatalf("expected state.ErrorMessage 'provider exploded', got %q", st.ErrorMessage)
	}
	if st.IsStreaming {
		t.Fatal("expected IsStreaming false after failure")
	}
}

// TestAgentPanickingTransformContextEmitsFailureLifecycle covers the
// transformContext throw path (pi handleRunFailure also fires for these).
func TestAgentPanickingTransformContextEmitsFailureLifecycle(t *testing.T) {
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn:     scriptedStream(&ai.AssistantMessage{StopReason: ai.StopStop}),
		TransformContext: func(ctx context.Context, messages []AgentMessage) []AgentMessage {
			panic("transform boom")
		},
	})
	var sawAgentEnd bool
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvAgentEnd {
			sawAgentEnd = true
		}
		return nil
	})
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !sawAgentEnd {
		t.Fatal("expected agent_end emitted after transformContext panic")
	}
	if a.State().ErrorMessage != "transform boom" {
		t.Fatalf("expected state error 'transform boom', got %q", a.State().ErrorMessage)
	}
}

// TestAgentPanickingHookProducesErrorToolResult pins pi's actual semantics: a
// throwing BeforeToolCall/AfterToolCall hook is caught locally and converted to
// an error tool result (agent-loop.ts:578-625, 676-701) — NOT a failure turn.
func TestAgentPanickingHookProducesErrorToolResult(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		tool := AgentTool{
			Name: "echo", Parameters: ai.Object(ai.Prop("text", ai.String())),
			Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
				return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ran"}}}, nil
			},
		}
		a := NewAgent(AgentOptions{
			InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
			StreamFn: scriptedStream(
				assistantWithToolCall("c1", "echo", map[string]any{"text": "hi"}),
				&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
			),
			BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
				panic(errors.New("before boom"))
			},
		})
		if err := a.Prompt(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		st := a.State()
		if st.ErrorMessage != "" {
			t.Fatalf("hook panic must not set run errorMessage, got %q", st.ErrorMessage)
		}
		tr, ok := st.Messages[2].(ai.ToolResultMessage)
		if !ok || !tr.IsError {
			t.Fatalf("expected error tool result at [2], got %#v", st.Messages[2])
		}
		if got := textOf(&ai.AssistantMessage{Content: tr.Content}); got != "before boom" {
			t.Fatalf("expected error text 'before boom', got %q", got)
		}
	})

	t.Run("after", func(t *testing.T) {
		tool := AgentTool{
			Name: "echo", Parameters: ai.Object(ai.Prop("text", ai.String())),
			Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
				return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ran"}}}, nil
			},
		}
		a := NewAgent(AgentOptions{
			InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
			StreamFn: scriptedStream(
				assistantWithToolCall("c1", "echo", map[string]any{"text": "hi"}),
				&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
			),
			AfterToolCall: func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult {
				panic("after boom")
			},
		})
		if err := a.Prompt(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		st := a.State()
		if st.ErrorMessage != "" {
			t.Fatalf("hook panic must not set run errorMessage, got %q", st.ErrorMessage)
		}
		tr, ok := st.Messages[2].(ai.ToolResultMessage)
		if !ok || !tr.IsError {
			t.Fatalf("expected error tool result at [2], got %#v", st.Messages[2])
		}
		if got := textOf(&ai.AssistantMessage{Content: tr.Content}); got != "after boom" {
			t.Fatalf("expected error text 'after boom', got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Task 2: serialized hooks with parallel tool execution (run under -race)
// ---------------------------------------------------------------------------

// TestAgentParallelToolsStatefulHooksRace runs many parallel tool calls whose
// Before/After hooks mutate shared state, asserting under -race that hook bodies
// never interleave and that emit/tool-result ordering still matches pi.
func TestAgentParallelToolsStatefulHooksRace(t *testing.T) {
	const n = 12
	// Shared, deliberately non-thread-safe accumulators only ever touched from
	// hook bodies. The loop must serialize hooks so these are race-free.
	var beforeCalls, afterCalls int
	sharedMap := map[string]bool{}

	tools := make([]AgentTool, n)
	var content ai.ContentList
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%d", i)
		tools[i] = AgentTool{
			Name: name, Parameters: ai.Object(),
			Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
				// Real concurrent work; touches no shared state.
				return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: id}}}, nil
			},
		}
		content = append(content, ai.ToolCall{ID: fmt.Sprintf("id%d", i), Name: name, Arguments: map[string]any{}})
	}

	a := NewAgent(AgentOptions{
		InitialState:  &AgentState{Model: testModel, Tools: tools},
		ToolExecution: ToolParallel,
		StreamFn: scriptedStream(
			&ai.AssistantMessage{Content: content, StopReason: ai.StopToolUse},
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
		),
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			beforeCalls++ // serialized: no race
			sharedMap[c.ToolCall.ID] = true
			return nil
		},
		AfterToolCall: func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult {
			afterCalls++ // serialized: no race
			delete(sharedMap, c.ToolCall.ID)
			return nil
		},
	})

	// Track tool_execution_end (completion order) and tool-result message order.
	var endIDs, resultIDs []string
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		switch e.Type {
		case EvToolExecutionEnd:
			endIDs = append(endIDs, e.ToolCallID)
		case EvMessageStart:
			if tr, ok := e.Message.(ai.ToolResultMessage); ok {
				resultIDs = append(resultIDs, tr.ToolCallID)
			}
		}
		return nil
	})

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	if beforeCalls != n || afterCalls != n {
		t.Fatalf("hook call counts: before=%d after=%d, want %d each", beforeCalls, afterCalls, n)
	}
	if len(sharedMap) != 0 {
		t.Fatalf("shared map not balanced: %v", sharedMap)
	}
	// tool-result messages must be emitted in source order (pi guarantee).
	if len(resultIDs) != n {
		t.Fatalf("expected %d tool-result messages, got %d", n, len(resultIDs))
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("id%d", i)
		if resultIDs[i] != want {
			t.Fatalf("tool-result order: [%d]=%s want %s (full %v)", i, resultIDs[i], want, resultIDs)
		}
	}
	// every tool_execution_end fired exactly once.
	if len(endIDs) != n {
		t.Fatalf("expected %d tool_execution_end, got %d", n, len(endIDs))
	}
}

// ---------------------------------------------------------------------------
// Task 3: listener error propagates to a failure turn
// ---------------------------------------------------------------------------

func TestAgentListenerErrorFailsRun(t *testing.T) {
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn:     scriptedStream(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop}),
	})

	var sawFailureTurn bool
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvTurnEnd {
			if am, ok := asAssistant(e.Message); ok && am.StopReason == ai.StopError {
				sawFailureTurn = true
			}
		}
		// Throw on the assistant message_end; this should unwind the run and
		// route to the failure turn (pi agent.ts:553-555). Skip the synthetic
		// failure message_end (it carries an errorMessage) to avoid recursion.
		if e.Type == EvMessageEnd {
			if am, ok := asAssistant(e.Message); ok && am.StopReason == ai.StopStop {
				return errors.New("listener kaboom")
			}
		}
		return nil
	})

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	st := a.State()
	if st.ErrorMessage != "listener kaboom" {
		t.Fatalf("expected run error 'listener kaboom', got %q", st.ErrorMessage)
	}
	if !sawFailureTurn {
		t.Fatal("expected a failure turn_end with stopReason error")
	}
	if st.IsStreaming {
		t.Fatal("expected IsStreaming false after listener failure")
	}
}

// ---------------------------------------------------------------------------
// Task 4: abort coverage
// ---------------------------------------------------------------------------

// TestAgentAbortMidStream aborts while the assistant response is streaming. The
// stream resolves to an aborted message; the loop must emit the terminal
// sequence and reach idle terminal state.
func TestAgentAbortMidStream(t *testing.T) {
	started := make(chan struct{})
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			s := ai.NewAssistantMessageEventStream()
			go func() {
				s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: &ai.AssistantMessage{}})
				s.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, Partial: &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "par"}}}})
				close(started)
				// Wait for abort, then resolve as aborted (the provider contract).
				<-ctx.Done()
				s.Push(ai.AssistantMessageEvent{
					Type:   ai.EventError,
					Reason: ai.StopAborted,
					Error:  &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "par"}}, StopReason: ai.StopAborted, ErrorMessage: "aborted"},
				})
				s.End()
			}()
			return s
		},
	})

	var sawAgentEnd, sawAbortedTurn bool
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvAgentEnd {
			sawAgentEnd = true
		}
		if e.Type == EvTurnEnd {
			if am, ok := asAssistant(e.Message); ok && am.StopReason == ai.StopAborted {
				sawAbortedTurn = true
			}
		}
		return nil
	})

	go func() {
		<-started
		a.Abort()
	}()
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	st := a.State()
	if !sawAgentEnd {
		t.Fatal("expected agent_end on abort-mid-stream")
	}
	if !sawAbortedTurn {
		t.Fatal("expected an aborted turn_end")
	}
	if st.IsStreaming {
		t.Fatal("expected not streaming after abort")
	}
	if len(st.PendingToolCalls) != 0 {
		t.Fatalf("expected pending tool calls cleared, got %v", st.PendingToolCalls)
	}
	// Partial assistant message recorded (the aborted final message).
	last, ok := asAssistant(st.Messages[len(st.Messages)-1])
	if !ok || last.StopReason != ai.StopAborted {
		t.Fatalf("expected aborted assistant final message, got %#v", st.Messages[len(st.Messages)-1])
	}
}

// TestAgentAbortDuringToolExecution aborts a run while a tool call is being
// prepared (via the BeforeToolCall hook). The call hit by the abort must get an
// "Operation aborted" error result and its Execute must NOT run; the loop then
// breaks (later calls get no result, matching pi's sequential break), emits
// agent_end, and reaches terminal state with cleared pending tool calls.
func TestAgentAbortDuringToolExecution(t *testing.T) {
	var ag *Agent
	var ranAny int32
	slowTool := AgentTool{
		Name: "slow", Parameters: ai.Object(),
		Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
			atomic.AddInt32(&ranAny, 1)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: id}}}, nil
		},
	}
	// Sequential so the abort lands deterministically during prepare of call a.
	ag = NewAgent(AgentOptions{
		InitialState:  &AgentState{Model: testModel, Tools: []AgentTool{slowTool}},
		ToolExecution: ToolSequential,
		StreamFn: scriptedStream(&ai.AssistantMessage{
			Content: ai.ContentList{
				ai.ToolCall{ID: "a", Name: "slow", Arguments: map[string]any{}},
				ai.ToolCall{ID: "b", Name: "slow", Arguments: map[string]any{}},
			},
			StopReason: ai.StopToolUse,
		}),
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			ag.Abort() // abort during preparation of the first call
			return nil
		},
	})

	var sawAgentEnd bool
	resultText := map[string]string{}
	ag.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvAgentEnd {
			sawAgentEnd = true
		}
		if e.Type == EvMessageStart {
			if tr, ok := e.Message.(ai.ToolResultMessage); ok {
				resultText[tr.ToolCallID] = textOf(&ai.AssistantMessage{Content: tr.Content})
			}
		}
		return nil
	})

	if err := ag.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	st := ag.State()
	if !sawAgentEnd {
		t.Fatal("expected agent_end after abort during tool execution")
	}
	if st.IsStreaming {
		t.Fatal("expected not streaming after abort")
	}
	if len(st.PendingToolCalls) != 0 {
		t.Fatalf("expected pending tool calls cleared, got %v", st.PendingToolCalls)
	}
	if atomic.LoadInt32(&ranAny) != 0 {
		t.Fatalf("aborted tool Execute must not run, ran %d times", ranAny)
	}
	// The call hit by the abort gets "Operation aborted"; later call b never
	// prepared (loop broke), so it has no result — matching pi.
	if resultText["a"] != "Operation aborted" {
		t.Fatalf("expected 'Operation aborted' for call a, got %q (all: %v)", resultText["a"], resultText)
	}
	if _, ok := resultText["b"]; ok {
		t.Fatalf("expected no result for not-reached call b, got %q", resultText["b"])
	}
}

// ---------------------------------------------------------------------------
// Parity sweep 2: A2 + B1-B6
// ---------------------------------------------------------------------------

// TestAgentParallelToolsPanickingListenerDoesNotDeadlock (A2): a listener that
// PANICS on tool_execution_end must not leak serialMu; the other tool goroutine
// must still finalize and the run must terminate as a failure turn, not hang at
// wg.Wait.
func TestAgentParallelToolsPanickingListenerDoesNotDeadlock(t *testing.T) {
	var barrier sync.WaitGroup
	barrier.Add(2)
	mkTool := func(name string) AgentTool {
		return AgentTool{
			Name: name, Parameters: ai.Object(),
			Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
				barrier.Done()
				barrier.Wait() // both tools in-flight before either finalizes
				return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: name}}}, nil
			},
		}
	}
	a := NewAgent(AgentOptions{
		InitialState:  &AgentState{Model: testModel, Tools: []AgentTool{mkTool("a"), mkTool("b")}},
		ToolExecution: ToolParallel,
		StreamFn: scriptedStream(&ai.AssistantMessage{
			Content: ai.ContentList{
				ai.ToolCall{ID: "1", Name: "a", Arguments: map[string]any{}},
				ai.ToolCall{ID: "2", Name: "b", Arguments: map[string]any{}},
			},
			StopReason: ai.StopToolUse,
		}),
	})

	var panicked int32
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvToolExecutionEnd && atomic.CompareAndSwapInt32(&panicked, 0, 1) {
			panic("listener exploded")
		}
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- a.Prompt(context.Background(), "go") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run deadlocked after listener panic (serialMu leaked)")
	}

	st := a.State()
	if st.ErrorMessage != "listener exploded" {
		t.Fatalf("expected failure turn with panic message, got %q", st.ErrorMessage)
	}
	if st.IsStreaming {
		t.Fatal("expected idle after failure turn")
	}
}

// TestAgentPanickingToolYieldsErrorResultAndContinues (B1): a panicking tool
// Execute yields an error tool result (text = the panic message, pi
// createErrorToolResult) and the loop CONTINUES to the next LLM call, with a
// matched tool_execution_start/end pair.
func TestAgentPanickingToolYieldsErrorResultAndContinues(t *testing.T) {
	for _, mode := range []ToolExecutionMode{ToolParallel, ToolSequential} {
		t.Run(string(mode), func(t *testing.T) {
			tool := AgentTool{
				Name: "bomb", Parameters: ai.Object(),
				Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
					panic(errors.New("tool exploded"))
				},
			}
			a := NewAgent(AgentOptions{
				InitialState:  &AgentState{Model: testModel, Tools: []AgentTool{tool}},
				ToolExecution: mode,
				StreamFn: scriptedStream(
					assistantWithToolCall("c1", "bomb", map[string]any{}),
					&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "recovered"}}, StopReason: ai.StopStop},
				),
			})

			var starts, ends []string
			a.Subscribe(func(ctx context.Context, e AgentEvent) error {
				switch e.Type {
				case EvToolExecutionStart:
					starts = append(starts, e.ToolCallID)
				case EvToolExecutionEnd:
					ends = append(ends, e.ToolCallID)
					if !e.IsError {
						t.Error("expected tool_execution_end IsError for panicked tool")
					}
				}
				return nil
			})

			if err := a.Prompt(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}

			st := a.State()
			if st.ErrorMessage != "" {
				t.Fatalf("tool panic must not fail the run, got error %q", st.ErrorMessage)
			}
			tr, ok := st.Messages[2].(ai.ToolResultMessage)
			if !ok || !tr.IsError {
				t.Fatalf("expected error tool result at [2], got %#v", st.Messages[2])
			}
			if got := textOf(&ai.AssistantMessage{Content: tr.Content}); got != "tool exploded" {
				t.Fatalf("expected error text 'tool exploded', got %q", got)
			}
			// Run continued to the next LLM call.
			final, ok := asAssistant(st.Messages[len(st.Messages)-1])
			if !ok || textOf(final) != "recovered" {
				t.Fatalf("expected run to continue to final message, got %#v", st.Messages[len(st.Messages)-1])
			}
			if len(starts) != 1 || len(ends) != 1 || starts[0] != "c1" || ends[0] != "c1" {
				t.Fatalf("expected matched start/end pair for c1, got starts=%v ends=%v", starts, ends)
			}
		})
	}
}

// TestAgentListenerErrorOnToolUpdateDefersUntilToolCompletes (B6, and B1's
// listener-error-inside-onUpdate case): a listener error on
// tool_execution_update must not interrupt the tool mid-flight; the tool
// finishes its work, then the buffered error fails the run (pi awaits the
// update-event promises after execute settles, agent-loop.ts:633-654).
func TestAgentListenerErrorOnToolUpdateDefersUntilToolCompletes(t *testing.T) {
	var completed int32
	tool := AgentTool{
		Name: "worker", Parameters: ai.Object(),
		Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
			u(AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "partial"}}})
			// Work after the failing update emit: must still run.
			atomic.StoreInt32(&completed, 1)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "finished"}}}, nil
		},
	}
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: scriptedStream(
			assistantWithToolCall("c1", "worker", map[string]any{}),
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
		),
	})
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvToolExecutionUpdate {
			return errors.New("update listener err")
		}
		return nil
	})

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	if atomic.LoadInt32(&completed) != 1 {
		t.Fatal("tool must complete its work despite update-listener error")
	}
	st := a.State()
	if st.ErrorMessage != "update listener err" {
		t.Fatalf("expected run failure 'update listener err', got %q", st.ErrorMessage)
	}
	if st.IsStreaming {
		t.Fatal("expected idle after failure turn")
	}
}

// TestAgentStateSnapshotPendingToolCallsRace (B2): State().PendingToolCalls is
// a copy-on-write snapshot; iterating it while parallel tools run must be
// race-free under -race.
func TestAgentStateSnapshotPendingToolCallsRace(t *testing.T) {
	const n = 8
	tools := make([]AgentTool, n)
	var content ai.ContentList
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%d", i)
		tools[i] = AgentTool{
			Name: name, Parameters: ai.Object(),
			Execute: func(ctx context.Context, id string, p map[string]any, u ToolUpdateFunc) (AgentToolResult, error) {
				time.Sleep(time.Millisecond)
				return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: id}}}, nil
			},
		}
		content = append(content, ai.ToolCall{ID: fmt.Sprintf("id%d", i), Name: name, Arguments: map[string]any{}})
	}
	a := NewAgent(AgentOptions{
		InitialState:  &AgentState{Model: testModel, Tools: tools},
		ToolExecution: ToolParallel,
		StreamFn: scriptedStream(
			&ai.AssistantMessage{Content: content, StopReason: ai.StopToolUse},
			&ai.AssistantMessage{Content: content, StopReason: ai.StopToolUse},
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
		),
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				st := a.State()
				count := 0
				for id := range st.PendingToolCalls {
					_ = id
					count++
				}
				if count > n {
					t.Errorf("pending tool calls exceeded %d: %d", n, count)
					return
				}
			}
		}
	}()

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	if got := len(a.State().PendingToolCalls); got != 0 {
		t.Fatalf("expected pending tool calls cleared, got %d", got)
	}
}

// TestAgentContinueConcurrentPromptDoesNotLoseDrainedMessages (B3): Continue
// claims the run slot before draining; if a concurrent Prompt wins the slot,
// the steering message stays queued — it is never silently dropped.
func TestAgentContinueConcurrentPromptDoesNotLoseDrainedMessages(t *testing.T) {
	for i := 0; i < 25; i++ {
		a := NewAgent(AgentOptions{
			InitialState: &AgentState{Model: testModel, Messages: []AgentMessage{
				ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}},
				&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "hello"}}, StopReason: ai.StopStop},
			}},
			StreamFn: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				return scriptedStream()(ctx, model, req, opts) // always a clean done turn
			},
		})
		a.Steer(ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "steer-me"}}})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = a.Continue(context.Background()) }()
		go func() { defer wg.Done(); _ = a.Prompt(context.Background(), "prompt") }()
		wg.Wait()
		a.WaitForIdle()

		inTranscript := false
		for _, m := range a.State().Messages {
			if um, ok := m.(ai.UserMessage); ok {
				for _, c := range um.Content {
					if tc, ok := c.(ai.TextContent); ok && strings.Contains(tc.Text, "steer-me") {
						inTranscript = true
					}
				}
			}
		}
		if !inTranscript && !a.HasQueuedMessages() {
			t.Fatalf("iteration %d: drained steering message lost (not in transcript, not queued)", i)
		}
	}
}

// TestAgentFailureEventListenerErrorStopsAndReturns (B4): pi awaits each
// failure event; a listener rejection stops the remaining failure events and
// rejects prompt(), while the finally still finishes the run.
func TestAgentFailureEventListenerErrorStopsAndReturns(t *testing.T) {
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			panic(errors.New("provider exploded"))
		},
	})

	var events []EventType
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		events = append(events, e.Type)
		// Error on the synthetic failure message_end (it carries an errorMessage).
		if e.Type == EvMessageEnd {
			if am, ok := asAssistant(e.Message); ok && am.ErrorMessage != "" {
				return errors.New("failure listener err")
			}
		}
		return nil
	})

	err := a.Prompt(context.Background(), "hi")
	if err == nil || err.Error() != "failure listener err" {
		t.Fatalf("expected prompt to return 'failure listener err', got %v", err)
	}
	// Remaining failure events (turn_end, agent_end) must not be emitted.
	for _, e := range events {
		if e == EvTurnEnd || e == EvAgentEnd {
			t.Fatalf("expected no turn_end/agent_end after failure-listener error, got %v", events)
		}
	}
	// State cleanup (finally) still happened: run finished, agent reusable.
	st := a.State()
	if st.IsStreaming {
		t.Fatal("expected IsStreaming false after failure-listener error")
	}
	a.WaitForIdle()
	if got := a.Prompt(context.Background(), "again"); got != nil && got.Error() == "Agent is already processing." {
		t.Fatal("run slot leaked after failure-listener error")
	}
}

// TestAgentForwardsMetadataAndWebSocketTimeout (B5): Metadata and
// WebSocketConnectTimeoutMs flow from AgentOptions through AgentLoopConfig into
// the stream options (pi AgentLoopConfig extends SimpleStreamOptions and is
// spread into the stream call).
func TestAgentForwardsMetadataAndWebSocketTimeout(t *testing.T) {
	inner := scriptedStream(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop})
	var got *ai.SimpleStreamOptions
	a := NewAgent(AgentOptions{
		InitialState:              &AgentState{Model: testModel},
		Metadata:                  map[string]any{"user_id": "u1"},
		WebSocketConnectTimeoutMs: 1234,
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.Context, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			got = opts
			return inner(ctx, model, req, opts)
		},
	})
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("stream options not captured")
	}
	if got.WebSocketConnectTimeoutMs != 1234 {
		t.Fatalf("WebSocketConnectTimeoutMs not forwarded, got %d", got.WebSocketConnectTimeoutMs)
	}
	if v, ok := got.Metadata["user_id"]; !ok || v != "u1" {
		t.Fatalf("Metadata not forwarded, got %#v", got.Metadata)
	}
}

func textOf(m *ai.AssistantMessage) string {
	for _, c := range m.Content {
		if t, ok := c.(ai.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

func assertContains(t *testing.T, events []EventType, want EventType) {
	t.Helper()
	for _, e := range events {
		if e == want {
			return
		}
	}
	t.Fatalf("expected events to contain %s, got %v", want, events)
}
