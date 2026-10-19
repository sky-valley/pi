package agent

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

// runToolTurn prompts an agent whose model calls tool once with args, then
// stops, and returns the tool result message and the tool_execution_end event.
func runToolTurn(t *testing.T, tool AgentTool, args map[string]any, after func(context.Context, AfterToolCallContext) *AfterToolCallResult) (ai.ToolResultMessage, AgentEvent) {
	t.Helper()
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		StreamFn: scriptedStream(
			assistantWithToolCall("c1", tool.Name, args),
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
		),
		AfterToolCall: after,
	})
	var end AgentEvent
	a.Subscribe(func(_ context.Context, e AgentEvent) error {
		if e.Type == EvToolExecutionEnd {
			end = e
		}
		return nil
	})
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	for _, m := range a.State().Messages {
		if trm, ok := m.(ai.ToolResultMessage); ok {
			return trm, end
		}
	}
	t.Fatal("no tool result message")
	return ai.ToolResultMessage{}, end
}

// Upstream 2fd386840 (#6671, agent-loop.test.ts "should handle tool calls and
// results"): a tool can report the usage its own execution cost; afterToolCall
// observes it and can replace it (usage ?? result.usage), and the tool result
// message carries the final value.
func TestLoopCarriesToolResultUsage(t *testing.T) {
	toolUsage := &ai.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, TotalTokens: 10,
		Cost: ai.CostBreakdown{Input: 0.1, Output: 0.2, CacheRead: 0.3, CacheWrite: 0.4, Total: 1}}
	patched := &ai.Usage{Input: 5, Output: 6, CacheRead: 7, CacheWrite: 8, TotalTokens: 26,
		Cost: ai.CostBreakdown{Input: 0.5, Output: 0.6, CacheRead: 0.7, CacheWrite: 0.8, Total: 2.6}}
	echo := AgentTool{
		Name:       "echo",
		Parameters: ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echoed"}}, Usage: toolUsage}, nil
		},
	}

	t.Run("the tool's usage", func(t *testing.T) {
		trm, _ := runToolTurn(t, echo, map[string]any{"value": "hello"}, nil)
		if !reflect.DeepEqual(trm.Usage, toolUsage) {
			t.Fatalf("tool result usage = %+v, want the tool's %+v", trm.Usage, toolUsage)
		}
	})
	t.Run("replaced by afterToolCall", func(t *testing.T) {
		var observed *ai.Usage
		trm, _ := runToolTurn(t, echo, map[string]any{"value": "hello"}, func(_ context.Context, c AfterToolCallContext) *AfterToolCallResult {
			observed = c.Result.Usage
			return &AfterToolCallResult{Usage: patched}
		})
		if !reflect.DeepEqual(observed, toolUsage) {
			t.Fatalf("afterToolCall observed usage %+v, want %+v", observed, toolUsage)
		}
		if !reflect.DeepEqual(trm.Usage, patched) {
			t.Fatalf("tool result usage = %+v, want the patched %+v", trm.Usage, patched)
		}
	})
}

// runToolCall tools, ported from agent-loop.test.ts (upstream 8562bcf66).
var (
	runEcho = AgentTool{
		Name:         "echo",
		Label:        "Echo",
		Description:  "Echo tool",
		Parameters:   ai.Object(ai.Prop("value", ai.String())),
		OutputSchema: ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			if onUpdate != nil {
				onUpdate(AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "partial"}}, Details: map[string]any{}})
			}
			value, _ := params["value"].(string)
			return AgentToolResult{
				Content:           ai.ContentList{ai.TextContent{Text: value}},
				Details:           map[string]any{},
				StructuredContent: map[string]any{"value": value},
			}, nil
		},
	}
	runFailing = AgentTool{
		Name:        "failing",
		Label:       "Failing",
		Description: "Returns an error result",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "bad"}}, Details: map[string]any{"partial": true}, IsError: true}, nil
		},
	}
)

func runCall(id, name string, args map[string]any) ai.ToolCall {
	return ai.ToolCall{ID: id, Name: name, Arguments: args}
}

func outcomeText(o AgentToolCallOutcome) string { return textOfContent(o.Result.Content) }

// agent-loop.test.ts "validates, runs the hooks, and reports failures as
// error outcomes".
func TestRunToolCallValidatesRunsHooksAndReportsFailures(t *testing.T) {
	var hookCalls []string
	var updates []AgentToolResult
	opts := RunToolCallOptions{
		ToolCallHooks: ToolCallHooks{
			BeforeToolCall: func(_ context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
				hookCalls = append(hookCalls, "before "+c.ToolCall.ID)
				if c.Args["value"] == "blocked" {
					return &BeforeToolCallResult{Block: true, Reason: "nope"}
				}
				return nil
			},
			AfterToolCall: func(_ context.Context, c AfterToolCallContext) *AfterToolCallResult {
				hookCalls = append(hookCalls, "after "+c.ToolCall.ID)
				return nil
			},
		},
		Tools:            []AgentTool{runEcho, runFailing},
		AssistantMessage: &ai.AssistantMessage{},
		Context:          &AgentContext{},
		OnUpdate:         func(partial AgentToolResult) { updates = append(updates, partial) },
	}
	ctx := context.Background()

	a := RunToolCall(ctx, runCall("a", "echo", map[string]any{"value": "a"}), opts)
	if a.ToolCall.ID != "a" || a.IsError || !reflect.DeepEqual(a.Result.StructuredContent, map[string]any{"value": "a"}) {
		t.Errorf("echo a = %+v, want a success with structured content {value: a}", a)
	}
	if b := RunToolCall(ctx, runCall("b", "echo", map[string]any{"value": map[string]any{"nested": true}}), opts); !b.IsError {
		t.Errorf("invalid arguments = %+v, want an error outcome", b)
	}
	if c := RunToolCall(ctx, runCall("c", "echo", map[string]any{"value": "blocked"}), opts); !c.IsError || outcomeText(c) != "nope" {
		t.Errorf("blocked call = %+v, want error outcome \"nope\"", c)
	}
	if d := RunToolCall(ctx, runCall("d", "missing", map[string]any{}), opts); !d.IsError || outcomeText(d) != "Tool missing not found" {
		t.Errorf("unknown tool = %+v, want error outcome \"Tool missing not found\"", d)
	}
	// Error results keep their details.
	if e := RunToolCall(ctx, runCall("e", "failing", map[string]any{}), opts); !e.IsError || !reflect.DeepEqual(e.Result.Details, map[string]any{"partial": true}) {
		t.Errorf("isError result = %+v, want an error outcome keeping details {partial: true}", e)
	}
	wantUpdates := []AgentToolResult{{Content: ai.ContentList{ai.TextContent{Text: "partial"}}, Details: map[string]any{}}}
	if !reflect.DeepEqual(updates, wantUpdates) {
		t.Errorf("updates = %+v, want %+v", updates, wantUpdates)
	}
	// Validation failures and unknown tools never reach the hooks; blocked calls skip afterToolCall.
	if want := []string{"before a", "after a", "before c", "before e", "after e"}; !reflect.DeepEqual(hookCalls, want) {
		t.Errorf("hook calls = %q, want %q", hookCalls, want)
	}
}

// agent-loop.test.ts "lets afterToolCall replace structured content and drops
// it when only content is replaced".
func TestRunToolCallAfterToolCallStructuredContent(t *testing.T) {
	redacted := ai.ContentList{ai.TextContent{Text: "redacted"}}
	cases := []struct {
		name  string
		after AfterToolCallResult
		want  any
	}{
		{"content replaced alone drops it", AfterToolCallResult{Content: redacted, HasContent: true}, nil},
		{"replaced", AfterToolCallResult{StructuredContent: map[string]any{"value": "replaced"}}, map[string]any{"value": "replaced"}},
		{"replaced with content", AfterToolCallResult{Content: redacted, HasContent: true, StructuredContent: map[string]any{"value": "both"}}, map[string]any{"value": "both"}},
		{"untouched by a details override", AfterToolCallResult{Details: map[string]any{"note": "kept"}, HasDetails: true}, map[string]any{"value": "original"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after := tc.after
			outcome := RunToolCall(context.Background(), runCall("x", "echo", map[string]any{"value": "original"}), RunToolCallOptions{
				ToolCallHooks: ToolCallHooks{AfterToolCall: func(context.Context, AfterToolCallContext) *AfterToolCallResult {
					return &after
				}},
				Tools:            []AgentTool{runEcho},
				AssistantMessage: &ai.AssistantMessage{},
				Context:          &AgentContext{},
			})
			if !reflect.DeepEqual(outcome.Result.StructuredContent, tc.want) {
				t.Fatalf("structured content = %#v, want %#v", outcome.Result.StructuredContent, tc.want)
			}
		})
	}
}

// Upstream 8562bcf66: in a model-issued call too, a result with IsError is an
// error tool result that keeps its details, and the tool_execution_end event
// carries the structured content (the message does not record it).
func TestLoopToolResultIsErrorKeepsDetails(t *testing.T) {
	trm, end := runToolTurn(t, runFailing, map[string]any{}, nil)
	if !trm.IsError || textOfContent(trm.Content) != "bad" || !reflect.DeepEqual(trm.Details, map[string]any{"partial": true}) {
		t.Fatalf("tool result message = %+v, want an error result \"bad\" keeping details {partial: true}", trm)
	}
	if !end.IsError {
		t.Fatalf("tool_execution_end isError = false, want true")
	}

	_, end = runToolTurn(t, runEcho, map[string]any{"value": "v"}, nil)
	result, _ := end.Result.(AgentToolResult)
	if !reflect.DeepEqual(result.StructuredContent, map[string]any{"value": "v"}) {
		t.Fatalf("tool_execution_end structured content = %#v, want {value: v}", end.Result)
	}
}

// agent-loop.test.ts "records how long execute() took on the tool result,
// excluding hooks" (upstream 36a686ee8, #10549): the duration is measured
// around Execute alone, and a call that never ran has none — on the tool
// result message and on tool_execution_end alike.
func TestLoopRecordsToolExecutionDuration(t *testing.T) {
	echo := AgentTool{
		Name:        "echo",
		Label:       "Echo",
		Description: "Echo tool",
		Parameters:  ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			time.Sleep(30 * time.Millisecond)
			value, _ := params["value"].(string)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: value}}, Details: map[string]any{"value": value}}, nil
		},
	}
	call := &ai.AssistantMessage{
		Content: ai.ContentList{
			ai.ToolCall{ID: "ran", Name: "echo", Arguments: map[string]any{"value": "a"}},
			ai.ToolCall{ID: "blocked", Name: "echo", Arguments: map[string]any{"value": "b"}},
		},
		StopReason: ai.StopToolUse,
	}
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{echo}},
		StreamFn: scriptedStream(call,
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop}),
		BeforeToolCall: func(_ context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			time.Sleep(100 * time.Millisecond)
			if c.ToolCall.ID == "blocked" {
				return &BeforeToolCallResult{Block: true, Reason: "no"}
			}
			return nil
		},
	})
	ends := map[string]AgentEvent{}
	a.Subscribe(func(_ context.Context, e AgentEvent) error {
		if e.Type == EvToolExecutionEnd {
			ends[e.ToolCallID] = e
		}
		return nil
	})
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	results := map[string]ai.ToolResultMessage{}
	for _, m := range a.State().Messages {
		if trm, ok := m.(ai.ToolResultMessage); ok {
			results[trm.ToolCallID] = trm
		}
	}

	ran, blocked := results["ran"], results["blocked"]
	if ran.DurationMs == nil || *ran.DurationMs < 25 || *ran.DurationMs >= 100 {
		t.Fatalf("ran durationMs = %v, want execute()'s ~30ms without the 100ms hook", ran.DurationMs)
	}
	if end := ends["ran"]; end.DurationMs == nil || *end.DurationMs != *ran.DurationMs {
		t.Fatalf("tool_execution_end durationMs = %v, want the result's %d", end.DurationMs, *ran.DurationMs)
	}
	if !blocked.IsError || blocked.DurationMs != nil {
		t.Fatalf("blocked result = isError %v, durationMs %v; want an error with no duration", blocked.IsError, blocked.DurationMs)
	}
	if end := ends["blocked"]; end.DurationMs != nil {
		t.Fatalf("blocked tool_execution_end durationMs = %d, want none", *end.DurationMs)
	}
}

// pi's prepareToolCallArguments spreads the call (`{...toolCall, arguments}`),
// so a call whose arguments text was malformed (upstream 87f9a3641
// argumentsError) still fails validation with that error after a tool's
// prepareArguments hook has run — the tool never runs with the empty
// arguments, even when its schema accepts them.
func TestRunToolCallReportsArgumentsErrorAfterPrepareArguments(t *testing.T) {
	ran := false
	tool := AgentTool{
		Name:             "echo",
		Parameters:       ai.Object(ai.Opt("value", ai.String())),
		PrepareArguments: func(args map[string]any) map[string]any { return map[string]any{"value": "prepared"} },
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			ran = true
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ran"}}}, nil
		},
	}
	call := ai.ToolCall{ID: "a", Name: "echo", Arguments: map[string]any{}, ArgumentsError: "Unexpected end of JSON input\n\nReceived arguments:\n{\"value\":"}
	out := RunToolCall(context.Background(), call, RunToolCallOptions{Tools: []AgentTool{tool}, AssistantMessage: &ai.AssistantMessage{}, Context: &AgentContext{}})
	want := "Invalid arguments for tool \"echo\": Unexpected end of JSON input\n\nReceived arguments:\n{\"value\":"
	if ran || !out.IsError || outcomeText(out) != want {
		t.Errorf("RunToolCall = %+v (ran %v), want an error outcome %q", out, ran, want)
	}
}
