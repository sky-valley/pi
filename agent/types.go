// Package agent is the Go port of pi's agent runtime (@earendil-works/pi-agent-core).
// It drives an LLM conversation loop with tool calling, streaming, hooks, and
// steering/follow-up message queues on top of the ai package.
package agent

import (
	"context"

	"github.com/sky-valley/pi/ai"
)

// AgentMessage is a message in the agent transcript. The four ai message types
// (SystemMessage, UserMessage, AssistantMessage, ToolResultMessage) satisfy it;
// apps may add custom UI-only message types that implement ai.Message
// (MessageRole) and are filtered out by ConvertToLlm before reaching the
// provider. System messages in the transcript carry the prompt and the tool
// declarations.
type AgentMessage = ai.Message

// ToolExecutionMode controls how a batch of tool calls is executed.
type ToolExecutionMode string

const (
	// ToolSequential prepares, executes, and finalizes each call before the next.
	ToolSequential ToolExecutionMode = "sequential"
	// ToolParallel prepares calls sequentially, then runs allowed tools concurrently.
	ToolParallel ToolExecutionMode = "parallel"
	// ToolDefault defers to the loop-level default.
	ToolDefault ToolExecutionMode = ""
)

// QueueMode controls how many queued messages drain at a drain point.
type QueueMode string

const (
	// QueueAll drains every queued message at the drain point.
	QueueAll QueueMode = "all"
	// QueueOneAtATime drains only the oldest queued message.
	QueueOneAtATime QueueMode = "one-at-a-time"
)

// ThinkingLevel is the reasoning level for a turn ("off" disables reasoning).
type ThinkingLevel string

const (
	ThinkOff     ThinkingLevel = "off"
	ThinkMinimal ThinkingLevel = "minimal"
	ThinkLow     ThinkingLevel = "low"
	ThinkMedium  ThinkingLevel = "medium"
	ThinkHigh    ThinkingLevel = "high"
	ThinkXHigh   ThinkingLevel = "xhigh"
)

// AgentToolResult is the (partial or final) output of a tool.
type AgentToolResult struct {
	// Content is text/image content returned to the model.
	Content ai.ContentList
	// Details is arbitrary structured data for logs/UI.
	Details any
	// StructuredContent is a machine-readable result matching the tool's
	// OutputSchema, for programmatic callers such as a script that calls tools
	// (pi AgentToolResult.structuredContent, upstream 8562bcf66). It is not sent
	// to the model — Content stays the model-facing result — and not recorded on
	// the tool result message. nil is pi's undefined. pi also carries a JSON
	// null as a value (its tool_result event and codemode test
	// `=== undefined`); here that is a json.RawMessage("null"), which, being
	// non-nil, is present everywhere — including as an AfterToolCall
	// replacement, where pi's `??` would skip a null.
	StructuredContent any
	// Usage is what executing the tool cost, when the tool accounted for it (a
	// tool that calls a model itself, e.g.). It is carried onto the tool result
	// message and is not part of the main LLM context accounting (pi
	// AgentToolResult.usage, upstream 2fd386840).
	Usage *ai.Usage
	// IsError reports a failure without returning an error (upstream 8562bcf66):
	// the model sees Content as an error result, as it would a returned error,
	// but Details and StructuredContent are kept for the UI and programmatic
	// callers.
	IsError bool
	// Terminate hints that the agent should stop after the current tool batch.
	// Early termination only happens when every finalized result sets this.
	Terminate bool
}

// AgentToolCallOutcome is the final outcome of a tool call after its hooks ran
// (upstream 8562bcf66, pi's FinalizedToolCallOutcome): what the loop records
// for a model-issued call, and what RunToolCall returns for a call made from
// inside another tool.
type AgentToolCallOutcome struct {
	ToolCall ai.ToolCall
	Result   AgentToolResult
	// IsError is the final verdict, after AfterToolCall — the one the tool
	// result message carries. Result.IsError is only what the tool itself
	// reported, and an AfterToolCall override can make the two differ.
	IsError bool
	// DurationMs is how many milliseconds Execute took, measured with the
	// monotonic clock (upstream 36a686ee8); nil when the tool did not run.
	DurationMs *int64
}

// ToolUpdateFunc streams partial tool results during execution.
//
// The callback is scoped to the current Execute invocation. Calls made after
// Execute returns are ignored.
type ToolUpdateFunc func(partial AgentToolResult)

// AgentTool is a tool available to the agent. It mirrors pi's AgentTool object:
// a tool definition plus a UI label and an execute function. Stored by value in
// the agent's tool list.
type AgentTool struct {
	Name        string
	Description string
	Parameters  *ai.Schema
	// Label is a human-readable name for UI display.
	Label string
	// ExecutionMode optionally overrides the loop default for this tool.
	ExecutionMode ToolExecutionMode
	// PrepareArguments is an optional shim applied to raw arguments before schema
	// validation. Return the same map to indicate no change.
	PrepareArguments func(raw map[string]any) map[string]any
	// PromptGuidelines are optional bullet lines the system prompt builder folds
	// into its rules section (port of pi's tool promptGuidelines).
	PromptGuidelines []string
	// ConstrainedSampling optionally asks the provider to constrain sampling for
	// this tool (JSON schema or grammar). pi carries this on AgentTool for free
	// because its AgentTool extends Tool; Go has to forward it explicitly, and
	// asAITool is the only path from a tool to the provider request.
	ConstrainedSampling *ai.ConstrainedSamplingConfig
	// OutputSchema is the JSON Schema of StructuredContent in successful results
	// (upstream 8562bcf66). A tool that declares it should always set
	// StructuredContent.
	OutputSchema *ai.Schema
	// Execute runs the tool. On failure, return an error (the loop converts it
	// to an error tool result) or a result with IsError set; do not only
	// describe the failure in Content.
	Execute func(ctx context.Context, toolCallID string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error)
}

// asAITool returns the ai.Tool definition used for schema validation and the
// provider request.
func (t AgentTool) asAITool() ai.Tool {
	return ai.Tool{
		Name:                t.Name,
		Description:         t.Description,
		Parameters:          t.Parameters,
		ConstrainedSampling: t.ConstrainedSampling,
	}
}

// AgentContext is the snapshot passed into the low-level loop. It has no
// system prompt: the transcript's system messages carry the prompt and the
// tools declared to the model (upstream 9e05370b2).
type AgentContext struct {
	// Messages is the transcript visible to the model.
	Messages []AgentMessage
	// Tools are the tools available for execution in this run. Before each
	// provider request the loop declares any difference from the tools the
	// transcript declares with a system message.
	Tools []AgentTool
}

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

// BeforeToolCallContext is passed to BeforeToolCall.
type BeforeToolCallContext struct {
	AssistantMessage *ai.AssistantMessage
	ToolCall         ai.ToolCall
	Args             map[string]any
	Context          *AgentContext
}

// BeforeToolCallResult blocks tool execution when Block is true.
type BeforeToolCallResult struct {
	Block  bool
	Reason string
	// Terminate hints that the agent should stop after the current tool batch
	// when this call is blocked. Like any other result, it only ends the batch
	// if every finalized result in it terminates (see shouldTerminateBatch).
	// Ignored unless Block is set.
	Terminate bool
}

// BeforeToolCallFunc runs before a tool executes, once its arguments are
// validated. A result that blocks the call turns it into an error tool result.
type BeforeToolCallFunc func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult

// AfterToolCallFunc runs after a tool executes. Its result overrides parts of
// the finalized tool result.
type AfterToolCallFunc func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult

// AfterToolCallContext is passed to AfterToolCall.
type AfterToolCallContext struct {
	AssistantMessage *ai.AssistantMessage
	ToolCall         ai.ToolCall
	Args             map[string]any
	Result           AgentToolResult
	IsError          bool
	Context          *AgentContext
}

// AfterToolCallResult overrides parts of a finalized tool result. A nil field
// keeps the original value; there is no deep merge.
type AfterToolCallResult struct {
	Content    ai.ContentList
	HasContent bool
	Details    any
	HasDetails bool
	// StructuredContent replaces the structured content when set. Replacing
	// Content without it drops the structured content, which may no longer
	// match the content; return it along with Content to keep it (upstream
	// 8562bcf66).
	StructuredContent any
	IsError           *bool
	// Usage replaces the tool result's usage when set (pi `usage ?? result.usage`).
	Usage     *ai.Usage
	Terminate *bool
}

// AgentTurnContext describes a turn that has just completed. It is passed to
// FinishTurn and, one iteration later, to PrepareNextTurn — upstream gives the
// second role its own name (PrepareNextTurnContext, which extends this one with
// no added members, types.ts:181); Go keeps the single type rather than an
// empty alias.
type AgentTurnContext struct {
	Message *ai.AssistantMessage
	// ToolResults are the tool result messages emitted for the completed turn.
	ToolResults []ai.ToolResultMessage
	// Context is the current agent context after the turn's assistant message
	// and tool results have been appended.
	Context     *AgentContext
	NewMessages []AgentMessage
}

// AgentTurnDecision is returned by FinishTurn. The zero value preserves normal
// scheduling (pi's undefined).
type AgentTurnDecision string

const (
	// TurnContinue ensures one next provider request on a normal turn.
	// Tool-result, steering, or follow-up scheduling can satisfy that request
	// and adds no extra one; otherwise the loop continues once with the current
	// context.
	TurnContinue AgentTurnDecision = "continue"
	// TurnEnd ends the run after turn_end without polling the queues or
	// preparing another request.
	TurnEnd AgentTurnDecision = "end"
)

// FinishTurnFunc is called after a completed assistant turn and all of its
// tool-result messages, but before turn_end; its decision is applied after
// turn_end. It also runs for error and aborted responses, whose decision is
// ignored because those responses remain hard exits.
type FinishTurnFunc func(ctx context.Context, turn AgentTurnContext) AgentTurnDecision

// PrepareRequestContext is the runtime state available immediately before a
// conversational provider request.
type PrepareRequestContext struct {
	Context       *AgentContext
	Model         *ai.Model
	ThinkingLevel ThinkingLevel
}

// AgentRequestUpdate replaces runtime state for the provider request being
// prepared and for later requests in the run. A nil field leaves the loop's
// current value in place (pi's `?? current`).
type AgentRequestUpdate struct {
	Context       *AgentContext
	Model         *ai.Model
	ThinkingLevel *ThinkingLevel
}

// PrepareRequestFunc is called immediately before every conversational
// provider request, including the first. Pending messages have already been
// appended and emitted when it runs.
type PrepareRequestFunc func(ctx context.Context, request PrepareRequestContext) *AgentRequestUpdate

// applyTo folds a request update into the loop's live state. ThinkingLevel is
// the one field whose absent and "off" cases differ: absent keeps the current
// reasoning level, "off" clears it.
func (u *AgentRequestUpdate) applyTo(current *AgentContext, config *AgentLoopConfig) {
	if u.Context != nil {
		*current = *u.Context
	}
	if u.Model != nil {
		config.Model = u.Model
	}
	if u.ThinkingLevel != nil {
		if *u.ThinkingLevel == ThinkOff {
			config.Reasoning = ""
		} else {
			config.Reasoning = *u.ThinkingLevel
		}
	}
}

// AgentLoopTurnUpdate replaces runtime state before the next provider request.
// A nil field leaves the loop's current value in place (pi's `?? current`).
type AgentLoopTurnUpdate struct {
	// Context is the context for the next provider request.
	Context *AgentContext
	// Messages are appended before the next provider request, ahead of any
	// queued steering or follow-up messages, with the normal message_start and
	// message_end events.
	Messages []AgentMessage
	// Model is the model for the next provider request.
	Model *ai.Model
	// ThinkingLevel is the thinking level for the next provider request.
	ThinkingLevel *ThinkingLevel
}

// applyTo folds a PrepareNextTurn snapshot into the loop's live state
// (agent-loop.ts:184-197) and returns the messages it prepared for the next
// request.
func (u *AgentLoopTurnUpdate) applyTo(current *AgentContext, config *AgentLoopConfig) []AgentMessage {
	(&AgentRequestUpdate{Context: u.Context, Model: u.Model, ThinkingLevel: u.ThinkingLevel}).applyTo(current, config)
	return u.Messages
}

// AgentLoopConfig configures a single agent loop run.
type AgentLoopConfig struct {
	Model     *ai.Model
	Reasoning ThinkingLevel // "" or "off" disables reasoning

	SessionID                 string
	Transport                 ai.Transport
	ThinkingBudgets           *ai.ThinkingBudgets
	MaxRetryDelayMs           *int
	MaxRetries                int
	TimeoutMs                 int
	WebSocketConnectTimeoutMs int
	HTTPClient                ai.HTTPDoer
	Temperature               *float64
	MaxTokens                 *int
	CacheRetention            ai.CacheRetention
	Headers                   ai.ProviderHeaders
	Metadata                  map[string]any
	APIKey                    string
	OnPayload                 func(payload any, model *ai.Model) (any, error)
	OnResponse                func(resp ai.ProviderResponse, model *ai.Model) error
	// OnProviderStreamEvent is forwarded to the stream options as
	// ai.StreamOptions.OnProviderStreamEvent.
	OnProviderStreamEvent func(data any, model *ai.Model) error

	ToolExecution ToolExecutionMode

	// ConvertToLlm maps the agent transcript to provider messages before each
	// call: each AgentMessage becomes a SystemMessage, UserMessage,
	// AssistantMessage or ToolResultMessage, and messages the model cannot
	// understand (UI-only notifications) are dropped. Must not return an error
	// for runtime issues; return a safe fallback.
	ConvertToLlm func(messages []AgentMessage) []ai.Message
	// TransformContext optionally rewrites the transcript before ConvertToLlm.
	TransformContext func(ctx context.Context, messages []AgentMessage) []AgentMessage
	// GetApiKey resolves an API key per call (for expiring OAuth tokens).
	GetApiKey func(provider string) string

	BeforeToolCall BeforeToolCallFunc
	AfterToolCall  AfterToolCallFunc
	// FinishTurn is called after the assistant message and all tool-result
	// messages have been emitted, immediately before turn_end. TurnEnd ends the
	// run without polling the queues or preparing another request. On a normal
	// turn, TurnContinue ensures one next provider request: tool-result,
	// steering, or follow-up scheduling can satisfy that request and adds no
	// extra one; otherwise the loop continues once with the current context.
	// The zero decision preserves normal scheduling. Error and aborted
	// responses remain hard exits.
	FinishTurn FinishTurnFunc
	// PrepareRequest is called immediately before every conversational provider
	// request, including the first. Pending messages have already been
	// appended. The returned context, model, and thinking level replace the
	// runtime values for this and later requests in the run. It does not poll
	// the queues.
	PrepareRequest PrepareRequestFunc
	// PrepareNextTurn is called after turn_end ONLY WHEN THE LOOP WILL CONTINUE,
	// immediately before the next turn starts — so it does not run after a final
	// or terminating turn, nor after FinishTurn ends the run (upstream
	// 56700d42e). Return replacement context/model/thinking state or messages to
	// append to affect that turn, or nil to keep the current ones. Preparation
	// may be long-running (compaction); steering queued while it runs is picked
	// up before the turn starts.
	PrepareNextTurn func(c AgentTurnContext) *AgentLoopTurnUpdate
	// GetSteeringMessages returns steering messages to inject mid-run. It is
	// polled after the current assistant turn finishes executing its tool
	// calls, unless FinishTurn ends the run.
	GetSteeringMessages func() []AgentMessage
	GetFollowUpMessages func() []AgentMessage
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// EventType is the discriminator for AgentEvent.
type EventType string

const (
	EvAgentStart          EventType = "agent_start"
	EvAgentEnd            EventType = "agent_end"
	EvTurnStart           EventType = "turn_start"
	EvTurnEnd             EventType = "turn_end"
	EvMessageStart        EventType = "message_start"
	EvMessageUpdate       EventType = "message_update"
	EvMessageEnd          EventType = "message_end"
	EvToolExecutionStart  EventType = "tool_execution_start"
	EvToolExecutionUpdate EventType = "tool_execution_update"
	EvToolExecutionEnd    EventType = "tool_execution_end"
)

// AgentEvent is a lifecycle event emitted by the loop/agent.
type AgentEvent struct {
	Type EventType

	// AgentEnd: full new-message list for the run.
	Messages []AgentMessage
	// TurnEnd / Message*: the relevant message. Message lifecycle events are
	// emitted for system, user, assistant and tool-result messages;
	// MessageUpdate only for a streaming assistant message.
	Message AgentMessage
	// TurnEnd: tool results from the turn.
	ToolResults []ai.ToolResultMessage
	// MessageUpdate: the underlying assistant stream event.
	AssistantMessageEvent *ai.AssistantMessageEvent

	// Tool execution events.
	ToolCallID    string
	ToolName      string
	Args          map[string]any
	PartialResult any
	Result        any
	IsError       bool
	// ToolExecutionEnd: how many milliseconds Execute took, measured with the
	// monotonic clock (upstream 36a686ee8); nil when the tool did not run.
	DurationMs *int64
}

// EventSink receives loop events. Returning an error aborts the loop.
type EventSink func(event AgentEvent) error
