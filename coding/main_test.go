package coding

import (
	"fmt"
	"os"
	"testing"

	"github.com/sky-valley/pi/agent"
)

// TestMain gives the package's tests a temp directory of their own, removed
// afterwards. The shell tools save the full output of a truncated command
// there (pi-bash-*.log), and tests must not leave those files in the user's
// temp directory, where pi's own sessions write the same names.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pi-coding-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "coding tests: cannot create a temp directory:", err)
		os.Exit(1)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} { // TMP and TEMP are Windows'
		os.Setenv(key, dir)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// newTestSession is NewSession for a test that expects the options to be
// valid: an error fails the test.
func newTestSession(t testing.TB, opts SessionOptions) *Session {
	t.Helper()
	sess, err := NewSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// resolveTestTools is resolveTools with no session behind the tools, for
// options the test expects to be valid.
func resolveTestTools(t testing.TB, cwd string, opts SessionOptions) []agent.AgentTool {
	t.Helper()
	tools, err := resolveTools(cwd, opts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}
