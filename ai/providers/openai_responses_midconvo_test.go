package providers

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// openai-responses models with compat.supportsMidConvoSystemMessages receive
// later system messages in place (upstream 9e05370b2): each is sent as an
// instruction-role message carrying its rendered update. A model with
// additional_tools declares every tool in additional_tools items, the initial
// ones at the head of the input, and announces removals as text (upstream
// caa6967b6); a model with client tool search loads later additions with a
// synthetic tool search, its top-level tools being the state after the last
// removal or redeclaration. The tests before the capture tests transliterate
// the openai-responses cases of packages/ai/test/transcript-tool-changes.test.ts
// at caa6967b6, same fixtures and expected values, through ai.StreamSimple as
// the suite goes through compat.ts streamSimple.

// gpt54 is the suite's native openai-responses model.
func gpt54(compat string) *ai.Model {
	m := foldModel("gpt-5.4", ai.APIOpenAIResponses, "openai", compat)
	m.Name = "GPT-5.4"
	return m
}

// toolNames lists the name of each tool object in a payload array.
func toolNames(tools []map[string]any) []string {
	names := []string{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

// nestedTools reads an input item's tools array.
func nestedTools(t *testing.T, item map[string]any) []map[string]any {
	t.Helper()
	return payloadItems(t, item, "tools")
}

// additionalToolsModel is the suite's `additionalToolsModel`.
func additionalToolsModel() *ai.Model {
	return gpt54(`{"supportsMidConvoSystemMessages":true,"supportsAdditionalTools":true}`)
}

// firstAdditionalToolNames is `input.filter((item) => item.type ===
// "additional_tools").map((item) => item.tools?.[0]?.name)`.
func firstAdditionalToolNames(t *testing.T, input []map[string]any) []string {
	t.Helper()
	names := []string{}
	for _, item := range input {
		if item["type"] == "additional_tools" {
			name := ""
			if tools := nestedTools(t, item); len(tools) > 0 {
				name, _ = tools[0]["name"].(string)
			}
			names = append(names, name)
		}
	}
	return names
}

// itemContent is an input item's string content ("" for none).
func itemContent(item map[string]any) string {
	content, _ := item["content"].(string)
	return content
}

// assertNoRequestTools is `expect(payload.tools).toBeUndefined()`.
func assertNoRequestTools(t *testing.T, body map[string]any) {
	t.Helper()
	if tools, ok := body["tools"]; ok {
		t.Fatalf("body.tools = %v, want none", tools)
	}
}

func TestDeclaresEveryOpenAIToolInAdditionalToolsAtItsDeveloperMessage(t *testing.T) {
	body := captureStreamSimplePayload(t, additionalToolsModel(), transcriptAdditionContext())

	// A later additional_tools item cannot replace a top-level tool, so no tool
	// is declared there.
	assertNoRequestTools(t, body)
	input := payloadItems(t, body, "input")
	if got := firstAdditionalToolNames(t, input); !slices.Equal(got, []string{"base_tool", "late_tool"}) {
		t.Fatalf("additional_tools tools = %v, want [base_tool late_tool]", got)
	}
	// The incremental notice keeps models from reading a later item as the
	// complete tool set.
	if len(input) != 6 {
		t.Fatalf("input has %d items, want 6: %v", len(input), input)
	}
	if input[0]["type"] != "additional_tools" || itemContent(input[1]) != "base prompt" ||
		input[2]["role"] != "user" || !strings.Contains(itemContent(input[3]), "incremental tools update") ||
		input[4]["type"] != "additional_tools" || itemContent(input[5]) != "updated guidance" {
		t.Fatalf("input = %v, want [additional_tools, base prompt, user, incremental notice, additional_tools, updated guidance]", input)
	}
}

func TestMapsSystemMessageAdditionsIntoSyntheticToolSearch(t *testing.T) {
	model := gpt54(`{"supportsMidConvoSystemMessages":true,"supportsToolSearch":true}`)
	body := captureStreamSimplePayload(t, model, transcriptAdditionContext())

	if got := toolNames(payloadItems(t, body, "tools")); !slices.Equal(got, []string{"base_tool"}) {
		t.Fatalf("tools = %v, want [base_tool]", got)
	}
	input := payloadItems(t, body, "input")
	var types []any
	var searched []string
	found := false
	for _, item := range input {
		types = append(types, item["type"])
		if item["type"] == "tool_search_output" && !found {
			searched = toolNames(nestedTools(t, item))
			found = true
		}
	}
	if !slices.Contains(types, any("tool_search_call")) {
		t.Fatalf("input types = %v, want a tool_search_call", types)
	}
	if !slices.Equal(searched, []string{"late_tool"}) {
		t.Fatalf("tool_search_output tools = %v, want [late_tool]", searched)
	}
}

func TestAnnouncesOpenAIRemovalsAsText(t *testing.T) {
	body := captureStreamSimplePayload(t, additionalToolsModel(), transcriptChangesContext())

	assertNoRequestTools(t, body)
	input := payloadItems(t, body, "input")
	if got := firstAdditionalToolNames(t, input); !slices.Equal(got, []string{"base_tool", "late_tool"}) {
		t.Fatalf("additional_tools tools = %v, want [base_tool late_tool]", got)
	}
	update := itemContent(input[len(input)-1])
	if !strings.Contains(update, "updated guidance") ||
		!strings.Contains(update, "The following tools are no longer available. Do not call them:\n- base_tool") {
		t.Fatalf("last input content = %q, want the update and the removal notice for base_tool", update)
	}
}

func TestRedefinesAnOpenAIToolWithAdditionalToolsInsteadOfAnnouncingARemoval(t *testing.T) {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	changed := transcriptTool("base_tool")
	changed.Description = "changed"
	update := ai.NewSystemText("", 2)
	update.ToolsRemoved = []ai.ToolReference{{Name: "base_tool"}}
	update.ToolsAdded = []ai.Tool{changed}
	body := captureStreamSimplePayload(t, additionalToolsModel(), ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}})

	assertNoRequestTools(t, body)
	input := payloadItems(t, body, "input")
	if len(input) != 5 {
		t.Fatalf("input has %d items, want 5: %v", len(input), input)
	}
	declares := func(item map[string]any, description string) bool {
		if item["type"] != "additional_tools" {
			return false
		}
		tools := nestedTools(t, item)
		return len(tools) == 1 && tools[0]["name"] == "base_tool" && tools[0]["description"] == description
	}
	if !declares(input[0], "base_tool tool") ||
		input[1]["role"] != "developer" || itemContent(input[1]) != "base prompt" ||
		input[2]["role"] != "user" ||
		input[3]["role"] != "developer" || !strings.Contains(itemContent(input[3]), "latest definition replaces the earlier one") ||
		!declares(input[4], "changed") {
		t.Fatalf("input = %v, want [base_tool, base prompt, user, incremental notice, changed base_tool]", input)
	}
}

// transcriptToolHistory is the suite's `captureToolHistory` transcript after
// each step: initial tools, removal of drop_tool, then two loads.
func transcriptToolHistory() []ai.Context {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool"), transcriptTool("drop_tool")}
	removal := ai.NewSystemText("", 2)
	removal.ToolsRemoved = []ai.ToolReference{{Name: "drop_tool"}}
	firstLoad := ai.NewSystemText("", 4)
	firstLoad.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	secondLoad := ai.NewSystemText("", 6)
	secondLoad.ToolsAdded = []ai.Tool{transcriptTool("later_tool")}
	steps := [][]ai.Message{
		{ai.NewUserText("first", 1)},
		{removal, ai.NewUserText("after removal", 3)},
		{firstLoad, ai.NewUserText("after first load", 5)},
		{secondLoad, ai.NewUserText("after second load", 7)},
	}
	messages := []ai.Message{leading}
	var contexts []ai.Context
	for _, step := range steps {
		messages = append(messages, step...)
		contexts = append(contexts, ai.Context{Messages: slices.Clone(messages)})
	}
	return contexts
}

// captureToolHistory is the suite's helper of that name: one request body
// after each step of transcriptToolHistory.
func captureToolHistory(t *testing.T, model *ai.Model) []map[string]any {
	t.Helper()
	var bodies []map[string]any
	for _, req := range transcriptToolHistory() {
		bodies = append(bodies, captureStreamSimplePayload(t, model, req))
	}
	return bodies
}

// expectGrowingPrefix is the suite's helper of that name: every request's
// message list starts with the previous request's message list.
func expectGrowingPrefix(t *testing.T, bodies []map[string]any, key string) {
	t.Helper()
	var previous []string
	for index, body := range bodies {
		var current []string
		for _, item := range payloadItems(t, body, key) {
			current = append(current, mustJSON(t, item))
		}
		if index > 0 && (len(current) < len(previous) || !slices.Equal(current[:len(previous)], previous)) {
			t.Fatalf("request %d %s does not start with request %d's:\n got %v\nwant prefix %v", index, key, index-1, current, previous)
		}
		previous = current
	}
}

// #10940: deferred loads after a removal must not rewrite the cached prefix.
func TestKeepsTheOpenAIRequestPrefixAcrossARemovalAndLaterLoads(t *testing.T) {
	bodies := captureToolHistory(t, additionalToolsModel())

	for _, body := range bodies {
		assertNoRequestTools(t, body)
	}
	expectGrowingPrefix(t, bodies, "input")
	last := payloadItems(t, bodies[len(bodies)-1], "input")
	var declared [][]string
	for _, item := range last {
		if item["type"] == "additional_tools" {
			declared = append(declared, toolNames(nestedTools(t, item)))
		}
	}
	if want := [][]string{{"base_tool", "drop_tool"}, {"late_tool"}, {"later_tool"}}; !reflect.DeepEqual(declared, want) {
		t.Fatalf("additional_tools items = %v, want %v", declared, want)
	}
	if !slices.ContainsFunc(last, func(item map[string]any) bool {
		return strings.Contains(itemContent(item), "no longer available. Do not call them:\n- drop_tool")
	}) {
		t.Fatalf("no input item announces drop_tool's removal: %v", last)
	}
	// Each later load restates the tools that are still unavailable.
	var notices []string
	for _, item := range last {
		if strings.Contains(itemContent(item), "incremental tools update") {
			notices = append(notices, itemContent(item))
		}
	}
	if len(notices) != 2 || !strings.Contains(notices[0], "\nStill unavailable: drop_tool.") ||
		!strings.Contains(notices[1], "\nStill unavailable: drop_tool.") {
		t.Fatalf("incremental notices = %q, want two restating drop_tool", notices)
	}
}

func TestStopsListingAToolAsUnavailableOnceItIsDeclaredAgain(t *testing.T) {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool"), transcriptTool("drop_tool")}
	removal := ai.NewSystemText("", 1)
	removal.ToolsRemoved = []ai.ToolReference{{Name: "drop_tool"}}
	readd := ai.NewSystemText("", 2)
	readd.ToolsAdded = []ai.Tool{transcriptTool("drop_tool")}
	body := captureStreamSimplePayload(t, additionalToolsModel(), ai.Context{Messages: []ai.Message{leading, removal, readd, ai.NewUserText("go", 3)}})

	input := payloadItems(t, body, "input")
	index := slices.IndexFunc(input, func(item map[string]any) bool {
		return strings.Contains(itemContent(item), "incremental tools update")
	})
	if index < 0 {
		t.Fatalf("no incremental tools notice in %v", input)
	}
	if notice := itemContent(input[index]); strings.Contains(notice, "Still unavailable") {
		t.Fatalf("notice = %q, want no Still unavailable line", notice)
	}
	if item := input[len(input)-2]; item["type"] != "additional_tools" || !slices.Equal(toolNames(nestedTools(t, item)), []string{"drop_tool"}) {
		t.Fatalf("input[-2] = %v, want additional_tools [drop_tool]", item)
	}
}

// #10940: synthetic tool search cannot remove tools, so a removal rewrites the
// prefix once.
func TestKeepsTheOpenAIToolSearchPrefixAcrossLoadsAfterARemoval(t *testing.T) {
	bodies := captureToolHistory(t, gpt54(`{"supportsMidConvoSystemMessages":true,"supportsToolSearch":true}`))
	beforeRemoval, afterRemoval := bodies[0], bodies[1:]

	if got := toolNames(payloadItems(t, beforeRemoval, "tools")); !slices.Equal(got, []string{"base_tool", "drop_tool"}) {
		t.Fatalf("tools before the removal = %v, want [base_tool drop_tool]", got)
	}
	for i, body := range afterRemoval {
		if got := toolNames(payloadItems(t, body, "tools")); !slices.Equal(got, []string{"base_tool"}) {
			t.Fatalf("tools %d after the removal = %v, want [base_tool]", i, got)
		}
	}
	expectGrowingPrefix(t, afterRemoval, "input")
	var loaded []string
	for _, item := range payloadItems(t, afterRemoval[len(afterRemoval)-1], "input") {
		if item["type"] == "tool_search_output" {
			loaded = append(loaded, toolNames(nestedTools(t, item))[0])
		}
	}
	if !slices.Equal(loaded, []string{"late_tool", "later_tool"}) {
		t.Fatalf("tool_search_output tools = %v, want [late_tool later_tool]", loaded)
	}
}

const responsesMidConvoCaptureFile = "testdata/transcript/responses-midconvo-caa6967b6.json"

// responsesMidConvoCapture is what capture-responses-midconvo.mts records.
type responsesMidConvoCapture struct {
	SHA            string                                 `json:"sha"`
	Bodies         map[string]responsesMidConvoCase       `json:"bodies"`
	LoneSurrogates responsesMidConvoCase                  `json:"loneSurrogates"`
	Streams        map[string]responsesMidConvoStreamCase `json:"streams"`
}

// responsesMidConvoCase is one body case of the capture: the model and context
// pi was given and the request body it built.
type responsesMidConvoCase struct {
	Model   ai.Model        `json:"model"`
	Context json.RawMessage `json:"context"`
	Body    json.RawMessage `json:"body"`
}

// responsesMidConvoStreamCase is one stream case of the capture: the model and
// context pi was given, the SSE response it read, and what it parsed from it.
type responsesMidConvoStreamCase struct {
	Model      ai.Model        `json:"model"`
	Context    json.RawMessage `json:"context"`
	SSE        string          `json:"sse"`
	StopReason ai.StopReason   `json:"stopReason"`
	ToolCalls  json.RawMessage `json:"toolCalls"`
}

// loadResponsesMidConvoCapture reads the capture, checking it holds the cases
// the tests expect.
func loadResponsesMidConvoCapture(t *testing.T) responsesMidConvoCapture {
	t.Helper()
	data, err := os.ReadFile(responsesMidConvoCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture responsesMidConvoCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", responsesMidConvoCaptureFile, err)
	}
	if len(capture.Bodies) != 24 || len(capture.Streams) != 2 || capture.LoneSurrogates.Body == nil {
		t.Fatalf("%s has %d body cases, %d stream cases and loneSurrogates %t, want 24, 2 and true; rerun capture-responses-midconvo.mts",
			responsesMidConvoCaptureFile, len(capture.Bodies), len(capture.Streams), capture.LoneSurrogates.Body != nil)
	}
	return capture
}

// decodeTranscriptFixture decodes a recorded pi context ({messages}), whose
// messages are role-discriminated.
func decodeTranscriptFixture(t *testing.T, raw json.RawMessage) ai.Context {
	t.Helper()
	var recorded struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	var req ai.Context
	for i, m := range recorded.Messages {
		message, err := ai.UnmarshalMessage(m)
		if err != nil {
			t.Fatalf("messages[%d]: %v", i, err)
		}
		req.Messages = append(req.Messages, message)
	}
	return req
}

// The complete request bodies pi builds for native models, captured under node
// at upstream caa6967b6 by testdata/transcript/capture-responses-midconvo.mts:
// the suite's cases (one body per step of its tool history, for the
// additional_tools and the tool search model), then the tool_search call_id seed
// (pi_tool_load_<shortHash("system:<msgIndex>:<names>")>, where the leading
// system message does not advance msgIndex and a system message held behind a
// pending tool call takes its transformed position), rendered section updates,
// the system instruction role (additional_tools items stay developer under a
// system instruction role), defer_loading on custom and strict tool search
// results only, additional_tools over tool search, later system messages that
// add no tools (no additional_tools item, no tool_search pair), transcripts
// without a leading system message, one whose leading system message is the
// first transformed message because an aborted assistant message before it is
// dropped, and a removed grammar tool whose call still replays as a custom tool
// call.
func TestResponsesMidConvoRequestBodies(t *testing.T) {
	capture := loadResponsesMidConvoCapture(t)
	for _, name := range slices.Sorted(maps.Keys(capture.Bodies)) {
		t.Run(name, func(t *testing.T) {
			c := capture.Bodies[name]
			body := captureStreamSimplePayload(t, &c.Model, decodeTranscriptFixture(t, c.Context))
			assertRequestBody(t, body, c.Body)
		})
	}
}

// Leading and later system text goes out with lone UTF-16 surrogates removed
// and paired ones kept, as pi's sanitizeSurrogates does (captured under node at
// 9e05370b2, "loneSurrogates"). encoding/json turns a lone surrogate escape into
// U+FFFD, so the context is built here with the surrogates as WTF-8 triples,
// the form a Go string carries them in; each string is checked against the
// recorded context's JSON.stringify form.
func TestResponsesMidConvoSanitizesLoneSurrogatesInSystemText(t *testing.T) {
	const (
		highD83D = "\xed\xa0\xbd"
		lowDE48  = "\xed\xb9\x88"
	)
	c := loadResponsesMidConvoCapture(t).LoneSurrogates

	leadingText := "base " + highD83D + " prompt " + highD83D + lowDE48
	updateText := "updated " + lowDE48 + " guidance"
	rules := "<rules>\nr" + highD83D + "\n</rules>"
	for _, s := range []string{leadingText, updateText, rules} {
		if !strings.Contains(string(c.Context), jsQuote(s)) {
			t.Fatalf("recorded context does not carry %s; keep this fixture in step with capture-responses-midconvo.mts", jsQuote(s))
		}
	}
	leading := ai.NewSystemText(leadingText, 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	update := ai.NewSystemText(updateText, 2)
	update.Sections = ai.SystemSections{{Name: "rules", Value: &rules}}
	req := ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}

	body := captureStreamSimplePayload(t, &c.Model, req)
	assertRequestBody(t, body, c.Body)
}

// The stream parses grammar tool calls with the input properties of every tool
// the resolved transcript declared, as pi's stream does: a grammar tool that a
// later system message removed still parses as {query} on a native model, whose
// transcript keeps the declaring message, and as {input} on a collapsed one,
// whose single system message declares only the current tools. Captured under
// node at 9e05370b2 by feeding pi the recorded SSE through its fetch option.
func TestResponsesMidConvoStreamedGrammarToolCalls(t *testing.T) {
	capture := loadResponsesMidConvoCapture(t)
	for _, name := range slices.Sorted(maps.Keys(capture.Streams)) {
		t.Run(name, func(t *testing.T) {
			c := capture.Streams[name]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, c.SSE)
			}))
			defer server.Close()
			model := c.Model
			model.BaseURL = server.URL
			opts := &ai.SimpleStreamOptions{}
			opts.APIKey = "test-key"
			final := ai.StreamSimple(context.Background(), &model, decodeTranscriptFixture(t, c.Context), opts).Result()
			if final.StopReason != c.StopReason {
				t.Fatalf("stop reason = %s (%q), want %s", final.StopReason, final.ErrorMessage, c.StopReason)
			}
			type parsedToolCall struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			calls := []parsedToolCall{}
			for _, block := range final.Content {
				if call, ok := block.(ai.ToolCall); ok {
					calls = append(calls, parsedToolCall{Name: call.Name, Arguments: call.Arguments})
				}
			}
			gotJSON, err := json.Marshal(calls)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(gotJSON, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.ToolCalls, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parsed tool calls = %s, want pi's %s", gotJSON, c.ToolCalls)
			}
		})
	}
}

// Copilot dynamic headers read the transcript the adapter resolved: a native
// model keeps a trailing system message in place, so the request is
// agent-initiated (pi at 9e05370b2 under node: X-Initiator "agent" with
// supportsMidConvoSystemMessages, "user" without).
func TestCopilotInitiatorReadsTheResolvedResponsesTranscript(t *testing.T) {
	for _, tc := range []struct {
		compat string
		want   string
	}{
		{`{"supportsMidConvoSystemMessages":true}`, "agent"},
		{"", "user"},
	} {
		var initiator string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			initiator = r.Header.Get("X-Initiator")
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, responsesSSE)
		}))
		model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "github-copilot", BaseURL: server.URL, Input: []string{"text"}}
		if tc.compat != "" {
			model.Compat = json.RawMessage(tc.compat)
		}
		req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1), ai.NewSystemText("later guidance", 2)}}
		opts := &ai.SimpleStreamOptions{}
		opts.APIKey = "k"
		ai.StreamSimple(context.Background(), model, req, opts).Result()
		server.Close()
		if initiator != tc.want {
			t.Fatalf("compat %s: X-Initiator = %q, want %q", tc.compat, initiator, tc.want)
		}
	}
}
