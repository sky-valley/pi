package ai

import (
	"context"
	"reflect"
	"testing"
)

// These transliterate pi's packages/ai/test/opencode-provider-headers.test.ts:
// withOpenCodeSessionHeader over recording streams, asserting the headers the
// wrapped api receives.

func openCodeTestModel() *Model {
	return &Model{ID: "test-model", Name: "Test model", Api: "test-api", Provider: "opencode",
		BaseURL: "https://opencode.ai/zen/v1", Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}
}

func recordingStreams(capture func(headers ProviderHeaders)) ProviderStreams {
	done := func(model *Model) *AssistantMessageEventStream {
		stream := NewAssistantMessageEventStream()
		message := &AssistantMessage{Model: model.ID, StopReason: StopStop}
		stream.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: message})
		stream.End(message)
		return stream
	}
	return ProviderStreams{
		Stream: func(_ context.Context, model *Model, _ TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
			capture(opts.Headers)
			return done(model)
		},
		StreamSimple: func(_ context.Context, model *Model, _ TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
			capture(opts.Headers)
			return done(model)
		},
	}
}

func openCodeTestContext() TranscriptContext {
	return NormalizeContext(Context{Messages: []Message{NewUserText("hi", 0)}})
}

// pi: "maps sessionId for %s requests even without cache retention" (#9326).
func TestOpenCodeSessionHeaderWithoutCacheRetention(t *testing.T) {
	for _, method := range []string{"stream", "streamSimple"} {
		t.Run(method, func(t *testing.T) {
			var captured ProviderHeaders
			streams := openCodeStreams(recordingStreams(func(h ProviderHeaders) { captured = h }))
			options := StreamOptions{SessionID: "conversation-1", CacheRetention: CacheNone}
			if method == "stream" {
				streams.Stream(context.Background(), openCodeTestModel(), openCodeTestContext(), &options).Result()
			} else {
				streams.StreamSimple(context.Background(), openCodeTestModel(), openCodeTestContext(), &SimpleStreamOptions{StreamOptions: options}).Result()
			}
			if want := (ProviderHeaders{"x-opencode-session": HeaderValue("conversation-1")}); !reflect.DeepEqual(captured, want) {
				t.Fatalf("headers = %v, want %v", printableHeaders(captured), printableHeaders(want))
			}
		})
	}
}

// pi: "preserves a case-insensitive caller override", value and marker.
func TestOpenCodeSessionHeaderCallerOverride(t *testing.T) {
	for _, headers := range []ProviderHeaders{
		{"X-OpenCode-Session": HeaderValue("caller-value")},
		{"X-OpenCode-Session": nil},
	} {
		var captured ProviderHeaders
		streams := openCodeStreams(recordingStreams(func(h ProviderHeaders) { captured = h }))
		streams.StreamSimple(context.Background(), openCodeTestModel(), openCodeTestContext(),
			&SimpleStreamOptions{StreamOptions: StreamOptions{SessionID: "generated-value",
				ProviderRequestOptions: ProviderRequestOptions{Headers: headers}}}).Result()
		if !reflect.DeepEqual(captured, headers) {
			t.Fatalf("headers = %v, want the caller's %v", printableHeaders(captured), printableHeaders(headers))
		}
	}
}

// pi: "does not fabricate a session header when sessionId is absent".
func TestOpenCodeSessionHeaderAbsentWithoutSessionID(t *testing.T) {
	var captured ProviderHeaders
	streams := openCodeStreams(recordingStreams(func(h ProviderHeaders) { captured = h }))
	headers := ProviderHeaders{"x-custom": HeaderValue("value")}
	streams.StreamSimple(context.Background(), openCodeTestModel(), openCodeTestContext(),
		&SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Headers: headers}}}).Result()
	if !reflect.DeepEqual(captured, headers) {
		t.Fatalf("headers = %v, want %v", printableHeaders(captured), printableHeaders(headers))
	}
}

// The wrapper copies: the caller's map is never written through.
func TestOpenCodeSessionHeaderLeavesTheCallersMap(t *testing.T) {
	streams := openCodeStreams(recordingStreams(func(ProviderHeaders) {}))
	headers := ProviderHeaders{"x-custom": HeaderValue("value")}
	streams.Stream(context.Background(), openCodeTestModel(), openCodeTestContext(),
		&StreamOptions{SessionID: "s", ProviderRequestOptions: ProviderRequestOptions{Headers: headers}}).Result()
	if len(headers) != 1 {
		t.Fatalf("the caller's headers were written through: %v", printableHeaders(headers))
	}
}

// The built-in opencode and opencode-go providers carry the wrapper.
func TestBuiltinOpenCodeProvidersWrapTheirStreams(t *testing.T) {
	for _, provider := range []string{"opencode", "opencode-go"} {
		var captured ProviderHeaders
		streams := builtinProviderStreams(provider, recordingStreams(func(h ProviderHeaders) { captured = h }))
		streams.Stream(context.Background(), openCodeTestModel(), openCodeTestContext(), &StreamOptions{SessionID: "s"}).Result()
		if got := captured["x-opencode-session"]; got == nil || *got != "s" {
			t.Fatalf("%s: x-opencode-session = %v, want s", provider, printableHeaders(captured))
		}
	}
	var captured ProviderHeaders
	streams := builtinProviderStreams("openai", recordingStreams(func(h ProviderHeaders) { captured = h }))
	streams.Stream(context.Background(), openCodeTestModel(), openCodeTestContext(), &StreamOptions{SessionID: "s"}).Result()
	if captured != nil {
		t.Fatalf("openai's streams carry the OpenCode wrapper: %v", printableHeaders(captured))
	}
}

func printableHeaders(headers ProviderHeaders) map[string]any {
	out := map[string]any{}
	for name, value := range headers {
		if value == nil {
			out[name] = nil
		} else {
			out[name] = *value
		}
	}
	return out
}
