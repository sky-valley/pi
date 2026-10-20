package telemetry

// InstrumentOptions describe an instrument to the backend (pi
// InstrumentOptions). An empty field is pi's absent one.
type InstrumentOptions struct {
	Description string
	Unit        string
}

// HistogramOptions add bucket hints to a histogram's description (pi
// HistogramOptions). Callers supply finite, strictly increasing Boundaries;
// nil is pi's absent `boundaries`. Backends own aggregation and may
// translate them.
type HistogramOptions struct {
	InstrumentOptions
	Boundaries []float64
}

// Counter accumulates non-negative finite increments (pi TelemetryCounter).
// Like every instrument, it is safe for concurrent use.
type Counter interface {
	Add(value float64, attributes SpanAttributes)
}

// UpDownCounter accumulates finite signed increments (pi
// TelemetryUpDownCounter). It is safe for concurrent use.
type UpDownCounter interface {
	Add(value float64, attributes SpanAttributes)
}

// Histogram records finite signed values (pi TelemetryHistogram). A non-nil
// context correlates the measurement with that span, an exemplar. It is safe
// for concurrent use.
type Histogram interface {
	Record(value float64, attributes SpanAttributes, context Context)
}

// Metrics is an adapter's optional metric capability, exposed on every
// context it hands out through Context.Metrics (pi TelemetryMetrics).
// Factories and recording methods are synchronous and trust typed inputs:
// callers supply finite measurements and non-negative counter increments,
// and what an adapter makes of malformed ones is unspecified. Record only
// values business code already has, and only low-cardinality attributes.
// Metrics and the instruments it creates are safe for concurrent use by
// multiple goroutines.
type Metrics interface {
	Counter(name string, options InstrumentOptions) Counter
	UpDownCounter(name string, options InstrumentOptions) UpDownCounter
	Histogram(name string, options HistogramOptions) Histogram
}

// noopMetrics hands out shared instruments that discard their measurements
// (pi NOOP_TELEMETRY_METRICS).
type noopMetrics struct{}

// noopInstrument is the one inert instrument behind every NoopMetrics
// factory (pi's frozen noopCounter, noopUpDownCounter and noopHistogram,
// which differ only in name).
type noopInstrument struct{}

func (noopInstrument) Add(float64, SpanAttributes)             {}
func (noopInstrument) Record(float64, SpanAttributes, Context) {}

func (noopMetrics) Counter(string, InstrumentOptions) Counter { return noopInstrument{} }
func (noopMetrics) UpDownCounter(string, InstrumentOptions) UpDownCounter {
	return noopInstrument{}
}
func (noopMetrics) Histogram(string, HistogramOptions) Histogram { return noopInstrument{} }

// NoopMetrics is the shared metric capability whose shared instruments
// discard every measurement (pi NOOP_TELEMETRY_METRICS).
var NoopMetrics Metrics = noopMetrics{}

// TypedMetrics binds instrument names to the metric definitions of one or
// more schemas (pi createTypedMetrics). pi's typing also restricts each name
// to its kind and checks measurement attributes at compile time; Go has no
// such inference, so what is ported is the runtime half: each factory hands
// the adapter the definition's description, unit and, for a histogram,
// boundaries. A name no schema defines gets no options.
type TypedMetrics struct {
	metrics     Metrics
	definitions map[string]MetricDefinition
}

// NewTypedMetrics binds metrics to schemas' metric definitions; a later
// schema's definition of a name replaces an earlier one's (pi rejects the
// duplicate at compile time).
func NewTypedMetrics(metrics Metrics, schemas ...SchemaDefinition) TypedMetrics {
	definitions := map[string]MetricDefinition{}
	for _, schema := range schemas {
		for name, definition := range schema.Metrics {
			definitions[name] = definition
		}
	}
	return TypedMetrics{metrics: metrics, definitions: definitions}
}

func (m TypedMetrics) instrumentOptions(name string) InstrumentOptions {
	definition := m.definitions[name]
	return InstrumentOptions{Description: definition.Description, Unit: definition.Unit}
}

// Counter creates the counter the schemas define as name.
func (m TypedMetrics) Counter(name string) Counter {
	return m.metrics.Counter(name, m.instrumentOptions(name))
}

// UpDownCounter creates the up-down counter the schemas define as name.
func (m TypedMetrics) UpDownCounter(name string) UpDownCounter {
	return m.metrics.UpDownCounter(name, m.instrumentOptions(name))
}

// Histogram creates the histogram the schemas define as name.
func (m TypedMetrics) Histogram(name string) Histogram {
	return m.metrics.Histogram(name, HistogramOptions{
		InstrumentOptions: m.instrumentOptions(name),
		Boundaries:        m.definitions[name].Boundaries,
	})
}
