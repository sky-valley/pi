package providers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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
	var startStop ai.StopReason
	for e := range stream.Events() {
		switch e.Type {
		case ai.EventStart:
			sawStart = true
			if e.Partial != nil {
				startStop = e.Partial.StopReason
			}
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
	// Upstream f9a49869: even though the resolved message is a normal stop, the
	// in-flight partial reports pending until the terminal event.
	if startStop != ai.StopPending {
		t.Fatalf("in-flight partial should be pending, got %q", startStop)
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

// Upstream f9a49869: a faux response whose stop reason was never resolved
// (still "pending") is a test-authoring error — the stream fails rather than
// emitting done, and the in-flight partial carries the pending reason.
func TestFauxProviderPendingStopReasonFailsStream(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()

	// Build the message directly so we bypass FauxAssistantMessage's "" → stop
	// coercion and leave the stop reason genuinely pending.
	pending := &ai.AssistantMessage{
		Content:    ai.ContentList{FauxText("half a thought")},
		Api:        reg.Api,
		Provider:   reg.Models[0].Provider,
		Model:      reg.Models[0].ID,
		StopReason: ai.StopPending,
	}
	reg.SetResponses([]FauxResponseStep{FauxStatic(pending)})

	model := reg.GetModel()
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	stream := ai.StreamSimple(context.Background(), model, req, nil)

	var startPartialStop ai.StopReason
	sawStart, sawDone, sawError := false, false, false
	for e := range stream.Events() {
		switch e.Type {
		case ai.EventStart:
			sawStart = true
			if e.Partial != nil {
				startPartialStop = e.Partial.StopReason
			}
		case ai.EventDone:
			sawDone = true
		case ai.EventError:
			sawError = true
		}
	}
	final := stream.Result()

	if !sawStart || startPartialStop != ai.StopPending {
		t.Fatalf("in-flight partial should be pending, got start=%v stop=%q", sawStart, startPartialStop)
	}
	if sawDone || !sawError {
		t.Fatalf("pending faux response must error, not complete: done=%v error=%v", sawDone, sawError)
	}
	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	if final.ErrorMessage != "Faux response ended without a stop reason" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
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

// TestFauxProviderCacheCountsToFirstDifference mirrors pi faux-provider.test.ts
// "counts cached characters up to the first difference in the joined prompt"
// (upstream fe11328b0, which compares the prompts message by message and the
// characters only from the first differing one; the numbers are unchanged).
func TestFauxProviderCacheCountsToFirstDifference(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]FauxResponseStep{
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("a")}, ai.StopStop)),
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("b")}, ai.StopStop)),
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("c")}, ai.StopStop)),
	})
	model := reg.GetModel()
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{SessionID: "session-1", CacheRetention: ai.CacheShort}}
	complete := func(texts ...string) ai.Usage {
		messages := make([]ai.Message, len(texts))
		for i, text := range texts {
			messages[i] = ai.NewUserText(text, 1)
		}
		return ai.StreamSimple(context.Background(), model, ai.Context{Messages: messages}, opts).Result().Usage
	}

	// Prompt texts: "user:hello world" (16 characters), then 16 + 2 + "user:next" (9) = 27.
	complete("hello world")
	if got := complete("hello world", "next"); got.Input != 3 || got.CacheRead != 4 || got.CacheWrite != 3 {
		t.Fatalf("extended usage = %+v, want input 3, cacheRead 4, cacheWrite 3", got)
	}
	// The first message now differs after "user:hello w" (12 characters): "user:hello wide" + 2 + 9 = 26.
	if got := complete("hello wide", "next"); got.Input != 4 || got.CacheRead != 3 || got.CacheWrite != 4 {
		t.Fatalf("edited usage = %+v, want input 4, cacheRead 3, cacheWrite 4", got)
	}
}

// TestFauxProviderDeferredResponses locks pi 382aa641c's faux half: a
// submission that asks for deferral streams an empty "deferred" message
// carrying a handle, fetches answer with that same handle while the script
// says the response is still pending, and the next fetch redeems the scripted
// response.
func TestFauxProviderDeferredResponses(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{
		Deferred: &FauxDeferredOptions{PendingFetches: 1, PollAfterMs: 25},
	})
	defer reg.Unregister()
	reg.SetResponses([]FauxResponseStep{
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("ready")}, ai.StopStop)),
	})

	model := reg.GetModel()
	api, ok := ai.GetApiProvider(model.Api)
	if !ok || api.FetchDeferred == nil || api.CancelDeferred == nil {
		t.Fatal("the faux api must register both deferred entry points")
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	submission := ai.StreamSimple(context.Background(), model, req,
		&ai.SimpleStreamOptions{Deferred: &ai.DeferredRequest{Window: ai.DeferredWindow1h}})
	var types []ai.EventType
	for e := range submission.Events() {
		types = append(types, e.Type)
	}
	accepted := submission.Result()
	if len(types) != 2 || types[0] != ai.EventStart || types[1] != ai.EventDone {
		t.Fatalf("a deferred submission streams no content, got %v", types)
	}
	if accepted.StopReason != ai.StopDeferred || len(accepted.Content) != 0 {
		t.Fatalf("submission = %q with %d blocks, want an empty deferred message", accepted.StopReason, len(accepted.Content))
	}
	if accepted.Deferred == nil {
		t.Fatal("a deferred submission must carry a handle")
	}
	handle := *accepted.Deferred
	if handle.Provider != model.Provider || handle.ModelID != model.ID || handle.Api != model.Api || handle.ID == "" {
		t.Fatalf("handle does not identify the request: %+v", handle)
	}
	if handle.PollAfterMs != 25 {
		t.Fatalf("handle pollAfterMs = %d, want the scripted 25", handle.PollAfterMs)
	}

	// The scripted pending fetch answers with the same handle, not the response.
	pending := api.FetchDeferred(context.Background(), model, handle, &ai.DeferredFetchOptions{}).Result()
	if pending.StopReason != ai.StopDeferred || pending.Deferred == nil || pending.Deferred.ID != handle.ID {
		t.Fatalf("a pending fetch must return the handle again, got %q", pending.StopReason)
	}

	ready := api.FetchDeferred(context.Background(), model, handle, &ai.DeferredFetchOptions{}).Result()
	if ready.StopReason != ai.StopStop || len(ready.Content) != 1 {
		t.Fatalf("the next fetch must redeem the response, got %q (%s)", ready.StopReason, ready.ErrorMessage)
	}
	if text := ready.Content[0].(ai.TextContent).Text; text != "ready" {
		t.Fatalf("redeemed text = %q", text)
	}
	if ready.Usage.TotalTokens == 0 {
		t.Fatal("a redeemed response must be usage-estimated like a streamed one")
	}
	// The submission consumed one scripted response; both fetches were counted.
	if reg.State.CallCount != 1 || reg.State.DeferredFetchCount != 2 {
		t.Fatalf("state = callCount %d, deferredFetchCount %d; want 1 and 2",
			reg.State.CallCount, reg.State.DeferredFetchCount)
	}
}

// TestFauxProviderDeferredCancelAndFailures locks the faux's failure surface:
// cancellation is recorded and makes later fetches fail, and an unknown handle
// fails in-band rather than panicking.
func TestFauxProviderDeferredCancelAndFailures(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]FauxResponseStep{
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("cancelled")}, ai.StopStop)),
	})

	model := reg.GetModel()
	api, _ := ai.GetApiProvider(model.Api)
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	accepted := ai.StreamSimple(context.Background(), model, req,
		&ai.SimpleStreamOptions{Deferred: &ai.DeferredRequest{}}).Result()
	if accepted.Deferred == nil {
		t.Fatalf("bare deferral must still produce a handle, got %q", accepted.StopReason)
	}
	handle := *accepted.Deferred

	if err := api.CancelDeferred(context.Background(), model, handle, nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(reg.State.CancelledDeferred) != 1 || reg.State.CancelledDeferred[0].ID != handle.ID {
		t.Fatalf("cancellation not recorded: %+v", reg.State.CancelledDeferred)
	}
	cancelled := api.FetchDeferred(context.Background(), model, handle, &ai.DeferredFetchOptions{}).Result()
	if cancelled.StopReason != ai.StopError || !strings.Contains(cancelled.ErrorMessage, "was cancelled") {
		t.Fatalf("fetching a cancelled response must fail in-band, got %q (%s)",
			cancelled.StopReason, cancelled.ErrorMessage)
	}

	unknown := api.FetchDeferred(context.Background(), model,
		ai.DeferredHandle{Provider: model.Provider, ModelID: model.ID, Api: model.Api, ID: "nope"},
		&ai.DeferredFetchOptions{}).Result()
	if unknown.StopReason != ai.StopError || !strings.Contains(unknown.ErrorMessage, "Unknown faux deferred response") {
		t.Fatalf("an unknown handle must fail in-band, got %q (%s)", unknown.StopReason, unknown.ErrorMessage)
	}
}

// TestFauxProviderRedeemsWithSubmissionOptions locks upstream 686f193e5: the
// scripted step that produces a redeemed response sees the options the
// submission was made with, and the fetch's own options no longer override
// them. Before that change the faux spread `{...submissionOptions,
// ...fetchOptions}`, so a fetch could restate the request the provider had
// already accepted; a deferred fetch now carries request options only, and the
// ones it does carry (auth, headers, HTTP) belong to the fetch's own HTTP call.
// The submission's deferral request and response hook stay stripped, as they
// did before: the step must not re-request deferral, and the submission's hook
// was already fired.
func TestFauxProviderRedeemsWithSubmissionOptions(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()

	var seen *ai.SimpleStreamOptions
	reg.SetResponses([]FauxResponseStep{
		func(_ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *FauxState, _ *ai.Model) *ai.AssistantMessage {
			seen = opts
			return FauxAssistantMessage(ai.ContentList{FauxText("ready")}, ai.StopStop)
		},
	})

	model := reg.GetModel()
	api, _ := ai.GetApiProvider(model.Api)
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}

	submissionTokens := 512
	submissionResponses := 0
	accepted := ai.StreamSimple(context.Background(), model, req, &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:  "submission-key",
				Headers: ai.ProviderHeaders{"X-Origin": ai.HeaderValue("submission")},
				OnResponse: func(ai.ProviderResponse, *ai.Model) error {
					submissionResponses++
					return nil
				},
			},
			MaxTokens: &submissionTokens,
			SessionID: "submission-session",
		},
		Reasoning: ai.ThinkingHigh,
		Deferred:  &ai.DeferredRequest{Window: ai.DeferredWindow1h},
	}).Result()
	if accepted.Deferred == nil {
		t.Fatalf("submission must produce a handle, got %q", accepted.StopReason)
	}
	seen = nil
	afterSubmission := submissionResponses

	ready := api.FetchDeferred(context.Background(), model, *accepted.Deferred, &ai.DeferredFetchOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey:  "fetch-key",
			Headers: ai.ProviderHeaders{"X-Origin": ai.HeaderValue("fetch")},
		},
	}).Result()
	if ready.StopReason != ai.StopStop {
		t.Fatalf("fetch must redeem the response, got %q (%s)", ready.StopReason, ready.ErrorMessage)
	}
	if seen == nil {
		t.Fatal("redemption must run the scripted step")
	}
	if seen.APIKey != "submission-key" {
		t.Errorf("step saw apiKey %q, want the submission's; the fetch's options must not override it", seen.APIKey)
	}
	if origin := seen.Headers["X-Origin"]; origin == nil || *origin != "submission" {
		t.Errorf("step saw X-Origin %v, want the submission's header", origin)
	}
	if seen.MaxTokens == nil || *seen.MaxTokens != submissionTokens {
		t.Errorf("step saw maxTokens %v, want the submission's %d", seen.MaxTokens, submissionTokens)
	}
	if seen.SessionID != "submission-session" || seen.Reasoning != ai.ThinkingHigh {
		t.Errorf("step saw sessionId %q / reasoning %q, want the submission's", seen.SessionID, seen.Reasoning)
	}
	if seen.Deferred != nil {
		t.Error("the deferral request must be stripped before redemption")
	}
	if seen.OnResponse != nil {
		t.Error("the submission's OnResponse must be stripped before redemption")
	}
	if submissionResponses != afterSubmission {
		t.Errorf("the submission's OnResponse fired %d extra times during redemption",
			submissionResponses-afterSubmission)
	}
}

// TestFauxAssistantMessageOmitsUnsetOptionalFields pins pi 86bac52f9: a faux
// assistant message must not carry deferred/errorMessage/responseId keys it was
// never given. Upstream stopped assigning `undefined` and spreads them
// conditionally instead, because an explicit `"errorMessage": undefined`
// survives into serialized session JSON as a present-but-empty field. The Go
// port answers the same question with omitempty on the zero value, so this is a
// regression pin on the serialized shape rather than a change.
func TestFauxAssistantMessageOmitsUnsetOptionalFields(t *testing.T) {
	msg := FauxAssistantMessage(ai.ContentList{FauxText("hi")}, ai.StopStop)
	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"deferred", "errorMessage", "responseId"} {
		if raw, present := fields[key]; present {
			t.Errorf("%s must be absent, got %s", key, raw)
		}
	}
	if string(fields["stopReason"]) != `"stop"` {
		t.Errorf("stopReason = %s, want \"stop\"", fields["stopReason"])
	}
}

// TestFauxAssistantMessageKeepsSetOptionalFields is the other half of the pin
// above. Absence assertions alone are satisfied by a field that no longer
// serializes at all, so they would survive dropping the key (`json:"-"`) — the
// same session-file regression seen from the other side. A message that DOES
// carry the three fields must still write them under pi's own key names
// (types.ts:434,440,441: responseId, deferred, errorMessage).
func TestFauxAssistantMessageKeepsSetOptionalFields(t *testing.T) {
	msg := FauxAssistantMessage(ai.ContentList{FauxText("hi")}, ai.StopError)
	msg.ErrorMessage = "boom"
	msg.ResponseID = "resp_1"
	msg.Deferred = &ai.DeferredHandle{Provider: "faux", ModelID: "faux-1", Api: "faux", ID: "def_1"}

	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]string{
		"errorMessage": `"boom"`,
		"responseId":   `"resp_1"`,
	} {
		if got := string(fields[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	var handle ai.DeferredHandle
	if raw, present := fields["deferred"]; !present {
		t.Errorf("deferred must be present when the message carries a handle")
	} else if err := json.Unmarshal(raw, &handle); err != nil {
		t.Errorf("deferred: %v", err)
	} else if handle.ID != "def_1" {
		t.Errorf("deferred.id = %q, want def_1", handle.ID)
	}
}

// TestFauxProviderAbortedStreamMatchesPi: pi's faux provider ends a stream
// whose signal has aborted by the time it would start — before the call, or
// from onResponse — with one error event (reason aborted), stopReason
// aborted, "Request was aborted" and no content, and the scripted step it
// took stays consumed (faux.ts createAbortedMessage, run under node at
// 49681e1b7).
func TestFauxProviderAbortedStreamMatchesPi(t *testing.T) {
	for _, how := range []string{"already aborted", "onResponse aborts"} {
		t.Run(how, func(t *testing.T) {
			reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
			defer reg.Unregister()
			reg.SetResponses([]FauxResponseStep{FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("hello there")}, ai.StopStop))})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := &ai.SimpleStreamOptions{}
			if how == "already aborted" {
				cancel()
			} else {
				opts.OnResponse = func(ai.ProviderResponse, *ai.Model) error {
					cancel()
					return nil
				}
			}
			stream := ai.StreamSimple(ctx, reg.GetModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts)
			var events []string
			for ev := range stream.Events() {
				events = append(events, string(ev.Type)+":"+string(ev.Reason))
			}
			msg := stream.Result()
			if !slices.Equal(events, []string{"error:aborted"}) {
				t.Errorf("events %v, pi [error:aborted]", events)
			}
			if msg.StopReason != ai.StopAborted || msg.ErrorMessage != "Request was aborted" || len(msg.Content) != 0 {
				t.Errorf("stop %s %q, content %v; pi aborted \"Request was aborted\", []", msg.StopReason, msg.ErrorMessage, msg.Content)
			}
			if reg.PendingResponseCount() != 0 || reg.State.CallCount != 1 {
				t.Errorf("pending %d, calls %d; pi 0, 1", reg.PendingResponseCount(), reg.State.CallCount)
			}
		})
	}
}

// TestFauxProviderOnResponseErrorFailsStream locks pi's faux provider awaiting
// onResponse inside each call's try (faux.ts:512 stream, :579 fetchDeferred,
// :643 cancelDeferred at 8676a0dcd). Measured by running that source under
// node: a throw from stream's onResponse pushes only an error event — reason
// "error" even when the request was aborted, the thrown message, empty content
// and the default usage — and the scripted step it took stays consumed; a
// throw from fetchDeferred's fails that fetch alone, and the deferred response
// is still redeemable; a throw from cancelDeferred's rejects the cancel, which
// is recorded all the same.
func TestFauxProviderOnResponseErrorFailsStream(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]FauxResponseStep{
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("one")}, ai.StopStop)),
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("two")}, ai.StopStop)),
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("three")}, ai.StopStop)),
	})
	model := reg.GetModel()
	api, _ := ai.GetApiProvider(model.Api)
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	veto := func(msg string) func(ai.ProviderResponse, *ai.Model) error {
		return func(ai.ProviderResponse, *ai.Model) error { return errors.New(msg) }
	}
	collect := func(s *ai.AssistantMessageEventStream) ([]ai.EventType, *ai.AssistantMessage) {
		var types []ai.EventType
		for ev := range s.Events() {
			types = append(types, ev.Type)
		}
		return types, s.Result()
	}
	assertFailed := func(label string, types []ai.EventType, msg *ai.AssistantMessage, want string) {
		t.Helper()
		if !slices.Equal(types, []ai.EventType{ai.EventError}) {
			t.Errorf("%s: pushed %v, want only an error event", label, types)
		}
		if msg.StopReason != ai.StopError || msg.ErrorMessage != want {
			t.Errorf("%s: stop %q (%q), want error (%q)", label, msg.StopReason, msg.ErrorMessage, want)
		}
		if len(msg.Content) != 0 || msg.Usage != (ai.Usage{}) {
			t.Errorf("%s: content %v, usage %+v; want none and the default usage", label, msg.Content, msg.Usage)
		}
	}

	types, msg := collect(ai.StreamSimple(context.Background(), model, req, &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{OnResponse: veto("response veto")}},
	}))
	assertFailed("stream", types, msg, "response veto")
	if reg.PendingResponseCount() != 2 || reg.State.CallCount != 1 {
		t.Errorf("pending %d, calls %d after a vetoed stream; want the step consumed (2, 1)", reg.PendingResponseCount(), reg.State.CallCount)
	}

	aborted, cancel := context.WithCancel(context.Background())
	cancel()
	_, msg = collect(ai.StreamSimple(aborted, model, req, &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{OnResponse: veto("veto aborted")}},
	}))
	if msg.StopReason != ai.StopError || msg.ErrorMessage != "veto aborted" {
		t.Errorf("aborted stream: stop %q (%q), want error (veto aborted)", msg.StopReason, msg.ErrorMessage)
	}

	accepted := ai.StreamSimple(context.Background(), model, req, &ai.SimpleStreamOptions{Deferred: &ai.DeferredRequest{}}).Result()
	if accepted.Deferred == nil {
		t.Fatalf("deferral must produce a handle, got %q (%s)", accepted.StopReason, accepted.ErrorMessage)
	}
	handle := *accepted.Deferred
	types, msg = collect(api.FetchDeferred(context.Background(), model, handle, &ai.DeferredFetchOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{OnResponse: veto("fetch veto")},
	}))
	assertFailed("fetch", types, msg, "fetch veto")
	if redeemed := api.FetchDeferred(context.Background(), model, handle, &ai.DeferredFetchOptions{}).Result(); redeemed.StopReason != ai.StopStop {
		t.Errorf("a vetoed fetch must leave the response redeemable, got %q (%s)", redeemed.StopReason, redeemed.ErrorMessage)
	}

	err := api.CancelDeferred(context.Background(), model, handle, &ai.DeferredCancelOptions{OnResponse: veto("cancel veto")})
	if err == nil || err.Error() != "cancel veto" {
		t.Errorf("cancel returned %v, want the hook's error", err)
	}
	if len(reg.State.CancelledDeferred) != 1 {
		t.Errorf("a vetoed cancel must still be recorded, got %d", len(reg.State.CancelledDeferred))
	}
}
