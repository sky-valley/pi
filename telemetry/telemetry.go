// Package telemetry is the Go port of pi's telemetry contracts
// (@earendil-works/pi-telemetry, added by upstream 04d6447f7 and extracted to
// its own package by 6b461b75b): the Context and Span interfaces a host
// application implements to observe pi's logical operations, the optional
// Metrics and Propagation capabilities an adapter exposes on every context
// (upstream 48909d987, 2ec55d808), the shared no-op implementations used when
// it supplies none, and the in-memory reference adapter. Package telemetrytest
// holds pi's adapter conformance suite (@earendil-works/pi-telemetry/testing).
//
// pi's startSpan<T> threads the callback's return value through the returned
// promise; Go interface methods cannot be generic, so StartSpan takes an
// error-returning callback instead — results travel out through closure
// captures, and pi's two failure paths (a synchronous throw and a rejected
// promise, both surfacing as the awaited rejection) fold into the one
// returned error.
//
// Schemas are ported as plain data (SchemaDefinition and the definitions it
// holds): pi-ai's AI_TELEMETRY_SCHEMA is a runtime value whose metric
// metadata reaches the adapter through createTypedMetrics (NewTypedMetrics).
// What stays unported is the TypeScript type inference over that data —
// defineTelemetrySchema, the Infer* helpers, SchemaTelemetrySpan and the
// typing of createTypedSpanStarter and TypedTelemetryMetrics — which checks
// names and attributes at compile time and has no Go counterpart; Go callers
// name spans and attributes as plain strings, as pi's untyped API does.
package telemetry

// AttributeValue is a single telemetry attribute value. pi types it as the
// closed union string | number | boolean | readonly arrays of those; Go has
// no union types, so this is `any` under a documented contract: producers
// store string, bool, an int/float kind, or a slice of those, and an
// implementation may drop anything else.
type AttributeValue any

// SpanAttributes names the attribute values on a span or event
// (pi SpanAttributes). A nil map is pi's absent `attributes`, and a nil
// value is pi's undefined entry — implementations treat both as not present.
type SpanAttributes map[string]AttributeValue

// Carrier is an opaque, JSON-safe string map an adapter injects into and
// extracts from a transport (pi TelemetryCarrier). The adapter owns its
// format; W3C traceparent and tracestate fit without requiring them.
type Carrier map[string]string

// Propagation carries a parent's identity across a process or transport
// boundary (pi TelemetryPropagation). Both methods are synchronous, passive
// and never panic. Like every interface here, it must be safe for concurrent
// use by multiple goroutines.
type Propagation interface {
	// Inject returns a detached carrier for a span or extracted parent the
	// adapter knows, settled or not; nil for its root, a no-op context, or a
	// context it does not recognize.
	Inject(target Context) Carrier
	// Extract returns a receiver-owned parent for carrier. A nil, invalid or
	// unknown carrier falls back to the adapter's root, so incoming work is
	// still recorded.
	Extract(carrier Carrier) Context
}

// SpanLink relates a span to another context without changing its parent
// (pi SpanLink). Context is a span or an extracted parent; an adapter
// ignores a root, no-op or unrecognized target.
type SpanLink struct {
	Context    Context
	Attributes SpanAttributes
}

// SpanOptions name a span being started and optionally seed its attributes
// and links (pi SpanOptions).
type SpanOptions struct {
	Name string
	// Attributes seed the span's start attributes; nil starts it bare.
	Attributes SpanAttributes
	// Links relate the span to targets known at creation; Span.AddLink adds
	// later ones.
	Links []SpanLink
}

// StatusCode is the terminal disposition a span reports (the `status`
// discriminant of pi SpanStatus).
type StatusCode string

const (
	StatusOK    StatusCode = "ok"
	StatusError StatusCode = "error"
)

// SpanError identifies the failure behind an error status (pi's
// `{ name, message }`): Name classifies the failure the way a JS error name
// does — a Go caller typically uses the error's concrete type — and Message
// is its rendered text.
type SpanError struct {
	Name    string
	Message string
}

// SpanStatus is the terminal status a span reports. pi writes it as the
// union { status: "ok" } | { status: "error"; error? }; Go collapses that
// onto one struct: Error is meaningful only when Code is StatusError, and
// nil is pi's absent `error`.
type SpanStatus struct {
	Code  StatusCode
	Error *SpanError
}

// Context starts spans for whatever telemetry consumer the host application
// wired in (pi TelemetryContext). Every Span is itself a Context, so span
// parenting travels through values of this interface — start a child from
// the Span it belongs under — rather than through a context.Context, which
// StartSpan deliberately does not take: cancellation belongs to the work the
// callback closes over, not to the tracing of it.
//
// A host keeps its adapter, which starts root spans; request options and
// invocation contexts carry a Span. The adapter's capabilities stay
// available on every context it hands out — child spans, extracted parents,
// and the inert spans it falls back to.
//
// Implementations must be safe for concurrent use by multiple goroutines,
// and so must every Span, Metrics, instrument and Propagation they hand out:
// pi is single-threaded, Go callers are not. ai, for one, starts a request's
// span on a goroutine of its own and records into it from the goroutine that
// forwards the stream, while the caller goes on using the parent. Check an
// adapter with telemetrytest, under -race.
type Context interface {
	// StartSpan opens a span, runs callback inside it — synchronously, exactly
	// once — and closes the span when the callback returns. The callback's
	// error is returned to the caller unchanged.
	StartSpan(options SpanOptions, callback func(span Span) error) error
	// Metrics is the adapter's metric capability, or nil for an adapter that
	// only traces (pi's optional `metrics`).
	Metrics() Metrics
	// Propagation is the adapter's propagation capability, or nil when it
	// has none (pi's optional `propagation`).
	Propagation() Propagation
}

// Span is a live span: a Context for starting children, plus the mutators
// that record what happened inside it (pi TelemetrySpan). Every mutator is
// passive, never panics, and is ignored once the span settled. A Span is
// safe for concurrent use: its mutators and children may be called from
// several goroutines at once, during its callback and after it.
type Span interface {
	Context
	// AddLink relates the span to link.Context without changing its parent.
	AddLink(link SpanLink)
	// AddEvent records a point-in-time event on the span; nil attributes
	// record it bare.
	AddEvent(name string, attributes SpanAttributes)
	// SetAttributes merges attributes onto the span.
	SetAttributes(attributes SpanAttributes)
	// SetStatus sets the span's terminal status.
	SetStatus(status SpanStatus)
}

// noopSpan is the one inert span behind NoopContext (pi's frozen
// noopTelemetrySpan). It is fieldless, so it can neither inspect nor retain
// what it is handed, and every child StartSpan hands back the same shared
// span.
type noopSpan struct{}

func (s noopSpan) StartSpan(_ SpanOptions, callback func(span Span) error) error {
	return callback(s)
}

func (noopSpan) Metrics() Metrics                { return NoopMetrics }
func (noopSpan) Propagation() Propagation        { return NoopPropagation }
func (noopSpan) AddLink(SpanLink)                {}
func (noopSpan) AddEvent(string, SpanAttributes) {}
func (noopSpan) SetAttributes(SpanAttributes)    {}
func (noopSpan) SetStatus(SpanStatus)            {}

// NoopContext is the shared telemetry context used when an application does
// not provide one (pi NOOP_TELEMETRY_CONTEXT). It is a Span, so it can stand
// in wherever a parent span is expected. It admits every callback
// synchronously on the caller's goroutine, exposes NoopMetrics and
// NoopPropagation, and discards everything else.
var NoopContext Span = noopSpan{}

// noopPropagation never inspects its arguments (pi NOOP_TELEMETRY_PROPAGATION).
type noopPropagation struct{}

func (noopPropagation) Inject(Context) Carrier  { return nil }
func (noopPropagation) Extract(Carrier) Context { return NoopContext }

// NoopPropagation is the shared propagation capability that neither inspects
// nor retains its inputs (pi NOOP_TELEMETRY_PROPAGATION): Inject returns nil
// and Extract returns NoopContext.
var NoopPropagation Propagation = noopPropagation{}
