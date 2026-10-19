package chord

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/sky-valley/pi/telemetry"
)

// Port of packages/chord/test/context.test.ts at d2e7d8b7c. Upstream's
// Context is a TypeScript reimplementation of Go's context package, so the
// mirror is context.Context plus typed keys: createContextKey → NewKey,
// withContextValue → WithValue, context.value(key) → key.Value(ctx),
// withAbortSignal/withCancel → context.WithCancel, withoutAbortSignal →
// context.WithoutCancel, awaitWithContext → a select on ctx.Done(), and the
// telemetry member (upstream 48909d987, 2ec55d808) → Telemetry(ctx),
// WithTelemetry and TraceIn.

// "provides distinct empty root contexts": a key reads nothing from either
// root, and the roots are Go's own.
func TestKeyReadsNothingFromEmptyRoots(t *testing.T) {
	key := NewKey[string]("value")
	for _, ctx := range []context.Context{context.Background(), context.TODO()} {
		if v, ok := key.Value(ctx); ok || v != "" {
			t.Errorf("%v: Value = %q, %v; want \"\", false", ctx, v, ok)
		}
	}
	if context.Background() == context.TODO() {
		t.Error("roots are not distinct")
	}
	for _, ctx := range []context.Context{context.Background(), context.TODO()} {
		if got := Telemetry(ctx); got != telemetry.NoopContext {
			t.Errorf("%v: Telemetry = %#v, want telemetry.NoopContext", ctx, got)
		}
	}
}

// "layers typed values without modifying parents"
func TestKeyLayersTypedValuesWithoutModifyingParents(t *testing.T) {
	firstKey := NewKey[string]("first")
	secondKey := NewKey[int]("second")
	first := WithValue(context.Background(), firstKey, "one")
	second := WithValue(first, secondKey, 2)
	replaced := WithValue(second, firstKey, "updated")

	if _, ok := firstKey.Value(context.Background()); ok {
		t.Error("background context gained a value")
	}
	if v, _ := firstKey.Value(first); v != "one" {
		t.Errorf("first.first = %q, want one", v)
	}
	if _, ok := secondKey.Value(first); ok {
		t.Error("first sees a value layered on its child")
	}
	if v, _ := firstKey.Value(second); v != "one" {
		t.Errorf("second.first = %q, want one", v)
	}
	if v, _ := secondKey.Value(second); v != 2 {
		t.Errorf("second.second = %d, want 2", v)
	}
	if v, _ := firstKey.Value(replaced); v != "updated" {
		t.Errorf("replaced.first = %q, want updated", v)
	}
	if v, _ := firstKey.Value(second); v != "one" {
		t.Errorf("second.first after replace = %q, want one", v)
	}
}

// Keys are identities, not names: upstream mints a fresh Symbol per
// createContextKey, so two keys sharing a description never alias.
func TestKeysWithTheSameNameAreDistinct(t *testing.T) {
	a := NewKey[string]("value")
	b := NewKey[string]("value")
	ctx := WithValue(context.Background(), a, "for a")
	if v, ok := b.Value(ctx); ok {
		t.Errorf("b read a's value %q", v)
	}
	if got := a.String(); got != "value" {
		t.Errorf("String() = %q, want the key's name", got)
	}
}

// "inherits parent cancellation and isolates child cancellation"
func TestChildCancellationIsIsolatedAndParentCancellationInherited(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	defer cancelParent(nil)
	child, cancelChild := context.WithCancelCause(parent)
	sibling, cancelSibling := context.WithCancelCause(parent)
	defer cancelSibling(nil)

	childCause := errors.New("child")
	cancelChild(childCause)
	if child.Err() == nil || context.Cause(child) != childCause {
		t.Errorf("child: err %v cause %v", child.Err(), context.Cause(child))
	}
	if sibling.Err() != nil || parent.Err() != nil {
		t.Error("child cancellation leaked to sibling or parent")
	}

	parentCause := errors.New("parent")
	cancelParent(parentCause)
	if sibling.Err() == nil || context.Cause(sibling) != parentCause {
		t.Errorf("sibling: err %v cause %v", sibling.Err(), context.Cause(sibling))
	}
}

// "masks caller cancellation for mandatory cleanup": WithoutCancel drops the
// deadline but keeps every typed value.
func TestWithoutCancelKeepsTypedValues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	key := NewKey[string]("value")
	ctx = WithValue(ctx, key, "preserved")
	cleanup := context.WithoutCancel(ctx)

	cancel()
	if ctx.Err() == nil {
		t.Error("context not cancelled")
	}
	if cleanup.Err() != nil {
		t.Error("cleanup context cancelled")
	}
	if v, ok := key.Value(cleanup); !ok || v != "preserved" {
		t.Errorf("cleanup value = %q, %v; want preserved, true", v, ok)
	}
}

// "stops waiting when the invocation is cancelled": cancelling the waiter's
// context releases only the waiter; the work still completes on its own.
func TestWaitingStopsWhenTheInvocationIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	work := make(chan string, 1)
	cancellation := errors.New("cancelled")

	cancel(cancellation)
	select {
	case <-ctx.Done():
		if context.Cause(ctx) != cancellation {
			t.Errorf("cause = %v, want %v", context.Cause(ctx), cancellation)
		}
	case v := <-work:
		t.Fatalf("received %q before the work ran", v)
	}

	work <- "completed later"
	if v := <-work; v != "completed later" {
		t.Errorf("work = %q", v)
	}
	done := make(chan string, 1)
	done <- "completed"
	select {
	case <-context.Background().Done():
		t.Fatal("background context is done")
	case v := <-done:
		if v != "completed" {
			t.Errorf("got %q", v)
		}
	}
}

// "derives telemetry independently from values and cancellation"
func TestTelemetryDerivesIndependentlyFromValuesAndCancellation(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	other := telemetry.NewInMemoryContext()
	err := recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(parent telemetry.Span) error {
		return other.StartSpan(telemetry.SpanOptions{Name: "other"}, func(otherSpan telemetry.Span) error {
			key := NewKey[string]("principal")
			cancellable, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := WithValue(cancellable, key, "owner")
			ctx := WithTelemetry(base, parent)
			replaced := WithTelemetry(ctx, otherSpan)
			if got := Telemetry(base); got != telemetry.NoopContext {
				t.Errorf("Telemetry(base) = %#v, want NoopContext", got)
			}
			if got := Telemetry(ctx); got != parent {
				t.Errorf("Telemetry(ctx) = %#v, want the caller span", got)
			}
			if got := Telemetry(replaced); got != otherSpan {
				t.Errorf("Telemetry(replaced) = %#v, want the other span", got)
			}
			if v, _ := key.Value(replaced); v != "owner" {
				t.Errorf("replaced value = %q, want owner", v)
			}
			if replaced.Done() != base.Done() {
				t.Error("WithTelemetry changed cancellation")
			}
			if got := Telemetry(context.WithoutCancel(ctx)); got != parent {
				t.Errorf("Telemetry(WithoutCancel) = %#v, want the caller span", got)
			}
			err := TraceIn(ctx, telemetry.SpanOptions{Name: "outer"}, func(child context.Context) error {
				if v, _ := key.Value(child); v != "owner" {
					t.Errorf("child value = %q, want owner", v)
				}
				if child.Done() != ctx.Done() {
					t.Error("TraceIn changed cancellation")
				}
				Telemetry(child).AddEvent("started", nil)
				return TraceIn(child, telemetry.SpanOptions{Name: "inner"}, func(nested context.Context) error {
					Telemetry(nested).AddLink(telemetry.SpanLink{Context: Telemetry(child)})
					return nil
				})
			})
			if err != nil {
				t.Errorf("TraceIn = %v", err)
			}
			spans := recorder.Spans()
			if len(spans) != 3 {
				t.Fatalf("spans = %d, want caller, outer and inner", len(spans))
			}
			caller, outer, inner := spans[0], spans[1], spans[2]
			if outer.Name != "outer" || outer.ParentID != caller.ID {
				t.Errorf("outer = %q under %d, want outer under %d", outer.Name, outer.ParentID, caller.ID)
			}
			if inner.Name != "inner" || inner.ParentID != outer.ID {
				t.Errorf("inner = %q under %d, want inner under %d", inner.Name, inner.ParentID, outer.ID)
			}
			if len(outer.Events) != 1 || outer.Events[0].Name != "started" {
				t.Errorf("outer events = %+v, want started", outer.Events)
			}
			if len(inner.Links) != 1 || !reflect.DeepEqual(inner.Links[0].Context, outer.Context) {
				t.Errorf("inner links = %+v, want outer", inner.Links)
			}
			if got := Telemetry(ctx); got != parent {
				t.Errorf("Telemetry(ctx) after TraceIn = %#v, want the caller span", got)
			}
			cancel()
			if ctx.Err() == nil {
				t.Error("ctx not cancelled")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// "isolates concurrent traceIn branches and preserves callback results and
// errors": concurrent branches parent their own children, a failure comes
// back unchanged, and TraceIn on a context without telemetry runs on the
// no-op span.
func TestTraceInIsolatesConcurrentBranches(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(parent telemetry.Span) error {
		ctx := WithTelemetry(context.Background(), parent)
		gate := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]string, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = TraceIn(ctx, telemetry.SpanOptions{Name: "first"}, func(child context.Context) error {
				<-gate
				return TraceIn(child, telemetry.SpanOptions{Name: "first.child"}, func(context.Context) error {
					results[0] = "first"
					return nil
				})
			})
		}()
		go func() {
			defer wg.Done()
			_ = TraceIn(ctx, telemetry.SpanOptions{Name: "second"}, func(child context.Context) error {
				err := TraceIn(child, telemetry.SpanOptions{Name: "second.child"}, func(context.Context) error {
					results[1] = "second"
					return nil
				})
				close(gate)
				return err
			})
		}()
		wg.Wait()
		if !reflect.DeepEqual(results, []string{"first", "second"}) {
			t.Errorf("results = %v", results)
		}
		spans := recorder.Spans()
		byName := map[string]telemetry.RecordedSpan{}
		for _, span := range spans {
			byName[span.Name] = span
		}
		for _, name := range []string{"first", "second"} {
			parentSpan, ok := byName[name]
			if !ok {
				t.Errorf("no %s span", name)
				continue
			}
			if child := byName[name+".child"]; child.ParentID != parentSpan.ID {
				t.Errorf("%s.child under %d, want %d", name, child.ParentID, parentSpan.ID)
			}
		}
		failure := errors.New("same value")
		if err := TraceIn(ctx, telemetry.SpanOptions{Name: "fail"}, func(context.Context) error { return failure }); err != failure {
			t.Errorf("TraceIn = %v, want the callback's own error", err)
		}
		ran := false
		err := TraceIn(context.Background(), telemetry.SpanOptions{Name: "noop"}, func(child context.Context) error {
			Telemetry(child).AddLink(telemetry.SpanLink{Context: parent})
			ran = true
			return nil
		})
		if err != nil || !ran {
			t.Errorf("TraceIn without telemetry = %v (ran %v), want the callback run on the no-op span", err, ran)
		}
		return nil
	})
}
