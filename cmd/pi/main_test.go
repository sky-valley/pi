package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"
)

// Resuming a session file resumes its compaction: the next compaction extends
// the file's with the update prompt, its summary as <previous-summary>, as pi's
// does. A resume that loaded only the messages would summarize the old summary
// as a [User] turn under the initial prompt. The file is one pi 0.87.1 wrote
// (coding/testdata/compaction/capture-resume.mts).
func TestResumeExtendsTheSessionsCompaction(t *testing.T) {
	data, err := os.ReadFile("../../coding/testdata/compaction/resume-0.87.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Scenarios []struct {
			Name string `json:"name"`
			File string `json:"file"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	var file string
	for _, scenario := range capture.Scenarios {
		if scenario.Name == "previous-summary-with-files" {
			file = scenario.File
		}
	}
	if file == "" {
		t.Fatal("the capture holds no previous-summary-with-files scenario")
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := coding.LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}

	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 2000, MaxTokens: 8192}},
	})
	t.Cleanup(reg.Unregister)
	var summarizations []string
	step := func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		for _, m := range req.Messages {
			if u, ok := m.(ai.UserMessage); ok {
				if text := ai.ContentText(u.Content); strings.HasPrefix(text, "<conversation>\n") {
					summarizations = append(summarizations, text)
					return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "SUMMARY"}}, ai.StopStop)
				}
			}
		}
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "reply"}}, ai.StopStop)
	}
	reg.SetResponses([]providers.FauxResponseStep{step, step, step})
	// A reserve as large as the window compacts on every request.
	sess := coding.NewSession(coding.SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), SystemPrompt: "test", NoTools: coding.NoToolsAll,
		Compaction: &coding.CompactionSettings{Enabled: true, ReserveTokens: 2000, KeepRecentTokens: 1}})
	if rec := resume(sess, path, tree.BuildProjection(), true); rec != nil {
		t.Cleanup(func() { rec.Close() })
	}
	if _, err := sess.Run(context.Background(), "q5"); err != nil {
		t.Fatal(err)
	}
	if len(summarizations) == 0 {
		t.Fatal("the resumed session never compacted")
	}
	const previous = "<previous-summary>\n## Goal\nold work\n\n<read-files>\n/a/old.go\n</read-files>\n\n<modified-files>\n/a/changed.go\n</modified-files>\n</previous-summary>"
	if !strings.Contains(summarizations[0], previous) {
		t.Fatalf("the first compaction after resuming does not extend the file's.\n--- request ---\n%s", summarizations[0])
	}
}

// A provider configured without an API key — Anthropic through
// ANTHROPIC_AUTH_TOKEN, or workload identity federation (upstream a9424cd43) —
// passes the CLI's auth gate, as pi's coding agent gates on the provider's
// auth check rather than on an API key; the adapter authenticates the request
// itself. With nothing configured the gate still refuses, naming the fix.
func TestProviderAPIKeyGatesOnTheAuthResolver(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE",
		"ANTHROPIC_SERVICE_ACCOUNT_ID", "ANTHROPIC_WORKSPACE_ID"} {
		t.Setenv(name, "")
	}
	if _, err := providerAPIKey("anthropic"); err == nil || !strings.Contains(err.Error(), "_API_KEY") {
		t.Fatalf("nothing configured: err = %v, want a refusal saying which env var to set", err)
	}

	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_test")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-test")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", filepath.Join(t.TempDir(), "token"))
	if key, err := providerAPIKey("anthropic"); err != nil || key != "" {
		t.Fatalf("federation: (%q, %v), want no key and no error", key, err)
	}

	for _, name := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	if key, err := providerAPIKey("anthropic"); err != nil || key != "" {
		t.Fatalf("auth token: (%q, %v), want no key and no error", key, err)
	}

	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	if key, err := providerAPIKey("anthropic"); err != nil || key != "sk-ant-test" {
		t.Fatalf("api key: (%q, %v), want the key", key, err)
	}
}
