package telemetry

// The schema types below are pi's serializable schema definitions
// (TelemetrySchemaDefinition and the definitions it holds, upstream
// 04d6447f7 and 48909d987) as plain data. pi derives exact TypeScript types
// from a schema; Go keeps only the data — see the package doc.

// AttributeType is the declared type of an attribute (pi
// TelemetryAttributeType).
type AttributeType string

const (
	AttributeString       AttributeType = "string"
	AttributeNumber       AttributeType = "number"
	AttributeBoolean      AttributeType = "boolean"
	AttributeStringArray  AttributeType = "string[]"
	AttributeNumberArray  AttributeType = "number[]"
	AttributeBooleanArray AttributeType = "boolean[]"
)

// Cardinality annotates how many distinct values an attribute takes (pi's
// `cardinality`); empty is pi's absent annotation. Metric attributes must not
// be CardinalityHigh.
type Cardinality string

const (
	CardinalityLow  Cardinality = "low"
	CardinalityHigh Cardinality = "high"
)

// AttributeDefinition declares one attribute (pi TelemetryAttributeDefinition,
// and with Required its start, event and metric forms).
type AttributeDefinition struct {
	Type        AttributeType
	Description string
	Sensitive   bool
	Cardinality Cardinality
	// Values is the closed set of a scalar attribute; nil leaves it open.
	Values []AttributeValue
	// ElementValues is the closed set of an array attribute's elements.
	ElementValues []AttributeValue
	Examples      []AttributeValue
	// Required applies to start, event and metric attributes, which pi
	// requires to say true or false; end attributes are always optional.
	Required bool
}

// EventDefinition declares one span event (pi TelemetryEventDefinition).
type EventDefinition struct {
	Description string
	Attributes  map[string]AttributeDefinition
}

// ParentKind is where a span may start (pi TelemetryParentDefinition's
// discriminant).
type ParentKind string

const (
	ParentAny            ParentKind = "any"
	ParentRootOrExternal ParentKind = "root_or_external"
	ParentSpans          ParentKind = "spans"
)

// ParentDefinition declares a span's allowed parents (pi
// TelemetryParentDefinition); Spans applies to ParentSpans. Nothing enforces
// it at runtime.
type ParentDefinition struct {
	Kind  ParentKind
	Spans []string
}

// StatusDefinition documents a span's status (pi's `status`): its default,
// and when it is an error.
type StatusDefinition struct {
	Default   StatusCode
	ErrorWhen string
}

// SpanDefinition declares one span (pi TelemetrySpanDefinition).
// StartAttributes are known when the span starts; EndAttributes enrich it
// before it settles. Both land on the same span.
type SpanDefinition struct {
	Description     string
	Parents         ParentDefinition
	StartAttributes map[string]AttributeDefinition
	EndAttributes   map[string]AttributeDefinition
	Events          map[string]EventDefinition
	Status          StatusDefinition
}

// MetricKind is an instrument kind (pi TelemetryMetricDefinition's `kind`,
// and RecordedTelemetryMeasurement's).
type MetricKind string

const (
	MetricCounter       MetricKind = "counter"
	MetricUpDownCounter MetricKind = "upDownCounter"
	MetricHistogram     MetricKind = "histogram"
)

// MetricDefinition declares one instrument (pi TelemetryMetricDefinition).
// Only a histogram declares Boundaries.
type MetricDefinition struct {
	Kind        MetricKind
	Description string
	Unit        string
	Boundaries  []float64
	Attributes  map[string]AttributeDefinition
}

// SchemaDefinition is one package's telemetry vocabulary (pi
// TelemetrySchemaDefinition): the spans it starts and the instruments it
// records. pi's defineTelemetrySchema is a typed identity function, so a
// schema here is simply a value.
type SchemaDefinition struct {
	Version int
	Spans   map[string]SpanDefinition
	Metrics map[string]MetricDefinition
}
