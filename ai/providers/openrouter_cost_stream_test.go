package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// redirectDoer sends every request to target while the model keeps its own
// base URL, so a test can stand in for a host the adapter recognizes by name.
type redirectDoer struct{ target *url.URL }

func (d redirectDoer) Do(r *http.Request) (*http.Response, error) {
	r.URL.Scheme, r.URL.Host, r.Host = d.target.Scheme, d.target.Host, d.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

func openRouterSSEDoer(t *testing.T, body string) redirectDoer {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return redirectDoer{target}
}

// anthropicOpenRouterSSE is a minimal Messages stream whose message_delta usage
// carries the billing fields a production OpenRouter stream sent (upstream
// anthropic-sse-parsing.test.ts, #9980).
const anthropicOpenRouterSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"anthropic/claude-haiku-4.5","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"cost":0.004,"is_byok":false,"cost_details":{"upstream_inference_cost":0.003}}}

event: message_stop
data: {"type":"message_stop"}

`

// OpenRouter's Anthropic Messages endpoint reports the billed total in
// message_delta usage, and pi uses it in place of the catalog estimate for a
// model on OpenRouter's host, under whatever provider name (upstream
// cae8d69e5). The buckets stay catalog-derived, so they no longer sum to it.
func TestAnthropicUsesOpenRouterReportedTotal(t *testing.T) {
	for _, tc := range []struct {
		name, provider, baseURL string
		fromProvider            bool
	}{
		{"custom provider on OpenRouter's host", "openrouter-work", "https://openrouter.ai/api", true},
		{"another host keeps the catalog total", "custom", "https://example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := anthropicPlainModel()
			model.ID, model.Provider, model.BaseURL = "anthropic/claude-haiku-4.5", tc.provider, tc.baseURL
			model.Cost = ai.ModelCost{Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25}
			opts := apiKeyOptions("test-key")
			opts.HTTPClient = openRouterSSEDoer(t, anthropicOpenRouterSSE)
			final := StreamAnthropic(context.Background(), model, ai.NormalizeContext(anthropicHelloContext()), opts).Result()
			if final.StopReason != ai.StopStop {
				t.Fatalf("stopReason = %s (%s)", final.StopReason, final.ErrorMessage)
			}
			cost := final.Usage.Cost
			buckets := cost.Input + cost.Output + cost.CacheRead + cost.CacheWrite
			if tc.fromProvider {
				if cost.Total != 0.004 || cost.TotalFromProvider != ai.OptBoolTrue {
					t.Fatalf("cost = %+v, want OpenRouter's total 0.004 from the provider", cost)
				}
				if buckets == cost.Total {
					t.Fatalf("buckets sum to the reported total %v; the fixture no longer tells them apart", cost.Total)
				}
			} else if cost.Total != buckets || cost.TotalFromProvider != ai.OptBoolFalse {
				t.Fatalf("cost = %+v, want the catalog total %v, not from the provider", cost, buckets)
			}
		})
	}
}

func openRouterCompletionsSSE(usage string) string {
	return `data: {"id":"chatcmpl-cost","model":"openrouter/auto","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}],"usage":` + usage + "}\n\n" +
		"data: [DONE]\n\n"
}

// upstream openai-completions-provider-stream-event.test.ts's two #9980 cases:
// a non-BYOK total is OpenRouter's charge alone (its upstream cost is already
// in it), a BYOK one adds the upstream inference cost.
func TestOpenAICompletionsUsesOpenRouterReportedTotal(t *testing.T) {
	for _, tc := range []struct {
		name, provider, usage string
		want                  ai.CostBreakdown
	}{
		{
			"non-BYOK, custom provider on OpenRouter's host", "openrouter-work",
			`{"prompt_tokens":100,"completion_tokens":20,"cost":0.004,"is_byok":false,"cost_details":{"upstream_inference_cost":0.003}}`,
			ai.CostBreakdown{Total: 0.004, TotalFromProvider: ai.OptBoolTrue},
		},
		{
			"BYOK", "openrouter",
			`{"prompt_tokens":100,"completion_tokens":20,"cost":0.0002,"is_byok":true,"cost_details":{"upstream_inference_cost":0.003}}`,
			ai.CostBreakdown{Total: 0.0032, TotalFromProvider: ai.OptBoolTrue},
		},
		{
			"no accounting keeps the catalog total", "openrouter",
			`{"prompt_tokens":100,"completion_tokens":20}`,
			ai.CostBreakdown{TotalFromProvider: ai.OptBoolFalse},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{
				ID: "openrouter/auto", Name: "OpenRouter Auto", Api: ai.APIOpenAICompletions,
				Provider: tc.provider, BaseURL: "https://openrouter.ai/api/v1",
				Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 4096,
			}
			opts := &OpenAIOptions{}
			opts.APIKey = "test"
			opts.HTTPClient = openRouterSSEDoer(t, openRouterCompletionsSSE(tc.usage))
			final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{
				Messages: []ai.Message{ai.NewUserText("hi", 1)},
			}), opts).Result()
			if final.StopReason != ai.StopStop {
				t.Fatalf("stopReason = %s (%s)", final.StopReason, final.ErrorMessage)
			}
			if final.Usage.Cost != tc.want {
				t.Fatalf("cost = %+v, want %+v", final.Usage.Cost, tc.want)
			}
		})
	}
}
