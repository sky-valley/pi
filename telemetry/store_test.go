package telemetry

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// pi's test/store-conformance.ts (the shared read-store checks) and
// test/store.test.ts (the in-memory adapter's own store snapshots).
//
// pi mocks Date.now() and performance.now() with vi.spyOn; the in-memory
// recorder reads them through dateNow and performanceNow, which these tests
// replace for their duration.

// mockClock replaces *clock with one that returns the value the returned
// pointer holds, until the test ends.
func mockClock(t *testing.T, clock *func() float64, ms float64) *float64 {
	t.Helper()
	now, previous := ms, *clock
	*clock = func() float64 { return now }
	t.Cleanup(func() { *clock = previous })
	return &now
}

// storeFixture is pi's StoreFixture: a recording context with metrics and
// propagation, the store reading it back, and a flush (a no-op in memory).
type storeFixture struct {
	context     Context
	metrics     Metrics
	propagation Propagation
	store       Store
	flush       func()
}

func floatPtr(v float64) *float64 { return &v }

func mustFindTraces(t *testing.T, store Store, query TraceQuery) TracePage {
	t.Helper()
	page, err := store.FindTraces(context.Background(), query)
	if err != nil {
		t.Fatalf("FindTraces(%+v): %v", query, err)
	}
	return page
}

func mustGetTrace(t *testing.T, store Store, traceID string) *StoredTrace {
	t.Helper()
	trace, err := store.GetTrace(context.Background(), traceID)
	if err != nil {
		t.Fatalf("GetTrace(%q): %v", traceID, err)
	}
	return trace
}

func mustFindMetrics(t *testing.T, store Store, query MetricQuery) []StoredMetric {
	t.Helper()
	rows, err := store.FindMetrics(context.Background(), query)
	if err != nil {
		t.Fatalf("FindMetrics(%+v): %v", query, err)
	}
	return rows
}

func firstTrace(t *testing.T, page TracePage) StoredTrace {
	t.Helper()
	if len(page.Traces) == 0 {
		t.Fatal("no traces, want at least one")
	}
	return page.Traces[0]
}

func storedSpanNamed(t *testing.T, spans []StoredSpan, name string) StoredSpan {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("no stored span %q among %+v", name, spans)
	return StoredSpan{}
}

func firstSpanNames(traces []StoredTrace) []string {
	names := []string{}
	for _, trace := range traces {
		names = append(names, trace.Spans[0].Name)
	}
	return names
}

// storeConformance is pi's telemetryStoreConformance.
func storeConformance(t *testing.T, newFixture func(t *testing.T) storeFixture) {
	t.Run("returns only settled traces, complete span snapshots, and detached results", func(t *testing.T) {
		mockClock(t, &dateNow, 20_000)
		f := newFixture(t)
		gate, started, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			_ = f.context.StartSpan(SpanOptions{Name: "root", Attributes: SpanAttributes{"tags": []string{"original"}}}, func(root Span) error {
				root.AddEvent("event", SpanAttributes{"tags": []string{"original"}})
				close(started)
				<-gate
				return root.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
			})
		}()
		<-started
		f.flush()
		if traces := mustFindTraces(t, f.store, TraceQuery{}).Traces; len(traces) != 0 {
			t.Errorf("traces while the root is open = %+v, want none: only settled spans are stored", traces)
		}
		if trace := mustGetTrace(t, f.store, "missing"); trace != nil {
			t.Errorf("GetTrace(missing) = %+v, want nil", trace)
		}
		close(gate)
		<-done
		f.flush()
		traces := mustFindTraces(t, f.store, TraceQuery{}).Traces
		if len(traces) != 1 {
			t.Fatalf("traces = %+v, want one", traces)
		}
		trace := traces[0]
		root, child := storedSpanNamed(t, trace.Spans, "root"), storedSpanNamed(t, trace.Spans, "child")
		if !regexp.MustCompile(`^[0-9a-f-]{36}:[1-9][0-9]*$`).MatchString(root.ID) {
			t.Errorf("root id = %q, want <recorder UUID>:<counter>", root.ID)
		}
		if root.TraceID != root.ID || root.ParentID != "" {
			t.Errorf("root trace/parent = %q/%q, want its own id and none", root.TraceID, root.ParentID)
		}
		if child.ParentID != root.ID || child.TraceID != root.TraceID {
			t.Errorf("child parent/trace = %q/%q, want %q/%q", child.ParentID, child.TraceID, root.ID, root.TraceID)
		}
		if root.StartedMs != 20_000 || root.EndedMs < root.StartedMs {
			t.Errorf("root started/ended = %v/%v, want 20000 and no earlier", root.StartedMs, root.EndedMs)
		}
		if len(root.Events) != 1 || root.Events[0].TimeMs < 0 {
			t.Fatalf("root events = %+v, want one at a nonnegative offset", root.Events)
		}
		root.Attributes["tags"].([]string)[0] = "changed"
		root.Events[0].Attributes["tags"].([]string)[0] = "changed"
		fresh := storedSpanNamed(t, mustGetTrace(t, f.store, trace.TraceID).Spans, "root")
		if got := fresh.Attributes["tags"]; !reflect.DeepEqual(got, []string{"original"}) {
			t.Errorf("attributes after mutating a result = %v, want [original]", got)
		}
		if got := fresh.Events[0].Attributes["tags"]; !reflect.DeepEqual(got, []string{"original"}) {
			t.Errorf("event attributes after mutating a result = %v, want [original]", got)
		}
	})

	t.Run("filters by first-span name and start, any error, and all attributes on one span", func(t *testing.T) {
		now := mockClock(t, &dateNow, 10_000)
		f := newFixture(t)
		_ = f.context.StartSpan(SpanOptions{Name: "old"}, func(root Span) error {
			*now = 30_000
			_ = root.StartSpan(SpanOptions{Name: "recent-child", Attributes: SpanAttributes{"left": 1}}, func(Span) error { return nil })
			return root.StartSpan(SpanOptions{Name: "split", Attributes: SpanAttributes{"right": 2}}, func(Span) error { return nil })
		})
		*now = 20_000
		_ = f.context.StartSpan(SpanOptions{Name: "wanted"}, func(root Span) error {
			*now = 20_001
			return root.StartSpan(SpanOptions{Name: "failure", Attributes: SpanAttributes{"left": 1, "right": 2, "valid": true, `odd."key`: "value"}}, func(child Span) error {
				child.SetStatus(SpanStatus{Code: StatusError})
				return nil
			})
		})
		*now = 40_000
		_ = f.context.StartSpan(SpanOptions{Name: "new", Attributes: SpanAttributes{"valid": 1}}, func(Span) error { return nil })
		f.flush()
		for _, c := range []struct {
			query TraceQuery
			want  []string
		}{
			{TraceQuery{Name: "wanted", Attributes: SpanAttributes{"left": 1, "right": 2}, Errors: true, StartMin: floatPtr(20_000), StartMax: floatPtr(20_000)}, []string{"wanted"}},
			{TraceQuery{Name: "failure"}, []string{}},
			{TraceQuery{Attributes: SpanAttributes{"left": 1, "right": 2}}, []string{"wanted"}},
			{TraceQuery{Attributes: SpanAttributes{"valid": true, `odd."key`: "value"}}, []string{"wanted"}},
			{TraceQuery{Attributes: SpanAttributes{"valid": 1}}, []string{"new"}},
			{TraceQuery{Errors: true}, []string{"wanted"}},
			{TraceQuery{StartMin: floatPtr(20_000)}, []string{"new", "wanted"}},
			{TraceQuery{StartMax: floatPtr(10_000)}, []string{"old"}},
		} {
			if got := firstSpanNames(mustFindTraces(t, f.store, c.query).Traces); !reflect.DeepEqual(got, c.want) {
				t.Errorf("FindTraces(%+v) first spans = %q, want %q", c.query, got, c.want)
			}
		}
	})

	t.Run("pages complete traces without duplicates when trace starts tie", func(t *testing.T) {
		now := mockClock(t, &dateNow, 20_000)
		f := newFixture(t)
		for index := range 25 {
			*now = 20_000
			_ = f.context.StartSpan(SpanOptions{Name: "root", Attributes: SpanAttributes{"index": index}}, func(root Span) error {
				*now = 20_001
				return root.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
			})
		}
		f.flush()
		first := mustFindTraces(t, f.store, TraceQuery{})
		if len(first.Traces) != 20 || first.Cursor == "" {
			t.Fatalf("first page = %d traces, cursor %q; want 20 and a cursor", len(first.Traces), first.Cursor)
		}
		rest := mustFindTraces(t, f.store, TraceQuery{Cursor: first.Cursor})
		if len(rest.Traces) != 5 || rest.Cursor != "" {
			t.Fatalf("second page = %d traces, cursor %q; want 5 and none", len(rest.Traces), rest.Cursor)
		}
		all := mustFindTraces(t, f.store, TraceQuery{Limit: 100}).Traces
		if !reflect.DeepEqual(append(first.Traces, rest.Traces...), all) {
			t.Error("the two pages differ from one page of every trace")
		}
		for _, trace := range all {
			if len(trace.Spans) != 2 {
				t.Errorf("trace %s holds %d spans, want both", trace.TraceID, len(trace.Spans))
			}
		}
		var ids, want []string
		for _, trace := range all {
			want = append(want, trace.TraceID)
		}
		cursor := ""
		for {
			page := mustFindTraces(t, f.store, TraceQuery{Name: "root", Limit: 3, Cursor: cursor})
			for _, trace := range page.Traces {
				ids = append(ids, trace.TraceID)
			}
			if cursor = page.Cursor; cursor == "" {
				break
			}
		}
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("paged ids = %q, want %q", ids, want)
		}
	})

	t.Run("inherits remote trace IDs and preserves remote parents and cross-trace links", func(t *testing.T) {
		mockClock(t, &dateNow, 20_000)
		sender, receiver := newFixture(t), newFixture(t)
		var carrier Carrier
		_ = sender.context.StartSpan(SpanOptions{Name: "sender"}, func(span Span) error {
			carrier = sender.propagation.Inject(span)
			return nil
		})
		sender.flush()
		sent := firstTrace(t, mustFindTraces(t, sender.store, TraceQuery{}))
		remote := receiver.propagation.Extract(roundtripJSON(t, carrier))
		_ = remote.StartSpan(SpanOptions{Name: "receiver"}, func(Span) error { return nil })
		_ = receiver.context.StartSpan(SpanOptions{Name: "linked", Links: []SpanLink{{Context: remote, Attributes: SpanAttributes{"reason": "background"}}}}, func(Span) error { return nil })
		receiver.flush()
		continued := mustGetTrace(t, receiver.store, sent.TraceID)
		if continued == nil || len(continued.Spans) != 1 {
			t.Fatalf("receiver's trace %s = %+v, want the one continued span", sent.TraceID, continued)
		}
		span := continued.Spans[0]
		if span.TraceID != sent.TraceID || span.ParentID != sent.Spans[0].ID || span.Name != "receiver" || span.ID == sent.Spans[0].ID {
			t.Errorf("continued span = %+v, want receiver under %s in trace %s with an id of its own", span, sent.Spans[0].ID, sent.TraceID)
		}
		linked := firstTrace(t, mustFindTraces(t, receiver.store, TraceQuery{Name: "linked"}))
		if linked.TraceID == sent.TraceID {
			t.Errorf("linked trace = %s, want a trace of its own", linked.TraceID)
		}
		if want := []StoredLink{{TraceID: sent.TraceID, SpanID: sent.Spans[0].ID, Attributes: SpanAttributes{"reason": "background"}}}; !reflect.DeepEqual(linked.Spans[0].Links, want) {
			t.Errorf("links = %+v, want %+v", linked.Spans[0].Links, want)
		}
		// pi also sets each key to the number 42, which a Go Carrier cannot
		// hold: only the missing-key half has a Go form.
		for key := range carrier {
			incomplete := copyCarrier(carrier)
			delete(incomplete, key)
			_ = receiver.propagation.Extract(incomplete).StartSpan(SpanOptions{Name: "fallback"}, func(Span) error { return nil })
		}
		receiver.flush()
		fallbacks := mustFindTraces(t, receiver.store, TraceQuery{Name: "fallback"}).Traces
		if len(fallbacks) != len(carrier) {
			t.Fatalf("fallback traces = %d, want %d: a carrier missing any key extracts the root", len(fallbacks), len(carrier))
		}
		for _, trace := range fallbacks {
			if trace.Spans[0].ParentID != "" || trace.TraceID != trace.Spans[0].ID {
				t.Errorf("fallback span %+v, want a root starting its own trace", trace.Spans[0])
			}
		}
	})

	t.Run("accepts external string identities without imposing an ID format", func(t *testing.T) {
		f := newFixture(t)
		var carrier Carrier
		_ = f.context.StartSpan(SpanOptions{Name: "source"}, func(span Span) error {
			carrier = f.propagation.Inject(span)
			return nil
		})
		external := Carrier{}
		for key := range carrier {
			external[key] = "external-" + key
		}
		parent := f.propagation.Extract(external)
		if got := f.propagation.Inject(parent); !reflect.DeepEqual(got, external) {
			t.Errorf("Inject(extracted) = %v, want %v", got, external)
		}
		_ = parent.StartSpan(SpanOptions{Name: "continued"}, func(Span) error { return nil })
		f.flush()
		traces := mustFindTraces(t, f.store, TraceQuery{Name: "continued"}).Traces
		if len(traces) != 1 {
			t.Fatalf("continued traces = %+v, want one", traces)
		}
		if !strings.HasPrefix(traces[0].TraceID, "external-") || !strings.HasPrefix(traces[0].Spans[0].ParentID, "external-") {
			t.Errorf("trace/parent = %q/%q, want the external identities", traces[0].TraceID, traces[0].Spans[0].ParentID)
		}
	})

	t.Run("queries metric names, inclusive interval bounds, typed attribute equality, and detached values", func(t *testing.T) {
		now := mockClock(t, &dateNow, 20_001)
		f := newFixture(t)
		counter := f.metrics.Counter("hits", InstrumentOptions{Unit: "By"})
		counter.Add(2, SpanAttributes{"method": "read", "valid": true})
		counter.Add(3, SpanAttributes{"valid": true, "method": "read"})
		counter.Add(11, SpanAttributes{"method": "write", "valid": 1})
		f.metrics.Counter("other", InstrumentOptions{}).Add(100, SpanAttributes{"method": "read", "valid": true})
		*now = 30_001
		counter.Add(7, SpanAttributes{"method": "read", "valid": true})
		*now = 40_001
		f.flush()
		query := MetricQuery{Name: "hits", StartMin: floatPtr(20_000), StartMax: floatPtr(20_000), Attributes: SpanAttributes{"method": "read", "valid": true}}
		rows := mustFindMetrics(t, f.store, query)
		if len(rows) == 0 {
			t.Fatalf("FindMetrics(%+v) = none, want the two read measurements", query)
		}
		count, sum, low, high := 0, 0.0, rows[0].Min, rows[0].Max
		for _, row := range rows {
			count += row.Count
			sum += row.Sum
			low, high = min(low, row.Min), max(high, row.Max)
			if row.Kind != MetricCounter || row.Unit != "By" || row.IntervalMs != 20_000 {
				t.Errorf("row %+v, want a counter in By at interval 20000", row)
			}
		}
		if count != 2 || sum != 5 || low != 2 || high != 3 {
			t.Errorf("count/sum/min/max = %d/%v/%v/%v, want 2/5/2/3", count, sum, low, high)
		}
		rows[0].Attributes["method"] = "changed"
		if got := mustFindMetrics(t, f.store, query)[0].Attributes["method"]; got != "read" {
			t.Errorf("attributes after mutating a result = %v, want read", got)
		}
		if got := mustFindMetrics(t, f.store, MetricQuery{Name: "missing"}); len(got) != 0 {
			t.Errorf("missing = %+v, want none", got)
		}
		total := func(query MetricQuery) float64 {
			sum := 0.0
			for _, row := range mustFindMetrics(t, f.store, query) {
				sum += row.Sum
			}
			return sum
		}
		if got := total(MetricQuery{Name: "hits", Attributes: SpanAttributes{"valid": 1}}); got != 11 {
			t.Errorf("valid=1 sum = %v, want 11: true is not 1", got)
		}
		if got := total(MetricQuery{Name: "hits", StartMin: floatPtr(30_000)}); got != 7 {
			t.Errorf("from 30000 sum = %v, want 7", got)
		}
	})
}

// pi store.test.ts: the conformance run on InMemoryTelemetryContext.
func TestInMemoryStoreConformance(t *testing.T) {
	storeConformance(t, func(*testing.T) storeFixture {
		c := NewInMemoryContext()
		return storeFixture{context: c, metrics: c, propagation: c, store: c, flush: func() {}}
	})
}

// "records wall-clock boundaries and relative event times without changing
// normalized event snapshots"
func TestInMemoryStoreRecordsWallClockBoundariesAndRelativeEventTimes(t *testing.T) {
	mockClock(t, &dateNow, 1000)
	clock := mockClock(t, &performanceNow, 100)
	c := NewInMemoryContext()
	id := ""
	_ = c.StartSpan(SpanOptions{Name: "timed"}, func(span Span) error {
		carrier := c.Inject(span)
		id = carrier[memoryScopeKey] + ":" + carrier[memorySpanKey]
		*clock = 110
		span.AddEvent("event", SpanAttributes{"count": 1})
		*clock = 125
		return nil
	})
	want := &StoredTrace{TraceID: id, Spans: []StoredSpan{{
		ID:         id,
		TraceID:    id,
		Name:       "timed",
		StartedMs:  1000,
		EndedMs:    1025,
		Attributes: SpanAttributes{},
		Events:     []StoredEvent{{Name: "event", TimeMs: 10, Attributes: SpanAttributes{"count": 1}}},
		Links:      []StoredLink{},
		Status:     SpanStatus{Code: StatusOK},
	}}}
	if got := mustGetTrace(t, c, id); !reflect.DeepEqual(got, want) {
		t.Errorf("GetTrace = %+v, want %+v", got, want)
	}
	if got := c.Spans()[0].Events; !reflect.DeepEqual(got, []RecordedEvent{{Name: "event", Attributes: SpanAttributes{"count": 1}}}) {
		t.Errorf("recorded events = %+v, want the untimed snapshot", got)
	}
}

// "maps raw metric measurements to count-one rows without exposing private
// timing in getMetrics"
func TestInMemoryStoreMapsMeasurementsToCountOneRows(t *testing.T) {
	mockClock(t, &dateNow, 20_001)
	c := NewInMemoryContext()
	c.Counter("calls", InstrumentOptions{}).Add(2, nil)
	c.Counter("calls", InstrumentOptions{}).Add(3, nil)
	var got [][3]float64
	for _, row := range mustFindMetrics(t, c, MetricQuery{Name: "calls"}) {
		got = append(got, [3]float64{row.IntervalMs, float64(row.Count), row.Sum})
	}
	if want := [][3]float64{{20_000, 1, 2}, {20_000, 1, 3}}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	want := []RecordedMeasurement{
		{Kind: MetricCounter, Name: "calls", Value: 2, Attributes: SpanAttributes{}},
		{Kind: MetricCounter, Name: "calls", Value: 3, Attributes: SpanAttributes{}},
	}
	if got := c.Measurements(); !reflect.DeepEqual(got, want) {
		t.Errorf("measurements = %+v, want %+v", got, want)
	}
}

// "detaches link attributes and error details"
func TestInMemoryStoreDetachesLinkAttributesAndErrorDetails(t *testing.T) {
	c := NewInMemoryContext()
	var target Span
	_ = c.StartSpan(SpanOptions{Name: "target"}, func(span Span) error {
		target = span
		return nil
	})
	_ = c.StartSpan(SpanOptions{Name: "source", Links: []SpanLink{{Context: target, Attributes: SpanAttributes{"tags": []string{"original"}}}}}, func(span Span) error {
		span.SetStatus(SpanStatus{Code: StatusError, Error: &SpanError{Name: "Expected", Message: "original"}})
		return nil
	})
	traces := mustFindTraces(t, c, TraceQuery{Name: "source"}).Traces
	if len(traces) != 1 || len(traces[0].Spans[0].Links) != 1 || traces[0].Spans[0].Status.Error == nil {
		t.Fatalf("source traces = %+v, want one with its link and error", traces)
	}
	span := traces[0].Spans[0]
	span.Links[0].Attributes["tags"].([]string)[0] = "changed"
	span.Status.Error.Message = "changed"
	fresh := mustGetTrace(t, c, traces[0].TraceID).Spans[0]
	if got := fresh.Links[0].Attributes["tags"]; !reflect.DeepEqual(got, []string{"original"}) {
		t.Errorf("link attributes = %v, want [original]", got)
	}
	if want := (SpanStatus{Code: StatusError, Error: &SpanError{Name: "Expected", Message: "original"}}); !reflect.DeepEqual(fresh.Status, want) {
		t.Errorf("status = %s, want %s", formatStatus(fresh.Status), formatStatus(want))
	}
}

func formatStatus(status SpanStatus) string {
	if status.Error == nil {
		return string(status.Code)
	}
	return fmt.Sprintf("%s{%s: %s}", status.Code, status.Error.Name, status.Error.Message)
}

// A cursor FindTraces did not write is refused with a hint, where pi's
// JSON.parse throws.
func TestInMemoryFindTracesRejectsForeignCursors(t *testing.T) {
	c := NewInMemoryContext()
	for _, cursor := range []string{"not json", `{"a":1}`, `["x","y"]`, `[1]`} {
		_, err := c.FindTraces(context.Background(), TraceQuery{Cursor: cursor})
		if err == nil || !strings.Contains(err.Error(), "pass the Cursor of the previous page unchanged") {
			t.Errorf("FindTraces(cursor %q) error = %v, want a refusal that says which cursor to pass", cursor, err)
		}
	}
}

// The cursor is pi's JSON.stringify([startedMs, traceId]), byte for byte.
func TestInMemoryFindTracesCursorIsPisJSON(t *testing.T) {
	mockClock(t, &dateNow, 20_000)
	c := NewInMemoryContext()
	for range 2 {
		_ = c.StartSpan(SpanOptions{Name: "root"}, func(Span) error { return nil })
	}
	page := mustFindTraces(t, c, TraceQuery{Limit: 1})
	if want := `[20000,"` + firstTrace(t, page).TraceID + `"]`; page.Cursor != want {
		t.Errorf("cursor = %q, want %q", page.Cursor, want)
	}
}

// A negative Limit is outside pi's contract (a positive page size), where pi
// throws a TypeError or drops the last trace; it is refused with a hint,
// with or without traces to page.
func TestInMemoryFindTracesRejectsNegativeLimit(t *testing.T) {
	c := NewInMemoryContext()
	for traces := range 3 {
		if traces > 0 {
			_ = c.StartSpan(SpanOptions{Name: "root"}, func(Span) error { return nil })
		}
		if traces == 1 {
			continue
		}
		_, err := c.FindTraces(context.Background(), TraceQuery{Limit: -1})
		if err == nil || !strings.Contains(err.Error(), "pass a positive page size, or 0 for the default of 20") {
			t.Errorf("%d traces: FindTraces(Limit -1) error = %v, want a refusal naming the valid page sizes", traces, err)
		}
	}
}

// pi orders a trace's spans by start time, then by ID as JavaScript compares
// strings: with eleven spans in one millisecond, "<scope>:10" sorts before
// "<scope>:2".
func TestInMemoryGetTraceBreaksStartTiesByUTF16ID(t *testing.T) {
	mockClock(t, &dateNow, 20_000)
	c := NewInMemoryContext()
	var trace string
	_ = c.StartSpan(SpanOptions{Name: "root"}, func(root Span) error {
		trace = carrierID(c.Inject(root))
		for range 10 {
			_ = root.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
		}
		return nil
	})
	scope := strings.TrimSuffix(trace, ":1")
	var got []string
	for _, span := range mustGetTrace(t, c, trace).Spans {
		got = append(got, strings.TrimPrefix(span.ID, scope+":"))
	}
	if want := []string{"1", "10", "11", "2", "3", "4", "5", "6", "7", "8", "9"}; !reflect.DeepEqual(got, want) {
		t.Errorf("span ids = %q, want %q", got, want)
	}
}

// A measurement's interval is the ten seconds it falls in, floored: 15_999
// is in the interval starting at 10_000.
func TestInMemoryFindMetricsFloorsTheInterval(t *testing.T) {
	mockClock(t, &dateNow, 15_999)
	c := NewInMemoryContext()
	c.Counter("calls", InstrumentOptions{}).Add(1, nil)
	rows := mustFindMetrics(t, c, MetricQuery{Name: "calls"})
	if len(rows) != 1 || rows[0].IntervalMs != 10_000 {
		t.Errorf("rows = %+v, want one at interval 10000", rows)
	}
}
