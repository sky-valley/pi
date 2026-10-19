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
	"sync"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicInlineToolsCaptureFile is what testdata/anthropic-inline-tools/
// capture.mjs recorded from pi-ai 1.3.0's anthropic-messages streamSimple (src
// at b271b0a52 records the same file; at b271b0a52^ every row but the one
// without an initial tool moves): each row's model, messages and key, the
// params onPayload saw, and the request's anthropic-beta header and body.
const anthropicInlineToolsCaptureFile = "testdata/anthropic-inline-tools/inline-tools-1.3.0.json"

// wireRequest is one request as it reached the server.
type wireRequest struct {
	url    string
	header http.Header
	body   []byte
}

// wireResponse is one canned reply of a scriptedDoer.
type wireResponse struct {
	status int
	body   string
}

// scriptedDoer answers each request with the next response (the last one
// repeating) and records every request it was sent.
type scriptedDoer struct {
	mu        sync.Mutex
	responses []wireResponse
	requests  []wireRequest
}

func (d *scriptedDoer) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, wireRequest{url: req.URL.String(), header: req.Header.Clone(), body: body})
	r := d.responses[min(len(d.requests), len(d.responses))-1]
	contentType := "text/event-stream"
	if r.status != http.StatusOK {
		contentType = "application/json"
	}
	return &http.Response{
		StatusCode: r.status,
		Status:     http.StatusText(r.status),
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

// sameJSON reports whether two JSON texts hold the same value.
func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

func compactJSON(raw []byte) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// decodeCapturedMessages decodes a capture's pi messages.
func decodeCapturedMessages(t *testing.T, name string, raws []json.RawMessage) []ai.Message {
	t.Helper()
	messages := make([]ai.Message, 0, len(raws))
	for _, raw := range raws {
		m, err := ai.UnmarshalMessage(raw)
		if err != nil {
			t.Fatalf("%s: message %s: %v", name, raw, err)
		}
		messages = append(messages, m)
	}
	return messages
}

// Native tool changes define every later tool by value: the top-level list is
// the initial tools plus the deferred placeholder and never changes, each
// tool_addition carries {type: "tool_definition", definition} converted like a
// top-level tool (Claude Code names under OAuth, eager streaming, strict), a
// removal is skipped for a name the same update re-adds, and the beta is
// inline-tools-2026-09-15 (upstream b271b0a52). A same-name redefinition no
// longer falls back to resending the current tool list.
func TestAnthropicInlineToolsMatchPi(t *testing.T) {
	data, err := os.ReadFile(anthropicInlineToolsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name     string            `json:"name"`
			Model    ai.Model          `json:"model"`
			Messages []json.RawMessage `json:"messages"`
			APIKey   string            `json:"apiKey"`
			Payload  json.RawMessage   `json:"payload"`
			Request  struct {
				AnthropicBeta *string         `json:"anthropicBeta"`
				Body          json.RawMessage `json:"body"`
			} `json:"request"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", anthropicInlineToolsCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", anthropicInlineToolsCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			messages := decodeCapturedMessages(t, row.Name, row.Messages)
			doer := &scriptedDoer{responses: []wireResponse{{http.StatusOK, anthropicSSE}}}
			var payload []byte
			opts := &ai.SimpleStreamOptions{}
			opts.APIKey = row.APIKey
			opts.HTTPClient = doer
			opts.OnPayload = func(p any, _ *ai.Model) (any, error) {
				payload, err = json.Marshal(p)
				return nil, err
			}
			model := row.Model
			final := StreamSimpleAnthropic(context.Background(), &model, ai.NormalizeContext(ai.Context{Messages: messages}), opts).Result()
			if final.StopReason == ai.StopError || len(doer.requests) != 1 {
				t.Fatalf("stream ended %s (%q) after %d requests", final.StopReason, final.ErrorMessage, len(doer.requests))
			}
			if !sameJSON(t, payload, row.Payload) {
				t.Errorf("onPayload params\n%s\npi\n%s", payload, compactJSON(row.Payload))
			}
			sent := doer.requests[0]
			if !sameJSON(t, sent.body, row.Request.Body) {
				t.Errorf("request body\n%s\npi\n%s", sent.body, compactJSON(row.Request.Body))
			}
			values, present := sent.header["Anthropic-Beta"]
			switch {
			case row.Request.AnthropicBeta == nil && present:
				t.Errorf("anthropic-beta = %q, pi sends none", values)
			case row.Request.AnthropicBeta != nil && strings.Join(values, ", ") != *row.Request.AnthropicBeta:
				t.Errorf("anthropic-beta = %q, pi %q", values, *row.Request.AnthropicBeta)
			}
		})
	}
}
