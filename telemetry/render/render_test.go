package render

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-valley/pi/telemetry"
)

// pi's test/render.test.ts.

func span(id string, edit ...func(*telemetry.StoredSpan)) telemetry.StoredSpan {
	s := telemetry.StoredSpan{
		ID:         id,
		TraceID:    "trace",
		Name:       id,
		StartedMs:  0,
		EndedMs:    10,
		Attributes: telemetry.SpanAttributes{},
		Events:     []telemetry.StoredEvent{},
		Links:      []telemetry.StoredLink{},
		Status:     telemetry.SpanStatus{Code: telemetry.StatusOK},
	}
	for _, e := range edit {
		e(&s)
	}
	return s
}

func parent(id string) func(*telemetry.StoredSpan) {
	return func(s *telemetry.StoredSpan) { s.ParentID = id }
}

func times(started, ended float64) func(*telemetry.StoredSpan) {
	return func(s *telemetry.StoredSpan) { s.StartedMs, s.EndedMs = started, ended }
}

func named(name string) func(*telemetry.StoredSpan) {
	return func(s *telemetry.StoredSpan) { s.Name = name }
}

func check(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("Trace() =\n%s\nwant\n%s", got, want)
	}
}

// pi lists attributes in insertion order; a Go map has none, so they come out
// array-index keys first, then by UTF-16 code unit (D69's order).
func TestTraceRendersAttributesDurationsErrorsEventsAndLinks(t *testing.T) {
	root := span("root", times(1000, 1020), func(s *telemetry.StoredSpan) {
		s.Attributes = telemetry.SpanAttributes{
			"request": "a b",
			"attempt": 2,
			"cached":  false,
			"tags":    []string{"a", "b"},
			"sizes":   []float64{1, 2},
			"flags":   []bool{true, false},
		}
		s.Events = []telemetry.StoredEvent{
			{Name: "checkpoint", TimeMs: 2, Attributes: telemetry.SpanAttributes{"phase": "tools"}},
			{Name: "tick", TimeMs: 3.5, Attributes: telemetry.SpanAttributes{}},
		}
		s.Links = []telemetry.StoredLink{
			{TraceID: "remote-trace", SpanID: "remote", Attributes: telemetry.SpanAttributes{"reason": "steer"}},
			{TraceID: "other-trace", SpanID: "other"},
		}
	})
	first := span("first", parent("root"), times(1005, 1009.5), func(s *telemetry.StoredSpan) {
		s.Status = telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "Error", Message: "failed call"}}
	})
	nested := span("nested", parent("first"), times(1006, 1007))
	second := span("second", parent("root"), times(1010, 1012), func(s *telemetry.StoredSpan) {
		s.Status = telemetry.SpanStatus{Code: telemetry.StatusError}
	})
	spans := []telemetry.StoredSpan{second, nested, first, root}
	before := []telemetry.StoredSpan{
		span("second", parent("root"), times(1010, 1012), func(s *telemetry.StoredSpan) { s.Status = second.Status }),
		nested, first, root,
	}
	check(t, Trace(spans), strings.Join([]string{
		`root attempt=2 cached=false flags=[true,false] request="a b" sizes=[1,2] tags=["a","b"] (20ms)`,
		`  · checkpoint phase="tools" (+2ms)`,
		"  · tick (+3.5ms)",
		`  → link trace remote-trace span remote reason="steer"`,
		"  → link trace other-trace span other",
		"  first (4.5ms) error Error: failed call",
		"    nested (1ms)",
		"  second (2ms) error",
	}, "\n"))
	if !reflect.DeepEqual(spans, before) {
		t.Errorf("Trace mutated its input: %+v", spans)
	}
}

func TestTraceTreatsMissingParentsAsRootsAndOrdersByStart(t *testing.T) {
	check(t, Trace([]telemetry.StoredSpan{
		span("later", parent("external"), times(20, 30)),
		span("child", parent("later"), times(25, 30)),
		span("earlier", times(10, 15), func(s *telemetry.StoredSpan) {
			s.Links = []telemetry.StoredLink{{TraceID: "trace", SpanID: "later"}}
		}),
	}), strings.Join([]string{"earlier (5ms)", "  → link trace trace span later", "later (10ms)", "  child (5ms)"}, "\n"))
}

func TestTracePrintsCrossTraceLinksWithoutFollowingThem(t *testing.T) {
	check(t, Trace([]telemetry.StoredSpan{
		span("server:1", named("server"), func(s *telemetry.StoredSpan) {
			s.TraceID = "server-trace"
			s.Links = []telemetry.StoredLink{{TraceID: "worker-trace", SpanID: "worker:1"}}
		}),
		span("worker:1", named("worker"), func(s *telemetry.StoredSpan) {
			s.TraceID = "worker-trace"
			s.Links = []telemetry.StoredLink{{TraceID: "server-trace", SpanID: "server:1"}}
		}),
	}), strings.Join([]string{
		"server (10ms)",
		"  → link trace worker-trace span worker:1",
		"worker (10ms)",
		"  → link trace server-trace span server:1",
	}, "\n"))
}

func TestTraceDeduplicatesSpansMergedFromStores(t *testing.T) {
	root := span("server:1", named("root"))
	child := span("worker:1", named("child"), parent("server:1"), func(s *telemetry.StoredSpan) {
		s.Events = []telemetry.StoredEvent{{Name: "done", TimeMs: 3, Attributes: telemetry.SpanAttributes{}}}
	})
	grandchild := span("worker:2", named("grandchild"), parent("worker:1"))
	check(t, Trace([]telemetry.StoredSpan{child, root, grandchild, root, child}),
		"root (10ms)\n  child (10ms)\n    · done (+3ms)\n    grandchild (10ms)")
}

// pi's Map keeps a duplicate ID at its first position with its last value.
func TestTraceKeepsTheLastDuplicateAtTheFirstPosition(t *testing.T) {
	check(t, Trace([]telemetry.StoredSpan{
		span("a", named("first a")),
		span("b"),
		span("a", named("last a")),
	}), "last a (10ms)\nb (10ms)")
}

func TestTracePreservesInputOrderForEqualStarts(t *testing.T) {
	spans := []telemetry.StoredSpan{span("root"), span("z", parent("root")), span("a", parent("root")), span("other")}
	check(t, Trace(spans), "root (10ms)\n  z (10ms)\n  a (10ms)\nother (10ms)")
	if spans[1].ID != "z" || spans[2].ID != "a" {
		t.Errorf("Trace reordered its input: %+v", spans)
	}
}

func TestTraceEscapesStringAttributeValues(t *testing.T) {
	check(t, Trace([]telemetry.StoredSpan{span("root", func(s *telemetry.StoredSpan) {
		s.Attributes = telemetry.SpanAttributes{"text": "line one\n\"line two\""}
	})}), `root text="line one\n\"line two\"" (10ms)`)
}

// JSON.stringify writes <, > and & as themselves, where encoding/json escapes
// them.
func TestTraceWritesAttributeValuesAsJSONStringify(t *testing.T) {
	check(t, Trace([]telemetry.StoredSpan{span("root", func(s *telemetry.StoredSpan) {
		s.Attributes = telemetry.SpanAttributes{"html": "<a&b>", "big": 1e21, "small": 1e-7}
	})}), `root big=1e+21 html="<a&b>" small=1e-7 (10ms)`)
}

func TestTraceOrdersArrayIndexKeysFirst(t *testing.T) {
	check(t, Trace([]telemetry.StoredSpan{span("root", func(s *telemetry.StoredSpan) {
		s.Attributes = telemetry.SpanAttributes{"b": 1, "10": 2, "2": 3, "a": 4, "02": 5}
	})}), "root 2=3 10=2 02=5 a=4 b=1 (10ms)")
}

func TestTraceRendersDeepTreesWithoutRecursion(t *testing.T) {
	spans := make([]telemetry.StoredSpan, 2000)
	for index := range spans {
		spans[index] = span(strconv.Itoa(index))
		if index > 0 {
			spans[index].ParentID = strconv.Itoa(index - 1)
		}
	}
	rendered := Trace(spans)
	if lines := strings.Split(rendered, "\n"); len(lines) != len(spans) {
		t.Fatalf("lines = %d, want %d", len(lines), len(spans))
	}
	if want := strings.Repeat("  ", 1999) + "1999 (10ms)"; !strings.HasSuffix(rendered, want) {
		t.Errorf("last line = %q, want %q", rendered[strings.LastIndex(rendered, "\n")+1:], want)
	}
}

func TestTraceRendersEmptyInputAsEmptyString(t *testing.T) {
	check(t, Trace(nil), "")
}
