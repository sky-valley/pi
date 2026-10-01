package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// Transliterated from pi packages/ai/test/system-message-replay.test.ts at
// upstream 9e05370b2 (every case, same fixtures). Where that suite uses
// toEqual on a message, the Go side compares serialized bytes captured from
// the same fixtures run under node at the sha (the bytes also pin key order,
// which toEqual does not).

func replayTool(name string, description ...string) Tool {
	d := name + " tool"
	if len(description) > 0 {
		d = description[0]
	}
	return Tool{Name: name, Description: d, Parameters: Object()}
}

func strp(s string) *string { return &s }

// jsJSON marshals v and undoes Go's HTML escaping of <, > and &, so the bytes
// compare against JSON.stringify output captured from pi. That escaping is the
// recorded port-wide divergence D9 (docs/UPSTREAM.md), which is fixed at the
// encoder, not here; these tests pin everything else about the bytes.
func jsJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&").Replace(string(raw))
}

func withSystemText(text string, timestamp int64, edit func(*SystemMessage)) SystemMessage {
	m := NewSystemText(text, timestamp)
	if edit != nil {
		edit(&m)
	}
	return m
}

// replayTranscript is the suite's shared `transcript` fixture.
func replayTranscript() TranscriptContext {
	return NormalizeContext(Context{Messages: []Message{
		withSystemText("base", 10, func(m *SystemMessage) {
			m.Sections = SystemSections{{Name: "a", Value: strp("<a>1</a>")}, {Name: "b", Value: strp("<b>1</b>")}}
			m.ToolsAdded = []Tool{replayTool("first")}
		}),
		NewUserText("hello", 11),
		NewSystemText("also do this", 12),
		AssistantMessage{Content: ContentList{TextContent{Text: "ok"}}, Timestamp: 13},
		withSystemText("", 14, func(m *SystemMessage) {
			m.Sections = SystemSections{{Name: "a", Value: strp("<a>2</a>")}, {Name: "b"}, {Name: "c", Value: strp("<c>1</c>")}}
			m.ToolsRemoved = []ToolReference{{Name: "first"}}
			m.ToolsAdded = []Tool{replayTool("second")}
		}),
	}})
}

func messageRoles(messages []Message) []Role {
	roles := make([]Role, len(messages))
	for i, m := range messages {
		roles[i] = m.MessageRole()
	}
	return roles
}

func TestReplaysContentSectionsAndToolsIntoOneLeadingMessage(t *testing.T) {
	transcript := replayTranscript()
	current, ok := GetCurrentSystemMessage(transcript.Messages)
	if !ok {
		t.Fatal("GetCurrentSystemMessage reported no system message")
	}
	const want = `{"role":"system","content":"base\n\nalso do this","sections":{"a":"<a>2</a>","c":"<c>1</c>"},"toolsAdded":[{"name":"second","description":"second tool","parameters":{"type":"object","properties":{}}}],"timestamp":10}`
	if got := jsJSON(t, current); got != want {
		t.Fatalf("current system message\n got %s\nwant %s", got, want)
	}
	if got, want := GetCurrentSystemPrompt(transcript.Messages), "base\n\nalso do this\n\n<a>2</a>\n\n<c>1</c>"; got != want {
		t.Fatalf("GetCurrentSystemPrompt = %q, want %q", got, want)
	}
}

func TestCollapseKeepsOnlyNonSystemMessagesAfterTheReplayedHead(t *testing.T) {
	collapsed := CollapseSystemMessages(replayTranscript())
	roles := messageRoles(collapsed.Messages)
	if len(roles) != 3 || roles[0] != RoleSystem || roles[1] != RoleUser || roles[2] != RoleAssistant {
		t.Fatalf("collapsed roles = %v, want [system user assistant]", roles)
	}
	const want = `{"messages":[{"role":"system","content":"base\n\nalso do this","sections":{"a":"<a>2</a>","c":"<c>1</c>"},"toolsAdded":[{"name":"second","description":"second tool","parameters":{"type":"object","properties":{}}}],"timestamp":10},{"role":"user","content":"hello","timestamp":11},`
	if got := jsJSON(t, collapsed); len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("collapsed transcript\n got %s\nwant prefix %s", got, want)
	}
	if again, once := jsJSON(t, CollapseSystemMessages(collapsed)), jsJSON(t, collapsed); again != once {
		t.Fatalf("collapse is not idempotent\n once %s\ntwice %s", once, again)
	}
}

func TestReplayOfATranscriptWithoutSystemMessagesIsEmpty(t *testing.T) {
	context := NormalizeContext(Context{Messages: []Message{NewUserText("hi", 1)}})
	if m, ok := GetCurrentSystemMessage(context.Messages); ok {
		t.Fatalf("GetCurrentSystemMessage = %s, want none", jsJSON(t, m))
	}
	if got := GetCurrentSystemPrompt(context.Messages); got != "" {
		t.Fatalf("GetCurrentSystemPrompt = %q, want empty", got)
	}
	if got, want := jsJSON(t, CollapseSystemMessages(context).Messages), jsJSON(t, context.Messages); got != want {
		t.Fatalf("collapse changed a transcript without system messages\n got %s\nwant %s", got, want)
	}
}

func TestALateFullPatchWithoutALeadingMessageReplaysAsThePrompt(t *testing.T) {
	context := NormalizeContext(Context{Messages: []Message{
		NewUserText("old session", 1),
		withSystemText("", 2, func(m *SystemMessage) {
			m.Sections = SystemSections{{Name: "preamble", Value: strp("You are pi.")}}
			m.ToolsAdded = []Tool{replayTool("x")}
		}),
	}})
	if got := GetCurrentSystemPrompt(context.Messages); got != "You are pi." {
		t.Fatalf("GetCurrentSystemPrompt = %q, want %q", got, "You are pi.")
	}
	head, ok := CollapseSystemMessages(context).Messages[0].(SystemMessage)
	if !ok {
		t.Fatalf("collapsed head = %T, want SystemMessage", CollapseSystemMessages(context).Messages[0])
	}
	if len(head.ToolsAdded) != 1 || jsJSON(t, head.ToolsAdded[0]) != jsJSON(t, replayTool("x")) {
		t.Fatalf("collapsed head toolsAdded = %s, want [x]", jsJSON(t, head.ToolsAdded))
	}
}

func TestRendersCompletePromptsAndFramedUpdates(t *testing.T) {
	transcript := replayTranscript()
	leading, ok := transcript.Messages[0].(SystemMessage)
	update, ok2 := transcript.Messages[4].(SystemMessage)
	if !ok || !ok2 {
		t.Fatal("expected system messages")
	}
	if got, want := GetSystemMessageText(leading), "base\n\n<a>1</a>\n\n<b>1</b>"; got != want {
		t.Fatalf("GetSystemMessageText = %q, want %q", got, want)
	}
	want := `Updated system prompt section "a":` + "\n\n<a>2</a>" + "\n\n" +
		`Removed system prompt section "b".` + "\n\n" +
		`Updated system prompt section "c":` + "\n\n<c>1</c>"
	if got := RenderSystemMessageUpdate(update); got != want {
		t.Fatalf("RenderSystemMessageUpdate = %q, want %q", got, want)
	}
}

func TestNormalizesTheLegacyPromptAndToolFieldsIntoALeadingSystemMessage(t *testing.T) {
	messages := []Message{NewUserText("hi", 1)}
	if got, want := jsJSON(t, NormalizeContext(Context{Messages: messages})), `{"messages":[{"role":"user","content":"hi","timestamp":1}]}`; got != want {
		t.Fatalf("no prompt, no tools\n got %s\nwant %s", got, want)
	}
	if got, want := jsJSON(t, NormalizeContext(Context{SystemPrompt: "", Tools: []Tool{}, Messages: messages})), `{"messages":[{"role":"user","content":"hi","timestamp":1}]}`; got != want {
		t.Fatalf("empty prompt and tools\n got %s\nwant %s", got, want)
	}
	const want = `{"messages":[{"role":"system","content":"be brief","toolsAdded":[{"name":"a","description":"a tool","parameters":{"type":"object","properties":{}}}],"timestamp":0},{"role":"user","content":"hi","timestamp":1}]}`
	if got := jsJSON(t, NormalizeContext(Context{SystemPrompt: "be brief", Tools: []Tool{replayTool("a")}, Messages: messages})); got != want {
		t.Fatalf("prompt and tools\n got %s\nwant %s", got, want)
	}
}

func TestComparesToolDeclarationsWithoutUndefinedFields(t *testing.T) {
	// pi's case adds an `execute` function and `constrainedSampling: undefined`;
	// a Go Tool has no executable field, so the structural half is a distinct
	// but equal parameter schema with a nil (undefined) constrained sampling.
	executable := Tool{Name: "a", Description: "a tool", Parameters: Object(), ConstrainedSampling: nil}
	if !DeclarationsEqual(executable, replayTool("a")) {
		t.Fatal("equal declarations compared unequal")
	}
	if DeclarationsEqual(replayTool("a"), replayTool("a", "changed")) {
		t.Fatal("a changed description compared equal")
	}
	falseSampling := replayTool("a")
	falseSampling.ConstrainedSampling = &ConstrainedSamplingConfig{}
	if DeclarationsEqual(replayTool("a"), falseSampling) {
		t.Fatal("constrainedSampling:false compared equal to an absent one")
	}
}

func TestToolStateChangesTreatChangedDefinitionsAsRemovalPlusAddition(t *testing.T) {
	changes := GetToolStateChanges(
		[]Tool{replayTool("a"), replayTool("b")},
		[]Tool{replayTool("b", "changed"), replayTool("c")},
	)
	if got, want := jsJSON(t, changes.ToolsAdded), `[{"name":"b","description":"changed","parameters":{"type":"object","properties":{}}},{"name":"c","description":"c tool","parameters":{"type":"object","properties":{}}}]`; got != want {
		t.Fatalf("toolsAdded\n got %s\nwant %s", got, want)
	}
	if got, want := jsJSON(t, changes.ToolsRemoved), `[{"name":"a"},{"name":"b"}]`; got != want {
		t.Fatalf("toolsRemoved = %s, want %s", got, want)
	}
	same := GetToolStateChanges([]Tool{replayTool("a")}, []Tool{replayTool("a")})
	if len(same.ToolsAdded) != 0 || len(same.ToolsRemoved) != 0 {
		t.Fatalf("unchanged tools = %+v, want no changes", same)
	}
}

func TestDetectsNonAdditiveToolHistoryAndRedefinitions(t *testing.T) {
	transcript := replayTranscript()
	if !HasNonAdditiveToolChanges(transcript.Messages) {
		t.Fatal("a removal was not reported as non-additive")
	}
	if HasToolRedefinitions(transcript.Messages) {
		t.Fatal("distinct names were reported as a redefinition")
	}
	additive := NormalizeContext(Context{Messages: []Message{
		withSystemText("", 1, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("a")} }),
		withSystemText("", 2, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("b")} }),
	}})
	if HasNonAdditiveToolChanges(additive.Messages) {
		t.Fatal("additive history reported as non-additive")
	}
	redeclared := NormalizeContext(Context{Messages: []Message{
		withSystemText("", 1, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("a")} }),
		withSystemText("", 2, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("a", "changed")} }),
	}})
	if !HasNonAdditiveToolChanges(redeclared.Messages) {
		t.Fatal("a redeclaration was not reported as non-additive")
	}
	if !HasToolRedefinitions(redeclared.Messages) {
		t.Fatal("a changed redeclaration was not reported as a redefinition")
	}
	sameAgain := NormalizeContext(Context{Messages: []Message{
		withSystemText("", 1, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("a")} }),
		withSystemText("", 2, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("a")} }),
	}})
	if HasToolRedefinitions(sameAgain.Messages) {
		t.Fatal("an identical redeclaration was reported as a redefinition")
	}
	if !HasNonAdditiveToolChanges(sameAgain.Messages) {
		t.Fatal("an identical redeclaration is still not additive")
	}
}

// ---------------------------------------------------------------------------
// Go-side pins beyond the upstream suite: JS Map / object semantics the port
// reproduces by hand. Expected bytes captured under node at 9e05370b2.
// ---------------------------------------------------------------------------

// sectionsReplayFixture is Amendment A1's Map replay case.
func sectionsReplayFixture() []Message {
	tool := Tool{Name: "a", Description: "d", Parameters: &Schema{Type: "object", Properties: map[string]*Schema{}}}
	return []Message{
		withSystemText("", 5, func(m *SystemMessage) {
			m.Sections = SystemSections{{Name: "b", Value: strp("1")}, {Name: "10", Value: strp("x")}, {Name: "a", Value: strp("2")}}
			m.ToolsAdded = []Tool{tool}
		}),
		withSystemText("", 6, func(m *SystemMessage) {
			m.Sections = SystemSections{{Name: "b"}, {Name: "c", Value: strp("3")}}
		}),
		withSystemText("", 7, func(m *SystemMessage) {
			m.Sections = SystemSections{{Name: "b", Value: strp("again")}}
		}),
	}
}

func TestSectionReplayFollowsMapThenObjectOrder(t *testing.T) {
	current, ok := GetCurrentSystemMessage(sectionsReplayFixture())
	if !ok {
		t.Fatal("no current system message")
	}
	// A deleted-then-re-added name moves to the end of the Map; the integer-like
	// name leads once the Map is materialised as an object.
	const want = `{"role":"system","content":"","sections":{"10":"x","a":"2","c":"3","b":"again"},"toolsAdded":[{"name":"a","description":"d","parameters":{"type":"object","properties":{}}}],"timestamp":5}`
	if got := jsJSON(t, current); got != want {
		t.Fatalf("replayed sections\n got %s\nwant %s", got, want)
	}
	if got, want := GetSystemMessageText(current), "x\n\n2\n\n3\n\nagain"; got != want {
		t.Fatalf("replayed text = %q, want %q", got, want)
	}
}

func TestCollapseJoinsArrayContentAndDropsRemovedTools(t *testing.T) {
	tool := Tool{Name: "a", Description: "d", Parameters: &Schema{Type: "object", Properties: map[string]*Schema{}}}
	collapsed := CollapseSystemMessages(NormalizeContext(Context{Messages: []Message{
		NewUserText("u", 1),
		SystemMessage{Content: ContentList{TextContent{Text: "x"}, TextContent{Text: "y"}}, ToolsAdded: []Tool{tool}, Timestamp: 4},
		withSystemText("z", 3, func(m *SystemMessage) { m.ToolsRemoved = []ToolReference{{Name: "a"}} }),
	}}))
	// The head takes the FIRST system message's timestamp, array content joins
	// with "\n", and a tool added then removed leaves no toolsAdded key.
	const want = `{"messages":[{"role":"system","content":"x\ny\n\nz","timestamp":4},{"role":"user","content":"u","timestamp":1}]}`
	if got := jsJSON(t, collapsed); got != want {
		t.Fatalf("collapsed\n got %s\nwant %s", got, want)
	}
}

func TestResolveTranscriptKeepsOrCollapses(t *testing.T) {
	transcript := replayTranscript()
	if got := ResolveTranscript(transcript, true); len(got.Messages) != 5 {
		t.Fatalf("native transcript has %d messages, want the 5 in place", len(got.Messages))
	}
	if got, want := jsJSON(t, ResolveTranscript(transcript, false)), jsJSON(t, CollapseSystemMessages(transcript)); got != want {
		t.Fatalf("non-native transcript\n got %s\nwant %s", got, want)
	}
}

func TestToolMapsKeepSlotsLikeJSMap(t *testing.T) {
	messages := []Message{
		withSystemText("", 1, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("a"), replayTool("b")} }),
		withSystemText("", 2, func(m *SystemMessage) {
			m.ToolsRemoved = []ToolReference{{Name: "a"}}
			m.ToolsAdded = []Tool{replayTool("a", "a2")}
		}),
		withSystemText("", 3, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("b", "b2")} }),
	}
	obj := `{"type":"object","properties":{}}`
	// Removal then re-add moves "a" behind "b"; re-adding "b" without a removal
	// keeps its slot.
	if got, want := jsJSON(t, GetCurrentTools(messages)), `[{"name":"b","description":"b2","parameters":`+obj+`},{"name":"a","description":"a2","parameters":`+obj+`}]`; got != want {
		t.Fatalf("GetCurrentTools\n got %s\nwant %s", got, want)
	}
	// Declarations never delete, so "a" keeps its first slot with its last definition.
	if got, want := jsJSON(t, GetDeclaredTools(messages)), `[{"name":"a","description":"a2","parameters":`+obj+`},{"name":"b","description":"b2","parameters":`+obj+`}]`; got != want {
		t.Fatalf("GetDeclaredTools\n got %s\nwant %s", got, want)
	}
	resolved := ResolveTranscriptTools(messages, true)
	if resolved.AnchorsAdditions || jsJSON(t, resolved.RequestTools) != jsJSON(t, GetCurrentTools(messages)) {
		t.Fatalf("non-additive history = %+v, want the current tools and no anchoring", resolved)
	}
}

func TestResolveTranscriptToolsAnchorsOnlyAdditiveHistory(t *testing.T) {
	additive := []Message{
		withSystemText("", 1, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("a")} }),
		NewUserText("u", 2),
		withSystemText("", 3, func(m *SystemMessage) { m.ToolsAdded = []Tool{replayTool("b")} }),
	}
	anchored := ResolveTranscriptTools(additive, true)
	if !anchored.AnchorsAdditions || len(anchored.RequestTools) != 1 || anchored.RequestTools[0].Name != "a" {
		t.Fatalf("additive with support = %+v, want the initial [a] anchored", anchored)
	}
	flat := ResolveTranscriptTools(additive, false)
	if flat.AnchorsAdditions || len(flat.RequestTools) != 2 || flat.RequestTools[1].Name != "b" {
		t.Fatalf("additive without support = %+v, want the current [a b]", flat)
	}
	// Without a leading system message the initial tool list is empty.
	noLeading := ResolveTranscriptTools(additive[1:], true)
	if !noLeading.AnchorsAdditions || len(noLeading.RequestTools) != 0 {
		t.Fatalf("no leading message = %+v, want [] anchored", noLeading)
	}
}

func TestInitialSystemMessageHelpers(t *testing.T) {
	if _, ok := CreateInitialSystemMessage("", nil); ok {
		t.Fatal("empty prompt and tools produced a message")
	}
	promptOnly, ok := CreateInitialSystemMessage("p", nil)
	if !ok || jsJSON(t, promptOnly) != `{"role":"system","content":"p","timestamp":0}` {
		t.Fatalf("prompt only = %s", jsJSON(t, promptOnly))
	}
	toolsOnly, ok := CreateInitialSystemMessage("", []Tool{{Name: "a", Description: "d", Parameters: &Schema{Type: "object", Properties: map[string]*Schema{}}}})
	if !ok || jsJSON(t, toolsOnly) != `{"role":"system","content":"","toolsAdded":[{"name":"a","description":"d","parameters":{"type":"object","properties":{}}}],"timestamp":0}` {
		t.Fatalf("tools only = %s", jsJSON(t, toolsOnly))
	}

	user := NewUserText("u", 1)
	withLeading := []Message{NewSystemText("p", 0), user}
	if m, ok := GetInitialSystemMessage(withLeading); !ok || GetSystemMessageText(m) != "p" {
		t.Fatalf("GetInitialSystemMessage = %v, %v", m, ok)
	}
	if _, ok := GetInitialSystemMessage([]Message{user, NewSystemText("p", 0)}); ok {
		t.Fatal("a later system message was reported as the leading one")
	}
	if _, ok := GetInitialSystemMessage(nil); ok {
		t.Fatal("an empty transcript reported a leading message")
	}
	if rest := WithoutInitialSystemMessage(withLeading); len(rest) != 1 || rest[0].MessageRole() != RoleUser {
		t.Fatalf("WithoutInitialSystemMessage = %v", messageRoles(rest))
	}
	if rest := WithoutInitialSystemMessage([]Message{user}); len(rest) != 1 {
		t.Fatalf("WithoutInitialSystemMessage dropped a non-system head: %v", messageRoles(rest))
	}
	// Pointer-form messages replay like value-form ones.
	pointer := NewSystemText("from a pointer", 3)
	if got := GetCurrentSystemPrompt([]Message{&pointer}); got != "from a pointer" {
		t.Fatalf("pointer system message prompt = %q", got)
	}
}

func TestToToolDeclarationCopiesParameters(t *testing.T) {
	original := Tool{Name: "a", Description: "d", Parameters: Object(Prop("x", String()))}
	declared := ToToolDeclaration(original)
	if declared.Parameters == original.Parameters {
		t.Fatal("ToToolDeclaration aliased the parameter schema")
	}
	if got, want := jsJSON(t, declared), jsJSON(t, original); got != want {
		t.Fatalf("declaration\n got %s\nwant %s", got, want)
	}
	withFalse := original
	withFalse.ConstrainedSampling = &ConstrainedSamplingConfig{}
	if got, want := jsJSON(t, ToToolDeclaration(withFalse)), `{"name":"a","description":"d","parameters":{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}},"constrainedSampling":false}`; got != want {
		t.Fatalf("declaration with constrainedSampling:false\n got %s\nwant %s", got, want)
	}
}
