package telemetry

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"sync"
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
// aggregate, and carries no timestamp.
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

// The in-memory carrier: the recorder's scope and the span's id. The keys are
// an implementation detail, not a wire format for other adapters.
const (
	memoryScopeKey = "pi-memory-scope"
	memorySpanKey  = "pi-memory-span"
)

var (
	memoryScopePattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	memorySpanPattern  = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, the largest span id
// pi's Number.isSafeInteger check accepts from a carrier.
const maxSafeInteger = 1<<53 - 1

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
	measurements []RecordedMeasurement
	// settledCount is the number of spans settled so far; the next one to
	// settle takes settledCount+1 as its end-sequence.
	settledCount int
}

// NewInMemoryContext returns an empty recorder. The zero value is equally
// usable; this exists for symmetry with the rest of the package.
func NewInMemoryContext() *InMemoryContext { return &InMemoryContext{} }

// recordedSpan is the mutable state behind a live span
// (pi MutableRecordedTelemetrySpan). id, parentID, context, parentContext and
// name never change after creation, so they are read without the lock.
type recordedSpan struct {
	id             int
	parentID       int
	context        Carrier
	parentContext  Carrier
	links          []RecordedLink
	name           string
	attributes     SpanAttributes
	events         []RecordedEvent
	status         SpanStatus
	explicitStatus bool
	settled        bool
	endSequence    int
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

// Extract returns a parent for carrier. A carrier this recorder did not
// write — nil, missing a key, or holding a malformed scope or span id —
// falls back to the recorder itself, so the work is still recorded as a root.
// Keys other than the recorder's two are dropped.
func (c *InMemoryContext) Extract(carrier Carrier) Context {
	scope, hasScope := carrier[memoryScopeKey]
	spanID, hasSpan := carrier[memorySpanKey]
	if !hasScope || !hasSpan || !memoryScopePattern.MatchString(scope) || !memorySpanPattern.MatchString(spanID) {
		return c
	}
	id, err := strconv.ParseUint(spanID, 10, 64)
	if err != nil || id > maxSafeInteger {
		return c
	}
	parent := &extractedParent{
		recorder: c,
		parent:   inMemoryParent{context: Carrier{memoryScopeKey: scope, memorySpanKey: spanID}},
	}
	c.mu.Lock()
	// Span ids are assigned densely from 1, so the id names one of this
	// recorder's spans exactly when it is in range.
	if scope == c.scopeLocked() && id <= uint64(len(c.spans)) {
		parent.parent.localID = int(id)
	}
	c.mu.Unlock()
	return parent
}

// Counter creates a counter that records non-negative finite increments.
func (c *InMemoryContext) Counter(name string, options InstrumentOptions) Counter {
	return inMemoryCounter{c.instrument(MetricCounter, name, options, nil)}
}

// UpDownCounter creates an up-down counter that records finite increments.
func (c *InMemoryContext) UpDownCounter(name string, options InstrumentOptions) UpDownCounter {
	return inMemoryCounter{c.instrument(MetricUpDownCounter, name, options, nil)}
}

// Histogram creates a histogram that records finite values. Boundaries that
// are not finite and strictly increasing make it record nothing.
func (c *InMemoryContext) Histogram(name string, options HistogramOptions) Histogram {
	return inMemoryHistogram{c.instrument(MetricHistogram, name, options.InstrumentOptions, options.Boundaries)}
}

// Measurements returns detached measurements in recording order, without
// aggregation (pi getMetrics; Metrics is the capability accessor here).
func (c *InMemoryContext) Measurements() []RecordedMeasurement {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RecordedMeasurement, 0, len(c.measurements))
	for _, m := range c.measurements {
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
			events = append(events, RecordedEvent{Name: e.Name, Attributes: copyAttributes(e.Attributes)})
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
	span := &recordedSpan{
		id:         id,
		context:    Carrier{memoryScopeKey: c.scopeLocked(), memorySpanKey: strconv.Itoa(id)},
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

	// pi settles the span on a synchronous throw and rethrows. Go's analog is
	// a panic: settle it as a failure, then let it keep unwinding. settle is
	// idempotent, so the normal path below is unaffected.
	defer func() {
		if r := recover(); r != nil {
			c.settle(span, true, panicError{value: r})
			panic(r)
		}
	}()

	err := callback(live)
	c.settle(span, err != nil, err)
	return err
}

// settle closes a span exactly once, stamping the end-sequence that orders
// settlement across spans (pi settleSpan).
func (c *InMemoryContext) settle(span *recordedSpan, failed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if span.settled {
		return
	}
	if failed && !span.explicitStatus {
		span.status = automaticErrorStatus(err)
	}
	span.settled = true
	c.settledCount++
	span.endSequence = c.settledCount
}

// measurementRecorder appends one instrument's measurements (pi
// createMeasurementRecorder). A nil recorder is an instrument whose metadata
// was invalid; it records nothing.
type measurementRecorder struct {
	recorder *InMemoryContext
	metadata RecordedMeasurement
}

func (c *InMemoryContext) instrument(kind MetricKind, name string, options InstrumentOptions, boundaries []float64) *measurementRecorder {
	if kind != MetricHistogram {
		boundaries = nil
	}
	for i, b := range boundaries {
		if math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= boundaries[i-1]) {
			return nil
		}
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
	if r == nil || math.IsNaN(value) || math.IsInf(value, 0) || (r.metadata.Kind == MetricCounter && value < 0) {
		return
	}
	m := r.metadata
	m.Boundaries = copyBoundaries(m.Boundaries)
	m.Value = value
	m.Attributes = copyAttributes(attributes)
	m.Context = copyCarrier(carrierOf(context))
	r.recorder.mu.Lock()
	r.recorder.measurements = append(r.recorder.measurements, m)
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

// Recording is passive: every mutator below is a no-op once the span settled,
// matching pi's guards.

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
	s.span.events = append(s.span.events, RecordedEvent{Name: name, Attributes: copyAttributes(attributes)})
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
