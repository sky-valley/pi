// Package telemetry is the Go port of pi's telemetry tracing contracts
// (@earendil-works/pi-telemetry, added by upstream 04d6447f7 and extracted to
// its own package by 6b461b75b): the Context and Span interfaces a host
// application implements to observe pi's logical operations, and the shared
// no-op context used when it supplies none.
//
// pi's startSpan<T> threads the callback's return value through the returned
// promise; Go interface methods cannot be generic, so StartSpan takes an
// error-returning callback instead — results travel out through closure
// captures, and pi's two failure paths (a synchronous throw and a rejected
// promise, both surfacing as the awaited rejection) fold into the one
// returned error.
//
// The schema-definition half of the upstream package is deliberately
// unported: defineTelemetrySchema and the attribute type-inference machinery
// around it (TelemetrySchemaDefinition, InferStartAttributes,
// SchemaTelemetrySpan and friends) are compile-time TypeScript typing with
// no Go counterpart.
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

// SpanOptions name a span being started and optionally seed its attributes
// (pi SpanOptions).
type SpanOptions struct {
	Name string
	// Attributes seed the span's start attributes; nil starts it bare.
	Attributes SpanAttributes
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
type Context interface {
	// StartSpan opens a span, runs callback inside it, and closes the span
	// when the callback returns. The callback's error is returned to the
	// caller unchanged.
	StartSpan(options SpanOptions, callback func(span Span) error) error
}

// Span is a live span: a Context for starting children, plus the mutators
// that record what happened inside it (pi TelemetrySpan).
type Span interface {
	Context
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

func (noopSpan) AddEvent(string, SpanAttributes) {}
func (noopSpan) SetAttributes(SpanAttributes)    {}
func (noopSpan) SetStatus(SpanStatus)            {}

// NoopContext is the shared telemetry context used when an application does
// not provide one (pi NOOP_TELEMETRY_CONTEXT). It admits every callback
// synchronously on the caller's goroutine and discards everything else.
var NoopContext Context = noopSpan{}
