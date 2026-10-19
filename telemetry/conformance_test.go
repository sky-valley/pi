package telemetry

import (
	"encoding/json"
	"math"
	"reflect"
	"sync"
	"testing"
)

// The cases below port what upstream 48909d987 and 2ec55d808 added to pi's
// adapter conformance suite (packages/telemetry/src/testing/conformance.ts at
// d2e7d8b7c: capabilities, metrics, links, propagation), run against
// InMemoryContext as pi's conformance.test.ts does, plus that package's
// propagation.test.ts and metrics.test.ts. pi's "unreadable" Proxy inputs
// have no Go analog (see memory_test.go); everything else they guard is
// kept.

func findSpan(t *testing.T, spans []RecordedSpan, name string) RecordedSpan {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("no recorded span %q among %d", name, len(spans))
	return RecordedSpan{}
}

func roundtripJSON(t *testing.T, carrier Carrier) Carrier {
	t.Helper()
	data, err := json.Marshal(carrier)
	if err != nil {
		t.Fatal(err)
	}
	var wire Carrier
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

// "carries adapter capabilities through contexts and spans": the root, its
// spans, their children, an extracted parent and its span, and an inert
// child of a settled span all expose the recorder's own capabilities.
func TestConformanceCarriesCapabilities(t *testing.T) {
	c := NewInMemoryContext()
	assertCapabilities := func(name string, context Context) {
		t.Helper()
		if context.Metrics() != Metrics(c) {
			t.Errorf("%s: Metrics() = %#v, want the recorder", name, context.Metrics())
		}
		if context.Propagation() != Propagation(c) {
			t.Errorf("%s: Propagation() = %#v, want the recorder", name, context.Propagation())
		}
	}
	assertCapabilities("root", c)
	var settled Span
	_ = c.StartSpan(SpanOptions{Name: "capabilities"}, func(span Span) error {
		settled = span
		assertCapabilities("span", span)
		_ = span.StartSpan(SpanOptions{Name: "capabilities.child"}, func(child Span) error {
			assertCapabilities("child", child)
			return nil
		})
		parent := c.Extract(c.Inject(span))
		assertCapabilities("extracted parent", parent)
		return parent.StartSpan(SpanOptions{Name: "capabilities.extracted"}, func(extracted Span) error {
			assertCapabilities("extracted span", extracted)
			return nil
		})
	})
	_ = settled.StartSpan(SpanOptions{Name: "capabilities.inert"}, func(inert Span) error {
		assertCapabilities("inert span", inert)
		return inert.StartSpan(SpanOptions{Name: "capabilities.inert.child"}, func(child Span) error {
			assertCapabilities("inert child", child)
			return nil
		})
	})
	if got := len(c.Spans()); got != 3 {
		t.Errorf("spans = %d, want 3: inert spans are not recorded", got)
	}
}

// "records ordered measurements and optional histogram exemplars"
func TestConformanceRecordsOrderedMeasurements(t *testing.T) {
	c := NewInMemoryContext()
	c.Counter("bytes", InstrumentOptions{Description: "Encoded bytes", Unit: "By"}).Add(12, SpanAttributes{"direction": "out"})
	active := c.UpDownCounter("active", InstrumentOptions{})
	active.Add(1, nil)
	active.Add(-1, nil)
	histogram := c.Histogram("duration", HistogramOptions{InstrumentOptions: InstrumentOptions{Unit: "ms"}, Boundaries: []float64{1, 10}})
	_ = c.StartSpan(SpanOptions{Name: "measured"}, func(span Span) error {
		histogram.Record(5, SpanAttributes{"method": "read"}, span)
		return nil
	})

	measurements := c.Measurements()
	type row struct {
		kind  MetricKind
		name  string
		value float64
	}
	var got []row
	for _, m := range measurements {
		got = append(got, row{m.Kind, m.Name, m.Value})
	}
	want := []row{
		{MetricCounter, "bytes", 12},
		{MetricUpDownCounter, "active", 1},
		{MetricUpDownCounter, "active", -1},
		{MetricHistogram, "duration", 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("measurements = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(measurements[0].Attributes, SpanAttributes{"direction": "out"}) {
		t.Errorf("attributes = %v", measurements[0].Attributes)
	}
	if measurements[0].Description != "Encoded bytes" || measurements[0].Unit != "By" {
		t.Errorf("metadata = %q/%q", measurements[0].Description, measurements[0].Unit)
	}
	if !reflect.DeepEqual(measurements[3].Boundaries, []float64{1, 10}) {
		t.Errorf("boundaries = %v", measurements[3].Boundaries)
	}
	if want := findSpan(t, c.Spans(), "measured").Context; !reflect.DeepEqual(measurements[3].Context, want) {
		t.Errorf("exemplar = %v, want the span's carrier %v", measurements[3].Context, want)
	}
	if measurements[0].Context != nil || measurements[0].Boundaries != nil {
		t.Errorf("counter context/boundaries = %v/%v, want nil", measurements[0].Context, measurements[0].Boundaries)
	}
}

// "copies inputs and suppresses invalid measurements atomically"
func TestConformanceCopiesInputsAndSuppressesInvalidMeasurements(t *testing.T) {
	c := NewInMemoryContext()
	boundaries := []float64{1, 10}
	histogram := c.Histogram("snapshot", HistogramOptions{InstrumentOptions: InstrumentOptions{Description: "Original"}, Boundaries: boundaries})
	boundaries[1] = 100
	tags := []string{"original"}
	histogram.Record(-1, SpanAttributes{"tags": tags}, nil)
	tags[0] = "changed"
	counter := c.Counter("invalid", InstrumentOptions{})
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		counter.Add(value, nil)
	}
	c.Histogram("bad-boundaries", HistogramOptions{Boundaries: []float64{2, 1}}).Record(1, nil, nil)

	measurements := c.Measurements()
	if len(measurements) != 1 {
		t.Fatalf("measurements = %+v, want only the valid histogram value", measurements)
	}
	m := measurements[0]
	if !reflect.DeepEqual(m.Attributes, SpanAttributes{"tags": []string{"original"}}) {
		t.Errorf("attributes = %v, want the copy taken at record time", m.Attributes)
	}
	if !reflect.DeepEqual(m.Boundaries, []float64{1, 10}) {
		t.Errorf("boundaries = %v, want the copy taken at creation", m.Boundaries)
	}
	if m.Description != "Original" || m.Context != nil {
		t.Errorf("description/context = %q/%v", m.Description, m.Context)
	}
}

// "records initial and late links without changing parents"
func TestConformanceRecordsInitialAndLateLinks(t *testing.T) {
	c := NewInMemoryContext()
	var target Span
	_ = c.StartSpan(SpanOptions{Name: "target"}, func(span Span) error {
		target = span
		return nil
	})
	reason := []string{"initial"}
	_ = c.StartSpan(SpanOptions{Name: "linked", Links: []SpanLink{{Context: target, Attributes: SpanAttributes{"reason": reason}}}}, func(span Span) error {
		reason[0] = "mutated"
		span.AddLink(SpanLink{Context: target, Attributes: SpanAttributes{"reason": "late"}})
		span.AddLink(SpanLink{Context: NoopContext})
		return nil
	})
	spans := c.Spans()
	linked := findSpan(t, spans, "linked")
	if linked.ParentID != 0 || linked.ParentContext != nil {
		t.Errorf("parent = %d/%v, want a root", linked.ParentID, linked.ParentContext)
	}
	targetContext := findSpan(t, spans, "target").Context
	want := []RecordedLink{
		{Context: targetContext, Attributes: SpanAttributes{"reason": []string{"initial"}}},
		{Context: targetContext, Attributes: SpanAttributes{"reason": "late"}},
	}
	if !reflect.DeepEqual(linked.Links, want) {
		t.Errorf("links = %+v, want %+v", linked.Links, want)
	}
}

// "suppresses malformed links atomically and ignores settled sources": a
// settled span ignores AddLink, and a link to a context the recorder does
// not know records nothing.
func TestConformanceIgnoresSettledLinkSources(t *testing.T) {
	c := NewInMemoryContext()
	var source Span
	_ = c.StartSpan(SpanOptions{Name: "link-source"}, func(span Span) error {
		source = span
		span.AddLink(SpanLink{Context: c, Attributes: SpanAttributes{"root": true}})
		span.AddLink(SpanLink{})
		return nil
	})
	source.AddLink(SpanLink{Context: source})
	if links := findSpan(t, c.Spans(), "link-source").Links; len(links) != 0 {
		t.Errorf("links = %+v, want none", links)
	}
}

// "roundtrips parent identity through JSON without retaining the carrier"
func TestConformanceRoundtripsParentIdentity(t *testing.T) {
	c := NewInMemoryContext()
	_ = c.StartSpan(SpanOptions{Name: "sender"}, func(span Span) error {
		carrier := c.Inject(span)
		if carrier == nil {
			t.Fatal("Inject(span) = nil")
		}
		wire := roundtripJSON(t, carrier)
		parent := c.Extract(wire)
		for key := range wire {
			wire[key] = "mutated"
		}
		_ = parent.StartSpan(SpanOptions{Name: "receiver"}, func(Span) error { return nil })
		if again := c.Inject(span); !reflect.DeepEqual(again, carrier) {
			t.Errorf("Inject after mutating the wire = %v, want %v", again, carrier)
		}
		return nil
	})
	spans := c.Spans()
	receiver, sender := findSpan(t, spans, "receiver"), findSpan(t, spans, "sender")
	if !reflect.DeepEqual(receiver.ParentContext, sender.Context) {
		t.Errorf("receiver parent = %v, want sender %v", receiver.ParentContext, sender.Context)
	}
	if receiver.ParentID != sender.ID {
		t.Errorf("receiver parent id = %d, want %d: the carrier names this recorder's own span", receiver.ParentID, sender.ID)
	}
}

// "unknown and unreadable carriers produce recorded roots": nothing injects
// for the no-op context, and every carrier the recorder did not write
// extracts to its root, whose spans still run and are recorded.
func TestConformanceUnknownCarriersProduceRoots(t *testing.T) {
	c := NewInMemoryContext()
	if carrier := c.Inject(NoopContext); carrier != nil {
		t.Errorf("Inject(NoopContext) = %v, want nil", carrier)
	}
	if carrier := c.Inject(c); carrier != nil {
		t.Errorf("Inject(root) = %v, want nil", carrier)
	}
	for i, carrier := range []Carrier{nil, {}, {"unknown": "format"}} {
		result := 0
		_ = c.Extract(carrier).StartSpan(SpanOptions{Name: "fallback"}, func(Span) error {
			result = 42
			return nil
		})
		if result != 42 {
			t.Errorf("carrier %d: callback did not run", i)
		}
	}
	spans := c.Spans()
	if len(spans) != 3 {
		t.Fatalf("spans = %d, want 3 recorded roots", len(spans))
	}
	for _, span := range spans {
		if span.ParentContext != nil || span.ParentID != 0 {
			t.Errorf("span %d parent = %d/%v, want a root", span.ID, span.ParentID, span.ParentContext)
		}
	}
}

// pi's own carrier checks: a scope that is not a lowercase UUID, a span id
// with a leading zero or sign, or one past Number.MAX_SAFE_INTEGER falls
// back to the root (InMemoryTelemetryContext.extract's two patterns and
// Number.isSafeInteger).
func TestInMemoryExtractRejectsMalformedCarriers(t *testing.T) {
	c := NewInMemoryContext()
	var carrier Carrier
	_ = c.StartSpan(SpanOptions{Name: "s"}, func(span Span) error {
		carrier = c.Inject(span)
		return nil
	})
	scope := carrier[memoryScopeKey]
	for _, bad := range []Carrier{
		{memoryScopeKey: "bad", memorySpanKey: "1"},
		{memoryScopeKey: scope},
		{memorySpanKey: "1"},
		{memoryScopeKey: scope, memorySpanKey: "0"},
		{memoryScopeKey: scope, memorySpanKey: "01"},
		{memoryScopeKey: scope, memorySpanKey: "+1"},
		{memoryScopeKey: scope, memorySpanKey: "1\n"},
		{memoryScopeKey: scope, memorySpanKey: "9007199254740992"},
		{memoryScopeKey: "ABCDEF01-2345-6789-abcd-ef0123456789", memorySpanKey: "1"},
	} {
		if got := c.Extract(bad); got != Context(c) {
			t.Errorf("Extract(%v) = %#v, want the recorder itself", bad, got)
		}
	}
	if got := c.Extract(Carrier{memoryScopeKey: scope, memorySpanKey: "9007199254740991", "extra": "x"}); got == Context(c) {
		t.Error("the largest safe span id must extract a parent")
	}
}

// "propagates across fresh adapters and multiple hops"
func TestConformancePropagatesAcrossAdaptersAndHops(t *testing.T) {
	sender, receiver, worker := NewInMemoryContext(), NewInMemoryContext(), NewInMemoryContext()
	var senderCarrier, receiverCarrier Carrier
	_ = sender.StartSpan(SpanOptions{Name: "sender"}, func(span Span) error {
		senderCarrier = sender.Inject(span)
		return nil
	})
	_ = receiver.Extract(roundtripJSON(t, senderCarrier)).StartSpan(SpanOptions{Name: "receiver"}, func(span Span) error {
		receiverCarrier = receiver.Inject(span)
		return nil
	})
	_ = worker.Extract(roundtripJSON(t, receiverCarrier)).StartSpan(SpanOptions{Name: "worker"}, func(Span) error { return nil })

	if got := findSpan(t, receiver.Spans(), "receiver"); !reflect.DeepEqual(got.ParentContext, senderCarrier) || got.ParentID != 0 {
		t.Errorf("receiver parent = %d/%v, want remote %v", got.ParentID, got.ParentContext, senderCarrier)
	}
	if got := findSpan(t, worker.Spans(), "worker"); !reflect.DeepEqual(got.ParentContext, receiverCarrier) {
		t.Errorf("worker parent = %v, want %v", got.ParentContext, receiverCarrier)
	}
	if reflect.DeepEqual(findSpan(t, worker.Spans(), "worker").Context, receiverCarrier) {
		t.Error("the worker's own carrier must differ from its parent's")
	}
}

// "isolates crossed concurrent remote parents and links shared roots": two
// concurrent callers' remote parents stay apart in the provider's recorder;
// the first getter starts a shared activation root linked to it, and every
// getter links to the activation.
func TestPropagationIsolatesCrossedConcurrentRemoteParents(t *testing.T) {
	caller, provider := NewInMemoryContext(), NewInMemoryContext()
	var (
		mu         sync.Mutex
		activation Span
		shared     chan struct{} // closed once the activation span settled
		wg         sync.WaitGroup
	)
	gate := make(chan struct{})
	firstWaiting := make(chan struct{})
	run := func(name string) {
		defer wg.Done()
		_ = caller.StartSpan(SpanOptions{Name: name}, func(trigger Span) error {
			parent := provider.Extract(caller.Inject(trigger))
			return parent.StartSpan(SpanOptions{Name: name + ".method"}, func(getter Span) error {
				mu.Lock()
				if shared == nil {
					shared = make(chan struct{})
					started := make(chan struct{})
					go func(done chan struct{}) {
						_ = provider.StartSpan(SpanOptions{Name: "activation", Links: []SpanLink{{Context: getter}}}, func(span Span) error {
							activation = span
							close(started)
							<-gate
							return nil
						})
						close(done)
					}(shared)
					<-started
				}
				getter.AddLink(SpanLink{Context: activation})
				wait := shared
				mu.Unlock()
				if name == "later" {
					close(gate)
				} else {
					close(firstWaiting)
				}
				<-wait
				return nil
			})
		})
	}
	wg.Add(2)
	go run("first")
	<-firstWaiting // pi's Promise.all admits "first" before "later"
	go run("later")
	wg.Wait()

	spans := provider.Spans()
	root := findSpan(t, spans, "activation")
	if root.ParentContext != nil {
		t.Errorf("activation parent = %v, want a root", root.ParentContext)
	}
	callers := caller.Spans()
	first, later := findSpan(t, spans, "first.method"), findSpan(t, spans, "later.method")
	if !reflect.DeepEqual(first.ParentContext, findSpan(t, callers, "first").Context) {
		t.Errorf("first.method parent = %v", first.ParentContext)
	}
	if !reflect.DeepEqual(later.ParentContext, findSpan(t, callers, "later").Context) {
		t.Errorf("later.method parent = %v", later.ParentContext)
	}
	if len(root.Links) != 1 || !reflect.DeepEqual(root.Links[0].Context, first.Context) {
		t.Errorf("activation links = %+v, want first.method", root.Links)
	}
	if len(later.Links) != 1 || !reflect.DeepEqual(later.Links[0].Context, root.Context) {
		t.Errorf("later.method links = %+v, want activation", later.Links)
	}
}

// "detaches carrier and link snapshots and retains settled target identities"
func TestPropagationDetachesSnapshotsAndKeepsSettledTargets(t *testing.T) {
	c := NewInMemoryContext()
	var target Span
	_ = c.StartSpan(SpanOptions{Name: "target"}, func(span Span) error {
		target = span
		return nil
	})
	original := c.Inject(target)
	injected := c.Inject(target)
	if injected == nil {
		t.Fatal("Inject(settled span) = nil, want its carrier")
	}
	injected[memorySpanKey] = "999"
	if again := c.Inject(target); !reflect.DeepEqual(again, original) {
		t.Errorf("Inject after mutating a carrier = %v, want %v", again, original)
	}
	_ = c.StartSpan(SpanOptions{Name: "linked", Links: []SpanLink{{Context: target, Attributes: SpanAttributes{"tags": []string{"original"}}}}}, func(Span) error { return nil })
	spans := c.Spans()
	spans[0].Context[memorySpanKey] = "123"
	spans[1].Links[0].Context[memorySpanKey] = "456"
	spans[1].Links[0].Attributes["tags"] = []string{"mutated"}
	want := []RecordedLink{{Context: original, Attributes: SpanAttributes{"tags": []string{"original"}}}}
	if got := c.Spans()[1].Links; !reflect.DeepEqual(got, want) {
		t.Errorf("links = %+v, want %+v", got, want)
	}
}

// "links a new root to an extracted remote target"
func TestPropagationLinksRootToExtractedRemoteTarget(t *testing.T) {
	sender, receiver := NewInMemoryContext(), NewInMemoryContext()
	var carrier Carrier
	_ = sender.StartSpan(SpanOptions{Name: "trigger"}, func(span Span) error {
		carrier = sender.Inject(span)
		return nil
	})
	target := receiver.Extract(roundtripJSON(t, carrier))
	_ = receiver.StartSpan(SpanOptions{Name: "background", Links: []SpanLink{{Context: target, Attributes: SpanAttributes{"reason": "restart"}}}}, func(span Span) error {
		span.AddLink(SpanLink{Context: target})
		return nil
	})
	recorded := receiver.Spans()[0]
	if recorded.ParentContext != nil {
		t.Errorf("parent = %v, want a root", recorded.ParentContext)
	}
	want := []RecordedLink{
		{Context: carrier, Attributes: SpanAttributes{"reason": "restart"}},
		{Context: carrier, Attributes: SpanAttributes{}},
	}
	if !reflect.DeepEqual(recorded.Links, want) {
		t.Errorf("links = %+v, want %+v", recorded.Links, want)
	}
}

// "continues a parent in another Node process without shared recorder
// state": pi spawns a process; what it proves is that extraction needs
// nothing but the carrier, which a fresh recorder shows as well.
func TestPropagationContinuesWithoutSharedRecorderState(t *testing.T) {
	telemetry := NewInMemoryContext()
	var carrier Carrier
	_ = telemetry.StartSpan(SpanOptions{Name: "caller"}, func(span Span) error {
		carrier = telemetry.Inject(span)
		return nil
	})
	remote := NewInMemoryContext()
	_ = remote.Extract(roundtripJSON(t, carrier)).StartSpan(SpanOptions{Name: "remote"}, func(Span) error { return nil })
	recorded := remote.Spans()
	if len(recorded) != 1 {
		t.Fatalf("spans = %d, want 1", len(recorded))
	}
	if !reflect.DeepEqual(recorded[0].ParentContext, carrier) || recorded[0].ParentID != 0 {
		t.Errorf("parent = %d/%v, want remote %v", recorded[0].ParentID, recorded[0].ParentContext, carrier)
	}
	if reflect.DeepEqual(recorded[0].Context, carrier) {
		t.Error("the remote span's carrier must be its own")
	}
}

// An extracted parent naming one of the recorder's own spans records it as
// the local parent even after it settled; only a live child of a settled
// span is inert (pi passes no live span for an extracted parent).
func TestInMemoryExtractedParentOutlivesSettlement(t *testing.T) {
	c := NewInMemoryContext()
	var carrier Carrier
	_ = c.StartSpan(SpanOptions{Name: "parent"}, func(span Span) error {
		carrier = c.Inject(span)
		return nil
	})
	_ = c.Extract(carrier).StartSpan(SpanOptions{Name: "resumed"}, func(Span) error { return nil })
	resumed := findSpan(t, c.Spans(), "resumed")
	if resumed.ParentID != 1 || !reflect.DeepEqual(resumed.ParentContext, carrier) {
		t.Errorf("resumed parent = %d/%v, want 1/%v", resumed.ParentID, resumed.ParentContext, carrier)
	}
}

// pi metrics.test.ts "binds schema names, kinds, metadata, and exact
// measurement attributes", runtime half: each factory hands the adapter its
// definition's metadata, and a histogram's exemplar is the span passed.
func TestTypedMetricsBindDefinitionMetadata(t *testing.T) {
	traffic := SchemaDefinition{Version: 1, Metrics: map[string]MetricDefinition{
		"network.bytes": {
			Kind: MetricCounter, Description: "Encoded bytes", Unit: "By",
			Attributes: map[string]AttributeDefinition{
				"direction": {Type: AttributeString, Values: []AttributeValue{"in", "out"}, Required: true, Description: "Direction", Cardinality: CardinalityLow},
			},
		},
		"network.active": {Kind: MetricUpDownCounter, Description: "Open links"},
	}}
	latency := SchemaDefinition{Version: 1, Metrics: map[string]MetricDefinition{
		"call.duration": {
			Kind: MetricHistogram, Description: "Call duration", Unit: "ms", Boundaries: []float64{1, 10, 100},
			Attributes: map[string]AttributeDefinition{"method": {Type: AttributeString, Description: "Method"}},
		},
	}}
	adapter := NewInMemoryContext()
	metrics := NewTypedMetrics(adapter, traffic, latency)
	metrics.Counter("network.bytes").Add(128, SpanAttributes{"direction": "out"})
	metrics.UpDownCounter("network.active").Add(1, nil)
	metrics.UpDownCounter("network.active").Add(-1, nil)
	_ = adapter.StartSpan(SpanOptions{Name: "request"}, func(span Span) error {
		metrics.Histogram("call.duration").Record(12, SpanAttributes{"method": "read"}, span)
		metrics.Histogram("call.duration").Record(13, nil, nil)
		metrics.Histogram("call.duration").Record(14, nil, span)
		return nil
	})
	measurements := adapter.Measurements()
	if len(measurements) != 6 {
		t.Fatalf("measurements = %d, want 6", len(measurements))
	}
	first := measurements[0]
	if first.Kind != MetricCounter || first.Name != "network.bytes" || first.Description != "Encoded bytes" ||
		first.Unit != "By" || first.Value != 128 || !reflect.DeepEqual(first.Attributes, SpanAttributes{"direction": "out"}) {
		t.Errorf("counter measurement = %+v", first)
	}
	if measurements[1].Description != "Open links" || measurements[1].Unit != "" {
		t.Errorf("up-down metadata = %q/%q", measurements[1].Description, measurements[1].Unit)
	}
	spanContext := adapter.Spans()[0].Context
	if m := measurements[3]; m.Kind != MetricHistogram || !reflect.DeepEqual(m.Boundaries, []float64{1, 10, 100}) || !reflect.DeepEqual(m.Context, spanContext) {
		t.Errorf("histogram measurement = %+v", m)
	}
	if measurements[4].Context != nil {
		t.Errorf("context = %v, want nil without an exemplar", measurements[4].Context)
	}
	if !reflect.DeepEqual(measurements[5].Context, spanContext) {
		t.Errorf("context = %v, want %v", measurements[5].Context, spanContext)
	}
	metrics.Counter("unknown").Add(1, nil)
	if m := adapter.Measurements()[6]; m.Description != "" || m.Unit != "" {
		t.Errorf("an undefined name got metadata %q/%q", m.Description, m.Unit)
	}
}

// "returns detached measurement snapshots including exemplars and boundaries"
func TestInMemoryMeasurementsAreDetached(t *testing.T) {
	c := NewInMemoryContext()
	histogram := c.Histogram("duration", HistogramOptions{Boundaries: []float64{1, 2}})
	_ = c.StartSpan(SpanOptions{Name: "request"}, func(span Span) error {
		histogram.Record(1, SpanAttributes{"tags": []string{"original"}}, span)
		return nil
	})
	measurements := c.Measurements()
	if len(measurements) != 1 {
		t.Fatalf("measurements = %+v, want one", measurements)
	}
	m := measurements[0]
	m.Boundaries[0] = 3
	m.Attributes["tags"] = []string{"mutated"}
	m.Context[memorySpanKey] = "999"
	again := c.Measurements()[0]
	if !reflect.DeepEqual(again.Boundaries, []float64{1, 2}) ||
		!reflect.DeepEqual(again.Attributes, SpanAttributes{"tags": []string{"original"}}) ||
		!reflect.DeepEqual(again.Context, c.Spans()[0].Context) {
		t.Errorf("snapshot = %+v, want the recorded values", again)
	}
}

// "ignores malformed metadata, nonfinite values, and unreadable attributes",
// Go half: invalid boundaries make an instrument inert, non-finite values
// and negative counter increments are dropped, and an exemplar the recorder
// does not know records no context.
func TestInMemoryIgnoresInvalidMeasurements(t *testing.T) {
	c := NewInMemoryContext()
	for _, boundaries := range [][]float64{{1, 1}, {2, 1}, {math.NaN()}, {math.Inf(1)}} {
		c.Histogram("bad", HistogramOptions{Boundaries: boundaries}).Record(1, nil, nil)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c.Counter("bad", InstrumentOptions{}).Add(value, nil)
		c.UpDownCounter("bad", InstrumentOptions{}).Add(value, nil)
		c.Histogram("bad", HistogramOptions{}).Record(value, nil, nil)
	}
	c.Counter("bad", InstrumentOptions{}).Add(-1, nil)
	if got := c.Measurements(); len(got) != 0 {
		t.Fatalf("measurements = %+v, want none", got)
	}
	c.Histogram("valid", HistogramOptions{}).Record(-1, SpanAttributes{}, NoopContext)
	if got := c.Measurements(); len(got) != 1 || got[0].Context != nil || got[0].Value != -1 {
		t.Errorf("measurements = %+v, want one with no exemplar", got)
	}
}

// conformance.test.ts "returns detached snapshots without exposing mutable
// recording state": an open span reads unsettled with no end sequence.
func TestInMemoryOpenSpanSnapshot(t *testing.T) {
	c := NewInMemoryContext()
	var open RecordedSpan
	_ = c.StartSpan(SpanOptions{Name: "snapshot", Attributes: SpanAttributes{"tags": []string{"initial"}}}, func(span Span) error {
		span.AddEvent("event", SpanAttributes{"value": 1})
		open = c.Spans()[0]
		return nil
	})
	if open.Settled || open.EndSequence != 0 {
		t.Errorf("open snapshot settled/endSequence = %v/%d, want false/0", open.Settled, open.EndSequence)
	}
	first := c.Spans()[0]
	if !first.Settled || first.EndSequence != 1 {
		t.Errorf("settled/endSequence = %v/%d, want true/1", first.Settled, first.EndSequence)
	}
	first.Attributes["tags"] = []string{"mutated"}
	first.Events[0].Attributes["value"] = 2
	second := c.Spans()[0]
	if !reflect.DeepEqual(second.Attributes, SpanAttributes{"tags": []string{"initial"}}) ||
		!reflect.DeepEqual(second.Events, []RecordedEvent{{Name: "event", Attributes: SpanAttributes{"value": 1}}}) {
		t.Errorf("snapshot = %+v, want the recorded values", second)
	}
}

// Every span carries a carrier naming its recorder's scope — a lowercase
// v4 UUID, as crypto.randomUUID() returns — and its own id, and its
// children carry it as their parent context.
func TestInMemorySpanCarriers(t *testing.T) {
	var c InMemoryContext
	_ = c.StartSpan(SpanOptions{Name: "root"}, func(root Span) error {
		return root.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
	})
	spans := c.Spans()
	scope := spans[0].Context[memoryScopeKey]
	if !memoryScopePattern.MatchString(scope) || scope[14] != '4' || !containsRune("89ab", rune(scope[19])) {
		t.Errorf("scope = %q, want a lowercase v4 UUID", scope)
	}
	if want := (Carrier{memoryScopeKey: scope, memorySpanKey: "1"}); !reflect.DeepEqual(spans[0].Context, want) || spans[0].ParentContext != nil {
		t.Errorf("root context/parent = %v/%v, want %v/nil", spans[0].Context, spans[0].ParentContext, want)
	}
	if want := (Carrier{memoryScopeKey: scope, memorySpanKey: "2"}); !reflect.DeepEqual(spans[1].Context, want) || !reflect.DeepEqual(spans[1].ParentContext, spans[0].Context) {
		t.Errorf("child context/parent = %v/%v", spans[1].Context, spans[1].ParentContext)
	}
	if other := NewInMemoryContext(); other.Extract(spans[0].Context) == Context(other) {
		t.Error("another recorder must extract a well-formed foreign carrier as a remote parent")
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
