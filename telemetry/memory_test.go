package telemetry

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// The cases below mirror pi's telemetry conformance suite
// (packages/telemetry/src/testing/conformance.ts, upstream 35f5c265d) for
// every behaviour that survives the port. Its "unreadable payload" cases --
// a Proxy whose getters throw on read/enumerate -- have no Go analog: reading
// a map or appending to a slice cannot fail, so pi's defensive try/catch
// around each mutator guards a failure mode Go does not have.

func TestInMemoryContextRecordsSpanLifecycle(t *testing.T) {
	c := NewInMemoryContext()
	var seen Span
	err := c.StartSpan(SpanOptions{Name: "recording", Attributes: SpanAttributes{"kind": "read"}}, func(s Span) error {
		seen = s
		return nil
	})
	if err != nil {
		t.Fatalf("StartSpan: %v", err)
	}
	if seen == nil {
		t.Fatal("callback never ran")
	}
	spans := c.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.ID != 1 || s.ParentID != 0 {
		t.Fatalf("id/parent = %d/%d, want 1/0", s.ID, s.ParentID)
	}
	if s.Name != "recording" || s.Attributes["kind"] != "read" {
		t.Fatalf("name/attrs = %q/%v", s.Name, s.Attributes)
	}
	if s.Status.Code != StatusOK || !s.Settled || s.EndSequence != 1 {
		t.Fatalf("status/settled/seq = %v/%v/%d", s.Status.Code, s.Settled, s.EndSequence)
	}
}

func TestInMemoryContextPropagatesErrorAndDerivesStatus(t *testing.T) {
	c := NewInMemoryContext()
	sentinel := errors.New("boom")
	err := c.StartSpan(SpanOptions{Name: "failing"}, func(Span) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback's error unchanged", err)
	}
	s := c.Spans()[0]
	if s.Status.Code != StatusError {
		t.Fatalf("status = %v, want error", s.Status.Code)
	}
	if s.Status.Error == nil || s.Status.Error.Message != "boom" {
		t.Fatalf("status.error = %+v, want message boom", s.Status.Error)
	}
	if s.Status.Error.Name != fmt.Sprintf("%T", sentinel) {
		t.Fatalf("status.error.name = %q, want the concrete error type", s.Status.Error.Name)
	}
}

// Mirrors "uses last explicit status without automatic overwrite": a span that
// set its own status keeps it even when the callback then fails.
func TestInMemoryContextExplicitStatusSurvivesFailure(t *testing.T) {
	c := NewInMemoryContext()
	want := SpanStatus{Code: StatusError, Error: &SpanError{Name: "expected-failure", Message: "declared"}}
	_ = c.StartSpan(SpanOptions{Name: "explicit"}, func(s Span) error {
		s.SetStatus(want)
		return errors.New("returned failure")
	})
	got := c.Spans()[0].Status
	if got.Error == nil || got.Error.Name != "expected-failure" || got.Error.Message != "declared" {
		t.Fatalf("status = %+v, want the explicit status preserved", got.Error)
	}
}

func TestInMemoryContextMergesAttributesAndOrdersEvents(t *testing.T) {
	c := NewInMemoryContext()
	_ = c.StartSpan(SpanOptions{Name: "s", Attributes: SpanAttributes{"a": 1}}, func(s Span) error {
		s.SetAttributes(SpanAttributes{"b": 2})
		s.SetAttributes(SpanAttributes{"a": 3})
		s.AddEvent("first", SpanAttributes{"index": 1})
		s.AddEvent("second", SpanAttributes{"index": 2})
		return nil
	})
	got := c.Spans()[0]
	if got.Attributes["a"] != 3 || got.Attributes["b"] != 2 {
		t.Fatalf("attributes = %v, want a=3 b=2", got.Attributes)
	}
	if len(got.Events) != 2 || got.Events[0].Name != "first" || got.Events[1].Name != "second" {
		t.Fatalf("events = %+v, want [first second] in order", got.Events)
	}
}

// Mirrors "makes calls after settlement inert".
func TestInMemoryContextMutatorsInertAfterSettle(t *testing.T) {
	c := NewInMemoryContext()
	var escaped Span
	_ = c.StartSpan(SpanOptions{Name: "s"}, func(s Span) error {
		escaped = s
		return nil
	})
	escaped.AddEvent("late", nil)
	escaped.SetAttributes(SpanAttributes{"late": true})
	escaped.SetStatus(SpanStatus{Code: StatusError})

	got := c.Spans()[0]
	if len(got.Events) != 0 {
		t.Fatalf("events = %+v, want none after settle", got.Events)
	}
	if _, ok := got.Attributes["late"]; ok {
		t.Fatal("late SetAttributes was recorded")
	}
	if got.Status.Code != StatusOK {
		t.Fatalf("status = %v, want the settled ok status kept", got.Status.Code)
	}
}

// Mirrors "records nested and concurrent child relationships".
func TestInMemoryContextRecordsNestedChildren(t *testing.T) {
	c := NewInMemoryContext()
	_ = c.StartSpan(SpanOptions{Name: "root"}, func(root Span) error {
		if err := root.StartSpan(SpanOptions{Name: "child-a"}, func(a Span) error {
			return a.StartSpan(SpanOptions{Name: "grandchild"}, func(Span) error { return nil })
		}); err != nil {
			return err
		}
		return root.StartSpan(SpanOptions{Name: "child-b"}, func(Span) error { return nil })
	})

	spans := c.Spans()
	byName := map[string]RecordedSpan{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	if len(spans) != 4 {
		t.Fatalf("spans = %d, want 4", len(spans))
	}
	if byName["root"].ParentID != 0 {
		t.Fatal("root should have no parent")
	}
	if byName["child-a"].ParentID != byName["root"].ID || byName["child-b"].ParentID != byName["root"].ID {
		t.Fatal("children must point at root")
	}
	if byName["grandchild"].ParentID != byName["child-a"].ID {
		t.Fatal("grandchild must point at child-a")
	}
	// Children settle before their parent.
	if byName["grandchild"].EndSequence >= byName["child-a"].EndSequence ||
		byName["child-a"].EndSequence >= byName["root"].EndSequence {
		t.Fatalf("end sequences out of order: %+v", byName)
	}
}

// A child started under an already-settled parent runs, but is not recorded --
// pi runs it on an inert span that keeps the recorder's capabilities
// (TestConformanceCarriesCapabilities).
func TestInMemoryContextChildOfSettledParentNotRecorded(t *testing.T) {
	c := NewInMemoryContext()
	var escaped Span
	_ = c.StartSpan(SpanOptions{Name: "root"}, func(s Span) error {
		escaped = s
		return nil
	})
	ran := false
	if err := escaped.StartSpan(SpanOptions{Name: "orphan"}, func(Span) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("orphan span: %v", err)
	}
	if !ran {
		t.Fatal("callback must still run, on an inert span")
	}
	if got := len(c.Spans()); got != 1 {
		t.Fatalf("spans = %d, want 1 (orphan not recorded)", got)
	}
}

// Snapshots are detached: neither the caller's input nor the returned copy can
// reach recorded state (pi's copyAttributes / `[...value]`).
func TestInMemoryContextSnapshotsAreDetached(t *testing.T) {
	c := NewInMemoryContext()
	tags := []string{"a", "b"}
	_ = c.StartSpan(SpanOptions{Name: "s", Attributes: SpanAttributes{"tags": tags}}, func(Span) error { return nil })

	tags[0] = "mutated"
	first := c.Spans()[0]
	if got := first.Attributes["tags"].([]string)[0]; got != "a" {
		t.Fatalf("recorded slice aliased the caller's: %q", got)
	}
	first.Attributes["injected"] = true
	if _, ok := c.Spans()[0].Attributes["injected"]; ok {
		t.Fatal("mutating a snapshot reached recorded state")
	}
}

// Per the SpanAttributes contract a nil value is pi's `undefined` entry and is
// not recorded.
func TestInMemoryContextDropsNilAttributeValues(t *testing.T) {
	c := NewInMemoryContext()
	_ = c.StartSpan(SpanOptions{Name: "s", Attributes: SpanAttributes{"present": 1, "absent": nil}}, func(Span) error {
		return nil
	})
	attrs := c.Spans()[0].Attributes
	if _, ok := attrs["absent"]; ok {
		t.Fatalf("nil value was recorded: %v", attrs)
	}
	if attrs["present"] != 1 {
		t.Fatalf("attributes = %v", attrs)
	}
}

// Go's analog of pi's synchronous throw: the span settles as a failure and the
// panic keeps unwinding.
func TestInMemoryContextSettlesOnPanic(t *testing.T) {
	c := NewInMemoryContext()
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("panic did not propagate")
			}
		}()
		_ = c.StartSpan(SpanOptions{Name: "panicking"}, func(Span) error {
			panic("kaboom")
		})
	}()
	s := c.Spans()[0]
	if !s.Settled || s.Status.Code != StatusError {
		t.Fatalf("settled/status = %v/%v, want true/error", s.Settled, s.Status.Code)
	}
}

func TestInMemoryContextConcurrentSiblings(t *testing.T) {
	c := NewInMemoryContext()
	_ = c.StartSpan(SpanOptions{Name: "root"}, func(root Span) error {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = root.StartSpan(SpanOptions{Name: fmt.Sprintf("child-%d", i)}, func(s Span) error {
					s.AddEvent("tick", SpanAttributes{"i": i})
					return nil
				})
			}()
		}
		wg.Wait()
		return nil
	})
	if got := len(c.Spans()); got != 9 {
		t.Fatalf("spans = %d, want 9", got)
	}
	ids := map[int]bool{}
	for _, s := range c.Spans() {
		if ids[s.ID] {
			t.Fatalf("duplicate span id %d", s.ID)
		}
		ids[s.ID] = true
	}
}

// TestInMemoryContextZeroValueIsUsable pins the invariant the reviewer broke by
// hand: ids and end-sequences must come out of recorder state, not out of
// counters a constructor seeds, or `var c InMemoryContext` silently records
// id-0 spans with lost parent links and inverted settle order.
func TestInMemoryContextZeroValueIsUsable(t *testing.T) {
	var c InMemoryContext
	_ = c.StartSpan(SpanOptions{Name: "root"}, func(root Span) error {
		return root.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
	})
	spans := c.Spans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	root, child := spans[0], spans[1]
	if root.ID != 1 || root.ParentID != 0 {
		t.Fatalf("root id/parent = %d/%d, want 1/0", root.ID, root.ParentID)
	}
	if child.ID != 2 || child.ParentID != root.ID {
		t.Fatalf("child id/parent = %d/%d, want 2/%d", child.ID, child.ParentID, root.ID)
	}
	if child.EndSequence != 1 || root.EndSequence != 2 {
		t.Fatalf("end sequences = child %d, root %d, want 1 then 2", child.EndSequence, root.EndSequence)
	}
}

// A panic must be distinguishable from an ordinary returned error, which the
// bare %T of the error type cannot do (both are *errors.errorString).
func TestInMemoryContextPanicStatusIsDistinguishable(t *testing.T) {
	c := NewInMemoryContext()
	func() {
		defer func() { _ = recover() }()
		_ = c.StartSpan(SpanOptions{Name: "panicking"}, func(Span) error { panic("kaboom") })
	}()
	got := c.Spans()[0].Status
	if got.Error == nil || got.Error.Name != "panic" || got.Error.Message != "kaboom" {
		t.Fatalf("status.error = %+v, want name panic / message kaboom", got.Error)
	}
}
