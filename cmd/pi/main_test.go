package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
// as a [User] turn under the initial prompt. The file is one pi wrote at
// d2e7d8b7c (coding/testdata/compaction/capture-resume.mts).
func TestResumeExtendsTheSessionsCompaction(t *testing.T) {
	data, err := os.ReadFile("../../coding/testdata/compaction/resume-d2e7d8b7c.json")
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
	sess, err := coding.NewSession(coding.SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), SystemPrompt: "test", NoTools: coding.NoToolsAll,
		Compaction: &coding.CompactionSettings{Enabled: true, ReserveTokens: 2000, KeepRecentTokens: 1}})
	if err != nil {
		t.Fatal(err)
	}
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
func TestRequireProviderAuthGatesOnTheAuthResolver(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE",
		"ANTHROPIC_SERVICE_ACCOUNT_ID", "ANTHROPIC_WORKSPACE_ID"} {
		t.Setenv(name, "")
	}
	if err := requireProviderAuth("anthropic"); err == nil || !strings.Contains(err.Error(), "_API_KEY") {
		t.Fatalf("nothing configured: err = %v, want a refusal saying which env var to set", err)
	}

	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_test")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-test")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", filepath.Join(t.TempDir(), "token"))
	if err := requireProviderAuth("anthropic"); err != nil {
		t.Fatalf("federation: %v, want no error", err)
	}

	for _, name := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	if err := requireProviderAuth("anthropic"); err != nil {
		t.Fatalf("auth token: %v, want no error", err)
	}

	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	if err := requireProviderAuth("anthropic"); err != nil {
		t.Fatalf("api key: %v, want no error", err)
	}
}

// The key alone does not configure Cloudflare (ai/cloudflare_auth.go), so
// the gate refuses it, naming every variable the provider needs.
func TestRequireProviderAuthNamesCloudflaresVariables(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_KEY", "env-key")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "")
	for provider, want := range map[string]string{
		"cloudflare-workers-ai": "CLOUDFLARE_ACCOUNT_ID)",
		"cloudflare-ai-gateway": "CLOUDFLARE_GATEWAY_ID)",
	} {
		if err := requireProviderAuth(provider); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%s with only the key: err = %v, want a refusal ending %q", provider, err, want)
		}
	}
}

// A Cloudflare model the CLI streams reaches the account the environment
// names, as pi's coding agent's does: the session main builds holds no key,
// so compat resolves the request's auth through the built-in Models, which
// fills the base URL's {CLOUDFLARE_ACCOUNT_ID} from the resolved env
// (testdata/cloudflare-auth in ai/providers, Models rows). The model's host
// moves into a local server's path, placeholder and all, so the request's
// URL maps back to the one pi sends.
func TestCLICloudflareRequestReachesTheAccount(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_KEY", "env-key")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "env-account")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "")
	var url, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		url, authorization = "https://"+strings.TrimPrefix(r.URL.RequestURI(), "/"), r.Header.Get("Authorization")
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	catalog := ai.GetModel("cloudflare-workers-ai", "@cf/deepseek-ai/deepseek-v4-flash-0731")
	if err := requireProviderAuth(catalog.Provider); err != nil {
		t.Fatal(err)
	}
	model := *catalog
	model.BaseURL = server.URL + "/" + strings.TrimPrefix(catalog.BaseURL, "https://")
	sess, err := newSession(&model, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	const want = "https://api.cloudflare.com/client/v4/accounts/env-account/ai/v1/chat/completions"
	if url != want || authorization != "Bearer env-key" {
		t.Fatalf("request to %q with %q, pi to %q with Bearer env-key", url, authorization, want)
	}
}
