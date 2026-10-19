package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// keyOrderCaptureFile is what ai/testdata/key-order/capture.mjs recorded from
// pi-ai 1.3.0 and pi-agent-core 1.3.0.
const keyOrderCaptureFile = "../testdata/key-order/key-order-1.3.0.json"

// jsonKeys lists a JSON object's keys in the order the text writes them.
func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("%s is not an object", raw)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// The assistant messages the port writes — to a session file, the wire, an
// event — list their keys in the order pi's JSON.stringify writes them: each
// adapter's own message literal and assignment order (role, content, api,
// provider, model, usage, stopReason, timestamp, then what the stream
// learned, durationMs last), with the agent loop's thinkingLevel appended
// after durationMs (agent-loop.ts Object.assign on the result). Content blocks
// and usage follow pi's too.
func TestAssistantMessageKeyOrderMatchesPi(t *testing.T) {
	data, err := os.ReadFile(keyOrderCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name        string     `json:"name"`
			API         string     `json:"api"`
			Scenario    string     `json:"scenario"`
			Layer       string     `json:"layer"`
			Keys        []string   `json:"keys"`
			ContentKeys [][]string `json:"contentKeys"`
			UsageKeys   []string   `json:"usageKeys"`
			Message     struct {
				Usage map[string]any `json:"usage"`
			} `json:"message"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", keyOrderCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", keyOrderCaptureFile)
	}
	bodies := keyOrderBodies(t)
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			final := streamKeyOrderRow(t, row.API, row.Scenario, bodies)
			if row.Layer == "agent" {
				// The agent loop stamps the requested level on the stream's
				// result (agent/loop.go, pi's Object.assign).
				final.ThinkingLevel = ai.ModelThinkingLevel("low")
			}
			raw, err := json.Marshal(final)
			if err != nil {
				t.Fatal(err)
			}
			if got := jsonKeys(t, raw); !slices.Equal(got, row.Keys) {
				t.Errorf("message keys\n%s\npi\n%s", strings.Join(got, ","), strings.Join(row.Keys, ","))
			}
			var parts struct {
				Content []json.RawMessage `json:"content"`
				Usage   json.RawMessage   `json:"usage"`
			}
			_ = json.Unmarshal(raw, &parts)
			if len(parts.Content) != len(row.ContentKeys) {
				t.Fatalf("%d content blocks, pi %d", len(parts.Content), len(row.ContentKeys))
			}
			for i, block := range parts.Content {
				if got := jsonKeys(t, block); !slices.Equal(got, row.ContentKeys[i]) {
					t.Errorf("content[%d] keys %v, pi %v", i, got, row.ContentKeys[i])
				}
			}
			// pi writes a zero reasoning (openai, google) and cacheWrite1h
			// (anthropic) that the port omits — docs/UPSTREAM.md K12 and K24,
			// presence rather than order — so those keys are compared only when
			// they hold a count.
			wantUsage := slices.DeleteFunc(slices.Clone(row.UsageKeys), func(key string) bool {
				return (key == "reasoning" || key == "cacheWrite1h") && row.Message.Usage[key] == float64(0)
			})
			if got := jsonKeys(t, parts.Usage); !slices.Equal(got, wantUsage) {
				t.Errorf("usage keys %v, pi %v", got, wantUsage)
			}
		})
	}
}

// keyOrderBodies are the response bodies capture.mjs serves each adapter on
// success, written out again here (the capture records the keys, not the
// bodies).
func keyOrderBodies(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"anthropic-messages": sseNamed(t,
			`{"type":"message_start","message":{"id":"msg_1","model":"claude-fable-5-1-relabeled","usage":{"input_tokens":5,"output_tokens":0,"cache_creation_input_tokens":4,"cache_creation":{"ephemeral_1h_input_tokens":4}},"input_transformations":[{"type":"thinking_dropped","path":"messages[0]","reason":"r"}]}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		),
		"openai-completions": piMessagesSSE(
			`{"id":"chatcmpl_1","model":"gpt-4o-2024","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hmm"}}]}`,
			`{"id":"chatcmpl_1","model":"gpt-4o-2024","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
			`{"id":"chatcmpl_1","model":"gpt-4o-2024","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]}}]}`,
			`{"id":"chatcmpl_1","model":"gpt-4o-2024","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		) + "data: [DONE]\n\n",
		"openai-responses": piMessagesSSE(
			`{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"hmm"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hmm"}]}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"hi"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}}`,
			`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\":\"a\"}"}`,
			`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"a\"}","status":"completed"}}`,
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		),
		"google-generative-ai": piMessagesSSE(
			`{"responseId":"r_1","modelVersion":"gemini-2.5-pro-002","candidates":[{"content":{"role":"model","parts":[{"text":"hmm","thought":true,"thoughtSignature":"c2ln"}]}}]}`,
			`{"responseId":"r_1","candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`,
			`{"responseId":"r_1","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read","args":{"path":"a"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`,
		),
		"pi-messages": piMessagesSSE(
			`{"type":"start","responseId":"r_1"}`,
			`{"type":"thinking_start","contentIndex":0}`,
			`{"type":"thinking_delta","contentIndex":0,"delta":"hmm"}`,
			`{"type":"thinking_end","contentIndex":0,"content":"hmm","thinkingSignature":"sig"}`,
			`{"type":"text_start","contentIndex":1}`,
			`{"type":"text_delta","contentIndex":1,"delta":"hi"}`,
			`{"type":"text_end","contentIndex":1,"content":"hi"}`,
			`{"type":"toolcall_start","contentIndex":2,"id":"call_1","toolName":"read"}`,
			`{"type":"toolcall_delta","contentIndex":2,"delta":"{\"path\":\"a\"}"}`,
			`{"type":"toolcall_end","contentIndex":2,"toolCall":{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"a"}}}`,
			`{"type":"done","reason":"toolUse","usage":{"input":5,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":8,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"responseId":"r_1"}`,
		),
	}
}

// sseNamed builds an SSE body of named events from their JSON.
func sseNamed(t *testing.T, events ...string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(e), &head); err != nil {
			t.Fatal(err)
		}
		b.WriteString("event: " + head.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

// streamKeyOrderRow streams one row of the capture through the port's adapter
// and returns the final message.
func streamKeyOrderRow(t *testing.T, api, scenario string, bodies map[string]string) *ai.AssistantMessage {
	t.Helper()
	status, contentType, body := http.StatusOK, "text/event-stream", bodies[api]
	if scenario == "error" {
		status, contentType = http.StatusBadRequest, "application/json"
		body = `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`
		if api == string(ai.APIGoogleGenerativeAI) {
			body = `{"error":{"code":400,"message":"bad request","status":"INVALID_ARGUMENT"}}`
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	base := ai.Model{Reasoning: true, Input: []string{"text"}, Cost: ai.ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.2},
		ContextWindow: 100000, MaxTokens: 1000, BaseURL: server.URL}
	tools := ai.NewSystemText("", 0)
	tools.ToolsAdded = []ai.Tool{{Name: "read", Description: "read", Parameters: ai.Object(ai.Opt("path", ai.String()))}}
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{tools, ai.NewUserText("hi", 1)}})
	opts := &ai.SimpleStreamOptions{Reasoning: ai.ThinkingLow}
	opts.APIKey, opts.CacheRetention = "test-key", ai.CacheNone
	ctx := context.Background()
	model := base
	switch ai.Api(api) {
	case ai.APIAnthropicMessages:
		model.ID, model.Name, model.Api, model.Provider = "claude-fable-5-1", "Claude Fable 5.1", ai.APIAnthropicMessages, "anthropic"
		model.Compat = json.RawMessage(`{"forceAdaptiveThinking":true,"supportsMidConvoEffort":true}`)
		return StreamSimpleAnthropic(ctx, &model, req, opts).Result()
	case ai.APIOpenAICompletions:
		model.ID, model.Name, model.Api, model.Provider = "gpt-4o", "GPT-4o", ai.APIOpenAICompletions, "openai"
		return StreamSimpleOpenAICompletions(ctx, &model, req, opts).Result()
	case ai.APIOpenAIResponses:
		model.ID, model.Name, model.Api, model.Provider = "gpt-6-luna", "GPT-6 Luna", ai.APIOpenAIResponses, "openai"
		model.Compat = json.RawMessage(`{"supportsMidConvoEffort":true}`)
		return StreamSimpleOpenAIResponses(ctx, &model, req, opts).Result()
	case ai.APIGoogleGenerativeAI:
		model.ID, model.Name, model.Api, model.Provider = "gemini-2.5-pro", "Gemini 2.5 Pro", ai.APIGoogleGenerativeAI, "google"
		return StreamSimpleGoogle(ctx, &model, req, opts).Result()
	case ai.APIPiMessages:
		model.ID, model.Name, model.Api, model.Provider = "auto", "Auto", ai.APIPiMessages, "radius"
		model.BaseURL = server.URL + "/v1"
		return StreamSimplePiMessages(ctx, &model, req, opts).Result()
	}
	t.Fatalf("no adapter for %s", api)
	return nil
}
