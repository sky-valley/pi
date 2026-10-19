package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicOnResponseCaptureFile is written by
// testdata/anthropic-on-response/capture-anthropic-on-response.mts, which
// streams pi's anthropic-messages adapter, with its real SDK client, from
// source at the named sha.
const anthropicOnResponseCaptureFile = "testdata/anthropic-on-response/anthropic-on-response-8676a0dcd.json"

// TestAnthropicOnResponseMatchesPi serves each captured raw response and
// requires pi's onResponse calls, pushed events and outcome: only a 2xx
// response is reported (the SDK throws on any other status first), with pi's
// header record, and an OnResponse error fails the stream with its message
// before anything is pushed — aborted when the request was cancelled.
func TestAnthropicOnResponseMatchesPi(t *testing.T) {
	data, err := os.ReadFile(anthropicOnResponseCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/anthropic-on-response/capture-anthropic-on-response.mts)", anthropicOnResponseCaptureFile, err)
	}
	type call struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
	}
	var capture struct {
		Model string `json:"model"`
		Rows  []struct {
			Name         string        `json:"name"`
			Response     string        `json:"response"`
			Throws       bool          `json:"throws"`
			AbortFirst   bool          `json:"abortFirst"`
			Calls        []call        `json:"calls"`
			Pushed       []string      `json:"pushed"`
			StopReason   ai.StopReason `json:"stopReason"`
			ErrorMessage string        `json:"errorMessage"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", anthropicOnResponseCaptureFile, err)
	}
	if capture.Model != "anthropic/claude-haiku-4-5" || len(capture.Rows) == 0 {
		t.Fatalf("%s: model %q, %d rows", anthropicOnResponseCaptureFile, capture.Model, len(capture.Rows))
	}
	base := ai.GetModel("anthropic", "claude-haiku-4-5")
	if base == nil {
		t.Fatal("catalog has no anthropic/claude-haiku-4-5")
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			model := *base
			model.BaseURL = serveRaw(t, row.Response)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := []call{}
			stream := StreamAnthropic(ctx, &model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}}),
				&AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "sk-ant-api03-test",
					OnResponse: func(resp ai.ProviderResponse, _ *ai.Model) error {
						calls = append(calls, call{Status: resp.Status, Headers: resp.Headers})
						if row.Throws {
							if row.AbortFirst {
								cancel()
							}
							return errors.New("response veto")
						}
						return nil
					},
				}}})
			var pushed []string
			for ev := range stream.Events() {
				pushed = append(pushed, string(ev.Type))
			}
			final := stream.Result()

			if !reflect.DeepEqual(calls, row.Calls) {
				t.Errorf("onResponse calls = %+v, want %+v", calls, row.Calls)
			}
			if !reflect.DeepEqual(pushed, row.Pushed) {
				t.Errorf("pushed = %q, want %q", pushed, row.Pushed)
			}
			if final.StopReason != row.StopReason {
				t.Errorf("stopReason = %s, want %s (%s)", final.StopReason, row.StopReason, final.ErrorMessage)
			}
			if final.ErrorMessage != row.ErrorMessage {
				t.Errorf("errorMessage = %q, want %q", final.ErrorMessage, row.ErrorMessage)
			}
		})
	}
}
