package coding

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// TestLiveMechanics exercises the core agent mechanics against the real OpenAI
// API (Responses path): tool loop, in-session memory, durable resume, skills,
// context compaction, and prompt caching. Skipped unless OPENAI_API_KEY is set.
//
//	OPENAI_API_KEY=sk-... go test ./coding/ -run TestLiveMechanics -v
func TestLiveMechanics(t *testing.T) {
	providers.RegisterBuiltins()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	model, err := ResolveModel("openai/gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("tool_loop", func(t *testing.T) { liveToolLoop(t, model, key) })
	t.Run("memory_in_session", func(t *testing.T) { liveMemoryInSession(t, model, key) })
	t.Run("memory_resume_from_disk", func(t *testing.T) { liveMemoryResume(t, model, key) })
	t.Run("skills", func(t *testing.T) { liveSkills(t, model, key) })
	t.Run("compaction", func(t *testing.T) { liveCompaction(t, model, key) })
	t.Run("prompt_caching", func(t *testing.T) { livePromptCaching(t, model, key) })
	t.Run("reasoning_roundtrip", func(t *testing.T) { liveReasoningRoundtrip(t, key) })
	t.Run("coding_workflow", func(t *testing.T) { liveCodingWorkflow(t, model, key) })
	t.Run("code_research_search", func(t *testing.T) { liveCodeResearch(t, model, key) })
	t.Run("abort_midstream", func(t *testing.T) { liveAbort(t, model, key) })
	t.Run("permission_gate", func(t *testing.T) { livePermissionGate(t, model, key) })
	t.Run("image_multimodal", func(t *testing.T) { liveImageMultimodal(t, model, key) })
}

// --- permission gate: BeforeToolCall blocks a tool live; it must not execute ---
func livePermissionGate(t *testing.T, model *ai.Model, key string) {
	var executed bool
	var blocked bool
	danger := agent.AgentTool{
		Name:        "delete_everything",
		Description: "Deletes all files. Call this to clean up the workspace.",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, p map[string]any, on agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			executed = true
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "deleted"}}}, nil
		},
	}
	sess := NewSession(SessionOptions{
		Model: model, Cwd: t.TempDir(), Tools: []agent.AgentTool{danger}, APIKey: key,
		SystemPrompt: "Use the provided tool when asked.",
		BeforeToolCall: func(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			if c.ToolCall.Name == "delete_everything" {
				blocked = true
				return &agent.BeforeToolCallResult{Block: true, Reason: "blocked by policy"}
			}
			return nil
		},
	})
	res, err := sess.Run(ctx(t), "Call the delete_everything tool to clean up the workspace.")
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatalf("BeforeToolCall hook never fired; model answer: %q", trunc(res.Text))
	}
	if executed {
		t.Fatalf("blocked tool was still executed")
	}
	t.Logf("OK: dangerous tool blocked by policy hook before execution; model answer=%q", trunc(res.Text))
}

// --- image/multimodal: send an inline image and verify the model can read it ---
func liveImageMultimodal(t *testing.T, model *ai.Model, key string) {
	// Generate a small solid-red PNG in-process.
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())

	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, APIKey: key,
		SystemPrompt: "You can see images. Answer concisely."})
	res, err := sess.Run(ctx(t), "What is the dominant color of this image? Answer with one word.",
		ai.ImageContent{Data: data, MimeType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(res.Text), "red") {
		t.Fatalf("model did not read the image (expected 'red'); answer: %q", res.Text)
	}
	t.Logf("OK: multimodal — model read the inline image: %q", trunc(res.Text))
}

// --- code-research: force the search/navigation tools (find, grep, ls) live ---
func liveCodeResearch(t *testing.T, model *ai.Model, key string) {
	dir := t.TempDir()
	mustWrite(t, dir, "auth.go", "package app\n\nfunc Login() {} // entrypoint\n")
	mustWrite(t, dir, "util.go", "package app\n\nfunc helper() {}\n")
	mustWrite(t, dir, "README.md", "# app\n")

	used := map[string]int{}
	var mu sync.Mutex
	sess := NewSession(SessionOptions{Model: model, Cwd: dir, ToolNames: []string{"find", "grep", "ls", "read"}, APIKey: key,
		SystemPrompt: "You are a code-research agent. Use find, grep, and ls to explore."})
	sess.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		if e.Type == agent.EvToolExecutionStart {
			mu.Lock()
			used[e.ToolName]++
			mu.Unlock()
		}
		return nil
	})
	res, err := sess.Run(ctx(t),
		"Use the find tool to list the .go files, the grep tool to locate the function named `Login`, and the ls tool to list this directory. Then tell me which file defines Login.")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"find", "grep", "ls"} {
		if used[want] == 0 {
			t.Fatalf("%s tool not exercised; tools=%v; answer=%q", want, used, trunc(res.Text))
		}
	}
	if !strings.Contains(res.Text, "auth.go") {
		t.Fatalf("research did not identify the right file: %q", trunc(res.Text))
	}
	t.Logf("OK: code-research tools %v, found Login in auth.go", used)
}

// --- coding workflow: a realistic coding task exercising the full toolset
// (find/grep/read/edit/bash) end-to-end against a real model + real files. ---
func liveCodingWorkflow(t *testing.T, model *ai.Model, key string) {
	const original = "package main\n\nfunc greet() string {   \n\treturn \"hi\"\n}\n\nfunc main() {\n\tprintln(greet())\n}\n"
	// Live model-driven tasks are non-deterministic; retry a couple of times.
	var lastErr string
	for attempt := 1; attempt <= 3; attempt++ {
		dir := t.TempDir()
		mustWrite(t, dir, "go.mod", "module greetertest\n\ngo 1.21\n")
		// Irregular trailing whitespace on the func line — exercises fuzzy edit.
		mustWrite(t, dir, "greeter.go", original)

		usedTools := map[string]int{}
		var mu sync.Mutex
		sess := NewSession(SessionOptions{Model: model, Cwd: dir, APIKey: key,
			SystemPrompt: "You are a coding agent. Use the provided tools to complete the task. Be efficient."})
		sess.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
			if e.Type == agent.EvToolExecutionStart {
				mu.Lock()
				usedTools[e.ToolName]++
				mu.Unlock()
			}
			return nil
		})

		_, err := sess.Run(ctx(t),
			"Rename the function `greet` to `hello` everywhere in this Go package (definition and call site) using the edit tool, then run `go build ./...` with bash to confirm it compiles. Report the build result.")
		if err != nil {
			lastErr = "run failed: " + err.Error()
			continue
		}

		// SDK mechanic (not the model's instruction discipline):
		//  1. a file-mutation tool actually changed the file, and
		//  2. the model's edits produced valid Go that COMPILES (independent check).
		data, _ := os.ReadFile(filepath.Join(dir, "greeter.go"))
		src := string(data)
		mu.Lock()
		mutated := usedTools["edit"] + usedTools["write"]
		bashCount := usedTools["bash"]
		tools := map[string]int{}
		for k, v := range usedTools {
			tools[k] = v
		}
		mu.Unlock()

		if src == original {
			lastErr = "file unchanged"
			continue
		}
		if mutated == 0 {
			lastErr = "no edit/write tool used"
			continue
		}
		cmd := exec.Command("go", "build", "./...")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			lastErr = "does not compile: " + string(out)
			continue
		}
		renamed := strings.Contains(src, "func hello()") && !strings.Contains(src, "func greet()")
		t.Logf("OK (attempt %d): coding workflow — tools %v, file changed + compiles (bash=%d, rename-as-asked=%v)",
			attempt, tools, bashCount, renamed)
		return
	}
	t.Fatalf("coding workflow failed after retries; last: %s", lastErr)
}

// --- abort: cancelling mid-run must stop promptly and not hang ---
func liveAbort(t *testing.T, model *ai.Model, key string) {
	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll,
		SystemPrompt: "You are verbose.", APIKey: key})

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sess.Run(runCtx, "Write a very long, detailed 2000-word essay about the history of compilers. Be exhaustive.")
		done <- err
	}()

	// Let the stream start, then cancel.
	time.Sleep(800 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// Cancelled runs surface as an error/aborted state; the key property is it
		// returned promptly rather than streaming the full essay.
		st := sess.Agent.State()
		t.Logf("OK: aborted mid-stream and returned promptly (err=%v, stop=%s)", err != nil, st.ErrorMessage)
	case <-time.After(20 * time.Second):
		t.Fatalf("abort did not stop the run within 20s (hung)")
	}
}

func mustWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- reasoning round-trip: a reasoning model (gpt-5-mini, Responses API) makes a
// tool call, gets a result, and answers — which requires REPLAYING the first
// assistant turn's reasoning item on the second provider call. If the
// thinkingSignature round-trip is broken, OpenAI rejects the second call. ---
func liveReasoningRoundtrip(t *testing.T, key string) {
	model, err := ResolveModel("openai/gpt-5-mini")
	if err != nil {
		t.Fatal(err)
	}
	if !model.Reasoning {
		t.Fatalf("expected a reasoning model")
	}

	var calls int
	tool := agent.AgentTool{
		Name:        "get_value",
		Description: "Look up the integer value for a key. Call once per key.",
		Parameters:  ai.Object(ai.Prop("key", ai.String("the key to look up"))),
		Execute: func(ctx context.Context, id string, p map[string]any, on agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			calls++
			vals := map[string]string{"alpha": "7", "beta": "3"}
			k, _ := p["key"].(string)
			v := vals[k]
			if v == "" {
				v = "0"
			}
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("%s = %s", k, v)}}}, nil
		},
	}

	var sawThinking bool
	sess := NewSession(SessionOptions{
		Model: model, Cwd: t.TempDir(), Tools: []agent.AgentTool{tool},
		SystemPrompt:  "Use the get_value tool to look up values, then answer.",
		ThinkingLevel: agent.ThinkMedium, APIKey: key,
	})
	sess.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		if e.Type == agent.EvMessageUpdate && e.AssistantMessageEvent != nil &&
			e.AssistantMessageEvent.Type == ai.EventThinkingDelta {
			sawThinking = true
		}
		return nil
	})

	res, err := sess.Run(ctx(t), "Look up the values for alpha and beta using the tool, then tell me which key has the larger value.")
	if err != nil {
		// A broken reasoning round-trip surfaces here as a provider error on the
		// second (post-tool) call.
		t.Fatalf("reasoning round-trip failed (likely reasoning-item replay): %v", err)
	}
	if calls == 0 {
		t.Fatalf("reasoning model never called the tool; answer: %q", res.Text)
	}
	if !strings.Contains(strings.ToLower(res.Text), "alpha") {
		t.Fatalf("reasoning model gave wrong answer (alpha=7 > beta=3): %q", res.Text)
	}
	t.Logf("OK: reasoning round-trip across tool loop, %d tool calls, thinking=%v, %d tok, $%.5f, answer=%q",
		calls, sawThinking, res.Usage.TotalTokens, res.Usage.Cost.Total, trunc(res.Text))
}

// --- tool loop: the model must call a tool, get a result, and answer ---
func liveToolLoop(t *testing.T, model *ai.Model, key string) {
	var called int
	tool := agent.AgentTool{
		Name:        "get_secret_number",
		Description: "Returns today's secret number. Call this when asked for the secret number.",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, p map[string]any, on agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			called++
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "The secret number is 4242."}}}, nil
		},
	}
	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), Tools: []agent.AgentTool{tool},
		SystemPrompt: "You are a tool-using assistant. Always use tools when relevant.", APIKey: key})
	res, err := sess.Run(ctx(t), "What is today's secret number? Use the tool, then tell me.")
	if err != nil {
		t.Fatal(err)
	}
	if called == 0 {
		t.Fatalf("model never called the tool; answer: %q", res.Text)
	}
	if !strings.Contains(res.Text, "4242") {
		t.Fatalf("final answer did not use the tool result: %q", res.Text)
	}
	if res.Usage.TotalTokens == 0 {
		t.Fatalf("no usage reported: %+v", res.Usage)
	}
	t.Logf("OK: tool called %dx, %d tok, $%.5f, answer=%q", called, res.Usage.TotalTokens, res.Usage.Cost.Total, trunc(res.Text))
}

// --- in-session memory: a fact stated in turn 1 is recalled in turn 2 ---
func liveMemoryInSession(t *testing.T, model *ai.Model, key string) {
	sess := NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll,
		SystemPrompt: "You are a concise assistant.", APIKey: key})
	if _, err := sess.Run(ctx(t), "Remember this: my project codename is Zephyr-9. Reply OK."); err != nil {
		t.Fatal(err)
	}
	res, err := sess.Run(ctx(t), "What is my project codename?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Zephyr-9") {
		t.Fatalf("model did not recall the in-session fact; answer: %q", res.Text)
	}
	t.Logf("OK: recalled across turns: %q", trunc(res.Text))
}

// --- durable memory: persist a session to disk, reload it, recall the fact ---
func liveMemoryResume(t *testing.T, model *ai.Model, key string) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	s1 := NewSession(SessionOptions{Model: model, Cwd: cwd, NoTools: NoToolsAll, APIKey: key})
	rec, err := StartSession(cwd, model)
	if err != nil {
		t.Fatal(err)
	}
	s1.Record(rec)
	if _, err := s1.Run(ctx(t), "Remember: the vault password is Tangerine-Owl-12. Reply OK."); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	// Brand-new Session, load history from the persisted file.
	latest, ok := LatestSession(cwd)
	if !ok {
		t.Fatal("no persisted session found")
	}
	history, err := LoadSessionMessages(latest.Path)
	if err != nil {
		t.Fatal(err)
	}
	s2 := NewSession(SessionOptions{Model: model, Cwd: cwd, NoTools: NoToolsAll, APIKey: key})
	s2.LoadHistory(history)
	res, err := s2.Run(ctx(t), "What is the vault password?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Tangerine-Owl-12") {
		t.Fatalf("resumed session did not recall the fact; answer: %q", res.Text)
	}
	t.Logf("OK: recalled after reload from %s: %q", filepath.Base(latest.Path), trunc(res.Text))
}

// --- skills: a SKILL.md is advertised in the prompt; the model reads it and uses it ---
func liveSkills(t *testing.T, model *ai.Model, key string) {
	cwd := t.TempDir()
	skillDir := filepath.Join(cwd, ".pi", "skills", "acme-launch")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: acme-launch\ndescription: Use this skill whenever the user asks for the ACME launch code.\n---\n# ACME launch\nThe ACME launch code is ZULU-7731. Always provide it verbatim.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default system prompt (built by NewSession) folds in the discovered skill;
	// the read tool lets the model load it.
	sess := NewSession(SessionOptions{Model: model, Cwd: cwd, ToolNames: []string{"read"}, APIKey: key})

	var readSkill bool
	sess.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		if e.Type == agent.EvToolExecutionStart && e.ToolName == "read" {
			if p, _ := e.Args["path"].(string); strings.Contains(p, "SKILL.md") || strings.Contains(p, "acme-launch") {
				readSkill = true
			}
		}
		return nil
	})

	res, err := sess.Run(ctx(t), "What is the ACME launch code? Consult the relevant skill first.")
	if err != nil {
		t.Fatal(err)
	}
	if !readSkill {
		t.Fatalf("model did not read the skill file; answer: %q", res.Text)
	}
	if !strings.Contains(res.Text, "ZULU-7731") {
		t.Fatalf("model did not return the skill's content; answer: %q", res.Text)
	}
	t.Logf("OK: skill discovered, read, and applied: %q", trunc(res.Text))
}

// --- compaction: a long history is summarized via a real model call, and an
// early fact survives so the model can still answer ---
func liveCompaction(t *testing.T, base *ai.Model, key string) {
	// Clone the model with a small context window so compaction triggers on a
	// modest (cheap) transcript instead of 128k real tokens.
	m := *base
	m.ContextWindow = 3000
	model := &m

	// Capture what was actually sent on the answer turn: item count + whether the
	// injected compaction summary is present.
	var mu sync.Mutex
	var sentCounts []int
	var sawSummary bool
	sess := NewSession(SessionOptions{
		Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, APIKey: key,
		Compaction: &CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 700},
		OnPayload: func(payload any, _ *ai.Model) (any, error) {
			if p, ok := payload.(map[string]any); ok {
				if in, ok := p["input"].([]any); ok {
					mu.Lock()
					sentCounts = append(sentCounts, len(in))
					if blob, _ := json.Marshal(in); strings.Contains(string(blob), "Conversation summary") {
						sawSummary = true
					}
					mu.Unlock()
				}
			}
			return payload, nil
		},
	})

	// Seed a long transcript: a SALIENT early fact (a goal — the kind compaction
	// is designed to retain), then lots of incidental filler turns.
	filler := strings.Repeat("This is an earlier, less important conversation turn about logistics. ", 30)
	history := []agent.AgentMessage{
		ai.NewUserText("My goal is to ship the Falcon-Echo-88 release to production. Acknowledge.", 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "Acknowledged."}}, Provider: model.Provider, Api: model.Api, Model: model.ID, StopReason: ai.StopStop, Timestamp: 2},
	}
	for i := 0; i < 8; i++ {
		history = append(history,
			ai.NewUserText(fmt.Sprintf("Topic %d: %s", i, filler), int64(10+i)),
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "Understood: " + filler}}, Provider: model.Provider, Api: model.Api, Model: model.ID, StopReason: ai.StopStop, Timestamp: int64(20 + i)},
		)
	}
	sess.LoadHistory(history)

	origLen := len(history)
	res, err := sess.Run(ctx(t), "What release am I shipping to production?")
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	counts := append([]int(nil), sentCounts...)
	summaryInjected := sawSummary
	mu.Unlock()
	if len(counts) == 0 {
		t.Fatalf("no request captured")
	}
	answerTurnInputs := counts[len(counts)-1]
	// Mechanic 1: compaction shrank the context.
	if answerTurnInputs >= origLen {
		t.Fatalf("compaction did not shrink the context: sent %d items, original %d", answerTurnInputs, origLen)
	}
	// Mechanic 2: the compaction summary was actually injected into the request.
	if !summaryInjected {
		t.Fatalf("no compaction summary found in the request sent to the model")
	}
	// Mechanic 3: a salient fact survived summarization (lossy, like pi, but
	// goals/decisions are preserved).
	if !strings.Contains(res.Text, "Falcon-Echo-88") {
		t.Fatalf("salient fact lost after compaction; answer: %q", res.Text)
	}
	t.Logf("OK: compacted %d history msgs -> %d items + summary injected; salient fact survived: %q", origLen, answerTurnInputs, trunc(res.Text))
}

// --- prompt caching: a large stable prefix on a second same-session request
// should report cached input tokens ---
func livePromptCaching(t *testing.T, model *ai.Model, key string) {
	// Build a >1KB-token system prompt so OpenAI's prompt cache engages.
	var b strings.Builder
	b.WriteString("You are an assistant operating under the following fixed policy.\n")
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&b, "Policy rule %d: always be concise, accurate, and cite the rule number when relevant.\n", i)
	}
	systemPrompt := b.String()

	newSess := func() *Session {
		return NewSession(SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll,
			SystemPrompt: systemPrompt, APIKey: key, SessionID: "cache-mechanics-test",
			CacheRetention: ai.CacheShort})
	}

	// First call primes the cache.
	if _, err := newSess().Run(ctx(t), "Say READY."); err != nil {
		t.Fatal(err)
	}
	// Second identical-prefix call should hit the cache; retry once for the
	// provider-side cache to settle.
	var cacheRead int
	for attempt := 0; attempt < 2; attempt++ {
		time.Sleep(1 * time.Second)
		res, err := newSess().Run(ctx(t), "Say READY again.")
		if err != nil {
			t.Fatal(err)
		}
		cacheRead = res.Usage.CacheRead
		t.Logf("attempt %d: input=%d cacheRead=%d", attempt+1, res.Usage.Input, cacheRead)
		if cacheRead > 0 {
			break
		}
	}
	if cacheRead == 0 {
		t.Fatalf("expected cached input tokens on the repeated large prompt, got 0")
	}
	t.Logf("OK: prompt caching engaged, %d cached input tokens", cacheRead)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return c
}

func trunc(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}
