package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// identityCaptureFile is written by testdata/identity/capture-identity.mts,
// which sends each request through upstream's own adapters and Models under
// node and records the identity headers that reach fetch.
const identityCaptureFile = "testdata/identity/identity-9ffd27cfa.json"

type identityTargetJSON struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"baseUrl"`
}

type identityRow struct {
	Name            string                     `json:"name"`
	API             ai.Api                     `json:"api"`
	Provider        string                     `json:"provider"`
	BaseURL         string                     `json:"baseUrl"`
	ModelHeaders    map[string]string          `json:"modelHeaders"`
	APIKey          string                     `json:"apiKey"`
	Identity        json.RawMessage            `json:"identity"`
	Headers         map[string]json.RawMessage `json:"headers"`
	SessionID       string                     `json:"sessionId"`
	Via             string                     `json:"via"`
	ModelsIdentity  json.RawMessage            `json:"modelsIdentity"`
	WrapperIdentity json.RawMessage            `json:"wrapperIdentity"`
	Env             map[string]string          `json:"env"`
	Sent            map[string]string          `json:"sent"`
}

type identityCapture struct {
	SHA           string `json:"sha"`
	Runtime       string `json:"runtime"`
	PiAttribution struct {
		Name       string   `json:"name"`
		URL        string   `json:"url"`
		Categories []string `json:"categories"`
	} `json:"piAttribution"`
	Functions struct {
		Attribution []struct {
			Target   identityTargetJSON `json:"target"`
			Identity json.RawMessage    `json:"identity"`
			Headers  map[string]string  `json:"headers"`
		} `json:"attribution"`
		Session []struct {
			Target  identityTargetJSON `json:"target"`
			Headers map[string]string  `json:"headers"`
		} `json:"session"`
		Merge []struct {
			Sources []json.RawMessage `json:"sources"`
			Merged  json.RawMessage   `json:"merged"`
		} `json:"merge"`
		UserAgent []struct {
			Identity json.RawMessage `json:"identity"`
			Value    string          `json:"value"`
		} `json:"userAgent"`
	} `json:"functions"`
	Rows []identityRow `json:"rows"`
}

func loadIdentityCapture(t *testing.T) identityCapture {
	t.Helper()
	data, err := os.ReadFile(identityCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/identity/capture-identity.mts)", identityCaptureFile, err)
	}
	var capture identityCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", identityCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows", identityCaptureFile)
	}
	return capture
}

// parseIdentity reads a captured ClientIdentity: absent or "undefined" is
// nil, attribution null is ai.NoAttribution.
func parseIdentity(t *testing.T, raw json.RawMessage) *ai.ClientIdentity {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `"undefined"` {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("identity %s: %v", raw, err)
	}
	identity := &ai.ClientIdentity{}
	if ua, ok := fields["userAgent"]; ok {
		var mode string
		var custom struct {
			Custom *string `json:"custom"`
		}
		switch {
		case json.Unmarshal(ua, &mode) == nil && mode == "short":
			identity.UserAgent = ai.UserAgentShort
		case mode == "detailed":
			identity.UserAgent = ai.UserAgentDetailed
		case json.Unmarshal(ua, &custom) == nil && custom.Custom != nil:
			identity.UserAgent = ai.UserAgentCustom(*custom.Custom)
		default:
			t.Fatalf("userAgent %s", ua)
		}
	}
	if attribution, ok := fields["attribution"]; ok {
		if string(attribution) == "null" {
			identity.Attribution = ai.NoAttribution
		} else {
			var a struct {
				Name       string   `json:"name"`
				URL        string   `json:"url"`
				Categories []string `json:"categories"`
			}
			if err := json.Unmarshal(attribution, &a); err != nil {
				t.Fatalf("attribution %s: %v", attribution, err)
			}
			identity.Attribution = &ai.ClientAttribution{Name: a.Name, URL: a.URL, Categories: a.Categories}
		}
	}
	if version, ok := fields["version"]; ok {
		if err := json.Unmarshal(version, &identity.Version); err != nil {
			t.Fatalf("version %s: %v", version, err)
		}
	}
	return identity
}

// identityJSON is an identity as the capture writes a merge result: unset
// fields absent, NoAttribution as null, "undefined" for nil.
func identityJSON(identity *ai.ClientIdentity) any {
	if identity == nil {
		return "undefined"
	}
	out := map[string]any{}
	switch {
	case identity.UserAgent == ai.UserAgentShort:
		out["userAgent"] = "short"
	case identity.UserAgent == ai.UserAgentDetailed:
		out["userAgent"] = "detailed"
	case identity.UserAgent.IsSet():
		out["userAgent"] = map[string]any{"custom": ai.FormatUserAgent(&ai.ClientIdentity{UserAgent: identity.UserAgent})}
	}
	switch {
	case identity.Attribution == ai.NoAttribution:
		out["attribution"] = nil
	case identity.Attribution != nil:
		a := map[string]any{"name": identity.Attribution.Name, "url": identity.Attribution.URL}
		if identity.Attribution.Categories != nil {
			categories := make([]any, len(identity.Attribution.Categories))
			for i, c := range identity.Attribution.Categories {
				categories[i] = c
			}
			a["categories"] = categories
		}
		out["attribution"] = a
	}
	if identity.Version != "" {
		out["version"] = identity.Version
	}
	return out
}

// isRuntimeUserAgent reports whether a User-Agent is a transport's own default
// rather than one a source set: docs/UPSTREAM.md D12. Once a marker removes
// every source's agent, pi's fetch (undici) and @google/genai add theirs —
// the capture records the request before undici does, and genai's
// google-genai-sdk/<v> — and net/http adds Go-http-client/<v>.
func isRuntimeUserAgent(value string, present bool) bool {
	return !present || strings.HasPrefix(value, "google-genai-sdk/") || strings.HasPrefix(value, "Go-http-client/")
}

// identityWireValue is a header value as its wire bytes: fetch's Headers holds a
// ByteString, which Headers.get reads back one code unit per byte, so a
// captured "Über" is the byte 0xDC then "ber", as the port writes it.
func identityWireValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r > 0xFF {
			return value // not a ByteString; compared as text
		}
		b.WriteByte(byte(r))
	}
	return b.String()
}

// wantIdentitySent is a row's expected headers with a detailed User-Agent's
// platform, runtime and arch replaced by the port's: pi writes node's
// runtime there, the port Go's (ai.FormatUserAgent).
var detailedUserAgent = regexp.MustCompile(`^pi/([^ ]*) \(([^;]+); ([^;]+); ([^)]+)\)$`)

func wantIdentitySent(t *testing.T, capture identityCapture, sent map[string]string) map[string]string {
	t.Helper()
	want := make(map[string]string, len(sent))
	for name, value := range sent {
		if name == "user-agent" {
			if m := detailedUserAgent.FindStringSubmatch(value); m != nil {
				if m[3] != capture.Runtime {
					t.Fatalf("captured runtime slot %q, header says %q", m[3], capture.Runtime)
				}
				value = ai.FormatUserAgent(&ai.ClientIdentity{Version: m[1]})
				if m[1] == "" {
					t.Fatalf("an empty version cannot be replayed (see TestClientIdentityFunctionsMatchPi)")
				}
			}
		}
		want[name] = value
	}
	return want
}

// identityRecorded is the names a row's sent headers are compared on: the
// capture's recorded list plus the names the row's request headers set.
func identityRecorded(row identityRow) []string {
	names := map[string]bool{
		"user-agent": true, "http-referer": true, "x-openrouter-title": true, "x-openrouter-categories": true,
		"x-billing-invoke-origin": true, "x-opencode-client": true, "x-opencode-session": true, "x-app": true,
	}
	for name := range row.Headers {
		names[strings.ToLower(name)] = true
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// identitySSE answers every adapter with an error the way the capture's fetch
// does, a 400 none of them retries.
func identityServer(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"captured"}}`)
	}))
	t.Cleanup(server.Close)
	return server, func() http.Header { return got }
}

func replayIdentityRow(t *testing.T, row identityRow) http.Header {
	t.Helper()
	for name, value := range row.Env {
		t.Setenv(name, value)
	}
	server, sent := identityServer(t)
	target, _ := url.Parse(server.URL)
	model := &ai.Model{
		ID: "test-model", Name: "Test Model", Api: row.API, Provider: row.Provider, BaseURL: row.BaseURL,
		Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 4096,
	}
	if row.ModelHeaders != nil {
		model.Headers = ai.ProviderHeaders{}
		for name, value := range row.ModelHeaders {
			model.Headers[name] = ai.HeaderValue(value)
		}
	}
	request := ai.ProviderRequestOptions{Identity: parseIdentity(t, row.Identity)}
	if row.Headers != nil {
		request.Headers = ai.ProviderHeaders{}
		for name, raw := range row.Headers {
			if string(raw) == "null" {
				request.Headers[name] = nil
				continue
			}
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			request.Headers[name] = ai.HeaderValue(value)
		}
	}
	if row.API == ai.APIGoogleGenerativeAI {
		// google refuses a custom client, so its rows' base URL names no host a
		// target test reads and the request goes straight to the server.
		model.BaseURL = server.URL
	} else {
		request.HTTPClient = redirectDoer{target}
	}
	api, ok := ai.GetApiProvider(row.API)
	if !ok {
		t.Fatalf("no api provider for %s", row.API)
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hello", 1)}}
	if row.Via == "models" {
		models := ai.CreateModels(&ai.CreateModelsOptions{Identity: parseIdentity(t, row.ModelsIdentity)})
		key := row.APIKey
		models.SetProvider(ai.CreateProvider(ai.CreateProviderOptions{
			ID: row.Provider,
			Auth: ai.ProviderAuth{APIKey: &ai.ApiKeyAuth{Name: "Test",
				Resolve: func(context.Context, ai.AuthContext, *ai.Credential) (*ai.AuthResult, error) {
					return &ai.AuthResult{Auth: ai.ModelAuth{APIKey: key}}, nil
				}}},
			Models: []*ai.Model{model},
			API:    &ai.ProviderStreams{Stream: api.Stream, StreamSimple: api.StreamSimple},
		}))
		var through ai.Models = models
		if wrapper := parseIdentity(t, row.WrapperIdentity); wrapper != nil {
			through = ai.WithClientIdentity(models, *wrapper)
		}
		opts := &ai.ModelsSimpleStreamOptions{}
		opts.ProviderRequestOptions = request
		opts.SessionID = row.SessionID
		through.StreamSimple(context.Background(), model, req, opts).Result()
	} else {
		request.APIKey = row.APIKey
		api.Stream(context.Background(), model, ai.NormalizeContext(req), &ai.StreamOptions{
			ProviderRequestOptions: request, SessionID: row.SessionID,
		}).Result()
	}
	got := sent()
	if got == nil {
		t.Fatalf("no request reached the server")
	}
	return got
}

// TestIdentityHeadersMatchPi replays every captured request — each adapter's
// identity defaults, User-Agent modes, attribution per target, the sources
// that outrank it, and the Models layer's identities and OpenCode session
// header — and requires the headers pi sent, present and absent.
func TestIdentityHeadersMatchPi(t *testing.T) {
	capture := loadIdentityCapture(t)
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			got := replayIdentityRow(t, row)
			want := wantIdentitySent(t, capture, row.Sent)
			for _, name := range identityRecorded(row) {
				values := got.Values(name)
				value, present := strings.Join(values, ", "), len(values) > 0
				wantValue, wantPresent := want[name]
				wantValue = identityWireValue(wantValue)
				if name == "user-agent" && isRuntimeUserAgent(wantValue, wantPresent) {
					// D12: only that no source's agent survived is asserted.
					if !isRuntimeUserAgent(value, present) {
						t.Errorf("user-agent: port sent %q where pi at %s sent only a transport default", value, capture.SHA)
					}
					continue
				}
				if present != wantPresent || value != wantValue {
					t.Errorf("%s: port sent %q (present %v), pi at %s sent %q (present %v)", name, value, present, capture.SHA, wantValue, wantPresent)
				}
			}
		})
	}
}

// TestClientIdentityFunctionsMatchPi pins the exported functions on the cells
// no request reaches: targets whose base URL is not a URL, host case and
// schemes, empty attribution fields, and the merge's edges.
func TestClientIdentityFunctionsMatchPi(t *testing.T) {
	capture := loadIdentityCapture(t)
	if got := ai.PiAttribution; got.Name != capture.PiAttribution.Name || got.URL != capture.PiAttribution.URL ||
		!reflect.DeepEqual(got.Categories, capture.PiAttribution.Categories) {
		t.Errorf("PiAttribution = %+v, pi at %s %+v", got, capture.SHA, capture.PiAttribution)
	}
	for _, row := range capture.Functions.Attribution {
		target := ai.ClientIdentityTarget{Provider: row.Target.Provider, BaseURL: row.Target.BaseURL}
		got := ai.GetAttributionHeaders(target, parseIdentity(t, row.Identity))
		if !reflect.DeepEqual(got, row.Headers) {
			t.Errorf("GetAttributionHeaders(%+v, %s) = %v, pi %v", target, row.Identity, got, row.Headers)
		}
	}
	for _, row := range capture.Functions.Session {
		target := ai.ClientIdentityTarget{Provider: row.Target.Provider, BaseURL: row.Target.BaseURL}
		if got := ai.GetOpenCodeSessionHeaders(target, "s"); !reflect.DeepEqual(got, row.Headers) {
			t.Errorf("GetOpenCodeSessionHeaders(%+v) = %v, pi %v", target, got, row.Headers)
		}
	}
	for _, row := range capture.Functions.Merge {
		sources := make([]*ai.ClientIdentity, len(row.Sources))
		for i, raw := range row.Sources {
			sources[i] = parseIdentity(t, raw)
		}
		var want any
		if err := json.Unmarshal(row.Merged, &want); err != nil {
			t.Fatal(err)
		}
		gotJSON, _ := json.Marshal(identityJSON(ai.MergeClientIdentity(sources...)))
		var got any
		json.Unmarshal(gotJSON, &got)
		if emptyVersion.Match(identitySourcesJSON(row.Sources)) {
			// The port's Version is a string whose zero value is unset, so an
			// explicit "" cannot override (docs/UPSTREAM.md, Divergences).
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("MergeClientIdentity(%s) = %s, pi %s", identitySourcesJSON(row.Sources), gotJSON, row.Merged)
		}
	}
	for _, row := range capture.Functions.UserAgent {
		if emptyVersion.Match(row.Identity) {
			continue // the same Version divergence
		}
		if got := ai.FormatUserAgent(parseIdentity(t, row.Identity)); got != row.Value {
			t.Errorf("FormatUserAgent(%s) = %q, pi %q", row.Identity, got, row.Value)
		}
	}
}

// emptyVersion finds an identity that sets version to "", which the port's
// Version string cannot tell from unset (docs/UPSTREAM.md, Divergences).
var emptyVersion = regexp.MustCompile(`"version":\s*""`)

func identitySourcesJSON(values []json.RawMessage) []byte {
	out, _ := json.Marshal(values)
	return out
}
