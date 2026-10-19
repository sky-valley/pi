package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

// abortBeforeResponseRun is one row of
// testdata/abort-before-response/abort-before-response-*.json: how pi's
// adapter ended a stream whose signal aborted before a response arrived.
// capture.mts beside it says how the runs are made.
type abortBeforeResponseRun struct {
	API          string   `json:"api"`
	Mode         string   `json:"mode"`
	Events       []string `json:"events"`
	StopReason   string   `json:"stopReason"`
	ErrorMessage string   `json:"errorMessage"`
	OnPayload    int      `json:"onPayload"`
	Requests     int      `json:"requests"`
}

// serveAbortBeforeResponse is capture.mts's server: it never answers, or it
// answers 503 with retry-after: 5 when retry is set, and counts requests.
func serveAbortBeforeResponse(t *testing.T, retry bool, body string) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					req, err := http.ReadRequest(r)
					if err != nil {
						return
					}
					io.Copy(io.Discard, req.Body)
					requests.Add(1)
					if !retry {
						<-done
						return
					}
					fmt.Fprintf(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nretry-after: 5\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				}
			}()
		}
	}()
	return "http://" + ln.Addr().String(), &requests
}

// TestAbortBeforeResponseMatchesPi replays capture.mts's runs through each
// adapter that sends with sendWithRetry: pi's retryProviderRequest turns an
// abort that ends the request, or its retry wait, into
// Error("Request aborted"), and the google adapter's buildParams throws that
// same message before onPayload when the signal is already aborted. Since
// d4d53612b the anthropic adapter's request loop opens with
// signal.throwIfAborted(), so an already-aborted anthropic stream fails with
// the AbortError's "This operation was aborted" before onPayload.
func TestAbortBeforeResponseMatchesPi(t *testing.T) {
	raw, err := os.ReadFile("testdata/abort-before-response/abort-before-response-d4d53612b.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Runs []abortBeforeResponseRun `json:"runs"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		string(ai.APIAnthropicMessages):  `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
		string(ai.APIOpenAICompletions):  `{"error":{"message":"overloaded","type":"server_error"}}`,
		string(ai.APIOpenAIResponses):    `{"error":{"message":"overloaded","type":"server_error"}}`,
		string(ai.APIGoogleGenerativeAI): `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`,
	}
	if len(capture.Runs) != 12 {
		t.Fatalf("%d runs in the capture, want 12", len(capture.Runs))
	}
	for _, run := range capture.Runs {
		t.Run(run.API+"/"+run.Mode, func(t *testing.T) {
			baseURL, requests := serveAbortBeforeResponse(t, run.Mode == "an abort during a retry wait", bodies[run.API])
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if run.Mode == "an already-aborted signal" {
				cancel()
			} else {
				time.AfterFunc(200*time.Millisecond, cancel)
			}
			onPayload := 0
			opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-api-key",
				OnPayload: func(any, *ai.Model) (any, error) {
					onPayload++
					return nil, nil
				},
			}}
			if run.Mode == "an abort during a retry wait" {
				opts.MaxRetries = 2
			}
			model := &ai.Model{ID: "m", Api: ai.Api(run.API), BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000}
			req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
			var stream *ai.AssistantMessageEventStream
			switch ai.Api(run.API) {
			case ai.APIAnthropicMessages:
				model.Provider = "anthropic"
				stream = StreamAnthropic(ctx, model, req, &AnthropicOptions{StreamOptions: opts})
			case ai.APIOpenAICompletions:
				model.Provider = "openai"
				stream = StreamOpenAICompletions(ctx, model, req, &OpenAIOptions{StreamOptions: opts})
			case ai.APIOpenAIResponses:
				model.Provider = "openai"
				stream = StreamOpenAIResponses(ctx, model, req, &OpenAIResponsesOptions{StreamOptions: opts})
			case ai.APIGoogleGenerativeAI:
				model.Provider = "google"
				stream = StreamGoogle(ctx, model, req, &GoogleOptions{StreamOptions: opts})
			default:
				t.Fatalf("no Go adapter for %s", run.API)
			}
			var events []string
			for ev := range stream.Events() {
				events = append(events, string(ev.Type))
			}
			final := stream.Result()
			if !slices.Equal(events, run.Events) || string(final.StopReason) != run.StopReason || final.ErrorMessage != run.ErrorMessage {
				t.Errorf("events %v, stop %s %q\npi:    %v, stop %s %q", events, final.StopReason, final.ErrorMessage, run.Events, run.StopReason, run.ErrorMessage)
			}
			if onPayload != run.OnPayload || int(requests.Load()) != run.Requests {
				t.Errorf("OnPayload ran %d times and %d requests arrived; pi %d and %d", onPayload, requests.Load(), run.OnPayload, run.Requests)
			}
		})
	}
}
