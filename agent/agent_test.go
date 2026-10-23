package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
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
	return func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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
	// system(tool declaration), user, assistant(toolcall), toolResult, assistant(final)
	if len(st.Messages) != 5 {
		t.Fatalf("expected 5 messages, got %d: %#v", len(st.Messages), st.Messages)
	}
	if st.Messages[3].MessageRole() != ai.RoleToolResult {
		t.Fatalf("expected message[3] to be toolResult, got %s", st.Messages[3].MessageRole())
	}
	final, ok := asAssistant(st.Messages[4])
	if !ok || textOf(final) != "all done" {
		t.Fatalf("unexpected final message: %#v", st.Messages[4])
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

// TestAgentAfterToolCallContentOverridePreservesResult verifies that an
// AfterToolCall hook overriding only content keeps the rest of the execute
// result (pi's `{...result, content}` spread preservation). Upstream 9e05370b2
// deleted addedToolNames, the field this test used to carry through the hook;
// details is the surviving field the spread preserves.
func TestAgentAfterToolCallContentOverridePreservesResult(t *testing.T) {
	tool := AgentTool{
		Name:        "loader",
		Description: "loads more tools",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{
				Content: ai.ContentList{ai.TextContent{Text: "loaded"}},
				Details: "after_load",
			}, nil
		},
	}
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: scriptedStream(
			assistantWithToolCall("c1", "loader", map[string]any{}),
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
		),
		// The after-hook only overrides content; details must be preserved.
		AfterToolCall: func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult {
			return &AfterToolCallResult{Content: ai.ContentList{ai.TextContent{Text: "loaded!"}}, HasContent: true}
		},
	})
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	st := a.State()
	// [0] is the leading system message declaring the initial tools.
	tr, ok := st.Messages[3].(ai.ToolResultMessage)
	if !ok {
		t.Fatalf("message[3] = %#v, want ToolResultMessage", st.Messages[3])
	}
	if tr.Details != "after_load" {
		t.Fatalf("Details = %v, want after_load", tr.Details)
	}
	if textOf(&ai.AssistantMessage{Content: tr.Content}) != "loaded!" {
		t.Fatalf("after-hook content override lost: %#v", tr.Content)
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
	// [0] is the leading system message declaring the initial tools.
	tr, _ := st.Messages[3].(ai.ToolResultMessage)
	if !tr.IsError {
		t.Fatal("expected blocked tool result to be an error")
	}
	if got := textOf(&ai.AssistantMessage{Content: tr.Content}); got != "nope" {
		t.Fatalf("expected block reason 'nope', got %q", got)
	}
}

// TestAgentBlockedToolTerminatesBatch mirrors pi agent-loop.test.ts "should
// stop after a blocked tool call when beforeToolCall sets terminate=true"
// (upstream 1eb988cfe). The blocked call is the whole batch, so the batch
// terminates and the loop never asks the model again.
func TestAgentBlockedToolTerminatesBatch(t *testing.T) {
	var ran bool
	var llmCalls atomic.Int32
	tool := AgentTool{
		Name:       "echo",
		Parameters: ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			ran = true
			return AgentToolResult{}, nil
		},
	}
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			n := llmCalls.Add(1)
			s := ai.NewAssistantMessageEventStream()
			go func() {
				msg := &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "should not run"}}, StopReason: ai.StopStop}
				if n == 1 {
					msg = assistantWithToolCall("tool-1", "echo", map[string]any{})
				}
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Message: msg})
				s.End()
			}()
			return s
		},
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			return &BeforeToolCallResult{Block: true, Reason: "Blocked by policy", Terminate: true}
		},
	})
	if err := a.Prompt(context.Background(), "echo something"); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("blocked tool should not have executed")
	}
	if got := llmCalls.Load(); got != 1 {
		t.Fatalf("llm calls = %d, want 1 (terminate should stop the loop)", got)
	}
	// [0] is the leading system message declaring the initial tools.
	tr, ok := a.State().Messages[3].(ai.ToolResultMessage)
	if !ok || !tr.IsError {
		t.Fatalf("expected an error tool result, got %#v", a.State().Messages[3])
	}
	if got := textOf(&ai.AssistantMessage{Content: tr.Content}); got != "Blocked by policy" {
		t.Fatalf("block reason = %q, want %q", got, "Blocked by policy")
	}
}

// TestAgentBlockedToolTerminateNeedsWholeBatch mirrors pi agent-loop.test.ts
// "should continue after a mixed batch with one terminating blocked call".
// This is the case that proves terminate joins the AND rule rather than
// unilaterally ending the batch: one blocked+terminate call beside one normal
// call must NOT stop the loop.
func TestAgentBlockedToolTerminateNeedsWholeBatch(t *testing.T) {
	var mu sync.Mutex
	var executed []string
	var llmCalls atomic.Int32
	tool := AgentTool{
		Name:       "echo",
		Parameters: ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			v, _ := params["value"].(string)
			mu.Lock()
			executed = append(executed, v)
			mu.Unlock()
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echoed: " + v}}}, nil
		},
	}
	a := NewAgent(AgentOptions{
		InitialState:  &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		ToolExecution: ToolParallel,
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			n := llmCalls.Add(1)
			s := ai.NewAssistantMessageEventStream()
			go func() {
				msg := &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop}
				if n == 1 {
					msg = &ai.AssistantMessage{
						Content: ai.ContentList{
							ai.ToolCall{ID: "tool-1", Name: "echo", Arguments: map[string]any{"value": "first"}},
							ai.ToolCall{ID: "tool-2", Name: "echo", Arguments: map[string]any{"value": "second"}},
						},
						StopReason: ai.StopToolUse,
					}
				}
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Message: msg})
				s.End()
			}()
			return s
		},
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			if v, _ := c.Args["value"].(string); v == "first" {
				return &BeforeToolCallResult{Block: true, Reason: "Blocked first", Terminate: true}
			}
			return nil
		},
	})
	if err := a.Prompt(context.Background(), "echo both"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := slices.Clone(executed)
	mu.Unlock()
	if !slices.Equal(got, []string{"second"}) {
		t.Fatalf("executed = %v, want [second]", got)
	}
	if n := llmCalls.Load(); n != 2 {
		t.Fatalf("llm calls = %d, want 2 (mixed batch must not terminate)", n)
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
	// system(tool declaration), user, assistant(2 toolcalls), 2 toolResults —
	// then terminate stops the loop.
	if len(st.Messages) != 5 {
		t.Fatalf("expected 5 messages after terminate, got %d", len(st.Messages))
	}
}

func TestAgentRejectsConcurrentPrompt(t *testing.T) {
	block := make(chan struct{})
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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

// TestAgentRejectsResetWhileProcessing mirrors pi agent.test.ts "should reject
// reset while processing without corrupting the transcript" (upstream
// 1532c9994). The point is not just the error: a refused reset must leave the
// in-flight transcript and streaming flag untouched, and the run must still
// settle normally afterwards.
func TestAgentRejectsResetWhileProcessing(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			s := ai.NewAssistantMessageEventStream()
			go func() {
				// Mirrors pi's streamStarted deferred: signal only once the
				// run is genuinely mid-stream, i.e. after the user message has
				// been committed to the transcript.
				close(started)
				<-block
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Message: &ai.AssistantMessage{
					Content:    ai.ContentList{ai.TextContent{Text: "Done"}},
					StopReason: ai.StopStop,
				}})
				s.End()
			}()
			return s
		},
	})

	go a.Prompt(context.Background(), "Hello")
	<-started

	if got := messageRoles(a.State().Messages); !slices.Equal(got, []string{"user"}) {
		t.Fatalf("roles before reset = %v, want [user]", got)
	}
	err := a.Reset()
	if err == nil {
		t.Fatal("expected Reset to be rejected while a run is active")
	}
	const want = "Agent is already processing. Wait for completion before resetting."
	if err.Error() != want {
		t.Fatalf("Reset error = %q, want %q", err.Error(), want)
	}
	// The refused reset must not have partially cleared state.
	if !a.State().IsStreaming {
		t.Fatal("refused Reset cleared IsStreaming")
	}
	if got := messageRoles(a.State().Messages); !slices.Equal(got, []string{"user"}) {
		t.Fatalf("roles after refused reset = %v, want [user]", got)
	}

	close(block)
	a.WaitForIdle()

	if a.State().IsStreaming {
		t.Fatal("still streaming after completion")
	}
	if got := messageRoles(a.State().Messages); !slices.Equal(got, []string{"user", "assistant"}) {
		t.Fatalf("roles after completion = %v, want [user assistant]", got)
	}
}

// TestAgentResetClearsWhenIdle pins the success path the guard must not break.
func TestAgentResetClearsWhenIdle(t *testing.T) {
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: scriptedStream(&ai.AssistantMessage{
			Content:    ai.ContentList{ai.TextContent{Text: "hi"}},
			StopReason: ai.StopStop,
		}),
	})
	if err := a.Prompt(context.Background(), "Hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if len(a.State().Messages) == 0 {
		t.Fatal("expected a transcript to clear")
	}
	if err := a.Reset(); err != nil {
		t.Fatalf("Reset while idle: %v", err)
	}
	if got := len(a.State().Messages); got != 0 {
		t.Fatalf("messages after reset = %d, want 0", got)
	}
}

func messageRoles(messages []AgentMessage) []string {
	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		switch m.(type) {
		case ai.SystemMessage:
			roles = append(roles, "system")
		case ai.UserMessage:
			roles = append(roles, "user")
		case *ai.AssistantMessage:
			roles = append(roles, "assistant")
		default:
			roles = append(roles, "other")
		}
	}
	return roles
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
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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
		// [0] is the leading system message declaring the initial tools.
		tr, ok := st.Messages[3].(ai.ToolResultMessage)
		if !ok || !tr.IsError {
			t.Fatalf("expected error tool result at [3], got %#v", st.Messages[3])
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
		// [0] is the leading system message declaring the initial tools.
		tr, ok := st.Messages[3].(ai.ToolResultMessage)
		if !ok || !tr.IsError {
			t.Fatalf("expected error tool result at [3], got %#v", st.Messages[3])
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
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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

// TestAgentParallelPreflightAbortStopsPreparedTools ports pi's #8935 regression
// (upstream afda4d620). In PARALLEL mode preparation is sequential but execution
// is deferred, so a call can be fully prepared before a LATER call's preflight
// aborts the run. Before the fix the prepared call still ran, which for a tool
// with an external side effect meant the write happened after the user aborted.
// Every prepared-but-unstarted call must instead yield "Operation aborted",
// without executing and without running AfterToolCall — while both calls still
// get a tool_execution_start, a tool_execution_end and a tool result.
func TestAgentParallelPreflightAbortStopsPreparedTools(t *testing.T) {
	var ag *Agent
	var mu sync.Mutex
	var executed, preflights, afterHooks []string

	externalWrite := AgentTool{
		Name:       "external_write",
		Parameters: ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			mu.Lock()
			executed = append(executed, id)
			mu.Unlock()
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: id}}}, nil
		},
	}

	ag = NewAgent(AgentOptions{
		// ToolExecution left at ToolDefault: this batch runs in parallel.
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{externalWrite}},
		StreamFn: scriptedStream(&ai.AssistantMessage{
			Content: ai.ContentList{
				ai.ToolCall{ID: "first", Name: "external_write", Arguments: map[string]any{}},
				ai.ToolCall{ID: "second", Name: "external_write", Arguments: map[string]any{}},
			},
			StopReason: ai.StopToolUse,
		}),
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			mu.Lock()
			preflights = append(preflights, c.ToolCall.ID)
			mu.Unlock()
			// "first" prepares cleanly and is queued; the abort lands on the
			// SECOND call's preflight, after the first is already prepared.
			if c.ToolCall.ID == "second" {
				ag.Abort()
			}
			return nil
		},
		AfterToolCall: func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult {
			mu.Lock()
			afterHooks = append(afterHooks, c.ToolCall.ID)
			mu.Unlock()
			return nil
		},
	})

	var starts, ends, resultIDs, resultTexts []string
	var endIsError []bool
	ag.Subscribe(func(ctx context.Context, e AgentEvent) error {
		switch e.Type {
		case EvToolExecutionStart:
			starts = append(starts, e.ToolCallID)
		case EvToolExecutionEnd:
			ends = append(ends, e.ToolCallID)
			endIsError = append(endIsError, e.IsError)
		case EvMessageStart:
			if tr, ok := e.Message.(ai.ToolResultMessage); ok {
				resultIDs = append(resultIDs, tr.ToolCallID)
				resultTexts = append(resultTexts, textOfContent(tr.Content))
			}
		}
		return nil
	})

	if err := ag.Prompt(context.Background(), "run both writes"); err != nil {
		t.Fatal(err)
	}

	// Both calls were preflighted; neither tool body ran and no result hook fired.
	if got := strings.Join(preflights, ","); got != "first,second" {
		t.Fatalf("expected both calls preflighted, got %q", got)
	}
	if len(executed) != 0 {
		t.Fatalf("aborted batch must not execute any tool, ran %v", executed)
	}
	if len(afterHooks) != 0 {
		t.Fatalf("AfterToolCall must not run for an aborted call, ran %v", afterHooks)
	}

	// Both calls still get a start, an end flagged isError, and a tool result.
	// starts are emitted by the prepare loop, so they are strictly ordered.
	if got := strings.Join(starts, ","); got != "first,second" {
		t.Fatalf("expected starts [first second], got %q", got)
	}
	// ends are compared as a SET, exactly as pi's #8935 does: the aborted call's
	// own end is emitted inline by the prepare loop, ahead of the deferred ones,
	// so end order is not start order in pi either (here: second, then first).
	sortedEnds := slices.Clone(ends)
	slices.Sort(sortedEnds)
	if got := strings.Join(sortedEnds, ","); got != "first,second" {
		t.Fatalf("expected an end for each start, got %v", ends)
	}
	if !slices.Equal(endIsError, []bool{true, true}) {
		t.Fatalf("expected every tool_execution_end flagged isError, got %v", endIsError)
	}
	// pi asserts the tool results carry the same ids, in the same order, as the
	// starts, and that each reads exactly "Operation aborted".
	if !slices.Equal(resultIDs, starts) {
		t.Fatalf("expected tool results %v to match starts %v", resultIDs, starts)
	}
	if !slices.Equal(resultTexts, []string{"Operation aborted", "Operation aborted"}) {
		t.Fatalf("expected both results to read 'Operation aborted', got %v", resultTexts)
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
			// [0] is the leading system message declaring the initial tools.
			tr, ok := st.Messages[3].(ai.ToolResultMessage)
			if !ok || !tr.IsError {
				t.Fatalf("expected error tool result at [3], got %#v", st.Messages[3])
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
			StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
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

// TestAgentIgnoresLateToolUpdates mirrors pi agent.test.ts "should ignore tool
// updates after the tool execution settles" (daab056a, #5573). A tool fires one
// in-flight update (which must emit) and leaks its onUpdate callback to a
// goroutine that fires AFTER Execute returns; the late call must be dropped, so
// exactly one tool_execution_update is emitted. Run under -race.
func TestAgentIgnoresLateToolUpdates(t *testing.T) {
	var captured ToolUpdateFunc
	tool := AgentTool{
		Name:        "delayed_tool",
		Description: "captures progress callbacks",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, p map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			captured = onUpdate
			onUpdate(AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "running"}}})
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Terminate: true}, nil
		},
	}

	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: scriptedStream(
			assistantWithToolCall("call-1", "delayed_tool", map[string]any{}),
		),
	})

	var mu sync.Mutex
	var updateCount int
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		if e.Type == EvToolExecutionUpdate {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		return nil
	})

	if err := a.Prompt(context.Background(), "run tool"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	afterPrompt := updateCount
	mu.Unlock()
	if afterPrompt != 1 {
		t.Fatalf("expected exactly 1 tool_execution_update during execute, got %d", afterPrompt)
	}

	// Fire the captured callback after Execute has long since returned: it must
	// be dropped (guarded by acceptingUpdates under the update mutex).
	if captured == nil {
		t.Fatal("tool did not capture its onUpdate callback")
	}
	captured(AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "late"}}})

	mu.Lock()
	final := updateCount
	mu.Unlock()
	if final != 1 {
		t.Fatalf("late onUpdate must be ignored: expected 1 update, got %d", final)
	}
}

// Mirrors pi's "should not execute tool calls from a length-truncated
// assistant message" (agent-loop.test.ts, upstream 351efc82). The salvage
// parser can yield arguments that validate but are silently truncated, so a
// StopLength turn must fail every tool call instead of running it, and the loop
// must continue so the model can re-issue.
func TestAgentFailsToolCallsFromLengthTruncatedMessage(t *testing.T) {
	var executed []string
	tool := AgentTool{
		Name:        "echo",
		Description: "echo input",
		Parameters:  ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			executed = append(executed, params["value"].(string))
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echoed"}}}, nil
		},
	}

	// Output hit the token limit mid tool call: arguments may be truncated. Two
	// calls, because the contract is that EVERY call in the message fails.
	truncated := assistantWithToolCall("tool-1", "echo", map[string]any{"value": "hel"})
	truncated.Content = append(truncated.Content, ai.ToolCall{ID: "tool-2", Name: "echo", Arguments: map[string]any{"value": "wor"}})
	truncated.StopReason = ai.StopLength

	var streamCalls int32
	scripted := scriptedStream(
		truncated,
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
	)
	counting := func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		atomic.AddInt32(&streamCalls, 1)
		return scripted(ctx, model, req, opts)
	}

	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn:     counting,
	})

	// The per-call event sequence, in emission order.
	type endEvent struct {
		toolCallID string
		isError    bool
		text       string
	}
	var ends []endEvent
	var seq []EventType
	a.Subscribe(func(ctx context.Context, e AgentEvent) error {
		switch e.Type {
		case EvToolExecutionStart, EvToolExecutionEnd:
			seq = append(seq, e.Type)
		}
		if e.Type == EvToolExecutionEnd {
			ev := endEvent{toolCallID: e.ToolCallID, isError: e.IsError}
			if r, ok := e.Result.(AgentToolResult); ok {
				for _, c := range r.Content {
					if tc, ok := c.(ai.TextContent); ok {
						ev.text = tc.Text
					}
				}
			}
			ends = append(ends, ev)
		}
		return nil
	})

	if err := a.Prompt(context.Background(), "echo something"); err != nil {
		t.Fatal(err)
	}

	// Neither tool may execute with potentially truncated arguments.
	if len(executed) != 0 {
		t.Fatalf("tool must not execute on a length-truncated message, got %v", executed)
	}

	// Every tool call in the message fails, in order.
	if len(ends) != 2 {
		t.Fatalf("expected 2 tool_execution_end events, got %d: %#v", len(ends), ends)
	}
	// Byte-exact against pi's message (agent-loop.ts failToolCallsFromTruncatedMessage).
	want := `Tool call "echo" was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.`
	for i, wantID := range []string{"tool-1", "tool-2"} {
		if ends[i].toolCallID != wantID {
			t.Fatalf("tool_execution_end[%d]: got toolCallID %q, want %q", i, ends[i].toolCallID, wantID)
		}
		if !ends[i].isError {
			t.Fatalf("tool_execution_end[%d]: expected the failed tool call to be marked isError", i)
		}
		if ends[i].text != want {
			t.Fatalf("tool_execution_end[%d] text mismatch:\n got: %q\nwant: %q", i, ends[i].text, want)
		}
	}
	// pi emits start->end per call, sequentially, not start,start,end,end.
	wantSeq := []EventType{EvToolExecutionStart, EvToolExecutionEnd, EvToolExecutionStart, EvToolExecutionEnd}
	if len(seq) != len(wantSeq) {
		t.Fatalf("unexpected tool event sequence: %v", seq)
	}
	for i := range wantSeq {
		if seq[i] != wantSeq[i] {
			t.Fatalf("tool event sequence mismatch:\n got: %v\nwant: %v", seq, wantSeq)
		}
	}

	// The loop continues so the model can re-issue the tool calls.
	if got := atomic.LoadInt32(&streamCalls); got != 2 {
		t.Fatalf("expected the loop to continue (2 stream calls), got %d", got)
	}

	st := a.State()
	// system(tool declaration), user, assistant(truncated toolcalls),
	// toolResult x2 (error), assistant(final)
	if len(st.Messages) != 6 {
		t.Fatalf("expected 6 messages, got %d: %#v", len(st.Messages), st.Messages)
	}
	for _, i := range []int{3, 4} {
		trm, ok := st.Messages[i].(ai.ToolResultMessage)
		if !ok || !trm.IsError {
			t.Fatalf("expected an error tool result at message[%d], got %#v", i, st.Messages[i])
		}
	}
	if final, ok := asAssistant(st.Messages[5]); !ok || textOf(final) != "done" {
		t.Fatalf("expected the run to end on an assistant message, got %#v", st.Messages[5])
	}
}

func textOf(m *ai.AssistantMessage) string { return textOfContent(m.Content) }

func assertContains(t *testing.T, events []EventType, want EventType) {
	t.Helper()
	for _, e := range events {
		if e == want {
			return
		}
	}
	t.Fatalf("expected events to contain %s, got %v", want, events)
}

// Upstream 027a5847: pi's AgentLoopConfig extends SimpleStreamOptions and the
// whole config is spread into the stream call, so an injected HTTP client is
// reachable from the agent layer. The Go loop forwards StreamOptions field by
// field, so the new field needs its own check.
func TestAgentForwardsHTTPClientToStreamOptions(t *testing.T) {
	client := &http.Client{}
	var got ai.HTTPDoer
	var seen atomic.Bool

	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		HTTPClient:   client,
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			got = opts.HTTPClient
			seen.Store(true)
			return scriptedStream()(ctx, model, req, opts)
		},
	})

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !seen.Load() {
		t.Fatal("stream fn never ran")
	}
	if got != ai.HTTPDoer(client) {
		t.Fatalf("HTTPClient not forwarded to stream options: got %#v", got)
	}
}

// TestAgentForwardsProviderStreamEventObservers transliterates upstream's
// 'forwards provider stream event observers through AgentOptions' (002fc8385):
// a stream function that invokes the stream options' observer reaches the one
// given to AgentOptions.
func TestAgentForwardsProviderStreamEventObservers(t *testing.T) {
	var providerEvents []any
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		OnProviderStreamEvent: func(data any, model *ai.Model) error {
			providerEvents = append(providerEvents, data)
			return nil
		},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			s := ai.NewAssistantMessageEventStream()
			go func() {
				if opts.OnProviderStreamEvent != nil {
					if err := opts.OnProviderStreamEvent(map[string]any{"request_cost": 0.01}, model); err != nil {
						t.Errorf("observer: %v", err)
					}
				}
				msg := &ai.AssistantMessage{
					Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Api: model.Api, Provider: model.Provider,
					Model: model.ID, StopReason: ai.StopStop, Timestamp: 1,
				}
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: ai.StopStop, Message: msg})
				s.End()
			}()
			return s
		},
	})

	if err := a.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"request_cost": 0.01}}
	if !reflect.DeepEqual(providerEvents, want) {
		t.Fatalf("provider events = %v, want %v", providerEvents, want)
	}
}

// TestAgentFinishTurnObservesAbort locks the run's cancellation reaching the
// Agent-level FinishTurn hook. pi hands the loop's signal straight to finishTurn
// (agent-loop.ts, types.ts FinishTurn), so async work between turns — e.g.
// compaction or a token probe — can observe Abort() instead of blocking teardown.
func TestAgentFinishTurnObservesAbort(t *testing.T) {
	tool := AgentTool{
		Name:        "noop",
		Description: "Noop tool",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "tool complete"}}}, nil
		},
	}

	scripted := scriptedStream(
		assistantWithToolCall("tool-1", "noop", map[string]any{}),
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "should not run"}}, StopReason: ai.StopStop},
	)

	var a *Agent
	hookCtxErr := make(chan error, 1)
	var hookOnce sync.Once
	a = NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn:     scripted,
		FinishTurn: func(ctx context.Context, c AgentTurnContext) AgentTurnDecision {
			// Abort mid-hook, exactly like async between-turn work that is
			// cancelled while it runs. Without the run ctx the hook has no way
			// to see it and would block teardown.
			// FinishTurn also runs for the aborted response that can follow,
			// so only the first call reports.
			hookOnce.Do(func() {
				a.Abort()
				select {
				case <-ctx.Done():
					hookCtxErr <- ctx.Err()
				case <-time.After(2 * time.Second):
					hookCtxErr <- nil
				}
			})
			return TurnEnd
		},
	})

	_ = a.Prompt(context.Background(), "start")

	select {
	case err := <-hookCtxErr:
		if err == nil {
			t.Fatal("FinishTurn never observed the run's cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hook ctx error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FinishTurn was never called")
	}
}

// TestAgentForwardsFinishTurn locks the Agent-level FinishTurn option reaching
// the loop config: TurnEnd stops the run after the first turn completes (tool
// included), before a second provider request is made.
func TestAgentForwardsFinishTurn(t *testing.T) {
	tool := AgentTool{
		Name:        "noop",
		Description: "Noop tool",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "tool complete"}}}, nil
		},
	}

	var requests int32
	scripted := scriptedStream(
		assistantWithToolCall("tool-1", "noop", map[string]any{}),
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "should not run"}}, StopReason: ai.StopStop},
	)

	var roles []ai.Role
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			atomic.AddInt32(&requests, 1)
			return scripted(ctx, model, req, opts)
		},
		FinishTurn: func(_ context.Context, c AgentTurnContext) AgentTurnDecision {
			for _, m := range c.Context.Messages {
				roles = append(roles, m.MessageRole())
			}
			return TurnEnd
		},
	})

	if err := a.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("expected 1 provider request, got %d", got)
	}
	// The initial tools are declared by the leading system message.
	want := []ai.Role{ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult}
	if len(roles) != len(want) {
		t.Fatalf("callback saw roles %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("callback saw roles %v, want %v", roles, want)
		}
	}
}

// promptCtxKey tags the context a caller hands Agent.Prompt. Identity does not
// survive the trip — runWithLifecycle gives the loop a derived, abort-cancellable
// child — so the tag is what lets an assertion tell that child apart from a
// context substituted somewhere along the way.
type promptCtxKey struct{}

// TestAgentBuildsProviderContext pins the transform → convert → Context pipeline
// every request runs through (agent-loop.ts:288-302, upstream 3a0b9a3ee). The
// transform is the seam compaction hangs off in the coding agent, so all three
// of its inputs and its position matter: it runs first, on the live transcript,
// under the run's context — which is what carries an abort into a transform that
// calls the provider itself — and convert is then fed its output rather than the
// raw transcript. The system prompt and tools ride the transcript's leading
// system message, which this transform keeps.
func TestAgentBuildsProviderContext(t *testing.T) {
	tool := AgentTool{
		Name:       "echo",
		Parameters: ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{}, nil
		},
	}
	kept := ai.NewUserText("kept", 7)
	reply := scriptedStream(&ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop,
	})

	var callOrder []string
	var transformInput, convertInput []AgentMessage
	var transformCtx context.Context
	var got ai.TranscriptContext

	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "System prompt", Model: testModel, Tools: []AgentTool{tool}},
		TransformContext: func(ctx context.Context, messages []AgentMessage) []AgentMessage {
			callOrder = append(callOrder, "transform")
			transformCtx = ctx
			transformInput = messages
			return []AgentMessage{messages[0], kept}
		},
		ConvertToLlm: func(messages []AgentMessage) []ai.Message {
			callOrder = append(callOrder, "convert")
			convertInput = messages
			return defaultConvertToLlm(messages)
		},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			got = req
			return reply(ctx, model, req, opts)
		},
	})

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), promptCtxKey{}, "caller"))
	defer cancel()
	if err := a.Prompt(ctx, "discard"); err != nil {
		t.Fatal(err)
	}

	// The transform's context must still carry the run's cancellation, not just
	// its values: compaction calls the provider from inside the transform, so an
	// abort has to reach it.
	cancel()
	if transformCtx.Err() == nil {
		t.Fatal("transform context must cancel with the run")
	}

	if len(callOrder) != 2 || callOrder[0] != "transform" || callOrder[1] != "convert" {
		t.Fatalf("want transform then convert, got %v", callOrder)
	}
	if len(transformInput) != 2 || transformInput[0].MessageRole() != ai.RoleSystem || userText(transformInput[1]) != "discard" {
		t.Fatalf("transform must see the current transcript, got %v", transformInput)
	}
	if transformCtx == nil || transformCtx.Value(promptCtxKey{}) != "caller" {
		t.Fatal("transform must run under a context derived from the caller's")
	}
	if len(convertInput) != 2 || userText(convertInput[1]) != "kept" {
		t.Fatalf("convert must see the transform's output, got %v", convertInput)
	}
	// The provider receives a normalized transcript: the prompt and tools ride
	// its leading system message.
	leading, ok := ai.GetInitialSystemMessage(got.Messages)
	if !ok || ai.GetSystemMessageText(leading) != "System prompt" {
		t.Fatalf("system prompt must reach the provider verbatim, got %v", got.Messages)
	}
	conversation := ai.WithoutInitialSystemMessage(got.Messages)
	if len(conversation) != 1 || userText(conversation[0]) != "kept" {
		t.Fatalf("want the transformed transcript on the wire, got %v", got.Messages)
	}
	if tools := ai.GetCurrentTools(got.Messages); len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("context tools must carry the agent's tools, got %v", tools)
	}
}

// TestAgentLoopDefaultConvertToLlm covers the Go-only arm of the same block: pi
// requires convertToLlm on AgentLoopConfig (types.ts:178), while the Go loop
// falls back to defaultConvertToLlm — and the fallback must still be fed the
// transformed messages, system messages included (agent.ts defaultConvertToLlm,
// upstream 9e05370b2). Only reachable through AgentLoop; NewAgent fills the
// hook in.
func TestAgentLoopDefaultConvertToLlm(t *testing.T) {
	reply := scriptedStream(&ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop,
	})

	var got ai.TranscriptContext
	stream := AgentLoop(context.Background(),
		[]AgentMessage{ai.NewUserText("discard", 1)},
		AgentContext{},
		AgentLoopConfig{
			Model: testModel,
			TransformContext: func(ctx context.Context, messages []AgentMessage) []AgentMessage {
				return []AgentMessage{ai.NewSystemText("sp", 0), ai.NewUserText("kept", 7)}
			},
		},
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			got = req
			return reply(ctx, model, req, opts)
		})
	stream.Result()

	// The leading system message carries the "sp" prompt; the conversation
	// after it is the transformed transcript.
	if leading, ok := ai.GetInitialSystemMessage(got.Messages); !ok || ai.GetSystemMessageText(leading) != "sp" {
		t.Fatalf("default conversion must keep the transformed system message, got %v", got.Messages)
	}
	if conversation := ai.WithoutInitialSystemMessage(got.Messages); len(conversation) != 1 || userText(conversation[0]) != "kept" {
		t.Fatalf("default conversion must run on the transformed messages, got %v", got.Messages)
	}
}

// userText returns the first text block of a user message, or "".
func userText(m ai.Message) string {
	if um, ok := m.(ai.UserMessage); ok {
		return textOfContent(um.Content)
	}
	return ""
}

// textOfContent returns the first text block, the shared half of textOf.
func textOfContent(content ai.ContentList) string {
	for _, c := range content {
		if tc, ok := c.(ai.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// queuedTexts renders PeekQueuedMessages as the first text of each message.
func queuedTexts(a *Agent) []string {
	var out []string
	for _, m := range a.PeekQueuedMessages() {
		if u, ok := m.(ai.UserMessage); ok {
			out = append(out, textOfContent(u.Content))
		}
	}
	return out
}

// assertQueuesKept checks that the steering message queued during the run and
// the follow-up queued before it both survived, steering first.
func assertQueuesKept(t *testing.T, a *Agent) {
	t.Helper()
	if got := queuedTexts(a); !slices.Equal(got, []string{"steering"}) {
		t.Fatalf("peek = %v, want the kept steering message", got)
	}
	a.ClearSteeringQueue()
	if got := queuedTexts(a); !slices.Equal(got, []string{"follow-up"}) {
		t.Fatalf("peek after clearing steering = %v, want the kept follow-up", got)
	}
}

// steerOnAssistantEnd queues a steering message once the first assistant
// message of the run has completed, as a user typing during the response would.
func steerOnAssistantEnd(a *Agent) {
	var once sync.Once
	a.Subscribe(func(_ context.Context, e AgentEvent) error {
		if _, ok := asAssistant(e.Message); ok && e.Type == EvMessageEnd {
			once.Do(func() { a.Steer(ai.NewUserText("steering", 0)) })
		}
		return nil
	})
}

// agent.test.ts "previews the next selected queued messages without consuming
// them".
func TestAgentPeekQueuedMessagesDoesNotConsume(t *testing.T) {
	a := NewAgent(AgentOptions{SteeringMode: QueueOneAtATime, FollowUpMode: QueueAll, StreamFn: unusedStreamFn(t)})
	a.Steer(ai.NewUserText("first steering", 0))
	a.Steer(ai.NewUserText("second steering", 0))
	a.FollowUp(ai.NewUserText("follow-up", 0))
	a.FollowUp(ai.NewUserText("second follow-up", 0))

	for range 2 {
		if got := queuedTexts(a); !slices.Equal(got, []string{"first steering"}) {
			t.Fatalf("peek = %v, want [first steering]", got)
		}
	}
	a.ClearSteeringQueue()
	if got := queuedTexts(a); !slices.Equal(got, []string{"follow-up", "second follow-up"}) {
		t.Fatalf("peek = %v, want the whole all-mode follow-up queue", got)
	}
	if got := queuedTexts(a); len(got) != 2 {
		t.Fatalf("second all-mode peek = %v, want both follow-ups still queued", got)
	}
}

// agent.test.ts "rejects a queued continuation from $name context without
// draining queues".
func TestAgentContinueFromNothingKeepsQueues(t *testing.T) {
	for name, messages := range map[string][]AgentMessage{
		"empty":       nil,
		"system-only": {ai.NewSystemText("system only", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			a := NewAgent(AgentOptions{InitialState: &AgentState{Messages: messages}, StreamFn: unusedStreamFn(t)})
			a.Steer(ai.NewUserText("steering", 0))
			a.FollowUp(ai.NewUserText("follow-up", 0))

			if err := a.Continue(context.Background()); err == nil || err.Error() != "No messages to continue from" {
				t.Fatalf("Continue error = %v, want No messages to continue from", err)
			}
			assertQueuesKept(t, a)
		})
	}
}

// agent.test.ts "keeps queues when finishTurn ends the run".
func TestAgentFinishTurnEndKeepsQueues(t *testing.T) {
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		FinishTurn:   func(context.Context, AgentTurnContext) AgentTurnDecision { return TurnEnd },
		StreamFn:     scriptedStream(textMessage("done")),
	})
	a.FollowUp(ai.NewUserText("follow-up", 0))
	steerOnAssistantEnd(a)

	if err := a.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	assertQueuesKept(t, a)
}

// agent.test.ts "keeps queues on a %s response even when finishTurn requests
// continuation".
func TestAgentFailedResponseKeepsQueuesDespiteContinue(t *testing.T) {
	for _, reason := range []ai.StopReason{ai.StopError, ai.StopAborted} {
		t.Run(string(reason), func(t *testing.T) {
			a := NewAgent(AgentOptions{
				InitialState: &AgentState{Model: testModel},
				FinishTurn:   func(context.Context, AgentTurnContext) AgentTurnDecision { return TurnContinue },
				StreamFn:     scriptedStream(&ai.AssistantMessage{StopReason: reason, ErrorMessage: string(reason)}),
			})
			a.FollowUp(ai.NewUserText("follow-up", 0))
			steerOnAssistantEnd(a)

			_ = a.Prompt(context.Background(), "start")
			assertQueuesKept(t, a)
		})
	}
}

// recordUserTexts is a StreamFn that answers "done" and records the user texts
// of each request — the upstream continue() tests' `requests` array.
func recordUserTexts(requests *[][]string) StreamFn {
	var mu sync.Mutex
	return func(_ context.Context, _ *ai.Model, req ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		mu.Lock()
		*requests = append(*requests, requestUserTexts(req))
		mu.Unlock()
		return replyWith(textMessage("done"))
	}
}

// assertQueuedInOrder checks a continuation that queued first and second: the
// first request carries first and, when both are drained together, second;
// otherwise second waits for a request of its own.
func assertQueuedInOrder(t *testing.T, requests [][]string, first, second string, together bool) {
	t.Helper()
	wantRequests := 2
	if together {
		wantRequests = 1
	}
	if len(requests) != wantRequests {
		t.Fatalf("requests = %v, want %d", requests, wantRequests)
	}
	if !slices.Contains(requests[0], first) {
		t.Fatalf("first request %v lacks %q", requests[0], first)
	}
	if together {
		if !slices.Contains(requests[0], second) {
			t.Fatalf("first request %v lacks %q", requests[0], second)
		}
		return
	}
	if slices.Contains(requests[0], second) {
		t.Fatalf("first request %v already carries %q", requests[0], second)
	}
	if !slices.Contains(requests[1], second) {
		t.Fatalf("second request %v lacks %q", requests[1], second)
	}
}

// agent.test.ts "continue() keeps $mode steering semantics for assistant-tail
// fallback". Characterization (the port already matched pi); seen red under
// two mutations of Continue's assistant-tail path: dropping
// `skipInitialSteeringPoll = true` fails one-at-a-time (the startup poll hands
// over "Steering 2" too), and draining a single steering message whatever the
// mode fails all (two requests).
func TestAgentContinueFromAssistantTailKeepsSteeringMode(t *testing.T) {
	for _, tc := range []struct {
		mode     QueueMode
		together bool
	}{{QueueOneAtATime, false}, {QueueAll, true}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			var requests [][]string
			a := NewAgent(AgentOptions{
				InitialState: &AgentState{Model: testModel},
				SteeringMode: tc.mode,
				StreamFn:     recordUserTexts(&requests),
			})
			a.SetMessages([]AgentMessage{ai.NewUserText("Initial", 1), textMessage("Initial response")})
			a.Steer(ai.NewUserText("Steering 1", 2))
			a.Steer(ai.NewUserText("Steering 2", 3))

			if err := a.Continue(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertQueuedInOrder(t, requests, "Steering 1", "Steering 2", tc.together)
		})
	}
}

// agent.test.ts "polls $mode steering at continuation startup". From a
// non-assistant tail the steering reaches the first request only through the
// loop's startup poll. Characterization; seen red in both modes with runLoop's
// startup GetSteeringMessages poll removed (the first request lacks "first").
func TestAgentContinuePollsSteeringAtStartup(t *testing.T) {
	for _, tc := range []struct {
		mode     QueueMode
		together bool
	}{{QueueOneAtATime, false}, {QueueAll, true}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			var requests [][]string
			a := NewAgent(AgentOptions{
				InitialState: &AgentState{Model: testModel, Messages: []AgentMessage{ai.NewUserText("existing", 1)}},
				SteeringMode: tc.mode,
				StreamFn:     recordUserTexts(&requests),
			})
			a.Steer(ai.NewUserText("first", 2))
			a.Steer(ai.NewUserText("second", 3))

			if err := a.Continue(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertQueuedInOrder(t, requests, "first", "second", tc.together)
		})
	}
}

// agent.test.ts "keeps steering ahead of follow-up from a non-assistant
// continuation tail". Characterization; seen red with runLoop's startup poll
// preferring the follow-up queue over steering ("follow-up" reaches the first
// request, "steering" does not).
func TestAgentContinueKeepsSteeringAheadOfFollowUp(t *testing.T) {
	var requests [][]string
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Messages: []AgentMessage{ai.NewUserText("existing", 1)}},
		StreamFn:     recordUserTexts(&requests),
	})
	a.Steer(ai.NewUserText("steering", 2))
	a.FollowUp(ai.NewUserText("follow-up", 3))

	if err := a.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertQueuedInOrder(t, requests, "steering", "follow-up", false)
}

// agent.test.ts "defers follow-up input on the first continuation request from
// a $name tail". Characterization; seen red for both tails with Continue's
// assistant-tail queue drain applied to every tail ("follow-up" reaches the
// first request).
func TestAgentContinueDefersFollowUpFromANonAssistantTail(t *testing.T) {
	for name, messages := range map[string][]AgentMessage{
		"user": {ai.NewUserText("existing user", 1)},
		"toolResult": {
			ai.NewUserText("existing user", 1),
			assistantWithToolCall("call-1", "noop", map[string]any{}),
			ai.ToolResultMessage{ToolCallID: "call-1", ToolName: "noop", Content: ai.ContentList{ai.TextContent{Text: "done"}}, Timestamp: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var requests [][]string
			a := NewAgent(AgentOptions{
				InitialState: &AgentState{Model: testModel, Messages: messages},
				StreamFn:     recordUserTexts(&requests),
			})
			a.FollowUp(ai.NewUserText("follow-up", 2))

			if err := a.Continue(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 || slices.Contains(requests[0], "follow-up") || !slices.Contains(requests[1], "follow-up") {
				t.Fatalf("requests = %v, want follow-up deferred to the second request", requests)
			}
		})
	}
}

// TestAgentForwardsPrepareRequest locks the Agent-level PrepareRequest option
// reaching the loop config with the run's model and thinking level (an Agent at
// ThinkOff hands the hook "off"), and its replacement reaching the request.
func TestAgentForwardsPrepareRequest(t *testing.T) {
	replacementModel := &ai.Model{ID: "replacement", Name: "replacement", Api: "faux", Provider: "faux"}
	var handed string
	var sentModel string
	scripted := scriptedStream(textMessage("done"))
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, ThinkingLevel: ThinkOff},
		PrepareRequest: func(_ context.Context, r PrepareRequestContext) *AgentRequestUpdate {
			handed = r.Model.ID + "/" + string(r.ThinkingLevel)
			return &AgentRequestUpdate{Model: replacementModel}
		},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			sentModel = model.ID
			return scripted(ctx, model, req, opts)
		},
	})

	if err := a.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	if handed != "faux/off" {
		t.Fatalf("PrepareRequest was handed %q, want faux/off", handed)
	}
	if sentModel != replacementModel.ID {
		t.Fatalf("request model = %q, want the replacement %q", sentModel, replacementModel.ID)
	}
}

// upstream agent.test.ts "attributes requests to pi by default and merges a
// configured identity over it" (adc5fa9f8, 04a93bad8).
func TestAgentIdentityDefaultsToPiAttribution(t *testing.T) {
	var received []*ai.ClientIdentity
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			received = append(received, opts.Identity)
			return replyWith(textMessage("ok"))
		},
	})
	if err := a.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	acme := &ai.ClientAttribution{Name: "acme", URL: "https://acme.example"}
	a.Identity = &ai.ClientIdentity{UserAgent: ai.UserAgentShort, Attribution: acme}
	if err := a.Prompt(context.Background(), "hello again"); err != nil {
		t.Fatal(err)
	}
	a.Identity = &ai.ClientIdentity{UserAgent: ai.UserAgentShort}
	if err := a.Prompt(context.Background(), "and again"); err != nil {
		t.Fatal(err)
	}
	want := []ai.ClientIdentity{
		{Attribution: &ai.PiAttribution},
		{Attribution: acme, UserAgent: ai.UserAgentShort},
		{Attribution: &ai.PiAttribution, UserAgent: ai.UserAgentShort},
	}
	if len(received) != len(want) {
		t.Fatalf("received %d requests, want %d", len(received), len(want))
	}
	for i, got := range received {
		if got == nil || got.Attribution != want[i].Attribution || got.UserAgent != want[i].UserAgent || got.Version != "" {
			t.Errorf("request %d identity = %+v, want %+v", i, got, want[i])
		}
	}
	// NoAttribution turns pi's attribution off.
	a.Identity = &ai.ClientIdentity{Attribution: ai.NoAttribution}
	if err := a.Prompt(context.Background(), "once more"); err != nil {
		t.Fatal(err)
	}
	if got := received[len(received)-1]; got.Attribution != ai.NoAttribution {
		t.Errorf("identity with NoAttribution = %+v", got)
	}
}
