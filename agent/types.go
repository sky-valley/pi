// Package agent is the Go port of pi's agent runtime (@earendil-works/pi-agent-core).
// It drives an LLM conversation loop with tool calling, streaming, hooks, and
// steering/follow-up message queues on top of the ai package.
package agent

import (
	"context"

	"github.com/sky-valley/pi/ai"
)

// AgentMessage is a message in the agent transcript. The three ai message types
// (UserMessage, AssistantMessage, ToolResultMessage) satisfy it; apps may add
// custom UI-only message types that implement ai.Message (MessageRole) and are
// filtered out by ConvertToLlm before reaching the provider.
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
	// Terminate hints that the agent should stop after the current tool batch.
	// Early termination only happens when every finalized result sets this.
	Terminate bool
}

// ToolUpdateFunc streams partial tool results during execution.
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
	// Execute runs the tool. Return an error on failure (the loop converts it to
	// an error tool result) rather than encoding errors in Content.
	Execute func(ctx context.Context, toolCallID string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error)
}

// asAITool returns the ai.Tool definition used for schema validation and the
// provider request.
func (t AgentTool) asAITool() ai.Tool {
	return ai.Tool{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
}

// AgentContext is the snapshot passed into the low-level loop.
type AgentContext struct {
	SystemPrompt string
	Messages     []AgentMessage
	Tools        []AgentTool
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
}

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
	IsError    *bool
	Terminate  *bool
}

// ShouldStopAfterTurnContext is passed to ShouldStopAfterTurn / PrepareNextTurn.
type ShouldStopAfterTurnContext struct {
	Message     *ai.AssistantMessage
	ToolResults []ai.ToolResultMessage
	Context     *AgentContext
	NewMessages []AgentMessage
}

// AgentLoopTurnUpdate replaces runtime state before the next provider request.
type AgentLoopTurnUpdate struct {
	Context       *AgentContext
	Model         *ai.Model
	ThinkingLevel *ThinkingLevel
}

// AgentLoopConfig configures a single agent loop run.
type AgentLoopConfig struct {
	Model     *ai.Model
	Reasoning ThinkingLevel // "" or "off" disables reasoning

	SessionID       string
	Transport       ai.Transport
	ThinkingBudgets *ai.ThinkingBudgets
	MaxRetryDelayMs *int
	MaxRetries      int
	TimeoutMs       int
	Temperature     *float64
	MaxTokens       *int
	CacheRetention  ai.CacheRetention
	Headers         map[string]string
	APIKey          string
	OnPayload       func(payload any, model *ai.Model) (any, error)
	OnResponse      func(resp ai.ProviderResponse, model *ai.Model) error

	ToolExecution ToolExecutionMode

	// ConvertToLlm maps the agent transcript to provider messages before each call.
	// Must not return an error for runtime issues; return a safe fallback.
	ConvertToLlm func(messages []AgentMessage) []ai.Message
	// TransformContext optionally rewrites the transcript before ConvertToLlm.
	TransformContext func(ctx context.Context, messages []AgentMessage) []AgentMessage
	// GetApiKey resolves an API key per call (for expiring OAuth tokens).
	GetApiKey func(provider string) string

	BeforeToolCall      func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall       func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
	ShouldStopAfterTurn func(c ShouldStopAfterTurnContext) bool
	PrepareNextTurn     func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
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
	// TurnEnd / Message*: the relevant message.
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
}

// EventSink receives loop events. Returning an error aborts the loop.
type EventSink func(event AgentEvent) error
