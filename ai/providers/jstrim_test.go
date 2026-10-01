package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Where pi trims, it trims with String.prototype.trim, which strips U+FEFF and
// keeps U+0085 — the reverse of strings.TrimSpace. Every expectation here is
// pi's own, captured under node at 2b0a123de (openai 7.19.0) by
// testdata/jstrim/capture-jstrim.mts; the contexts below are that script's.

const jstrimCaptureFile = "testdata/jstrim/jstrim-2b0a123de.json"

const (
	jsBOM = "\ufeff" // blank to JavaScript, not to strings.TrimSpace
	jsNEL = "\u0085" // blank to strings.TrimSpace, not to JavaScript
)

type jstrimOutcome struct {
	Payload json.RawMessage `json:"payload"`
	Error   string          `json:"error"`
}

type jstrimStream struct {
	SSE          string `json:"sse"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
	Text         string `json:"text"`
}

type jstrimCapture struct {
	Bodies struct {
		Anthropic      jstrimOutcome   `json:"anthropic"`
		Completions    jstrimOutcome   `json:"completions"`
		GoogleContents json.RawMessage `json:"googleContents"`
	} `json:"bodies"`
	Auth      map[string]json.RawMessage         `json:"auth"`
	Streams   map[string]map[string]jstrimStream `json:"streams"`
	Transform []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"transform"`
	Grammar map[string]struct {
		Lark   string `json:"lark"`
		Regex  string `json:"regex"`
		Result *struct {
			Format        string `json:"format"`
			Definition    string `json:"definition"`
			InputProperty string `json:"inputProperty"`
		} `json:"result"`
		Error string `json:"error"`
	} `json:"grammar"`
	StreamingJSON []struct {
		Input  string          `json:"input"`
		Result json.RawMessage `json:"result"`
	} `json:"streamingJson"`
	RetryAfter []struct {
		Bytes   []int    `json:"bytes"`
		Seconds *float64 `json:"seconds"`
	} `json:"retryAfter"`
}

func loadJSTrimCapture(t *testing.T) jstrimCapture {
	t.Helper()
	data, err := os.ReadFile(jstrimCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var c jstrimCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s: %v; rerun capture-jstrim.mts", jstrimCaptureFile, err)
	}
	return c
}

// jstrimAssistant is the capture's `assistant` helper.
func jstrimAssistant(api ai.Api, provider ai.ProviderId, model string, ts int64, content ...ai.Content) ai.AssistantMessage {
	return ai.AssistantMessage{Content: content, Api: api, Provider: provider, Model: model, StopReason: ai.StopStop, Timestamp: ts}
}

func TestJSTrimAnthropicRequestBody(t *testing.T) {
	c := loadJSTrimCapture(t)
	model := foldModel("claude-sonnet-4-5", ai.APIAnthropicMessages, "anthropic", "")
	model.Name = "Claude Sonnet 4.5"
	model.Headers = ai.ProviderHeaders{"anthropic-beta": foldStr(jsBOM + "x-beta" + jsBOM + ", " + jsNEL + "y-beta,\u3000")}
	req := ai.Context{SystemPrompt: "base prompt", Messages: []ai.Message{
		ai.NewUserText(jsBOM, 1),
		ai.NewUserText(jsNEL, 2),
		ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: jsBOM}, ai.TextContent{Text: jsNEL}}, Timestamp: 3},
		jstrimAssistant(ai.APIAnthropicMessages, "anthropic", "claude-sonnet-4-5", 4,
			ai.TextContent{Text: jsBOM},
			ai.TextContent{Text: jsNEL},
			ai.ThinkingContent{Thinking: jsBOM, ThinkingSignature: jsBOM},
			ai.ThinkingContent{Thinking: jsNEL, ThinkingSignature: jsNEL},
			ai.ThinkingContent{Thinking: "unsigned", ThinkingSignature: "\u3000"},
		),
		ai.NewUserText("last", 5),
	}}
	assertRequestBody(t, captureAnthropicPayload(t, model, req, "test-key"), c.Bodies.Anthropic.Payload)
}

// The same-model turn exercises the converter's own trims; the other model's
// turn exercises transformMessages', which decides what becomes text first.
func TestJSTrimCompletionsRequestBody(t *testing.T) {
	c := loadJSTrimCapture(t)
	model := foldModel("gpt-4o", ai.APIOpenAICompletions, "openai", "")
	model.Name = "GPT-4o"
	req := ai.Context{SystemPrompt: "base prompt", Messages: []ai.Message{
		ai.NewUserText("first", 1),
		jstrimAssistant(ai.APIOpenAICompletions, "openai", "gpt-4o", 2,
			ai.TextContent{Text: jsBOM},
			ai.TextContent{Text: jsNEL},
			ai.ThinkingContent{Thinking: jsBOM, ThinkingSignature: "reasoning_content"},
			ai.ThinkingContent{Thinking: jsNEL, ThinkingSignature: "reasoning_content"},
		),
		ai.NewUserText("second", 3),
		jstrimAssistant(ai.APIAnthropicMessages, "anthropic", "claude-sonnet-4-5", 4,
			ai.ThinkingContent{Thinking: jsBOM},
			ai.ThinkingContent{Thinking: jsNEL},
			ai.TextContent{Text: "visible"},
		),
		ai.NewUserText("third", 5),
	}}
	assertRequestBody(t, captureStreamSimplePayload(t, model, req), c.Bodies.Completions.Payload)
}

func TestJSTrimGoogleContents(t *testing.T) {
	c := loadJSTrimCapture(t)
	model := &ai.Model{ID: "gemini-3-pro-preview", Name: "Gemini 3 Pro", Api: ai.APIGoogleGenerativeAI, Provider: "google",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("Hi", 1),
		jstrimAssistant(ai.APIGoogleGenerativeAI, "google", "gemini-3-pro-preview", 2,
			ai.TextContent{Text: jsBOM},
			ai.TextContent{Text: jsNEL},
			ai.ThinkingContent{Thinking: jsBOM, ThinkingSignature: jsBOM},
			ai.ThinkingContent{Thinking: jsNEL, ThinkingSignature: jsNEL},
			ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "ls"}},
		),
	}}
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
	assertRequestBody(t, map[string]any{"contents": body["contents"]},
		json.RawMessage(`{"contents":`+string(c.Bodies.GoogleContents)+`}`))
}

// A credential header counts only when it is non-blank as JavaScript trims.
func TestJSTrimCredentialHeaders(t *testing.T) {
	t.Setenv(ai.AnthropicAuthTokenEnv, "")
	c := loadJSTrimCapture(t)
	anthropicModel := foldModel("claude-sonnet-4-5", ai.APIAnthropicMessages, "anthropic", "")
	completionsModel := foldModel("gpt-4o", ai.APIOpenAICompletions, "openai", "")
	userHi := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})

	for _, tc := range []struct {
		key    string
		header string
		value  string
		stream func(*ai.SimpleStreamOptions) *ai.AssistantMessageEventStream
	}{
		{"anthropicBom", "authorization", jsBOM, func(o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return StreamSimpleAnthropic(context.Background(), anthropicModel, userHi, o)
		}},
		{"anthropicNel", "x-api-key", jsNEL, func(o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return StreamSimpleAnthropic(context.Background(), anthropicModel, userHi, o)
		}},
		{"completionsBom", "authorization", jsBOM, func(o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return StreamSimpleOpenAICompletions(context.Background(), completionsModel, userHi, o)
		}},
		{"completionsNel", "authorization", jsNEL, func(o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return StreamSimpleOpenAICompletions(context.Background(), completionsModel, userHi, o)
		}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			var want struct {
				Payload bool   `json:"payload"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(c.Auth[tc.key], &want); err != nil {
				t.Fatalf("auth.%s: %v", tc.key, err)
			}
			reached := false
			opts := &ai.SimpleStreamOptions{}
			opts.Headers = ai.ProviderHeaders{tc.header: foldStr(tc.value)}
			opts.OnPayload = func(any, *ai.Model) (any, error) {
				reached = true
				return nil, errors.New("payload captured")
			}
			final := tc.stream(opts).Result()
			if reached != want.Payload {
				t.Fatalf("%s %+q: request reached its body = %v, pi = %v (stream ended %s: %q)",
					tc.header, tc.value, reached, want.Payload, final.StopReason, final.ErrorMessage)
			}
			if !want.Payload && final.ErrorMessage != want.Error {
				t.Fatalf("errorMessage = %q, pi = %q", final.ErrorMessage, want.Error)
			}
		})
	}
}

// jstrimServe answers every request with body.
func jstrimServe(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func jstrimText(m *ai.AssistantMessage) string {
	var b strings.Builder
	for _, block := range m.Content {
		if text, ok := block.(ai.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// The SSE decoders: the genai SDK and pi-messages trim event data as
// JavaScript does, while the openai SDK strips exactly one leading space from a
// field and hands the rest to JSON.parse. Where pi accepted the padded event
// the whole outcome is compared. Where pi rejected it, pi's stream fails with
// V8's SyntaxError text. The openai loops fail with that text too
// (jstext.JSONSyntaxError), so their whole outcome is compared either way; for
// google and pi-messages the assertion is the stop reason, with the message
// wherever it is pi's own.
func TestJSTrimStreamDecoding(t *testing.T) {
	c := loadJSTrimCapture(t)
	userHi := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	opts := func(key string) *ai.SimpleStreamOptions {
		o := &ai.SimpleStreamOptions{}
		o.APIKey = key
		return o
	}
	run := map[string]func(url string) *ai.AssistantMessage{
		"completions": func(url string) *ai.AssistantMessage {
			model := foldModel("gpt-4o", ai.APIOpenAICompletions, "openai", "")
			model.BaseURL = url
			return StreamSimpleOpenAICompletions(context.Background(), model, userHi, opts("test-key")).Result()
		},
		"responses": func(url string) *ai.AssistantMessage {
			model := foldModel("gpt-5", ai.APIOpenAIResponses, "openai", "")
			model.BaseURL = url
			return StreamSimpleOpenAIResponses(context.Background(), model, userHi, opts("test-key")).Result()
		},
		"google": func(url string) *ai.AssistantMessage {
			model := foldModel("gemini-2.5-flash", ai.APIGoogleGenerativeAI, "google", "")
			model.BaseURL = url
			return StreamSimpleGoogle(context.Background(), model, userHi, opts("k")).Result()
		},
		"piMessages": func(url string) *ai.AssistantMessage {
			model := foldModel("auto", ai.APIPiMessages, "radius", "")
			model.BaseURL = url + "/v1"
			return StreamSimplePiMessages(context.Background(), model, userHi, opts("test-key")).Result()
		},
	}
	// syntaxErrorText are the decoders whose parse failure text the port
	// reproduces: the openai SDK's own SyntaxError text.
	syntaxErrorText := map[string]bool{"completions": true, "responses": true}
	for api, cases := range c.Streams {
		if run[api] == nil {
			t.Fatalf("capture has streams for %q, which this test does not run", api)
		}
		for name, want := range cases {
			t.Run(api+"/"+name, func(t *testing.T) {
				got := run[api](jstrimServe(t, want.SSE))
				if text := jstrimText(got); text != want.Text {
					t.Fatalf("text = %q, pi = %q (stream ended %s: %q)", text, want.Text, got.StopReason, got.ErrorMessage)
				}
				if string(got.StopReason) != want.StopReason {
					t.Fatalf("stopReason = %s (%q), pi = %s (%q)", got.StopReason, got.ErrorMessage, want.StopReason, want.ErrorMessage)
				}
				if (syntaxErrorText[api] || !strings.HasPrefix(want.ErrorMessage, "Unexpected ")) && got.ErrorMessage != want.ErrorMessage {
					t.Fatalf("errorMessage = %q, pi = %q", got.ErrorMessage, want.ErrorMessage)
				}
			})
		}
	}
}

// transformMessages drops another model's blank thinking and turns the rest
// into text. Every converter drops blank text afterwards, so only the
// transformed content itself shows which blocks the trim let through.
func TestJSTrimTransformMessages(t *testing.T) {
	want := loadJSTrimCapture(t).Transform
	if len(want) == 0 {
		t.Fatalf("%s has no transform content; rerun capture-jstrim.mts", jstrimCaptureFile)
	}
	model := foldModel("gpt-4o", ai.APIOpenAICompletions, "openai", "")
	out := transformMessages([]ai.Message{
		ai.NewUserText("hi", 1),
		jstrimAssistant(ai.APIAnthropicMessages, "anthropic", "claude-sonnet-4-5", 2,
			ai.ThinkingContent{Thinking: jsBOM},
			ai.ThinkingContent{Thinking: jsNEL},
			ai.ThinkingContent{Thinking: "\u3000"},
			ai.TextContent{Text: "visible"},
		),
	}, model, nil)
	am, ok := asAssistantMsg(out[1])
	if !ok {
		t.Fatalf("transformed message 1 is %T, want an assistant message", out[1])
	}
	var got []string
	for _, block := range am.Content {
		if text, ok := block.(ai.TextContent); ok {
			got = append(got, "text:"+text.Text)
		} else {
			got = append(got, fmt.Sprintf("%T", block))
		}
	}
	var wantBlocks []string
	for _, block := range want {
		wantBlocks = append(wantBlocks, block.Type+":"+block.Text)
	}
	if !slices.Equal(got, wantBlocks) {
		t.Fatalf("content = %+q, pi = %+q", got, wantBlocks)
	}
}

func TestJSTrimGrammarVariantSelection(t *testing.T) {
	for name, want := range loadJSTrimCapture(t).Grammar {
		t.Run(name, func(t *testing.T) {
			tool := ai.Tool{Name: "g", Description: "g", Parameters: ai.Object(ai.Prop("input", ai.String())),
				ConstrainedSampling: &ai.ConstrainedSamplingConfig{
					Type:     ai.ConstrainedSamplingGrammar,
					Variants: ai.GrammarVariants{OpenAILark: want.Lark, OpenAIRegex: want.Regex},
				}}
			got, err := resolveGrammarSampling(tool, true)
			if want.Error != "" {
				if err == nil || err.Error() != want.Error {
					t.Fatalf("err = %v, pi threw %q", err, want.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, pi resolved %+v", err, *want.Result)
			}
			if got.format != want.Result.Format || got.definition != want.Result.Definition || got.inputProperty != want.Result.InputProperty {
				t.Fatalf("resolved {%s %+q %s}, pi {%s %+q %s}", got.format, got.definition, got.inputProperty,
					want.Result.Format, want.Result.Definition, want.Result.InputProperty)
			}
		})
	}
}

func TestJSTrimParseStreamingJSON(t *testing.T) {
	for _, tc := range loadJSTrimCapture(t).StreamingJSON {
		got, _ := parseStreamingJSON(tc.Input)
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		_ = json.Unmarshal(raw, &gotValue)
		if err := json.Unmarshal(tc.Result, &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("parseStreamingJSON(%+q) = %s, pi = %s", tc.Input, raw, tc.Result)
		}
	}
}

// fetch decodes header bytes as latin1 before getRetryDelayMs parses them, so
// the raw bytes on the wire are the input: a UTF-8 NBSP is "Â\u00a0" to pi, not
// whitespace, while a bare 0xA0 byte is.
func TestJSTrimRetryAfterHeaderBytes(t *testing.T) {
	for _, tc := range loadJSTrimCapture(t).RetryAfter {
		raw := make([]byte, len(tc.Bytes))
		for i, b := range tc.Bytes {
			raw[i] = byte(b)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			_, _ = conn.Write([]byte("HTTP/1.1 429 Too Many Requests\r\nretry-after: "))
			_, _ = conn.Write(raw)
			_, _ = conn.Write([]byte("\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"))
		}()
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		ln.Close()
		if err != nil {
			t.Fatalf("% x: %v", raw, err)
		}
		resp.Body.Close()
		ms, ok := serverRetryDelayMs(resp)
		if tc.Seconds != nil {
			if want := *tc.Seconds * 1000; !ok || ms != want {
				t.Errorf("retry-after % x: delay = (%v, %v), pi = %v ms", raw, ms, ok, want)
			}
			continue
		}
		// pi's NaN reading falls through to Date.parse, NaN again for every
		// row but one, so no header dictates the delay and pi backs off
		// (upstream 2bbfcca43). The exception is K30: V8's fallback parser
		// reads "\u00c2 5" as a date in May 2001, so pi retries at once.
		if string(raw) == "\xc2\xa0\x35" {
			if !ok {
				continue
			}
			t.Errorf("retry-after % x: now dictates (%v ms); retire K30's Retry-After half", raw, ms)
			continue
		}
		if ok {
			t.Errorf("retry-after % x: delay = %v ms, pi falls back to the backoff", raw, ms)
		}
	}
}
