package providers

import (
	"context"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestFauxProviderStreamsProtocol(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()

	reg.SetResponses([]FauxResponseStep{
		FauxStatic(FauxAssistantMessage(ai.ContentList{
			FauxThinking("let me think"),
			FauxText("hello world this is a longer message to force multiple deltas"),
		}, ai.StopStop)),
	})

	model := reg.GetModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	stream := ai.StreamSimple(context.Background(), model, req, nil)

	var sawStart, sawTextDelta, sawThinkingDelta, sawDone bool
	for e := range stream.Events() {
		switch e.Type {
		case ai.EventStart:
			sawStart = true
		case ai.EventTextDelta:
			sawTextDelta = true
		case ai.EventThinkingDelta:
			sawThinkingDelta = true
		case ai.EventDone:
			sawDone = true
		}
	}
	final := stream.Result()
	if !sawStart || !sawTextDelta || !sawThinkingDelta || !sawDone {
		t.Fatalf("missing protocol events: start=%v textDelta=%v thinkDelta=%v done=%v", sawStart, sawTextDelta, sawThinkingDelta, sawDone)
	}
	if final == nil || final.StopReason != ai.StopStop {
		t.Fatalf("unexpected final message: %#v", final)
	}
	// Final text must be fully assembled from deltas.
	gotText := ""
	for _, c := range final.Content {
		if tc, ok := c.(ai.TextContent); ok {
			gotText = tc.Text
		}
	}
	if gotText != "hello world this is a longer message to force multiple deltas" {
		t.Fatalf("text not assembled correctly: %q", gotText)
	}
}

func TestFauxProviderUnknownApiRegisteredAndRemoved(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{Api: "faux-test-x"})
	if _, ok := ai.GetApiProvider("faux-test-x"); !ok {
		t.Fatal("provider not registered")
	}
	reg.Unregister()
	if _, ok := ai.GetApiProvider("faux-test-x"); ok {
		t.Fatal("provider not unregistered")
	}
}

func TestFauxProviderCachePrefix(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]FauxResponseStep{
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("a")}, ai.StopStop)),
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("b")}, ai.StopStop)),
	})
	model := reg.GetModel()
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{SessionID: "s1", CacheRetention: ai.CacheShort}}

	req := ai.Context{Messages: []ai.Message{ai.NewUserText("the quick brown fox jumps", 1)}}
	first := ai.StreamSimple(context.Background(), model, req, opts).Result()
	if first.Usage.CacheWrite == 0 {
		t.Fatalf("first call should write cache, got %+v", first.Usage)
	}
	// Same prefix on next call should produce cache reads.
	second := ai.StreamSimple(context.Background(), model, req, opts).Result()
	if second.Usage.CacheRead == 0 {
		t.Fatalf("second call should read cache, got %+v", second.Usage)
	}
}
