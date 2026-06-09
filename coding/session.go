package coding

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// DefaultThinkingLevel is pi's DEFAULT_THINKING_LEVEL (defaults.ts:3): an unset
// reasoning level starts at "medium" before clamping to the model's capabilities.
const DefaultThinkingLevel = agent.ThinkMedium

// NoToolsMode controls default tool suppression (mirrors createAgentSession).
type NoToolsMode string

const (
	// NoToolsOff keeps the default built-in tools enabled.
	NoToolsOff NoToolsMode = ""
	// NoToolsAll starts with no tools enabled.
	NoToolsAll NoToolsMode = "all"
	// NoToolsBuiltin disables the default built-in tools but keeps custom tools.
	NoToolsBuiltin NoToolsMode = "builtin"
)

// SessionOptions configures a coding Session. The tool fields mirror pi's
// createAgentSession: when Tools is nil the built-in set is resolved from
// ToolNames/ExcludeTools/NoTools, then CustomTools are appended.
type SessionOptions struct {
	Model *ai.Model
	Cwd   string

	// Tools, when non-nil, is used verbatim and bypasses name-based selection.
	Tools []agent.AgentTool
	// ToolNames is an allowlist of built-in tool names. When nil and NoTools is
	// off, the default set [read, bash, edit, write] is used.
	ToolNames []string
	// ExcludeTools is a denylist applied after ToolNames.
	ExcludeTools []string
	// NoTools suppresses the default built-in tools ("all" or "builtin").
	NoTools NoToolsMode
	// CustomTools are appended to the resolved built-in set.
	CustomTools []agent.AgentTool

	SystemPrompt  string
	ThinkingLevel agent.ThinkingLevel
	APIKey        string
	SessionID     string

	// Per-request provider controls (all optional).
	Temperature     *float64
	MaxTokens       *int
	CacheRetention  ai.CacheRetention
	MaxRetries      int
	TimeoutMs       int
	MaxRetryDelayMs *int
	Transport       ai.Transport
	ThinkingBudgets *ai.ThinkingBudgets
	// Headers are extra HTTP headers merged into every provider request
	// (e.g. OpenAI-Organization).
	Headers map[string]string
	// OnPayload can inspect/replace the provider request body before sending.
	OnPayload func(payload any, model *ai.Model) (any, error)
	// OnResponse is invoked after the HTTP response is received.
	OnResponse func(resp ai.ProviderResponse, model *ai.Model) error
	// BeforeToolCall runs after a tool call's args are validated and before it
	// executes. Return {Block:true, Reason:...} to deny it (the loop emits an
	// error tool result). This is the native equivalent of pi's tool_call
	// extension hook — use it for permission gates, path protection, etc.
	BeforeToolCall func(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult
	// AfterToolCall runs after a tool finishes; return overrides for the result.
	AfterToolCall func(ctx context.Context, c agent.AfterToolCallContext) *agent.AfterToolCallResult

	// Compaction, when non-nil, installs automatic context-window compaction.
	// Use &DefaultCompactionSettings for pi's defaults.
	Compaction *CompactionSettings
	// StreamFn overrides the stream function (for tests). Default: ai.StreamSimple.
	StreamFn agent.StreamFn
}

var defaultActiveToolNames = []string{"read", "bash", "edit", "write"}

// resolveTools builds the tool set from the name-based selection fields.
func resolveTools(cwd string, opts SessionOptions) []agent.AgentTool {
	if opts.Tools != nil {
		return append(append([]agent.AgentTool(nil), opts.Tools...), opts.CustomTools...)
	}
	var names []string
	switch {
	case opts.ToolNames != nil:
		names = opts.ToolNames
	case opts.NoTools != NoToolsOff:
		names = nil
	default:
		names = defaultActiveToolNames
	}
	excluded := map[string]bool{}
	for _, e := range opts.ExcludeTools {
		excluded[e] = true
	}
	var tools []agent.AgentTool
	for _, name := range names {
		if excluded[name] {
			continue
		}
		if t, err := CreateTool(name, cwd); err == nil {
			tools = append(tools, t)
		}
	}
	return append(tools, opts.CustomTools...)
}

// Session is a coding-agent session: an Agent wired with a model, tools, and the
// coding system prompt.
type Session struct {
	Agent    *agent.Agent
	Model    *ai.Model
	Cwd      string
	Recorder *SessionRecorder
	apiKey   string
}

// Record attaches a SessionRecorder; finalized messages are appended to it.
func (s *Session) Record(r *SessionRecorder) {
	s.Recorder = r
	if r == nil {
		return
	}
	s.Agent.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		if e.Type == agent.EvMessageEnd {
			r.RecordMessage(e.Message)
		}
		return nil
	})
}

// LoadHistory seeds the agent transcript from a prior session's messages.
func (s *Session) LoadHistory(messages []agent.AgentMessage) {
	s.Agent.SetMessages(messages)
}

// SetModel switches the active model (and API key) for future turns.
func (s *Session) SetModel(model *ai.Model, apiKey string) {
	s.Model = model
	s.apiKey = apiKey
	s.Agent.SetModel(model)
	s.Agent.GetApiKey = func(provider string) string { return apiKey }
	if r := s.Recorder; r != nil {
		r.RecordModelChange(model.Provider, model.ID)
	}
}

// SetThinkingLevel sets the reasoning level for future turns.
func (s *Session) SetThinkingLevel(level agent.ThinkingLevel) {
	s.Agent.SetThinkingLevel(level)
	if r := s.Recorder; r != nil {
		r.RecordThinkingLevel(string(level))
	}
}

// History returns the current transcript.
func (s *Session) History() []agent.AgentMessage { return s.Agent.State().Messages }

// Reset clears the transcript.
func (s *Session) Reset() { s.Agent.Reset() }

// LastAssistantText returns the most recent assistant message text.
func (s *Session) LastAssistantText() string {
	return lastAssistantText(s.Agent.State().Messages)
}

// NewSession builds a Session. If Tools is nil, the default coding tools are used;
// if SystemPrompt is empty, a system prompt is built from the tool set.
func NewSession(opts SessionOptions) *Session {
	cwd := opts.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	tools := resolveTools(cwd, opts)
	systemPrompt := opts.SystemPrompt
	if systemPrompt == "" {
		var names []string
		for _, t := range tools {
			names = append(names, t.Name)
		}
		systemPrompt = BuildSystemPrompt(BuildSystemPromptOptions{
			SelectedTools: names,
			ToolSnippets:  ToolSnippets,
			Cwd:           cwd,
			ContextFiles:  LoadProjectContextFiles(cwd),
			Skills:        LoadSkills(cwd),
		})
	}
	thinking := opts.ThinkingLevel
	if thinking == "" {
		// pi defaults.ts: DEFAULT_THINKING_LEVEL = "medium" (then clamped to the
		// model's capabilities below; a non-reasoning model clamps back to "off").
		thinking = DefaultThinkingLevel
	}
	// Clamp the requested reasoning level to what the model actually supports
	// (mirrors createAgentSession's clampThinkingLevel). pi clamps to "off" when
	// there is no model (sdk.ts:237-241).
	if opts.Model != nil {
		thinking = agent.ThinkingLevel(ai.ClampThinkingLevel(opts.Model, ai.ModelThinkingLevel(thinking)))
	} else {
		thinking = agent.ThinkOff
	}

	a := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{
			Model:         opts.Model,
			SystemPrompt:  systemPrompt,
			Tools:         tools,
			ThinkingLevel: thinking,
		},
		StreamFn:        opts.StreamFn,
		SessionID:       opts.SessionID,
		GetApiKey:       func(provider string) string { return opts.APIKey },
		Temperature:     opts.Temperature,
		MaxTokens:       opts.MaxTokens,
		CacheRetention:  opts.CacheRetention,
		MaxRetries:      opts.MaxRetries,
		TimeoutMs:       opts.TimeoutMs,
		MaxRetryDelayMs: opts.MaxRetryDelayMs,
		Transport:       opts.Transport,
		ThinkingBudgets: opts.ThinkingBudgets,
		Headers:         opts.Headers,
		OnPayload:       opts.OnPayload,
		OnResponse:      opts.OnResponse,
		BeforeToolCall:  opts.BeforeToolCall,
		AfterToolCall:   opts.AfterToolCall,
	})

	sess := &Session{Agent: a, Model: opts.Model, Cwd: cwd, apiKey: opts.APIKey}
	if opts.Compaction != nil && opts.Compaction.Enabled {
		sess.EnableCompaction(*opts.Compaction)
	}
	return sess
}

// RunResult is the structured outcome of a single Run turn, suited to embedding
// pi as an SDK rather than a CLI.
type RunResult struct {
	// Text is the concatenated text of the final assistant message.
	Text string
	// Messages are the messages produced during this run (prompt → final).
	Messages []agent.AgentMessage
	// ToolCalls are the tool calls the model made during this run.
	ToolCalls []ai.ToolCall
	// Usage is the aggregate token usage + cost across every provider request in
	// this run (multi-turn tool loops are summed).
	Usage ai.Usage
	// StopReason is the final assistant stop reason.
	StopReason ai.StopReason
	// ErrorMessage is set when the run failed or was aborted.
	ErrorMessage string
}

// Subscribe registers an agent event listener (passthrough to the Agent), useful
// for streaming tokens/tool activity into an app UI. Returns an unsubscribe func.
func (s *Session) Subscribe(l agent.Listener) func() { return s.Agent.Subscribe(l) }

// Steer queues a message to inject after the current assistant turn finishes.
func (s *Session) Steer(m agent.AgentMessage) { s.Agent.Steer(m) }

// FollowUp queues a message to run after the agent would otherwise stop.
func (s *Session) FollowUp(m agent.AgentMessage) { s.Agent.FollowUp(m) }

// Continue continues from the current transcript (last message must be a user or
// tool-result message, or a queued message must exist).
func (s *Session) Continue(ctx context.Context) error { return s.Agent.Continue(ctx) }

// Abort cancels the in-flight run, if any.
func (s *Session) Abort() { s.Agent.Abort() }

// WaitForIdle blocks until the current run and its listeners finish.
func (s *Session) WaitForIdle() { s.Agent.WaitForIdle() }

// Run executes a prompt and returns a structured RunResult. Unlike RunPrint it
// does not write to an io.Writer — use Subscribe for streaming.
func (s *Session) Run(ctx context.Context, prompt string, images ...ai.ImageContent) (*RunResult, error) {
	content := ai.ContentList{ai.TextContent{Text: prompt}}
	for _, img := range images {
		content = append(content, img)
	}
	return s.RunMessages(ctx, []agent.AgentMessage{ai.UserMessage{Content: content, Timestamp: nowMillisCoding()}})
}

// RunMessages executes explicit prompt messages and returns a structured result.
func (s *Session) RunMessages(ctx context.Context, prompts []agent.AgentMessage) (*RunResult, error) {
	before := len(s.Agent.State().Messages)

	result := &RunResult{}
	unsub := s.Agent.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		if e.Type != agent.EvMessageEnd {
			return nil
		}
		if am, ok := messageAsAssistant(e.Message); ok {
			addUsage(&result.Usage, am.Usage)
		}
		return nil
	})
	defer unsub()

	if err := s.Agent.PromptMessages(ctx, prompts); err != nil {
		return nil, err
	}

	st := s.Agent.State()
	if before <= len(st.Messages) {
		result.Messages = append([]agent.AgentMessage(nil), st.Messages[before:]...)
	}
	for _, m := range result.Messages {
		if am, ok := messageAsAssistant(m); ok {
			for _, c := range am.Content {
				if tc, ok := c.(ai.ToolCall); ok {
					result.ToolCalls = append(result.ToolCalls, tc)
				}
			}
			result.StopReason = am.StopReason
		}
	}
	result.Text = s.LastAssistantText()
	result.ErrorMessage = st.ErrorMessage
	if result.ErrorMessage != "" {
		return result, fmt.Errorf("%s", result.ErrorMessage)
	}
	return result, nil
}

func messageAsAssistant(m agent.AgentMessage) (*ai.AssistantMessage, bool) {
	switch v := m.(type) {
	case *ai.AssistantMessage:
		return v, true
	case ai.AssistantMessage:
		return &v, true
	}
	return nil, false
}

// addUsage accumulates token counts and cost into dst.
func addUsage(dst *ai.Usage, u ai.Usage) {
	dst.Input += u.Input
	dst.Output += u.Output
	dst.CacheRead += u.CacheRead
	dst.CacheWrite += u.CacheWrite
	dst.TotalTokens += u.TotalTokens
	dst.Cost.Input += u.Cost.Input
	dst.Cost.Output += u.Cost.Output
	dst.Cost.CacheRead += u.Cost.CacheRead
	dst.Cost.CacheWrite += u.Cost.CacheWrite
	dst.Cost.Total += u.Cost.Total
}

func nowMillisCoding() int64 { return time.Now().UnixMilli() }

// RunPrint runs a single prompt and renders streaming output to w, returning the
// final assistant text. Tool activity is rendered as compact status lines.
func (s *Session) RunPrint(ctx context.Context, w io.Writer, prompt string) (string, error) {
	var lastTextLen int
	unsub := s.Agent.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		switch e.Type {
		case agent.EvMessageUpdate:
			if e.AssistantMessageEvent != nil && e.AssistantMessageEvent.Type == ai.EventTextDelta {
				fmt.Fprint(w, e.AssistantMessageEvent.Delta)
				lastTextLen += len(e.AssistantMessageEvent.Delta)
			}
		case agent.EvToolExecutionStart:
			fmt.Fprintf(w, "\n\033[2m· %s(%s)\033[0m\n", e.ToolName, compactArgs(e.Args))
		case agent.EvToolExecutionEnd:
			status := "ok"
			if e.IsError {
				status = "error"
			}
			fmt.Fprintf(w, "\033[2m  └ %s\033[0m\n", status)
		}
		return nil
	})
	defer unsub()

	if err := s.Agent.Prompt(ctx, prompt); err != nil {
		return "", err
	}
	st := s.Agent.State()
	if st.ErrorMessage != "" {
		return "", fmt.Errorf("%s", st.ErrorMessage)
	}
	return lastAssistantText(st.Messages), nil
}

func compactArgs(args map[string]any) string {
	for _, k := range []string{"command", "path", "pattern"} {
		if v, ok := args[k].(string); ok {
			if len(v) > 60 {
				v = v[:57] + "..."
			}
			return v
		}
	}
	return ""
}

func lastAssistantText(messages []agent.AgentMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		var am *ai.AssistantMessage
		switch v := messages[i].(type) {
		case *ai.AssistantMessage:
			am = v
		case ai.AssistantMessage:
			am = &v
		default:
			continue
		}
		var parts []string
		for _, c := range am.Content {
			if tc, ok := c.(ai.TextContent); ok {
				parts = append(parts, tc.Text)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}
