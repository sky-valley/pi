package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// TestSummarizationRequestShape pins the faithful summarization request builder
// (pi compaction.ts generateSummary + utils.ts): a dedicated system prompt, the
// serialized conversation wrapped in <conversation>...</conversation>, tool-result
// truncation to 2000 chars, no maxTokens of its own (upstream 6fcb48670: the
// provider's context-clamped default applies), and the read/modified file lists
// appended to the returned summary text.
func TestSummarizationRequestShape(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 200000}},
	})
	defer reg.Unregister()
	model := reg.GetModel()
	model.MaxTokens = 1_000_000

	var captured ai.TranscriptContext
	var capturedMax *int
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			captured = req
			capturedMax = opts.MaxTokens
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\ncheckpoint"}}, ai.StopStop)
		},
	})

	sess := newTestSession(t, SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll})

	bigResult := strings.Repeat("z", 5000) // > 2000 chars, must be truncated
	older := []agent.AgentMessage{
		ai.NewUserText("please refactor the parser", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{
				ai.TextContent{Text: "reading files"},
				ai.ToolCall{ID: "r1", Name: "read", Arguments: map[string]any{"path": "/a/only_read.go"}},
				ai.ToolCall{ID: "e1", Name: "edit", Arguments: map[string]any{"path": "/a/changed.go"}},
				ai.ToolCall{ID: "r2", Name: "read", Arguments: map[string]any{"path": "/a/changed.go"}},
			},
			StopReason: ai.StopToolUse, Timestamp: 2,
		},
		ai.ToolResultMessage{ToolCallID: "r1", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: bigResult}}, Timestamp: 3},
	}

	const reserve = 16384
	summary := sess.summarize(context.Background(), older)

	// System prompt present and exact, as the transcript's leading system message.
	leading, hasLeading := ai.GetInitialSystemMessage(captured.Messages)
	if !hasLeading || ai.GetSystemMessageText(leading) != summarizationSystemPrompt {
		t.Fatalf("summarization system prompt missing/wrong:\n%q", ai.GetCurrentSystemPrompt(captured.Messages))
	}

	// Single user message with the <conversation> wrapper + the summarization prompt.
	conversation := ai.WithoutInitialSystemMessage(captured.Messages)
	if len(conversation) != 1 {
		t.Fatalf("expected 1 summarization message, got %d", len(conversation))
	}
	um, ok := conversation[0].(ai.UserMessage)
	if !ok {
		t.Fatalf("expected user message, got %T", conversation[0])
	}
	text := textOf(um.Content)
	if !strings.HasPrefix(text, "<conversation>\n") || !strings.Contains(text, "\n</conversation>\n\n") {
		t.Fatalf("conversation wrapper missing: %q", text)
	}
	if !strings.HasSuffix(text, summarizationPrompt) {
		t.Fatalf("summarization prompt not appended after </conversation>")
	}
	if !strings.Contains(text, "[User]: please refactor the parser") {
		t.Fatalf("user turn not serialized: %q", text)
	}
	if !strings.Contains(text, "[Assistant tool calls]: read(path=\"/a/only_read.go\")") {
		t.Fatalf("tool-call serialization missing: %q", text)
	}

	// Tool result truncated to 2000 chars + marker.
	if !strings.Contains(text, "[... 3000 more characters truncated]") {
		t.Fatalf("tool result not truncated to 2000 chars: %q", text[len(text)-200:])
	}
	if strings.Count(text, "z") > 2100 {
		t.Fatalf("tool result kept too many chars (truncation failed)")
	}

	// The request sets no maxTokens: reserveTokens only triggers compaction.
	if capturedMax != nil {
		t.Fatalf("maxTokens = %d, want none (reserve %d no longer caps the summary)", *capturedMax, reserve)
	}

	// File lists appended to the summary: only_read.go is read-only; changed.go
	// was edited (so excluded from read-files, present in modified-files).
	if !strings.Contains(summary, "<read-files>\n/a/only_read.go\n</read-files>") {
		t.Fatalf("read-files list missing/wrong:\n%s", summary)
	}
	if !strings.Contains(summary, "<modified-files>\n/a/changed.go\n</modified-files>") {
		t.Fatalf("modified-files list missing/wrong:\n%s", summary)
	}
	if strings.Contains(summary, "<read-files>\n/a/changed.go") {
		t.Fatalf("changed.go must not appear in read-files")
	}
}

// testdata/compaction/capture-requests.mts runs the npm build's compact() over
// split-turn and history-only preparations, with and without a previous
// summary, and records every summarization request pi sends, the reply it got
// and the summary compact() returns. The port's compaction over the same
// transcripts, answered with the same replies, must send the same requests byte
// for byte, in the same order, and checkpoint the same summary: the system
// prompt, the history request's <conversation> wrapper and prompts, and the
// split-turn prefix's "# Conversation" / "# Instructions" framing (upstream
// d192bd6dc).
//
// An empty reply is still a summary. pi's compact() returns it (plus the file
// lists) and AgentSession appends it as the compaction, so every scenario must
// leave a checkpoint behind. A previous summary that is present but empty is not
// an absent one: `previousSummary ?? "No prior history."` keeps it.
func TestSummarizationRequestsMatchPiCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/compaction/requests-d2e7d8b7c.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		ReserveTokens  int `json:"reserveTokens"`
		ModelMaxTokens int `json:"modelMaxTokens"`
		Scenarios      []struct {
			Name            string            `json:"name"`
			Messages        []json.RawMessage `json:"messages"`
			HistoryCount    int               `json:"historyCount"`
			PrefixCount     int               `json:"prefixCount"`
			PreviousSummary *string           `json:"previousSummary"` // nil: no previous compaction
			Requests        []struct {
				SystemPrompt string `json:"systemPrompt"`
				Text         string `json:"text"`
				MaxTokens    *int   `json:"maxTokens"` // nil: the request sets none
				Reply        string `json:"reply"`
			} `json:"requests"`
			Summary string `json:"summary"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Scenarios) == 0 {
		t.Fatal("the capture holds no scenarios")
	}
	for _, scenario := range capture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			messages := make([]agent.AgentMessage, 0, len(scenario.Messages))
			for _, raw := range scenario.Messages {
				message, err := ai.UnmarshalMessage(raw)
				if err != nil {
					t.Fatal(err)
				}
				messages = append(messages, message)
			}
			// The port cuts the transcript itself. Keeping 1 token cuts at the last
			// message; the scenario is only a check if that cut splits the
			// transcript where pi's preparation did.
			preparation, ok := prepareCompaction(messages, 0, 1)
			if !ok || len(preparation.messagesToSummarize) != scenario.HistoryCount || len(preparation.turnPrefixMessages) != scenario.PrefixCount {
				t.Fatalf("the port's cut (history %d, prefix %d) is not the capture's (%d, %d): fix the scenario in capture-requests.mts so a 1-token keep cuts where its counts say",
					len(preparation.messagesToSummarize), len(preparation.turnPrefixMessages), scenario.HistoryCount, scenario.PrefixCount)
			}

			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
				// A window no larger than the reserve compacts any transcript.
				Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: capture.ReserveTokens, MaxTokens: capture.ModelMaxTokens}},
			})
			t.Cleanup(reg.Unregister)
			type request struct {
				systemPrompt string
				text         string
				maxTokens    *int
			}
			var got []request
			record := func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
				var r request
				if lead, ok := ai.GetInitialSystemMessage(req.Messages); ok {
					r.systemPrompt = ai.GetSystemMessageText(lead)
				}
				if conversation := ai.WithoutInitialSystemMessage(req.Messages); len(conversation) == 1 {
					r.text = userText(conversation[0])
				} else {
					t.Errorf("request %d carries %d messages after its system prompt, want 1", len(got)+1, len(conversation))
				}
				if opts != nil {
					r.maxTokens = opts.MaxTokens
				}
				got = append(got, r)
				reply := fmt.Sprintf("unexpected request %d", len(got))
				if len(got) <= len(scenario.Requests) {
					reply = scenario.Requests[len(got)-1].Reply
				}
				return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: reply}}, ai.StopStop)
			}
			steps := make([]providers.FauxResponseStep, len(scenario.Requests))
			for i := range steps {
				steps[i] = record
			}
			reg.SetResponses(steps)

			sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll})
			state := &compactionState{
				settings: CompactionSettings{Enabled: true, ReserveTokens: capture.ReserveTokens, KeepRecentTokens: 1},
			}
			if scenario.PreviousSummary != nil {
				state.checkpoint = &compactionCheckpoint{summary: *scenario.PreviousSummary}
			}
			sess.compact(context.Background(), state, messages)

			if len(got) != len(scenario.Requests) {
				t.Fatalf("sent %d summarization requests, pi sent %d", len(got), len(scenario.Requests))
			}
			for i, want := range scenario.Requests {
				if got[i].systemPrompt != want.SystemPrompt {
					t.Errorf("request %d system prompt drifts from pi.\n--- got ---\n%s\n--- pi ---\n%s", i+1, got[i].systemPrompt, want.SystemPrompt)
				}
				if got[i].text != want.Text {
					t.Errorf("request %d text drifts from pi.\n--- got ---\n%s\n--- pi ---\n%s", i+1, got[i].text, want.Text)
				}
				if formatMaxTokens(got[i].maxTokens) != formatMaxTokens(want.MaxTokens) {
					t.Errorf("request %d maxTokens = %s, pi %s", i+1, formatMaxTokens(got[i].maxTokens), formatMaxTokens(want.MaxTokens))
				}
			}
			checkpoint := state.checkpoint
			if checkpoint == nil {
				t.Fatalf("no checkpoint: pi's compact() returned %q here and AgentSession appends it as the compaction", scenario.Summary)
			}
			if checkpoint.prefixLen != preparation.firstKeptIndex || checkpoint.compactedLen != len(messages) {
				t.Errorf("checkpoint keeps from %d (compacted at %d), want %d (%d)", checkpoint.prefixLen, checkpoint.compactedLen, preparation.firstKeptIndex, len(messages))
			}
			if checkpoint.summary != scenario.Summary {
				t.Errorf("summary drifts from pi.\n--- got ---\n%q\n--- pi ---\n%q", checkpoint.summary, scenario.Summary)
			}
		})
	}
}

// formatMaxTokens renders a request's maxTokens, "none" when it sets none.
func formatMaxTokens(maxTokens *int) string {
	if maxTokens == nil {
		return "none"
	}
	return fmt.Sprint(*maxTokens)
}

// TestSummarizationIsolatesRouting mirrors pi's "uses fresh routing sessions
// without prompt caching" (9b3a2059): every summarization request carries
// cacheRetention "none" and its own session id, so summaries neither reuse nor
// pollute the main session's cache and routing.
func TestSummarizationIsolatesRouting(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 200000}},
	})
	defer reg.Unregister()
	model := reg.GetModel()

	var retentions []ai.CacheRetention
	var sessionIDs []string
	capture := func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
		retentions = append(retentions, opts.CacheRetention)
		sessionIDs = append(sessionIDs, opts.SessionID)
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
	}
	reg.SetResponses([]providers.FauxResponseStep{capture, capture})

	sess := newTestSession(t, SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, SessionID: "main-session"})
	older := []agent.AgentMessage{ai.NewUserText("hi", 1)}
	sess.summarize(context.Background(), older)
	sess.summarize(context.Background(), older)

	if len(retentions) != 2 {
		t.Fatalf("expected 2 summarization requests, got %d", len(retentions))
	}
	for i, r := range retentions {
		if r != ai.CacheNone {
			t.Fatalf("request %d cacheRetention = %q, want %q", i, r, ai.CacheNone)
		}
	}
	for i, id := range sessionIDs {
		if id == "" {
			t.Fatalf("request %d has no session id, want a fresh one", i)
		}
		if id == "main-session" {
			t.Fatalf("request %d reused the session's own id %q", i, id)
		}
	}
	if sessionIDs[0] == sessionIDs[1] {
		t.Fatalf("both summarization requests shared session id %q, want distinct ids", sessionIDs[0])
	}
}

// TestSummarizationUsesAuthResolvedBaseURL ports pi's regression test
// 6768-copilot-compaction-base-url.test.ts ("uses the auth-resolved base URL
// through the SDK-style stream wrapper"): a Copilot compaction request must go
// to the credential-resolved Enterprise endpoint, not the catalog's Individual
// one.
//
// The provider mirrors upstream's: the Enterprise base URL is reachable only
// through the stored OAuth credential's toAuth. An explicit apiKey override
// short-circuits resolution to the api-key path, so the Models runtime resolves
// no base URL and never rebuilds the request model — and summarization passes
// the key it resolved explicitly, which is exactly how the Individual endpoint
// leaked through. The session streams through the real ai.Models runtime, as
// upstream's SDK-style stream wrapper does, not a stream double.
func TestSummarizationUsesAuthResolvedBaseURL(t *testing.T) {
	const individualBaseURL = "https://api.individual.githubcopilot.com"
	const enterpriseBaseURL = "https://api.enterprise.githubcopilot.com"

	var requestBaseURL string
	respond := func(_ context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		requestBaseURL = model.BaseURL
		s := ai.NewAssistantMessageEventStream()
		s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: ai.StopStop, Message: &ai.AssistantMessage{
			Api: model.Api, Provider: model.Provider, Model: model.ID,
			Content: ai.ContentList{ai.TextContent{Text: "summary"}}, StopReason: ai.StopStop,
		}})
		s.End()
		return s
	}

	catalogModel := &ai.Model{Provider: "github-copilot", ID: "gpt", Api: "copilot-api", BaseURL: individualBaseURL}
	provider := ai.CreateProvider(ai.CreateProviderOptions{
		ID: catalogModel.Provider,
		Auth: ai.ProviderAuth{
			APIKey: &ai.ApiKeyAuth{
				Name: "Copilot token",
				Resolve: func(_ context.Context, _ ai.AuthContext, credential *ai.Credential) (*ai.AuthResult, error) {
					if credential == nil || credential.Key == "" {
						return nil, nil
					}
					return &ai.AuthResult{Auth: ai.ModelAuth{APIKey: credential.Key}, Source: "explicit token"}, nil
				},
			},
			OAuth: &ai.OAuthAuth{
				Name:    "Copilot OAuth",
				Refresh: func(_ context.Context, c ai.OAuthCredentials) (ai.OAuthCredentials, error) { return c, nil },
				ToAuth: func(c ai.OAuthCredentials) (ai.ModelAuth, error) {
					return ai.ModelAuth{APIKey: c.Access, BaseURL: enterpriseBaseURL}, nil
				},
			},
		},
		Models: []*ai.Model{catalogModel},
		APIByApi: map[ai.Api]ai.ProviderStreams{catalogModel.Api: {
			Stream: func(ctx context.Context, m *ai.Model, req ai.TranscriptContext, _ *ai.StreamOptions) *ai.AssistantMessageEventStream {
				return respond(ctx, m, req, nil)
			},
			StreamSimple: respond,
		}},
	})

	credentials := ai.NewInMemoryCredentialStore()
	if _, err := credentials.Modify(context.Background(), catalogModel.Provider, func(*ai.Credential) (*ai.Credential, error) {
		return &ai.Credential{
			Type: ai.CredentialOAuth, Access: "enterprise-token", Refresh: "refresh-token",
			Expires: time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	models := ai.CreateModels(&ai.CreateModelsOptions{Credentials: credentials})
	models.SetProvider(provider)

	// The session streams through models, as upstream's SDK-style wrapper
	// streams through the model runtime.
	sess := newTestSession(t, SessionOptions{
		Model: catalogModel, Cwd: t.TempDir(), NoTools: NoToolsAll, Models: models,
	})
	older := []agent.AgentMessage{
		ai.NewUserText("summarize me", 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop, Timestamp: 2},
	}
	if summary := sess.summarize(context.Background(), older); summary == "" {
		t.Fatal("summarization produced no summary")
	}

	if requestBaseURL != enterpriseBaseURL {
		t.Fatalf("compaction request base URL = %q, want %q", requestBaseURL, enterpriseBaseURL)
	}
}

// anthropicSummarizationSSE is a minimal Anthropic stream carrying one text
// block, enough for a summarization round trip.
const anthropicSummarizationSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"## Goal\ncheckpoint"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// TestSummarizationFallbacksComeFromTheCatalog replaces pi's deleted
// getAnthropicSummarizationFallback coverage (upstream ed867e909). Compaction no
// longer picks a server-side refusal fallback per call site — the option it used
// is gone from SimpleStreamOptions, so a summarization request cannot carry one —
// yet a summarization sent to an Anthropic model whose catalog lists permitted
// fallback models must STILL arrive with `fallbacks`, because the provider
// derives the field from the model. A model with no permitted targets still sends
// none.
func TestSummarizationFallbacksComeFromTheCatalog(t *testing.T) {
	cases := []struct {
		name   string
		compat string
		want   []string
	}{
		{
			name:   "catalog lists permitted fallback models",
			compat: `{"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}]}`,
			want:   []string{"claude-opus-4-8"},
		},
		{name: "catalog lists none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				w.Header().Set("content-type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, anthropicSummarizationSSE)
			}))
			defer server.Close()

			model := &ai.Model{
				ID: "claude-opus-5", Api: ai.APIAnthropicMessages, Provider: "anthropic",
				Input: []string{"text"}, MaxTokens: 8192, ContextWindow: 200000,
				BaseURL: server.URL,
			}
			if tc.compat != "" {
				model.Compat = []byte(tc.compat)
			}
			// The adapter directly, with a key, as a session's request carries one.
			withKey := func(ctx context.Context, m *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				keyed := *opts
				keyed.APIKey = "k"
				return providers.StreamSimpleAnthropic(ctx, m, req, &keyed)
			}
			sess := newTestSession(t, SessionOptions{
				Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll, StreamFn: withKey,
			})
			older := []agent.AgentMessage{
				ai.NewUserText("summarize me", 1),
				ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop, Timestamp: 2},
			}
			if summary := sess.summarize(context.Background(), older); summary == "" {
				t.Fatal("summarization produced no summary")
			}

			if tc.want == nil {
				if _, has := body["fallbacks"]; has {
					t.Fatalf("want no fallbacks on the summarization request, got %v", body["fallbacks"])
				}
				return
			}
			fallbacks, ok := body["fallbacks"].([]any)
			if !ok || len(fallbacks) != len(tc.want) {
				t.Fatalf("want %v on the summarization request, got %#v", tc.want, body["fallbacks"])
			}
			for i, id := range tc.want {
				entry, _ := fallbacks[i].(map[string]any)
				if len(entry) != 1 || entry["model"] != id {
					t.Fatalf("fallbacks[%d] must carry only {model: %s}, got %v", i, id, entry)
				}
			}
		})
	}
}

// TestSummarizationDoesNotOverrideToolChoice pins pi 6b36eb592, which withdrew
// the `toolChoice: "none"` 90305d90a had put on summarization requests: the
// option is left absent so the provider's own default applies. The guard that
// outlived it stands — a response that called a tool anyway is a failed
// summarization, not something to checkpoint.
func TestSummarizationDoesNotOverrideToolChoice(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 200000}},
	})
	defer reg.Unregister()
	model := reg.GetModel()

	var capturedChoice ai.ToolChoice
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			if opts != nil {
				capturedChoice = opts.ToolChoice
			}
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "## Goal\nsummary"}}, ai.StopStop)
		},
	})

	sess := newTestSession(t, SessionOptions{Model: model, Cwd: t.TempDir(), NoTools: NoToolsAll})
	older := []agent.AgentMessage{ai.NewUserText("do the thing", 1)}

	if got := sess.summarize(context.Background(), older); got == "" {
		t.Fatal("expected a summary from a text-only response")
	}
	if capturedChoice != "" {
		t.Fatalf("summarization must leave toolChoice absent, got %q", capturedChoice)
	}

	// Same request, but the model calls a tool: no summary survives.
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			return providers.FauxAssistantMessage(ai.ContentList{
				ai.TextContent{Text: "## Goal\nsummary"},
				ai.ToolCall{ID: "t1", Name: "read", Arguments: map[string]any{"path": "/a"}},
			}, ai.StopToolUse)
		},
	})
	if got := sess.summarize(context.Background(), older); got != "" {
		t.Fatalf("a tool call must fail the summarization, got %q", got)
	}
}
