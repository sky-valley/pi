package providers

import (
	"strings"

	"github.com/sky-valley/pi/ai"
)

const (
	nonVisionUserImagePlaceholder = "(image omitted: model does not support images)"
	nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"
)

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
// downgrades unsupported images, rewrites thinking blocks for cross-model
// replay, normalizes tool-call ids, drops errored/aborted assistant turns, and
// inserts synthetic results for orphaned tool calls.
func transformMessages(messages []ai.Message, model *ai.Model, normalizeToolCallID func(id string) string) []ai.Message {
	toolCallIDMap := map[string]string{}
	imageAware := downgradeUnsupportedImages(messages, model)

	transformed := make([]ai.Message, 0, len(imageAware))
	for _, m := range imageAware {
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
				if b.Redacted {
					if isSameModel {
						content = append(content, b)
					}
					continue
				}
				if isSameModel && b.ThinkingSignature != "" {
					content = append(content, b)
					continue
				}
				if strings.TrimSpace(b.Thinking) == "" {
					continue
				}
				if isSameModel {
					content = append(content, b)
				} else {
					content = append(content, ai.TextContent{Text: b.Thinking})
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
	insertSynthetic := func() {
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
	}

	for _, m := range transformed {
		if am, ok := asAssistantMsg(m); ok {
			insertSynthetic()
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
		} else if _, ok := asUserMsg(m); ok {
			insertSynthetic()
			result = append(result, m)
		} else {
			result = append(result, m)
		}
	}
	insertSynthetic()
	return result
}
