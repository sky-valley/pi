package providers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicWholeSSE is a complete anthropic stream: the 205 rows' server sends
// it, and net/http reads it where fetch gives a 205 no body.
const anthropicWholeSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hi\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// A response whose body is null fails pi's anthropic and pi-messages loops
// with their own texts. fetch gives 204 and 205 a null body whatever the
// server sends, and a custom fetch can answer `new Response(null, {status})`
// at any status, which a custom HTTPClient's nil Body stands for. anthropic's
// iterateAnthropicEvents throws after onResponse and start; pi-messages
// throws after onResponse, before any event. A non-2xx null body reads as an
// empty one. Measured against pi at 2b0a123de (node v26.4.0): every row's
// onResponse statuses, events and message are pi's.
func TestNullBodyAnthropicAndPiMessagesLikePi(t *testing.T) {
	const anthropicNoBody = "Attempted to iterate over an Anthropic response with no body"
	const piMessagesNoBody = "radius response has no body" // `${model.provider} response has no body`
	start, fail := ai.EventStart, ai.EventError
	for _, tc := range []struct {
		name       string
		api        ai.Api
		status     int
		body       string
		custom     bool
		onResponse []int
		events     []ai.EventType
		message    string // "" leaves the message unchecked
	}{
		{"anthropic/204", ai.APIAnthropicMessages, 204, "", false, []int{204}, []ai.EventType{start, fail}, anthropicNoBody},
		{"anthropic/205 with a whole stream", ai.APIAnthropicMessages, 205, anthropicWholeSSE, false, []int{205}, []ai.EventType{start, fail}, anthropicNoBody},
		{"anthropic/custom null body", ai.APIAnthropicMessages, 200, "", true, []int{200}, []ai.EventType{start, fail}, anthropicNoBody},
		{"anthropic/custom null body at 500", ai.APIAnthropicMessages, 500, "", true, nil, []ai.EventType{fail}, "500 status code (no body)"},
		{"pi-messages/204", ai.APIPiMessages, 204, "", false, []int{204}, []ai.EventType{fail}, piMessagesNoBody},
		{"pi-messages/205 with a whole stream", ai.APIPiMessages, 205, piMessagesSSE(`{"type":"start"}`, `{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`), false, []int{205}, []ai.EventType{fail}, piMessagesNoBody},
		{"pi-messages/custom null body", ai.APIPiMessages, 200, "", true, []int{200}, []ai.EventType{fail}, piMessagesNoBody},
		{"pi-messages/custom null body at 500", ai.APIPiMessages, 500, "", true, []int{500}, []ai.EventType{fail}, "500 : "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()

			var statuses []int
			opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "k",
				OnResponse: func(r ai.ProviderResponse, _ *ai.Model) error {
					statuses = append(statuses, r.Status)
					return nil
				},
			}}
			if tc.custom {
				opts.HTTPClient = nullBodyDoer(tc.status)
			}
			req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
			var stream *ai.AssistantMessageEventStream
			if tc.api == ai.APIAnthropicMessages {
				model := anthropicPlainModel()
				model.BaseURL = server.URL
				stream = StreamAnthropic(t.Context(), model, req, &AnthropicOptions{StreamOptions: opts})
			} else {
				stream = StreamPiMessages(t.Context(), piMessagesTestModel(server.URL+"/v1"), req, &PiMessagesOptions{StreamOptions: opts})
			}
			var events []ai.EventType
			for e := range stream.Events() {
				events = append(events, e.Type)
			}
			final := stream.Result()

			if !slices.Equal(statuses, tc.onResponse) {
				t.Errorf("onResponse statuses = %v, pi's %v", statuses, tc.onResponse)
			}
			if !slices.Equal(events, tc.events) {
				t.Errorf("events = %v, pi's %v", events, tc.events)
			}
			if final.StopReason != ai.StopError {
				t.Errorf("stopReason = %s (%q), pi's error", final.StopReason, final.ErrorMessage)
			}
			if tc.message != "" && final.ErrorMessage != tc.message {
				t.Errorf("errorMessage = %q, pi's %q", final.ErrorMessage, tc.message)
			}
		})
	}
}
