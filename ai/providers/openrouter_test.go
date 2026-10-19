package providers

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// openRouterCaptureFile is written by testdata/openrouter/capture.mts, which
// runs pi's own openrouter-api.ts and openrouter.ts under node.
const openRouterCaptureFile = "testdata/openrouter/openrouter-d2e7d8b7c.json"

type openRouterCapture struct {
	SHA  string `json:"sha"`
	Cost []struct {
		Pricing json.RawMessage `json:"pricing"`
		Cost    any             `json:"cost"`
	} `json:"cost"`
	Catalog     []*ai.Model `json:"catalog"`
	Personalize []struct {
		Name       string          `json:"name"`
		Credential *ai.Credential  `json:"credential"`
		BaseURL    string          `json:"baseUrl"`
		Status     int             `json:"status"`
		Body       string          `json:"body"`
		Request    *capturedFetch  `json:"request"`
		Result     json.RawMessage `json:"result"`
		Error      *string         `json:"error"`
	} `json:"personalize"`
}

type capturedFetch struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

func loadOpenRouterCapture(t *testing.T) openRouterCapture {
	t.Helper()
	data, err := os.ReadFile(openRouterCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/openrouter/capture.mts)", openRouterCaptureFile, err)
	}
	var capture openRouterCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", openRouterCaptureFile, err)
	}
	if len(capture.Cost) == 0 || len(capture.Personalize) == 0 {
		t.Fatalf("%s has no rows", openRouterCaptureFile)
	}
	return capture
}

// jsCostValue renders a ModelCost as the capture writes pi's: plain JSON
// numbers, with a non-finite one as its String().
func jsCostValue(cost ai.ModelCost) any {
	num := func(f float64) any {
		if math.IsInf(f, 1) {
			return "Infinity"
		}
		if math.IsInf(f, -1) {
			return "-Infinity"
		}
		return f
	}
	out := map[string]any{"input": num(cost.Input), "output": num(cost.Output), "cacheRead": num(cost.CacheRead), "cacheWrite": num(cost.CacheWrite)}
	if len(cost.Tiers) > 0 {
		tiers := make([]any, len(cost.Tiers))
		for i, tier := range cost.Tiers {
			tiers[i] = map[string]any{
				"inputTokensAbove": float64(tier.InputTokensAbove),
				"input":            num(tier.Input), "output": num(tier.Output),
				"cacheRead": num(tier.CacheRead), "cacheWrite": num(tier.CacheWrite),
			}
		}
		out["tiers"] = tiers
	}
	return out
}

func TestOpenRouterCostMatchesPi(t *testing.T) {
	capture := loadOpenRouterCapture(t)
	for i, row := range capture.Cost {
		pricing, err := jstext.Parse(row.Pricing)
		if err != nil {
			t.Fatal(err)
		}
		if got := jsCostValue(openRouterCost(pricing)); !reflect.DeepEqual(got, row.Cost) {
			t.Errorf("row %d: openRouterCost(%s) = %v, pi at %s = %v", i, row.Pricing, got, capture.SHA, row.Cost)
		}
	}
}

// openRouterRequest is the request a test server received.
type openRouterRequest struct {
	method, path string
	header       http.Header
}

// recordingOpenRouterDoer answers every request with one canned response and
// records the request's context deadline — the one thing a server cannot see.
type recordingOpenRouterDoer struct {
	status   int
	body     string
	request  *http.Request
	deadline time.Duration
}

func (d *recordingOpenRouterDoer) Do(req *http.Request) (*http.Response, error) {
	d.request = req
	if deadline, ok := req.Context().Deadline(); ok {
		d.deadline = time.Until(deadline)
	}
	return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader(d.body)), Header: http.Header{}}, nil
}

// The request goes to pi's URL — the configured base URL, trailing slashes
// trimmed, else OpenRouter's — and is bounded by pi's 15-second timeout.
func TestPersonalizeOpenRouterCatalogRequestURLAndDeadline(t *testing.T) {
	capture := loadOpenRouterCapture(t)
	for _, tc := range capture.Personalize {
		if tc.Request == nil {
			continue
		}
		t.Run(tc.Name, func(t *testing.T) {
			doer := &recordingOpenRouterDoer{status: tc.Status, body: tc.Body}
			_, _ = personalizeOpenRouterCatalog(context.Background(), capture.Catalog,
				ai.RefreshModelsContext{Credential: tc.Credential, BaseURL: tc.BaseURL, AllowNetwork: true}, doer)
			if doer.request == nil {
				t.Fatalf("made no request; pi requests %s", tc.Request.URL)
			}
			if got := doer.request.URL.String(); got != tc.Request.URL {
				t.Errorf("request URL %s, pi's %s", got, tc.Request.URL)
			}
			if doer.deadline <= 14*time.Second || doer.deadline > 15*time.Second {
				t.Errorf("request deadline %v out, want pi's 15s timeout", doer.deadline)
			}
		})
	}
}

func TestPersonalizeOpenRouterCatalogMatchesPi(t *testing.T) {
	capture := loadOpenRouterCapture(t)
	for _, tc := range capture.Personalize {
		t.Run(tc.Name, func(t *testing.T) {
			var received *openRouterRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received = &openRouterRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone()}
				w.WriteHeader(tc.Status)
				io.WriteString(w, tc.Body)
			}))
			defer server.Close()
			// The row's base URL on the test server: its path, OpenRouter's
			// default /api/v1 when it has none.
			basePath := "/api/v1"
			if tc.BaseURL != "" {
				parsed, err := url.Parse(tc.BaseURL)
				if err != nil {
					t.Fatal(err)
				}
				basePath = parsed.Path
			}

			catalog := capture.Catalog
			got, err := PersonalizeOpenRouterCatalog(context.Background(), catalog,
				ai.RefreshModelsContext{Credential: tc.Credential, BaseURL: server.URL + basePath, AllowNetwork: true})

			switch {
			case tc.Request == nil && received != nil:
				t.Fatalf("requested %s; pi makes no request", received.path)
			case tc.Request != nil:
				if received == nil {
					t.Fatalf("made no request; pi requests %s", tc.Request.URL)
				}
				want, err := url.Parse(tc.Request.URL)
				if err != nil {
					t.Fatal(err)
				}
				if received.method != http.MethodGet || received.path != want.Path {
					t.Errorf("request = %s %s, pi's = GET %s", received.method, received.path, want.Path)
				}
				// pi's headers, as the server got them; the transport's own
				// (User-Agent, Accept-Encoding) are fetch's business, not pi's.
				for name, value := range tc.Request.Headers {
					if got := received.header.Get(name); got != value {
						t.Errorf("header %s = %q, pi's = %q", name, got, value)
					}
				}
			}

			if tc.Error != nil {
				if err == nil || err.Error() != *tc.Error {
					t.Fatalf("error = %v, pi's = %q", err, *tc.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var unchanged string
			if json.Unmarshal(tc.Result, &unchanged) == nil && unchanged == "unchanged" {
				if len(got) != len(catalog) || (len(got) > 0 && &got[0] != &catalog[0]) {
					t.Fatalf("pi returns the catalog unchanged, got %d models", len(got))
				}
				return
			}
			var want []*ai.Model
			if err := json.Unmarshal(tc.Result, &want); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("got %d models, pi %d", len(got), len(want))
			}
			byID := map[string]*ai.Model{}
			for _, m := range catalog {
				byID[m.ID] = m
			}
			for i := range want {
				g, w := got[i], want[i]
				if g.ID != w.ID || g.ContextWindow != w.ContextWindow || g.MaxTokens != w.MaxTokens || !reflect.DeepEqual(g.Cost, w.Cost) {
					t.Errorf("model %d = {%s %d %d %+v}, pi's = {%s %d %d %+v}", i, g.ID, g.ContextWindow, g.MaxTokens, g.Cost, w.ID, w.ContextWindow, w.MaxTokens, w.Cost)
				}
				if !ai.IsModelType(g, ai.ModelTypeChat) && g != byID[g.ID] {
					t.Errorf("model %s: pi returns non-chat models as they are", g.ID)
				}
				if ai.IsModelType(g, ai.ModelTypeChat) && g == byID[g.ID] {
					t.Errorf("model %s: pi returns a copy of a listed chat model, not the catalog's", g.ID)
				}
			}
		})
	}
}
