package ai

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// toUpperCaptureFile is written by testdata/identity/capture-to-upper.mjs.
const toUpperCaptureFile = "testdata/identity/to-upper-node-v26.4.0.json"

// jsToUpper is String.prototype.toUpperCase for every BMP character Node
// changes, and identity for every other: jsUpperDiffs holds exactly the
// characters unicode.ToUpper gets wrong. A failure after a Go or Node upgrade
// names the entries to add to or drop from it.
func TestJSToUpperMatchesNode(t *testing.T) {
	data, err := os.ReadFile(toUpperCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/identity/capture-to-upper.mjs)", toUpperCaptureFile, err)
	}
	var capture struct {
		Node string              `json:"node"`
		Rows [][]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	changed := map[rune]string{}
	for _, row := range capture.Rows {
		var c int
		var upper string
		json.Unmarshal(row[0], &c)
		json.Unmarshal(row[1], &upper)
		changed[rune(c)] = upper
	}
	if len(changed) < 1000 {
		t.Fatalf("%s holds %d characters; the capture is incomplete", toUpperCaptureFile, len(changed))
	}
	for r := rune(0); r <= 0xFFFF; r++ {
		if 0xD800 <= r && r <= 0xDFFF {
			continue
		}
		want, ok := changed[r]
		if !ok {
			want = string(r)
		}
		if got := jsToUpper(r); got != want {
			t.Errorf("jsToUpper(%U) = %q, Node %s %q", r, got, capture.Node, want)
		}
	}
}

// NVIDIA's invoke origin is the name with its first UTF-16 code unit upper-cased:
// a name that opens with an astral character is kept as it is.
func TestUpperFirst(t *testing.T) {
	for in, want := range map[string]string{
		"pi": "Pi", "": "", "ßeta": "SSeta", "über": "Über", "Über": "Über", "ﬀ": "FF",
		"\U0001F600x": "\U0001F600x", "\U00010428x": "\U00010428x", "1x": "1x",
	} {
		if got := upperFirst(in); got != want {
			t.Errorf("upperFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

// The detailed User-Agent names the Go runtime where pi names node's, and
// platform and arch in Node's spelling.
func TestFormatUserAgentDetailed(t *testing.T) {
	m := regexp.MustCompile(`^pi/(.+) \(([^;]+); go/([^;]+); ([^)]+)\)$`).FindStringSubmatch(FormatUserAgent(nil))
	if m == nil {
		t.Fatalf("FormatUserAgent(nil) = %q, want pi/<version> (<platform>; go/<version>; <arch>)", FormatUserAgent(nil))
	}
	if m[1] != PiAIVersion {
		t.Errorf("version = %q, want %q", m[1], PiAIVersion)
	}
	wantPlatform := runtime.GOOS
	if wantPlatform == "windows" {
		wantPlatform = "win32"
	}
	if m[2] != wantPlatform {
		t.Errorf("platform = %q, want Node's %q", m[2], wantPlatform)
	}
	if want := strings.TrimPrefix(runtime.Version(), "go"); m[3] != want {
		t.Errorf("runtime = go/%s, want go/%s", m[3], want)
	}
	arch := map[string]string{"amd64": "x64", "386": "ia32", "mipsle": "mipsel", "ppc64le": "ppc64"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	if m[4] != arch {
		t.Errorf("arch = %q, want Node's %q", m[4], arch)
	}
	if got := FormatUserAgent(&ClientIdentity{Version: "9.9.9"}); !strings.HasPrefix(got, "pi/9.9.9 (") {
		t.Errorf("FormatUserAgent(version 9.9.9) = %q", got)
	}
}

// pi's withClientIdentity: a MutableModels stays mutable, and everything but
// the requests is delegated unchanged.
func TestWithClientIdentityKeepsMutableModelsMutable(t *testing.T) {
	models := CreateModels(nil)
	wrapped := WithClientIdentity(models, ClientIdentity{UserAgent: UserAgentShort})
	mutable, ok := wrapped.(MutableModels)
	if !ok {
		t.Fatal("WithClientIdentity over a MutableModels returned an immutable Models")
	}
	mutable.SetProvider(CreateProvider(CreateProviderOptions{ID: "p", API: &ProviderStreams{
		Stream: func(context.Context, *Model, TranscriptContext, *StreamOptions) *AssistantMessageEventStream {
			return nil
		},
	}}))
	if models.GetProvider("p") == nil {
		t.Fatal("SetProvider through the wrapper did not reach the collection")
	}
	mutable.DeleteProvider("p")
	if models.GetProvider("p") != nil {
		t.Fatal("DeleteProvider through the wrapper did not reach the collection")
	}
	if _, ok := WithClientIdentity(immutableModels{models}, ClientIdentity{}).(MutableModels); ok {
		t.Fatal("WithClientIdentity over an immutable Models returned a MutableModels")
	}
}

type immutableModels struct{ Models }

// Every request method of the WithClientIdentity wrapper merges its identity
// beneath the request's own and over the collection's (pi withClientIdentity's
// withIdentity on stream, complete, streamSimple, completeSimple,
// streamDeferred, fetchDeferred and cancelDeferred).
func TestWithClientIdentityMergesOnEveryRequestMethod(t *testing.T) {
	var received *ClientIdentity
	done := func(model *Model) *AssistantMessageEventStream {
		stream := NewAssistantMessageEventStream()
		message := &AssistantMessage{Model: model.ID, StopReason: StopStop}
		stream.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: message})
		stream.End(message)
		return stream
	}
	streams := &ProviderStreams{
		Stream: func(_ context.Context, model *Model, _ TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
			received = opts.Identity
			return done(model)
		},
		StreamSimple: func(_ context.Context, model *Model, _ TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
			received = opts.Identity
			return done(model)
		},
		FetchDeferred: func(_ context.Context, model *Model, _ DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
			received = opts.Identity
			return done(model)
		},
		CancelDeferred: func(_ context.Context, _ *Model, _ DeferredHandle, opts *DeferredCancelOptions) error {
			received = opts.Identity
			return nil
		},
	}
	model := &Model{ID: "m", Name: "m", Api: "test-api", Provider: "p", Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}
	models := CreateModels(&CreateModelsOptions{Identity: &ClientIdentity{Version: "1.0.0", UserAgent: UserAgentShort}})
	models.SetProvider(CreateProvider(CreateProviderOptions{ID: "p", Auth: noAuthConfigured(), Models: []*Model{model}, API: streams}))
	wrapper := &ClientAttribution{Name: "wrapper", URL: "https://acme.example"}
	wrapped := WithClientIdentity(models, ClientIdentity{Attribution: wrapper, Version: "2.0.0"})
	request := &ClientIdentity{Version: "3.0.0"}
	handle := DeferredHandle{Provider: "p", ModelID: "m", Api: "test-api", ID: "h"}
	ctx := context.Background()
	req := Context{Messages: []Message{NewUserText("hi", 1)}}
	for name, call := range map[string]func(){
		"Stream": func() {
			wrapped.Stream(ctx, model, req, &ModelsStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Identity: request}}}).Result()
		},
		"Complete": func() {
			wrapped.Complete(ctx, model, req, &ModelsStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Identity: request}}})
		},
		"StreamSimple": func() {
			wrapped.StreamSimple(ctx, model, req, &ModelsSimpleStreamOptions{SimpleStreamOptions: SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Identity: request}}}}).Result()
		},
		"CompleteSimple": func() {
			wrapped.CompleteSimple(ctx, model, req, &ModelsSimpleStreamOptions{SimpleStreamOptions: SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Identity: request}}}})
		},
		"StreamDeferred": func() {
			wrapped.StreamDeferred(ctx, model, handle, &ModelsDeferredFetchOptions{DeferredFetchOptions: DeferredFetchOptions{ProviderRequestOptions: ProviderRequestOptions{Identity: request}}}).Result()
		},
		"FetchDeferred": func() {
			wrapped.FetchDeferred(ctx, model, handle, &ModelsDeferredFetchOptions{DeferredFetchOptions: DeferredFetchOptions{ProviderRequestOptions: ProviderRequestOptions{Identity: request}}})
		},
		"CancelDeferred": func() {
			if err := wrapped.CancelDeferred(ctx, model, handle, &ModelsDeferredCancelOptions{DeferredCancelOptions: ProviderRequestOptions{Identity: request}}); err != nil {
				t.Fatalf("CancelDeferred: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			received = nil
			call()
			// The collection's user agent, the wrapper's attribution, the request's version.
			if received == nil || received.UserAgent != UserAgentShort || received.Attribution != wrapper || received.Version != "3.0.0" {
				t.Fatalf("identity = %+v, want short, the wrapper's attribution, version 3.0.0", received)
			}
		})
	}
}

// The wrapper's ClearProviders reaches the collection.
func TestWithClientIdentityClearsProviders(t *testing.T) {
	models := CreateModels(nil)
	models.SetProvider(CreateProvider(CreateProviderOptions{ID: "p", Auth: noAuthConfigured(), API: &ProviderStreams{
		Stream: func(context.Context, *Model, TranscriptContext, *StreamOptions) *AssistantMessageEventStream {
			return nil
		},
	}}))
	WithClientIdentity(models, ClientIdentity{}).(MutableModels).ClearProviders()
	if len(models.GetProviders()) != 0 {
		t.Fatalf("ClearProviders through the wrapper left %d providers", len(models.GetProviders()))
	}
}
