package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// openaiStreamCaptureFile is what testdata/openai-stream/capture.mts recorded
// from pi's source at 87f9a3641 and the openai SDK its lockfile pins for pi-ai
// (7.19.0, nested under packages/ai since ab30693d6): how the
// SDK's stream iterator and pi's two openai adapters read the same SSE bodies.
const openaiStreamCaptureFile = "testdata/openai-stream/openai-stream-87f9a3641.json"

type openaiStreamOutcome struct {
	Observed        []string `json:"observed"`
	SameModel       bool     `json:"sameModel"`
	OnResponseCalls int      `json:"onResponseCalls"`
	StopReason      string   `json:"stopReason"`
	ErrorMessage    string   `json:"errorMessage"`
	Text            string   `json:"text"`
	// ResponseID and RawStopReason are String() of pi's values, "" for null,
	// undefined and a value String() throws on: pi assigns whatever the
	// provider sent.
	ResponseID    string `json:"responseId"`
	RawStopReason string `json:"rawStopReason"`
	// Usage holds each usage member as String() of a number, and as
	// "<typeof>:<String()>" of anything else pi holds there.
	Usage map[string]string `json:"usage"`
	// ToolCalls are the tool calls, JSON.stringify'd, id and name as String().
	ToolCalls []string `json:"toolCalls"`
	// Thinking are the thinking blocks, JSON.stringify'd, the signature as
	// String().
	Thinking []string `json:"thinking"`
	// events are the Go stream's event types, in order; the capture does not
	// record pi's.
	events []ai.EventType
}

type openaiStreamThrown struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// openaiSDKReading is what the SDK's stream iterator yielded (each item
// JSON.stringify'd) and what it threw.
type openaiSDKReading struct {
	Yields []string            `json:"yields"`
	Threw  *openaiStreamThrown `json:"threw"`
}

type openaiStreamRow struct {
	// The row's body is SSE, or SSEBase64 where it carries bytes that are
	// not UTF-8 (body).
	SSE       string `json:"sse"`
	SSEBase64 string `json:"sseBase64"`
	SDK       *struct {
		openaiSDKReading
		// Aborted and ReadFailed are the body read to its end and then
		// failing, while the stream's signal is live: with an AbortError, as
		// a custom fetch's body can throw one, and with TypeError
		// "terminated". AbortedOnFirst is the body read whole with the
		// stream's own signal aborted as its first item is yielded.
		Aborted        openaiSDKReading `json:"aborted"`
		ReadFailed     openaiSDKReading `json:"readFailed"`
		AbortedOnFirst openaiSDKReading `json:"abortedOnFirst"`
	} `json:"sdk"`
	Completions *openaiStreamOutcome `json:"completions"`
	Responses   *openaiStreamOutcome `json:"responses"`
}

type openaiStreamCapture struct {
	Sha         string                     `json:"sha"`
	OpenAI      string                     `json:"openai"`
	Dispatch    map[string]openaiStreamRow `json:"dispatch"`
	Completions map[string]openaiStreamRow `json:"completions"`
	Responses   map[string]openaiStreamRow `json:"responses"`
	Pricing     map[string]struct {
		SSE         string  `json:"sse"`
		ServiceTier string  `json:"serviceTier"`
		StopReason  string  `json:"stopReason"`
		CostTotal   float64 `json:"costTotal"`
	} `json:"pricing"`
	Hooks map[string]struct {
		Adapter string `json:"adapter"`
		SSE     string `json:"sse"`
		Status  int    `json:"status"`
		// Hold keeps the connection open after the body; AbortOnEvent aborts
		// the request as the observer sees that event (1-based, 0 never).
		Hold         bool `json:"hold"`
		AbortOnEvent int  `json:"abortOnEvent"`
		// CustomNullBody answers the request with a custom fetch's
		// `new Response(null, { status })` instead of the server.
		CustomNullBody bool                `json:"customNullBody"`
		Outcome        openaiStreamOutcome `json:"outcome"`
	} `json:"hooks"`
	// Lines is the SDK's LineDecoder over chunk sequences that cut a line
	// ending across reads: the lines it made of them, flushed at the end.
	Lines map[string]struct {
		Chunks []string `json:"chunks"`
		Lines  []string `json:"lines"`
	} `json:"lines"`
}

// body is the row's SSE body.
func (r openaiStreamRow) body(t *testing.T) string {
	t.Helper()
	if r.SSEBase64 == "" {
		return r.SSE
	}
	b, err := base64.StdEncoding.DecodeString(r.SSEBase64)
	if err != nil {
		t.Fatalf("sseBase64: %v; rerun capture.mts", err)
	}
	return string(b)
}

func loadOpenAIStreamCapture(t *testing.T) openaiStreamCapture {
	t.Helper()
	data, err := os.ReadFile(openaiStreamCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var c openaiStreamCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s: %v; rerun capture.mts", openaiStreamCaptureFile, err)
	}
	if c.OpenAI != "7.19.0" {
		t.Fatalf("%s was captured with openai %s; 87f9a3641's package-lock.json locks 7.19.0 for pi-ai", openaiStreamCaptureFile, c.OpenAI)
	}
	return c
}

// openaiStreamModel is the capture's model for adapter "completions" or
// "responses".
func openaiStreamModel(adapter, baseURL string) *ai.Model {
	if adapter == "completions" {
		return &ai.Model{
			ID: "openrouter/auto", Name: "OpenRouter Auto", Api: ai.APIOpenAICompletions, Provider: "openrouter",
			BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 200000, MaxTokens: 8192,
		}
	}
	return &ai.Model{
		ID: "gpt-5-mini", Name: "GPT-5 Mini", Api: ai.APIOpenAIResponses, Provider: "openai",
		BaseURL: baseURL, Reasoning: true, Input: []string{"text"}, ContextWindow: 400000, MaxTokens: 128000,
	}
}

// openaiStreamHooks are capture.mts's Hooks: which of the stream's hooks fail,
// and whether the server holds the connection open after the body.
type openaiStreamHooks struct {
	failOnResponse bool
	// failOnEvent is "throw" (the provider stream event observer fails) or
	// "abort-then-throw" (it cancels the request first).
	failOnEvent string
	// abortOnEvent cancels the request, without failing, as the observer sees
	// that event (1-based).
	abortOnEvent int
	hold         bool
	// customNullBody answers the request with a custom HTTPClient's nil Body
	// (nullBodyDoer) instead of the server.
	customNullBody bool
}

// nullBodyDoer is a custom HTTPClient that answers every request with this
// status and a nil Body: pi's custom fetch answering with
// `new Response(null, { status })`.
type nullBodyDoer int

func (status nullBodyDoer) Do(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(status), Header: http.Header{}, Request: r}, nil
}

// runOpenAIStreamAdapter streams body, served with status, through the Go
// adapter the capture calls adapter, the way capture.mts's piRun does.
func runOpenAIStreamAdapter(t *testing.T, adapter string, status int, body string, hooks openaiStreamHooks) openaiStreamOutcome {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusOK {
			w.Header().Set("content-type", "text/event-stream")
		} else {
			w.Header().Set("content-type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		if hooks.hold {
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
				t.Errorf("the client never gave up on the held connection")
			}
		}
	}))
	t.Cleanup(server.Close)
	model := openaiStreamModel(adapter, server.URL+"/v1")
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "k"
	if hooks.customNullBody {
		opts.HTTPClient = nullBodyDoer(status)
	}
	var observed []string
	sameModel := true
	opts.OnProviderStreamEvent = func(data any, eventModel *ai.Model) error {
		text, err := jstext.Stringify(data)
		if err != nil {
			t.Errorf("observed %#v, which has no JSON form: %v", data, err)
		}
		observed = append(observed, text)
		sameModel = sameModel && eventModel == model
		if hooks.abortOnEvent == len(observed) {
			cancel()
		}
		if hooks.failOnEvent == "abort-then-throw" {
			cancel()
		}
		if hooks.failOnEvent != "" {
			return errors.New("observer boom")
		}
		return nil
	}
	onResponseCalls := 0
	opts.OnResponse = func(ai.ProviderResponse, *ai.Model) error {
		onResponseCalls++
		if hooks.failOnResponse {
			return errors.New("response veto")
		}
		return nil
	}
	var stream *ai.AssistantMessageEventStream
	if adapter == "completions" {
		stream = StreamSimpleOpenAICompletions(ctx, model, req, opts)
	} else {
		stream = StreamSimpleOpenAIResponses(ctx, model, req, opts)
	}
	var events []ai.EventType
	for e := range stream.Events() {
		events = append(events, e.Type)
	}
	final := stream.Result()
	outcome := openaiStreamOutcome{
		events:          events,
		Observed:        observed,
		SameModel:       sameModel,
		OnResponseCalls: onResponseCalls,
		StopReason:      string(final.StopReason),
		ErrorMessage:    final.ErrorMessage,
		Text:            jstrimText(final),
		ResponseID:      final.ResponseID,
		RawStopReason:   final.RawStopReason,
		Usage: map[string]string{
			"input":       strconv.Itoa(final.Usage.Input),
			"output":      strconv.Itoa(final.Usage.Output),
			"cacheRead":   strconv.Itoa(final.Usage.CacheRead),
			"cacheWrite":  strconv.Itoa(final.Usage.CacheWrite),
			"reasoning":   strconv.Itoa(final.Usage.Reasoning),
			"totalTokens": strconv.Itoa(final.Usage.TotalTokens),
		},
		ToolCalls: []string{},
		Thinking:  []string{},
	}
	for _, c := range final.Content {
		if th, ok := c.(ai.ThinkingContent); ok {
			text, err := jstext.Stringify(ai.OrderedObject{{Key: "thinking", Value: th.Thinking}, {Key: "thinkingSignature", Value: th.ThinkingSignature}})
			if err != nil {
				t.Fatalf("thinking block %#v has no JSON form: %v", th, err)
			}
			outcome.Thinking = append(outcome.Thinking, text)
		}
		if tc, ok := c.(ai.ToolCall); ok {
			text, err := jstext.Stringify(ai.OrderedObject{{Key: "id", Value: tc.ID}, {Key: "name", Value: tc.Name}, {Key: "arguments", Value: tc.OrderedArguments()}})
			if err != nil {
				t.Fatalf("tool call %#v has no JSON form: %v", tc, err)
			}
			outcome.ToolCalls = append(outcome.ToolCalls, text)
		}
	}
	return outcome
}

// jsIntegerText matches String() of a JS number that is an integer, the
// usage values ai.Usage can hold as pi does.
var jsIntegerText = regexp.MustCompile(`^-?[0-9]+$`)

// compareOpenAIStreamEnding checks how a stream ended against pi's record.
func compareOpenAIStreamEnding(t *testing.T, got, want openaiStreamOutcome) {
	t.Helper()
	if got.StopReason != want.StopReason || got.ErrorMessage != want.ErrorMessage {
		t.Errorf("ended %s %q, pi %s %q", got.StopReason, got.ErrorMessage, want.StopReason, want.ErrorMessage)
	}
	if got.Text != want.Text {
		t.Errorf("text = %q, pi = %q", got.Text, want.Text)
	}
	if got.ResponseID != want.ResponseID {
		t.Errorf("responseId = %q, pi = %q", got.ResponseID, want.ResponseID)
	}
	if got.RawStopReason != want.RawStopReason {
		t.Errorf("rawStopReason = %q, pi = %q", got.RawStopReason, want.RawStopReason)
	}
	if !slices.Equal(got.Thinking, want.Thinking) {
		t.Errorf("thinking:\n got %q\n  pi %q", got.Thinking, want.Thinking)
	}
	if !slices.Equal(got.ToolCalls, want.ToolCalls) {
		t.Errorf("tool calls:\n got %q\n  pi %q", got.ToolCalls, want.ToolCalls)
	}
	// ai.Usage holds integers: where pi holds a fraction, a string or
	// undefined, the port holds what that value converts to, a divergence the
	// row cannot compare.
	for member, value := range want.Usage {
		if jsIntegerText.MatchString(value) && got.Usage[member] != value {
			t.Errorf("usage.%s = %s, pi = %s", member, got.Usage[member], value)
		}
	}
}

// A completed response's service_tier prices it unless it is null or absent
// (pi: `response?.service_tier ?? options.serviceTier`); one that is not a
// tier pi knows — "", a number — prices at ×1 rather than falling back to the
// requested tier.
func TestOpenAIResponsesServiceTierPricingLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	if len(c.Pricing) == 0 {
		t.Fatalf("%s has no pricing rows; rerun capture.mts", openaiStreamCaptureFile)
	}
	for name, row := range c.Pricing {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				_, _ = io.WriteString(w, row.SSE)
			}))
			t.Cleanup(server.Close)
			model := openaiStreamModel("responses", server.URL+"/v1")
			model.Cost = ai.ModelCost{Input: 1} // capture.mts's pricedModel
			opts := &OpenAIResponsesOptions{ServiceTier: row.ServiceTier}
			opts.APIKey = "k"
			req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
			final := StreamOpenAIResponses(context.Background(), model, req, opts).Result()
			if string(final.StopReason) != row.StopReason || final.Usage.Cost.Total != row.CostTotal {
				t.Errorf("ended %s costing %v, pi %s costing %v (%s)", final.StopReason, final.Usage.Cost.Total, row.StopReason, row.CostTotal, final.ErrorMessage)
			}
		})
	}
}

// The responses adapter ends each of its bodies the way pi's does. Among them:
// a `data: null` event reaches pi's processResponsesStream as null, whose
// event.type read throws, and an `error` event fails with a template literal
// over whatever code and message it carries.
func TestOpenAIResponsesEndsLikePi(t *testing.T) {
	for name, row := range loadOpenAIStreamCapture(t).Responses {
		t.Run(name, func(t *testing.T) {
			got := runOpenAIStreamAdapter(t, "responses", http.StatusOK, row.SSE, openaiStreamHooks{})
			compareOpenAIStreamEnding(t, got, *row.Responses)
		})
	}
}

// openaiStreamReads are the ways a test hands a body to the loops: in one read,
// and one byte per read, so that every event separator arrives split across
// reads of the body, for the chunk reader to put back together as the SDK's
// iterSSEChunks does. The capture measured that the SDK reads a body the same
// either way.
var openaiStreamReads = map[string]func(string) io.Reader{
	"whole":    func(body string) io.Reader { return strings.NewReader(body) },
	"one-byte": func(body string) io.Reader { return iotest.OneByteReader(strings.NewReader(body)) },
}

// Both loops read a body into exactly the items the openai SDK's stream
// iterator yields — blank-line dispatch, joined multi-line data, every line
// ending wherever the reads split it, only an exact "[DONE]" ending the
// stream, an event the body ends inside dispatched at its end, "thread.*"
// events wrapped — and fail where the SDK throws: on an item carrying an error
// or an event named "error", with its APIError message, and on data JSON.parse
// rejects, with the SDK's own SyntaxError text.
func TestOpenAIStreamReadsLikeTheSDK(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for name, row := range c.Dispatch {
		for read, reader := range openaiStreamReads {
			t.Run(name+"/"+read, func(t *testing.T) {
				readOpenAIStreamLikeTheSDK(t, nil, reader(row.body(t)), row.SDK.openaiSDKReading, nil, nil)
			})
		}
	}
}

// An event's data is read as JSON.parse reads it, in one pass: accepted where
// node's JSON.parse accepts it, over every row of jstext's JSON.parse capture,
// and otherwise failing with the SDK's own SyntaxError text, whatever
// JSON.parse's reason (openai 7.19.0's Stream no longer rethrows V8's error).
func TestOpenAIStreamJSONParsesLikeTheSDK(t *testing.T) {
	data, err := os.ReadFile("../../internal/jstext/testdata/json-parse-errors-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Rows [][2]*string `json:"rows"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil || len(oracle.Rows) < 1000 {
		t.Fatalf("the JSON.parse capture: %v, %d rows", err, len(oracle.Rows))
	}
	for _, row := range oracle.Rows {
		text, threw := *row[0], row[1]
		value, err := openaiStreamJSON([]byte(text))
		switch {
		case threw == nil && err != nil:
			t.Errorf("openaiStreamJSON(%q): %v; JSON.parse accepts it", text, err)
		case threw != nil && (err == nil || err.Error() != "Error reading response: malformed server-sent event JSON."):
			t.Errorf("openaiStreamJSON(%q) = %#v, %v; JSON.parse throws %q, so the SDK throws its malformed-JSON SyntaxError", text, value, err, *threw)
		}
	}
}

// Data nested deeper than the port's JSON decoder reads fails, as JSON.parse's
// SyntaxError fails pi's stream for this unterminated array, and without
// working out V8's message, whose ported parser takes a stack frame per level:
// at the 16 MiB a line may reach that overflows the goroutine stack and kills
// the process. The message is the port's own (a port limit), so only the
// failure is asserted.
func TestOpenAIStreamJSONFailsOnDeepNesting(t *testing.T) {
	if _, err := openaiStreamJSON(bytes.Repeat([]byte("["), 16<<20)); err == nil {
		t.Fatal("16 MiB of \"[\" parsed; JSON.parse rejects it")
	}
}

// emptyReader returns no data and no error, forever.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

// stutteringReader returns nothing on every other read, and one byte of r on
// the others.
type stutteringReader struct {
	r     io.Reader
	empty bool
}

func (s *stutteringReader) Read(p []byte) (int, error) {
	if s.empty = !s.empty; s.empty || len(p) == 0 {
		return 0, nil
	}
	return s.r.Read(p[:1])
}

// A body whose reads keep returning nothing fails the stream, as bufio.Scanner
// fails its reader, rather than spinning: the chunk reader loops on its reads
// until it has an event, out of the scanner's sight. Empty reads between ones
// that return data do not add up.
func TestOpenAIStreamBrokenBodyFails(t *testing.T) {
	body := strings.Repeat("data: {\"id\":\"a\"}\n\n", 20)
	items := 0
	err := iterateOpenAIStream(&stutteringReader{r: strings.NewReader(body)}, context.Background(), func(openaiStreamItem) error { items++; return nil })
	if err != nil || items != 20 {
		t.Fatalf("a body with an empty read between each byte: %d items, err %v; want 20 and no error", items, err)
	}

	done := make(chan error, 1)
	go func() {
		body := io.MultiReader(strings.NewReader("data: {\"id\":\"a\"}\n\n"), emptyReader{})
		done <- iterateOpenAIStream(body, context.Background(), func(openaiStreamItem) error { return nil })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrNoProgress) || !strings.Contains(err.Error(), "report it with the provider") {
			t.Fatalf("err = %v, want the port's guard wrapping io.ErrNoProgress", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("iterateOpenAIStream still reading a body that returns nothing forever")
	}
}

// A line past the reader's limit fails the stream with the port's own error,
// which says the limit is the port's and what to report: the SDK's
// LineDecoder reads a line of any length.
func TestOpenAIStreamLineLimitSaysItIsThePorts(t *testing.T) {
	body := "data: {\"id\":\"" + strings.Repeat("x", maxOpenAISSELine) + "\"}\n\n"
	err := iterateOpenAIStream(strings.NewReader(body), context.Background(), func(openaiStreamItem) error { return nil })
	want := "an openai stream line is longer than the port's 16 MiB limit (pi reads a line of any length); this is a port limit, report it with the provider and model: bufio.Scanner: token too long"
	if err == nil || err.Error() != want || !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want %q wrapping bufio.ErrTooLong", err, want)
	}
	under := "data: {\"id\":\"" + strings.Repeat("x", maxOpenAISSELine-20) + "\"}\n\n"
	if err := iterateOpenAIStream(strings.NewReader(under), context.Background(), func(openaiStreamItem) error { return nil }); err != nil {
		t.Fatalf("a line just under the limit: %v", err)
	}
}

// The line splitter ends lines where the SDK's LineDecoder does wherever the
// reads cut a line ending: "\r\n" is one line ending even when a read ends
// between its two bytes — a "\r" that ends what has been read ends its line at
// once, and a "\n" beginning the next read is skipped. readOpenAISSE hands the
// splitter iterSSEChunks' pieces, and a piece can end in a "\r" whose "\n"
// begins the next one (findDoubleNewlineIndex counts a "\r" at the end as a
// whole line ending); its scanner also meets the cut where its buffer ends
// inside a piece, in an event of 64 KiB or more. Here the splitter gets the
// capture's chunks one per read, as the LineDecoder did.
func TestOpenAISSELinesSplitLikeTheSDK(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	if len(c.Lines) == 0 {
		t.Fatalf("%s has no lines rows; rerun capture.mts", openaiStreamCaptureFile)
	}
	for name, row := range c.Lines {
		t.Run(name, func(t *testing.T) {
			scanner := bufio.NewScanner(&openaiChunkedBody{chunks: slices.Clone(row.Chunks)})
			var lines openaiSSELines
			scanner.Split(lines.split)
			var got []string
			for scanner.Scan() {
				got = append(got, scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, row.Lines) {
				t.Errorf("lines of %q:\n got %q\nsdk %q", row.Chunks, got, row.Lines)
			}
		})
	}
}

// openaiChunkedBody is a body that arrives in the given chunks, one per read.
type openaiChunkedBody struct{ chunks []string }

func (b *openaiChunkedBody) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	if b.chunks[0] = b.chunks[0][n:]; b.chunks[0] == "" {
		b.chunks = b.chunks[1:]
	}
	return n, nil
}

// A body read that fails with an abort of the body's own while the request is
// live — context.Canceled from a custom client's body, the Go stand-in for the
// AbortError a custom fetch's body throws — ends the reading without an error:
// the SDK's Stream swallows a transport abort, so pi's adapter goes on to its
// post-loop checks. Everything read before it is dispatched, except what its
// iterSSEChunks held back past the last event separator, which only the
// body's end would have passed on.
func TestOpenAIStreamAbortErrorReadEndsLikeTheSDK(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for name, row := range c.Dispatch {
		for read, reader := range openaiStreamReads {
			t.Run(name+"/"+read, func(t *testing.T) {
				body := io.MultiReader(reader(row.body(t)), iotest.ErrReader(context.Canceled))
				// The request stays live: context.Background can never be
				// done, so the reads are not raced against it
				// (newOpenAIAbortableBody).
				readOpenAIStreamLikeTheSDK(t, context.Background(), body, row.SDK.Aborted, nil, nil)
			})
		}
	}
}

// Once the request's context is done the reading ends at once, and without an
// error: the SDK checks its signal before every line and races every read
// against it, so nothing is dispatched after the item the abort came on — not
// even what has already been read. Here the context is cancelled as the first
// item is yielded, as the capture aborts the Stream's signal.
func TestOpenAIStreamAbortEndsTheReadingLikeTheSDK(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for name, row := range c.Dispatch {
		for read, reader := range openaiStreamReads {
			t.Run(name+"/"+read, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				onItem := func(n int) {
					if n == 1 {
						cancel()
					}
				}
				readOpenAIStreamLikeTheSDK(t, ctx, reader(row.body(t)), row.SDK.AbortedOnFirst, nil, onItem)
			})
		}
	}
}

// A body read that fails while the request is live fails the stream with the
// read's error, after dispatching exactly what the SDK dispatches before its
// read throws.
func TestOpenAIStreamFailedReadLikeTheSDK(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	errTerminated := errors.New("terminated")
	for name, row := range c.Dispatch {
		for read, reader := range openaiStreamReads {
			t.Run(name+"/"+read, func(t *testing.T) {
				body := io.MultiReader(reader(row.body(t)), iotest.ErrReader(errTerminated))
				readOpenAIStreamLikeTheSDK(t, context.Background(), body, row.SDK.ReadFailed, errTerminated, nil)
			})
		}
	}
}

// readOpenAIStreamLikeTheSDK iterates body and compares the items and the
// error with what the SDK made of it. readErr, when set, is the error the
// body's read fails with, which stands for the SDK's TypeError "terminated".
// onItem, when set, is called with the count of items yielded so far, as each
// is yielded.
func readOpenAIStreamLikeTheSDK(t *testing.T, ctx context.Context, body io.Reader, want openaiSDKReading, readErr error, onItem func(n int)) {
	t.Helper()
	var got []string
	err := iterateOpenAIStream(body, ctx, func(item openaiStreamItem) error {
		text, err := jstext.Stringify(item.value)
		if err != nil {
			t.Fatalf("yielded item %#v has no JSON form: %v", item.value, err)
		}
		got = append(got, text)
		if onItem != nil {
			onItem(len(got))
		}
		return nil
	})
	switch {
	case want.Threw != nil && readErr != nil && want.Threw.Name == "TypeError" && want.Threw.Message == readErr.Error():
		if !errors.Is(err, readErr) {
			t.Errorf("error = %v, want the failed read's %v", err, readErr)
		}
	case want.Threw != nil:
		if err == nil || err.Error() != want.Threw.Message {
			t.Errorf("error = %v, sdk threw %s %q", err, want.Threw.Name, want.Threw.Message)
		}
	case err != nil:
		t.Errorf("error = %v, sdk threw nothing", err)
	}
	if !slices.Equal(got, want.Yields) {
		t.Errorf("items:\n got %q\nsdk %q", got, want.Yields)
	}
}

// Both adapters end a dispatch body the way pi's do: the stop reason, the
// error message — the SDK's APIError message for an error item, with
// completions appending error.metadata.raw as String() writes it — the text
// and the response id.
func TestOpenAIStreamEndsLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for name, row := range c.Dispatch {
		for adapter, want := range map[string]*openaiStreamOutcome{"completions": row.Completions, "responses": row.Responses} {
			t.Run(name+"/"+adapter, func(t *testing.T) {
				compareOpenAIStreamEnding(t, runOpenAIStreamAdapter(t, adapter, http.StatusOK, row.body(t), openaiStreamHooks{}), *want)
			})
		}
	}
}

// A request cancelled while the server holds the connection open ends like
// pi's: the SDK's Stream swallows its body read's AbortError, so each adapter
// runs its post-loop checks — completions finishes its blocks and fails
// "Request was aborted"; responses fails first for a missing terminal event,
// and with "Request was aborted" only once it saw one.
func TestOpenAIStreamAbortLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for _, name := range []string{"completions/abort-held", "completions/abort-held-after-finish", "responses/abort-held", "responses/abort-held-after-terminal"} {
		t.Run(name, func(t *testing.T) {
			row, ok := c.Hooks[name]
			if !ok || !row.Hold || row.AbortOnEvent == 0 {
				t.Fatalf("%s has no held, aborting hooks row %s; rerun capture.mts", openaiStreamCaptureFile, name)
			}
			got := runOpenAIStreamAdapter(t, row.Adapter, row.Status, row.SSE, openaiStreamHooks{hold: true, abortOnEvent: row.AbortOnEvent})
			compareOpenAIStreamObserved(t, got, row.Outcome)
			compareOpenAIStreamEnding(t, got, row.Outcome)
		})
	}
}

// A body that ends in "[DONE]" and is then held open ends the stream there,
// with no abort: the SDK reads nothing after "[DONE]" (it breaks out of its
// loop and cancels the body), so neither adapter waits on the connection the
// server still holds.
func TestOpenAIStreamDoneEndsTheReadingLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for _, adapter := range []string{"completions", "responses"} {
		t.Run(adapter, func(t *testing.T) {
			row, ok := c.Hooks[adapter+"/done-then-held"]
			if !ok || !row.Hold || row.AbortOnEvent != 0 {
				t.Fatalf("%s has no held, non-aborting hooks row %s/done-then-held; rerun capture.mts", openaiStreamCaptureFile, adapter)
			}
			got := runOpenAIStreamAdapter(t, adapter, row.Status, row.SSE, openaiStreamHooks{hold: true})
			compareOpenAIStreamObserved(t, got, row.Outcome)
			compareOpenAIStreamEnding(t, got, row.Outcome)
		})
	}
}

// pi awaits onResponse only once the SDK has a 2xx response, and a throw from it
// fails the stream in both adapters.
func TestOpenAIStreamOnResponseLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for _, name := range []string{"on-response-throws", "http-400", "http-400-on-response-throws"} {
		for _, adapter := range []string{"completions", "responses"} {
			t.Run(adapter+"/"+name, func(t *testing.T) {
				row, ok := c.Hooks[adapter+"/"+name]
				if !ok {
					t.Fatalf("%s has no hooks row %s/%s; rerun capture.mts", openaiStreamCaptureFile, adapter, name)
				}
				got := runOpenAIStreamAdapter(t, adapter, row.Status, row.SSE, openaiStreamHooks{failOnResponse: strings.Contains(name, "on-response-throws")})
				want := row.Outcome
				if got.OnResponseCalls != want.OnResponseCalls {
					t.Errorf("onResponse ran %d times, pi %d", got.OnResponseCalls, want.OnResponseCalls)
				}
				compareOpenAIStreamEnding(t, got, want)
			})
		}
	}
}

// A 2xx response whose body is null fails both adapters as pi's fail: the
// SDK's Stream throws on its first iteration, after pi has awaited onResponse
// and pushed start, before anything reaches the observer. fetch gives a 204 or
// a 205 a null body whatever the server sends — the 205 row's server sends a
// whole stream, which net/http reads — and a custom fetch's null body, a
// custom HTTPClient's nil Body, is null at any status. A non-2xx one reads as
// an empty body, and an empty body is not null: a 200's, which net/http hands
// over as http.NoBody, ends in the adapters' post-loop checks.
func TestOpenAIStreamNullBodyLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for _, name := range []string{"empty-body", "null-body-204", "null-body-205", "custom-fetch-null-body", "custom-fetch-null-body-500"} {
		for _, adapter := range []string{"completions", "responses"} {
			t.Run(adapter+"/"+name, func(t *testing.T) {
				row, ok := c.Hooks[adapter+"/"+name]
				if !ok {
					t.Fatalf("%s has no hooks row %s/%s; rerun capture.mts", openaiStreamCaptureFile, adapter, name)
				}
				got := runOpenAIStreamAdapter(t, adapter, row.Status, row.SSE, openaiStreamHooks{customNullBody: row.CustomNullBody})
				want := row.Outcome
				if got.OnResponseCalls != want.OnResponseCalls {
					t.Errorf("onResponse ran %d times, pi %d", got.OnResponseCalls, want.OnResponseCalls)
				}
				compareOpenAIStreamObserved(t, got, want)
				compareOpenAIStreamEnding(t, got, want)
				// pi pushes start right after onResponse, and a response that
				// never reaches onResponse fails with its error alone.
				wantEvents := []ai.EventType{ai.EventStart, ai.EventError}
				if want.OnResponseCalls == 0 {
					wantEvents = []ai.EventType{ai.EventError}
				}
				if !slices.Equal(got.events, wantEvents) {
					t.Errorf("events = %v, pi's %v", got.events, wantEvents)
				}
			})
		}
	}
}

// compareOpenAIStreamObserved checks the provider stream events against pi's:
// every item pi's loop iterated, in order, as JSON.stringify writes it — key
// order included — each with the stream's own model.
func compareOpenAIStreamObserved(t *testing.T, got, want openaiStreamOutcome) {
	t.Helper()
	if !slices.Equal(got.Observed, want.Observed) {
		t.Errorf("observed:\n got %q\n pi %q", got.Observed, want.Observed)
	}
	if !got.SameModel || !want.SameModel {
		t.Errorf("observer's model is the stream's: got %v, pi %v", got.SameModel, want.SameModel)
	}
}

// Both adapters hand OnProviderStreamEvent every item pi's onProviderStreamEvent
// receives (upstream 002fc8385), before normalizing it: chunks with no choices,
// null, scalars and arrays, events the loop ignores, the event it fails on —
// and nothing the SDK does not yield (an error item, an event named "error",
// anything after [DONE]).
func TestOpenAIStreamObservesLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	type run struct {
		adapter string
		sse     string
		want    *openaiStreamOutcome
	}
	runs := map[string]run{}
	for name, row := range c.Dispatch {
		runs["dispatch/"+name+"/completions"] = run{"completions", row.body(t), row.Completions}
		runs["dispatch/"+name+"/responses"] = run{"responses", row.body(t), row.Responses}
	}
	for name, row := range c.Completions {
		runs["completions/"+name] = run{"completions", row.SSE, row.Completions}
	}
	for name, row := range c.Responses {
		runs["responses/"+name] = run{"responses", row.SSE, row.Responses}
	}
	for name, r := range runs {
		t.Run(name, func(t *testing.T) {
			got := runOpenAIStreamAdapter(t, r.adapter, http.StatusOK, r.sse, openaiStreamHooks{})
			compareOpenAIStreamObserved(t, got, *r.want)
			compareOpenAIStreamEnding(t, got, *r.want)
		})
	}
}

// An observer's error fails the stream with its own message, before the event
// it rejected is normalized; when the request was cancelled first, the stream
// ends aborted (pi: stopReason from signal.aborted, errorMessage the throw's).
func TestOpenAIStreamObserverErrorLikePi(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	for _, adapter := range []string{"completions", "responses"} {
		for hook, failOnEvent := range map[string]string{"callback-throws": "throw", "callback-aborts": "abort-then-throw"} {
			t.Run(adapter+"/"+hook, func(t *testing.T) {
				row, ok := c.Hooks[adapter+"/"+hook]
				if !ok {
					t.Fatalf("%s has no hooks row %s/%s; rerun capture.mts", openaiStreamCaptureFile, adapter, hook)
				}
				got := runOpenAIStreamAdapter(t, adapter, row.Status, row.SSE, openaiStreamHooks{failOnEvent: failOnEvent})
				compareOpenAIStreamObserved(t, got, row.Outcome)
				compareOpenAIStreamEnding(t, got, row.Outcome)
			})
		}
	}
}

// Go-only: the observer's error surfaces as the terminal error event with its
// message verbatim, and the event it rejected is never normalized — the text
// it carried produces no text_delta.
func TestOpenAIProviderStreamEventErrorFailsStream(t *testing.T) {
	c := loadOpenAIStreamCapture(t)
	cases := []struct {
		adapter string
		sse     string
		// rejectAt is the observed event carrying the stream's text.
		rejectAt int
	}{
		{"completions", c.Dispatch["blank-line-dispatch"].SSE, 0},
		{"responses", c.Responses["completed"].SSE, 3},
	}
	for _, tc := range cases {
		for _, cancelFirst := range []bool{false, true} {
			name := tc.adapter
			want := ai.StopError
			if cancelFirst {
				name += "/cancelled"
				want = ai.StopAborted
			}
			t.Run(name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("content-type", "text/event-stream")
					_, _ = io.WriteString(w, tc.sse)
				}))
				t.Cleanup(server.Close)
				model := openaiStreamModel(tc.adapter, server.URL+"/v1")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				seen := 0
				opts := &ai.SimpleStreamOptions{}
				opts.APIKey = "k"
				opts.OnProviderStreamEvent = func(any, *ai.Model) error {
					seen++
					if seen <= tc.rejectAt {
						return nil
					}
					if cancelFirst {
						cancel()
					}
					return errors.New("observer boom")
				}
				req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
				var stream *ai.AssistantMessageEventStream
				if tc.adapter == "completions" {
					stream = StreamSimpleOpenAICompletions(ctx, model, req, opts)
				} else {
					stream = StreamSimpleOpenAIResponses(ctx, model, req, opts)
				}
				var events []ai.AssistantMessageEvent
				for ev := range stream.Events() {
					events = append(events, ev)
				}
				for _, ev := range events {
					if ev.Type == ai.EventTextDelta {
						t.Fatalf("text_delta %q pushed for the event the observer rejected", ev.Delta)
					}
				}
				last := events[len(events)-1]
				if last.Type != ai.EventError || last.Error == nil {
					t.Fatalf("stream ended with %s, want an error event", last.Type)
				}
				if last.Reason != want || last.Error.StopReason != want || last.Error.ErrorMessage != "observer boom" {
					t.Fatalf("ended %s/%s %q, want %s \"observer boom\"", last.Reason, last.Error.StopReason, last.Error.ErrorMessage, want)
				}
				if seen != tc.rejectAt+1 {
					t.Fatalf("observer ran %d times, want %d: the stream must stop at its error", seen, tc.rejectAt+1)
				}
			})
		}
	}
}
