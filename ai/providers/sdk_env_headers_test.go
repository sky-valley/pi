package providers

import (
	"net/http"
	"os"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// The vendor SDK clients read headers of their own from the process
// environment when pi constructs them, once per stream: openai (7.19.0 since
// ab30693d6, as 6.40.0 did) reads OPENAI_ORG_ID, OPENAI_PROJECT_ID and
// OPENAI_CUSTOM_HEADERS, and @anthropic-ai/sdk 0.124.0 reads
// ANTHROPIC_CUSTOM_HEADERS. Every want here was measured on pi's wire:
// 8676a0dcd's src under node v26.4.0 with the SDK versions its package-lock
// names, against a raw socket, with the variables set in process.env, and the
// openai rows again at 2b0a123de under openai 7.19.0, which moves only the text
// of a refused name.

// String.prototype.trim removes a BOM and keeps a NEL, where Go's
// strings.TrimSpace does the reverse; a kept NEL is sent as its Latin-1 byte.
var (
	nbsp = string(rune(0xa0))
	bom  = string(rune(0xfeff))
	nel  = string(rune(0x85))
)

// sdkEnvVars are the variables the SDK clients read headers from.
var sdkEnvVars = []string{"OPENAI_ORG_ID", "OPENAI_PROJECT_ID", "OPENAI_CUSTOM_HEADERS", "ANTHROPIC_CUSTOM_HEADERS"}

// TestMain unsets the variables the SDK clients read headers from. Every other
// want in this package is pi's wire with them unset, and one set where the
// tests run would reach the port's requests as it reaches pi's.
func TestMain(m *testing.M) {
	for _, name := range sdkEnvVars {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}

// setSDKEnv sets env for the test and blanks every other SDK header variable,
// which the SDKs read as unset, so a variable set where the tests run cannot
// leak into a row.
func setSDKEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range sdkEnvVars {
		t.Setenv(name, "")
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

// sdkEnvRow is one request with env set, and the wire or the error it yields.
type sdkEnvRow struct {
	name    string
	env     map[string]string
	key     string // "" means "k"
	headers ai.ProviderHeaders
	// want maps a header to its one wire value; "" wants it absent.
	want map[string]string
	// err is the stream's error when no request is sent, and payloads how many
	// times onPayload ran before it.
	err      string
	payloads int
}

func (row sdkEnvRow) run(t *testing.T, adapter string) {
	t.Helper()
	setSDKEnv(t, row.env)
	key := row.key
	if key == "" {
		key = "k"
	}
	payloads := 0
	h, final := runSDKWire(t, adapter, nil, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
		APIKey: key, Headers: row.headers,
		OnPayload: func(any, *ai.Model) (any, error) { payloads++; return nil, nil },
	}})
	if row.err != "" {
		wantNotSent(t, h, final, row.err)
		if payloads != row.payloads {
			t.Fatalf("onPayload ran %d times, want %d", payloads, row.payloads)
		}
		return
	}
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	for name, want := range row.want {
		if want == "" {
			if values, present := h[http.CanonicalHeaderKey(name)]; present {
				t.Fatalf("%s = %q, want it absent", name, values)
			}
			continue
		}
		wantOneValue(t, h, name, want)
	}
}

// OPENAI_ORG_ID and OPENAI_PROJECT_ID are openai's constructor defaults for
// options pi never passes, so they reach every request through the client —
// a groq one too — from the SDK's own bundle, below pi's object. readEnv trims
// them as String.prototype.trim does, and reads a blank one as unset.
func TestOpenAIClientSendsOrgAndProjectFromEnv(t *testing.T) {
	rows := []sdkEnvRow{
		{name: "set", env: map[string]string{"OPENAI_ORG_ID": " org-123 ", "OPENAI_PROJECT_ID": "proj-9"},
			want: map[string]string{"openai-organization": "org-123", "openai-project": "proj-9"}},
		{name: "blank", env: map[string]string{"OPENAI_ORG_ID": " \t "}, want: map[string]string{"openai-organization": ""}},
		{name: "trimmed as JS trims", env: map[string]string{"OPENAI_ORG_ID": nbsp + "o" + nbsp}, want: map[string]string{"openai-organization": "o"}},
		{name: "BOM trimmed", env: map[string]string{"OPENAI_ORG_ID": bom + "o"}, want: map[string]string{"openai-organization": "o"}},
		{name: "NEL kept", env: map[string]string{"OPENAI_ORG_ID": "o" + nel}, want: map[string]string{"openai-organization": "o\x85"}},
		{name: "marker", env: map[string]string{"OPENAI_ORG_ID": "org-123"}, headers: ai.ProviderHeaders{"openai-organization": nil},
			want: map[string]string{"openai-organization": ""}},
		{name: "override", env: map[string]string{"OPENAI_ORG_ID": "org-123"}, headers: ai.ProviderHeaders{"OpenAI-Organization": strPtr("x")},
			want: map[string]string{"openai-organization": "x"}},
		// The SDK's own bundle is converted after its auth header and before
		// pi's object, at request time.
		{name: "refused value", env: map[string]string{"OPENAI_ORG_ID": "org-" + string(rune(cjk))}, err: byteStringError(4, cjk), payloads: 1},
		{name: "after the key", env: map[string]string{"OPENAI_ORG_ID": "org-" + string(rune(cjk))}, key: "k" + string(rune(0x100)),
			err: byteStringError(8, 0x100), payloads: 1},
		{name: "ahead of pi's object", env: map[string]string{"OPENAI_ORG_ID": "org-" + string(rune(cjk))},
			headers: ai.ProviderHeaders{"X-B": strPtr(string(rune(0x100)))}, err: byteStringError(4, cjk), payloads: 1},
		{name: "refused LF", env: map[string]string{"OPENAI_ORG_ID": "a\nb"}, err: invalidValueError("a\nb"), payloads: 1},
	}
	for _, adapter := range []string{"openai-completions", "openai-completions-groq", "openai-responses"} {
		for _, row := range rows {
			t.Run(adapter+"/"+row.name, func(t *testing.T) { row.run(t, adapter) })
		}
	}
	t.Run("anthropic-messages ignores them", func(t *testing.T) {
		sdkEnvRow{env: map[string]string{"OPENAI_ORG_ID": "org-123", "OPENAI_PROJECT_ID": "proj-9"},
			want: map[string]string{"openai-organization": "", "openai-project": ""}}.run(t, "anthropic-messages")
	})
}

// openai's constructor folds OPENAI_CUSTOM_HEADERS under pi's object into the
// Headers it keeps as defaultHeaders — buildHeaders([parsed, defaultHeaders])
// — so pi's object wins a name both carry, the variable's headers replace the
// SDK's own and its auth header, and every name and value in either is
// converted and checked when pi builds the client: before the params and
// onPayload, and ahead of the api key.
func TestOpenAIClientFoldsCustomHeadersEnv(t *testing.T) {
	custom := func(v string) map[string]string { return map[string]string{"OPENAI_CUSTOM_HEADERS": v} }
	rows := []sdkEnvRow{
		{name: "lines", env: custom("X-Custom: one\nX-Other : two \nnocolon\nX-Over:env\r"), headers: ai.ProviderHeaders{"X-Over": strPtr("opt")},
			want: map[string]string{"x-custom": "one", "x-other": "two", "x-over": "opt", "nocolon": ""}},
		{name: "pi's object wins", env: custom("User-Agent: env"), want: map[string]string{"user-agent": ai.FormatUserAgent(nil)}},
		{name: "later line wins", env: custom("X-A: 1\nx-a: 2\nX-A: 3"), want: map[string]string{"x-a": "2"}},
		{name: "marker", env: custom("X-A: v"), headers: ai.ProviderHeaders{"x-a": nil}, want: map[string]string{"x-a": ""}},
		{name: "replaces the auth header", env: custom("Authorization: env"), want: map[string]string{"authorization": "env"}},
		{name: "a marker still deletes the auth header", env: custom("X-A: v"), headers: ai.ProviderHeaders{"authorization": nil},
			want: map[string]string{"authorization": "", "x-a": "v"}},
		{name: "trimmed as JS trims", env: custom("X-A:" + nbsp + "v" + nbsp + "\n" + nbsp + "X-B" + nbsp + ": w"),
			want: map[string]string{"x-a": "v", "x-b": "w"}},
		{name: "BOM trimmed", env: custom("X-A: v" + bom), want: map[string]string{"x-a": "v"}},
		{name: "NEL kept", env: custom("X-A: v" + nel), want: map[string]string{"x-a": "v\x85"}},
		{name: "refused value", env: custom("X-A: " + string(rune(cjk))), err: byteStringError(0, cjk)},
		{name: "pi's object converted with it", env: custom("X-A: v"), headers: ai.ProviderHeaders{"X-B": strPtr(string(rune(cjk)))},
			err: byteStringError(0, cjk)},
		{name: "ahead of the key", env: custom("X-A: v"), key: "k" + string(rune(0x100)), headers: ai.ProviderHeaders{"X-B": strPtr(string(rune(cjk)))},
			err: byteStringError(0, cjk)},
		{name: "a value with no header line still folds", env: custom("nocolon"), headers: ai.ProviderHeaders{"X-B": strPtr(string(rune(cjk)))},
			err: byteStringError(0, cjk)},
		{name: "empty name", env: custom(": v"), err: invalidNameError("openai-completions", "")},
		{name: "refused name", env: custom("bad name: v"), err: invalidNameError("openai-completions", "bad name")},
		// openai checks a name against the token grammar before it converts it.
		{name: "name above U+00FF", env: custom("X-" + string(rune(0x100)) + ": v"), err: invalidNameError("openai-completions", "X-"+string(rune(0x100)))},
		{name: "refused LF in pi's object", env: custom("X-A: v"), headers: ai.ProviderHeaders{"X-B": strPtr("a\nb")}, err: invalidValueError("a\nb")},
		{name: "ahead of the org header", env: map[string]string{"OPENAI_ORG_ID": "org-" + string(rune(cjk)), "OPENAI_CUSTOM_HEADERS": "X-A: " + string(rune(0x100))},
			err: byteStringError(0, 0x100)},
		{name: "ANTHROPIC_CUSTOM_HEADERS is not openai's", env: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "X-A: v"}, want: map[string]string{"x-a": ""}},
	}
	for _, adapter := range []string{"openai-completions", "openai-responses"} {
		for _, row := range rows {
			t.Run(adapter+"/"+row.name, func(t *testing.T) { row.run(t, adapter) })
		}
	}
	// The control: with the variable unset, pi's object is converted at request
	// time, after onPayload and after the key.
	t.Run("unset converts at request time", func(t *testing.T) {
		sdkEnvRow{key: "k" + string(rune(0x100)), headers: ai.ProviderHeaders{"X-B": strPtr(string(rune(cjk)))},
			err: byteStringError(8, 0x100), payloads: 1}.run(t, "openai-completions")
	})
}

// @anthropic-ai/sdk's constructor spreads ANTHROPIC_CUSTOM_HEADERS under pi's
// object, `{...parsed, ...defaultHeaders}`: the variable's headers take the
// first slots of the one plain object, which the SDK converts at request time.
// So a source spelled exactly like one of them takes its slot — the OAuth
// branch's "user-agent" identity lands in slot 0 under "user-agent: x" — and
// the object's last slot for a name wins: on the OAuth branch, which sends no
// client identity since upstream adc5fa9f8, that is Claude Code's agent
// either way.
func TestAnthropicClientSpreadsCustomHeadersEnv(t *testing.T) {
	custom := func(v string) map[string]string { return map[string]string{"ANTHROPIC_CUSTOM_HEADERS": v} }
	for _, row := range []sdkEnvRow{
		{name: "lines", env: custom("X-Custom: a1\nUser-Agent: env\nnocolon"),
			want: map[string]string{"x-custom": "a1", "user-agent": ai.FormatUserAgent(nil), "nocolon": ""}},
		{name: "replaces the auth header", env: custom("x-api-key: env"), want: map[string]string{"x-api-key": "env"}},
		{name: "marker across spellings", env: custom("X-A: v"), headers: ai.ProviderHeaders{"x-a": nil}, want: map[string]string{"x-a": ""}},
		{name: "marker in its slot", env: custom("X-A: v"), headers: ai.ProviderHeaders{"X-A": nil}, want: map[string]string{"x-a": ""}},
		{name: "later line wins", env: custom("X-A: 1\nx-a: 2\nX-A: 3"), want: map[string]string{"x-a": "2"}},
		{name: "trimmed as JS trims", env: custom("X-A:" + nbsp + "v" + nbsp), want: map[string]string{"x-a": "v"}},
		{name: "BOM trimmed", env: custom("X-A: v" + bom), want: map[string]string{"x-a": "v"}},
		{name: "NEL kept", env: custom("X-A: v" + nel), want: map[string]string{"x-a": "v\x85"}},
		{name: "OAuth identity in slot 0", env: custom("user-agent: env"), key: "sk-ant-oat-x",
			want: map[string]string{"user-agent": "claude-cli/" + claudeCodeVersion}},
		{name: "OAuth identity in its own slot", env: custom("User-Agent: env"), key: "sk-ant-oat-x",
			want: map[string]string{"user-agent": "claude-cli/" + claudeCodeVersion}},
		{name: "refused value at request time", env: custom("X-A: " + string(rune(cjk))), err: byteStringError(0, cjk), payloads: 1},
		{name: "empty name at request time", env: custom(": v"), err: invalidNameError("anthropic-messages", ""), payloads: 1},
		{name: "refused name at request time", env: custom("bad name: v"), err: invalidNameError("anthropic-messages", "bad name"), payloads: 1},
		{name: "OPENAI_CUSTOM_HEADERS is not anthropic's", env: map[string]string{"OPENAI_CUSTOM_HEADERS": "X-A: v"}, want: map[string]string{"x-a": ""}},
	} {
		t.Run(row.name, func(t *testing.T) { row.run(t, "anthropic-messages") })
	}
}
