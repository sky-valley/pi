package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/sky-valley/pi/internal/jstext"
)

// RecordedEvent is one point-in-time event captured on a recorded span
// (pi RecordedTelemetryEvent).
type RecordedEvent struct {
	Name       string
	Attributes SpanAttributes
}

// RecordedLink is one link captured on a recorded span (pi
// RecordedTelemetryLink): the target's carrier and the link's attributes.
type RecordedLink struct {
	Context    Carrier
	Attributes SpanAttributes
}

// RecordedSpan is a detached snapshot of one span an InMemoryContext observed
// (pi RecordedTelemetrySpan).
type RecordedSpan struct {
	// ID is assigned in span-start order, from 1.
	ID int
	// ParentID is the enclosing span's ID when this recorder owns it, or 0 for
	// a root or a parent owned by another recorder. pi models this as
	// `number | null`; IDs start at 1, so 0 is unambiguous here.
	ParentID int
	// Context is the span's own carrier, what Inject returns for it.
	Context Carrier
	// ParentContext is the parent's carrier, or nil for a root. A remote
	// parent has a ParentContext but no ParentID.
	ParentContext Carrier
	Links         []RecordedLink
	Name          string
	// Attributes are the start attributes merged with every SetAttributes
	// call the span accepted.
	Attributes SpanAttributes
	Events     []RecordedEvent
	Status     SpanStatus
	Settled    bool
	// EndSequence orders settlement across spans, from 1, and is 0 while the
	// span is still open. pi omits the key entirely in that case.
	EndSequence int
}

// RecordedMeasurement is one measurement an InMemoryContext's instruments
// recorded (pi RecordedTelemetryMeasurement). It is a log entry, not an
// aggregate; the recorder keeps its time for FindMetrics, not here.
type RecordedMeasurement struct {
	Kind MetricKind
	Name string
	// Description and Unit are the instrument's; empty is pi's absent key.
	Description string
	Unit        string
	// Boundaries are a histogram's bucket hints; nil when it has none.
	Boundaries []float64
	Value      float64
	Attributes SpanAttributes
	// Context is the exemplar span's carrier, or nil (pi's null).
	Context Carrier
}

// The in-memory carrier: the recorder's scope, the span's id, and the trace
// it belongs to. The keys are an implementation detail, not a wire format for
// other adapters.
const (
	memoryScopeKey = "pi-memory-scope"
	memorySpanKey  = "pi-memory-span"
	memoryTraceKey = "pi-memory-trace"
)

// carrierID is a carrier's globally unique span ID, "<scope>:<span>" (pi
// carrierId): a StoredSpan's ID, and a root's trace ID.
func carrierID(carrier Carrier) string {
	return carrier[memoryScopeKey] + ":" + carrier[memorySpanKey]
}

// processStart is the origin of performanceNow, as pi's performance.now()
// counts from the process's time origin.
var processStart = time.Now()

// dateNow and performanceNow are pi's Date.now() — wall-clock epoch
// milliseconds, which a span's start and a measurement's interval read — and
// performance.now() — monotonic milliseconds, which durations and event
// offsets read. Tests replace them, as pi's mock the globals.
var (
	dateNow        = func() float64 { return float64(time.Now().UnixMilli()) }
	performanceNow = func() float64 { return float64(time.Since(processStart)) / float64(time.Millisecond) }
)

// intervalLength is the width of the metric intervals FindMetrics reports a
// measurement in, ten seconds.
const intervalLength = 10_000

// InMemoryContext is the backend-neutral reference adapter that records spans
// and measurements in process memory (pi InMemoryTelemetryContext). It is
// the root Context, its own Metrics and its own Propagation; every span and
// extracted parent it hands out carries those capabilities. Create a fresh
// one to isolate tests or independent recording scopes.
//
// Its carriers name a random per-recorder scope and a span id, so they
// survive a JSON round trip and work across processes with no shared
// registry. Inject recognizes the spans and extracted parents of every
// InMemoryContext, as pi's module-wide registry does, so a link or exemplar
// may name another recorder's span.
//
// It is a Store too: FindTraces, GetTrace and FindMetrics read back what it
// recorded, with the wall-clock start and monotonic duration of every span,
// each measurement a count-one row in its ten-second interval.
//
// It is safe for concurrent use, as the Context contract requires of every
// adapter: the recorder takes a mutex around its state. The lock is never
// held across a callback — a callback starting a child span would otherwise
// deadlock.
//
// The usable zero value is deliberate: span ids and end-sequences are derived
// from state the recorder already keeps rather than from counters a constructor
// has to seed, and the scope is drawn on first use. A `var c InMemoryContext`
// that recorded id 0 spans would break the 0-means-root and 0-means-open
// invariants below without erroring.
type InMemoryContext struct {
	mu           sync.Mutex
	scope        string
	spans        []*recordedSpan
	measurements []timedMeasurement
	// settledCount is the number of spans settled so far; the next one to
	// settle takes settledCount+1 as its end-sequence.
	settledCount int
}

// NewInMemoryContext returns an empty recorder. The zero value is equally
// usable; this exists for symmetry with the rest of the package.
func NewInMemoryContext() *InMemoryContext { return &InMemoryContext{} }

// timedMeasurement is a measurement and the start of the interval it was
// recorded in (pi's { intervalMs, measurement } log entry).
type timedMeasurement struct {
	intervalMs  float64
	measurement RecordedMeasurement
}

// recordedSpan is the mutable state behind a live span
// (pi MutableRecordedTelemetrySpan). id, parentID, context, parentContext,
// name, startedMs and clock never change after creation, so they are read
// without the lock.
type recordedSpan struct {
	// startedMs is the wall-clock start (pi Date.now()); clock the monotonic
	// one (pi performance.now()) that durations and event offsets count from.
	startedMs      float64
	clock          float64
	endedMs        float64
	id             int
	parentID       int
	context        Carrier
	parentContext  Carrier
	links          []RecordedLink
	name           string
	attributes     SpanAttributes
	events         []timedEvent
	status         SpanStatus
	explicitStatus bool
	settled        bool
	endSequence    int
}

// timedEvent is a recorded event and its offset from the span's start
// (pi MutableRecordedTelemetryEvent).
type timedEvent struct {
	timeMs float64
	event  RecordedEvent
}

// inMemoryParent is where a new span belongs (pi InMemoryParent): the
// parent's carrier, its local id (0 when another recorder owns it), and its
// live span when it has one — an extracted parent has none.
type inMemoryParent struct {
	context Carrier
	localID int
	span    *recordedSpan
}

// StartSpan opens a root span on this recorder.
func (c *InMemoryContext) StartSpan(options SpanOptions, callback func(span Span) error) error {
	return c.startSpan(nil, options, callback)
}

// Metrics returns the recorder itself.
func (c *InMemoryContext) Metrics() Metrics { return c }

// Propagation returns the recorder itself.
func (c *InMemoryContext) Propagation() Propagation { return c }

// Inject returns a detached carrier for a span or extracted parent of any
// InMemoryContext, settled spans included; nil for anything else.
func (c *InMemoryContext) Inject(target Context) Carrier {
	carrier := carrierOf(target)
	if carrier == nil {
		return nil
	}
	return copyCarrier(carrier)
}

// Extract returns a parent for carrier. A carrier missing any of the
// recorder's three keys falls back to the recorder itself, so the work is
// still recorded as a root; their values are opaque and not checked. Keys
// other than the three are dropped.
func (c *InMemoryContext) Extract(carrier Carrier) Context {
	scope, hasScope := carrier[memoryScopeKey]
	spanID, hasSpan := carrier[memorySpanKey]
	traceID, hasTrace := carrier[memoryTraceKey]
	if !hasScope || !hasSpan || !hasTrace {
		return c
	}
	parent := &extractedParent{
		recorder: c,
		parent:   inMemoryParent{context: Carrier{memoryScopeKey: scope, memorySpanKey: spanID, memoryTraceKey: traceID}},
	}
	c.mu.Lock()
	// pi takes Number(span) in its own scope without looking the span up; a
	// number that is no Go int (pi's NaN, a fraction) names no local parent.
	if scope == c.scopeLocked() {
		if n := jstext.StringToNumber(spanID); n == math.Trunc(n) && math.Abs(n) <= 1<<53 {
			parent.parent.localID = int(n)
		}
	}
	c.mu.Unlock()
	return parent
}

// Counter creates a counter. Increments are recorded as given: callers
// supply finite, non-negative ones.
func (c *InMemoryContext) Counter(name string, options InstrumentOptions) Counter {
	return inMemoryCounter{c.instrument(MetricCounter, name, options, nil)}
}

// UpDownCounter creates an up-down counter. Increments are recorded as given.
func (c *InMemoryContext) UpDownCounter(name string, options InstrumentOptions) UpDownCounter {
	return inMemoryCounter{c.instrument(MetricUpDownCounter, name, options, nil)}
}

// Histogram creates a histogram. Values and boundaries are recorded as
// given: callers supply finite values and strictly increasing boundaries.
func (c *InMemoryContext) Histogram(name string, options HistogramOptions) Histogram {
	return inMemoryHistogram{c.instrument(MetricHistogram, name, options.InstrumentOptions, options.Boundaries)}
}

// Measurements returns detached measurements in recording order, without
// aggregation or their times (pi getMetrics; Metrics is the capability
// accessor here).
func (c *InMemoryContext) Measurements() []RecordedMeasurement {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RecordedMeasurement, 0, len(c.measurements))
	for _, timed := range c.measurements {
		m := timed.measurement
		m.Boundaries = copyBoundaries(m.Boundaries)
		m.Attributes = copyAttributes(m.Attributes)
		m.Context = copyCarrier(m.Context)
		out = append(out, m)
	}
	return out
}

// Spans returns detached snapshots in span-start order. Mutating the result
// cannot reach the recorder's state.
func (c *InMemoryContext) Spans() []RecordedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RecordedSpan, 0, len(c.spans))
	for _, s := range c.spans {
		links := make([]RecordedLink, 0, len(s.links))
		for _, l := range s.links {
			links = append(links, RecordedLink{Context: copyCarrier(l.Context), Attributes: copyAttributes(l.Attributes)})
		}
		events := make([]RecordedEvent, 0, len(s.events))
		for _, e := range s.events {
			events = append(events, RecordedEvent{Name: e.event.Name, Attributes: copyAttributes(e.event.Attributes)})
		}
		out = append(out, RecordedSpan{
			ID:            s.id,
			ParentID:      s.parentID,
			Context:       copyCarrier(s.context),
			ParentContext: copyCarrier(s.parentContext),
			Links:         links,
			Name:          s.name,
			Attributes:    copyAttributes(s.attributes),
			Events:        events,
			Status:        copyStatus(s.status),
			Settled:       s.settled,
			EndSequence:   s.endSequence,
		})
	}
	return out
}

// scopeLocked returns the recorder's scope, drawing it on first use (pi
// crypto.randomUUID() at construction).
func (c *InMemoryContext) scopeLocked() string {
	if c.scope == "" {
		c.scope = randomUUID()
	}
	return c.scope
}

func (c *InMemoryContext) startSpan(parent *inMemoryParent, options SpanOptions, callback func(span Span) error) error {
	// Links are resolved before the lock: a link's carrier never changes once
	// its span exists.
	links := make([]RecordedLink, 0, len(options.Links))
	for _, link := range options.Links {
		if recorded, ok := copyLink(link); ok {
			links = append(links, recorded)
		}
	}

	// pi: a child started under an already-settled parent is not recorded; it
	// runs on an inert span that keeps the recorder's capabilities. The check
	// and the append share one critical section -- splitting them lets the
	// parent settle in between and a child still be recorded under it.
	c.mu.Lock()
	if parent != nil && parent.span != nil && parent.span.settled {
		c.mu.Unlock()
		return inertSpan{c}.StartSpan(options, callback)
	}
	id := len(c.spans) + 1
	scope := c.scopeLocked()
	// A root starts a trace named by its own global ID; every descendant,
	// local or across a carrier, inherits it.
	trace := scope + ":" + strconv.Itoa(id)
	if parent != nil {
		if inherited, ok := parent.context[memoryTraceKey]; ok {
			trace = inherited
		}
	}
	span := &recordedSpan{
		startedMs:  dateNow(),
		clock:      performanceNow(),
		id:         id,
		context:    Carrier{memoryScopeKey: scope, memorySpanKey: strconv.Itoa(id), memoryTraceKey: trace},
		links:      links,
		name:       options.Name,
		attributes: copyAttributes(options.Attributes),
		status:     SpanStatus{Code: StatusOK},
	}
	if parent != nil {
		span.parentID = parent.localID
		span.parentContext = copyCarrier(parent.context)
	}
	c.spans = append(c.spans, span)
	c.mu.Unlock()

	live := &inMemorySpan{recorder: c, span: span}

	// pi's try/catch/finally: a throw sets the automatic error status and
	// rethrows, and the span settles however the callback leaves. Go's throw
	// is a panic, which keeps unwinding once the span settled; a
	// runtime.Goexit settles it with the status it had.
	returned := false
	defer func() {
		if returned {
			return
		}
		r := recover()
		if r != nil {
			c.settle(span, panicError{value: r})
			panic(r)
		}
		c.settle(span, nil)
	}()
	err := callback(live)
	returned = true
	c.settle(span, err)
	return err
}

// settle closes a span, stamping its end time and the end-sequence that
// orders settlement across spans (pi's finally block). A non-nil err fails a
// span whose status was not set explicitly (pi's catch block).
func (c *InMemoryContext) settle(span *recordedSpan, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && !span.explicitStatus {
		span.status = automaticErrorStatus(err)
	}
	span.settled = true
	span.endedMs = span.startedMs + performanceNow() - span.clock
	c.settledCount++
	span.endSequence = c.settledCount
}

// measurementRecorder appends one instrument's measurements (pi
// createMeasurementRecorder).
type measurementRecorder struct {
	recorder *InMemoryContext
	metadata RecordedMeasurement
}

func (c *InMemoryContext) instrument(kind MetricKind, name string, options InstrumentOptions, boundaries []float64) *measurementRecorder {
	if kind != MetricHistogram {
		boundaries = nil
	}
	return &measurementRecorder{recorder: c, metadata: RecordedMeasurement{
		Kind:        kind,
		Name:        name,
		Description: options.Description,
		Unit:        options.Unit,
		Boundaries:  copyBoundaries(boundaries),
	}}
}

func (r *measurementRecorder) record(value float64, attributes SpanAttributes, context Context) {
	m := r.metadata
	m.Boundaries = copyBoundaries(m.Boundaries)
	m.Value = value
	m.Attributes = copyAttributes(attributes)
	m.Context = copyCarrier(carrierOf(context))
	interval := math.Floor(dateNow()/intervalLength) * intervalLength
	r.recorder.mu.Lock()
	r.recorder.measurements = append(r.recorder.measurements, timedMeasurement{intervalMs: interval, measurement: m})
	r.recorder.mu.Unlock()
}

type inMemoryCounter struct{ recorder *measurementRecorder }

func (i inMemoryCounter) Add(value float64, attributes SpanAttributes) {
	i.recorder.record(value, attributes, nil)
}

type inMemoryHistogram struct{ recorder *measurementRecorder }

func (i inMemoryHistogram) Record(value float64, attributes SpanAttributes, context Context) {
	i.recorder.record(value, attributes, context)
}

// panicError carries a recovered panic into the status derivation so a panic
// is distinguishable from an ordinary returned error, which %T alone cannot do
// (both collapse to *errors.errorString for most stdlib errors).
type panicError struct{ value any }

func (e panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// automaticErrorStatus derives the status of a span that failed without one
// being set explicitly. pi reads a JS Error's name and message; Go's nearest
// equivalent of `error.name` is the error's concrete type, which carries real
// signal only for typed errors.
func automaticErrorStatus(err error) SpanStatus {
	// Unreachable today -- both callers pass a non-nil error -- but kept so a
	// future caller cannot turn a nil into a nil-deref inside err.Error().
	if err == nil {
		return SpanStatus{Code: StatusError}
	}
	var pe panicError
	if errors.As(err, &pe) {
		return SpanStatus{
			Code:  StatusError,
			Error: &SpanError{Name: "panic", Message: fmt.Sprint(pe.value)},
		}
	}
	return SpanStatus{
		Code:  StatusError,
		Error: &SpanError{Name: fmt.Sprintf("%T", err), Message: err.Error()},
	}
}

// inMemorySpan is the live Span handed to a callback.
type inMemorySpan struct {
	recorder *InMemoryContext
	span     *recordedSpan
}

func (s *inMemorySpan) StartSpan(options SpanOptions, callback func(span Span) error) error {
	return s.recorder.startSpan(&inMemoryParent{context: s.span.context, localID: s.span.id, span: s.span}, options, callback)
}

func (s *inMemorySpan) Metrics() Metrics         { return s.recorder }
func (s *inMemorySpan) Propagation() Propagation { return s.recorder }

// Every mutator below is a no-op once the span settled, matching pi's
// guards.

func (s *inMemorySpan) AddLink(link SpanLink) {
	recorded, ok := copyLink(link)
	if !ok {
		return
	}
	s.recorder.mu.Lock()
	defer s.recorder.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.links = append(s.span.links, recorded)
}

func (s *inMemorySpan) AddEvent(name string, attributes SpanAttributes) {
	s.recorder.mu.Lock()
	defer s.recorder.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.events = append(s.span.events, timedEvent{
		timeMs: performanceNow() - s.span.clock,
		event:  RecordedEvent{Name: name, Attributes: copyAttributes(attributes)},
	})
}

func (s *inMemorySpan) SetAttributes(attributes SpanAttributes) {
	s.recorder.mu.Lock()
	defer s.recorder.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.attributes = mergeAttributes(s.span.attributes, attributes)
}

func (s *inMemorySpan) SetStatus(status SpanStatus) {
	s.recorder.mu.Lock()
	defer s.recorder.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.status = copyStatus(status)
	s.span.explicitStatus = true
}

// extractedParent is a parent Extract returned (pi's extracted `parent`
// context): children record its carrier as their parent context, and its
// local id when it names one of the recorder's own spans, settled or not.
type extractedParent struct {
	recorder *InMemoryContext
	parent   inMemoryParent
}

func (p *extractedParent) StartSpan(options SpanOptions, callback func(span Span) error) error {
	return p.recorder.startSpan(&p.parent, options, callback)
}

func (p *extractedParent) Metrics() Metrics         { return p.recorder }
func (p *extractedParent) Propagation() Propagation { return p.recorder }

// inertSpan is what a child of a settled span runs on (pi startInertSpan):
// nothing is recorded, its own children are inert too, and it keeps the
// recorder's capabilities.
type inertSpan struct{ recorder *InMemoryContext }

func (s inertSpan) StartSpan(_ SpanOptions, callback func(span Span) error) error {
	return callback(s)
}

func (s inertSpan) Metrics() Metrics              { return s.recorder }
func (s inertSpan) Propagation() Propagation      { return s.recorder }
func (inertSpan) AddLink(SpanLink)                {}
func (inertSpan) AddEvent(string, SpanAttributes) {}
func (inertSpan) SetAttributes(SpanAttributes)    {}
func (inertSpan) SetStatus(SpanStatus)            {}

// carrierOf returns the carrier of a span or extracted parent of any
// InMemoryContext (pi's module-wide contextCarriers registry), or nil. The
// carrier is shared, never to be mutated.
func carrierOf(context Context) Carrier {
	switch c := context.(type) {
	case *inMemorySpan:
		return c.span.context
	case *extractedParent:
		return c.parent.context
	}
	return nil
}

// copyLink records link, or reports false when its target is not a context
// an InMemoryContext knows (pi copyLink).
func copyLink(link SpanLink) (RecordedLink, bool) {
	carrier := carrierOf(link.Context)
	if carrier == nil {
		return RecordedLink{}, false
	}
	return RecordedLink{Context: copyCarrier(carrier), Attributes: copyAttributes(link.Attributes)}, true
}

func copyCarrier(carrier Carrier) Carrier {
	if carrier == nil {
		return nil
	}
	copied := make(Carrier, len(carrier))
	for k, v := range carrier {
		copied[k] = v
	}
	return copied
}

func copyBoundaries(boundaries []float64) []float64 {
	if boundaries == nil {
		return nil
	}
	return append([]float64{}, boundaries...)
}

// randomUUID returns a random version 4 UUID in pi's crypto.randomUUID form.
func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// copyAttributes detaches a caller's attribute map. Per the SpanAttributes
// contract a nil value is pi's `undefined` entry, so it is dropped rather
// than recorded.
func copyAttributes(attributes SpanAttributes) SpanAttributes {
	copied := SpanAttributes{}
	for name, value := range attributes {
		if value == nil {
			continue
		}
		copied[name] = copyAttributeValue(value)
	}
	return copied
}

func mergeAttributes(current, attributes SpanAttributes) SpanAttributes {
	merged := copyAttributes(current)
	for name, value := range attributes {
		if value == nil {
			continue
		}
		merged[name] = copyAttributeValue(value)
	}
	return merged
}

// copyAttributeValue clones slice values so a caller mutating the slice it
// passed cannot reach into recorded state (pi's `[...value]`). Scalars are
// already values in Go and pass through.
func copyAttributeValue(value AttributeValue) AttributeValue {
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice || rv.IsNil() {
		return value
	}
	clone := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
	reflect.Copy(clone, rv)
	return clone.Interface()
}

// storedSpansLocked returns detached StoredSpan snapshots of the settled
// spans, by start time with span ID breaking ties (pi storedSpans).
func (c *InMemoryContext) storedSpansLocked() []StoredSpan {
	out := []StoredSpan{}
	for _, s := range c.spans {
		if !s.settled {
			continue
		}
		events := make([]StoredEvent, 0, len(s.events))
		for _, e := range s.events {
			events = append(events, StoredEvent{Name: e.event.Name, TimeMs: e.timeMs, Attributes: copyAttributes(e.event.Attributes)})
		}
		links := make([]StoredLink, 0, len(s.links))
		for _, l := range s.links {
			links = append(links, StoredLink{TraceID: l.Context[memoryTraceKey], SpanID: carrierID(l.Context), Attributes: copyAttributes(l.Attributes)})
		}
		stored := StoredSpan{
			ID:         carrierID(s.context),
			TraceID:    s.context[memoryTraceKey],
			Name:       s.name,
			StartedMs:  s.startedMs,
			EndedMs:    s.endedMs,
			Attributes: copyAttributes(s.attributes),
			Events:     events,
			Links:      links,
			Status:     copyStatus(s.status),
		}
		if s.parentContext != nil {
			stored.ParentID = carrierID(s.parentContext)
		}
		out = append(out, stored)
	}
	slices.SortStableFunc(out, func(a, b StoredSpan) int {
		if a.StartedMs != b.StartedMs {
			return compareNumbers(a.StartedMs, b.StartedMs)
		}
		return jstext.CompareUTF16(a.ID, b.ID)
	})
	return out
}

// compareNumbers orders a before b as pi's `a - b` comparator does.
func compareNumbers(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// GetTrace returns the settled spans of traceID, or nil when the recorder
// holds none.
func (c *InMemoryContext) GetTrace(_ context.Context, traceID string) (*StoredTrace, error) {
	c.mu.Lock()
	all := c.storedSpansLocked()
	c.mu.Unlock()
	var spans []StoredSpan
	for _, span := range all {
		if span.TraceID == traceID {
			spans = append(spans, span)
		}
	}
	if len(spans) == 0 {
		return nil, nil
	}
	return &StoredTrace{TraceID: traceID, Spans: spans}, nil
}

// FindTraces returns a page of the traces of settled spans that query
// matches. Its cursor is pi's JSON [first span start, trace ID]; a cursor it
// did not write is refused.
func (c *InMemoryContext) FindTraces(_ context.Context, query TraceQuery) (TracePage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = 20
	}
	var after *traceCursor
	if query.Cursor != "" {
		cursor, err := parseTraceCursor(query.Cursor)
		if err != nil {
			return TracePage{}, err
		}
		after = &cursor
	}
	c.mu.Lock()
	all := c.storedSpansLocked()
	c.mu.Unlock()

	var grouped []*StoredTrace
	byID := map[string]*StoredTrace{}
	for _, span := range all {
		trace := byID[span.TraceID]
		if trace == nil {
			trace = &StoredTrace{TraceID: span.TraceID}
			byID[span.TraceID] = trace
			grouped = append(grouped, trace)
		}
		trace.Spans = append(trace.Spans, span)
	}
	matches := []StoredTrace{}
	for _, trace := range grouped {
		if traceMatches(*trace, query, after) {
			matches = append(matches, *trace)
		}
	}
	slices.SortStableFunc(matches, func(a, b StoredTrace) int {
		if a.Spans[0].StartedMs != b.Spans[0].StartedMs {
			return compareNumbers(b.Spans[0].StartedMs, a.Spans[0].StartedMs)
		}
		return jstext.CompareUTF16(a.TraceID, b.TraceID)
	})
	page := TracePage{Traces: matches[:min(limit, len(matches))]}
	if len(matches) > limit {
		last := page.Traces[len(page.Traces)-1]
		cursor, err := jstext.Stringify([]any{last.Spans[0].StartedMs, last.TraceID})
		if err != nil {
			return TracePage{}, fmt.Errorf("telemetry: encode the next page's cursor: %w", err)
		}
		page.Cursor = cursor
	}
	return page, nil
}

// traceCursor is where a page of FindTraces ended: its last trace's first
// span start and trace ID.
type traceCursor struct {
	startedMs float64
	traceID   string
}

func parseTraceCursor(cursor string) (traceCursor, error) {
	var fields []any
	if err := json.Unmarshal([]byte(cursor), &fields); err == nil && len(fields) == 2 {
		startedMs, isNumber := fields[0].(float64)
		traceID, isString := fields[1].(string)
		if isNumber && isString {
			return traceCursor{startedMs: startedMs, traceID: traceID}, nil
		}
	}
	return traceCursor{}, fmt.Errorf("telemetry: trace cursor %q is not one FindTraces returned; pass the Cursor of the previous page unchanged, with the same filters, or none for the first page", cursor)
}

// traceMatches applies pi findTraces' filter to a trace whose spans are in
// stored order.
func traceMatches(trace StoredTrace, query TraceQuery, after *traceCursor) bool {
	first := trace.Spans[0]
	if query.Name != "" && first.Name != query.Name {
		return false
	}
	if query.StartMin != nil && !(first.StartedMs >= *query.StartMin) {
		return false
	}
	if query.StartMax != nil && !(first.StartedMs <= *query.StartMax) {
		return false
	}
	if after != nil && !(first.StartedMs < after.startedMs || (first.StartedMs == after.startedMs && jstext.CompareUTF16(trace.TraceID, after.traceID) > 0)) {
		return false
	}
	if query.Errors && !slices.ContainsFunc(trace.Spans, func(span StoredSpan) bool { return span.Status.Code == StatusError }) {
		return false
	}
	if len(query.Attributes) > 0 && !slices.ContainsFunc(trace.Spans, func(span StoredSpan) bool { return attributesMatch(span.Attributes, query.Attributes) }) {
		return false
	}
	return true
}

// attributesMatch reports whether every queried attribute strictly equals
// (pi's ===) the one in attributes.
func attributesMatch(attributes, query SpanAttributes) bool {
	for key, value := range query {
		if !strictEqual(attributes[key], value) {
			return false
		}
	}
	return true
}

// strictEqual is JavaScript's === between attribute values: numbers compare
// as numbers whatever their Go kind, NaN equals nothing, a nil is pi's
// undefined, and an array equals nothing (no two are the same object).
func strictEqual(a, b AttributeValue) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if x, ok := attributeNumber(a); ok {
		y, ok := attributeNumber(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}

// attributeNumber returns an attribute value of any Go number kind as the
// JavaScript number it stands for.
func attributeNumber(value AttributeValue) (float64, bool) {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	}
	return 0, false
}

// FindMetrics returns every recorded measurement query matches as a
// count-one row in its ten-second interval, in recording order, without
// aggregating them.
func (c *InMemoryContext) FindMetrics(_ context.Context, query MetricQuery) ([]StoredMetric, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := []StoredMetric{}
	for _, timed := range c.measurements {
		m := timed.measurement
		if m.Name != query.Name ||
			(query.StartMin != nil && !(timed.intervalMs >= *query.StartMin)) ||
			(query.StartMax != nil && !(timed.intervalMs <= *query.StartMax)) ||
			!attributesMatch(m.Attributes, query.Attributes) {
			continue
		}
		rows = append(rows, StoredMetric{
			IntervalMs: timed.intervalMs,
			Name:       m.Name,
			Kind:       m.Kind,
			Unit:       m.Unit,
			Attributes: copyAttributes(m.Attributes),
			Count:      1,
			Sum:        m.Value,
			Min:        m.Value,
			Max:        m.Value,
		})
	}
	return rows, nil
}

func copyStatus(status SpanStatus) SpanStatus {
	if status.Code != StatusError {
		return SpanStatus{Code: StatusOK}
	}
	if status.Error == nil {
		return SpanStatus{Code: StatusError}
	}
	return SpanStatus{
		Code:  StatusError,
		Error: &SpanError{Name: status.Error.Name, Message: status.Error.Message},
	}
}
