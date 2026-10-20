package coding

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
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
	// ToolNames is an allowlist of tool names or patterns, where `*` matches
	// any characters (pi createAgentSession's tools). When nil and NoTools is
	// off, the default set [read, bash, edit, write] is used. A list of only
	// `+name` and `-name` entries is not an allowlist: it adds tools to or
	// removes them from the default selection, and takes exact names; mixing
	// them with plain entries makes NewSession fail.
	ToolNames []string
	// ExcludeTools is a denylist of tool names or patterns applied after
	// ToolNames.
	ExcludeTools []string
	// NoTools suppresses the default built-in tools ("all" or "builtin").
	NoTools NoToolsMode
	// CustomTools are appended to the resolved built-in set.
	CustomTools []agent.AgentTool

	SystemPrompt  string
	ThinkingLevel agent.ThinkingLevel
	SessionID     string

	// TrustProject enables discovery of project-local resources under
	// <cwd>/.pi — currently the skills directory. It is pi's isProjectTrusted()
	// and it defaults to FALSE, which is the answer pi itself gives a host with
	// no UI to prompt with (project-trust.ts). Set it only after trust has
	// actually been established: a project skill's name and description reach
	// the system prompt, so an untrusted repo would otherwise get to author part
	// of the prompt. See LoadSkillsWithTrust.
	TrustProject bool

	// Models is the session's model runtime (pi createAgentSession's
	// modelRuntime): every request the session makes — turns, compaction and
	// branch summaries — streams through its StreamSimple, which resolves the
	// request's auth from its credential store and the environment as the
	// model's provider does, and a summary takes the auth it resolves. A
	// session holds no API key of its own, as pi's does not: a host that has
	// a key sets it on the runtime's credentials (see RuntimeCredentials).
	// Nil streams through the compat global ai.StreamSimple, which resolves
	// each request's key from the environment — pi's default runtime reads
	// auth.json as well, which the port does not have.
	Models ai.Models

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
	// (e.g. OpenAI-Organization). A nil value suppresses a provider default
	// header of that name (see ai.ProviderHeaders).
	Headers ai.ProviderHeaders
	// OnPayload can inspect/replace the provider request body before sending.
	OnPayload func(payload any, model *ai.Model) (any, error)
	// OnResponse is invoked after the HTTP response is received.
	OnResponse func(resp ai.ProviderResponse, model *ai.Model) error
	// OnProviderStreamEvent observes each parsed provider stream event before
	// it is normalized, for adapters that support it (see
	// ai.StreamOptions.OnProviderStreamEvent). It is the native equivalent of
	// pi's provider_stream_event extension event (upstream 002fc8385): data is
	// the event's data, and model carries the event's provider, api and model
	// as Provider, Api and ID. It keeps pi-ai's semantics — the callback is
	// awaited and a non-nil error fails the stream — where pi's extension
	// runner reports a handler's throw and leaves the response alone.
	OnProviderStreamEvent func(data any, model *ai.Model) error
	// BeforeToolCall runs after a tool call's args are validated and before it
	// executes. Return {Block:true, Reason:...} to deny it (the loop emits an
	// error tool result). This is the native equivalent of pi's tool_call
	// extension hook — use it for permission gates, path protection, etc.
	BeforeToolCall agent.BeforeToolCallFunc
	// AfterToolCall runs after a tool finishes; return overrides for the result.
	AfterToolCall agent.AfterToolCallFunc

	// Compaction, when non-nil, installs automatic context-window compaction.
	// Use DefaultCompactionSettings() for pi's defaults.
	Compaction *CompactionSettings
	// StreamFn overrides the stream function (for tests). Default: Models'
	// StreamSimple, or ai.StreamSimple without Models. Either way the session
	// asserts a chat model first (see sessionStreamFn).
	StreamFn agent.StreamFn
}

// sessionStreamFn is the stream function a session's agent uses for every
// request it makes — turns, and the compaction and branch summaries that reuse
// the agent's stream function. pi's createAgentSession streams all of them
// through ModelRuntime.streamSimple, whose first act is assertChatModel
// (upstream a328aa89a), so a model of another type fails with "Model
// <provider>/<id> is not a chat model" before any provider dispatch. The port's
// session streams through models' StreamSimple, which asserts it too, or,
// without models, through ai.StreamSimple, the compat global, which asserts
// nothing (pi's compat streamSimple does not either), so the session asserts
// here and then delegates to next, or to models or ai.StreamSimple when next
// is nil.
func sessionStreamFn(models ai.Models, next agent.StreamFn) agent.StreamFn {
	if next == nil {
		next = func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return ai.StreamSimple(ctx, model, ai.Context{Messages: req.Messages}, opts)
		}
		if models != nil {
			next = func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				var options *ai.ModelsSimpleStreamOptions
				if opts != nil {
					options = &ai.ModelsSimpleStreamOptions{SimpleStreamOptions: *opts}
				}
				return models.StreamSimple(ctx, model, ai.Context{Messages: req.Messages}, options)
			}
		}
	}
	return func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		if err := ai.AssertChatModel(model); err != nil {
			return ai.ErrorStream(model, err)
		}
		return next(ctx, model, req, opts)
	}
}

// defaultActiveToolNames is pi's DEFAULT_TOOL_NAMES: the tools enabled at
// startup when nothing changes them.
var defaultActiveToolNames = []string{"read", "bash", "edit", "write"}

// toolPatternRegexp is pi's toolPatternRegExp (core/mcp-servers.ts): `*`
// matches any characters, every other character is literal, and the whole
// name must match. JS's `.` stops at a line terminator, so `*` does too.
func toolPatternRegexp(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile(`^` + strings.Join(parts, `[^\n\r\x{2028}\x{2029}]*`) + `$`)
}

// toolNameMatcher is pi's createToolNameMatcher (upstream 04b97ef00): whether
// a tool name matches any of the entries, each an exact name or a pattern
// where `*` matches any characters.
func toolNameMatcher(entries []string) func(name string) bool {
	names := map[string]bool{}
	var patterns []*regexp.Regexp
	for _, entry := range entries {
		if strings.Contains(entry, "*") {
			patterns = append(patterns, toolPatternRegexp(entry))
		} else {
			names[entry] = true
		}
	}
	return func(name string) bool {
		return names[name] || slices.ContainsFunc(patterns, func(pattern *regexp.Regexp) bool {
			return pattern.MatchString(name)
		})
	}
}

// isToolModifier is pi's isToolModifier: whether a tool list entry is a
// `+name` or `-name` modifier.
func isToolModifier(entry string) bool {
	return strings.HasPrefix(entry, "+") || strings.HasPrefix(entry, "-")
}

// toolListError is pi's getToolListError (upstream ddaa0a034): a tool list is
// either an allowlist of names and patterns or only `+name`/`-name` entries
// with exact names. It returns the problem, or "" for a valid list.
func toolListError(entries []string) string {
	modifiers := 0
	for _, entry := range entries {
		if isToolModifier(entry) {
			modifiers++
		}
	}
	if modifiers == 0 {
		return ""
	}
	if modifiers < len(entries) {
		return "tool names cannot be mixed with +name or -name entries"
	}
	for _, entry := range entries {
		if strings.Contains(entry, "*") {
			return "+name and -name entries take exact tool names, not patterns: " + entry
		}
	}
	return ""
}

// applyToolModifiers is pi's applyToolModifiers: the `+name` and `-name`
// entries applied to base in order — `+name` adds a tool not yet there,
// `-name` removes one. Other entries are ignored. The result is never nil,
// as pi's is always an array: under NoTools "all" it is the allowlist, and
// a nil one would allow everything.
func applyToolModifiers(base, entries []string) []string {
	tools := append([]string{}, base...)
	for _, entry := range entries {
		if !isToolModifier(entry) {
			continue
		}
		name := entry[1:]
		index := slices.Index(tools, name)
		if strings.HasPrefix(entry, "+") && index == -1 && name != "" {
			tools = append(tools, name)
		} else if strings.HasPrefix(entry, "-") && index != -1 {
			tools = slices.Delete(tools, index, index+1)
		}
	}
	return tools
}

// invalidToolsOptionError is pi's "Invalid tools option" throw, with how to
// write a valid list.
func invalidToolsOptionError(problem string) error {
	return fmt.Errorf("Invalid tools option: %s (pass either tool names and * patterns, or only +name/-name entries naming exact tools, e.g. [\"+grep\", \"-write\"])", problem)
}

// resolveTools builds the active tool set, porting pi's selection:
// createAgentSession (sdk.ts) validates the tools list and resolves the
// initial selection, and agent-session.ts _refreshToolRegistry passes EVERY
// tool — built-in and custom — through _isAllowedTool (allowlist, then the
// excludeTools denylist). Consequences: NoTools "all" disables custom tools
// too; a ToolNames allowlist constrains custom tools; ExcludeTools applies to
// custom tools. ToolNames and ExcludeTools entries are names or `*` patterns
// (upstream 04b97ef00). A ToolNames list of only `+name`/`-name` entries is not
// an allowlist: it adds tools to or removes them from the default selection
// (upstream ddaa0a034). An invalid list fails with pi's message.
//
// Not ported: 04b97ef00's MCP retention (an allowlist without `mcp__` entries
// keeping MCP tools registered for codemode and tool_search), which needs MCP
// and tool exposure — Scope rows 21 and 20.
func resolveTools(cwd string, opts SessionOptions, sessionEnv sessionEnvFn, model toolModelFn) ([]agent.AgentTool, error) {
	if problem := toolListError(opts.ToolNames); problem != "" {
		return nil, invalidToolsOptionError(problem)
	}
	var baseNames []string
	for _, t := range opts.Tools {
		baseNames = append(baseNames, t.Name)
	}
	// The default selection: none under NoTools, the Tools override's names
	// (it plays pi's baseToolsOverride, agent-session.ts _buildRuntime), else
	// pi's DEFAULT_TOOL_NAMES. The port has no defaultTools setting.
	var defaultNames []string
	switch {
	case opts.NoTools != NoToolsOff:
	case opts.Tools != nil:
		defaultNames = baseNames
	default:
		defaultNames = defaultActiveToolNames
	}
	// sdk.ts: a list of only modifiers changes the default selection instead
	// of replacing it, and is an allowlist only under NoTools "all".
	var toolModifiers []string
	if slices.ContainsFunc(opts.ToolNames, isToolModifier) {
		toolModifiers = opts.ToolNames
	}
	selected := opts.ToolNames
	var allowedEntries []string // nil = everything allowed (pi: undefined)
	switch {
	case toolModifiers != nil:
		selected = applyToolModifiers(defaultNames, toolModifiers)
		if opts.NoTools == NoToolsAll {
			allowedEntries = selected
		}
	case opts.ToolNames != nil:
		allowedEntries = opts.ToolNames
	case opts.NoTools == NoToolsAll:
		allowedEntries = []string{}
	}
	var allowed func(string) bool
	if allowedEntries != nil {
		allowed = toolNameMatcher(allowedEntries)
	}
	excluded := toolNameMatcher(opts.ExcludeTools)
	isAllowed := func(name string) bool {
		return !excluded(name) && (allowed == nil || allowed(name))
	}

	// Registry: base definitions (the verbatim Tools override, like pi's
	// baseToolsOverride, or the built-in factory set) then custom tools, all
	// filtered through isAllowedTool. Custom tools override same-named built-ins
	// (pi sets them into the registry after the built-ins).
	registry := map[string]agent.AgentTool{}
	var registryOrder []string
	addReg := func(t agent.AgentTool) {
		if _, ok := registry[t.Name]; !ok {
			registryOrder = append(registryOrder, t.Name)
		}
		registry[t.Name] = t
	}
	if opts.Tools != nil {
		for _, t := range opts.Tools {
			if isAllowed(t.Name) {
				addReg(t)
			}
		}
	} else {
		for _, name := range ToolNames {
			if !isAllowed(name) {
				continue
			}
			if t, err := createTool(name, cwd, sessionEnv, model); err == nil {
				addReg(t)
			}
		}
		// Extra built-ins beyond pi's core set (e.g. web_fetch) are opt-in: a
		// selected name CreateTool knows admits one. A pattern never does.
		for _, name := range selected {
			if _, ok := registry[name]; ok || !isAllowed(name) {
				continue
			}
			if t, err := createTool(name, cwd, sessionEnv, model); err == nil {
				addReg(t)
			}
		}
	}
	var customNames []string
	for _, t := range opts.CustomTools {
		if !isAllowed(t.Name) {
			continue
		}
		addReg(t)
		customNames = append(customNames, t.Name)
	}

	// Initial active names (sdk.ts): the selection, else the default one,
	// filtered by excludeTools. A pattern names no registry tool, so it adds
	// nothing here; the allowlist pass below activates what it matches.
	initial := selected
	if initial == nil {
		initial = defaultNames
	}

	// _refreshToolRegistry: start from the initial names (filtered through
	// isAllowedTool); with an allowlist, every registry tool it names or
	// matches is activated; without one, all custom tools are activated
	// (includeAllExtensionTools on session construction). Dedupe keeps first.
	seen := map[string]bool{}
	var active []agent.AgentTool
	push := func(name string) {
		if seen[name] || !isAllowed(name) {
			return
		}
		t, ok := registry[name]
		if !ok {
			return
		}
		seen[name] = true
		active = append(active, t)
	}
	for _, n := range initial {
		push(n)
	}
	if allowed != nil {
		for _, n := range registryOrder {
			if allowed(n) {
				push(n)
			}
		}
	} else {
		for _, n := range customNames {
			push(n)
		}
	}
	return active, nil
}

// toolPromptGuidelines maps each tool with prompt guidelines to them, as
// agent-session.ts builds _toolPromptGuidelines for _rebuildSystemPrompt: a
// tool's guidelines are normalized (_normalizePromptGuidelines: trimmed as JS
// trims, empties dropped, deduplicated in first-occurrence order) and a tool
// left with none has no entry. BuildSystemPromptSections folds the selected
// tools' guidelines into the rules section in tool order and deduplicates
// across tools.
func toolPromptGuidelines(tools []agent.AgentTool) map[string][]string {
	guidelines := map[string][]string{}
	for _, tool := range tools {
		var normalized []string
		for _, guideline := range tool.PromptGuidelines {
			guideline = jstext.Trim(guideline)
			if guideline != "" && !slices.Contains(normalized, guideline) {
				normalized = append(normalized, guideline)
			}
		}
		if len(normalized) > 0 {
			guidelines[tool.Name] = normalized
		}
	}
	return guidelines
}

// Session is a coding-agent session: an Agent wired with a model, tools, and the
// coding system prompt.
type Session struct {
	Agent    *agent.Agent
	Model    *ai.Model
	Cwd      string
	Recorder *SessionRecorder
	models   ai.Models
	// recMu guards Recorder against the tool-execution goroutine reading it for
	// bash session metadata while Record attaches one.
	recMu sync.RWMutex
	// systemPromptOptions are the normalized prompt inputs a prompt declares
	// (pi AgentSession._baseSystemPromptOptions). They are fixed for the
	// session: the tool loadout does not change after NewSession.
	systemPromptOptions BuildSystemPromptOptions
	// compactTransform is the compaction stage of the chain
	// installTransformContext installs, filled by EnableCompaction. It is a slot
	// rather than the Agent.TransformContext field so enabling compaction after
	// NewSession neither discards the chain nor reorders it.
	compactTransform func(ctx context.Context, messages []agent.AgentMessage) []agent.AgentMessage
	// compactState is the compaction checkpoint, kept across EnableCompaction
	// calls so a re-enable cannot drop a summary already taken.
	compactState *compactionState
}

// bashSessionEnv returns the PI_* session metadata exposed to bash commands
// (pi bb3d7d39): the session id and file come from the recorder, the closest
// Go analog of pi's session manager, so both are absent until one is attached.
// pi assigns its session file at session start and omits PI_SESSION_FILE only
// when persistence is off; the observable behavior matches, but note an SDK
// embedder that never calls Record gets no PI_SESSION_ID either, where pi would
// still have one. The model and reasoning level are read live, so they follow
// /model and thinking-level changes.
// It runs on the tool-execution goroutine while the main loop may be switching
// models or attaching a recorder, so every field it reads is taken through a
// synchronized path: the model and thinking level from the agent's guarded
// state, the recorder under recMu.
func (s *Session) bashSessionEnv() map[string]string {
	env := map[string]string{}
	if r := s.recorder(); r != nil {
		env["PI_SESSION_ID"] = r.ID()
		env["PI_SESSION_FILE"] = r.Path()
	}
	// One snapshot: State() copies under the agent's mutex, so a single call is
	// both race-free and internally consistent — model and level describe the
	// same instant even if /model lands mid-read. SetModel writes the agent's
	// model too, so this is the genuinely live value.
	st := s.Agent.State()
	if m := st.Model; m != nil {
		env["PI_PROVIDER"] = m.Provider
		env["PI_MODEL"] = m.ID
	}
	// pi guards on truthiness only, and "off" is truthy there — an explicitly
	// disabled reasoning level is still reported.
	if level := st.ThinkingLevel; level != "" {
		env["PI_REASONING_LEVEL"] = string(level)
	}
	return env
}

// currentModel is the session's CURRENT model — pi's `this.model`
// (agent-session.ts) and the model of the request a tool runs for — read per
// call rather than captured so SetModel reaches the next tool call and image
// without rebuilding the tools. Like bashSessionEnv it runs on the
// tool-execution goroutine while the caller may be switching models, so it
// reads the model from ONE snapshot of the agent's guarded state — pi reads
// `this.agent.state.model` for the same reason — and never the plain
// Session.Model field, which SetModel writes without a lock.
func (s *Session) currentModel() *ai.Model {
	return s.Agent.State().Model
}

// recorder reads the attached SessionRecorder under recMu. Every read goes
// through here: bash commands read it from the tool-execution goroutine while
// the main loop may be attaching one.
func (s *Session) recorder() *SessionRecorder {
	s.recMu.RLock()
	defer s.recMu.RUnlock()
	return s.Recorder
}

// Record attaches a SessionRecorder; finalized messages are appended to it.
// The write is guarded because bash commands read the recorder concurrently for
// their PI_SESSION_ID/PI_SESSION_FILE metadata. Assigning the exported Recorder
// field directly bypasses that guard — use this method.
//
// Like pi's _handleAgentEvent (agent-session.ts:925-945), it persists system,
// user, assistant and toolResult messages and no other role, so an app's
// UI-only message stays out of the file. pi also writes its own custom
// messages as custom_message entries; the port has no such message type.
func (s *Session) Record(r *SessionRecorder) {
	s.recMu.Lock()
	s.Recorder = r
	s.recMu.Unlock()
	if r == nil {
		return
	}
	s.Agent.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		if e.Type != agent.EvMessageEnd {
			return nil
		}
		switch e.Message.MessageRole() {
		case ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			r.RecordMessage(e.Message)
		}
		return nil
	})
}

// LoadHistory replaces the transcript with a prior session's messages. It
// drops the session's compaction, which indexes the transcript it replaces; to
// resume a session file together with its compaction, use LoadBranch.
func (s *Session) LoadHistory(messages []agent.AgentMessage) {
	s.Agent.SetMessages(messages)
	s.setCompaction(nil, 0)
}

// LoadBranch resumes a session branch from its projection
// (SessionTree.BuildProjection): its messages become the transcript and its
// newest compaction the session's checkpoint, so the next compaction extends
// it as pi's does — from the messages it kept, with its summary as the
// previous summary and its file lists merged. An assistant usage recorded
// before the branch's latest compaction or context edit measured a context
// that has since changed, so, as in pi, it does not decide the next
// compaction.
func (s *Session) LoadBranch(p BranchProjection) {
	s.Agent.SetMessages(p.Messages)
	s.setCompaction(resumedCompaction(p))
}

// SetModel switches the active model for future turns. As pi's setModel, it
// takes no key: the session's requests resolve the new model's auth as they
// did the old one's (see SessionOptions.Models).
func (s *Session) SetModel(model *ai.Model) {
	s.Model = model
	s.Agent.SetModel(model)
	if r := s.recorder(); r != nil {
		r.RecordModelChange(model.Provider, model.ID)
	}
}

// SetThinkingLevel sets the reasoning level for future turns.
func (s *Session) SetThinkingLevel(level agent.ThinkingLevel) {
	s.Agent.SetThinkingLevel(level)
	if r := s.recorder(); r != nil {
		r.RecordThinkingLevel(string(level))
	}
}

// History returns the current transcript.
func (s *Session) History() []agent.AgentMessage { return s.Agent.State().Messages }

// SystemPrompt returns the session's current effective system prompt,
// including sections not yet declared to the model (pi AgentSession's
// systemPrompt getter). It is built from the session's prompt options, so it is
// available before the first Run, while Agent.State().SystemPrompt replays only
// what the transcript has declared so far. The error is BuildSystemPrompt's.
func (s *Session) SystemPrompt() (string, error) {
	return BuildSystemPrompt(s.systemPromptOptions)
}

// Reset clears the transcript and drops its compaction: a new transcript
// starts with none, as pi's /new starts a fresh SessionManager. It errors while
// a run is active.
func (s *Session) Reset() error {
	if err := s.Agent.Reset(); err != nil {
		return err
	}
	s.setCompaction(nil, 0)
	return nil
}

// LastAssistantText returns the most recent assistant message text.
func (s *Session) LastAssistantText() string {
	return lastAssistantText(s.Agent.State().Messages)
}

// NewSession builds a Session. If Tools is nil, the default coding tools are used;
// if SystemPrompt is empty, the default prompt is built from the tool set. The
// prompt is not part of the transcript until the first prompt declares it. It
// fails, as pi's createAgentSession throws, when ToolNames mixes tool names
// with +name/-name entries or a modifier names a pattern.
func NewSession(opts SessionOptions) (*Session, error) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// The bash tool reads session metadata through this indirection because the
	// recorder, model, and thinking level all change during the session (pi
	// reads them off the live ExtensionContext per call). The Session is
	// allocated up front so the closure captures a stable, non-nil pointer; its
	// Agent is filled in below, before NewSession returns and any tool can run.
	sess := &Session{Cwd: cwd, Model: opts.Model, models: opts.Models}
	tools, err := resolveTools(cwd, opts, sess.bashSessionEnv, sess.currentModel)
	if err != nil {
		return nil, err
	}
	// A custom SystemPrompt still goes through the prompt builder with discovery:
	// pi adds project context files, skills and cwd to custom prompts too; only
	// the tools, rules and docs sections are exclusive to the default prompt.
	// names is non-nil even when empty: pi passes the concrete (possibly empty)
	// active-tool list, never undefined, so the builder must not fall back to
	// its [read,bash,edit,write] default. Snippets cover every built-in tool,
	// as pi's cover every registry tool; the builder shows only selected ones.
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sess.systemPromptOptions = NormalizeBuildSystemPromptOptions(BuildSystemPromptOptions{
		CustomPrompt:   opts.SystemPrompt,
		SelectedTools:  names,
		ToolSnippets:   ToolSnippets,
		ToolGuidelines: toolPromptGuidelines(tools),
		Cwd:            cwd,
		ContextFiles:   LoadProjectContextFiles(cwd),
		Skills:         sessionSkills(cwd, opts.TrustProject),
	})
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

	// pi's sdk.ts builds the Agent with no prompt and no tools: seeding either
	// would put an event-less system message at the head of the transcript
	// that the session never records. The tools are set directly (the loop
	// declares them with the first request); each prompt declares the system
	// prompt sections it needs (declareSystemPrompt), and so does every later
	// turn (prepareNextTurn).
	a := agent.NewAgent(agent.AgentOptions{
		PrepareNextTurn: sess.prepareNextTurn,
		InitialState: &agent.AgentState{
			Model:         opts.Model,
			ThinkingLevel: thinking,
		},
		StreamFn:        sessionStreamFn(opts.Models, opts.StreamFn),
		SessionID:       opts.SessionID,
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
		AfterToolCall:   withToolResultImageNormalization(opts.AfterToolCall, sess.currentModel),

		// pi's createAgentSession wires the extension runner's
		// provider_stream_event handler here, beside onPayload/onResponse.
		OnProviderStreamEvent: opts.OnProviderStreamEvent,
	})

	a.SetTools(tools)
	sess.Agent = a
	sess.installTransformContext()
	if opts.Compaction != nil && opts.Compaction.Enabled {
		sess.EnableCompaction(*opts.Compaction)
	}
	return sess, nil
}

// installTransformContext installs the session's single per-request transform,
// as pi's AgentSession constructor installs _installAgentForcedPromptProjection
// over whatever transformContext is already there. Order, outermost last:
// whatever was installed before, then compaction (docs/UPSTREAM.md D4 — pi
// compacts in the prompt path instead), then the forced-prompt projection,
// which must be last because it rewrites the head the others produce.
//
// Compaction lives in a SLOT rather than in the field, so EnableCompaction can
// be called later without discarding this chain — and so an embedder who wraps
// Agent.TransformContext after NewSession keeps their wrapper, since nothing
// reassigns the field afterwards.
func (s *Session) installTransformContext() {
	previous := s.Agent.TransformContext
	s.Agent.TransformContext = func(ctx context.Context, messages []agent.AgentMessage) []agent.AgentMessage {
		transformed := messages
		if previous != nil {
			transformed = previous(ctx, messages)
		}
		if compact := s.compactTransform; compact != nil {
			transformed = compact(ctx, transformed)
		}
		return s.projectForcedPrompt(ctx, transformed)
	}
}

// projectForcedPrompt is pi's _installAgentForcedPromptProjection (upstream
// 16292398a).
//
// A before_agent_start handler that forces the whole prompt needs that exact
// text at the head of the REQUEST; a mid-conversation system message would
// leave the original prompt in place, and recording the forced text would lose
// the structured sections the transcript is built from. So the forced text is
// projected onto the request instead: the system messages collapse into one
// head holding it and the tools the transcript currently declares, keeping the
// head's timestamp so a stable forced prompt is a stable cache prefix.
//
// The port's public options carry no forced prompt — pi's comes from a
// before_agent_start handler, which is Scope entry 12 — so today only
// systemPromptOptions.ForceSystemPrompt reaches this, as it does
// BuildSystemPromptState.
func (s *Session) projectForcedPrompt(_ context.Context, transformed []agent.AgentMessage) []agent.AgentMessage {
	{
		forced := s.systemPromptOptions.ForceSystemPrompt
		if forced == nil {
			return transformed
		}
		timestamp := nowMillisCoding()
		head := ai.NewSystemText(*forced, timestamp)
		if current, found := ai.GetCurrentSystemMessage(transformed); found {
			// pi spreads `toolsAdded` only when the replayed head has one, so a
			// transcript that declares no tools yields a head without the key.
			head.ToolsAdded = current.ToolsAdded
			head.Timestamp = current.Timestamp
		}
		out := make([]agent.AgentMessage, 0, len(transformed)+1)
		out = append(out, head)
		for _, message := range transformed {
			// By ROLE, as pi's `m.role !== "system"` is, and as
			// CollapseSystemMessages and withoutSystemMessages are. A concrete-type
			// check would miss a *ai.SystemMessage — which GetCurrentSystemMessage
			// two lines up DOES read — and leave the prompt this head replaces in
			// the request beside it.
			if message.MessageRole() == ai.RoleSystem {
				continue
			}
			out = append(out, message)
		}
		return out
	}
}

// withToolResultImageNormalization wraps the caller's AfterToolCall hook so that
// images returned by tools are normalized as they enter session history (pi
// agent-session.ts afterToolCall). Normalization runs AFTER the hook — pi runs
// it after the tool_result extension hook — so images the hook injects or
// replaces are normalized too. When there is no hook result and normalization
// changed nothing, the tool result is left untouched.
func withToolResultImageNormalization(hook agent.AfterToolCallFunc, model toolModelFn) agent.AfterToolCallFunc {
	return func(ctx context.Context, c agent.AfterToolCallContext) *agent.AfterToolCallResult {
		var hookResult *agent.AfterToolCallResult
		if hook != nil {
			hookResult = hook(ctx, c)
		}

		content := c.Result.Content
		if hookResult != nil && hookResult.HasContent {
			content = hookResult.Content
		}
		normalized, changed := normalizeToolResultImages(content, imageResizeOptionsOf(model.get()))
		if hookResult == nil && !changed {
			return nil
		}

		out := agent.AfterToolCallResult{Content: normalized, HasContent: true}
		// Returned content drops structured content the result does not also
		// carry, so pass the tool's own on — unless the hook replaced the
		// content without supplying its own, which pi's tool_result runner
		// drops as no longer matching (upstream 8562bcf66).
		out.StructuredContent = c.Result.StructuredContent
		if hookResult != nil {
			if hookResult.StructuredContent != nil {
				out.StructuredContent = hookResult.StructuredContent
			} else if hookResult.HasContent {
				out.StructuredContent = nil
			}
			// Everything the hook decided other than content is passed through
			// verbatim, including Terminate (pi's extension hook has no terminate,
			// but the SDK's own AfterToolCall does).
			out.Details = hookResult.Details
			out.HasDetails = hookResult.HasDetails
			out.IsError = hookResult.IsError
			out.Usage = hookResult.Usage
			out.Terminate = hookResult.Terminate
		}
		return &out
	}
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

// normalizePromptImages is pi's _normalizePromptImages (agent-session.ts,
// upstream f5c946480): an image entering the transcript is converted to a
// supported inline type and resized against the CURRENT model's profile before
// it is recorded, and the pipeline's notes ride back on the user text instead
// of being dropped. An image the pipeline cannot handle contributes its message
// and no block, so the turn still goes out.
//
// Normalizing here rather than at the caller is the point of that commit: the
// model is only settled once the turn starts, and an image already in history
// must never be re-processed — that would invalidate the prompt cache.
//
// pi gates auto-resize on settingsManager.getImageAutoResize(); the port has no
// settings manager, so it is always on, as at the read tool and the tool-result
// normalizer.
func (s *Session) normalizePromptImages(images []ai.ImageContent) ([]ai.ImageContent, []string) {
	if len(images) == 0 {
		return nil, nil
	}
	resize := imageResizeOptionsOf(s.currentModel())
	var out []ai.ImageContent
	var hints []string
	for _, img := range images {
		// Whatever the payload decodes to goes to processImage, which is what
		// reports one that is not an image — pi's Buffer.from never throws.
		processed := processImage(decodeNodeBase64(img.Data), img.MimeType, true, resize)
		if !processed.Ok {
			hints = append(hints, processed.Message)
			continue
		}
		out = append(out, ai.ImageContent{Data: encodeBase64(processed.Data), MimeType: processed.MimeType})
		hints = append(hints, processed.Hints...)
	}
	return out, hints
}

// Run executes a prompt and returns a structured RunResult. Unlike RunPrint it
// does not write to an io.Writer — use Subscribe for streaming.
func (s *Session) Run(ctx context.Context, prompt string, images ...ai.ImageContent) (*RunResult, error) {
	normalized, hints := s.normalizePromptImages(images)
	text := prompt
	if len(hints) > 0 {
		text = prompt + "\n\n" + strings.Join(hints, "\n")
	}
	content := ai.ContentList{ai.TextContent{Text: text}}
	for _, img := range normalized {
		content = append(content, img)
	}
	return s.RunMessages(ctx, []agent.AgentMessage{ai.UserMessage{Content: content, Timestamp: nowMillisCoding()}})
}

// systemPromptUpdate is pi's _preparePromptAndToolLoadout for the session's
// fixed loadout: the sections built from the session's options are diffed
// against the ones messages replay, and a change becomes a
// {role: "system", content: "", sections: patch} message; it reports false
// when the prompt is unchanged. The agent loop attaches any tool declarations
// to the message.
//
// A forced prompt plays no part here (upstream 16292398a): it never reaches the
// transcript, which keeps recording the structured sections. The request gets
// the forced text from forcedPromptProjection instead.
func (s *Session) systemPromptUpdate(messages []agent.AgentMessage) (ai.SystemMessage, bool, error) {
	sections, err := BuildSystemPromptSections(s.systemPromptOptions)
	if err != nil {
		return ai.SystemMessage{}, false, err
	}
	var previous ai.SystemSections
	if current, found := ai.GetCurrentSystemMessage(messages); found {
		previous = current.Sections
	}
	patch, changed := DiffSystemPromptSections(previous, sections)
	if !changed {
		return ai.SystemMessage{}, false, nil
	}
	return ai.SystemMessage{Sections: patch, Timestamp: nowMillisCoding()}, true, nil
}

// declareSystemPrompt puts the system prompt sections the model does not have
// yet ahead of prompts (pi AgentSession.prompt → _preparePromptAndToolLoadout).
// An unchanged prompt adds nothing.
func (s *Session) declareSystemPrompt(prompts []agent.AgentMessage) ([]agent.AgentMessage, error) {
	update, ok, err := s.systemPromptUpdate(s.Agent.State().Messages)
	if err != nil || !ok {
		return prompts, err
	}
	return append([]agent.AgentMessage{update}, prompts...), nil
}

// prepareNextTurn is the agent's PrepareNextTurn, installed by NewSession as
// pi's AgentSession constructor installs _installAgentNextTurnRefresh: before
// every turn after a run's first — Run's and Continue's alike — it declares the
// prompt the turn's transcript does not replay yet (systemPromptUpdate), and
// hands the loop the context with the agent's tools, the agent's model and its
// thinking level, so a mid-run model or thinking-level change reaches the next
// request. A transcript without a system message that Continue resumes is
// declared from its second request, as in pi, whose agent.continue() declares
// nothing up front. Compaction stays in the per-request TransformContext
// (docs/UPSTREAM.md D4) instead of running here first.
//
// The session's options carry no custom sections, the only input
// BuildSystemPromptSections rejects, so the build cannot fail here; were it to,
// the turn would proceed undeclared, and the next Run reports the error.
func (s *Session) prepareNextTurn(turn agent.AgentTurnContext) *agent.AgentLoopTurnUpdate {
	st := s.Agent.State()
	next := agent.AgentContext{Messages: turn.Context.Messages, Tools: slices.Clone(st.Tools)}
	thinkingLevel := st.ThinkingLevel
	prepared := &agent.AgentLoopTurnUpdate{Context: &next, Model: st.Model, ThinkingLevel: &thinkingLevel}
	if update, ok, err := s.systemPromptUpdate(turn.Context.Messages); err == nil && ok {
		prepared.Messages = []agent.AgentMessage{update}
	}
	return prepared
}

// RunMessages executes explicit prompt messages and returns a structured result.
// The system prompt sections the model does not have yet are declared ahead of
// them, so Messages starts with that system message when there is one.
func (s *Session) RunMessages(ctx context.Context, prompts []agent.AgentMessage) (*RunResult, error) {
	before := len(s.Agent.State().Messages)
	prompts, err := s.declareSystemPrompt(prompts)
	if err != nil {
		return nil, err
	}

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

func messageAsToolResult(m agent.AgentMessage) (*ai.ToolResultMessage, bool) {
	switch v := m.(type) {
	case *ai.ToolResultMessage:
		return v, true
	case ai.ToolResultMessage:
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

	prompts, err := s.declareSystemPrompt([]agent.AgentMessage{
		ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: prompt}}, Timestamp: nowMillisCoding()},
	})
	if err != nil {
		return "", err
	}
	if err := s.Agent.PromptMessages(ctx, prompts); err != nil {
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
