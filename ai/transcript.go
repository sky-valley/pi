package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// Transcript replay, ported from pi packages/ai/src/utils/transcript.ts
// (upstream 9e05370b2). Everything here is root-exported upstream
// (`export * from "./utils/transcript.ts"`).
//
// pi's replay helpers accept any `{ role: string }[]` so agent transcripts that
// carry custom roles pass without filtering; []Message is that list here (the
// agent package's AgentMessage is an alias of Message), and only system
// messages are read.

// SystemSection is one named prompt section. A nil Value is JSON null: on a
// later system message it removes the section.
type SystemSection struct {
	Name  string
	Value *string
}

// SystemSections is pi's `Record<string, string | null>` for
// SystemMessage.sections: an insertion-ordered set of named sections.
//
// pi renders sections with Object.values/Object.entries, which walk a JS
// object's own properties in its own order — array-index-like names ascending
// first, then every other name in insertion order. Entries, MarshalJSON and
// every renderer here follow that order, which is why upstream warns "Avoid
// integer-like names". Setting an existing name keeps its slot, as assigning a
// JS property does; a repeated name in a literal keeps its first slot and its
// last value, as a JS object literal does.
type SystemSections []SystemSection

// Get returns the value for name and whether the name is present. A present
// name with a nil value is a removal.
func (s SystemSections) Get(name string) (*string, bool) {
	var value *string
	found := false
	for _, section := range s {
		if section.Name == name {
			value, found = section.Value, true
		}
	}
	return value, found
}

// Set assigns value to name, keeping the slot of an existing name and
// appending a new one.
func (s *SystemSections) Set(name string, value *string) {
	for i, section := range *s {
		if section.Name == name {
			(*s)[i].Value = value
			s.dropRepeatsAfter(i)
			return
		}
	}
	*s = append(*s, SystemSection{Name: name, Value: value})
}

// dropRepeatsAfter removes later entries repeating the name at index i, so a
// value just assigned is not shadowed by a stale literal duplicate.
func (s *SystemSections) dropRepeatsAfter(i int) {
	name := (*s)[i].Name
	for _, section := range (*s)[i+1:] {
		if section.Name == name {
			*s = s.without(name, i+1)
			return
		}
	}
}

// without returns a fresh slice holding every entry except those named name
// at or after index from. It never writes through the receiver's backing
// array, which another message may share.
func (s SystemSections) without(name string, from int) SystemSections {
	out := make(SystemSections, 0, len(s))
	for i, section := range s {
		if i < from || section.Name != name {
			out = append(out, section)
		}
	}
	return out
}

// Delete removes name.
func (s *SystemSections) Delete(name string) {
	if _, ok := s.Get(name); !ok {
		return
	}
	*s = s.without(name, 0)
}

// Len reports the number of distinct names.
func (s SystemSections) Len() int {
	return len(s.unique())
}

// unique collapses repeated names onto their first slot with their last value.
func (s SystemSections) unique() []SystemSection {
	out := make([]SystemSection, 0, len(s))
	index := make(map[string]int, len(s))
	for _, section := range s {
		if i, ok := index[section.Name]; ok {
			out[i].Value = section.Value
			continue
		}
		index[section.Name] = len(out)
		out = append(out, section)
	}
	return out
}

// Entries returns the distinct sections in JS own-property order.
func (s SystemSections) Entries() []SystemSection {
	entries := s.unique()
	sort.SliceStable(entries, func(i, j int) bool {
		a, aIndex := jstext.ArrayIndexKey(entries[i].Name)
		b, bIndex := jstext.ArrayIndexKey(entries[j].Name)
		if aIndex && bIndex {
			return a < b
		}
		return aIndex && !bIndex
	})
	return entries
}

// MarshalJSON writes the sections as a JSON object in JS own-property order; a
// nil value is null.
func (s SystemSections) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, section := range s.Entries() {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(section.Name)
		if err != nil {
			return nil, err
		}
		buf.Write(name)
		buf.WriteByte(':')
		value, err := json.Marshal(section.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads a JSON object of string-or-null values, keeping document
// order (a repeated name keeps its first slot and its last value, as
// JSON.parse does).
func (s *SystemSections) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		*s = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return err
	} else if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("ai: system message sections must be a JSON object of strings or null, got %s", bytes.TrimSpace(data))
	}
	out := SystemSections{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("ai: system message section %q must be a string or null, got %s", name, raw)
		}
		out.Set(name, value)
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	*s = out
	return nil
}

// systemMessageOf reads a system message in value or pointer form.
func systemMessageOf(message Message) (SystemMessage, bool) {
	switch m := message.(type) {
	case SystemMessage:
		return m, true
	case *SystemMessage:
		if m != nil {
			return *m, true
		}
	}
	return SystemMessage{}, false
}

// CreateInitialSystemMessage builds the leading system message for a prompt and
// tool set. It reports false when both are empty, so an empty transcript stays
// empty.
func CreateInitialSystemMessage(systemPrompt string, tools []Tool) (SystemMessage, bool) {
	hasSystemPrompt := len(systemPrompt) > 0
	hasTools := len(tools) > 0
	if !hasSystemPrompt && !hasTools {
		return SystemMessage{}, false
	}
	message := NewSystemText(systemPrompt, 0)
	if hasTools {
		message.ToolsAdded = tools
	}
	return message, true
}

// NormalizeContext folds Context.SystemPrompt and Context.Tools into a leading
// system message. It is the only producer of a TranscriptContext; every
// provider-facing function expects its result.
func NormalizeContext(context Context) TranscriptContext {
	initial, ok := CreateInitialSystemMessage(context.SystemPrompt, context.Tools)
	if !ok {
		return TranscriptContext{Messages: context.Messages}
	}
	messages := make([]Message, 0, len(context.Messages)+1)
	messages = append(messages, initial)
	messages = append(messages, context.Messages...)
	return TranscriptContext{Messages: messages}
}

// GetInitialSystemMessage returns the leading system message, if the transcript
// starts with one.
func GetInitialSystemMessage(messages []Message) (SystemMessage, bool) {
	if len(messages) == 0 {
		return SystemMessage{}, false
	}
	return systemMessageOf(messages[0])
}

// WithoutInitialSystemMessage drops the leading system message for APIs that
// carry the prompt outside the message list.
func WithoutInitialSystemMessage(messages []Message) []Message {
	if _, ok := GetInitialSystemMessage(messages); ok {
		return messages[1:]
	}
	return messages
}

// toolMap is a JS Map<string, Tool>: set keeps an existing name's slot, delete
// frees it, and values come back in insertion order.
type toolMap struct {
	order  []string
	byName map[string]Tool
}

func newToolMap() *toolMap { return &toolMap{byName: map[string]Tool{}} }

func (m *toolMap) set(tool Tool) {
	if _, ok := m.byName[tool.Name]; !ok {
		m.order = append(m.order, tool.Name)
	}
	m.byName[tool.Name] = tool
}

func (m *toolMap) get(name string) (Tool, bool) {
	tool, ok := m.byName[name]
	return tool, ok
}

func (m *toolMap) delete(name string) {
	if _, ok := m.byName[name]; !ok {
		return
	}
	delete(m.byName, name)
	for i, n := range m.order {
		if n == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *toolMap) values() []Tool {
	out := make([]Tool, len(m.order))
	for i, name := range m.order {
		out[i] = m.byName[name]
	}
	return out
}

// GetCurrentTools resolves the tools available after applying every transcript
// delta in order.
func GetCurrentTools(messages []Message) []Tool {
	tools := newToolMap()
	for _, message := range messages {
		system, ok := systemMessageOf(message)
		if !ok {
			continue
		}
		for _, tool := range system.ToolsRemoved {
			tools.delete(tool.Name)
		}
		for _, tool := range system.ToolsAdded {
			tools.set(tool)
		}
	}
	return tools.values()
}

// GetCurrentSystemMessage replays every system message into one leading system
// message holding the current prompt and tools. Later content is appended to
// the base prompt, sections are patched by name, and tools are resolved with
// GetCurrentTools. It reports false when the transcript has no system message.
func GetCurrentSystemMessage(messages []Message) (SystemMessage, bool) {
	var content []string
	// A SystemSections used as pi's Map<string, string>: Set keeps a slot,
	// Delete frees it, and the result is re-read in object order when it is
	// materialised onto the message (pi's Object.fromEntries).
	sections := SystemSections{}
	var timestamp int64
	found := false
	for _, message := range messages {
		system, ok := systemMessageOf(message)
		if !ok {
			continue
		}
		if !found {
			timestamp, found = system.Timestamp, true
		}
		if text := ContentText(system.Content); len(text) > 0 {
			content = append(content, text)
		}
		for _, section := range system.Sections.Entries() {
			if section.Value == nil {
				sections.Delete(section.Name)
			} else {
				sections.Set(section.Name, section.Value)
			}
		}
	}
	tools := GetCurrentTools(messages)
	if !found && len(tools) == 0 {
		return SystemMessage{}, false
	}
	current := NewSystemText(strings.Join(content, "\n\n"), timestamp)
	if len(sections) > 0 {
		current.Sections = sections
	}
	if len(tools) > 0 {
		current.ToolsAdded = tools
	}
	return current, true
}

// GetCurrentSystemPrompt renders the current system prompt text after
// replaying every system message.
func GetCurrentSystemPrompt(messages []Message) string {
	if message, ok := GetCurrentSystemMessage(messages); ok {
		return GetSystemMessageText(message)
	}
	return ""
}

// CollapseSystemMessages rebuilds the transcript for APIs without
// mid-conversation system messages: the replayed system message leads, and
// every later system message is dropped.
func CollapseSystemMessages(context TranscriptContext) TranscriptContext {
	head, hasHead := GetCurrentSystemMessage(context.Messages)
	messages := make([]Message, 0, len(context.Messages)+1)
	if hasHead {
		messages = append(messages, head)
	}
	for _, message := range context.Messages {
		if message.MessageRole() != RoleSystem {
			messages = append(messages, message)
		}
	}
	return TranscriptContext{Messages: messages}
}

// ResolveTranscript keeps later system messages in place when the model accepts
// them, and collapses them otherwise.
func ResolveTranscript(context TranscriptContext, supportsMidConvoSystemMessages bool) TranscriptContext {
	if supportsMidConvoSystemMessages {
		return context
	}
	return CollapseSystemMessages(context)
}

// ToToolDeclaration strips a tool down to what it declares to the model — name,
// description, parameters and constrained sampling — for transcript comparison
// or persistence. Parameters are deep-copied through JSON, as pi's
// `JSON.parse(JSON.stringify(...))` does.
func ToToolDeclaration(tool Tool) Tool {
	declaration := Tool{Name: tool.Name, Description: tool.Description}
	if tool.Parameters != nil {
		var parameters Schema
		if raw, err := json.Marshal(tool.Parameters); err == nil && json.Unmarshal(raw, &parameters) == nil {
			declaration.Parameters = &parameters
		} else {
			// A schema JSON cannot carry (pi's JSON.stringify would throw) is still
			// copied, so the declaration never aliases the caller's tool.
			declaration.Parameters = tool.Parameters.Clone()
		}
	}
	if tool.ConstrainedSampling != nil {
		sampling := *tool.ConstrainedSampling
		declaration.ConstrainedSampling = &sampling
	}
	return declaration
}

// DeclarationsEqual reports whether two tools declare the same interface to the
// model: both sides go through ToToolDeclaration and their serialized forms are
// compared. A declaration that cannot be serialized equals nothing.
func DeclarationsEqual(left, right Tool) bool {
	l, err := json.Marshal(ToToolDeclaration(left))
	if err != nil {
		return false
	}
	r, err := json.Marshal(ToToolDeclaration(right))
	if err != nil {
		return false
	}
	return bytes.Equal(l, r)
}

// ToolStateChanges is the difference between two complete tool states.
type ToolStateChanges struct {
	ToolsAdded   []Tool
	ToolsRemoved []ToolReference
}

// GetToolStateChanges compares two complete tool states. A changed definition
// is a removal followed by an addition.
func GetToolStateChanges(previous, current []Tool) ToolStateChanges {
	previousTools := newToolMap()
	for _, tool := range previous {
		previousTools.set(tool)
	}
	currentTools := newToolMap()
	for _, tool := range current {
		currentTools.set(tool)
	}
	var changes ToolStateChanges
	for _, tool := range current {
		if previousTool, ok := previousTools.get(tool.Name); !ok || !DeclarationsEqual(previousTool, tool) {
			changes.ToolsAdded = append(changes.ToolsAdded, ToToolDeclaration(tool))
		}
	}
	for _, tool := range previous {
		if currentTool, ok := currentTools.get(tool.Name); !ok || !DeclarationsEqual(tool, currentTool) {
			changes.ToolsRemoved = append(changes.ToolsRemoved, ToolReference{Name: tool.Name})
		}
	}
	return changes
}

// GetDeclaredTools returns every definition referenced by transcript tool
// state, in first-declaration order (a later declaration of a name replaces
// the definition in place).
func GetDeclaredTools(messages []Message) []Tool {
	definitions := newToolMap()
	for _, message := range messages {
		system, ok := systemMessageOf(message)
		if !ok {
			continue
		}
		for _, tool := range system.ToolsAdded {
			definitions.set(tool)
		}
	}
	return definitions.values()
}

// HasToolRedefinitions reports whether a tool name was declared twice with
// different definitions. A transport that can only reference previously
// declared tools by name cannot replay such a history.
//
// Deprecated: No built-in transport needs this anymore: Anthropic expresses
// redefinitions with inline tool_definition blocks (upstream b271b0a52). Kept
// for API compatibility, as pi keeps it; it will be removed when pi removes it.
func HasToolRedefinitions(messages []Message) bool {
	declared := newToolMap()
	for _, message := range messages {
		system, ok := systemMessageOf(message)
		if !ok {
			continue
		}
		for _, tool := range system.ToolsAdded {
			if previous, ok := declared.get(tool.Name); ok && !DeclarationsEqual(previous, tool) {
				return true
			}
			declared.set(tool)
		}
	}
	return false
}

// HasNonAdditiveToolChanges reports whether tool history contains a removal or
// a same-name redeclaration that an addition-only transport cannot replay.
func HasNonAdditiveToolChanges(messages []Message) bool {
	declared := map[string]bool{}
	for _, message := range messages {
		system, ok := systemMessageOf(message)
		if !ok {
			continue
		}
		if len(system.ToolsRemoved) > 0 {
			return true
		}
		for _, tool := range system.ToolsAdded {
			if declared[tool.Name] {
				return true
			}
			declared[tool.Name] = true
		}
	}
	return false
}

// TranscriptToolSupport is how a transport can express tool changes after the
// first request (pi's TranscriptToolSupport).
type TranscriptToolSupport string

const (
	// TranscriptToolSupportNone: only the top-level tool list exists; it always
	// holds the current tools.
	TranscriptToolSupportNone TranscriptToolSupport = "none"
	// TranscriptToolSupportAdditions: tools can be added at a later message,
	// but not removed or redefined.
	TranscriptToolSupportAdditions TranscriptToolSupport = "additions"
	// TranscriptToolSupportChanges: every tool, including the initial ones, is
	// declared in place, and a later declaration under the same name replaces
	// the earlier one. Removals are not expressible on the wire, so the
	// transport announces them as text and the removed tools stay declared.
	TranscriptToolSupportChanges TranscriptToolSupport = "changes"
)

// TranscriptTools splits tool declarations between the top-level request field
// and in-place changes.
type TranscriptTools struct {
	// RequestTools are the tools sent in the top-level request field.
	RequestTools []Tool
	// FoldedSystemMessages is the number of leading system messages whose tool
	// changes RequestTools already holds. Every later system message carries
	// its own tool changes in place.
	FoldedSystemMessages int
}

// ResolveTranscriptTools splits tool declarations between the top-level request
// field and in-place changes.
//
// With TranscriptToolSupportChanges, the top-level list stays empty and every
// system message, including the leading one, declares its tools in place: an
// in-place declaration can replace an earlier in-place one, but not a
// top-level one. With TranscriptToolSupportAdditions, a removal or
// redefinition cannot be expressed in place, so the top-level list becomes the
// tool state after the last such change; later additions stay anchored, and
// the cached prefix is invalidated once per removal instead of on every later
// addition. With TranscriptToolSupportNone, the top-level list is the current
// tool set.
func ResolveTranscriptTools(messages []Message, support TranscriptToolSupport) TranscriptTools {
	var systemMessages []Message
	for _, message := range messages {
		if _, ok := systemMessageOf(message); ok {
			systemMessages = append(systemMessages, message)
		}
	}
	if support == TranscriptToolSupportNone {
		return TranscriptTools{RequestTools: GetCurrentTools(systemMessages), FoldedSystemMessages: len(systemMessages)}
	}
	if support == TranscriptToolSupportChanges {
		return TranscriptTools{RequestTools: []Tool{}, FoldedSystemMessages: 0}
	}
	folded := 0
	if _, ok := GetInitialSystemMessage(messages); ok {
		folded = 1
	}
	if support == TranscriptToolSupportAdditions {
		// A name declared before counts as a redeclaration even after its
		// removal: Kimi K3 ignores a removed tool that a later tool message
		// declares again (verified October 2026).
		declared := map[string]bool{}
		for index, message := range systemMessages {
			system, _ := systemMessageOf(message)
			redeclares := slices.ContainsFunc(system.ToolsAdded, func(tool Tool) bool { return declared[tool.Name] })
			if len(system.ToolsRemoved) > 0 || redeclares {
				folded = max(folded, index+1)
			}
			for _, tool := range system.ToolsAdded {
				declared[tool.Name] = true
			}
		}
	}
	return TranscriptTools{RequestTools: GetCurrentTools(systemMessages[:folded]), FoldedSystemMessages: folded}
}
