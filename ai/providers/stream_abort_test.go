package providers

import (
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
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/sky-valley/pi/ai"
)

// streamAbortCaptureFile is written by testdata/stream-abort/capture.mts,
// which streams pi's adapters from source at the named sha.
const streamAbortCaptureFile = "testdata/stream-abort/stream-abort-71023aec1.json"

// streamAbortRun is one row of the capture: how pi's adapter ended a stream
// whose signal aborted once the request was on its way, or whose connection
// dropped mid-body. capture.mts says how each mode is made.
type streamAbortRun struct {
	API  string `json:"api"`
	Mode string `json:"mode"`
	// Status and Segments are what the server wrote before holding the
	// connection, each segment one HTTP chunk and one body read; zero and
	// empty when it never answered.
	Status int `json:"status"`
	// RetryAfter is the retry-after header a retryable error carried, and
	// MaxRetries the maxRetries the run streamed with.
	RetryAfter   string   `json:"retryAfter"`
	MaxRetries   int      `json:"maxRetries"`
	Segments     []string `json:"segments"`
	Observed     []string `json:"observed"`
	Events       []string `json:"events"`
	StopReason   string   `json:"stopReason"`
	ErrorMessage string   `json:"errorMessage"`
	// Content is JSON.stringify of the final message's content.
	Content string `json:"content"`
	// Diagnostics holds the type of each of the final message's diagnostics.
	Diagnostics []string `json:"diagnostics"`
	// CustomBodyError is the error a custom fetch's body failed with.
	CustomBodyError string `json:"customBodyError"`
	// Headers are the options.headers the run streamed with.
	Headers map[string]string `json:"headers"`
	// CustomFetchError is the error a custom fetch rejected with.
	CustomFetchError string `json:"customFetchError"`
	// TimeoutMs is the timeoutMs the run streamed with.
	TimeoutMs int `json:"timeoutMs"`
}

// rejectingDoer fails every request with err, as a custom fetch rejects.
type rejectingDoer struct{ err error }

func (d rejectingDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

// serveRawOnce answers every connection on a loopback listener by reading the
// request's first bytes and then writing reply, which may be empty, and
// closing the connection.
func serveRawOnce(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.Read(make([]byte, 64*1024))
				io.WriteString(conn, reply)
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

func loadStreamAbortCapture(t *testing.T) []streamAbortRun {
	t.Helper()
	raw, err := os.ReadFile(streamAbortCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/stream-abort/capture.mts)", streamAbortCaptureFile, err)
	}
	var capture struct {
		Runs []streamAbortRun `json:"runs"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatalf("decode %s: %v", streamAbortCaptureFile, err)
	}
	if len(capture.Runs) == 0 {
		t.Fatalf("%s has no runs", streamAbortCaptureFile)
	}
	return capture.Runs
}

// heldBody is a response body the way net/http delivers one whose server
// wrote its segments and then held the connection: each read returns the
// next segment whole — one that has arrived is read whether or not the
// request's context has ended, as net/http reads what it has buffered — and
// once they are gone a read blocks until the context ends and then fails with
// the transport's error. cancelOnHold, when set, is called as that read
// starts: the abort lands while it is pending.
type heldBody struct {
	ctx          context.Context
	segments     []string
	cancelOnHold context.CancelFunc
}

func (b *heldBody) Read(p []byte) (int, error) {
	if len(b.segments) > 0 {
		n := copy(p, b.segments[0])
		b.segments[0] = b.segments[0][n:]
		if b.segments[0] == "" {
			b.segments = b.segments[1:]
		}
		return n, nil
	}
	if b.cancelOnHold != nil {
		b.cancelOnHold()
	}
	<-b.ctx.Done()
	return 0, context.Canceled
}

func (b *heldBody) Close() error { return nil }

// ownBody is a body as the port's own client hands it to an adapter: read
// through fetchBody, undici's body semantics. The adapter wraps its own
// client's body so; a test doer delivering what that client would deliver,
// read by read, wraps it here, since the adapter takes the doer for a custom
// client and reads its body as it is.
type ownBody struct {
	fetchBody
	io.Closer
}

func ownClientBody(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	return ownBody{fetchBody{ctx, body}, body}
}

// abortingBody is a custom client's body that delivers segment on its first
// read, then aborts the request as its next read starts and fails that read
// with its own err.
type abortingBody struct {
	segment string
	read    bool
	cancel  context.CancelFunc
	err     error
}

func (b *abortingBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, b.segment), nil
	}
	b.cancel()
	return 0, b.err
}

func (b *abortingBody) Close() error { return nil }

// hangingBody is a custom client's body that delivers segment on its first
// read; its next read aborts the request as it starts and then blocks until
// release is closed, whatever the request's context does: a body that ignores
// the cancellation.
type hangingBody struct {
	segment string
	read    bool
	cancel  context.CancelFunc
	release chan struct{}
}

func (b *hangingBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, b.segment), nil
	}
	b.cancel()
	<-b.release
	return 0, io.EOF
}

func (b *hangingBody) Close() error { return nil }

// abortingDoer is a custom client that aborts the request and then rejects
// it with err.
type abortingDoer struct {
	cancel context.CancelFunc
	err    error
}

func (d abortingDoer) Do(*http.Request) (*http.Response, error) {
	d.cancel()
	return nil, d.err
}

// heldDoer answers every request 200 text/event-stream with body.
type heldDoer struct{ body io.ReadCloser }

func (d heldDoer) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       d.body,
		Request:    req,
	}, nil
}

// errorBodyDoer answers every request status, application/json, with
// retryAfter (when set) and body: a custom fetch's error response.
type errorBodyDoer struct {
	status     int
	retryAfter string
	body       io.ReadCloser
}

func (d errorBodyDoer) Do(req *http.Request) (*http.Response, error) {
	header := http.Header{"Content-Type": {"application/json"}}
	if d.retryAfter != "" {
		header.Set("Retry-After", d.retryAfter)
	}
	return &http.Response{
		StatusCode: d.status,
		Status:     fmt.Sprintf("%d %s", d.status, http.StatusText(d.status)),
		Header:     header,
		Body:       d.body,
		Request:    req,
	}, nil
}

// streamAbortAdapter streams the capture's model for api from baseURL.
func streamAbortAdapter(t *testing.T, ctx context.Context, api, baseURL string, opts ai.StreamOptions) *ai.AssistantMessageEventStream {
	t.Helper()
	opts.APIKey = "test-api-key"
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	switch ai.Api(api) {
	case ai.APIAnthropicMessages:
		model := &ai.Model{ID: "claude-sonnet-4-5", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000}
		return StreamAnthropic(ctx, model, req, &AnthropicOptions{StreamOptions: opts})
	case ai.APIPiMessages:
		model := &ai.Model{ID: "auto", Api: ai.APIPiMessages, Provider: "radius", BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000}
		return StreamPiMessages(ctx, model, req, &PiMessagesOptions{StreamOptions: opts})
	case ai.APIOpenAICompletions:
		model := &ai.Model{ID: "gpt-4o", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000}
		return StreamOpenAICompletions(ctx, model, req, &OpenAIOptions{StreamOptions: opts})
	case ai.APIOpenAIResponses:
		model := &ai.Model{ID: "gpt-5-mini", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000}
		return StreamOpenAIResponses(ctx, model, req, &OpenAIResponsesOptions{StreamOptions: opts})
	case ai.APIGoogleGenerativeAI:
		model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 1000}
		return StreamGoogle(ctx, model, req, &GoogleOptions{StreamOptions: opts})
	}
	t.Fatalf("no Go adapter for %s", api)
	return nil
}

// observedTypes is an OnProviderStreamEvent that records each event's type
// and calls onEvent with how many it has seen.
func observedTypes(observed *[]string, onEvent func(seen int)) func(any, *ai.Model) error {
	return func(data any, _ *ai.Model) error {
		typ := ""
		if event, ok := data.(ai.OrderedObject); ok {
			typ, _ = event.Plain()["type"].(string)
		}
		*observed = append(*observed, typ)
		onEvent(len(*observed))
		return nil
	}
}

// assertStreamAbortMatchesPi compares a replayed stream's outcome with the
// run pi recorded.
func assertStreamAbortMatchesPi(t *testing.T, run streamAbortRun, observed, events []string, final *ai.AssistantMessage) {
	t.Helper()
	if !slices.Equal(observed, run.Observed) && (len(observed) > 0 || len(run.Observed) > 0) {
		t.Errorf("observed %v, pi %v", observed, run.Observed)
	}
	if !slices.Equal(events, run.Events) {
		t.Errorf("events %v, pi %v", events, run.Events)
	}
	if string(final.StopReason) != run.StopReason || final.ErrorMessage != run.ErrorMessage {
		t.Errorf("stop %s %q, pi %s %q", final.StopReason, final.ErrorMessage, run.StopReason, run.ErrorMessage)
	}
	gotJSON, err := json.Marshal(final.Content)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(gotJSON, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(run.Content), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("content %s, pi %s", gotJSON, run.Content)
	}
	var diagnostics []string
	for _, d := range final.Diagnostics {
		diagnostics = append(diagnostics, d.Type)
	}
	if !slices.Equal(diagnostics, run.Diagnostics) && (len(diagnostics) > 0 || len(run.Diagnostics) > 0) {
		t.Errorf("diagnostics %v, pi %v", diagnostics, run.Diagnostics)
	}
}

// drain collects a stream's event types and its result.
func drain(stream *ai.AssistantMessageEventStream) ([]string, *ai.AssistantMessage) {
	var events []string
	for ev := range stream.Events() {
		events = append(events, string(ev.Type))
	}
	return events, stream.Result()
}

// TestStreamAbortMatchesPi replays each captured run. Once the body is being
// read, pi's anthropic adapter checks its signal before each read — "Request
// was aborted", only after every event the last read held is handled — and a
// read the abort rejects fails with undici's AbortError, "This operation was
// aborted"; pi-messages never checks its signal, so every abort there is the
// rejected read's AbortError (fetch errors the body's stream, so a chunk that
// had already arrived is never read), or the rejected fetch's when the
// response had not arrived. Those runs' body is heldBody as the port's own
// client delivers it (ownClientBody), so each segment is exactly one read,
// as it was for pi. A body that has ended before it is read is no exception:
// undici reports its end on a read of its own, which an abort before it
// rejects, so pi-messages' and google's next read after an abort on the
// last event fails with the AbortError, and anthropic's own check comes
// first. An abort while an error body is read is the SDKs' thrown
// error caught by pi's retryProviderRequest, which says "Request aborted"
// first thing, and pi-messages' rejected text() read. A connection that
// drops mid-body, with no abort, fails the read with undici's TypeError
// "terminated". A custom fetch is pi's caller's: its rejection and its body's
// errors are its own, abort or no abort, and a body that ignores the abort is
// read on to its end by pi-messages, which never checks the signal. The openai
// SDK (7.19.0) checks its signal before every line and races each body read
// against it, so both openai loops dispatch nothing after the abort — not what
// the read in hand still holds — and end on a custom body that ignores it,
// whether that body delivers on or hangs.
func TestStreamAbortMatchesPi(t *testing.T) {
	for _, run := range loadStreamAbortCapture(t) {
		t.Run(run.API+"/"+run.Mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var observed []string
			cancelOn := 0
			switch run.Mode {
			case "an abort from the callback with events left in its read", "an abort from the callback with the next read already in",
				"a custom fetch's body ignores an abort from the callback":
				cancelOn = 1
			case "an abort from the callback on its read's last event",
				"an abort from the callback on the last event of a body already complete":
				cancelOn = len(run.Observed)
			}
			opts := ai.StreamOptions{OnProviderStreamEvent: observedTypes(&observed, func(seen int) {
				if seen == cancelOn {
					cancel()
				}
			})}
			baseURL := "http://pi.invalid"
			switch run.Mode {
			case "an abort while a read is pending":
				opts.HTTPClient = heldDoer{ownClientBody(ctx, &heldBody{ctx: ctx, segments: slices.Clone(run.Segments), cancelOnHold: cancel})}
			case "an abort from the callback with events left in its read", "an abort from the callback on its read's last event",
				"an abort from the callback with the next read already in":
				opts.HTTPClient = heldDoer{ownClientBody(ctx, &heldBody{ctx: ctx, segments: slices.Clone(run.Segments)})}
			case "an abort from the callback on the last event of a body already complete":
				// The server writes the head, each segment as an HTTP chunk and
				// the terminating chunk in one write, and holds the connection:
				// net/http then reads the body's last bytes and its end
				// together, where undici's reader reports the end on a read of
				// its own.
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
					for _, segment := range run.Segments {
						fmt.Fprintf(rw, "%x\r\n%s\r\n", len(segment), segment)
					}
					fmt.Fprint(rw, "0\r\n\r\n")
					rw.Flush()
					<-release
				}))
				defer server.Close()
				defer close(release)
				baseURL = server.URL
			case "a custom fetch rejects once the signal aborts":
				opts.HTTPClient = abortingDoer{cancel, errors.New(run.CustomFetchError)}
			case "a custom fetch's body ignores an abort from the callback":
				// Every segment is a read of its own, abort or no abort.
				opts.HTTPClient = heldDoer{io.NopCloser(&readsReader{reads: slices.Clone(run.Segments)})}
			case "a custom fetch's body fails once the signal aborts":
				opts.HTTPClient = heldDoer{&abortingBody{segment: run.Segments[0], cancel: cancel, err: errors.New(run.CustomBodyError)}}
			case "a custom fetch's body hangs, ignoring an abort while a read is pending":
				// The body's next read would wait past the test; the openai SDK
				// races it against the signal.
				release := make(chan struct{})
				defer close(release)
				opts.HTTPClient = heldDoer{&hangingBody{segment: run.Segments[0], cancel: cancel, release: release}}
			case "an abort while an error body is read", "an abort while a retryable error body is read":
				// The server answers the run's status (with its retry-after)
				// and the start of the body and holds; the abort lands 100ms
				// on, while the adapter reads the rest. (Were it to land before
				// the response, pi's message would be the same.)
				opts.MaxRetries = run.MaxRetries
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					if run.RetryAfter != "" {
						w.Header().Set("Retry-After", run.RetryAfter)
					}
					w.WriteHeader(run.Status)
					io.WriteString(w, run.Segments[0])
					w.(http.Flusher).Flush()
					time.AfterFunc(100*time.Millisecond, cancel)
					<-r.Context().Done()
				}))
				defer server.Close()
				baseURL = server.URL
			case "an already-aborted signal", "an abort before the response arrives":
				// The server never answers; the abort lands before the call,
				// or once the request has reached the server.
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					cancel()
					<-r.Context().Done()
				}))
				defer server.Close()
				baseURL = server.URL
				if run.Mode == "an already-aborted signal" {
					cancel()
				}
			case "the request gets no response: the connection is refused":
				baseURL = "http://127.0.0.1:1"
			case "the request gets no response: the connection is refused at a /timeout path":
				baseURL = "http://127.0.0.1:1/timeout"
			case "the request gets no response: the server closes the connection":
				baseURL = serveRawOnce(t, "")
			case "the request gets no response: the response is not HTTP":
				baseURL = serveRawOnce(t, "NOT HTTP\r\n\r\n")
			case "the request gets no response: undici's client refuses a header value":
				baseURL = serveRawOnce(t, "")
				opts.Headers = ai.ProviderHeaders{}
				for name, value := range run.Headers {
					opts.Headers[name] = ai.HeaderValue(value)
				}
			case "the request gets no response: timeoutMs passes first":
				// The server reads the request and never answers.
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					<-r.Context().Done()
				}))
				defer server.Close()
				baseURL = server.URL
				opts.TimeoutMs = run.TimeoutMs
			case "a custom fetch rejects", "a custom fetch rejects with a timeout":
				opts.HTTPClient = rejectingDoer{errors.New(run.CustomFetchError)}
			case "a custom fetch rejects with an AbortError of its own":
				// A Go client's own abort is its context's.
				opts.HTTPClient = rejectingDoer{context.Canceled}
			case "a custom fetch's body fails mid-body":
				// A custom client is pi's custom fetch: its body's own error is
				// the stream's.
				opts.HTTPClient = heldDoer{io.NopCloser(io.MultiReader(strings.NewReader(run.Segments[0]), iotest.ErrReader(errors.New(run.CustomBodyError))))}
			case "a custom fetch's retryable error body fails mid-body":
				// The SDK quotes its body read's own error, read as it is.
				opts.MaxRetries = run.MaxRetries
				opts.HTTPClient = errorBodyDoer{
					status:     run.Status,
					retryAfter: run.RetryAfter,
					body:       io.NopCloser(io.MultiReader(strings.NewReader(run.Segments[0]), iotest.ErrReader(errors.New(run.CustomBodyError)))),
				}
			case "the connection drops mid-body", "the connection drops while an error body is read", "the connection drops while a retryable error body is read":
				// The server writes the head (with the run's retry-after) and
				// one chunk, then closes the connection inside the chunked body.
				opts.MaxRetries = run.MaxRetries
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					contentType := "text/event-stream"
					if run.Status != http.StatusOK {
						contentType = "application/json"
					}
					retryAfter := ""
					if run.RetryAfter != "" {
						retryAfter = "Retry-After: " + run.RetryAfter + "\r\n"
					}
					fmt.Fprintf(rw, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\n%sTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n",
						run.Status, http.StatusText(run.Status), contentType, retryAfter, len(run.Segments[0]), run.Segments[0])
					rw.Flush()
				}))
				defer server.Close()
				baseURL = server.URL
			default:
				t.Fatalf("no replay for mode %q", run.Mode)
			}
			events, final := drain(streamAbortAdapter(t, ctx, run.API, baseURL, opts))
			assertStreamAbortMatchesPi(t, run, observed, events, final)
		})
	}
}

// triggerBody wraps a real response body: once armed, the next read cancels
// the request's context as it starts, so the abort lands while net/http's
// read is pending.
type triggerBody struct {
	io.ReadCloser
	armed  *atomic.Bool
	cancel context.CancelFunc
}

func (b triggerBody) Read(p []byte) (int, error) {
	if b.armed.Load() {
		b.cancel()
	}
	return b.ReadCloser.Read(p)
}

// triggerDoer sends through a fresh net/http client and wraps each response
// body in a triggerBody, read as the port's own client's (ownClientBody).
type triggerDoer struct {
	armed  *atomic.Bool
	cancel context.CancelFunc
}

func (d triggerDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body = ownClientBody(req.Context(), triggerBody{resp.Body, d.armed, d.cancel})
	return resp, nil
}

// TestStreamAbortDuringANetHTTPReadMatchesPi is "an abort while a read is
// pending" over a real connection: the server writes the segment and holds,
// and the abort lands during net/http's next body read, which then fails with
// the transport's error — pi's read rejects with undici's AbortError.
func TestStreamAbortDuringANetHTTPReadMatchesPi(t *testing.T) {
	for _, run := range loadStreamAbortCapture(t) {
		if run.Mode != "an abort while a read is pending" {
			continue
		}
		t.Run(run.API, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, run.Segments[0])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var armed atomic.Bool
			var observed []string
			opts := ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{HTTPClient: triggerDoer{&armed, cancel}},
				// Every event of the segment has been read once the last one
				// is observed, so the next read waits on the held connection.
				OnProviderStreamEvent: observedTypes(&observed, func(seen int) { armed.Store(seen == len(run.Observed)) }),
			}
			events, final := drain(streamAbortAdapter(t, ctx, run.API, strings.TrimSuffix(server.URL, "/"), opts))
			assertStreamAbortMatchesPi(t, run, observed, events, final)
		})
	}
}
