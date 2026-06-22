package providers

import (
	"encoding/json"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// ctkModel builds a chat-template model with the given ordered kwargs JSON and
// thinkingLevelMap. thinkingFormat:"chat-template" overrides the detected format.
func ctkModel(kwargsJSON string, tlm ai.ThinkingLevelMap) *ai.Model {
	return openAIModel(func(m *ai.Model) {
		m.ID = "custom-chat-template"
		m.Reasoning = true
		m.ThinkingLevelMap = tlm
		m.Compat = json.RawMessage(`{"thinkingFormat":"chat-template","chatTemplateKwargs":` + kwargsJSON + `}`)
	})
}

func ctkParam(t *testing.T, body map[string]any) string {
	t.Helper()
	v, ok := body["chat_template_kwargs"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal chat_template_kwargs: %v", err)
	}
	return string(b)
}

// TestOpenAIChatTemplateKwargs pins pi's chat-template thinking compat
// (upstream 8b97e75c): $var resolution, omitWhenOff, scalar pass-through
// (including null), thinkingLevelMap mapping, and insertion-order preservation.
func TestOpenAIChatTemplateKwargs(t *testing.T) {
	// Ordered: enabled var, effort var (omitWhenOff), literal scalar, null scalar.
	kwargs := `{"thinking":{"$var":"thinking.enabled"},"reasoning_effort":{"$var":"thinking.effort","omitWhenOff":true},"literal":"x","nullable":null}`
	tlm := ai.ThinkingLevelMap{
		"off":     strPtr("none"),
		"minimal": nil,
		"low":     strPtr("low"),
		"high":    strPtr("high"),
	}

	// Effort high: enabled->true, effort->mapped "high", scalars carried, order kept.
	bHigh := buildOpenAIParams(ctkModel(kwargs, tlm), baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if got, want := ctkParam(t, bHigh), `{"thinking":true,"reasoning_effort":"high","literal":"x","nullable":null}`; got != want {
		t.Fatalf("high: chat_template_kwargs = %s, want %s", got, want)
	}

	// Off: enabled->false, effort omitted via omitWhenOff, scalars remain.
	bOff := buildOpenAIParams(ctkModel(kwargs, tlm), baseReq(), &OpenAIOptions{})
	if got, want := ctkParam(t, bOff), `{"thinking":false,"literal":"x","nullable":null}`; got != want {
		t.Fatalf("off: chat_template_kwargs = %s, want %s", got, want)
	}

	// minimal maps to null -> effort key omitted (thinking still true).
	bMin := buildOpenAIParams(ctkModel(kwargs, tlm), baseReq(), &OpenAIOptions{ReasoningEffort: "minimal"})
	if got, want := ctkParam(t, bMin), `{"thinking":true,"literal":"x","nullable":null}`; got != want {
		t.Fatalf("minimal: chat_template_kwargs = %s, want %s", got, want)
	}
}

// TestOpenAIChatTemplateEffortFallbacks covers the thinking.effort lookup edges:
// unmapped level falls back to the raw level; off with a mapped string sends it;
// off with no entry omits.
func TestOpenAIChatTemplateEffortFallbacks(t *testing.T) {
	kwargs := `{"effort":{"$var":"thinking.effort"}}`

	// Unmapped level "medium" (not in map) -> raw level string.
	bMed := buildOpenAIParams(ctkModel(kwargs, ai.ThinkingLevelMap{"high": strPtr("high")}),
		baseReq(), &OpenAIOptions{ReasoningEffort: "medium"})
	if got, want := ctkParam(t, bMed), `{"effort":"medium"}`; got != want {
		t.Fatalf("unmapped level: %s, want %s", got, want)
	}

	// Off with tlm.off mapped to a string -> sent (no omitWhenOff).
	bOffStr := buildOpenAIParams(ctkModel(kwargs, ai.ThinkingLevelMap{"off": strPtr("none")}),
		baseReq(), &OpenAIOptions{})
	if got, want := ctkParam(t, bOffStr), `{"effort":"none"}`; got != want {
		t.Fatalf("off mapped string: %s, want %s", got, want)
	}

	// Off with no tlm.off entry -> reasoningEffort undefined -> whole object omitted.
	bOffNone := buildOpenAIParams(ctkModel(kwargs, ai.ThinkingLevelMap{"high": strPtr("high")}),
		baseReq(), &OpenAIOptions{})
	if has(bOffNone, "chat_template_kwargs") {
		t.Fatalf("off with no off-entry must omit chat_template_kwargs, got %s", ctkParam(t, bOffNone))
	}
}

// TestOpenAIChatTemplateAllOmitted: when every kwarg resolves to omitted, pi
// returns undefined and no chat_template_kwargs param is emitted.
func TestOpenAIChatTemplateAllOmitted(t *testing.T) {
	kwargs := `{"effort":{"$var":"thinking.effort","omitWhenOff":true}}`
	body := buildOpenAIParams(ctkModel(kwargs, ai.ThinkingLevelMap{"off": strPtr("none")}),
		baseReq(), &OpenAIOptions{})
	if has(body, "chat_template_kwargs") {
		t.Fatalf("all-omitted must drop chat_template_kwargs, got %s", ctkParam(t, body))
	}
}
