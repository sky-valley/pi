package coding

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// The session declares its prompt in the transcript (upstream 9e05370b2): pi's
// sdk.ts builds the Agent with no prompt and no tools, and prompt() puts a
// sections patch ahead of the user message; the agent loop attaches the tool
// declarations to that pending system message. Both reach the provider and the
// session file like any other message.

func messageRoles(messages []agent.AgentMessage) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, string(m.MessageRole()))
	}
	return out
}

func sectionNames(sections ai.SystemSections) []string {
	names := []string{}
	for _, section := range sections.Entries() {
		names = append(names, section.Name)
	}
	return names
}

// expectedSessionPrompt is the prompt NewSession must declare for its default
// tools in cwd, built independently of the Session.
func expectedSessionPrompt(t *testing.T, cwd string) string {
	t.Helper()
	tools := resolveTestTools(t, cwd, SessionOptions{})
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return mustBuildSystemPrompt(t, BuildSystemPromptOptions{
		SelectedTools:  names,
		ToolSnippets:   ToolSnippets,
		ToolGuidelines: toolPromptGuidelines(tools),
		Cwd:            cwd,
		ContextFiles:   LoadProjectContextFiles(cwd),
		Skills:         sessionSkills(cwd, false),
	})
}

func fauxText(text string) providers.FauxResponseStep {
	return providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: text}}, ai.StopStop))
}

// system-prompt-updates.test.ts 'declares the prompt and tools once and reuses
// them across resume'.
func TestSessionDeclaresPromptAndToolsOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})

	reg.SetResponses([]providers.FauxResponseStep{fauxText("first"), fauxText("second")})
	if _, err := sess.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}

	history := sess.History()
	if got, want := messageRoles(history), []string{"system", "user", "assistant", "user", "assistant"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	head, ok := history[0].(ai.SystemMessage)
	if !ok {
		t.Fatalf("head is %T", history[0])
	}
	if content, isString := head.StringContent(); !isString || content != "" {
		t.Fatalf("head content = %q (string form %v), want \"\"", content, isString)
	}
	if got, want := sectionNames(head.Sections), []string{"preamble", "tools", "rules", "docs", "cwd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("head sections = %v, want %v", got, want)
	}
	if got, want := toolNames(head.ToolsAdded), []string{"read", "bash", "edit", "write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("head toolsAdded = %v, want %v", got, want)
	}
	if got, want := ai.GetSystemMessageText(head), expectedSessionPrompt(t, cwd); got != want {
		t.Fatalf("declared prompt drift:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if got, want := ai.GetSystemMessageText(head), sessionSystemPrompt(t, sess); got != want {
		t.Fatalf("declared prompt differs from SystemPrompt():\n--- declared ---\n%s\n--- SystemPrompt ---\n%s", got, want)
	}
}

// system-prompt-updates.test.ts 'keeps tool declarations stable across a
// session JSON round-trip': a resumed transcript replays to the same prompt and
// tools, so the next Run declares nothing.
func TestSessionResumedTranscriptDeclaresNothingNew(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})
	reg.SetResponses([]providers.FauxResponseStep{fauxText("first"), fauxText("second")})
	if _, err := sess.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}

	resumed := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})
	var decoded []agent.AgentMessage
	for _, m := range sess.History() {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		message, err := ai.UnmarshalMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, message)
	}
	resumed.LoadHistory(decoded)
	if _, err := resumed.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if got, want := messageRoles(resumed.History()), []string{"system", "user", "assistant", "user", "assistant"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v (nothing re-declared)", got, want)
	}
}

// A transcript declared under another tool loadout, resumed into this session,
// gets exactly the difference: the sections that changed and the tool removals.
func TestSessionResumedTranscriptDeclaresOnlyTheDifference(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	declaring := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})
	reg.SetResponses([]providers.FauxResponseStep{fauxText("first"), fauxText("second")})
	if _, err := declaring.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}

	resumed := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd, NoTools: NoToolsAll})
	resumed.LoadHistory(declaring.History())
	result, err := resumed.Run(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	patch, ok := result.Messages[0].(ai.SystemMessage)
	if !ok {
		t.Fatalf("the run must start with a declaration, got roles %v", messageRoles(result.Messages))
	}
	if got, want := sectionNames(patch.Sections), []string{"tools", "rules"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("patched sections = %v, want %v", got, want)
	}
	var removed []string
	for _, tool := range patch.ToolsRemoved {
		removed = append(removed, tool.Name)
	}
	if want := []string{"read", "bash", "edit", "write"}; !reflect.DeepEqual(removed, want) || patch.ToolsAdded != nil {
		t.Fatalf("tool changes = removed %v added %v, want removed %v", removed, patch.ToolsAdded, want)
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsonKeys(t, raw), []string{"role", "content", "sections", "timestamp", "toolsRemoved"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("patch keys = %v, want %v", got, want)
	}
}

// system-prompt-updates.test.ts 'opens a transcript without a system message
// and declares the prompt on the first request': a transcript written before
// transcript-owned prompts (every Go session file until now) gets nothing
// synthesized on load, then the whole declaration — sections and tools — with
// the next prompt.
func TestSessionDeclaresPromptForTranscriptWithoutSystemMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})
	sess.LoadHistory([]agent.AgentMessage{ai.NewUserText("existing", 1)})
	if got := messageRoles(sess.History()); !reflect.DeepEqual(got, []string{"user"}) {
		t.Fatalf("roles after load = %v, want [user]", got)
	}
	if _, ok := ai.GetCurrentSystemMessage(sess.History()); ok {
		t.Fatal("loading a transcript must not synthesize a system message")
	}

	var request ai.TranscriptContext
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			request = req
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
		},
	})
	if _, err := sess.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	history := sess.History()
	if got, want := messageRoles(history), []string{"user", "system", "user", "assistant"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	declaration := history[1].(ai.SystemMessage)
	if got, want := sectionNames(declaration.Sections), []string{"preamble", "tools", "rules", "docs", "cwd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("declared sections = %v, want %v", got, want)
	}
	if got, want := toolNames(declaration.ToolsAdded), []string{"read", "bash", "edit", "write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("declared tools = %v, want %v", got, want)
	}
	if got, want := ai.GetCurrentSystemPrompt(request.Messages), expectedSessionPrompt(t, cwd); got != want {
		t.Fatalf("request prompt drift:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// RunPrint prompts through the same declaration as Run.
func TestSessionRunPrintDeclaresPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})
	reg.SetResponses([]providers.FauxResponseStep{fauxText("ok")})
	if _, err := sess.RunPrint(context.Background(), &bytes.Buffer{}, "hi"); err != nil {
		t.Fatal(err)
	}
	head, ok := sess.History()[0].(ai.SystemMessage)
	if !ok || head.Sections.Len() == 0 {
		t.Fatalf("RunPrint must declare the sectioned prompt, history roles %v", messageRoles(sess.History()))
	}
}

// uiNotice is an app-defined, UI-only message, which agent.AgentMessage
// invites and ConvertToLlm drops before the provider.
type uiNotice struct{ Text string }

func (uiNotice) MessageRole() ai.Role { return "notice" }

// Session.Record writes what pi's _handleAgentEvent persists on message_end —
// system, user, assistant and toolResult messages — and no other role, so an
// app's UI-only message stays out of the session file
// (agent-session.ts:925-945 at 2b0a123de).
func TestSessionRecordSkipsUIOnlyMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd, Tools: CreateAllTools(cwd)})
	rec, err := StartSession(cwd, reg.GetModel(), "off")
	if err != nil {
		t.Fatal(err)
	}
	sess.Record(rec)
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			providers.FauxToolCall("ls", map[string]any{}, "c1"),
		}, ai.StopToolUse)),
		fauxText("ok"),
	})
	prompts := []agent.AgentMessage{uiNotice{Text: "indexing"}, ai.NewUserText("hi", 1)}
	if _, err := sess.RunMessages(t.Context(), prompts); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	want := []string{"session", "model_change", "thinking_level_change", "system", "user", "assistant", "toolResult", "assistant"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
}

// The recorder persists the declared system message like any other finished
// message (pi _handleAgentEvent's role === "system" arm): role "system", the
// sections in prompt order, an integer timestamp, and the tool declarations the
// loop attached after it. Reloading the file replays the same prompt text.
func TestSessionRecordsDeclaredSystemMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})
	rec, err := StartSession(cwd, reg.GetModel(), "off")
	if err != nil {
		t.Fatal(err)
	}
	sess.Record(rec)
	reg.SetResponses([]providers.FauxResponseStep{fauxText("ok")})
	if _, err := sess.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	file, err := os.Open(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var messageRolesInFile []string
	var system json.RawMessage
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for scanner.Scan() {
		var entry struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type != "message" {
			continue
		}
		var head struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(entry.Message, &head)
		messageRolesInFile = append(messageRolesInFile, head.Role)
		if head.Role == "system" {
			system = entry.Message
		}
	}
	if got, want := messageRolesInFile, []string{"system", "user", "assistant"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded message roles = %v, want %v", got, want)
	}
	if got, want := jsonKeys(t, system), []string{"role", "content", "sections", "timestamp", "toolsAdded"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded system message keys = %v, want %v", got, want)
	}
	var fields struct {
		Sections  json.RawMessage `json:"sections"`
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if err := json.Unmarshal(system, &fields); err != nil {
		t.Fatal(err)
	}
	if got, want := jsonKeys(t, fields.Sections), []string{"preamble", "tools", "rules", "docs", "cwd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded sections = %v, want %v", got, want)
	}
	if ts := string(fields.Timestamp); ts == "" || strings.ContainsAny(ts, ".eE\"") {
		t.Fatalf("recorded timestamp = %s, want an integer", ts)
	}

	messages, err := LoadSessionMessages(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ai.GetCurrentSystemPrompt(messages), expectedSessionPrompt(t, cwd); got != want {
		t.Fatalf("reloaded prompt drift:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// jsonKeys returns an object's keys in document order.
func jsonKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// system-prompt-updates.test.ts at 16292398a, 'a forced prompt is sent as the
// leading prompt for the run and never recorded'. The port has no
// before_agent_start (Scope entry 12), so prompts two and three set the options
// a handler returning that systemPrompt leaves, for their run only, as pi's run
// options are, and prompt three also adds the section pi's handler adds. Every
// recorded system message keeps the ordinary producer key order — a forced
// prompt writes none of its own.
func TestSessionForcedPromptIsSentButNeverRecorded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})
	rec, err := StartSession(cwd, reg.GetModel(), "off")
	if err != nil {
		t.Fatal(err)
	}
	sess.Record(rec)

	var requests []ai.TranscriptContext
	var steps []providers.FauxResponseStep
	texts := []string{"one", "two", "three", "four"}
	for _, text := range texts {
		reply := providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: text}}, ai.StopStop)
		steps = append(steps, func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			requests = append(requests, req)
			return reply
		})
	}
	reg.SetResponses(steps)
	force := "Exact prompt."
	for turn, text := range texts {
		if turn == 1 || turn == 2 {
			sess.systemPromptOptions.ForceSystemPrompt = &force
		}
		// pi's handler adds a section on the third turn, so the transcript has
		// something to record WHILE the prompt is forced.
		if turn == 2 {
			plan := "Plan only."
			sess.systemPromptOptions.Sections = ai.SystemSections{}
			sess.systemPromptOptions.Sections.Set("plan_mode", &plan)
		}
		if turn == 3 {
			sess.systemPromptOptions.Sections = nil
		}
		_, err := sess.Run(context.Background(), text)
		sess.systemPromptOptions.ForceSystemPrompt = nil
		if err != nil {
			t.Fatal(err)
		}
	}
	rec.Close()

	systemMessages := make([][]ai.SystemMessage, len(requests))
	counts := make([]int, len(requests))
	for i, request := range requests {
		for _, message := range request.Messages {
			if system, ok := message.(ai.SystemMessage); ok {
				systemMessages[i] = append(systemMessages[i], system)
			}
		}
		counts[i] = len(systemMessages[i])
	}
	// Forced turns collapse to one leading message; the unforced fourth turn
	// passes the recorded head and both plan_mode patches through.
	if want := []int{1, 1, 1, 3}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("system messages per request = %v, want %v", counts, want)
	}

	declared := systemMessages[0][0]
	forced := systemMessages[1][len(systemMessages[1])-1]
	if content, ok := forced.StringContent(); !ok || content != force || forced.Sections != nil || forced.ToolsRemoved != nil ||
		jsonText(t, forced.ToolsAdded) != jsonText(t, declared.ToolsAdded) || forced.Timestamp != declared.Timestamp {
		t.Fatalf("forced message = %s, want {role: system, content: %q, toolsAdded: the declared tools, timestamp: the head's}", jsonText(t, forced), force)
	}
	// The third request forces the same text over a transcript that has grown a
	// section patch: the projection still yields exactly the same head.
	if got := systemMessages[2][len(systemMessages[2])-1]; jsonText(t, got) != jsonText(t, forced) {
		t.Fatalf("third request head = %s, want the forced message %s", jsonText(t, got), jsonText(t, forced))
	}
	if got := ai.GetCurrentSystemPrompt(requests[2].Messages); got != force {
		t.Fatalf("replayed prompt of request three = %q, want %q", got, force)
	}
	// The projection leaves one system message, so there is nothing for a
	// native provider to keep in place.
	if got, want := messageRoles(requests[2].Messages), []string{"system", "user", "assistant", "user", "assistant", "user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request roles = %v, want %v", got, want)
	}

	prompt, err := sess.SystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if got := ai.GetCurrentSystemPrompt(sess.History()); got != prompt {
		t.Fatalf("replayed prompt drift from SystemPrompt():\n--- replayed ---\n%s\n--- SystemPrompt ---\n%s", got, prompt)
	}

	// The transcript records only the structured sections, never the forced text.
	messages, err := LoadSessionMessages(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	var recordedSections []string
	var recordedKeys [][]string
	for _, message := range messages {
		system, ok := message.(ai.SystemMessage)
		if !ok {
			continue
		}
		recordedSections = append(recordedSections, jsonText(t, system.Sections))
		raw, err := json.Marshal(system)
		if err != nil {
			t.Fatal(err)
		}
		recordedKeys = append(recordedKeys, jsonKeys(t, raw))
	}
	planAdded := ai.SystemSections{}
	planText := "<plan_mode>\nPlan only.\n</plan_mode>"
	planAdded.Set("plan_mode", &planText)
	planRemoved := ai.SystemSections{}
	planRemoved.Set("plan_mode", nil)
	wantSections := []string{
		jsonText(t, declared.Sections),
		jsonText(t, planAdded),
		jsonText(t, planRemoved),
	}
	if !reflect.DeepEqual(recordedSections, wantSections) {
		t.Fatalf("recorded sections = %v, want %v", recordedSections, wantSections)
	}
	wantKeys := [][]string{
		{"role", "content", "sections", "timestamp", "toolsAdded"},
		{"role", "content", "sections", "timestamp"},
		{"role", "content", "sections", "timestamp"},
	}
	if !reflect.DeepEqual(recordedKeys, wantKeys) {
		t.Fatalf("recorded system message keys = %v, want %v", recordedKeys, wantKeys)
	}
	if got := ai.GetCurrentSystemPrompt(messages); got != prompt {
		t.Fatalf("reloaded prompt drift:\n--- got ---\n%s\n--- want ---\n%s", got, prompt)
	}
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The forced-prompt projection drops the transcript's system messages by ROLE,
// as pi's `messages.filter(m => m.role !== "system")` does. A concrete-type
// check misses a *ai.SystemMessage, which GetCurrentSystemMessage — called in
// the same function — does read, so the prompt the head replaces would be left
// in the request beside it and the forced text would read as an ADDITION to it.
func TestSessionForcedPromptProjectionDropsPointerSystemMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})
	force := "Exact forced prompt."
	sess.systemPromptOptions.ForceSystemPrompt = &force

	recorded := ai.NewSystemText("ORIGINAL RECORDED PROMPT", 1000)
	sess.Agent.SetMessages([]agent.AgentMessage{&recorded, ai.NewUserText("hi", 2)})

	out := sess.Agent.TransformContext(context.Background(), sess.Agent.State().Messages)
	systems := 0
	for _, message := range out {
		if message.MessageRole() == ai.RoleSystem {
			systems++
		}
	}
	if systems != 1 {
		t.Fatalf("system messages after the projection = %d, want 1", systems)
	}
	if got := ai.GetCurrentSystemPrompt(out); got != force {
		t.Fatalf("replayed prompt = %q, want %q", got, force)
	}
}

// Compaction lives in a slot, not in Agent.TransformContext, so EnableCompaction
// after NewSession keeps a wrapper an embedder installed on the field — the only
// correct embedder pattern now that NewSession always installs a transform.
func TestSessionEnableCompactionKeepsAnEmbeddersTransform(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})

	called := false
	previous := sess.Agent.TransformContext
	sess.Agent.TransformContext = func(ctx context.Context, m []agent.AgentMessage) []agent.AgentMessage {
		called = true
		return previous(ctx, m)
	}
	sess.EnableCompaction(CompactionSettings{Enabled: true})
	sess.Agent.TransformContext(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)})
	if !called {
		t.Fatal("EnableCompaction dropped the embedder's TransformContext wrapper")
	}
}

// Re-enabling compaction updates the settings and keeps the checkpoint:
// compaction is permanent, so a fresh state would drop a summary already taken
// and let the turns it replaced reappear.
func TestSessionReEnableCompactionKeepsTheCheckpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})

	sess.EnableCompaction(CompactionSettings{Enabled: true, ReserveTokens: 1000})
	first := sess.compactState
	first.mu.Lock()
	checkpoint := &compactionCheckpoint{prefixLen: 3, compactedLen: 7, summary: "an existing summary"}
	first.checkpoint = checkpoint
	first.mu.Unlock()

	sess.EnableCompaction(CompactionSettings{Enabled: true, ReserveTokens: 2000})
	if sess.compactState != first {
		t.Fatal("re-enabling compaction replaced the checkpoint state")
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if first.checkpoint != checkpoint || checkpoint.summary != "an existing summary" || checkpoint.prefixLen != 3 || checkpoint.compactedLen != 7 {
		t.Fatalf("checkpoint lost: %+v", first.checkpoint)
	}
	if first.settings.ReserveTokens != 2000 {
		t.Fatalf("settings not updated: ReserveTokens = %d, want 2000", first.settings.ReserveTokens)
	}
}
