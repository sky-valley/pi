package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// toolCallArgumentsCaptureFile is what testdata/tool-call-arguments/capture.mjs
// recorded from pi-ai 1.3.0's parseToolCallArguments (upstream 87f9a3641): the
// arguments and argumentsError of a finished call for every prefix of the
// streaming-json documents and for single inputs.
const toolCallArgumentsCaptureFile = "testdata/tool-call-arguments/tool-call-arguments-1.3.0.json"

// A finished tool call's arguments are read strictly, as pi's
// parseToolCallArguments reads them: JSON.parse of the text and then of its
// repair, kept only when the result is an object, else {} and an
// argumentsError of JSON.parse's own SyntaxError message (or "Tool arguments
// must be a JSON object") and the text the model sent. A blank text by JS
// trim is {} with no error.
func TestParseToolCallArgumentsLikePi(t *testing.T) {
	data, err := os.ReadFile(toolCallArgumentsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	type result [2]*string // [JSON.stringify(arguments), message or null]
	var capture struct {
		Documents []struct {
			Doc     string   `json:"doc"`
			Results []result `json:"results"`
		} `json:"documents"`
		Cases []struct {
			Input  string `json:"input"`
			Result result `json:"result"`
		} `json:"cases"`
		Full []struct {
			Input          string `json:"input"`
			ArgumentsError string `json:"argumentsError"`
		} `json:"full"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", toolCallArgumentsCaptureFile, err)
	}
	compared, failed := 0, 0
	check := func(input string, want result) {
		t.Helper()
		if want[0] == nil {
			t.Fatalf("%q: the capture records no arguments; rerun capture.mjs", input)
		}
		if hasLoneSurrogateEscape(*want[0]) {
			return // half a surrogate pair, which a Go string cannot hold
		}
		compared++
		args, ordered, argsErr := parseToolCallArguments(input)
		got := "{}"
		if ordered != nil {
			if got, err = jstext.Stringify(ordered); err != nil {
				t.Fatalf("parseToolCallArguments(%q) has no JSON form: %v", input, err)
			}
		}
		if len(args) != len(ordered) {
			t.Errorf("parseToolCallArguments(%q): the map holds %d members, the ordered form %d", input, len(args), len(ordered))
		}
		wantErr := ""
		if want[1] != nil {
			wantErr = *want[1] + "\n\nReceived arguments:\n" + input
		}
		if got != *want[0] || argsErr != wantErr {
			failed++
			if failed <= 20 {
				t.Errorf("parseToolCallArguments(%q) = %s, %q\npi                             %s, %q", input, got, argsErr, *want[0], wantErr)
			}
		}
	}
	for _, doc := range capture.Documents {
		runes := []rune(doc.Doc)
		if len(runes) != len(doc.Results) {
			t.Fatalf("%q has %d code points and %d results; rerun capture.mjs", doc.Doc, len(runes), len(doc.Results))
		}
		for n := range runes {
			check(string(runes[:n+1]), doc.Results[n])
		}
	}
	for _, c := range capture.Cases {
		check(c.Input, c.Result)
	}
	if len(capture.Full) == 0 {
		t.Fatal("the capture records no complete argumentsError texts; rerun capture.mjs")
	}
	for _, f := range capture.Full {
		if _, _, got := parseToolCallArguments(f.Input); got != f.ArgumentsError {
			t.Errorf("parseToolCallArguments(%q).argumentsError = %q, pi %q", f.Input, got, f.ArgumentsError)
		}
	}
	if failed > 20 {
		t.Errorf("... %d inputs differ in all", failed)
	}
	if compared < 5000 {
		t.Fatalf("compared %d results; the capture records over 5000", compared)
	}
}

// toolCallStreamsCaptureFile is what testdata/tool-call-arguments/
// capture-streams.mjs recorded from pi-ai 1.3.0's anthropic-messages,
// openai-completions and openai-responses adapters: the final message for SSE
// bodies whose tool call finishes with malformed, non-object, empty and valid
// arguments.
const toolCallStreamsCaptureFile = "testdata/tool-call-arguments/tool-call-streams-1.3.0.json"

// Each adapter finishes a tool call with parseToolCallArguments (upstream
// 87f9a3641): a call whose final arguments text is not a JSON object carries
// argumentsError and empty arguments, and the content persists with
// argumentsError after arguments, as pi's JSON.stringify writes it.
func TestFinishedToolCallArgumentsLikePi(t *testing.T) {
	data, err := os.ReadFile(toolCallStreamsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			API          string  `json:"api"`
			Body         string  `json:"body"`
			StopReason   string  `json:"stopReason"`
			ErrorMessage *string `json:"errorMessage"`
			Content      string  `json:"content"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture-streams.mjs", toolCallStreamsCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture-streams.mjs", toolCallStreamsCaptureFile)
	}
	for _, row := range capture.Rows {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, row.Body)
		}))
		req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 0)}})
		base := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}
		var final *ai.AssistantMessage
		switch row.API {
		case "anthropic-messages":
			model := &ai.Model{ID: "claude-x", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}
			final = StreamAnthropic(context.Background(), model, req, &AnthropicOptions{StreamOptions: base}).Result()
		case "openai-completions":
			model := &ai.Model{ID: "gpt-x", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}
			final = StreamOpenAICompletions(context.Background(), model, req, &OpenAIOptions{StreamOptions: base}).Result()
		case "openai-responses":
			model := &ai.Model{ID: "gpt-5-mini", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: server.URL, Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}
			final = StreamOpenAIResponses(context.Background(), model, req, &OpenAIResponsesOptions{StreamOptions: base}).Result()
		default:
			t.Fatalf("row for api %q; rerun capture-streams.mjs", row.API)
		}
		server.Close()
		content, err := json.Marshal(final.Content)
		if err != nil {
			t.Fatal(err)
		}
		gotErr := final.ErrorMessage
		wantErr := ""
		if row.ErrorMessage != nil {
			wantErr = *row.ErrorMessage
		}
		if string(final.StopReason) != row.StopReason || gotErr != wantErr || string(content) != row.Content {
			t.Errorf("%s over\n%s\nended %s %q with %s\npi    %s %q with %s", row.API, row.Body, final.StopReason, gotErr, content, row.StopReason, wantErr, row.Content)
		}
	}
}
