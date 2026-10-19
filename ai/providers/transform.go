package providers

import (
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

const (
	nonVisionUserImagePlaceholder = "(image omitted: model does not support images)"
	nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"
	errorToolImagePlaceholder     = "(image attached after the tool results)"
)

// splitErrorToolResult is pi's SplitErrorToolResult: an error tool result split
// into a text-only result and the images to send after the tool results.
type splitErrorToolResult struct {
	// content is the result's content with each run of images replaced by a
	// note.
	content ai.ContentList
	// caption names the tool call, to send before images.
	caption string
	images  []ai.ImageContent
}

// splitErrorToolResultImages is pi's splitErrorToolResultImages (upstream
// 5ab7fe74c), for APIs that reject images in error results (Anthropic, and
// Claude on Bedrock): send content as the tool result and the caption and
// images after all tool results of the turn, in the same user message. ok is
// false for a result that needs no split.
func splitErrorToolResultImages(msg ai.ToolResultMessage) (split splitErrorToolResult, ok bool) {
	if !msg.IsError {
		return split, false
	}
	for _, block := range msg.Content {
		if image, isImage := block.(ai.ImageContent); isImage {
			split.images = append(split.images, image)
		}
	}
	if len(split.images) == 0 {
		return split, false
	}
	split.content = replaceImagesWithPlaceholder(msg.Content, errorToolImagePlaceholder)
	split.caption = "Images from the error result of tool call " + msg.ToolCallID + " (" + msg.ToolName + "):"
	return split, true
}

// asAssistantMsg normalizes a value/pointer assistant message.
func asAssistantMsg(m ai.Message) (*ai.AssistantMessage, bool) {
	switch v := m.(type) {
	case *ai.AssistantMessage:
		return v, true
	case ai.AssistantMessage:
		return &v, true
	}
	return nil, false
}

// asSystemMsg normalizes a value/pointer system message.
func asSystemMsg(m ai.Message) (ai.SystemMessage, bool) {
	switch v := m.(type) {
	case ai.SystemMessage:
		return v, true
	case *ai.SystemMessage:
		if v != nil {
			return *v, true
		}
	}
	return ai.SystemMessage{}, false
}

func asUserMsg(m ai.Message) (ai.UserMessage, bool) {
	switch v := m.(type) {
	case ai.UserMessage:
		return v, true
	case *ai.UserMessage:
		return *v, true
	}
	return ai.UserMessage{}, false
}

func asToolResultMsg(m ai.Message) (ai.ToolResultMessage, bool) {
	switch v := m.(type) {
	case ai.ToolResultMessage:
		return v, true
	case *ai.ToolResultMessage:
		return *v, true
	}
	return ai.ToolResultMessage{}, false
}

func replaceImagesWithPlaceholder(content ai.ContentList, placeholder string) ai.ContentList {
	var result ai.ContentList
	previousWasPlaceholder := false
	for _, block := range content {
		if _, ok := block.(ai.ImageContent); ok {
			if !previousWasPlaceholder {
				result = append(result, ai.TextContent{Text: placeholder})
			}
			previousWasPlaceholder = true
			continue
		}
		result = append(result, block)
		if tc, ok := block.(ai.TextContent); ok {
			previousWasPlaceholder = tc.Text == placeholder
		} else {
			previousWasPlaceholder = false
		}
	}
	return result
}

func modelSupportsImages(model *ai.Model) bool {
	for _, i := range model.Input {
		if i == "image" {
			return true
		}
	}
	return false
}

func downgradeUnsupportedImages(messages []ai.Message, model *ai.Model) []ai.Message {
	if modelSupportsImages(model) {
		return messages
	}
	out := make([]ai.Message, len(messages))
	for i, m := range messages {
		if um, ok := asUserMsg(m); ok {
			um.Content = replaceImagesWithPlaceholder(um.Content, nonVisionUserImagePlaceholder)
			out[i] = um
			continue
		}
		if tr, ok := asToolResultMsg(m); ok {
			tr.Content = replaceImagesWithPlaceholder(tr.Content, nonVisionToolImagePlaceholder)
			out[i] = tr
			continue
		}
		out[i] = m
	}
	return out
}

// transformMessages normalizes messages for cross-provider compatibility:
// downgrades unsupported images, drops thinking blocks from other models,
// normalizes tool-call ids, drops errored/aborted assistant turns,
// inserts synthetic results for orphaned tool calls, and holds a system message
// that lands between a tool call and its results until they close.
func transformMessages(messages []ai.Message, model *ai.Model, normalizeToolCallID func(id string) string) []ai.Message {
	toolCallIDMap := map[string]string{}
	imageAware := downgradeUnsupportedImages(messages, model)

	transformed := make([]ai.Message, 0, len(imageAware))
	for _, m := range imageAware {
		// System and user messages pass through unchanged.
		if sm, ok := asSystemMsg(m); ok {
			transformed = append(transformed, sm)
			continue
		}
		if um, ok := asUserMsg(m); ok {
			transformed = append(transformed, um)
			continue
		}
		if tr, ok := asToolResultMsg(m); ok {
			if normID, has := toolCallIDMap[tr.ToolCallID]; has && normID != tr.ToolCallID {
				tr.ToolCallID = normID
			}
			transformed = append(transformed, tr)
			continue
		}
		am, ok := asAssistantMsg(m)
		if !ok {
			transformed = append(transformed, m)
			continue
		}
		isSameModel := am.Provider == model.Provider && am.Api == model.Api && am.Model == model.ID
		var content ai.ContentList
		for _, block := range am.Content {
			switch b := block.(type) {
			case ai.ThinkingContent:
				// Thinking from another model is dropped (upstream 911309429).
				// Signatures and encrypted reasoning are only valid for the model
				// that produced them, and sending readable thinking as plain text
				// makes the new model treat it as part of the visible reply.
				if !isSameModel {
					continue
				}
				// Keep redacted and signed blocks even if the thinking text is
				// empty (OpenAI encrypted reasoning); they are needed for replay.
				if b.Redacted || b.ThinkingSignature != "" || jstext.Trim(b.Thinking) != "" {
					content = append(content, b)
				}
			case ai.TextContent:
				if isSameModel {
					content = append(content, b)
				} else {
					content = append(content, ai.TextContent{Text: b.Text})
				}
			case ai.ToolCall:
				tc := b
				if !isSameModel && tc.ThoughtSignature != "" {
					tc.ThoughtSignature = ""
				}
				if !isSameModel && normalizeToolCallID != nil {
					normID := normalizeToolCallID(tc.ID)
					if normID != tc.ID {
						toolCallIDMap[tc.ID] = normID
						tc.ID = normID
					}
				}
				content = append(content, tc)
			default:
				content = append(content, block)
			}
		}
		copy := *am
		copy.Content = content
		transformed = append(transformed, &copy)
	}

	// Second pass: synthetic results for orphaned tool calls; drop bad assistants.
	var result []ai.Message
	var pendingToolCalls []ai.ToolCall
	existingResultIDs := map[string]bool{}
	// System messages are transparent to tool-call accounting: one that lands
	// between a tool call and its results is held back and emitted after the
	// results (synthetic ones included), so it never causes a duplicate result
	// for a call that is answered later.
	var heldSystemMessages []ai.Message
	closePendingToolCalls := func() {
		if len(pendingToolCalls) > 0 {
			for _, tc := range pendingToolCalls {
				if !existingResultIDs[tc.ID] {
					result = append(result, ai.ToolResultMessage{
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Content:    ai.ContentList{ai.TextContent{Text: "No result provided"}},
						IsError:    true,
						Timestamp:  nowMillis(),
					})
				}
			}
			pendingToolCalls = nil
			existingResultIDs = map[string]bool{}
		}
		result = append(result, heldSystemMessages...)
		heldSystemMessages = nil
	}

	for _, m := range transformed {
		if am, ok := asAssistantMsg(m); ok {
			closePendingToolCalls()
			if am.StopReason == ai.StopError || am.StopReason == ai.StopAborted {
				continue
			}
			var toolCalls []ai.ToolCall
			for _, c := range am.Content {
				if tc, ok := c.(ai.ToolCall); ok {
					toolCalls = append(toolCalls, tc)
				}
			}
			if len(toolCalls) > 0 {
				pendingToolCalls = toolCalls
				existingResultIDs = map[string]bool{}
			}
			result = append(result, m)
		} else if tr, ok := asToolResultMsg(m); ok {
			existingResultIDs[tr.ToolCallID] = true
			result = append(result, m)
		} else if _, ok := asSystemMsg(m); ok {
			if len(pendingToolCalls) > 0 {
				heldSystemMessages = append(heldSystemMessages, m)
			} else {
				result = append(result, m)
			}
		} else if _, ok := asUserMsg(m); ok {
			// A new user turn interrupts tool flow - insert synthetic results for
			// orphaned calls.
			closePendingToolCalls()
			result = append(result, m)
		} else {
			result = append(result, m)
		}
	}
	// If the conversation ends with unresolved tool calls, synthesize results now.
	closePendingToolCalls()
	return result
}
