package providers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// providerErrorResponseCaptureFile is written by
// testdata/provider-error-response/capture.mts, which streams pi's SDK-based
// adapters from source at the named sha against a loopback server.
const providerErrorResponseCaptureFile = "testdata/provider-error-response/provider-error-response-8a57d42a5.json"

type providerErrorResponseCall struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Empty   bool              `json:"empty"`
}

type providerErrorResponseCapture struct {
	Attempts map[string]struct {
		API     ai.Api `json:"api"`
		Replies []struct {
			Status      int               `json:"status"`
			ContentType string            `json:"contentType"`
			Body        string            `json:"body"`
			Headers     map[string]string `json:"headers"`
		} `json:"replies"`
		MaxRetries   int                         `json:"maxRetries"`
		Throws       bool                        `json:"throws"`
		Requests     int                         `json:"requests"`
		Calls        []providerErrorResponseCall `json:"calls"`
		SameModel    bool                        `json:"sameModel"`
		Events       []ai.EventType              `json:"events"`
		StopReason   ai.StopReason               `json:"stopReason"`
		ErrorMessage string                      `json:"errorMessage"`
	} `json:"attempts"`
	// Hooks re-captures openai-stream's non-2xx hooks rows, keyed as there.
	Hooks map[string]struct {
		OnResponseStatuses []int         `json:"onResponseStatuses"`
		StopReason         ai.StopReason `json:"stopReason"`
		ErrorMessage       string        `json:"errorMessage"`
	} `json:"hooks"`
}

func loadProviderErrorResponseCapture(t *testing.T) providerErrorResponseCapture {
	t.Helper()
	data, err := os.ReadFile(providerErrorResponseCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/provider-error-response/capture.mts)", providerErrorResponseCaptureFile, err)
	}
	var c providerErrorResponseCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode %s: %v", providerErrorResponseCaptureFile, err)
	}
	if len(c.Attempts) == 0 || len(c.Hooks) == 0 {
		t.Fatalf("%s holds %d attempts and %d hooks rows; rerun capture.mts", providerErrorResponseCaptureFile, len(c.Attempts), len(c.Hooks))
	}
	return c
}

// TestProviderErrorResponsesReachOnResponseLikePi (upstream 8a57d42a5,
// #12376) requires pi's onResponse calls from each SDK-based adapter over a
// reply sequence: every failed attempt is reported, retried or not, before
// the retry; a retried request's success is reported after the failure
// (google reports no success, as before); the error replies' headers are the
// record (google's is empty: @google/genai's ApiError has none); and an
// OnResponse error on a failed attempt fails the request with its message,
// with no retry.
func TestProviderErrorResponsesReachOnResponseLikePi(t *testing.T) {
	c := loadProviderErrorResponseCapture(t)
	for name, row := range c.Attempts {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				reply := row.Replies[min(int(requests.Add(1))-1, len(row.Replies)-1)]
				w.Header().Set("content-type", reply.ContentType)
				for k, v := range reply.Headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(reply.Status)
				_, _ = io.WriteString(w, reply.Body)
			}))
			t.Cleanup(server.Close)
			model := &ai.Model{
				ID: "test-model", Name: "Test Model", Api: row.API, Provider: "test", BaseURL: server.URL,
				Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100,
			}
			calls := []providerErrorResponseCall{}
			sameModel := true
			so := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:     "test-key",
				MaxRetries: row.MaxRetries,
				OnResponse: func(resp ai.ProviderResponse, m *ai.Model) error {
					sameModel = sameModel && m == model
					picked := map[string]string{}
					for _, k := range []string{"content-type", "retry-after", "x-request-id"} {
						if v, ok := resp.Headers[k]; ok {
							picked[k] = v
						}
					}
					calls = append(calls, providerErrorResponseCall{Status: resp.Status, Headers: picked, Empty: len(resp.Headers) == 0})
					if row.Throws {
						return errors.New("response veto")
					}
					return nil
				},
			}}
			req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 0)}})
			var stream *ai.AssistantMessageEventStream
			switch row.API {
			case ai.APIAnthropicMessages:
				stream = StreamAnthropic(t.Context(), model, req, &AnthropicOptions{StreamOptions: so})
			case ai.APIOpenAICompletions:
				stream = StreamOpenAICompletions(t.Context(), model, req, &OpenAIOptions{StreamOptions: so})
			case ai.APIOpenAIResponses:
				stream = StreamOpenAIResponses(t.Context(), model, req, &OpenAIResponsesOptions{StreamOptions: so})
			case ai.APIGoogleGenerativeAI:
				stream = StreamGoogle(t.Context(), model, req, &GoogleOptions{StreamOptions: so})
			default:
				t.Fatalf("no Go adapter for %s", row.API)
			}
			var events []ai.EventType
			for ev := range stream.Events() {
				events = append(events, ev.Type)
			}
			final := stream.Result()

			if got := int(requests.Load()); got != row.Requests {
				t.Errorf("%d requests, pi sent %d", got, row.Requests)
			}
			if !reflect.DeepEqual(calls, row.Calls) {
				t.Errorf("onResponse calls\n%+v\npi\n%+v", calls, row.Calls)
			}
			if !sameModel || !row.SameModel {
				t.Errorf("onResponse got another model (pi same: %v)", row.SameModel)
			}
			if !reflect.DeepEqual(events, row.Events) {
				t.Errorf("events %v, pi %v", events, row.Events)
			}
			if final.StopReason != row.StopReason {
				t.Errorf("stopReason %q, pi %q", final.StopReason, row.StopReason)
			}
			// google's HTTP-error message is not pi's (docs/UPSTREAM.md K18).
			if row.API != ai.APIGoogleGenerativeAI || row.Throws || row.ErrorMessage == "" {
				if final.ErrorMessage != row.ErrorMessage {
					t.Errorf("errorMessage %q, pi %q", final.ErrorMessage, row.ErrorMessage)
				}
			} else if !strings.Contains(final.ErrorMessage, "rate limited") {
				t.Errorf("errorMessage %q does not quote the provider", final.ErrorMessage)
			}
		})
	}
}
