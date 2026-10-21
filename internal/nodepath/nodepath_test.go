package nodepath

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
)

// nodeCase is one answer of Node v26.4.0 (testdata/capture.mjs).
type nodeCase struct {
	Fn     string            `json:"fn"`
	Args   []json.RawMessage `json:"args"`
	Result json.RawMessage   `json:"result"`
	Error  *string           `json:"error"`
}

// TestAgreesWithNode runs every case Node answered (testdata/node.json) and
// compares results and failures. Node's error messages are not compared:
// callers only learn that the conversion failed.
func TestAgreesWithNode(t *testing.T) {
	data, err := os.ReadFile("testdata/node.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Node  string     `json:"node"`
		Cwd   string     `json:"cwd"`
		Cases []nodeCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("testdata/node.json has no cases; recapture it with node testdata/capture.mjs")
	}
	host := &Host{Cwd: func() string { return golden.Cwd }, Getenv: func(string) string { return "" }}
	flavors := map[string]Flavor{"posix": PosixPath{Host: host}, "win32": Win32Path{Host: host}}
	unsupported := 0
	for _, c := range golden.Cases {
		var strs []string
		var windows bool
		for _, arg := range c.Args {
			var s string
			if json.Unmarshal(arg, &s) == nil {
				strs = append(strs, s)
			} else if err := json.Unmarshal(arg, &windows); err != nil {
				t.Fatalf("%s: argument %s", c.Fn, arg)
			}
		}
		var got any
		var gotErr error
		switch c.Fn {
		case "fileURLToPath":
			got, gotErr = FileURLToPath(strs[0], windows)
		default:
			name, method, ok := cutDot(c.Fn)
			if !ok {
				t.Fatalf("unknown function %s", c.Fn)
			}
			flavor := flavors[name]
			switch method {
			case "normalize":
				got = flavor.Normalize(strs[0])
			case "isAbsolute":
				got = flavor.IsAbsolute(strs[0])
			case "basename":
				got = flavor.Basename(strs[0])
			case "toNamespacedPath":
				got = flavor.ToNamespacedPath(strs[0])
			case "resolve":
				got = flavor.Resolve(strs...)
			case "join":
				got = flavor.Join(strs...)
			default:
				t.Fatalf("unknown function %s", c.Fn)
			}
		}
		label := fmt.Sprintf("%s(%s)", c.Fn, joinRaw(c.Args))
		if c.Error != nil {
			if gotErr == nil {
				t.Errorf("%s = %q, Node throws %q", label, got, *c.Error)
			}
			continue
		}
		if errors.Is(gotErr, ErrUnsupportedHost) {
			// Known debt: IPv6 and IDNA hosts (ledger-notes, env-client.md).
			unsupported++
			t.Logf("%s: unsupported host; Node gives %s", label, c.Result)
			continue
		}
		if gotErr != nil {
			t.Errorf("%s: %v, Node gives %s", label, gotErr, c.Result)
			continue
		}
		want, _ := json.Marshal(got)
		var wantValue, gotValue any
		_ = json.Unmarshal(c.Result, &wantValue)
		_ = json.Unmarshal(want, &gotValue)
		if fmt.Sprint(wantValue) != fmt.Sprint(gotValue) {
			t.Errorf("%s = %s, Node gives %s", label, want, c.Result)
		}
	}
	if unsupported > 3 {
		t.Errorf("%d cases fell back on unsupported hosts; the corpus has 3 (IPv6, punycode, non-ASCII)", unsupported)
	}
}

func cutDot(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

func joinRaw(args []json.RawMessage) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += ", "
		}
		out += string(a)
	}
	return out
}
