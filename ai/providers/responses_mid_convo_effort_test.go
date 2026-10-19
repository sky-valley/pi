package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// midConvoEffortCaptureFile is what testdata/mid-convo-effort/capture.mjs
// recorded from pi-ai 1.3.0's openai-responses stream, replaying upstream's
// openai-responses-mid-convo-effort.test.ts: each run's model, messages,
// options and response body, and the request it sent and the reply it built.
const midConvoEffortCaptureFile = "testdata/mid-convo-effort/mid-convo-effort-1.3.0.json"

// recordingDoer serves body to every request and keeps the last request's
// body.
type recordingDoer struct {
	body    string
	request *[]byte
}

func (d recordingDoer) Do(req *http.Request) (*http.Response, error) {
	sent, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	*d.request = sent
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(d.body)),
		Request:    req,
	}, nil
}

// With compat.supportsMidConvoEffort, an API key and a reasoning effort, the
// request keeps reasoning.effort at "medium" and sends the level as
// configuration_update input items: one before each earlier reply of this
// model that recorded its level, one at the end. The reply records the level
// as providerThinkingLevel, so earlier input never changes (upstream
// 60ac3bcc3). Sign in with ChatGPT, the flag off, and no effort send
// reasoning.effort as before and record nothing.
func TestResponsesMidConvoEffortMatchesPi(t *testing.T) {
	data, err := os.ReadFile(midConvoEffortCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Runs []struct {
			Name            string            `json:"name"`
			Model           ai.Model          `json:"model"`
			Messages        []json.RawMessage `json:"messages"`
			APIKey          string            `json:"apiKey"`
			ReasoningEffort *string           `json:"reasoningEffort"`
			Body            string            `json:"body"`
			Payload         json.RawMessage   `json:"payload"`
			Message         struct {
				ProviderThinkingLevel string `json:"providerThinkingLevel"`
			} `json:"message"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", midConvoEffortCaptureFile, err)
	}
	if len(capture.Runs) == 0 {
		t.Fatalf("%s holds no runs; rerun capture.mjs", midConvoEffortCaptureFile)
	}
	for _, run := range capture.Runs {
		var messages []ai.Message
		for _, raw := range run.Messages {
			m, err := ai.UnmarshalMessage(raw)
			if err != nil {
				t.Fatalf("%s: message %s: %v", run.Name, raw, err)
			}
			messages = append(messages, m)
		}
		var sent []byte
		opts := &OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey:     run.APIKey,
			HTTPClient: recordingDoer{body: run.Body, request: &sent},
		}}}
		if run.ReasoningEffort != nil {
			opts.ReasoningEffort = *run.ReasoningEffort
		}
		model := run.Model
		final := StreamOpenAIResponses(context.Background(), &model, ai.NormalizeContext(ai.Context{Messages: messages}), opts).Result()
		if final.StopReason == ai.StopError {
			t.Errorf("%s: stream failed: %s", run.Name, final.ErrorMessage)
			continue
		}
		var got, want any
		if err := json.Unmarshal(sent, &got); err != nil {
			t.Fatalf("%s: request body %q: %v", run.Name, sent, err)
		}
		_ = json.Unmarshal(run.Payload, &want)
		if !reflect.DeepEqual(got, want) {
			var compact bytes.Buffer
			_ = json.Compact(&compact, run.Payload)
			t.Errorf("%s: request\n%s\npi\n%s", run.Name, sent, compact.String())
		}
		if final.ProviderThinkingLevel != run.Message.ProviderThinkingLevel {
			t.Errorf("%s: providerThinkingLevel %q, pi %q", run.Name, final.ProviderThinkingLevel, run.Message.ProviderThinkingLevel)
		}
	}
}
