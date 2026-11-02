package formats

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// goldenRow is a capture row, [value, results] or, when value holds a lone
// surrogate, [value, results, results on value.toWellFormed()]: JSON
// decoding turns the lone surrogate into U+FFFD, so the well-formed results
// are what the Go checks must match.
type goldenRow struct {
	Value      string
	Results    map[string]bool
	WellFormed map[string]bool
}

func (r *goldenRow) UnmarshalJSON(data []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	if len(parts) != 2 && len(parts) != 3 {
		return fmt.Errorf("golden row has %d elements", len(parts))
	}
	if err := json.Unmarshal(parts[0], &r.Value); err != nil {
		return err
	}
	if err := json.Unmarshal(parts[1], &r.Results); err != nil {
		return err
	}
	if len(parts) == 3 {
		return json.Unmarshal(parts[2], &r.WellFormed)
	}
	return nil
}

type golden struct {
	Upstream string      `json:"upstream"`
	Node     string      `json:"node"`
	Formats  []string    `json:"formats"`
	Checks   int         `json:"checks"`
	Rows     []goldenRow `json:"rows"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	data, err := os.ReadFile("testdata/golden/capture-6536f8012.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestGolden replays every check upstream 6536f8012's formats recorded under
// Node v22 (testdata/golden/capture.mjs).
func TestGolden(t *testing.T) {
	g := loadGolden(t)
	checks := All()
	if got := slices.Sorted(maps.Keys(checks)); !slices.Equal(got, slices.Sorted(slices.Values(g.Formats))) {
		t.Fatalf("All() has %v, upstream has %v", got, g.Formats)
	}
	ran, surrogateRows := 0, 0
	failures := map[string][]string{}
	for _, row := range g.Rows {
		want := row.Results
		if row.WellFormed != nil {
			want = row.WellFormed
			surrogateRows++
		}
		for _, name := range slices.Sorted(maps.Keys(want)) {
			ran++
			if got := checks[name](row.Value); got != want[name] {
				failures[name] = append(failures[name], fmt.Sprintf("%s = %v, want %v", quote(row.Value), got, want[name]))
			}
		}
	}
	if ran != g.Checks {
		t.Errorf("replayed %d checks, the capture recorded %d", ran, g.Checks)
	}
	for _, name := range slices.Sorted(maps.Keys(failures)) {
		fs := failures[name]
		shown := fs[:min(len(fs), 15)]
		t.Errorf("%s: %d of the golden checks differ:\n  %s", name, len(fs), strings.Join(shown, "\n  "))
	}
	t.Logf("%d values, %d checks against %s on Node %s (%d values with a lone surrogate replayed as U+FFFD)",
		len(g.Rows), ran, g.Upstream, g.Node, surrogateRows)
}

// TestGoldenLoneSurrogates lists where a lone surrogate, which a Go string
// cannot hold, changes upstream's answer: the divergence the package doc
// describes.
func TestGoldenLoneSurrogates(t *testing.T) {
	g := loadGolden(t)
	for _, row := range g.Rows {
		for name, raw := range row.Results {
			if wf, ok := row.WellFormed[name]; ok && wf != raw {
				t.Logf("%s(%s): %v with the lone surrogate, %v with U+FFFD", name, quote(row.Value), raw, wf)
			}
		}
	}
}

func TestAllReturnsANewMap(t *testing.T) {
	a := All()
	delete(a, "date")
	if _, ok := All()["date"]; !ok {
		t.Fatal("All() shares its map between calls")
	}
	if len(All()) != 21 {
		t.Fatalf("All() has %d formats, want 21", len(All()))
	}
}

// quote shows a golden value in a failure, eliding the middle of long ones.
func quote(s string) string {
	q := strconv.QuoteToASCII(s)
	if len(q) > 120 {
		return fmt.Sprintf("%s…%s (%d bytes)", q[:60], q[len(q)-40:], len(s))
	}
	return q
}
