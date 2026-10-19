package telemetrytest_test

import (
	"testing"

	"github.com/sky-valley/pi/telemetry"
	"github.com/sky-valley/pi/telemetry/telemetrytest"
)

// pi's conformance.test.ts runs the suite against InMemoryTelemetryContext.
func TestInMemoryContextConformance(t *testing.T) {
	telemetrytest.Run(t, func(testing.TB) telemetrytest.Fixture {
		c := telemetry.NewInMemoryContext()
		return telemetrytest.Fixture{Context: c, Propagation: c, Metrics: c, Spans: c.Spans, Measurements: c.Measurements}
	})
}

// NoopContext records nothing, so the suite checks its behaviour only:
// capabilities, admission, errors, inert children and concurrent use.
func TestNoopContextConformance(t *testing.T) {
	telemetrytest.Run(t, func(testing.TB) telemetrytest.Fixture {
		return telemetrytest.Fixture{Context: telemetry.NoopContext}
	})
}
