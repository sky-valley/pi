package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// azureRecordingDoer records every request a stream hands its custom client,
// URL and Authorization, and answers 400 so nothing streams.
type azureRecordingDoer struct{ sent *[]string }

func (d azureRecordingDoer) Do(req *http.Request) (*http.Response, error) {
	*d.sent = append(*d.sent, req.URL.String()+" "+req.Header.Get("Authorization"))
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stop here"}}`)),
		Request:    req,
	}, nil
}

// pi serves an azure model on openai-completions through azureStreams
// (providers/azure.ts, upstream a37306d43), which resolves the Azure endpoint
// and deployment before the shared Chat Completions stream runs; the catalog
// ships those models with an empty baseUrl. The port has no such wrapper yet
// (Scope queue row 4), and without it the empty baseUrl falls through to the
// OpenAI SDK default — the Azure key would go to api.openai.com. Until the
// wrapper lands, the stream must fail before any request leaves, and say what
// to do instead.
func TestAzureChatCompletionsModelSendsNothing(t *testing.T) {
	model := &ai.Model{
		ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Api: ai.APIOpenAICompletions,
		Provider: "azure", BaseURL: "", Reasoning: true, Input: []string{"text"},
		ContextWindow: 1000000, MaxTokens: 384000,
	}
	req := ai.TranscriptContext{Messages: []ai.Message{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}}}}
	for name, run := range map[string]func(ai.HTTPDoer) *ai.AssistantMessageEventStream{
		"stream": func(doer ai.HTTPDoer) *ai.AssistantMessageEventStream {
			return StreamOpenAICompletions(context.Background(), model, req, &OpenAIOptions{StreamOptions: ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "azure-key", HTTPClient: doer},
			}})
		},
		"streamSimple": func(doer ai.HTTPDoer) *ai.AssistantMessageEventStream {
			return StreamSimpleOpenAICompletions(context.Background(), model, req, &ai.SimpleStreamOptions{
				StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "azure-key", HTTPClient: doer}},
				Reasoning:     "high",
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			var sent []string
			result := run(azureRecordingDoer{&sent}).Result()
			if len(sent) != 0 {
				t.Fatalf("azure/%s sent %q; it must fail before any request leaves", model.ID, sent)
			}
			if result.StopReason != ai.StopError {
				t.Fatalf("stopReason = %q, want error", result.StopReason)
			}
			for _, want := range []string{"azure/deepseek-v4-pro", "another provider", "/openai/v1"} {
				if !strings.Contains(result.ErrorMessage, want) {
					t.Errorf("error %q does not name %q", result.ErrorMessage, want)
				}
			}
		})
	}
}

// The same refusal, for every azure catalog model on openai-completions, down
// the path the coding session takes: the compat ai.StreamSimple, with the key
// resolved from AZURE_OPENAI_API_KEY as pi's withEnvApiKey resolves it.
func TestAzureCatalogChatCompletionsModelsSendNothing(t *testing.T) {
	var models []*ai.Model
	for _, model := range ai.GetModels(azureProvider) {
		if model.Api == ai.APIOpenAICompletions {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		t.Fatal("the catalog has no azure openai-completions model; this test no longer tests anything")
	}
	req := ai.Context{Messages: []ai.Message{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}}}}
	for _, model := range models {
		t.Run(model.ID, func(t *testing.T) {
			if model.BaseURL != "" {
				t.Fatalf("baseUrl = %q; pi ships azure models without one", model.BaseURL)
			}
			var sent []string
			result := ai.StreamSimple(context.Background(), model, req, &ai.SimpleStreamOptions{
				StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					HTTPClient: azureRecordingDoer{&sent},
					Env:        map[string]string{"AZURE_OPENAI_API_KEY": "azure-key"},
				}},
				Reasoning: "high",
			}).Result()
			if len(sent) != 0 {
				t.Fatalf("sent %q; it must fail before any request leaves", sent)
			}
			if result.StopReason != ai.StopError || !strings.Contains(result.ErrorMessage, "azure/"+model.ID) {
				t.Fatalf("result = %s %q, want the azure refusal", result.StopReason, result.ErrorMessage)
			}
		})
	}
}
