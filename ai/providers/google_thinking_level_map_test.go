package providers

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Ported from pi packages/ai/test/google-thinking-level-map.test.ts (upstream
// af2c35223, 16235fd93). The file's google-vertex cases wait for that adapter:
// google-vertex is Scope queue entry 5 (docs/UPSTREAM.md), and 16235fd93's
// google-vertex.ts hunk is queued there with it.

func googleLevelMapModel(id string, levels map[ai.ModelThinkingLevel]*string) *ai.Model {
	return &ai.Model{
		ID:               id,
		Name:             id,
		Api:              ai.APIGoogleGenerativeAI,
		Provider:         "test-google",
		BaseURL:          "https://example.invalid/v1beta",
		Reasoning:        true,
		ThinkingLevelMap: levels,
		Input:            []string{"text"},
		ContextWindow:    128000,
		MaxTokens:        4096,
	}
}

func levelMap(pairs map[ai.ModelThinkingLevel]string) map[ai.ModelThinkingLevel]*string {
	out := make(map[ai.ModelThinkingLevel]*string, len(pairs))
	for k, v := range pairs {
		value := v
		out[k] = &value
	}
	return out
}

// effortLevelMap is the test file's models.dev-effort map (#9455): off, minimal,
// xhigh and max unsupported; low/medium/high native.
func effortLevelMap() ai.ThinkingLevelMap {
	return ai.ThinkingLevelMap{
		"off": nil, "minimal": nil, "low": strPtr("low"), "medium": strPtr("medium"), "high": strPtr("high"),
		"xhigh": nil, "max": nil,
	}
}

func TestResolveGoogleThinkingLevel(t *testing.T) {
	// Logical levels resolve to themselves.
	defaults := map[ai.ThinkingLevel]string{
		"minimal": "minimal",
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
	}
	for level, want := range defaults {
		t.Run(string(level), func(t *testing.T) {
			got, err := resolveGoogleThinkingLevel(googleLevelMapModel("gemini-3.7-flash", nil), level)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Fatalf("want %q, got %q", want, got)
			}
		})
	}

	// A mapping value wins over the requested level, case-insensitively.
	for _, mapped := range []string{"minimal", "low", "medium", "high", "MINIMAL", "LOW", "MEDIUM", "HIGH"} {
		want := strings.ToLower(mapped)
		model := googleLevelMapModel("gemini-3.7-flash", levelMap(map[ai.ModelThinkingLevel]string{
			"high": mapped, "xhigh": mapped, "max": mapped,
		}))
		for _, level := range []ai.ThinkingLevel{"high", "xhigh", "max"} {
			got, err := resolveGoogleThinkingLevel(model, level)
			if err != nil {
				t.Fatalf("%s -> %s: unexpected error: %v", level, mapped, err)
			}
			if got != want {
				t.Fatalf("%s -> %s: want %q, got %q", level, mapped, want, got)
			}
		}
	}
}

func TestResolveGoogleThinkingLevelErrors(t *testing.T) {
	// The message is model-visible and byte-exact against pi's template, JS
	// String(mapped) rendering included.
	cases := []struct {
		name  string
		model *ai.Model
		level ai.ThinkingLevel
		want  string
	}{
		{
			name:  "unmappable value",
			model: googleLevelMapModel("gemini-3.7-flash", levelMap(map[ai.ModelThinkingLevel]string{"xhigh": "extreme"})),
			level: "xhigh",
			want:  "Unsupported Google thinking level mapping for test-google/gemini-3.7-flash: xhigh -> extreme",
		},
		{
			name:  "absent key renders undefined",
			model: googleLevelMapModel("gemini-3.7-flash", nil),
			level: "max",
			want:  "Unsupported Google thinking level mapping for test-google/gemini-3.7-flash: max -> undefined",
		},
		{
			// pi's `typeof mapped === "string"` guard sends an explicit null down
			// the same fallback as an absent key, but String(null) prints "null".
			name:  "null entry renders null",
			model: googleLevelMapModel("gemini-3.7-flash", map[ai.ModelThinkingLevel]*string{"max": nil}),
			level: "max",
			want:  "Unsupported Google thinking level mapping for test-google/gemini-3.7-flash: max -> null",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveGoogleThinkingLevel(tc.model, tc.level)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if err.Error() != tc.want {
				t.Fatalf("want %q, got %q", tc.want, err.Error())
			}
		})
	}
}

// captureGoogleSimplePayload runs StreamSimpleGoogle and returns the request body
// it would have sent, aborting the stream from OnPayload the way pi's test does.
// An empty reasoning is pi's omitted `reasoning`.
func captureGoogleSimplePayload(t *testing.T, model *ai.Model, reasoning ai.ThinkingLevel, budgets *ai.ThinkingBudgets) map[string]any {
	t.Helper()
	var captured any
	opts := &ai.SimpleStreamOptions{Reasoning: reasoning, ThinkingBudgets: budgets}
	opts.APIKey = "test"
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		captured = payload
		return nil, errors.New("payload captured")
	}
	msg := StreamSimpleGoogle(t.Context(), model, ai.NormalizeContext(ai.Context{
		Messages: []ai.Message{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "Hello"}}}},
	}), opts).Result()
	if !strings.Contains(msg.ErrorMessage, "payload captured") {
		t.Fatalf("stream did not reach OnPayload: %q", msg.ErrorMessage)
	}
	body, _ := captured.(map[string]any)
	if body == nil {
		t.Fatalf("no payload captured (got %T)", captured)
	}
	return roundtripBody(t, body)
}

func googleThinkingConfig(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	gen, _ := body["generationConfig"].(map[string]any)
	if gen == nil {
		t.Fatalf("no generationConfig: %v", body)
	}
	cfg, _ := gen["thinkingConfig"].(map[string]any)
	if cfg == nil {
		t.Fatalf("no thinkingConfig: %v", gen)
	}
	return cfg
}

// assertGoogleThinkingConfig is pi's toEqual on thinkingConfig: every key, and
// no others.
func assertGoogleThinkingConfig(t *testing.T, got, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("thinkingConfig\n got %v\nwant %v", got, want)
	}
}

// Regression test for https://github.com/earendil-works/pi/issues/9455
func TestGoogleSimpleUsesLowestSupportedLevelWhenReasoningOmitted(t *testing.T) {
	model := googleLevelMapModel("gemini-3.8-flash", effortLevelMap())
	cfg := googleThinkingConfig(t, captureGoogleSimplePayload(t, model, "", nil))
	assertGoogleThinkingConfig(t, cfg, map[string]any{"thinkingLevel": "LOW"})
}

func TestGoogleSimplePreservesNativeMediumForGemini31Pro(t *testing.T) {
	model := googleLevelMapModel("gemini-3.1-pro-preview", effortLevelMap())
	cfg := googleThinkingConfig(t, captureGoogleSimplePayload(t, model, ai.ThinkingMedium, nil))
	assertGoogleThinkingConfig(t, cfg, map[string]any{"includeThoughts": true, "thinkingLevel": "MEDIUM"})
}

func TestGoogleSimpleDisablesGemini25WhenReasoningOmitted(t *testing.T) {
	model := googleLevelMapModel("gemini-2.5-flash", ai.ThinkingLevelMap{})
	cfg := googleThinkingConfig(t, captureGoogleSimplePayload(t, model, "", nil))
	assertGoogleThinkingConfig(t, cfg, map[string]any{"thinkingBudget": float64(0)})
}

// pi's `!options?.reasoning` does not catch the string "off": an explicit or
// clamped "off" returns the stream with thinking disabled rather than resolving
// a level (#9455).
func TestGoogleSimpleOffDisablesThinking(t *testing.T) {
	cases := []struct {
		name      string
		model     *ai.Model
		reasoning ai.ThinkingLevel
	}{
		{"explicit off, budget model", googleLevelMapModel("gemini-2.5-flash", nil), "off"},
		{"explicit off, level model without a map", googleLevelMapModel("gemini-3-flash-preview", nil), "off"},
		{
			"high clamps to off",
			googleLevelMapModel("gemini-3.8-flash", ai.ThinkingLevelMap{"minimal": nil, "low": nil, "medium": nil, "high": nil}),
			ai.ThinkingHigh,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := googleThinkingConfig(t, captureGoogleSimplePayload(t, tc.model, tc.reasoning, nil))
			assertGoogleThinkingConfig(t, cfg, map[string]any{"thinkingBudget": float64(0)})
		})
	}
}

func TestGoogleSimpleMapsExtendedLevels(t *testing.T) {
	// xhigh/max only survive clamping because the map opts into them; they must
	// then reach the wire as the level they map to.
	model := googleLevelMapModel("gemini-3.7-flash", levelMap(map[ai.ModelThinkingLevel]string{
		"xhigh": "high", "max": "high",
	}))
	for _, reasoning := range []ai.ThinkingLevel{"xhigh", "max"} {
		t.Run(string(reasoning), func(t *testing.T) {
			cfg := googleThinkingConfig(t, captureGoogleSimplePayload(t, model, reasoning, nil))
			if cfg["includeThoughts"] != true || cfg["thinkingLevel"] != "HIGH" {
				t.Fatalf("want includeThoughts+thinkingLevel HIGH, got %v", cfg)
			}
		})
	}
}

func TestGoogleSimpleHonorsUppercaseMappingForStandardLevel(t *testing.T) {
	model := googleLevelMapModel("gemini-3.7-flash", levelMap(map[ai.ModelThinkingLevel]string{"high": "LOW"}))
	cfg := googleThinkingConfig(t, captureGoogleSimplePayload(t, model, "high", nil))
	if cfg["thinkingLevel"] != "LOW" {
		t.Fatalf("want thinkingLevel LOW, got %v", cfg)
	}
}

func TestGoogleSimpleUsesMappedLevelForTokenBudget(t *testing.T) {
	// The custom-budget lookup keys off the RESOLVED level, not the requested one.
	model := googleLevelMapModel("gemini-2.5-flash", levelMap(map[ai.ModelThinkingLevel]string{"xhigh": "high"}))
	high := 1234
	budgets := &ai.ThinkingBudgets{High: &high}
	cfg := googleThinkingConfig(t, captureGoogleSimplePayload(t, model, "xhigh", budgets))
	if cfg["thinkingBudget"] != float64(1234) {
		t.Fatalf("want thinkingBudget 1234, got %v", cfg)
	}
}

func TestGoogleSimpleUnresolvableLevelFailsStream(t *testing.T) {
	// pi throws out of streamSimple; the Go seam encodes it as a terminal error.
	model := googleLevelMapModel("gemini-3.7-flash", levelMap(map[ai.ModelThinkingLevel]string{"xhigh": "extreme"}))
	opts := &ai.SimpleStreamOptions{Reasoning: "xhigh"}
	opts.APIKey = "test"
	// Record rather than t.Fatal: this runs on the provider's stream goroutine,
	// where a Goexit would strand Result() instead of failing the test.
	built := false
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		built = true
		return nil, errors.New("request must not be built for an unresolvable level")
	}
	msg := StreamSimpleGoogle(t.Context(), model, ai.NormalizeContext(ai.Context{}), opts).Result()
	if built {
		t.Fatal("request must not be built for an unresolvable level")
	}
	if msg.StopReason != ai.StopError {
		t.Fatalf("want stopReason error, got %q", msg.StopReason)
	}
	want := "Unsupported Google thinking level mapping for test-google/gemini-3.7-flash: xhigh -> extreme"
	if msg.ErrorMessage != want {
		t.Fatalf("want %q, got %q", want, msg.ErrorMessage)
	}
	if msg.Model != model.ID || msg.Provider != model.Provider || msg.Api != model.Api {
		t.Fatalf("error message must identify the model: %+v", msg)
	}
}

// googleThinkingCaptureFile holds pi's thinkingConfig for a table of models and
// reasoning levels, captured under node at upstream 1692bba63 by
// testdata/google-thinking/capture-thinking-config.mts (pi-ai 1.3.0's dist
// records the same file). Each case carries its own fixture, so both sides
// build the same model.
const googleThinkingCaptureFile = "testdata/google-thinking/thinking-config-1692bba63.json"

type googleThinkingCase struct {
	Name               string              `json:"name"`
	ID                 string              `json:"id"`
	ThinkingLevelMap   ai.ThinkingLevelMap `json:"thinkingLevelMap"`
	Reasoning          ai.ThinkingLevel    `json:"reasoning"`
	ThinkingBudgets    *ai.ThinkingBudgets `json:"thinkingBudgets"`
	ModelReasoning     *bool               `json:"modelReasoning"`
	StrictRequiredTool bool                `json:"strictRequiredTool"`

	// Exactly one outcome: the body's thinkingConfig (JSON null when it has
	// none), a throw out of streamSimple, or a stream error before any request.
	ThinkingConfig json.RawMessage `json:"thinkingConfig"`
	Thrown         *string         `json:"thrown"`
	Error          *string         `json:"error"`
}

func TestGoogleThinkingConfigMatchesPi(t *testing.T) {
	data, err := os.ReadFile(googleThinkingCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Cases []googleThinkingCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", googleThinkingCaptureFile, err)
	}
	if len(capture.Cases) == 0 {
		t.Fatalf("%s has no cases; rerun capture-thinking-config.mts", googleThinkingCaptureFile)
	}
	for _, tc := range capture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			model := googleLevelMapModel(tc.ID, tc.ThinkingLevelMap)
			if tc.ModelReasoning != nil {
				model.Reasoning = *tc.ModelReasoning
			}
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 0)}}
			if tc.StrictRequiredTool {
				tool := ai.Tool{Name: "strict_tool", Description: "strict tool", Parameters: ai.Object()}
				tool.ConstrainedSampling = &ai.ConstrainedSamplingConfig{
					Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingRequire,
				}
				req.Tools = []ai.Tool{tool}
			}
			var captured map[string]any
			opts := &ai.SimpleStreamOptions{Reasoning: tc.Reasoning, ThinkingBudgets: tc.ThinkingBudgets}
			opts.APIKey = "test"
			opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
				captured, _ = payload.(map[string]any)
				return nil, errors.New("payload captured")
			}
			msg := StreamSimpleGoogle(t.Context(), model, ai.NormalizeContext(req), opts).Result()

			switch {
			case tc.Thrown != nil || tc.Error != nil:
				want := tc.Error
				if tc.Thrown != nil {
					want = tc.Thrown
				}
				if captured != nil {
					t.Fatalf("pi fails before building the request (%q); Go built %v", *want, captured)
				}
				if msg.StopReason != ai.StopError || msg.ErrorMessage != *want {
					t.Fatalf("want error %q, got %s %q", *want, msg.StopReason, msg.ErrorMessage)
				}
			case tc.ThinkingConfig != nil:
				if captured == nil {
					t.Fatalf("no request built: %s %q", msg.StopReason, msg.ErrorMessage)
				}
				gen, _ := roundtripBody(t, captured)["generationConfig"].(map[string]any)
				var want any
				if err := json.Unmarshal(tc.ThinkingConfig, &want); err != nil {
					t.Fatal(err)
				}
				got, present := gen["thinkingConfig"]
				if !present {
					got = nil
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("thinkingConfig\n got %v\nwant %v", got, want)
				}
			default:
				t.Fatalf("%s: case %q records no outcome", googleThinkingCaptureFile, tc.Name)
			}
		})
	}
}

// catalogGemini4CaptureFile is what testdata/google-thinking/
// capture-catalog-gemini4.mjs recorded from pi-ai 1.3.0: the thinkingConfig
// its streamSimple sends for every google-generative-ai catalog model whose
// id is Gemini 4 or later, at every reasoning level.
const catalogGemini4CaptureFile = "testdata/google-thinking/catalog-gemini4-1.3.0.json"

// The shipped catalog's Gemini 4 models send thinkingLevel, not a token
// budget (upstream 1692bba63, #10890): the catalog entries themselves, so a
// catalog change that moved their maps or ids would show here. The capture
// leaves out google-vertex's gemini-4-argon, which has no Go adapter yet
// (Scope queue entry 5).
func TestCatalogGemini4ThinkingConfigMatchesPi(t *testing.T) {
	data, err := os.ReadFile(catalogGemini4CaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Provider       ai.ProviderId     `json:"provider"`
			ID             string            `json:"id"`
			Reasoning      *ai.ThinkingLevel `json:"reasoning"`
			ThinkingConfig any               `json:"thinkingConfig"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", catalogGemini4CaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows; rerun capture-catalog-gemini4.mjs", catalogGemini4CaptureFile)
	}
	for _, row := range capture.Rows {
		model := ai.GetModel(row.Provider, row.ID)
		if model == nil {
			t.Fatalf("the catalog has no %s/%s; rerun capture-catalog-gemini4.mjs", row.Provider, row.ID)
		}
		var reasoning ai.ThinkingLevel
		if row.Reasoning != nil {
			reasoning = *row.Reasoning
		}
		gen, _ := captureGoogleSimplePayload(t, model, reasoning, nil)["generationConfig"].(map[string]any)
		if got := gen["thinkingConfig"]; !reflect.DeepEqual(got, row.ThinkingConfig) {
			t.Errorf("%s/%s reasoning %q: thinkingConfig %v, pi %v", row.Provider, row.ID, reasoning, got, row.ThinkingConfig)
		}
	}
}
