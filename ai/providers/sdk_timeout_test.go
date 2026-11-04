package providers

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

// The SDK adapters send through undici's fetch, whose headersTimeout (300s by
// default; pi's CLI installs its httpIdleTimeoutMs there) bounds the wait for a
// response's headers whatever timeoutMs says: the SDK's own timeoutMs timer can
// only cut that wait shorter, and wins a tie, since it starts before fetch
// does. Measured against pi at 2b0a123de (node v26.4.0, @anthropic-ai/sdk
// 0.124.0, openai 7.19.0) with undici's headersTimeout shortened through a
// global Agent and a server that reads the request and never answers:
//
//   - headersTimeout 1000ms, timeoutMs unset (the SDKs' 600s), 2000 or 3000:
//     every SDK adapter fails at undici's timeout, anthropic "Request timed
//     out." and both openai loops with openai 7.19.0's own text for
//     UND_ERR_HEADERS_TIMEOUT (openaiHeadersTimeoutText);
//   - timeoutMs 1000 (the tie) or 100: each fails "Request timed out." at
//     timeoutMs, the SDK's own timer.
//
// The port shortens undiciHeadersTimeoutMs the same way. Since upstream
// 4c992154d pi appends the error's cause (withCauseDetail), and openai 7.19.0
// keeps the rejected fetch as its timeout error's cause where anthropic keeps
// none: openai's headers timeout reads "...: Headers Timeout Error
// (UND_ERR_HEADERS_TIMEOUT)", its own timer's "Request timed out.: This
// operation was aborted (20)" (the AbortError its abort makes fetch reject
// with), and anthropic's stays "Request timed out." (measured with node
// v22.23.4, undici 6.29.1, openai 7.19.0, @anthropic-ai/sdk 0.129.0).

// openaiHeadersTimeoutText is openai 7.19.0's APIConnectionTimeoutError
// message for a fetch whose cause is undici's UND_ERR_HEADERS_TIMEOUT.
const openaiHeadersTimeoutText = "Request timed out. Node.js fetch timed out waiting for response headers; configure a matching undici fetch and fetchOptions.dispatcher with an Agent whose headersTimeout is at least the SDK timeout."

// shortenUndiciHeadersTimeout sets undici's headersTimeout to ms for the test.
func shortenUndiciHeadersTimeout(t *testing.T, ms int) {
	t.Helper()
	if undiciHeadersTimeoutMs != 300_000 {
		t.Fatalf("undiciHeadersTimeoutMs %d; undici's headersTimeout default is 300000", undiciHeadersTimeoutMs)
	}
	old := undiciHeadersTimeoutMs
	undiciHeadersTimeoutMs = ms
	t.Cleanup(func() { undiciHeadersTimeoutMs = old })
}

// streamSDKAdapter streams adapter's model from baseURL within ctx.
func streamSDKAdapter(ctx context.Context, adapter, baseURL string, opts ai.StreamOptions) *ai.AssistantMessage {
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	model := &ai.Model{BaseURL: baseURL, Input: []string{"text"}, MaxTokens: 4096}
	switch adapter {
	case "openai-completions":
		model.ID, model.Api, model.Provider = "gpt-test", ai.APIOpenAICompletions, "openai"
		return StreamOpenAICompletions(ctx, model, req, &OpenAIOptions{StreamOptions: opts}).Result()
	case "openai-responses":
		model.ID, model.Api, model.Provider = "gpt-test", ai.APIOpenAIResponses, "openai"
		return StreamOpenAIResponses(ctx, model, req, &OpenAIResponsesOptions{StreamOptions: opts}).Result()
	}
	model.ID, model.Api, model.Provider = "claude-test", ai.APIAnthropicMessages, "anthropic"
	return StreamAnthropic(ctx, model, req, &AnthropicOptions{StreamOptions: opts}).Result()
}

// TestSDKAdaptersWaitOnUndiciHeadersTimeout replays the measurement above with
// undici's headersTimeout at 100ms. The context bounds a wait on timeoutMs
// alone (the SDKs' 600s default) to a few seconds, which then ends the stream
// aborted.
func TestSDKAdaptersWaitOnUndiciHeadersTimeout(t *testing.T) {
	shortenUndiciHeadersTimeout(t, 100)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	for _, tc := range []struct {
		name      string
		timeoutMs int
		// undici is whether undici's headersTimeout fires first.
		undici bool
	}{
		{"timeoutMs unset", 0, true},
		{"timeoutMs past undici's", 3_000, true},
		{"timeoutMs tied with undici's", 100, false},
		{"timeoutMs short of undici's", 50, false},
	} {
		for _, adapter := range sdkAdapters {
			t.Run(tc.name+"/"+adapter, func(t *testing.T) {
				want := "Request timed out."
				switch {
				case adapter == "anthropic-messages":
				case tc.undici:
					want = openaiHeadersTimeoutText + ": Headers Timeout Error (UND_ERR_HEADERS_TIMEOUT)"
				default:
					want += ": This operation was aborted (20)"
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				start := time.Now()
				final := streamSDKAdapter(ctx, adapter, server.URL, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "k", TimeoutMs: tc.timeoutMs,
				}})
				took := time.Since(start)
				if final.StopReason != ai.StopError || final.ErrorMessage != want {
					t.Fatalf("stream = %s %q after %v; pi: error %q", final.StopReason, final.ErrorMessage, took.Round(time.Millisecond), want)
				}
				// Anthropic's text is the same either way: undici's timeout
				// shows in when the stream ends.
				if tc.undici && tc.timeoutMs > 0 && took >= time.Duration(tc.timeoutMs)*time.Millisecond/2 {
					t.Fatalf("stream ended after %v; pi ends at undici's headersTimeout, before timeoutMs %dms", took.Round(time.Millisecond), tc.timeoutMs)
				}
			})
		}
	}
}

// TestSDKAdaptersConnectTimeoutIsNotUndicisHeadersTimeout: only a wait for
// headers once the request is written is undici's headersTimeout. A TLS
// handshake that never completes is undici's connect timeout, which openai
// 7.19.0 words as any other, "Request timed out." — net/http's own TLS
// handshake timeout (10s, as undici's connect timeout is) stands for it here.
// Measured against pi at 2b0a123de: an https base URL whose server never
// answers the handshake fails both openai loops "Request timed out." after
// 10.5s; since 4c992154d the message ends with its cause, undici's
// ConnectTimeoutError for the URL's host and port.
func TestSDKAdaptersConnectTimeoutIsNotUndicisHeadersTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on net/http's 10s TLS handshake timeout")
	}
	shortenUndiciHeadersTimeout(t, 100)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The parallel subtests run once this function has returned; a cleanup
	// outlives them.
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, conn := range held {
				conn.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection: the client's handshake never completes.
			held = append(held, conn)
		}
	}()
	for _, adapter := range []string{"openai-completions", "openai-responses"} {
		t.Run(adapter, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			final := streamSDKAdapter(ctx, adapter, "https://"+ln.Addr().String(), ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}})
			want := "Request timed out.: Connect Timeout Error (attempted address: " + ln.Addr().String() + ", timeout: 10000ms) (UND_ERR_CONNECT_TIMEOUT)"
			if final.StopReason != ai.StopError || final.ErrorMessage != want {
				t.Fatalf("stream = %s %q; pi: error %q", final.StopReason, final.ErrorMessage, want)
			}
		})
	}
}
