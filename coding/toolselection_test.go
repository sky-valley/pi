package coding

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// toolSelectionCapture is testdata/toolselection: what pi-coding-agent 1.3.0's
// createToolNameMatcher, getToolListError, applyToolModifiers and
// createAgentSession make of tool lists (upstream 04b97ef00, ddaa0a034).
type toolSelectionCapture struct {
	MatcherNames []string `json:"matcherNames"`
	Matcher      []struct {
		Entries []string `json:"entries"`
		Matched []string `json:"matched"`
	} `json:"matcher"`
	Lists []struct {
		Entries []string `json:"entries"`
		Error   *string  `json:"error"`
		Applied []string `json:"applied"`
	} `json:"lists"`
	Sessions []struct {
		Name    string `json:"name"`
		Options struct {
			Tools        []string    `json:"tools"`
			ExcludeTools []string    `json:"excludeTools"`
			NoTools      NoToolsMode `json:"noTools"`
		} `json:"options"`
		CustomTools []string `json:"customTools"`
		Active      []string `json:"active"`
		Error       string   `json:"error"`
	} `json:"sessions"`
}

func loadToolSelectionCapture(t *testing.T) toolSelectionCapture {
	t.Helper()
	data, err := os.ReadFile("testdata/toolselection/toolselection-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture toolSelectionCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Matcher) == 0 || len(capture.Lists) == 0 || len(capture.Sessions) == 0 {
		t.Fatal("the tool selection capture holds no cases")
	}
	return capture
}

// Every entry list matches exactly the names pi's createToolNameMatcher
// matches: exact names, and `*` patterns whose other characters are literal
// and whose `*` stops at a JS line terminator.
func TestToolNameMatcherMatchesPi(t *testing.T) {
	capture := loadToolSelectionCapture(t)
	for _, row := range capture.Matcher {
		matches := toolNameMatcher(row.Entries)
		var got []string
		for _, name := range capture.MatcherNames {
			if matches(name) {
				got = append(got, name)
			}
		}
		if !reflect.DeepEqual(got, row.Matched) && (len(got) > 0 || len(row.Matched) > 0) {
			t.Errorf("entries %q match %q, pi %q", row.Entries, got, row.Matched)
		}
	}
}

// getToolListError and applyToolModifiers over DEFAULT_TOOL_NAMES.
func TestToolListModifiersMatchPi(t *testing.T) {
	for _, row := range loadToolSelectionCapture(t).Lists {
		want := ""
		if row.Error != nil {
			want = *row.Error
		}
		if got := toolListError(row.Entries); got != want {
			t.Errorf("toolListError(%q) = %q, pi %q", row.Entries, got, want)
		}
		if got := applyToolModifiers(defaultActiveToolNames, row.Entries); !reflect.DeepEqual(got, row.Applied) && (len(got) > 0 || len(row.Applied) > 0) {
			t.Errorf("applyToolModifiers(default, %q) = %q, pi %q", row.Entries, got, row.Applied)
		}
	}
}

// NewSession selects the tools createAgentSession activates for the same
// tools, excludeTools, noTools and custom tools, in pi's order, and fails
// where pi throws, with pi's message followed by a resolution hint.
func TestSessionToolSelectionMatchesPi(t *testing.T) {
	for _, row := range loadToolSelectionCapture(t).Sessions {
		t.Run(row.Name, func(t *testing.T) {
			var custom []agent.AgentTool
			for _, name := range row.CustomTools {
				custom = append(custom, agent.AgentTool{Name: name, Parameters: ai.Object()})
			}
			sess, err := NewSession(SessionOptions{
				Cwd:          t.TempDir(),
				ToolNames:    row.Options.Tools,
				ExcludeTools: row.Options.ExcludeTools,
				NoTools:      row.Options.NoTools,
				CustomTools:  custom,
			})
			if row.Error != "" {
				if err == nil || !strings.HasPrefix(err.Error(), row.Error+" (") {
					t.Fatalf("error = %v, want pi's %q followed by a resolution hint", err, row.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := toolNamesOf(sess); !reflect.DeepEqual(got, row.Active) && (len(got) > 0 || len(row.Active) > 0) {
				t.Fatalf("active tools = %q, pi %q", got, row.Active)
			}
		})
	}
}
