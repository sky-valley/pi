package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// attrDoneSSE is a minimal Chat Completions stream that ends at once.
const attrDoneSSE = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// captureOpenAICompletionsHeaders runs the openai-completions adapter against a
// stub server and returns the request headers. BaseURL is overwritten with the
// test server, so only the provider name reaches a target test.
func captureOpenAICompletionsHeaders(t *testing.T, provider ai.ProviderId, sessionID string, optsHeaders ai.ProviderHeaders) http.Header {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, attrDoneSSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: provider + "-test", Api: ai.APIOpenAICompletions, Provider: provider, BaseURL: server.URL,
		Input: []string{"text"}, MaxTokens: 4096,
	}
	opts := &OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: optsHeaders}, SessionID: sessionID}}
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}), opts).Result()
	return got
}
