package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// baseURLRun is one row of testdata/base-url/base-url-*.json: what pi's
// adapter said for a base URL its request URL could not be made from.
type baseURLRun struct {
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
	// Divergence, when set, says how the WHATWG URL parser and net/url
	// differ on the base URL.
	Divergence   string   `json:"divergence"`
	Events       []string `json:"events"`
	StopReason   string   `json:"stopReason"`
	ErrorMessage string   `json:"errorMessage"`
	OnPayload    int      `json:"onPayload"`
}

// baseURLRequest is one of the capture's requests: what pi's adapter sent a
// recording server for a base URL naming it by {PORT}, or the errorMessage
// it failed with when it sent nothing.
type baseURLRequest struct {
	API     string `json:"api"`
	Label   string `json:"label"`
	BaseURL string `json:"baseUrl"`
	// Divergence, when set, says how the port differs from pi here.
	Divergence   string           `json:"divergence"`
	Request      *recordedRequest `json:"request"`
	ErrorMessage string           `json:"errorMessage"`
	OnPayload    int              `json:"onPayload"`
}

// recordedRequest is a request as the recording server saw it: its request
// line, and its Host and Authorization headers ("" when absent).
type recordedRequest struct {
	Line          string `json:"line"`
	Host          string `json:"host"`
	Authorization string `json:"authorization"`
}

// baseURLHref is one of the capture's hrefs: node's `new URL(input).href`,
// or "Invalid URL".
type baseURLHref struct {
	Input string `json:"input"`
	// Divergence, when set, says how requestURL reads the input otherwise.
	Divergence string `json:"divergence"`
	Href       string `json:"href"`
}

// baseURLCustomFetch is one of the capture's customFetch rows: the URL a
// custom fetch was handed for a base URL with credentials.
type baseURLCustomFetch struct {
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
	URL     string `json:"url"`
}

type baseURLCapture struct {
	Rows        []baseURLRun         `json:"rows"`
	Hrefs       []baseURLHref        `json:"hrefs"`
	CustomFetch []baseURLCustomFetch `json:"customFetch"`
	Requests    []baseURLRequest     `json:"requests"`
}

func loadBaseURLCapture(t *testing.T) baseURLCapture {
	t.Helper()
	raw, err := os.ReadFile("testdata/base-url/base-url-2b0a123de.json")
	if err != nil {
		t.Fatalf("read the base-url capture: %v (regenerate it with testdata/base-url/capture.mts)", err)
	}
	var capture baseURLCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) == 0 || len(capture.Hrefs) == 0 || len(capture.Requests) == 0 {
		t.Fatalf("the base-url capture has %d rows, %d hrefs and %d requests", len(capture.Rows), len(capture.Hrefs), len(capture.Requests))
	}
	for pointer, message := range causeDetailMessages(t, "base-url") {
		var i int
		switch {
		case scanPointer(pointer, "/rows/%d", &i) && i < len(capture.Rows):
			capture.Rows[i].ErrorMessage = message
		case scanPointer(pointer, "/requests/%d", &i) && i < len(capture.Requests):
			capture.Requests[i].ErrorMessage = message
		default:
			t.Fatalf("the cause-detail overlay names %s", pointer)
		}
	}
	return capture
}

// requestURLText is requestURL's result as the capture's hrefs spell it: the
// URL, or the error's message.
func requestURLText(input string) string {
	u, err := requestURL(input)
	if err != nil {
		return err.Error()
	}
	return u
}

// TestRequestURLMatchesWHATWG requires requestURL to make of each captured
// input what node's `new URL(input)` makes of it — its href, or "Invalid
// URL" — where the port claims the WHATWG parser's reading: the input
// cleanup, a special scheme's slashes and backslashes, controls
// percent-encoded, the host lowercased, the port normalized and a default one
// dropped, an empty path read as "/".
func TestRequestURLMatchesWHATWG(t *testing.T) {
	ran := 0
	for _, row := range loadBaseURLCapture(t).Hrefs {
		if row.Divergence != "" {
			continue
		}
		ran++
		if got := requestURLText(row.Input); got != row.Href {
			t.Errorf("requestURL(%q) = %q; new URL: %q", row.Input, got, row.Href)
		}
	}
	if ran < 20 {
		t.Fatalf("only %d agreeing hrefs in the capture", ran)
	}
}

// TestRequestURLDivergencesStillDiffer requires each captured href the port
// reads otherwise (docs/UPSTREAM.md K26) to still differ, so the row moves the
// day it does not.
func TestRequestURLDivergencesStillDiffer(t *testing.T) {
	ran := 0
	for _, row := range loadBaseURLCapture(t).Hrefs {
		if row.Divergence == "" {
			continue
		}
		ran++
		if got := requestURLText(row.Input); got == row.Href {
			t.Errorf("requestURL(%q) is new URL's %q now; retag the row: %s", row.Input, got, row.Divergence)
		}
	}
	if ran == 0 {
		t.Fatal("no divergence hrefs in the capture")
	}
}

// countingPayload is StreamOptions with the capture's key and an OnPayload
// that counts its calls.
func countingPayload(calls *int) ai.StreamOptions {
	return ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
		APIKey: "test-api-key",
		OnPayload: func(any, *ai.Model) (any, error) {
			*calls++
			return nil, nil
		},
	}}
}

// TestInvalidBaseURLMatchesPi streams each captured base URL both parsers
// refuse through its adapter and requires pi's outcome: TypeError "Invalid
// URL", after onPayload for the SDK adapters (their request URL is built in
// the SDK's request) and before it for pi-messages (which builds its URL
// first), and no request sent.
func TestInvalidBaseURLMatchesPi(t *testing.T) {
	ran := 0
	for _, run := range loadBaseURLCapture(t).Rows {
		if run.Divergence != "" {
			continue
		}
		ran++
		t.Run(run.API+"/"+run.BaseURL, func(t *testing.T) {
			onPayload := 0
			events, final := drain(streamAbortAdapter(t, context.Background(), run.API, run.BaseURL, countingPayload(&onPayload)))
			if !slices.Equal(events, run.Events) || string(final.StopReason) != run.StopReason || final.ErrorMessage != run.ErrorMessage {
				t.Errorf("events %v, stop %s %q\npi:    %v, stop %s %q", events, final.StopReason, final.ErrorMessage, run.Events, run.StopReason, run.ErrorMessage)
			}
			if onPayload != run.OnPayload {
				t.Errorf("OnPayload ran %d times; pi %d", onPayload, run.OnPayload)
			}
		})
	}
	if ran < 40 {
		t.Fatalf("only %d agreeing rows in the capture", ran)
	}
}

// TestInvalidBaseURLDivergencesStillDiffer requires each captured divergence
// to still be one: where the WHATWG URL parser refuses a URL net/url takes,
// the port does not say pi's "Invalid URL", and where it takes one net/url
// refuses, the port does. Each is decided by requestURL, so no request is
// sent; the day either parser's reading changes, the row must move.
func TestInvalidBaseURLDivergencesStillDiffer(t *testing.T) {
	ran := 0
	for _, run := range loadBaseURLCapture(t).Rows {
		if run.Divergence == "" {
			continue
		}
		ran++
		t.Run(run.API+"/"+run.BaseURL, func(t *testing.T) {
			piRefuses := run.ErrorMessage == "Invalid URL"
			_, err := requestURL(strings.TrimRight(run.BaseURL, "/") + "/x")
			if portRefuses := err != nil; portRefuses == piRefuses {
				t.Errorf("%s: the port and pi agree now (both refuse: %v); retag the row: %s", run.BaseURL, piRefuses, run.Divergence)
			}
		})
	}
	if ran == 0 {
		t.Fatal("no divergence rows in the capture")
	}
}

// urlRecordingDoer is a custom HTTPClient that records the URL of each
// request it is handed and answers 400.
type urlRecordingDoer struct{ urls *[]string }

func (d urlRecordingDoer) Do(req *http.Request) (*http.Response, error) {
	*d.urls = append(*d.urls, req.URL.String())
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stop here"}}`)),
		Request:    req,
	}, nil
}

// TestCredentialsReachACustomClient streams each captured base URL with
// credentials through a custom HTTPClient — pi's custom fetch, which no
// undici refusal stands in front of — and requires the client to be handed
// the URL pi's custom fetch was handed, credentials included.
func TestCredentialsReachACustomClient(t *testing.T) {
	rows := loadBaseURLCapture(t).CustomFetch
	if len(rows) == 0 {
		t.Fatal("no customFetch rows in the capture")
	}
	for _, row := range rows {
		t.Run(row.API, func(t *testing.T) {
			var urls []string
			opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-api-key", HTTPClient: urlRecordingDoer{&urls}}}
			drain(streamAbortAdapter(t, context.Background(), row.API, row.BaseURL, opts))
			if len(urls) != 1 || urls[0] != row.URL {
				t.Errorf("the custom client was handed %q; pi's custom fetch %q", urls, row.URL)
			}
		})
	}
}

// recordingServer answers every request 400, closing the connection, and
// keeps the last request it received.
type recordingServer struct {
	port string
	mu   sync.Mutex
	last *recordedRequest
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &recordingServer{port: strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *recordingServer) serve(conn net.Conn) {
	defer conn.Close()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	io.Copy(io.Discard, req.Body)
	s.mu.Lock()
	s.last = &recordedRequest{
		Line:          strings.ReplaceAll(fmt.Sprintf("%s %s %s", req.Method, req.RequestURI, req.Proto), s.port, "{PORT}"),
		Host:          strings.ReplaceAll(req.Host, s.port, "{PORT}"),
		Authorization: req.Header.Get("Authorization"),
	}
	s.mu.Unlock()
	const body = `{"error":{"message":"stop here"}}`
	fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

// take returns the request received since the last take, if any.
func (s *recordingServer) take() *recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.last
	s.last = nil
	return last
}

// replayBaseURLRequest streams run's base URL, naming the recording server,
// through its adapter and describes how the outcome differs from pi's: the
// request the server received (or that none came), the error the stream
// ended with when pi sent nothing, and how often OnPayload ran. An error
// after a request was sent is the server's 400, whose text is not compared.
func replayBaseURLRequest(t *testing.T, server *recordingServer, run baseURLRequest) []string {
	t.Helper()
	server.take()
	onPayload := 0
	baseURL := strings.ReplaceAll(run.BaseURL, "{PORT}", server.port)
	_, final := drain(streamAbortAdapter(t, context.Background(), run.API, baseURL, countingPayload(&onPayload)))
	got := server.take()
	var diffs []string
	switch {
	case run.Request != nil && got == nil:
		diffs = append(diffs, fmt.Sprintf("sent nothing (%q); pi sent %+v", final.ErrorMessage, *run.Request))
	case run.Request != nil && *got != *run.Request:
		diffs = append(diffs, fmt.Sprintf("sent %+v; pi sent %+v", *got, *run.Request))
	case run.Request == nil && got != nil:
		diffs = append(diffs, fmt.Sprintf("sent %+v; pi sent nothing and failed %q", *got, run.ErrorMessage))
	case run.Request == nil && strings.ReplaceAll(final.ErrorMessage, server.port, "{PORT}") != run.ErrorMessage:
		diffs = append(diffs, fmt.Sprintf("failed %q; pi %q", final.ErrorMessage, run.ErrorMessage))
	}
	if onPayload != run.OnPayload {
		diffs = append(diffs, fmt.Sprintf("OnPayload ran %d times; pi %d", onPayload, run.OnPayload))
	}
	return diffs
}

// TestBaseURLRequestsMatchPi streams each captured base URL that names the
// recording server and requires the request pi's adapter sent — its request
// line, Host and Authorization — or, where pi sent none, its error: the
// WHATWG URL parser's input handling (leading and trailing C0 controls and
// spaces stripped, tabs and newlines removed anywhere), a special scheme's
// slashes and backslashes read as it reads them, other controls
// percent-encoded, the host lowercased and the port normalized.
func TestBaseURLRequestsMatchPi(t *testing.T) {
	server := newRecordingServer(t)
	ran := 0
	for _, run := range loadBaseURLCapture(t).Requests {
		if run.Divergence != "" {
			continue
		}
		ran++
		t.Run(run.API+"/"+run.Label, func(t *testing.T) {
			for _, d := range replayBaseURLRequest(t, server, run) {
				t.Error(d)
			}
		})
	}
	if ran < 100 {
		t.Fatalf("only %d agreeing requests in the capture", ran)
	}
}

// TestBaseURLRequestDivergencesStillDiffer requires each captured request the
// port is known to get wrong (the parsers differ there; docs/UPSTREAM.md K26)
// to still differ from pi's, so the row moves the day it does not.
func TestBaseURLRequestDivergencesStillDiffer(t *testing.T) {
	server := newRecordingServer(t)
	ran := 0
	for _, run := range loadBaseURLCapture(t).Requests {
		if run.Divergence == "" {
			continue
		}
		ran++
		t.Run(run.API+"/"+run.Label, func(t *testing.T) {
			if diffs := replayBaseURLRequest(t, server, run); len(diffs) == 0 {
				t.Errorf("the port sends what pi sends now; retag the row: %s", run.Divergence)
			}
		})
	}
	if ran == 0 {
		t.Fatal("no divergence requests in the capture")
	}
}
