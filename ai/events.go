package ai

import "encoding/json"

// EventType is the discriminator for AssistantMessageEvent.
type EventType string

const (
	EventStart         EventType = "start"
	EventTextStart     EventType = "text_start"
	EventTextDelta     EventType = "text_delta"
	EventTextEnd       EventType = "text_end"
	EventThinkingStart EventType = "thinking_start"
	EventThinkingDelta EventType = "thinking_delta"
	EventThinkingEnd   EventType = "thinking_end"
	EventToolCallStart EventType = "toolcall_start"
	EventToolCallDelta EventType = "toolcall_delta"
	EventToolCallEnd   EventType = "toolcall_end"
	EventDone          EventType = "done"
	EventError         EventType = "error"
)

// AssistantMessageEvent is one event in the streaming protocol. It is a flat
// struct (Go has no discriminated unions) carrying the union of fields used by
// the variants documented in pi's AssistantMessageEvent.
//
// Streams emit "start" before partial updates, then terminate with either
// "done" (final successful message) or "error" (final message with stopReason
// "error"/"aborted" and ErrorMessage).
type AssistantMessageEvent struct {
	Type EventType `json:"type"`
	// ContentIndex is set for per-block events (text/thinking/toolcall). pi marks
	// it required on those events (types.ts:360-368); no omitempty so index 0 is
	// not dropped on serialize.
	ContentIndex int `json:"contentIndex"`
	// Delta is the incremental text for *_delta events. Required on delta events
	// in pi (types.ts:361,364,367).
	Delta string `json:"delta"`
	// Content is the finished text for text_end / thinking_end events. Required on
	// those events in pi (types.ts:362,365).
	Content string `json:"content"`
	// ToolCall is the finished tool call for toolcall_end events.
	ToolCall *ToolCall `json:"toolCall,omitempty"`
	// Partial is the in-progress assistant message (all non-terminal events).
	Partial *AssistantMessage `json:"partial,omitempty"`
	// Reason is the stop reason for done/error events. Required on those events in
	// pi (types.ts:369-370).
	Reason StopReason `json:"reason"`
	// Message is the final assistant message for "done" events.
	Message *AssistantMessage `json:"message,omitempty"`
	// Error is the final assistant message for "error" events.
	Error *AssistantMessage `json:"error,omitempty"`
}

// MarshalJSON serializes the event with exactly the fields pi's corresponding
// union variant carries (types.ts AssistantMessageEvent):
//
//	start                                  {type, partial}
//	text_start/thinking_start/toolcall_start {type, contentIndex, partial}
//	text_delta/thinking_delta/toolcall_delta {type, contentIndex, delta, partial}
//	text_end/thinking_end                  {type, contentIndex, content, partial}
//	toolcall_end                           {type, contentIndex, toolCall, partial}
//	done                                   {type, reason, message}
//	error                                  {type, reason, error}
//
// Fields a variant requires are always emitted (contentIndex:0 included);
// fields the variant lacks are never emitted (no spurious "reason":"" on
// start, no "contentIndex":0 on done, ...).
func (e AssistantMessageEvent) MarshalJSON() ([]byte, error) {
	switch e.Type {
	case EventStart:
		return json.Marshal(struct {
			Type    EventType         `json:"type"`
			Partial *AssistantMessage `json:"partial"`
		}{e.Type, e.Partial})
	case EventTextStart, EventThinkingStart, EventToolCallStart:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.Partial})
	case EventTextDelta, EventThinkingDelta, EventToolCallDelta:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			Delta        string            `json:"delta"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.Delta, e.Partial})
	case EventTextEnd, EventThinkingEnd:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			Content      string            `json:"content"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.Content, e.Partial})
	case EventToolCallEnd:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			ToolCall     *ToolCall         `json:"toolCall"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.ToolCall, e.Partial})
	case EventDone:
		return json.Marshal(struct {
			Type    EventType         `json:"type"`
			Reason  StopReason        `json:"reason"`
			Message *AssistantMessage `json:"message"`
		}{e.Type, e.Reason, e.Message})
	case EventError:
		return json.Marshal(struct {
			Type   EventType         `json:"type"`
			Reason StopReason        `json:"reason"`
			Error  *AssistantMessage `json:"error"`
		}{e.Type, e.Reason, e.Error})
	default:
		// Unknown discriminator: fall back to the flat struct form.
		type alias AssistantMessageEvent
		return json.Marshal(alias(e))
	}
}

// AssistantMessageEventStream is an EventStream specialized for the assistant
// message protocol. The terminal event ("done" or "error") yields the final
// AssistantMessage.
type AssistantMessageEventStream = EventStream[AssistantMessageEvent, *AssistantMessage]

// NewAssistantMessageEventStream creates an AssistantMessageEventStream.
func NewAssistantMessageEventStream() *AssistantMessageEventStream {
	return NewEventStream(
		func(e AssistantMessageEvent) bool {
			return e.Type == EventDone || e.Type == EventError
		},
		func(e AssistantMessageEvent) *AssistantMessage {
			switch e.Type {
			case EventDone:
				return e.Message
			case EventError:
				return e.Error
			default:
				return nil
			}
		},
	)
}
