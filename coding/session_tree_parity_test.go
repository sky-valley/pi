package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Session-tree reconstruction parity against real pi.
//
// The .json scenarios in testdata/sessparity and their .golden.json expected
// outputs were captured by running pi's own buildSessionContext+convertToLlm
// over the same scenarios (testdata/sessparity/capture.mts, npm build 0.87.0 of
// @earendil-works/pi-coding-agent; one scenario from 1.3.0, see capture.mts).
// This test reconstructs each scenario through the Go port and asserts the
// {role,text,stringContent} projection — plus the whole message for a system
// message — matches pi byte-for-byte, as does BuildProjection's per-entry
// provenance, so any drift from pi's behaviour fails the build.

type parityScenario struct {
	LeafID  json.RawMessage   `json:"leafId"`
	Entries []json.RawMessage `json:"entries"`
}

type parityModel struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

type parityMsg struct {
	Role string `json:"role"`
	Text string `json:"text"`
	// StringContent marks content held as a plain string rather than blocks.
	StringContent bool `json:"stringContent,omitempty"`
	// Message is the whole system message, whose key order, sections and tool
	// fields are part of what a resumed session replays.
	Message json.RawMessage `json:"message,omitempty"`
}

// parityEntry is one selected entry of pi's buildSessionProjection: its id and
// the number of model messages it contributes.
type parityEntry struct {
	ID       string `json:"id"`
	Messages int    `json:"messages"`
}

type parityOut struct {
	ThinkingLevel string        `json:"thinkingLevel"`
	Model         *parityModel  `json:"model"`
	Messages      []parityMsg   `json:"messages"`
	Entries       []parityEntry `json:"entries"`
}

func parityText(content ai.ContentList) string {
	var b strings.Builder
	for _, c := range content {
		switch v := c.(type) {
		case ai.TextContent:
			b.WriteString(v.Text)
		case ai.ImageContent:
			b.WriteString("<image>")
		default:
			b.WriteString("<other>")
		}
	}
	return b.String()
}

func projectParityMsg(m ai.Message) parityMsg {
	switch v := m.(type) {
	case ai.UserMessage:
		_, isString := v.StringContent()
		return parityMsg{Role: "user", Text: parityText(v.Content), StringContent: isString}
	case *ai.AssistantMessage:
		return parityMsg{Role: "assistant", Text: parityText(v.Content)}
	case ai.AssistantMessage:
		return parityMsg{Role: "assistant", Text: parityText(v.Content)}
	case ai.ToolResultMessage:
		return parityMsg{Role: "toolResult", Text: parityText(v.Content)}
	case ai.SystemMessage:
		raw, err := json.Marshal(v)
		if err != nil {
			return parityMsg{Role: "system", Text: "<unmarshalable>"}
		}
		_, isString := v.StringContent()
		return parityMsg{Role: "system", Text: parityText(v.Content), StringContent: isString, Message: raw}
	}
	return parityMsg{Role: string(m.MessageRole())}
}

func reconstructScenario(t *testing.T, scenarioPath string) parityOut {
	t.Helper()
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		t.Fatal(err)
	}
	var sc parityScenario
	if err := json.Unmarshal(data, &sc); err != nil {
		t.Fatal(err)
	}

	// Materialize the entries as a JSONL session file (LoadSessionTree's input).
	tmp, err := os.CreateTemp(t.TempDir(), "sess-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sc.Entries {
		tmp.Write(e)
		tmp.WriteString("\n")
	}
	tmp.Close()

	tree, err := LoadSessionTree(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}

	explicitNull := string(sc.LeafID) == "null"
	var leafID string
	if !explicitNull && len(sc.LeafID) > 0 {
		json.Unmarshal(sc.LeafID, &leafID)
	}
	tree.LeafID = leafID

	ctx := tree.BuildContext()
	projection := tree.BuildProjection()
	if explicitNull {
		ctx = tree.BuildContextNull()
		projection = BranchProjection{}
	}

	out := parityOut{ThinkingLevel: ctx.ThinkingLevel, Messages: []parityMsg{}, Entries: []parityEntry{}}
	if ctx.Provider != "" || ctx.ModelID != "" {
		out.Model = &parityModel{Provider: ctx.Provider, ModelID: ctx.ModelID}
	}
	for _, m := range ctx.Messages {
		out.Messages = append(out.Messages, projectParityMsg(m))
	}
	for _, e := range projection.Entries {
		out.Entries = append(out.Entries, parityEntry{ID: e.SourceEntry.ID, Messages: len(e.Messages)})
	}
	return out
}

func TestSessionTreeParityWithPi(t *testing.T) {
	scenarios, err := filepath.Glob("testdata/sessparity/*.json")
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, scenarioPath := range scenarios {
		if strings.HasSuffix(scenarioPath, ".golden.json") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(scenarioPath), ".json")
		t.Run(name, func(t *testing.T) {
			got := reconstructScenario(t, scenarioPath)

			goldenPath := filepath.Join("testdata/sessparity", name+".golden.json")
			goldenData, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			var want parityOut
			if err := json.Unmarshal(goldenData, &want); err != nil {
				t.Fatal(err)
			}

			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			wantJSON, _ := json.MarshalIndent(want, "", "  ")
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("reconstruction diverges from pi golden:\n--- pi (want) ---\n%s\n--- go (got) ---\n%s", wantJSON, gotJSON)
			}
		})
		ran++
	}
	if ran == 0 {
		t.Fatal("no parity scenarios found in testdata/sessparity")
	}
}
