package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// pi's AgentSession declares its prompt along two paths (upstream 9e05370b2):
// prompt() puts a sections patch ahead of the user message, and the next-turn
// refresh its constructor installs on the agent (_installAgentNextTurnRefresh)
// re-diffs before every later turn of a run, prompt's and continue's alike.
// A forced prompt is never declared into the transcript: it is projected onto
// the request by the forced-prompt projection the constructor installs, so the
// transcript keeps recording the structured sections (upstream 16292398a).
// testdata/sessionprompt/capture.mts runs the real upstream Agent under
// AgentSession's own members at 16292398a; these tests replay the same steps
// through a Session and compare what each request carries.

const sessionPromptCaptureFile = "testdata/sessionprompt/sessionprompt-16292398a.json"

type sessionPromptThen struct {
	Model         string   `json:"model"`
	ThinkingLevel string   `json:"thinkingLevel"`
	Tools         []string `json:"tools"`
}

type sessionPromptReply struct {
	Text     *string `json:"text"`
	ToolCall *struct {
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"toolCall"`
	Then *sessionPromptThen `json:"then"`
}

type sessionPromptStep struct {
	Kind     string               `json:"kind"`
	Messages []json.RawMessage    `json:"messages"`
	Append   bool                 `json:"append"`
	Text     string               `json:"text"`
	Force    *string              `json:"force"`
	Replies  []sessionPromptReply `json:"replies"`
}

// promptProjection is a message as the capture projects it: the role, and for
// a system message its content text when non-empty, its section names and its
// declared tool names.
type promptProjection struct {
	Role         string   `json:"role"`
	Content      string   `json:"content,omitempty"`
	Sections     []string `json:"sections,omitempty"`
	ToolsAdded   []string `json:"toolsAdded,omitempty"`
	ToolsRemoved []string `json:"toolsRemoved,omitempty"`
}

// requestState is what the refresh hands the loop, as the provider sees it.
type requestState struct {
	Model     string   `json:"model"`
	Reasoning *string  `json:"reasoning"`
	Tools     []string `json:"tools"`
}

type sessionPromptScenario struct {
	Name                         string               `json:"name"`
	LoadoutChange                bool                 `json:"loadoutChange"`
	Steps                        []sessionPromptStep  `json:"steps"`
	Requests                     [][]promptProjection `json:"requests"`
	RequestState                 []requestState       `json:"requestState"`
	Transcript                   []promptProjection   `json:"transcript"`
	ReplayedPromptIsSystemPrompt bool                 `json:"replayedPromptIsSystemPrompt"`
}

// guidelineEntry is one [name, guidelines] pair of pi's _toolPromptGuidelines.
type guidelineEntry struct {
	Name       string
	Guidelines []string
}

func (e *guidelineEntry) UnmarshalJSON(data []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(data, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("guideline entry %s is not a [name, guidelines] pair", data)
	}
	if err := json.Unmarshal(pair[0], &e.Name); err != nil {
		return err
	}
	return json.Unmarshal(pair[1], &e.Guidelines)
}

type sessionPromptCapture struct {
	Sha              string                  `json:"sha"`
	Sessions         []sessionPromptScenario `json:"sessions"`
	PromptGuidelines struct {
		Tools []struct {
			Name             string   `json:"name"`
			PromptGuidelines []string `json:"promptGuidelines"`
		} `json:"tools"`
		Normalized []guidelineEntry `json:"normalized"`
	} `json:"promptGuidelines"`
}

func loadSessionPromptCapture(t *testing.T) sessionPromptCapture {
	t.Helper()
	data, err := os.ReadFile(sessionPromptCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture sessionPromptCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", sessionPromptCaptureFile, err)
	}
	if len(capture.Sessions) == 0 || len(capture.PromptGuidelines.Tools) == 0 {
		t.Fatalf("%s holds no cases", sessionPromptCaptureFile)
	}
	return capture
}

func projectPromptMessages[M agent.AgentMessage](messages []M) []promptProjection {
	out := make([]promptProjection, 0, len(messages))
	for _, m := range messages {
		system, ok := agent.AgentMessage(m).(ai.SystemMessage)
		if !ok {
			out = append(out, promptProjection{Role: string(m.MessageRole())})
			continue
		}
		projection := promptProjection{
			Role:     "system",
			Content:  ai.ContentText(system.Content),
			Sections: sectionNames(system.Sections),
		}
		if system.ToolsAdded != nil {
			projection.ToolsAdded = toolNames(system.ToolsAdded)
		}
		for _, tool := range system.ToolsRemoved {
			projection.ToolsRemoved = append(projection.ToolsRemoved, tool.Name)
		}
		out = append(out, projection)
	}
	return out
}

// sameJSON compares two values by their JSON encoding, so an empty list and an
// absent one (omitted either way) compare equal as they do in the capture.
func sameJSON(t *testing.T, got, want any) (string, string, bool) {
	t.Helper()
	g, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(g), string(w), string(g) == string(w)
}

func TestSessionDeclaresPromptAcrossTurnsLikePi(t *testing.T) {
	for _, scenario := range loadSessionPromptCapture(t).Sessions {
		t.Run(scenario.Name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
				Models: []providers.FauxModelDefinition{{ID: "faux-1", Reasoning: true}, {ID: "faux-2", Reasoning: true}},
			})
			defer reg.Unregister()
			sess := newTestSession(t, SessionOptions{Model: reg.GetModel("faux-1"), Cwd: t.TempDir()})

			var requests [][]promptProjection
			var states []requestState
			respond := func(reply sessionPromptReply) providers.FauxResponseStep {
				return func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, model *ai.Model) *ai.AssistantMessage {
					requests = append(requests, projectPromptMessages(req.Messages))
					state := requestState{Model: model.ID, Tools: toolNames(ai.GetCurrentTools(req.Messages))}
					if opts != nil && opts.Reasoning != "" {
						reasoning := string(opts.Reasoning)
						state.Reasoning = &reasoning
					}
					states = append(states, state)
					if then := reply.Then; then != nil {
						if then.Model != "" {
							sess.SetModel(reg.GetModel(then.Model), "")
						}
						if then.ThinkingLevel != "" {
							sess.SetThinkingLevel(agent.ThinkingLevel(then.ThinkingLevel))
						}
						if then.Tools != nil {
							var kept []agent.AgentTool
							for _, tool := range sess.Agent.State().Tools {
								for _, name := range then.Tools {
									if tool.Name == name {
										kept = append(kept, tool)
									}
								}
							}
							sess.Agent.SetTools(kept)
						}
					}
					if reply.Text != nil {
						return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: *reply.Text}}, ai.StopStop)
					}
					call := reply.ToolCall
					return providers.FauxAssistantMessage(ai.ContentList{ai.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments}}, ai.StopToolUse)
				}
			}
			responses := func(replies []sessionPromptReply) []providers.FauxResponseStep {
				steps := make([]providers.FauxResponseStep, 0, len(replies))
				for _, reply := range replies {
					steps = append(steps, respond(reply))
				}
				return steps
			}

			for _, step := range scenario.Steps {
				switch step.Kind {
				case "load":
					var messages []agent.AgentMessage
					if step.Append {
						messages = append(messages, sess.History()...)
					}
					for _, raw := range step.Messages {
						message, err := ai.UnmarshalMessage(raw)
						if err != nil {
							t.Fatal(err)
						}
						messages = append(messages, message)
					}
					sess.LoadHistory(messages)
				case "prompt":
					reg.SetResponses(responses(step.Replies))
					// The port has no before_agent_start (Scope entry 12): a forced
					// step sets the options a handler returning that systemPrompt
					// leaves, for this run only, as pi's run options are.
					sess.systemPromptOptions.ForceSystemPrompt = step.Force
					_, err := sess.Run(context.Background(), step.Text)
					sess.systemPromptOptions.ForceSystemPrompt = nil
					if err != nil {
						t.Fatal(err)
					}
				case "continue":
					reg.SetResponses(responses(step.Replies))
					if err := sess.Continue(context.Background()); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unknown step kind %q", step.Kind)
				}
				if n := reg.PendingResponseCount(); n != 0 {
					t.Fatalf("step %q left %d scripted replies unused", step.Kind, n)
				}
			}

			if got, want, ok := sameJSON(t, states, scenario.RequestState); !ok {
				t.Fatalf("request state drift from pi.\n--- got ---\n%s\n--- pi ---\n%s", got, want)
			}
			if scenario.LoadoutChange {
				// A mid-run loadout change also re-derives the prompt's tool
				// sections in pi (selectedTools = getActiveToolNames()): that is
				// setActiveTools' half of the refresh, queued with it.
				return
			}
			if got, want, ok := sameJSON(t, requests, scenario.Requests); !ok {
				t.Fatalf("requests drift from pi.\n--- got ---\n%s\n--- pi ---\n%s", got, want)
			}
			if got, want, ok := sameJSON(t, projectPromptMessages(sess.History()), scenario.Transcript); !ok {
				t.Fatalf("transcript drift from pi.\n--- got ---\n%s\n--- pi ---\n%s", got, want)
			}
			prompt, err := sess.SystemPrompt()
			if err != nil {
				t.Fatal(err)
			}
			current, ok := ai.GetCurrentSystemMessage(sess.History())
			if got := ok && ai.GetSystemMessageText(current) == prompt; got != scenario.ReplayedPromptIsSystemPrompt {
				t.Fatalf("replayed prompt equals SystemPrompt() = %v, pi %v", got, scenario.ReplayedPromptIsSystemPrompt)
			}
		})
	}
}

// Each tool's guidelines are normalized as pi's _normalizePromptGuidelines
// does — JS trim (which keeps U+0085 and U+180E and strips U+FEFF), empties
// dropped, first occurrence kept — and a tool left with none has no entry.
func TestToolPromptGuidelinesMatchPiCapture(t *testing.T) {
	capture := loadSessionPromptCapture(t).PromptGuidelines
	tools := make([]agent.AgentTool, 0, len(capture.Tools))
	for _, tool := range capture.Tools {
		tools = append(tools, agent.AgentTool{Name: tool.Name, PromptGuidelines: tool.PromptGuidelines})
	}
	want := map[string][]string{}
	for _, entry := range capture.Normalized {
		want[entry.Name] = entry.Guidelines
	}
	if got := toolPromptGuidelines(tools); !reflect.DeepEqual(got, want) {
		t.Fatalf("toolPromptGuidelines drift from pi.\n--- got ---\n%q\n--- pi ---\n%q", got, want)
	}
}

// pi's systemPrompt getter builds from the session's options, so the prompt is
// there before any request declares it, and the first request declares exactly
// that text.
func TestSessionSystemPromptBeforeTheFirstRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	cwd := t.TempDir()
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: cwd})

	before, err := sess.SystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if want := expectedSessionPrompt(t, cwd); before != want {
		t.Fatalf("SystemPrompt() before the first run:\n--- got ---\n%s\n--- want ---\n%s", before, want)
	}
	if declared := sess.Agent.State().SystemPrompt; declared != "" {
		t.Fatalf("nothing is declared before the first run, the transcript replays %q", declared)
	}

	reg.SetResponses([]providers.FauxResponseStep{fauxText("ok")})
	if _, err := sess.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if declared := sess.Agent.State().SystemPrompt; declared != before {
		t.Fatalf("declared prompt differs from SystemPrompt():\n--- declared ---\n%s\n--- SystemPrompt ---\n%s", declared, before)
	}
}
