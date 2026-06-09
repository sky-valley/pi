package ai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateToolArguments validates (and coerces) tool-call arguments against the
// tool's schema, returning the validated arguments or an error formatted like
// pi's validateToolArguments.
func ValidateToolArguments(tool Tool, toolCall ToolCall) (map[string]any, error) {
	if tool.Parameters == nil {
		return toolCall.Arguments, nil
	}
	// Deep-copy the arguments so coercion does not mutate the original.
	args, _ := deepCopy(toolCall.Arguments).(map[string]any)
	if args == nil {
		args = map[string]any{}
	}

	coerced := tool.Parameters.Coerce(args)
	if obj, ok := coerced.(map[string]any); ok {
		args = obj
	}

	if errs := tool.Parameters.validate(args, ""); len(errs) > 0 {
		var lines []string
		for _, e := range errs {
			lines = append(lines, fmt.Sprintf("  - %s: %s", e.Path, e.Message))
		}
		received, _ := json.MarshalIndent(toolCall.Arguments, "", "  ")
		return nil, fmt.Errorf("Validation failed for tool %q:\n%s\n\nReceived arguments:\n%s",
			toolCall.Name, strings.Join(lines, "\n"), string(received))
	}
	return args, nil
}

// ValidateToolCall finds a tool by name and validates the call's arguments.
func ValidateToolCall(tools []Tool, toolCall ToolCall) (map[string]any, error) {
	for _, t := range tools {
		if t.Name == toolCall.Name {
			return ValidateToolArguments(t, toolCall)
		}
	}
	return nil, fmt.Errorf("Tool %q not found", toolCall.Name)
}
