package chord

import (
	"context"

	"github.com/sky-valley/pi/telemetry"
)

// Key is the typed identity of one value carried by a context.Context.
//
// A key is an identity, not a name: each NewKey mints a distinct key, and two
// keys sharing a name never alias — upstream's Symbol per createContextKey.
// The pointer is the context key, so a Key must be shared, normally as a
// package-level var. T binds the value type at both ends, which is what
// context.Value's untyped any cannot do.
type Key[T any] struct{ name string }

// NewKey mints a key. The name only labels it, in String and in the context's
// own String.
func NewKey[T any](name string) *Key[T] { return &Key[T]{name: name} }

func (k *Key[T]) String() string { return k.name }

// Value returns the value stored under k in ctx, or the zero T and false when
// none is.
func (k *Key[T]) Value(ctx context.Context) (T, bool) {
	value, ok := ctx.Value(k).(T)
	return value, ok
}

// WithValue derives a context carrying value under key, leaving parent
// unchanged. A later WithValue for the same key shadows this one.
func WithValue[T any](parent context.Context, key *Key[T], value T) context.Context {
	return context.WithValue(parent, key, value)
}

// telemetryKey carries an invocation's telemetry parent (pi's private
// TELEMETRY_CONTEXT_KEY).
var telemetryKey = NewKey[telemetry.Span]("chord.telemetry")

// Telemetry returns ctx's telemetry parent: the span WithTelemetry stored,
// or telemetry.NoopContext when there is none (pi Context.telemetry).
func Telemetry(ctx context.Context) telemetry.Span {
	if span, _ := telemetryKey.Value(ctx); span != nil {
		return span
	}
	return telemetry.NoopContext
}

// WithTelemetry derives a context whose telemetry parent is span, leaving
// cancellation and every other value unchanged (pi withTelemetry).
func WithTelemetry(ctx context.Context, span telemetry.Span) context.Context {
	return WithValue(ctx, telemetryKey, span)
}

// TraceIn runs fn inside a child span of ctx's telemetry parent, passing a
// context derived from ctx whose telemetry parent is that span (pi traceIn).
// fn's error is returned unchanged.
func TraceIn(ctx context.Context, options telemetry.SpanOptions, fn func(ctx context.Context) error) error {
	return Telemetry(ctx).StartSpan(options, func(span telemetry.Span) error {
		return fn(WithTelemetry(ctx, span))
	})
}
