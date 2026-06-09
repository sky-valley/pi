package coding

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// TestBashStreamsPartialOutput verifies bash emits throttled partial output via
// onUpdate before the command finishes (so a host app can show live progress).
func TestBashStreamsPartialOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh sleep")
	}
	var mu sync.Mutex
	var updates []string
	onUpdate := func(r agent.AgentToolResult) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range r.Content {
			if tc, ok := c.(ai.TextContent); ok {
				updates = append(updates, tc.Text)
			}
		}
	}
	// Each line is followed by a sleep longer than the 100ms throttle, so an
	// intermediate update should fire before the final line.
	final, err := bashTool(t.TempDir()).Execute(context.Background(), "id",
		map[string]any{"command": "echo first; sleep 0.2; echo second; sleep 0.2; echo third"},
		onUpdate)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) == 0 {
		t.Fatalf("expected at least one streamed partial update, got none")
	}
	// A partial update should have arrived before "third" was printed.
	sawEarly := false
	for _, u := range updates {
		if strings.Contains(u, "first") && !strings.Contains(u, "third") {
			sawEarly = true
		}
	}
	if !sawEarly {
		t.Fatalf("expected an early partial update containing 'first' but not 'third'; updates=%v", updates)
	}
	// Final result still has everything.
	got := resultText(final)
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(got, want) {
			t.Fatalf("final output missing %q: %q", want, got)
		}
	}
}
