package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/telemetry"
)

// telemetryCaptureFile is what testdata/telemetry/capture.mts recorded from
// pi's Models at d2e7d8b7c.
const telemetryCaptureFile = "testdata/telemetry/telemetry-d2e7d8b7c.json"

var telemetryTestModel = &Model{
	ID:            "model",
	Name:          "Model",
	Api:           "telemetry-test",
	Provider:      "test",
	BaseURL:       "https://example.test",
	Reasoning:     true,
	Input:         []string{"text"},
	Cost:          ModelCost{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4},
	ContextWindow: 1000,
	MaxTokens:     100,
}

var telemetryTestHandle = DeferredHandle{Provider: "test", ModelID: "model", Api: "telemetry-test", ID: "deferred-secret"}

// telemetryTestMessage is capture.mts's message(): text "a😀b" and tool
// deltas with an astral character pin UTF-16 block lengths.
func telemetryTestMessage(stopReason StopReason, errorMessage string) *AssistantMessage {
	return &AssistantMessage{
		Content: ContentList{
			TextContent{Text: "a😀b"},
			ThinkingContent{Thinking: "thinking-secret"},
			ToolCall{ID: "call-secret", Name: "lookup", Arguments: map[string]any{"v": "😀"}},
		},
		Api:      "telemetry-test",
		Provider: "test",
		Model:    "model",
		Usage: Usage{
			Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44, TotalTokens: 110,
			Cost: CostBreakdown{Input: 0.1, Output: 0.2, CacheRead: 0.3, CacheWrite: 0.4, Total: 1.5},
		},
		ResponseID:   "response",
		StopReason:   stopReason,
		ErrorMessage: errorMessage,
		Timestamp:    nowMillis(),
	}
}

func telemetryTerminal(result *AssistantMessage) AssistantMessageEvent {
	switch result.StopReason {
	case StopError, StopAborted:
		return AssistantMessageEvent{Type: EventError, Reason: result.StopReason, Error: result}
	case StopPending:
		return AssistantMessageEvent{Type: EventDone, Reason: StopDeferred, Message: result}
	}
	return AssistantMessageEvent{Type: EventDone, Reason: result.StopReason, Message: result}
}

// telemetryScripts are capture.mts's event scripts, by name.
var telemetryScripts = map[string]func(result *AssistantMessage) []AssistantMessageEvent{
	"blocks": telemetryBlockEvents,
	"interleaved": func(result *AssistantMessage) []AssistantMessageEvent {
		toolCall := &ToolCall{ID: "call", Name: "lookup", Arguments: map[string]any{}}
		return []AssistantMessageEvent{
			{Type: EventToolCallDelta, ContentIndex: 2, Delta: "ab", Partial: result},
			{Type: EventToolCallDelta, ContentIndex: 3, Delta: "123", Partial: result},
			{Type: EventToolCallDelta, ContentIndex: 2, Delta: "c", Partial: result},
			{Type: EventToolCallEnd, ContentIndex: 3, ToolCall: toolCall, Partial: result},
			{Type: EventToolCallEnd, ContentIndex: 2, ToolCall: toolCall, Partial: result},
			{Type: EventToolCallEnd, ContentIndex: 4, ToolCall: toolCall, Partial: result},
			telemetryTerminal(result),
		}
	},
	"blocks-without-terminal": func(result *AssistantMessage) []AssistantMessageEvent {
		events := telemetryBlockEvents(result)
		return events[:len(events)-1]
	},
	"none": func(*AssistantMessage) []AssistantMessageEvent { return nil },
}

func telemetryBlockEvents(result *AssistantMessage) []AssistantMessageEvent {
	toolCall := result.Content[2].(ToolCall)
	return []AssistantMessageEvent{
		{Type: EventStart, Partial: result},
		{Type: EventTextDelta, ContentIndex: 0, Delta: "a😀b", Partial: result},
		{Type: EventTextEnd, ContentIndex: 0, Content: "a😀b", Partial: result},
		{Type: EventThinkingEnd, ContentIndex: 1, Content: "thinking-secret", Partial: result},
		{Type: EventToolCallDelta, ContentIndex: 2, Delta: `{"v":`, Partial: result},
		{Type: EventToolCallDelta, ContentIndex: 2, Delta: `"😀"}`, Partial: result},
		{Type: EventToolCallEnd, ContentIndex: 2, ToolCall: &toolCall, Partial: result},
		telemetryTerminal(result),
	}
}

// telemetryModels serves telemetryTestModel from a provider that replays
// events and ends with result on every entry point.
func telemetryModels(events []AssistantMessageEvent, result *AssistantMessage) MutableModels {
	respond := func() *AssistantMessageEventStream {
		stream := NewAssistantMessageEventStream()
		for _, event := range events {
			stream.Push(event)
		}
		stream.End(result)
		return stream
	}
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "test",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("Test", "K")},
		Models: []*Model{telemetryTestModel},
		API: ptrStreams(ProviderStreams{
			Stream: func(context.Context, *Model, TranscriptContext, *StreamOptions) *AssistantMessageEventStream {
				return respond()
			},
			StreamSimple: func(context.Context, *Model, TranscriptContext, *SimpleStreamOptions) *AssistantMessageEventStream {
				return respond()
			},
			FetchDeferred: func(context.Context, *Model, DeferredHandle, *DeferredFetchOptions) *AssistantMessageEventStream {
				return respond()
			},
		}),
	}))
	return m
}

// telemetryRequest runs one Models entry point with span as the telemetry
// parent and returns the delivered events and the result.
func telemetryRequest(m Models, entry string, span telemetry.Span, reasoning ThinkingLevel) ([]AssistantMessageEvent, *AssistantMessage) {
	ctx := context.Background()
	request := ProviderRequestOptions{Telemetry: span}
	var stream *AssistantMessageEventStream
	switch entry {
	case "Stream":
		stream = m.Stream(ctx, telemetryTestModel, Context{}, &ModelsStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: request}})
	case "StreamSimple":
		stream = m.StreamSimple(ctx, telemetryTestModel, Context{}, &ModelsSimpleStreamOptions{SimpleStreamOptions: SimpleStreamOptions{
			StreamOptions: StreamOptions{ProviderRequestOptions: request},
			Reasoning:     reasoning,
		}})
	case "StreamDeferred":
		stream = m.StreamDeferred(ctx, telemetryTestModel, telemetryTestHandle, &ModelsDeferredFetchOptions{DeferredFetchOptions: DeferredFetchOptions{ProviderRequestOptions: request}})
	default:
		panic("unknown entry " + entry)
	}
	var delivered []AssistantMessageEvent
	for event := range stream.Events() {
		delivered = append(delivered, event)
	}
	return delivered, stream.Result()
}

// waitSettled waits for every span the recorder holds to settle: a request
// span settles once onResult released its callback, which a caller holding
// the result does not wait for, and a caller span once its own callback
// returned — upstream's tests wait the same way (vi.waitFor).
func waitSettled(t *testing.T, recorder *telemetry.InMemoryContext) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		settled := true
		for _, span := range recorder.Spans() {
			settled = settled && span.Settled
		}
		if settled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spans never settled: %+v", recorder.Spans())
		}
		time.Sleep(time.Millisecond)
	}
}

// factorySpy wraps a recorder's metric capability to log the instruments a
// request creates and the metadata it hands the adapter, as capture.mts's
// spyFactories does.
type factorySpy struct {
	metrics telemetry.Metrics
	mu      sync.Mutex
	calls   []map[string]any
}

func (s *factorySpy) log(kind, name string, options telemetry.InstrumentOptions, boundaries []float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := map[string]any{"kind": kind, "name": name, "description": nil, "unit": nil, "boundaries": nil}
	if options.Description != "" {
		call["description"] = options.Description
	}
	if options.Unit != "" {
		call["unit"] = options.Unit
	}
	if boundaries != nil {
		call["boundaries"] = boundaries
	}
	s.calls = append(s.calls, call)
}

func (s *factorySpy) Counter(name string, options telemetry.InstrumentOptions) telemetry.Counter {
	s.log("counter", name, options, nil)
	return s.metrics.Counter(name, options)
}

func (s *factorySpy) UpDownCounter(name string, options telemetry.InstrumentOptions) telemetry.UpDownCounter {
	s.log("upDownCounter", name, options, nil)
	return s.metrics.UpDownCounter(name, options)
}

func (s *factorySpy) Histogram(name string, options telemetry.HistogramOptions) telemetry.Histogram {
	s.log("histogram", name, options.InstrumentOptions, options.Boundaries)
	return s.metrics.Histogram(name, options)
}

// spiedSpan is a caller span whose metric capability is replaced: by the
// spy, or by nothing for pi's tracing-only `{ ...span, metrics: undefined }`.
type spiedSpan struct {
	telemetry.Span
	metrics telemetry.Metrics
}

func (s spiedSpan) Metrics() telemetry.Metrics { return s.metrics }

// carrierRef is capture.mts's spanRef: the recorder-local span id a carrier
// names, or nil.
func carrierRef(carrier telemetry.Carrier) any {
	if carrier == nil {
		return nil
	}
	id, err := strconv.Atoi(carrier["pi-memory-span"])
	if err != nil {
		return carrier
	}
	return id
}

// recordedTelemetry renders a recorder the way capture.mts's record() does,
// as generic JSON, less endSequence: the order a request span and its caller
// settle in is scheduling, in pi (microtask depth) as in Go (goroutines).
func recordedTelemetry(t *testing.T, recorder *telemetry.InMemoryContext) map[string]any {
	t.Helper()
	var spans []map[string]any
	for _, span := range recorder.Spans() {
		links := []map[string]any{}
		for _, link := range span.Links {
			links = append(links, map[string]any{"context": carrierRef(link.Context), "attributes": link.Attributes})
		}
		events := []map[string]any{}
		for _, event := range span.Events {
			events = append(events, map[string]any{"name": event.Name, "attributes": event.Attributes})
		}
		status := map[string]any{"status": string(span.Status.Code)}
		if span.Status.Error != nil {
			status["error"] = map[string]any{"name": span.Status.Error.Name, "message": span.Status.Error.Message}
		}
		var parentID any
		if span.ParentID != 0 {
			parentID = span.ParentID
		}
		spans = append(spans, map[string]any{
			"id":            span.ID,
			"parentId":      parentID,
			"context":       carrierRef(span.Context),
			"parentContext": carrierRef(span.ParentContext),
			"links":         links,
			"name":          span.Name,
			"attributes":    span.Attributes,
			"events":        events,
			"status":        status,
			"settled":       span.Settled,
		})
	}
	measurements := []map[string]any{}
	for _, m := range recorder.Measurements() {
		var value any = m.Value
		if strings.HasSuffix(m.Name, ".duration") && m.Value >= 0 {
			value = "duration"
		}
		optional := func(s string) any {
			if s == "" {
				return nil
			}
			return s
		}
		var boundaries any
		if m.Boundaries != nil {
			boundaries = m.Boundaries
		}
		measurements = append(measurements, map[string]any{
			"kind":        string(m.Kind),
			"name":        m.Name,
			"description": optional(m.Description),
			"unit":        optional(m.Unit),
			"boundaries":  boundaries,
			"value":       value,
			"attributes":  m.Attributes,
			"context":     carrierRef(m.Context),
		})
	}
	return normalizeJSON(t, map[string]any{"spans": spans, "measurements": measurements}).(map[string]any)
}

// normalizeJSON round-trips v through encoding/json, so Go ints and pi
// numbers compare as float64 and a lone surrogate pi wrote as \ud83d reads
// as U+FFFD on both sides.
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func dropEndSequence(spans any) any {
	list, _ := spans.([]any)
	for _, span := range list {
		delete(span.(map[string]any), "endSequence")
	}
	return list
}

type telemetryCapture struct {
	Rows []struct {
		Name               string          `json:"name"`
		Entry              string          `json:"entry"`
		Events             string          `json:"events"`
		Reasoning          *string         `json:"reasoning"`
		TracingOnly        bool            `json:"tracingOnly"`
		NoProvider         bool            `json:"noProvider"`
		ResultStopReason   *StopReason     `json:"resultStopReason"`
		ResultErrorMessage *string         `json:"resultErrorMessage"`
		Delivered          []EventType     `json:"delivered"`
		StopReason         StopReason      `json:"stopReason"`
		ErrorMessage       *string         `json:"errorMessage"`
		FactoryCalls       json.RawMessage `json:"factoryCalls"`
		Spans              json.RawMessage `json:"spans"`
		Measurements       json.RawMessage `json:"measurements"`
	} `json:"rows"`
	InstrumentsOnce struct {
		FactoryCalls json.RawMessage `json:"factoryCalls"`
		Spans        json.RawMessage `json:"spans"`
		Measurements json.RawMessage `json:"measurements"`
	} `json:"instrumentsOnce"`
}

func loadTelemetryCapture(t *testing.T) telemetryCapture {
	t.Helper()
	data, err := os.ReadFile(telemetryCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture telemetryCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mts", telemetryCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mts", telemetryCaptureFile)
	}
	return capture
}

func decodeRaw(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestModelsTelemetryMatchesPi replays each capture row through Go's Models
// under a caller span of an InMemoryContext and compares what was recorded —
// spans, parents, attributes, events, statuses, links, measurements and the
// instruments created — with what pi recorded, and the delivered events and
// result with the provider's own (the observation must not change them).
func TestModelsTelemetryMatchesPi(t *testing.T) {
	capture := loadTelemetryCapture(t)
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			script, ok := telemetryScripts[row.Events]
			if !ok {
				t.Fatalf("unknown event script %q; port it from capture.mts", row.Events)
			}
			var result *AssistantMessage
			var events []AssistantMessageEvent
			m := modelsWithEnv(nil, nil)
			if !row.NoProvider {
				errorMessage := ""
				if row.ResultErrorMessage != nil {
					errorMessage = *row.ResultErrorMessage
				}
				result = telemetryTestMessage(*row.ResultStopReason, errorMessage)
				events = script(result)
				m = telemetryModels(events, result)
			}
			var reasoning ThinkingLevel
			if row.Reasoning != nil {
				reasoning = ThinkingLevel(*row.Reasoning)
			}

			recorder := telemetry.NewInMemoryContext()
			spy := &factorySpy{metrics: recorder, calls: []map[string]any{}}
			var delivered []AssistantMessageEvent
			var got *AssistantMessage
			err := recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(parent telemetry.Span) error {
				span := spiedSpan{Span: parent, metrics: spy}
				if row.TracingOnly {
					span.metrics = nil
				}
				delivered, got = telemetryRequest(m, row.Entry, span, reasoning)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			waitSettled(t, recorder)

			var deliveredTypes []EventType
			for _, event := range delivered {
				deliveredTypes = append(deliveredTypes, event.Type)
			}
			if !slices.Equal(deliveredTypes, row.Delivered) {
				t.Errorf("delivered %v, pi %v", deliveredTypes, row.Delivered)
			}
			if result != nil {
				if !reflect.DeepEqual(delivered, events) {
					t.Errorf("delivered events differ from the provider's")
				}
				if got != result {
					t.Errorf("result %p, want the provider's own %p", got, result)
				}
			}
			if got == nil || got.StopReason != row.StopReason {
				t.Fatalf("result = %+v, pi stopped %q", got, row.StopReason)
			}
			if row.ErrorMessage != nil && got.ErrorMessage != *row.ErrorMessage {
				t.Errorf("errorMessage = %q, pi %q", got.ErrorMessage, *row.ErrorMessage)
			}

			recorded := recordedTelemetry(t, recorder)
			if want := dropEndSequence(decodeRaw(t, row.Spans)); !reflect.DeepEqual(recorded["spans"], want) {
				t.Errorf("spans:\n go %v\n pi %v", recorded["spans"], want)
			}
			if want := decodeRaw(t, row.Measurements); !reflect.DeepEqual(recorded["measurements"], want) {
				t.Errorf("measurements:\n go %v\n pi %v", recorded["measurements"], want)
			}
			spy.mu.Lock()
			calls := normalizeJSON(t, spy.calls)
			spy.mu.Unlock()
			if want := decodeRaw(t, row.FactoryCalls); !reflect.DeepEqual(calls, want) {
				t.Errorf("instruments created:\n go %v\n pi %v", calls, want)
			}
		})
	}
}

// TestModelsTelemetryCreatesInstrumentsOncePerMetrics mirrors pi "creates
// instruments once per metrics object across root and child requests" with
// the capture's three requests on one recorder: the four instruments are
// created by the first request only, and every request still measures.
func TestModelsTelemetryCreatesInstrumentsOncePerMetrics(t *testing.T) {
	capture := loadTelemetryCapture(t)
	recorder := telemetry.NewInMemoryContext()
	spy := &factorySpy{metrics: recorder, calls: []map[string]any{}}
	result := telemetryTestMessage(StopStop, "")
	m := telemetryModels(telemetryBlockEvents(result), result)
	for _, entry := range []string{"StreamSimple", "StreamDeferred", "Stream"} {
		_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(parent telemetry.Span) error {
			telemetryRequest(m, entry, spiedSpan{Span: parent, metrics: spy}, "")
			return nil
		})
	}
	waitSettled(t, recorder)
	recorded := recordedTelemetry(t, recorder)
	if want := dropEndSequence(decodeRaw(t, capture.InstrumentsOnce.Spans)); !reflect.DeepEqual(recorded["spans"], want) {
		t.Errorf("spans:\n go %v\n pi %v", recorded["spans"], want)
	}
	if want := decodeRaw(t, capture.InstrumentsOnce.Measurements); !reflect.DeepEqual(recorded["measurements"], want) {
		t.Errorf("measurements:\n go %v\n pi %v", recorded["measurements"], want)
	}
	if want := decodeRaw(t, capture.InstrumentsOnce.FactoryCalls); !reflect.DeepEqual(normalizeJSON(t, spy.calls), want) {
		t.Errorf("instruments created:\n go %v\n pi %v", spy.calls, want)
	}
}

// slowSpan delays its own and its children's recording, so an observer that
// ran behind the consumer would be caught with the event or result
// unrecorded.
type slowSpan struct{ telemetry.Span }

const slowSpanDelay = 20 * time.Millisecond

func (s slowSpan) StartSpan(options telemetry.SpanOptions, callback func(telemetry.Span) error) error {
	return s.Span.StartSpan(options, func(child telemetry.Span) error { return callback(slowSpan{child}) })
}

func (s slowSpan) AddEvent(name string, attributes telemetry.SpanAttributes) {
	time.Sleep(slowSpanDelay)
	s.Span.AddEvent(name, attributes)
}

func (s slowSpan) SetAttributes(attributes telemetry.SpanAttributes) {
	time.Sleep(slowSpanDelay)
	s.Span.SetAttributes(attributes)
}

// TestModelsTelemetryObservesBeforeDelivery pins what pi's lazyStream
// guarantees a consumer that iterates the stream: onEvent runs synchronously
// right after push, before the consumer's loop resumes, and onResult runs
// before that loop sees the terminal event. Probed against pi at d2e7d8b7c
// (InMemoryTelemetryContext, a buffered and a live provider stream): at
// each delivered event the request span already holds that event's
// telemetry, at "done" all seven measurements are recorded, and at the
// result the end attributes. A consumer that changes an event it was handed
// therefore cannot change what was recorded (the tool name here). The
// adapter below records slowly, so a Go observer running behind the
// consumer is caught.
func TestModelsTelemetryObservesBeforeDelivery(t *testing.T) {
	result := telemetryTestMessage(StopStop, "")
	m := telemetryModels(telemetryBlockEvents(result), result)
	recorder := telemetry.NewInMemoryContext()
	_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(parent telemetry.Span) error {
		stream := m.Stream(context.Background(), telemetryTestModel, Context{}, &ModelsStreamOptions{
			StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Telemetry: slowSpan{parent}}},
		})
		blocks := 0
		for event := range stream.Events() {
			switch event.Type {
			case EventTextEnd, EventThinkingEnd, EventToolCallEnd:
				blocks++
			}
			request := recorder.Spans()[1]
			if got := len(request.Events); got < 1+blocks {
				t.Errorf("at %s the request span holds %d events, want first_chunk and %d blocks", event.Type, got, blocks)
			}
			if event.Type == EventToolCallEnd {
				event.ToolCall.Name = "changed-by-the-consumer"
			}
			if event.Type == EventDone {
				if got := len(recorder.Measurements()); got != 7 {
					t.Errorf("at done %d measurements are recorded, want 7", got)
				}
			}
		}
		stream.Result()
		if got := len(recorder.Measurements()); got != 7 {
			t.Errorf("at the result %d measurements are recorded, want 7", got)
		}
		if got := recorder.Spans()[1].Attributes["stop"]; got != "stop" {
			t.Errorf("at the result the request span's stop = %v, want stop", got)
		}
		return nil
	})
	waitSettled(t, recorder)
	events := recorder.Spans()[1].Events
	if got := events[len(events)-1].Attributes["tool"]; got != "lookup" {
		t.Errorf("recorded tool = %v, want lookup: the observer read the event after the consumer changed it", got)
	}
}

// TestModelsTelemetryBoundsPollErrorMessage mirrors pi "bounds poll error
// messages, including missing messages": the provider_error status carries
// the first 500 characters of the error message, or "" when there is none.
func TestModelsTelemetryBoundsPollErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		errorMessage, want string
	}{
		{strings.Repeat("x", 500) + "discarded-tail", strings.Repeat("x", 500)},
		{"", ""},
	} {
		result := telemetryTestMessage(StopError, tc.errorMessage)
		m := telemetryModels(telemetryBlockEvents(result), result)
		recorder := telemetry.NewInMemoryContext()
		_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
			got := m.FetchDeferred(context.Background(), telemetryTestModel, telemetryTestHandle, &ModelsDeferredFetchOptions{
				DeferredFetchOptions: DeferredFetchOptions{ProviderRequestOptions: ProviderRequestOptions{Telemetry: span}},
			})
			if got != result {
				t.Errorf("FetchDeferred = %p, want the provider's result %p", got, result)
			}
			return nil
		})
		waitSettled(t, recorder)
		status := recorder.Spans()[1].Status
		if status.Code != telemetry.StatusError || status.Error == nil || status.Error.Name != "provider_error" || status.Error.Message != tc.want {
			t.Errorf("status = %+v (error %+v), want provider_error %q", status, status.Error, tc.want)
		}
	}
}

// skippingSpan is a non-conforming adapter: its StartSpan returns without
// running the callback.
type skippingSpan struct{ telemetry.Span }

func (skippingSpan) StartSpan(telemetry.SpanOptions, func(telemetry.Span) error) error {
	return errors.New("adapter closed")
}

// TestModelsTelemetrySpanNeverStarted pins what pi does when the parent's
// startSpan never runs its callback, probed against pi at d2e7d8b7c: the
// request has no span, so the observer's first use of it throws, and
// lazyStream's catch ends the stream with an error. The consumer gets the
// first event, then an error event, and an error result (pi's message is the
// TypeError's own, "Cannot read properties of undefined (reading
// 'addEvent')"; Go's names the broken adapter). A provider stream with no
// events ends with its own result: pi throws only at onResult, after the
// stream ended. A setup failure ends with the setup error; pi throws in its
// catch before end() and never ends the stream, which Go does not mirror.
// Every call returns.
func TestModelsTelemetrySpanNeverStarted(t *testing.T) {
	stop := telemetryTestMessage(StopStop, "")
	for _, tc := range []struct {
		name      string
		models    MutableModels
		delivered []EventType
		stop      StopReason
		message   string
	}{
		{"events", telemetryModels(telemetryBlockEvents(stop), stop), []EventType{EventStart, EventError}, StopError, "StartSpan returned without running its callback (adapter closed)"},
		{"no events", telemetryModels(nil, stop), nil, StopStop, ""},
		{"setup failure", modelsWithEnv(nil, nil), []EventType{EventError}, StopError, "Unknown provider: test"},
	} {
		done := make(chan struct{})
		var delivered []EventType
		var result *AssistantMessage
		go func() {
			defer close(done)
			stream := tc.models.Stream(context.Background(), telemetryTestModel, Context{}, &ModelsStreamOptions{
				StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Telemetry: skippingSpan{telemetry.NoopContext}}},
			})
			for event := range stream.Events() {
				delivered = append(delivered, event.Type)
			}
			result = stream.Result()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the request never returned: it waits for a span the adapter never handed over", tc.name)
		}
		if !reflect.DeepEqual(delivered, tc.delivered) {
			t.Errorf("%s: delivered %v, want %v", tc.name, delivered, tc.delivered)
		}
		if result == nil || result.StopReason != tc.stop || !strings.Contains(result.ErrorMessage, tc.message) {
			t.Errorf("%s: result %+v, want stop %q with an error message containing %q", tc.name, result, tc.stop, tc.message)
		}
	}
}

// TestModelsTelemetryNoopIsNotObserved: pi observes every request, but with
// no telemetry, or the no-op context, its span and instruments discard
// everything, so Go skips the observation outright — no instruments,
// attribute maps or block lengths per request.
func TestModelsTelemetryNoopIsNotObserved(t *testing.T) {
	for _, parent := range []telemetry.Span{nil, telemetry.NoopContext} {
		if observer := observeModelRequest(telemetryTestModel, parent, "", false); observer != nil {
			t.Errorf("parent %v: a request without telemetry is observed by %T", parent, observer)
		}
	}
}

// TestModelsTelemetryDefaultsToNoop mirrors pi "defaults to no-op
// telemetry": a request without a telemetry span runs as before, and the
// exported schema is plain data.
func TestModelsTelemetryDefaultsToNoop(t *testing.T) {
	result := telemetryTestMessage(StopStop, "")
	m := telemetryModels(telemetryBlockEvents(result), result)
	if got := m.CompleteSimple(context.Background(), telemetryTestModel, Context{}, nil); got.StopReason != StopStop {
		t.Errorf("stopReason = %q, want stop", got.StopReason)
	}
	if !TelemetrySchema.Spans["pi.ai.request"].StartAttributes["provider"].Required {
		t.Error("pi.ai.request's provider start attribute must be required")
	}
}

// TestRequestTelemetryRecordsMonotonicDurations mirrors pi "records
// low-cardinality metrics with monotonic durations": with the clock at 100
// when the request starts, 125 at the first event and 175 at the result, the
// first chunk took 25ms and the request 75ms, and the counters carry the
// usage, each instrument with its schema unit.
func TestRequestTelemetryRecordsMonotonicDurations(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	result := telemetryTestMessage(StopStop, "")
	result.Usage.Cost.Total = 1
	clock := time.UnixMilli(100)
	now := func() time.Time { return clock }
	_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		observer := startRequestObservation(telemetryTestModel, span, "", false, now)
		clock = time.UnixMilli(125)
		observer.onEvent(AssistantMessageEvent{Type: EventStart, Partial: result})
		clock = time.UnixMilli(175)
		observer.onResult(result)
		return nil
	})
	waitSettled(t, recorder)
	type row struct {
		Name       string
		Value      float64
		Attributes telemetry.SpanAttributes
		Unit       string
		Exemplar   bool
	}
	var got []row
	for _, m := range recorder.Measurements() {
		got = append(got, row{m.Name, m.Value, m.Attributes, m.Unit, m.Context != nil})
	}
	tokens := func(kind string, value float64) row {
		return row{"pi.ai.tokens", value, telemetry.SpanAttributes{"provider": "test", "model": "model", "kind": kind}, "token", false}
	}
	want := []row{
		{"pi.ai.first_chunk.duration", 25, telemetry.SpanAttributes{"provider": "test", "model": "model"}, "ms", true},
		{"pi.ai.request.duration", 75, telemetry.SpanAttributes{"provider": "test", "model": "model", "stop": "stop"}, "ms", true},
		tokens("input", 11), tokens("output", 22), tokens("cache_read", 33), tokens("cache_write", 44),
		{"pi.ai.cost", 1, telemetry.SpanAttributes{"provider": "test", "model": "model"}, "USD", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("measurements:\n got %+v\nwant %+v", got, want)
	}
}

// recordingObserver logs what a streamSetup shows it. Like upstream's test
// observer, its onEvent starts a waiter on the stream's result, which logs
// "resolved" once the result is available.
type recordingObserver struct {
	stream *AssistantMessageEventStream
	mu     sync.Mutex
	log    []string
}

func (o *recordingObserver) record(entry string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.log = append(o.log, entry)
}

func (o *recordingObserver) entries() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.log)
}

func (o *recordingObserver) onEvent(event AssistantMessageEvent) error {
	o.record("event " + string(event.Type))
	if o.stream != nil {
		go func() {
			o.stream.Result()
			o.record("resolved")
		}()
	}
	return nil
}

func (o *recordingObserver) onResult(result *AssistantMessage) {
	entry := "result <nil>"
	if result != nil {
		entry = "result " + string(result.StopReason)
	}
	o.record(entry)
}

// TestStreamSetupObservesBeforeDelivery mirrors pi lazyStream observation
// "uses one source iterator and observes after push, once per terminal
// result" and "observes a result-only source once": the observer sees the
// event, then the result, once each, and a result-only source's result is
// observed by the time the consumer has it.
//
// pi's order is ["event", "resolved", "result"]: its onEvent runs right after
// push, so a result() reaction registered there is queued before
// forwardStream reaches onResult, while a consumer iterating the stream sees
// the terminal event only after onResult. Go cannot order a Result waiter
// apart from an iterating consumer — both wait on the terminal event — so it
// takes the iterating consumer's order for both, observes first and delivers
// after, and the waiter logs "resolved" last (see streamObserver).
func TestStreamSetupObservesBeforeDelivery(t *testing.T) {
	result := telemetryTestMessage(StopStop, "")
	for _, tc := range []struct {
		name   string
		events []AssistantMessageEvent
		want   []string
	}{
		{"terminal event", []AssistantMessageEvent{{Type: EventDone, Reason: StopStop, Message: result}}, []string{"event done", "result stop", "resolved"}},
		{"result only", nil, []string{"result stop"}},
	} {
		inner := NewAssistantMessageEventStream()
		for _, event := range tc.events {
			inner.Push(event)
		}
		inner.End(result)
		observer := &recordingObserver{}
		setup := startStreamSetup(telemetryTestModel, observer)
		observer.stream = setup.stream
		stream := setup.forward(inner)
		if got := stream.Result(); got != result {
			t.Errorf("%s: Result = %p, want %p", tc.name, got, result)
		}
		if got := observer.entries(); !slices.Contains(got, "result stop") {
			t.Errorf("%s: at the result the observer had seen %q, want the result observed", tc.name, got)
		}
		for range stream.Events() {
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(observer.entries()) < len(tc.want) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := observer.entries(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: observed %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStreamSetupObservesSetupFailureOnce mirrors pi "reports setup failure
// events and the result once".
func TestStreamSetupObservesSetupFailureOnce(t *testing.T) {
	observer := &recordingObserver{}
	setup := startStreamSetup(telemetryTestModel, observer)
	stream := setup.fail(errors.New("setup-secret"))
	if got := stream.Result(); got.ErrorMessage != "setup-secret" {
		t.Errorf("errorMessage = %q", got.ErrorMessage)
	}
	if got, want := observer.entries(), []string{"event error", "result error"}; !reflect.DeepEqual(got, want) {
		t.Errorf("observed %q, want %q", got, want)
	}
}

// TestCompatCloudflareStreamsThroughModels mirrors pi's compat stream() and
// streamSimple() on a built-in cloudflare-* model with no resolved key: they
// route through compatModels, the Models runtime, so the request is observed
// under options.telemetry and a non-chat model is refused. Every row of
// testdata/telemetry/compat-d2e7d8b7c.json (capture-compat.mts) runs with no
// Cloudflare credentials, as pi's did.
func TestCompatCloudflareStreamsThroughModels(t *testing.T) {
	for _, name := range []string{"CLOUDFLARE_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"} {
		t.Setenv(name, "")
	}
	data, err := os.ReadFile("testdata/telemetry/compat-d2e7d8b7c.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name         string          `json:"name"`
			Entry        string          `json:"entry"`
			Provider     string          `json:"provider"`
			ModelID      string          `json:"modelId"`
			Type         *string         `json:"type"`
			Reasoning    *string         `json:"reasoning"`
			Delivered    []EventType     `json:"delivered"`
			StopReason   StopReason      `json:"stopReason"`
			ErrorMessage *string         `json:"errorMessage"`
			Spans        json.RawMessage `json:"spans"`
			Measurements json.RawMessage `json:"measurements"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			catalog := GetModel(row.Provider, row.ModelID)
			if catalog == nil {
				t.Fatalf("%s/%s is not in the catalog", row.Provider, row.ModelID)
			}
			model := *catalog
			if row.Type != nil {
				model.Type = ModelType(*row.Type)
			}
			recorder := telemetry.NewInMemoryContext()
			var delivered []EventType
			var result *AssistantMessage
			_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
				request := StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Telemetry: span}}
				var stream *AssistantMessageEventStream
				if row.Entry == "stream" {
					stream = Stream(context.Background(), &model, Context{}, &request)
				} else {
					simple := SimpleStreamOptions{StreamOptions: request}
					if row.Reasoning != nil {
						simple.Reasoning = ThinkingLevel(*row.Reasoning)
					}
					stream = StreamSimple(context.Background(), &model, Context{}, &simple)
				}
				for event := range stream.Events() {
					delivered = append(delivered, event.Type)
				}
				result = stream.Result()
				return nil
			})
			waitSettled(t, recorder)
			if !reflect.DeepEqual(delivered, row.Delivered) {
				t.Errorf("delivered %v, pi %v", delivered, row.Delivered)
			}
			if result == nil {
				t.Fatal("no result")
			}
			if result.StopReason != row.StopReason || row.ErrorMessage != nil && result.ErrorMessage != *row.ErrorMessage {
				t.Errorf("result stopped %q with %q, pi %q with %q", result.StopReason, result.ErrorMessage, row.StopReason, *row.ErrorMessage)
			}
			recorded := recordedTelemetry(t, recorder)
			if want := dropEndSequence(decodeRaw(t, row.Spans)); !reflect.DeepEqual(recorded["spans"], want) {
				t.Errorf("spans:\n go %v\n pi %v", recorded["spans"], want)
			}
			if want := decodeRaw(t, row.Measurements); !reflect.DeepEqual(recorded["measurements"], want) {
				t.Errorf("measurements:\n go %v\n pi %v", recorded["measurements"], want)
			}
		})
	}
}

// TestCompatCloudflareResolvedOrOverriddenStreamsDirect: compat.ts sends a
// cloudflare-* model straight to its api provider, unobserved, when the
// request carries an explicit key or a cf-aig-authorization header, and when
// the registry no longer holds the implementation compatModels was built
// from (getBuiltinProviderForModel compares the registry's provider with
// builtinApiProviderInstances).
func TestCompatCloudflareResolvedOrOverriddenStreamsDirect(t *testing.T) {
	model := GetModel("cloudflare-workers-ai", "@cf/deepseek-ai/deepseek-v4-flash-0731")
	if model == nil {
		t.Fatal("cloudflare-workers-ai/@cf/deepseek-ai/deepseek-v4-flash-0731 is not in the catalog")
	}
	// direct streams a row and reports whether only the caller's span was
	// recorded: no pi.ai.request, so the request went straight to the api
	// provider.
	direct := func(options StreamOptions) (*AssistantMessage, bool) {
		recorder := telemetry.NewInMemoryContext()
		var result *AssistantMessage
		_ = recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
			options.Telemetry = span
			result = Stream(context.Background(), model, Context{}, &options).Result()
			return nil
		})
		return result, len(recorder.Spans()) == 1
	}
	gateway := "Bearer k"
	for _, tc := range []struct {
		name    string
		options StreamOptions
	}{
		{"explicit key", StreamOptions{ProviderRequestOptions: ProviderRequestOptions{APIKey: "k"}}},
		{"gateway header", StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Headers: ProviderHeaders{"cf-aig-authorization": &gateway}}}},
	} {
		if result, ok := direct(tc.options); !ok {
			t.Errorf("%s: streamed through Models (%q), want the api provider", tc.name, result.ErrorMessage)
		}
	}

	// The built-ins were recorded without one (package ai registers none), so
	// an implementation registered for the api now is an override.
	RegisterApiProvider(ApiProvider{
		Api: model.Api,
		Stream: func(context.Context, *Model, TranscriptContext, *StreamOptions) *AssistantMessageEventStream {
			return ErrorStream(model, errors.New("direct"))
		},
	}, "compat-cloudflare-test")
	defer UnregisterApiProviders("compat-cloudflare-test")
	if result, ok := direct(StreamOptions{}); !ok || result.ErrorMessage != "direct" {
		t.Errorf("overridden api: streamed through %q, want the overriding api provider", result.ErrorMessage)
	}
}
