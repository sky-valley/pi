package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// setupErrorTimingCaptureFile is what testdata/setup-error-timing/capture.mjs
// recorded from pi-ai 1.3.0's Models.
const setupErrorTimingCaptureFile = "testdata/setup-error-timing/setup-error-timing-1.3.0.json"

// pi's Models.stream, streamSimple and streamDeferred run their setup —
// provider lookup and applyAuth, credential resolution and transformHeaders
// included — inside lazyStream, which reads the call's start and creates the
// stream before setup runs (models.ts, lazy.ts). A setup failure therefore
// carries the call's start as its timestamp and the setup time as its
// durationMs: a slow credential resolution or header transform that fails
// shows up as a long failure, not an instant one.
func TestModelsSetupErrorTimingMatchesPi(t *testing.T) {
	data, err := os.ReadFile(setupErrorTimingCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name            string   `json:"name"`
			Entry           string   `json:"entry"`
			Setup           string   `json:"setup"`
			WaitMs          int64    `json:"waitMs"`
			Events          []string `json:"events"`
			StopReason      string   `json:"stopReason"`
			ErrorMessage    string   `json:"errorMessage"`
			TimestampOffset int64    `json:"timestampOffset"`
			DurationMs      *int64   `json:"durationMs"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", setupErrorTimingCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", setupErrorTimingCaptureFile)
	}
	never := func() *AssistantMessageEventStream {
		t.Error("the provider was reached")
		return ErrorStream(&Model{}, errors.New("the provider was reached"))
	}
	streams := &ProviderStreams{
		Stream: func(context.Context, *Model, TranscriptContext, *StreamOptions) *AssistantMessageEventStream {
			return never()
		},
		StreamSimple: func(context.Context, *Model, TranscriptContext, *SimpleStreamOptions) *AssistantMessageEventStream {
			return never()
		},
		FetchDeferred: func(context.Context, *Model, DeferredHandle, *DeferredFetchOptions) *AssistantMessageEventStream {
			return never()
		},
	}
	chatModel := func(provider string) *Model {
		return &Model{ID: "m", Name: "M", Api: "test-api", Provider: provider, BaseURL: "http://127.0.0.1:9",
			Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			// pi's own reading of the same run: the error is stamped at the
			// call's start and timed across the wait.
			if row.TimestampOffset > 1 || row.DurationMs == nil || *row.DurationMs < row.WaitMs {
				t.Fatalf("pi recorded offset %d and durationMs %v for a %dms wait; the oracle no longer shows the rule",
					row.TimestampOffset, row.DurationMs, row.WaitMs)
			}
			wait := time.Duration(row.WaitMs) * time.Millisecond
			models := CreateModels(&CreateModelsOptions{AuthContext: fakeAuthContext{}})
			models.SetProvider(CreateProvider(CreateProviderOptions{
				ID: "slow", Name: "slow", Models: []*Model{chatModel("slow")}, API: streams,
				Auth: ProviderAuth{APIKey: &ApiKeyAuth{Resolve: func(context.Context, AuthContext, *Credential) (*AuthResult, error) {
					time.Sleep(wait)
					return nil, nil
				}}},
			}))
			models.SetProvider(CreateProvider(CreateProviderOptions{
				ID: "keyed", Name: "keyed", Models: []*Model{chatModel("keyed")}, API: streams,
				Auth: ProviderAuth{APIKey: &ApiKeyAuth{Resolve: func(context.Context, AuthContext, *Credential) (*AuthResult, error) {
					return &AuthResult{Auth: ModelAuth{APIKey: "k"}}, nil
				}}},
			}))
			var model *Model
			var transforms ModelsRequestTransforms
			switch row.Setup {
			case "slow auth finds no key":
				model = chatModel("slow")
			case "transformHeaders fails after a wait":
				model = chatModel("keyed")
				transforms.TransformHeaders = func(ProviderHeaders) (ProviderHeaders, error) {
					time.Sleep(wait)
					return nil, errors.New("headers refused")
				}
			default:
				t.Fatalf("unknown setup %q", row.Setup)
			}
			req := Context{Messages: []Message{NewUserText("hi", 1)}}
			callStart := time.Now().UnixMilli()
			var stream *AssistantMessageEventStream
			switch row.Entry {
			case "stream":
				stream = models.Stream(context.Background(), model, req, &ModelsStreamOptions{ModelsRequestTransforms: transforms})
			case "streamSimple":
				stream = models.StreamSimple(context.Background(), model, req, &ModelsSimpleStreamOptions{ModelsRequestTransforms: transforms})
			case "streamDeferred":
				stream = models.StreamDeferred(context.Background(), model, DeferredHandle{ID: "h"}, &ModelsDeferredFetchOptions{ModelsRequestTransforms: transforms})
			default:
				t.Fatalf("unknown entry %q", row.Entry)
			}
			var events []string
			for ev := range stream.Events() {
				events = append(events, string(ev.Type))
			}
			final := stream.Result()
			if !slices.Equal(events, row.Events) || string(final.StopReason) != row.StopReason || final.ErrorMessage != row.ErrorMessage {
				t.Fatalf("events %v, %s %q; pi %v, %s %q", events, final.StopReason, final.ErrorMessage, row.Events, row.StopReason, row.ErrorMessage)
			}
			// pi's offset is 0 or 1; the margin only absorbs a slow scheduler
			// (-race), while a stamp taken after the wait would be wait ms late.
			if offset := final.Timestamp - callStart; offset < 0 || offset >= row.WaitMs/2 {
				t.Errorf("timestamp is %dms after the call, pi stamps the call's start (offset %d)", offset, row.TimestampOffset)
			}
			if final.DurationMs == nil || *final.DurationMs < row.WaitMs {
				got := "none"
				if final.DurationMs != nil {
					got = strconv.FormatInt(*final.DurationMs, 10)
				}
				t.Errorf("durationMs %s, pi times the %dms setup (%d)", got, row.WaitMs, *row.DurationMs)
			}
		})
	}
}
