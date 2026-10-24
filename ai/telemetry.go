package ai

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sky-valley/pi/telemetry"
)

// Request telemetry, ported from pi packages/ai/src/telemetry.ts (upstream
// 2ec55d808): Models.Stream, StreamSimple and StreamDeferred observe the
// stream they forward (lazy.ts's LazyStreamObserver) and report one span per
// logical request or poll under ProviderRequestOptions.Telemetry, with
// events for the first chunk and each completed block — never their
// content — and four low-cardinality metrics. Providers are unchanged.

// Attribute definitions TelemetrySchema shares between its spans and metrics.
var (
	telemetryProviderAttribute = telemetry.AttributeDefinition{Type: telemetry.AttributeString, Description: "Provider identifier.", Required: true}
	telemetryModelAttribute    = telemetry.AttributeDefinition{Type: telemetry.AttributeString, Description: "Requested model identifier.", Required: true}
	telemetryStopAttribute     = telemetry.AttributeDefinition{
		Type:        telemetry.AttributeString,
		Description: "Normalized stop reason.",
		Values:      []telemetry.AttributeValue{"pending", "stop", "length", "toolUse", "error", "aborted", "deferred"},
	}
)

func telemetryRequestSpan(description string) telemetry.SpanDefinition {
	return telemetry.SpanDefinition{
		Description: description,
		Parents:     telemetry.ParentDefinition{Kind: telemetry.ParentAny},
		StartAttributes: map[string]telemetry.AttributeDefinition{
			"provider":  telemetryProviderAttribute,
			"api":       {Type: telemetry.AttributeString, Description: "API identifier.", Required: true},
			"model":     telemetryModelAttribute,
			"reasoning": {Type: telemetry.AttributeString, Description: "Scalar requested reasoning level."},
		},
		EndAttributes: map[string]telemetry.AttributeDefinition{
			"stop":              telemetryStopAttribute,
			"usage.input":       {Type: telemetry.AttributeNumber, Description: "Uncached input tokens."},
			"usage.output":      {Type: telemetry.AttributeNumber, Description: "Output tokens."},
			"usage.cache_read":  {Type: telemetry.AttributeNumber, Description: "Cache-read tokens."},
			"usage.cache_write": {Type: telemetry.AttributeNumber, Description: "Cache-write tokens."},
			"cost.total":        {Type: telemetry.AttributeNumber, Description: "Total request cost in USD."},
			"response.id":       {Type: telemetry.AttributeString, Description: "Provider response identifier.", Cardinality: telemetry.CardinalityHigh},
		},
		Events: map[string]telemetry.EventDefinition{
			"pi.ai.first_chunk": {Description: "First normalized stream event delivered.", Attributes: map[string]telemetry.AttributeDefinition{}},
			"pi.ai.block": {
				Description: "A completed content block; content is never recorded.",
				Attributes: map[string]telemetry.AttributeDefinition{
					"type": {
						Type:        telemetry.AttributeString,
						Description: "Block kind.",
						Values:      []telemetry.AttributeValue{"text", "thinking", "tool_call"},
						Required:    true,
					},
					"index": {Type: telemetry.AttributeNumber, Description: "Content block index.", Required: true},
					"chars": {Type: telemetry.AttributeNumber, Description: "UTF-16 character count.", Required: true},
					"tool":  {Type: telemetry.AttributeString, Description: "Tool name, for tool-call blocks only."},
				},
			},
		},
		Status: telemetry.StatusDefinition{
			Default:   telemetry.StatusOK,
			ErrorWhen: "Stop reason is error: provider_error with at most 500 error-message characters. Aborted stays ok.",
		},
	}
}

// TelemetrySchema is pi-ai's telemetry vocabulary (pi AI_TELEMETRY_SCHEMA):
// the pi.ai.request and pi.ai.poll spans and the request metrics. Models
// hands each metric's description and unit to the adapter when it creates
// the instrument.
var TelemetrySchema = telemetry.SchemaDefinition{
	Version: 1,
	Spans: map[string]telemetry.SpanDefinition{
		"pi.ai.request": telemetryRequestSpan("One logical model request, including authentication and provider setup."),
		"pi.ai.poll":    telemetryRequestSpan("One deferred-response poll, not the deferred request's lifetime."),
	},
	Metrics: map[string]telemetry.MetricDefinition{
		"pi.ai.request.duration": {
			Kind:        telemetry.MetricHistogram,
			Description: "Logical request or poll duration.",
			Unit:        "ms",
			Attributes: map[string]telemetry.AttributeDefinition{
				"provider": telemetryProviderAttribute,
				"model":    telemetryModelAttribute,
				"stop":     withRequired(telemetryStopAttribute),
			},
		},
		"pi.ai.first_chunk.duration": {
			Kind:        telemetry.MetricHistogram,
			Description: "Time to the first normalized stream event.",
			Unit:        "ms",
			Attributes: map[string]telemetry.AttributeDefinition{
				"provider": telemetryProviderAttribute,
				"model":    telemetryModelAttribute,
			},
		},
		"pi.ai.tokens": {
			Kind:        telemetry.MetricCounter,
			Description: "Reported model token usage; pending and deferred polls are excluded.",
			Unit:        "token",
			Attributes: map[string]telemetry.AttributeDefinition{
				"provider": telemetryProviderAttribute,
				"model":    telemetryModelAttribute,
				"kind": {
					Type:        telemetry.AttributeString,
					Description: "Token usage category.",
					Values:      []telemetry.AttributeValue{"input", "output", "cache_read", "cache_write"},
					Required:    true,
				},
			},
		},
		"pi.ai.cost": {
			Kind:        telemetry.MetricCounter,
			Description: "Reported model cost; pending and deferred polls are excluded.",
			Unit:        "USD",
			Attributes: map[string]telemetry.AttributeDefinition{
				"provider": telemetryProviderAttribute,
				"model":    telemetryModelAttribute,
			},
		},
	},
}

func withRequired(definition telemetry.AttributeDefinition) telemetry.AttributeDefinition {
	definition.Required = true
	return definition
}

// requestInstruments are the four instruments one metrics capability
// records request measurements on (pi createInstruments).
type requestInstruments struct {
	requestDuration    telemetry.Histogram
	firstChunkDuration telemetry.Histogram
	tokens             telemetry.Counter
	cost               telemetry.Counter
}

func createRequestInstruments(adapter telemetry.Metrics) *requestInstruments {
	metrics := telemetry.NewTypedMetrics(adapter, TelemetrySchema)
	return &requestInstruments{
		requestDuration:    metrics.Histogram("pi.ai.request.duration"),
		firstChunkDuration: metrics.Histogram("pi.ai.first_chunk.duration"),
		tokens:             metrics.Counter("pi.ai.tokens"),
		cost:               metrics.Counter("pi.ai.cost"),
	}
}

// instrumentCache keeps the instruments of the metrics capability that
// recorded last, so a host's one adapter creates them once (pi caches them
// per capability in a WeakMap). Go has no ephemeron map — a map holding the
// instruments would hold the adapter they record into for good — so the
// cache keeps one slot; a program alternating between adapters re-creates
// them on each switch. Two caveats follow from the slot: it keeps the last
// adapter alive, and with it whatever that adapter recorded, until another
// one takes the slot; and it compares adapters with ==, so equal values of a
// comparable value type share one entry where pi's WeakMap, keyed by object
// identity, would keep one per object. An adapter that is not comparable is
// never cached. The no-op capability's instruments are shared and never take
// the slot.
var instrumentCache struct {
	mu          sync.Mutex
	metrics     telemetry.Metrics
	instruments *requestInstruments
}

// noopInstruments record nothing, for every request whose parent has no
// metric capability.
var noopInstruments = sync.OnceValue(func() *requestInstruments {
	return createRequestInstruments(telemetry.NoopMetrics)
})

func instrumentsFor(adapter telemetry.Metrics) *requestInstruments {
	if adapter == telemetry.NoopMetrics {
		return noopInstruments()
	}
	if !reflect.ValueOf(adapter).Comparable() {
		return createRequestInstruments(adapter)
	}
	instrumentCache.mu.Lock()
	defer instrumentCache.mu.Unlock()
	if instrumentCache.instruments == nil || instrumentCache.metrics != adapter {
		instrumentCache.metrics = adapter
		instrumentCache.instruments = createRequestInstruments(adapter)
	}
	return instrumentCache.instruments
}

// requestObserver is the streamObserver of one Models request (pi
// observeModelRequest). Its span is open from the call until the result is
// observed. onEvent and onResult run one at a time, on the goroutine that
// forwards the stream or fails its setup.
type requestObserver struct {
	span telemetry.Span
	// spanless is why span is nil: the parent's StartSpan returned without
	// running its callback.
	spanless    error
	finish      func()
	now         func() time.Time
	started     time.Time
	poll        bool
	provider    string
	model       string
	instruments *requestInstruments
	first       bool
	toolChars   map[int]int
}

// observeModelRequest starts the pi.ai.request span — pi.ai.poll for a
// deferred poll — under parent. A request without telemetry (a nil parent,
// or NoopContext) is not observed: pi observes it into the no-op span and
// instruments, which discard everything, so skipping it changes nothing but
// the cost.
func observeModelRequest(model *Model, parent telemetry.Span, reasoning ThinkingLevel, poll bool) streamObserver {
	if parent == nil || parent == telemetry.NoopContext {
		return nil
	}
	return startRequestObservation(model, parent, reasoning, poll, time.Now)
}

func startRequestObservation(model *Model, parent telemetry.Span, reasoning ThinkingLevel, poll bool, now func() time.Time) *requestObserver {
	o := &requestObserver{
		now:       now,
		started:   now(),
		poll:      poll,
		provider:  string(model.Provider),
		model:     model.ID,
		first:     true,
		toolChars: map[int]int{},
	}
	adapter := parent.Metrics()
	if adapter == nil {
		adapter = telemetry.NoopMetrics
	}
	o.instruments = instrumentsFor(adapter)

	options := telemetry.SpanOptions{
		Name: "pi.ai.request",
		Attributes: telemetry.SpanAttributes{
			"provider": o.provider,
			"model":    o.model,
			"api":      string(model.Api),
		},
	}
	if poll {
		options.Name = "pi.ai.poll"
	}
	if reasoning != "" {
		options.Attributes["reasoning"] = string(reasoning)
	}

	// pi's span stays open until the promise its callback returned settles.
	// StartSpan closes the span when the callback returns, so the callback
	// hands its span over and blocks until the result is observed. An adapter
	// admits the callback synchronously, so the span is started — and, for a
	// recording adapter, recorded — before the request runs.
	span := make(chan telemetry.Span, 1)
	finished := make(chan struct{})
	returned := make(chan struct{})
	var startErr error
	go func() {
		defer close(returned)
		// The callback never fails, so an error here is the adapter's own; pi
		// drops it too (`void start(...)`). It is reported only when the
		// callback never ran.
		startErr = parent.StartSpan(options, func(s telemetry.Span) error {
			span <- s
			<-finished
			return nil
		})
	}()
	select {
	case o.span = <-span:
	case <-returned:
		// A non-conforming adapter returned without running the callback, or
		// runs it later on a goroutine of its own. pi's request then has no
		// span, and its first use throws (see onEvent).
		select {
		case o.span = <-span:
		default:
			o.spanless = spanlessError(options.Name, startErr)
		}
	}
	o.finish = sync.OnceFunc(func() { close(finished) })
	return o
}

// spanlessError is the request's failure when its span never started. pi's
// is the TypeError of reading the undefined span.
func spanlessError(name string, startErr error) error {
	cause := ""
	if startErr != nil {
		cause = " (" + startErr.Error() + ")"
	}
	return fmt.Errorf("telemetry: %s has no span: the parent's StartSpan returned without running its callback%s; a telemetry.Context runs the callback exactly once, before StartSpan returns (check the adapter with telemetrytest.Run)", name, cause)
}

// sinceStart is the time since the request started in fractional
// milliseconds (pi's performance.now() difference).
func (o *requestObserver) sinceStart() float64 {
	return float64(o.now().Sub(o.started)) / float64(time.Millisecond)
}

func (o *requestObserver) attributes() telemetry.SpanAttributes {
	return telemetry.SpanAttributes{"provider": o.provider, "model": o.model}
}

// onEvent sees each event before the stream delivers it. Without a span it
// fails where pi's observer throws: the first event and each completed block
// read the span.
func (o *requestObserver) onEvent(event AssistantMessageEvent) error {
	if o.span == nil {
		switch {
		case o.first, event.Type == EventTextEnd, event.Type == EventThinkingEnd, event.Type == EventToolCallEnd:
			o.first = false
			return o.spanless
		}
		return nil
	}
	if o.first {
		o.first = false
		o.span.AddEvent("pi.ai.first_chunk", nil)
		o.instruments.firstChunkDuration.Record(o.sinceStart(), o.attributes(), o.span)
	}
	switch event.Type {
	case EventToolCallDelta:
		o.toolChars[event.ContentIndex] += jsStringLength(event.Delta)
	case EventTextEnd, EventThinkingEnd:
		blockType := "text"
		if event.Type == EventThinkingEnd {
			blockType = "thinking"
		}
		o.span.AddEvent("pi.ai.block", telemetry.SpanAttributes{
			"type":  blockType,
			"index": event.ContentIndex,
			"chars": jsStringLength(event.Content),
		})
	case EventToolCallEnd:
		chars := o.toolChars[event.ContentIndex]
		delete(o.toolChars, event.ContentIndex)
		attributes := telemetry.SpanAttributes{"type": "tool_call", "index": event.ContentIndex, "chars": chars}
		if event.ToolCall != nil {
			attributes["tool"] = event.ToolCall.Name
		}
		o.span.AddEvent("pi.ai.block", attributes)
	}
	return nil
}

// onResult records the result, nil when the stream ended without one, and
// closes the span. Without a span there is nothing to record: pi's observer
// throws there, after the stream ended, where no consumer sees it.
func (o *requestObserver) onResult(message *AssistantMessage) {
	if message != nil && o.span != nil {
		end := telemetry.SpanAttributes{
			"stop":              string(message.StopReason),
			"usage.input":       message.Usage.Input,
			"usage.output":      message.Usage.Output,
			"usage.cache_read":  message.Usage.CacheRead,
			"usage.cache_write": message.Usage.CacheWrite,
			"cost.total":        message.Usage.Cost.Total,
		}
		if message.ResponseID != "" {
			end["response.id"] = message.ResponseID
		}
		o.span.SetAttributes(end)
		if message.StopReason == StopError {
			// The excerpt owns its bytes (pi's ownedSlice, upstream f715a15c1):
			// a substring would keep a megabyte error message alive as long as
			// the span holds its first 500 characters.
			o.span.SetStatus(telemetry.SpanStatus{
				Code:  telemetry.StatusError,
				Error: &telemetry.SpanError{Name: "provider_error", Message: strings.Clone(truncateUTF16Units(message.ErrorMessage, 500))},
			})
		}
		duration := o.attributes()
		duration["stop"] = string(message.StopReason)
		o.instruments.requestDuration.Record(o.sinceStart(), duration, o.span)
		if !o.poll || (message.StopReason != StopPending && message.StopReason != StopDeferred) {
			for _, usage := range []struct {
				kind  string
				value int
			}{
				{"input", message.Usage.Input},
				{"output", message.Usage.Output},
				{"cache_read", message.Usage.CacheRead},
				{"cache_write", message.Usage.CacheWrite},
			} {
				attributes := o.attributes()
				attributes["kind"] = usage.kind
				o.instruments.tokens.Add(float64(usage.value), attributes)
			}
			o.instruments.cost.Add(message.Usage.Cost.Total, o.attributes())
		}
	}
	o.finish()
}

// truncateUTF16Units is JavaScript's s.slice(0, n): the first n UTF-16 code
// units. A cut through a surrogate pair leaves U+FFFD where pi keeps the
// lone high surrogate, which no Go string holds; U+FFFD is what a JSON
// decoder makes of that surrogate, and it is one code unit as well.
func truncateUTF16Units(s string, n int) string {
	units := 0
	for i, r := range s {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		switch {
		case units+width <= n:
			units += width
		case units < n:
			return s[:i] + string(utf8.RuneError)
		default:
			return s[:i]
		}
	}
	return s
}
