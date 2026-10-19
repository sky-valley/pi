package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/internal/jstext"
)

// testdata/compaction/capture-resume.mts writes session files with pi's own
// SessionManager, each holding one compaction entry, and records how pi extends
// that compaction: prepareCompaction reads the entry's summary as the previous
// summary, starts the cut search after it, and merges its details' file lists
// (unless an extension produced it); compact() then sends its requests, and the
// new compaction's context follows. Resumed as cmd/pi resumes a file
// (LoadSessionTree, BuildProjection, LoadBranch), the port must send the same
// requests and rebuild the same context. Where pi's compact() throws after its
// requests (a details value with no string form), the port's compaction fails
// too, and the resumed checkpoint keeps rebuilding the context pi keeps.
func TestResumedCompactionMatchesPiCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/compaction/resume-0.87.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		ReserveTokens  int `json:"reserveTokens"`
		ContextWindow  int `json:"contextWindow"`
		ModelMaxTokens int `json:"modelMaxTokens"`
		Scenarios      []struct {
			Name string `json:"name"`
			// File is the session file as pi wrote it.
			File    string   `json:"file"`
			Resumed []string `json:"resumed"`
			// PreviousSummary is absent when pi's prepareCompaction found no
			// previous compaction.
			PreviousSummary *string `json:"previousSummary"`
			Requests        []struct {
				SystemPrompt string `json:"systemPrompt"`
				Text         string `json:"text"`
				MaxTokens    int    `json:"maxTokens"`
			} `json:"requests"`
			Summary string `json:"summary"`
			// Error is compact()'s message when it threw; nothing was appended.
			Error     string   `json:"error"`
			Compacted []string `json:"compacted"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Scenarios) == 0 {
		t.Fatal("capture holds no scenarios")
	}
	for _, scenario := range capture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			tree, err := LoadSessionTree(writeSessionText(t, scenario.File))
			if err != nil {
				t.Fatal(err)
			}
			branch := tree.BuildProjection()
			if got := describeMessages(branch.Messages); !slices.Equal(got, scenario.Resumed) {
				t.Fatalf("resumed context drifts from pi:\n got %q\n  pi %q", got, scenario.Resumed)
			}

			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
				Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: capture.ContextWindow, MaxTokens: capture.ModelMaxTokens}},
			})
			t.Cleanup(reg.Unregister)
			type request struct {
				systemPrompt, text string
				maxTokens          int
			}
			var got []request
			record := func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
				lead, _ := ai.GetInitialSystemMessage(req.Messages)
				r := request{systemPrompt: ai.GetSystemMessageText(lead), text: userText(ai.WithoutInitialSystemMessage(req.Messages)[0])}
				if opts != nil && opts.MaxTokens != nil {
					r.maxTokens = *opts.MaxTokens
				}
				got = append(got, r)
				return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: fmt.Sprintf("SUMMARY %d", len(got))}}, ai.StopStop)
			}
			reg.SetResponses([]providers.FauxResponseStep{record, record, record})
			sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll,
				Compaction: &CompactionSettings{Enabled: true, ReserveTokens: capture.ReserveTokens, KeepRecentTokens: 1}})
			sess.LoadBranch(branch)
			checkpoint := sess.compactState.checkpoint
			if (checkpoint != nil) != (scenario.PreviousSummary != nil) {
				t.Fatalf("resumed checkpoint %+v, but pi found a previous compaction: %v", checkpoint, scenario.PreviousSummary != nil)
			}
			// Until the next compaction, the checkpoint rebuilds the context pi
			// resumed.
			view := sess.History()
			if checkpoint != nil {
				view = checkpoint.apply(view)
			}
			if got := describeMessages(view); !slices.Equal(got, scenario.Resumed) {
				t.Fatalf("the resumed checkpoint rebuilds a context that drifts from pi:\n got %q\n  pi %q", got, scenario.Resumed)
			}

			out := sess.compact(context.Background(), sess.compactState, sess.History())
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
				if got[i].maxTokens != want.MaxTokens {
					t.Errorf("request %d maxTokens = %d, pi %d", i+1, got[i].maxTokens, want.MaxTokens)
				}
			}
			if scenario.Error != "" {
				if sess.compactState.checkpoint != checkpoint {
					t.Errorf("pi's compact() threw %q and appended nothing, but the port compacted: %q", scenario.Error, sess.compactState.checkpoint.summary)
				}
			} else if checkpoint := checkpointOf(t, sess.compactState); checkpoint.summary != scenario.Summary {
				t.Errorf("summary drifts from pi.\n--- got ---\n%q\n--- pi ---\n%q", checkpoint.summary, scenario.Summary)
			}
			if got := describeMessages(out); !slices.Equal(got, scenario.Compacted) {
				t.Errorf("compacted context drifts from pi:\n got %q\n  pi %q", got, scenario.Compacted)
			}
		})
	}
}

// testdata/compaction/capture-trigger.mts resumes session files through a real
// pi AgentSession and prompts once, recording the requests up to the prompt's
// own. pi's _checkCompaction decides before the prompt is sent: it skips an
// assistant older than the latest compaction, and trusts usage only when it
// was recorded after the latest compaction or context_edit
// (estimateProjectedContextTokens); without trusted usage it estimates the
// current system message once plus the other messages. Resumed as cmd/pi
// resumes a file, the port must compact before the same prompts, with the
// same requests, and send the same context.
//
// The port decides in its per-request transform, after the prompt and the
// system message it declares are in the transcript; pi's check runs before
// either lands. So the session's system prompt is kept small here, under
// keepRecentTokens, or the cut would land on it instead of where pi's does.
func TestResumedCompactionTriggerMatchesPi(t *testing.T) {
	capture := loadTriggerCapture(t)
	for _, scenario := range capture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			tree, err := LoadSessionTree(writeCapturedSession(t, scenario.Entries))
			if err != nil {
				t.Fatal(err)
			}
			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
				Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: capture.ContextWindow, MaxTokens: 8192}},
			})
			t.Cleanup(reg.Unregister)
			var got []triggerRequest
			prompted := false
			summaries := 0
			record := func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
				if lead, ok := ai.GetInitialSystemMessage(req.Messages); ok && ai.GetSystemMessageText(lead) == summarizationSystemPrompt {
					if !prompted {
						text := userText(ai.WithoutInitialSystemMessage(req.Messages)[0])
						got = append(got, triggerRequest{Summarization: &text})
					}
					summaries++
					return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: fmt.Sprintf("SUMMARY %d", summaries)}}, ai.StopStop)
				}
				if !prompted {
					prompted = true
					var lines []string
					for _, m := range req.Messages {
						if m.MessageRole() == ai.RoleSystem {
							continue
						}
						line := string(m.MessageRole()) + ":"
						switch v := m.(type) {
						case ai.UserMessage:
							line += textOf(v.Content)
						case ai.AssistantMessage:
							line += textOf(v.Content)
						case ai.ToolResultMessage:
							line += textOf(v.Content)
						}
						lines = append(lines, line)
					}
					got = append(got, triggerRequest{Prompt: lines})
				}
				return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "reply"}}, ai.StopStop)
			}
			reg.SetResponses([]providers.FauxResponseStep{record, record, record, record})
			settings := CompactionSettings{Enabled: capture.Settings.Enabled, ReserveTokens: capture.Settings.ReserveTokens, KeepRecentTokens: capture.Settings.KeepRecentTokens}
			sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), SystemPrompt: "test", NoTools: NoToolsAll, Compaction: &settings})
			sess.LoadBranch(tree.BuildProjection())
			if _, err := sess.Run(context.Background(), scenario.Prompt); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(scenario.Requests) {
				t.Fatalf("sent %d requests up to the prompt's, pi sent %d:\n got %s\n  pi %s", len(got), len(scenario.Requests), describeTriggerRequests(got), describeTriggerRequests(scenario.Requests))
			}
			for i, want := range scenario.Requests {
				switch {
				case (got[i].Summarization != nil) != (want.Summarization != nil):
					t.Fatalf("request %d: got %s, pi sent %s", i+1, describeTriggerRequests(got[i:i+1]), describeTriggerRequests(scenario.Requests[i:i+1]))
				case want.Summarization != nil && *got[i].Summarization != *want.Summarization:
					t.Errorf("summarization request %d drifts from pi.\n--- got ---\n%s\n--- pi ---\n%s", i+1, *got[i].Summarization, *want.Summarization)
				case want.Summarization == nil && !slices.Equal(got[i].Prompt, want.Prompt):
					t.Errorf("the prompt's request drifts from pi:\n got %q\n  pi %q", got[i].Prompt, want.Prompt)
				}
			}
		})
	}
}

// pi's prepareCompaction finds nothing to compact when the branch's last entry
// is the compaction itself; the port finds nothing when no message was
// recorded after its checkpoint. capture-trigger.mts records what pi's
// prepareCompaction finds on each file as written. Compacting the resumed
// transcript as is, with a reserve that always calls for compaction, the port
// must summarize exactly when pi prepared something.
func TestResumedCompactionPreparesWhatPiPrepares(t *testing.T) {
	capture := loadTriggerCapture(t)
	for _, scenario := range capture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			tree, err := LoadSessionTree(writeCapturedSession(t, scenario.Entries))
			if err != nil {
				t.Fatal(err)
			}
			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
				Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: capture.ContextWindow, MaxTokens: 8192}},
			})
			t.Cleanup(reg.Unregister)
			requests := 0
			step := func(ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
				requests++
				return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "SUMMARY"}}, ai.StopStop)
			}
			reg.SetResponses([]providers.FauxResponseStep{step, step})
			sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll,
				Compaction: &CompactionSettings{Enabled: true, ReserveTokens: capture.ContextWindow, KeepRecentTokens: capture.Settings.KeepRecentTokens}})
			sess.LoadBranch(tree.BuildProjection())
			sess.compact(context.Background(), sess.compactState, sess.History())
			if (requests > 0) != scenario.Prepared {
				t.Fatalf("sent %d summarization requests; pi's prepareCompaction prepared a compaction: %v", requests, scenario.Prepared)
			}
		})
	}
}

// triggerCapture is testdata/compaction/trigger-0.87.1.json, written by
// capture-trigger.mts.
type triggerCapture struct {
	ContextWindow int `json:"contextWindow"`
	Settings      struct {
		Enabled          bool `json:"enabled"`
		ReserveTokens    int  `json:"reserveTokens"`
		KeepRecentTokens int  `json:"keepRecentTokens"`
	} `json:"settings"`
	Scenarios []struct {
		Name     string            `json:"name"`
		Entries  []json.RawMessage `json:"entries"`
		Prepared bool              `json:"prepared"`
		Prompt   string            `json:"prompt"`
		Requests []triggerRequest  `json:"requests"`
	} `json:"scenarios"`
}

func loadTriggerCapture(t *testing.T) triggerCapture {
	t.Helper()
	data, err := os.ReadFile("testdata/compaction/trigger-0.87.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture triggerCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Scenarios) == 0 {
		t.Fatal("capture holds no scenarios")
	}
	return capture
}

// triggerRequest is one request capture-trigger.mts records: a summarization
// request's user text, or the prompt's own request as its non-system messages.
type triggerRequest struct {
	Summarization *string  `json:"summarization"`
	Prompt        []string `json:"prompt"`
}

func describeTriggerRequests(requests []triggerRequest) string {
	kinds := make([]string, len(requests))
	for i, r := range requests {
		kinds[i] = "prompt"
		if r.Summarization != nil {
			kinds[i] = "summarization"
		}
	}
	return "[" + strings.Join(kinds, ", ") + "]"
}

// writeCapturedSession writes a capture's session entries as a JSONL file and
// returns its path.
func writeCapturedSession(t *testing.T, entries []json.RawMessage) string {
	t.Helper()
	var file strings.Builder
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		file.Write(line)
		file.WriteByte('\n')
	}
	return writeSessionText(t, file.String())
}

// writeSessionText writes a session file's text as is and returns its path.
func writeSessionText(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// describeMessages renders each message as capture-resume.mts's describe does:
// its role, then its text; an assistant's tool calls follow as
// [name JSON.stringify(arguments)].
func describeMessages(messages []agent.AgentMessage) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		switch v := m.(type) {
		case ai.SystemMessage:
			out = append(out, "system:"+ai.GetSystemMessageText(v))
		case ai.UserMessage:
			out = append(out, "user:"+textOf(v.Content))
		case ai.ToolResultMessage:
			out = append(out, "toolResult:"+textOf(v.Content))
		case ai.AssistantMessage:
			line := "assistant:" + textOf(v.Content)
			for _, c := range v.Content {
				if call, ok := c.(ai.ToolCall); ok {
					args, _ := jstext.Stringify(call.OrderedArguments())
					line += " [" + call.Name + " " + args + "]"
				}
			}
			out = append(out, line)
		default:
			out = append(out, fmt.Sprintf("%T", m))
		}
	}
	return out
}
