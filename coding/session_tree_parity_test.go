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
// (from @earendil-works/pi-coding-agent) over the same scenarios. This test
// reconstructs each scenario through the Go port and asserts the {role,text}
// projection matches pi byte-for-byte, so any drift from pi's behaviour fails
// the build.

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
}

type parityOut struct {
	ThinkingLevel string       `json:"thinkingLevel"`
	Model         *parityModel `json:"model"`
	Messages      []parityMsg  `json:"messages"`
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
		return parityMsg{Role: "user", Text: parityText(v.Content)}
	case *ai.AssistantMessage:
		return parityMsg{Role: "assistant", Text: parityText(v.Content)}
	case ai.AssistantMessage:
		return parityMsg{Role: "assistant", Text: parityText(v.Content)}
	case ai.ToolResultMessage:
		return parityMsg{Role: "toolResult", Text: parityText(v.Content)}
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
	if explicitNull {
		ctx = tree.BuildContextNull()
	}

	out := parityOut{ThinkingLevel: ctx.ThinkingLevel, Messages: []parityMsg{}}
	if ctx.Provider != "" || ctx.ModelID != "" {
		out.Model = &parityModel{Provider: ctx.Provider, ModelID: ctx.ModelID}
	}
	for _, m := range ctx.Messages {
		out.Messages = append(out.Messages, projectParityMsg(m))
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
