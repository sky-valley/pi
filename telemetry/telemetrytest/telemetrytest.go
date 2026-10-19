// Package telemetrytest checks a telemetry adapter against pi's adapter
// conformance suite (@earendil-works/pi-telemetry/testing at d2e7d8b7c,
// upstream 48909d987 and 2ec55d808), as fstest checks a file system: an
// adapter's own tests hand it a way to build a fresh adapter, and every case
// runs against one of its own.
//
//	func TestConformance(t *testing.T) {
//		telemetrytest.Run(t, func(t testing.TB) telemetrytest.Fixture {
//			adapter := newMyAdapter()
//			t.Cleanup(adapter.Close)
//			return telemetrytest.Fixture{Context: adapter, Spans: adapter.FinishedSpans}
//		})
//	}
//
// The cases check capability identity on roots, spans, extracted parents and
// inert spans, single admission, the callback's error returned unchanged,
// automatic and explicit status, attribute merging, event order, inert
// calls after settlement, nested and concurrent parentage, and links. A
// fixture that supplies Propagation is also checked for JSON round trips,
// hops across fresh adapters and the fallback for unknown carriers; one that
// supplies Metrics, for ordered measurements, exemplars, input copying and
// the suppression of invalid measurements.
//
// pi's cases that hand the adapter "unreadable" payloads — Proxies whose
// getters throw — have no Go counterpart, since reading a map or a slice
// cannot fail; the parts of those cases that can fail in Go are kept.
// pi's two failure paths, a thrown and a rejected callback, are Go's one
// returned error.
//
// Go adds one group, "concurrency": pi is single-threaded, but Go code drives
// one adapter from many goroutines (ai records a request's span from the
// goroutine that forwards its stream while the caller works on the parent),
// and the telemetry interfaces require it. Run the suite with -race.
package telemetrytest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/sky-valley/pi/telemetry"
)

// Fixture is one fresh adapter under test (pi TelemetryAdapterFixture).
type Fixture struct {
	// Context is the adapter's root.
	Context telemetry.Context
	// Propagation, when set, is checked for propagation (pi's optional
	// `propagation`). The capabilities case falls back to
	// Context.Propagation().
	Propagation telemetry.Propagation
	// Metrics, when set, is checked for measurements, which Measurements
	// must then read (pi's optional `metrics`). The capabilities case falls
	// back to Context.Metrics().
	Metrics telemetry.Metrics
	// Spans returns the spans the adapter recorded as normalized snapshots,
	// settled or not, in start order (pi getSpans); it may flush an exporter
	// first. Leave it nil for an adapter that records nothing, such as
	// telemetry.NoopContext: the cases then check the contract's behaviour
	// and skip what they would read back.
	Spans func() []telemetry.RecordedSpan
	// Measurements returns the measurements Metrics recorded, in order, as
	// normalized snapshots (pi getMetrics).
	Measurements func() []telemetry.RecordedMeasurement
}

// NewFixture builds an isolated Fixture for one case (pi
// TelemetryAdapterFixtureFactory). It registers whatever tearing down the
// adapter needs with t.Cleanup (pi's Symbol.asyncDispose), and may fail t.
type NewFixture func(t testing.TB) Fixture

// Case is one conformance case (pi TelemetryAdapterConformanceCase).
type Case struct {
	Group string
	Name  string
	// Run builds the case's fixture and checks it.
	Run func(t *testing.T)
}

// Run runs every case of Cases(newFixture) as the subtest group/name.
func Run(t *testing.T, newFixture NewFixture) {
	t.Helper()
	for _, c := range Cases(newFixture) {
		t.Run(c.Group+"/"+c.Name, c.Run)
	}
}

// Cases returns the conformance cases for the adapter newFixture builds (pi
// createTelemetryAdapterConformance).
func Cases(newFixture NewFixture) []Case {
	cases := []struct {
		group, name string
		test        func(t *testing.T, f Fixture, newFixture NewFixture)
	}{
		{"capabilities", "carries adapter capabilities through contexts and spans", carriesCapabilities},
		{"metrics", "records ordered measurements and optional histogram exemplars", recordsOrderedMeasurements},
		{"metrics", "copies inputs and suppresses invalid measurements", copiesInputsAndSuppressesInvalidMeasurements},
		{"links", "records initial and late links without changing parents", recordsInitialAndLateLinks},
		{"links", "suppresses malformed links and ignores settled sources", suppressesMalformedLinks},
		{"propagation", "roundtrips parent identity through JSON without retaining the carrier", roundtripsParentIdentity},
		{"propagation", "unknown carriers produce recorded roots", unknownCarriersProduceRoots},
		{"propagation", "propagates across fresh adapters and multiple hops", propagatesAcrossAdapters},
		{"callback lifecycle", "admits once synchronously and preserves the result", admitsOnce},
		{"callback lifecycle", "returns the callback's error unchanged", returnsCallbackError},
		{"status", "uses last explicit status without automatic overwrite", usesLastExplicitStatus},
		{"recording", "merges attributes and records ordered events", mergesAttributes},
		{"recording", "makes calls after settlement inert", inertAfterSettlement},
		{"parentage", "records nested and concurrent child relationships", recordsParentage},
		{"concurrency", "records spans, events and measurements from many goroutines", concurrentUse},
	}
	out := make([]Case, 0, len(cases))
	for _, c := range cases {
		test := c.test
		out = append(out, Case{Group: c.group, Name: c.name, Run: func(t *testing.T) {
			test(t, newFixture(t), newFixture)
		}})
	}
	return out
}

// spans reads the fixture's recorded spans; ok is false for an adapter that
// records nothing.
func spans(f Fixture) (recorded []telemetry.RecordedSpan, ok bool) {
	if f.Spans == nil {
		return nil, false
	}
	return f.Spans(), true
}

func findSpan(t *testing.T, recorded []telemetry.RecordedSpan, name string) telemetry.RecordedSpan {
	t.Helper()
	for _, span := range recorded {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("no recorded span %q among %d: the adapter must record every span it starts", name, len(recorded))
	return telemetry.RecordedSpan{}
}

// measurements reads the fixture's measurements; a fixture with Metrics must
// supply the reader.
func measurements(t *testing.T, f Fixture) []telemetry.RecordedMeasurement {
	t.Helper()
	if f.Measurements == nil {
		t.Fatal("a Fixture with Metrics needs Measurements, the normalized measurement reader")
	}
	return f.Measurements()
}

// same is identity for capability values: == for comparable ones.
func same(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	return ta == tb && ta.Comparable() && a == b
}

// equalJSON compares recorded values as JSON, as pi's deepStrictEqual sees
// them: a slice of strings equals a []any of the same strings, and every
// number is a JavaScript number.
func equalJSON(t *testing.T, got, want any) bool {
	t.Helper()
	encode := func(v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("recorded value %#v is not JSON: %v", v, err)
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		normalized, _ := json.Marshal(decoded)
		return string(normalized)
	}
	return encode(got) == encode(want)
}

func roundtripJSON(t *testing.T, carrier telemetry.Carrier) telemetry.Carrier {
	t.Helper()
	data, err := json.Marshal(carrier)
	if err != nil {
		t.Fatal(err)
	}
	var wire telemetry.Carrier
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func noop(telemetry.Span) error { return nil }

func carriesCapabilities(t *testing.T, f Fixture, _ NewFixture) {
	metrics := f.Metrics
	if metrics == nil {
		metrics = f.Context.Metrics()
	}
	propagation := f.Propagation
	if propagation == nil {
		propagation = f.Context.Propagation()
	}
	check := func(where string, context telemetry.Context) {
		t.Helper()
		if got := context.Metrics(); !same(got, metrics) {
			t.Errorf("%s: Metrics() = %#v, want the adapter's own %#v on every context it hands out", where, got, metrics)
		}
		if got := context.Propagation(); !same(got, propagation) {
			t.Errorf("%s: Propagation() = %#v, want the adapter's own %#v on every context it hands out", where, got, propagation)
		}
	}
	check("root", f.Context)
	var settled telemetry.Span
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "capabilities"}, func(span telemetry.Span) error {
		settled = span
		check("span", span)
		_ = span.StartSpan(telemetry.SpanOptions{Name: "capabilities.child"}, func(child telemetry.Span) error {
			check("child span", child)
			return nil
		})
		if propagation != nil {
			parent := propagation.Extract(propagation.Inject(span))
			check("extracted parent", parent)
			_ = parent.StartSpan(telemetry.SpanOptions{Name: "capabilities.extracted"}, func(extracted telemetry.Span) error {
				check("span of an extracted parent", extracted)
				return nil
			})
		}
		return nil
	})
	if settled == nil {
		t.Fatal("StartSpan never ran its callback")
	}
	_ = settled.StartSpan(telemetry.SpanOptions{Name: "capabilities.inert"}, func(inert telemetry.Span) error {
		check("child of a settled span", inert)
		return nil
	})
}

func recordsOrderedMeasurements(t *testing.T, f Fixture, _ NewFixture) {
	if f.Metrics == nil {
		t.Skip("the Fixture has no Metrics")
	}
	f.Metrics.Counter("bytes", telemetry.InstrumentOptions{Description: "Encoded bytes", Unit: "By"}).Add(12, telemetry.SpanAttributes{"direction": "out"})
	active := f.Metrics.UpDownCounter("active", telemetry.InstrumentOptions{})
	active.Add(1, nil)
	active.Add(-1, nil)
	histogram := f.Metrics.Histogram("duration", telemetry.HistogramOptions{InstrumentOptions: telemetry.InstrumentOptions{Unit: "ms"}, Boundaries: []float64{1, 10}})
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "measured"}, func(span telemetry.Span) error {
		histogram.Record(5, telemetry.SpanAttributes{"method": "read"}, span)
		return nil
	})

	recorded := measurements(t, f)
	type row struct {
		Kind  telemetry.MetricKind
		Name  string
		Value float64
	}
	var got []row
	for _, m := range recorded {
		got = append(got, row{m.Kind, m.Name, m.Value})
	}
	want := []row{
		{telemetry.MetricCounter, "bytes", 12},
		{telemetry.MetricUpDownCounter, "active", 1},
		{telemetry.MetricUpDownCounter, "active", -1},
		{telemetry.MetricHistogram, "duration", 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("measurements %v, want %v, in recording order", got, want)
	}
	if !equalJSON(t, recorded[0].Attributes, telemetry.SpanAttributes{"direction": "out"}) {
		t.Errorf("counter attributes %v, want direction=out", recorded[0].Attributes)
	}
	if recorded[0].Description != "Encoded bytes" || recorded[0].Unit != "By" {
		t.Errorf("counter description and unit %q, %q, want the instrument's", recorded[0].Description, recorded[0].Unit)
	}
	if !reflect.DeepEqual(recorded[3].Boundaries, []float64{1, 10}) {
		t.Errorf("histogram boundaries %v, want [1 10]", recorded[3].Boundaries)
	}
	if spans, ok := spans(f); ok {
		if want := findSpan(t, spans, "measured").Context; !reflect.DeepEqual(recorded[3].Context, want) {
			t.Errorf("histogram exemplar %v, want the span's carrier %v", recorded[3].Context, want)
		}
	}
}

func copiesInputsAndSuppressesInvalidMeasurements(t *testing.T, f Fixture, _ NewFixture) {
	if f.Metrics == nil {
		t.Skip("the Fixture has no Metrics")
	}
	// pi grows the arrays it handed over and renames the options afterwards;
	// a Go caller can only write through a slice it handed over (options are
	// passed by value).
	boundaries := []float64{1, 10}
	histogram := f.Metrics.Histogram("snapshot", telemetry.HistogramOptions{
		InstrumentOptions: telemetry.InstrumentOptions{Description: "Original"},
		Boundaries:        boundaries,
	})
	boundaries[1] = 100
	tags := []string{"original"}
	attributes := telemetry.SpanAttributes{"tags": tags}
	histogram.Record(-1, attributes, nil)
	tags[0] = "changed"
	attributes["added"] = true
	counter := f.Metrics.Counter("invalid", telemetry.InstrumentOptions{})
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		counter.Add(value, nil)
	}
	f.Metrics.Histogram("bad-boundaries", telemetry.HistogramOptions{Boundaries: []float64{2, 1}}).Record(1, nil, nil)

	recorded := measurements(t, f)
	if len(recorded) != 1 {
		t.Fatalf("%d measurements %+v, want the one valid histogram value: a non-finite or negative counter increment and an instrument with unordered boundaries record nothing", len(recorded), recorded)
	}
	m := recorded[0]
	if !equalJSON(t, m.Attributes, telemetry.SpanAttributes{"tags": []string{"original"}}) {
		t.Errorf("attributes %v, want the copy taken when recorded", m.Attributes)
	}
	if !reflect.DeepEqual(m.Boundaries, []float64{1, 10}) {
		t.Errorf("boundaries %v, want the copy taken when the instrument was created", m.Boundaries)
	}
	if m.Description != "Original" || m.Context != nil {
		t.Errorf("description %q and context %v, want Original and none", m.Description, m.Context)
	}
}

func recordsInitialAndLateLinks(t *testing.T, f Fixture, _ NewFixture) {
	var target telemetry.Span
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "target"}, func(span telemetry.Span) error {
		target = span
		return nil
	})
	if target == nil {
		t.Fatal("StartSpan never ran its callback")
	}
	reason := []string{"initial"}
	_ = f.Context.StartSpan(telemetry.SpanOptions{
		Name:  "linked",
		Links: []telemetry.SpanLink{{Context: target, Attributes: telemetry.SpanAttributes{"reason": reason}}},
	}, func(span telemetry.Span) error {
		reason[0] = "mutated"
		span.AddLink(telemetry.SpanLink{Context: target, Attributes: telemetry.SpanAttributes{"reason": "late"}})
		span.AddLink(telemetry.SpanLink{Context: telemetry.NoopContext})
		return nil
	})
	recorded, ok := spans(f)
	if !ok {
		return
	}
	linked := findSpan(t, recorded, "linked")
	if linked.ParentID != 0 || linked.ParentContext != nil {
		t.Errorf("linked span's parent %d, %v, want a root: links do not change parents", linked.ParentID, linked.ParentContext)
	}
	targetContext := findSpan(t, recorded, "target").Context
	want := []telemetry.RecordedLink{
		{Context: targetContext, Attributes: telemetry.SpanAttributes{"reason": []string{"initial"}}},
		{Context: targetContext, Attributes: telemetry.SpanAttributes{"reason": "late"}},
	}
	if !equalJSON(t, linked.Links, want) {
		t.Errorf("links %+v, want %+v: the initial link as given at start, the late one, and none to the no-op context", linked.Links, want)
	}
}

func suppressesMalformedLinks(t *testing.T, f Fixture, _ NewFixture) {
	var source telemetry.Span
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "link-source"}, func(span telemetry.Span) error {
		source = span
		// Go's malformed link is one with no context at all.
		span.AddLink(telemetry.SpanLink{Attributes: telemetry.SpanAttributes{"partial": true}})
		return nil
	})
	if source == nil {
		t.Fatal("StartSpan never ran its callback")
	}
	source.AddLink(telemetry.SpanLink{Context: source})
	recorded, ok := spans(f)
	if !ok {
		return
	}
	if links := findSpan(t, recorded, "link-source").Links; len(links) != 0 {
		t.Errorf("links %+v, want none: a link without a context, and one added after settlement, record nothing", links)
	}
}

func roundtripsParentIdentity(t *testing.T, f Fixture, _ NewFixture) {
	if f.Propagation == nil {
		t.Skip("the Fixture has no Propagation")
	}
	p := f.Propagation
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "sender"}, func(span telemetry.Span) error {
		carrier := p.Inject(span)
		if carrier == nil {
			t.Fatal("Inject(span) = nil, want a carrier for a span the adapter started")
		}
		wire := roundtripJSON(t, carrier)
		parent := p.Extract(wire)
		for key := range wire {
			wire[key] = "mutated"
		}
		_ = parent.StartSpan(telemetry.SpanOptions{Name: "receiver"}, noop)
		if again := p.Inject(span); !reflect.DeepEqual(again, carrier) {
			t.Errorf("Inject(span) after the extracted carrier changed = %v, want %v: Extract must not retain the carrier", again, carrier)
		}
		return nil
	})
	recorded, ok := spans(f)
	if !ok {
		return
	}
	if got, want := findSpan(t, recorded, "receiver").ParentContext, findSpan(t, recorded, "sender").Context; !reflect.DeepEqual(got, want) {
		t.Errorf("receiver's parent %v, want the sender's carrier %v", got, want)
	}
}

func unknownCarriersProduceRoots(t *testing.T, f Fixture, _ NewFixture) {
	if f.Propagation == nil {
		t.Skip("the Fixture has no Propagation")
	}
	if carrier := f.Propagation.Inject(telemetry.NoopContext); carrier != nil {
		t.Errorf("Inject(NoopContext) = %v, want nil", carrier)
	}
	for i, carrier := range []telemetry.Carrier{nil, {}, {"unknown": "format"}} {
		context := f.Propagation.Extract(carrier)
		if context == nil {
			t.Fatalf("Extract(%v) = nil, want the adapter's root", carrier)
		}
		ran := false
		_ = context.StartSpan(telemetry.SpanOptions{Name: fmt.Sprintf("fallback-%d", i)}, func(telemetry.Span) error {
			ran = true
			return nil
		})
		if !ran {
			t.Errorf("Extract(%v).StartSpan did not run its callback", carrier)
		}
	}
	recorded, _ := spans(f)
	for _, span := range recorded {
		if span.ParentContext != nil {
			t.Errorf("span %q has parent %v, want a root: an unknown carrier extracts the adapter's root", span.Name, span.ParentContext)
		}
	}
}

func propagatesAcrossAdapters(t *testing.T, f Fixture, newFixture NewFixture) {
	if f.Propagation == nil {
		t.Skip("the Fixture has no Propagation")
	}
	receiver, worker := newFixture(t), newFixture(t)
	if receiver.Propagation == nil || worker.Propagation == nil {
		t.Fatal("every Fixture newFixture builds must supply Propagation when one does")
	}
	var senderCarrier, receiverCarrier telemetry.Carrier
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "sender"}, func(span telemetry.Span) error {
		senderCarrier = f.Propagation.Inject(span)
		return nil
	})
	if senderCarrier == nil {
		t.Fatal("Inject(span) = nil, want a carrier for a span the adapter started")
	}
	_ = receiver.Propagation.Extract(roundtripJSON(t, senderCarrier)).StartSpan(telemetry.SpanOptions{Name: "receiver"}, func(span telemetry.Span) error {
		receiverCarrier = receiver.Propagation.Inject(span)
		return nil
	})
	if receiverCarrier == nil {
		t.Fatal("Inject(span) = nil on the receiving adapter")
	}
	_ = worker.Propagation.Extract(roundtripJSON(t, receiverCarrier)).StartSpan(telemetry.SpanOptions{Name: "worker"}, noop)
	if recorded, ok := spans(receiver); ok {
		if got := findSpan(t, recorded, "receiver").ParentContext; !reflect.DeepEqual(got, senderCarrier) {
			t.Errorf("receiver's parent %v, want the sender's carrier %v", got, senderCarrier)
		}
	}
	if recorded, ok := spans(worker); ok {
		if got := findSpan(t, recorded, "worker").ParentContext; !reflect.DeepEqual(got, receiverCarrier) {
			t.Errorf("worker's parent %v, want the receiver's carrier %v", got, receiverCarrier)
		}
	}
}

func admitsOnce(t *testing.T, f Fixture, _ NewFixture) {
	calls := 0
	var result int
	err := f.Context.StartSpan(telemetry.SpanOptions{Name: "success"}, func(telemetry.Span) error {
		calls++
		result = 42
		return nil
	})
	if calls != 1 || err != nil || result != 42 {
		t.Fatalf("callback ran %d times, StartSpan returned %v: want it run once, before StartSpan returns, and nil back", calls, err)
	}
	recorded, ok := spans(f)
	if !ok {
		return
	}
	span := findSpan(t, recorded, "success")
	if span.Status != (telemetry.SpanStatus{Code: telemetry.StatusOK}) || !span.Settled {
		t.Errorf("span status %+v, settled %v, want ok and settled", span.Status, span.Settled)
	}
}

type conformanceError struct{ kind string }

func (e *conformanceError) Error() string { return e.kind }

func returnsCallbackError(t *testing.T, f Fixture, _ NewFixture) {
	base := errors.New("sync")
	failures := []struct {
		name string
		err  error
	}{
		{"plain-error", base},
		{"typed-error", &conformanceError{kind: "typed"}},
		{"wrapped-error", fmt.Errorf("wrapped: %w", base)},
	}
	for _, failure := range failures {
		if got := f.Context.StartSpan(telemetry.SpanOptions{Name: failure.name}, func(telemetry.Span) error { return failure.err }); got != failure.err {
			t.Errorf("%s: StartSpan returned %v, want the callback's error itself", failure.name, got)
		}
	}
	recorded, ok := spans(f)
	if !ok {
		return
	}
	for _, failure := range failures {
		if span := findSpan(t, recorded, failure.name); span.Status.Code != telemetry.StatusError {
			t.Errorf("%s: status %+v, want error: a failed callback fails its span", failure.name, span.Status)
		}
	}
}

func usesLastExplicitStatus(t *testing.T, f Fixture, _ NewFixture) {
	expected := func(message string) telemetry.SpanStatus {
		return telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "Expected", Message: message}}
	}
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "last-status"}, func(span telemetry.Span) error {
		span.SetStatus(expected("first"))
		span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
		return nil
	})
	thrown := errors.New("after explicit status")
	if got := f.Context.StartSpan(telemetry.SpanOptions{Name: "explicit-before-error"}, func(span telemetry.Span) error {
		span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
		return thrown
	}); got != thrown {
		t.Errorf("StartSpan returned %v, want the callback's error itself", got)
	}
	rejected := errors.New("after explicit error status")
	if got := f.Context.StartSpan(telemetry.SpanOptions{Name: "explicit-error-before-error"}, func(span telemetry.Span) error {
		span.SetStatus(expected("async failure"))
		return rejected
	}); got != rejected {
		t.Errorf("StartSpan returned %v, want the callback's error itself", got)
	}
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "expected-failure"}, func(span telemetry.Span) error {
		span.SetStatus(expected("returned failure"))
		return nil
	})
	recorded, ok := spans(f)
	if !ok {
		return
	}
	for name, want := range map[string]telemetry.SpanStatus{
		"last-status":                 {Code: telemetry.StatusOK},
		"explicit-before-error":       {Code: telemetry.StatusOK},
		"explicit-error-before-error": expected("async failure"),
		"expected-failure":            expected("returned failure"),
	} {
		if got := findSpan(t, recorded, name).Status; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: status %s, want %s: the last explicit status stands, whatever the callback returns", name, formatStatus(got), formatStatus(want))
		}
	}
}

func formatStatus(status telemetry.SpanStatus) string {
	if status.Error == nil {
		return string(status.Code)
	}
	return fmt.Sprintf("%s{%s: %s}", status.Code, status.Error.Name, status.Error.Message)
}

func mergesAttributes(t *testing.T, f Fixture, _ NewFixture) {
	_ = f.Context.StartSpan(telemetry.SpanOptions{
		Name:       "recording",
		Attributes: telemetry.SpanAttributes{"start": "value", "overwrite": "start", "ignored": nil},
	}, func(span telemetry.Span) error {
		span.SetAttributes(telemetry.SpanAttributes{"count": 1, "overwrite": "middle"})
		span.SetAttributes(telemetry.SpanAttributes{"count": nil, "overwrite": "end"})
		span.AddEvent("first", telemetry.SpanAttributes{"index": 1, "ignored": nil})
		span.AddEvent("second", telemetry.SpanAttributes{"index": 2})
		return nil
	})
	recorded, ok := spans(f)
	if !ok {
		return
	}
	span := findSpan(t, recorded, "recording")
	if want := (telemetry.SpanAttributes{"start": "value", "overwrite": "end", "count": 1}); !equalJSON(t, span.Attributes, want) {
		t.Errorf("attributes %v, want %v: later values win, and a nil value is no value", span.Attributes, want)
	}
	want := []telemetry.RecordedEvent{
		{Name: "first", Attributes: telemetry.SpanAttributes{"index": 1}},
		{Name: "second", Attributes: telemetry.SpanAttributes{"index": 2}},
	}
	if !equalJSON(t, span.Events, want) {
		t.Errorf("events %+v, want %+v, in order", span.Events, want)
	}
}

func inertAfterSettlement(t *testing.T, f Fixture, _ NewFixture) {
	var settled telemetry.Span
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "settled", Attributes: telemetry.SpanAttributes{"value": "initial"}}, func(span telemetry.Span) error {
		settled = span
		return nil
	})
	if settled == nil {
		t.Fatal("StartSpan never ran its callback")
	}
	settled.SetAttributes(telemetry.SpanAttributes{"value": "late"})
	settled.AddEvent("late", telemetry.SpanAttributes{"value": true})
	settled.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusError})
	admitted := false
	result := 0
	err := settled.StartSpan(telemetry.SpanOptions{Name: "late-child"}, func(telemetry.Span) error {
		admitted = true
		result = 7
		return nil
	})
	if !admitted || err != nil || result != 7 {
		t.Errorf("a child of a settled span: admitted %v, StartSpan returned %v; want it run once and nil back", admitted, err)
	}
	recorded, ok := spans(f)
	if !ok {
		return
	}
	if len(recorded) != 1 {
		t.Fatalf("%d spans recorded, want only the settled one: a child of a settled span is not recorded", len(recorded))
	}
	span := recorded[0]
	if !equalJSON(t, span.Attributes, telemetry.SpanAttributes{"value": "initial"}) || len(span.Events) != 0 || span.Status != (telemetry.SpanStatus{Code: telemetry.StatusOK}) {
		t.Errorf("settled span attributes %v, events %v, status %+v: calls after settlement must change nothing", span.Attributes, span.Events, span.Status)
	}
}

func recordsParentage(t *testing.T, f Fixture, _ NewFixture) {
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "parent"}, func(parent telemetry.Span) error {
		// The first child stays open on its own goroutine while the second
		// runs to the end (pi awaits a gate inside the first).
		admitted, release, first := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			first <- parent.StartSpan(telemetry.SpanOptions{Name: "first-child"}, func(telemetry.Span) error {
				close(admitted)
				<-release
				return nil
			})
		}()
		<-admitted
		result := ""
		_ = parent.StartSpan(telemetry.SpanOptions{Name: "second-child"}, func(telemetry.Span) error {
			result = "done"
			return nil
		})
		if result != "done" {
			t.Error("the second child did not run while the first was open")
		}
		close(release)
		return <-first
	})
	recorded, ok := spans(f)
	if !ok {
		return
	}
	parent, first, second := findSpan(t, recorded, "parent"), findSpan(t, recorded, "first-child"), findSpan(t, recorded, "second-child")
	if parent.ParentID != 0 || first.ParentID != parent.ID || second.ParentID != parent.ID {
		t.Errorf("parent ids: parent %d, first %d, second %d; want a root and two children of %d", parent.ParentID, first.ParentID, second.ParentID, parent.ID)
	}
	if !(second.EndSequence > 0 && second.EndSequence < first.EndSequence && first.EndSequence < parent.EndSequence) {
		t.Errorf("end sequences: second %d, first %d, parent %d; want them settled in that order", second.EndSequence, first.EndSequence, parent.EndSequence)
	}
}

// concurrentUse is Go's own case: every goroutine starts a root and a child,
// and several write the same span at once, as ai's forward goroutine records
// into a request span while the caller works on its parent.
func concurrentUse(t *testing.T, f Fixture, _ NewFixture) {
	const goroutines, events = 8, 16
	var histogram telemetry.Histogram
	var counter telemetry.Counter
	if f.Metrics != nil {
		histogram = f.Metrics.Histogram("concurrent.duration", telemetry.HistogramOptions{})
		counter = f.Metrics.Counter("concurrent.count", telemetry.InstrumentOptions{})
	}
	_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "shared"}, func(shared telemetry.Span) error {
		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range events {
					shared.AddEvent("tick", telemetry.SpanAttributes{"goroutine": g, "index": i})
					shared.SetAttributes(telemetry.SpanAttributes{fmt.Sprintf("goroutine.%d", g): i})
					shared.AddLink(telemetry.SpanLink{Context: shared})
					if histogram != nil {
						histogram.Record(float64(i), telemetry.SpanAttributes{"goroutine": g}, shared)
						counter.Add(1, nil)
					}
				}
				_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "root"}, func(root telemetry.Span) error {
					root.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
					return root.StartSpan(telemetry.SpanOptions{Name: "child"}, func(child telemetry.Span) error {
						child.AddEvent("child", nil)
						if p := f.Propagation; p != nil {
							_ = p.Extract(p.Inject(child)).StartSpan(telemetry.SpanOptions{Name: "extracted"}, noop)
						}
						return nil
					})
				})
			}()
		}
		wg.Wait()
		return nil
	})
	if f.Metrics != nil {
		if got := len(measurements(t, f)); got != 2*goroutines*events {
			t.Errorf("%d measurements, want %d", got, 2*goroutines*events)
		}
	}
	recorded, ok := spans(f)
	if !ok {
		return
	}
	counts := map[string]int{}
	for _, span := range recorded {
		counts[span.Name]++
	}
	want := map[string]int{"shared": 1, "root": goroutines, "child": goroutines}
	if f.Propagation != nil {
		want["extracted"] = goroutines
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("spans by name %v, want %v", counts, want)
	}
	shared := findSpan(t, recorded, "shared")
	if len(shared.Events) != goroutines*events || len(shared.Attributes) != goroutines {
		t.Errorf("shared span holds %d events and %d attributes, want %d and %d: concurrent writes were lost", len(shared.Events), len(shared.Attributes), goroutines*events, goroutines)
	}
}
