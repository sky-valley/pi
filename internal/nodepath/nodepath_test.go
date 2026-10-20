package nodepath

import (
	"encoding/json"
	"os"
	"testing"
)

// corpus is Node v26.4.0's own answers, captured for the env module's copy of
// these functions (env/internal/nodepath/testdata/capture.mjs).
const corpus = "../../env/internal/nodepath/testdata/node.json"

// TestAgreesWithNode runs every win32.normalize and win32.join case Node
// answered.
func TestAgreesWithNode(t *testing.T) {
	data, err := os.ReadFile(corpus)
	if err != nil {
		t.Fatalf("read Node's corpus: %v (it lives in the env module of this repository)", err)
	}
	// Other functions' cases carry non-string arguments and results, so each
	// case is decoded only once its function is known to be one of these two.
	var golden struct {
		Cases []json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	ran := map[string]int{}
	for _, raw := range golden.Cases {
		var c struct {
			Fn     string          `json:"fn"`
			Args   json.RawMessage `json:"args"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if c.Fn != "win32.normalize" && c.Fn != "win32.join" {
			continue
		}
		var args []string
		var want string
		if err := json.Unmarshal(c.Args, &args); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if err := json.Unmarshal(c.Result, &want); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var got string
		if c.Fn == "win32.normalize" {
			got = Win32Normalize(args[0])
		} else {
			got = Win32Join(args...)
		}
		ran[c.Fn]++
		if got != want {
			t.Errorf("%s(%q) = %q, Node gives %q", c.Fn, args, got, want)
		}
	}
	if ran["win32.normalize"] == 0 || ran["win32.join"] == 0 {
		t.Fatalf("corpus ran %v; want both win32.normalize and win32.join cases", ran)
	}
}
