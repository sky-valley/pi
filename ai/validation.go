package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"weak"

	"github.com/sky-valley/pi/internal/jstext"
	"github.com/sky-valley/pi/schema"
	"github.com/sky-valley/pi/schema/formats"
)

// validators holds one compiled validator per tool schema (pi validation.ts's
// validatorCache WeakMap): tool schemas are long-lived, and an entry goes with
// its schema once nothing else holds it.
var validators sync.Map // weak.Pointer[Schema] -> *schema.Validator

// validatorFor compiles s the way pi's getValidator does: the standard
// formats, coercion on, and a $ref that names nothing (a dangling definition
// in an MCP server's schema) rejecting only values at its place instead of
// failing every call of the tool.
func validatorFor(s *Schema) (*schema.Validator, error) {
	key := weak.Make(s)
	if v, ok := validators.Load(key); ok {
		return v.(*schema.Validator), nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	doc, err := schema.Decode(data)
	if err != nil {
		return nil, err
	}
	v, err := schema.Compile(doc, schema.Options{
		Formats:        formats.All(),
		Coerce:         true,
		UnresolvedRefs: schema.UnresolvedReject,
	})
	if err != nil {
		return nil, err
	}
	if actual, loaded := validators.LoadOrStore(key, v); loaded {
		return actual.(*schema.Validator), nil
	}
	runtime.AddCleanup(s, func(key weak.Pointer[Schema]) { validators.Delete(key) }, key)
	return v, nil
}

// ValidateToolArguments validates tool-call arguments against the tool's
// schema after coercing them (pi-schema's parse: "42" for a number, a single
// value for an array, null for an omitted optional property), returning the
// coerced arguments or an error formatted like pi's validateToolArguments. A
// call whose arguments text was malformed fails with its ArgumentsError
// before the arguments are looked at. The call's own arguments are never
// modified.
func ValidateToolArguments(tool Tool, toolCall ToolCall) (args map[string]any, err error) {
	if toolCall.ArgumentsError != "" {
		return nil, fmt.Errorf(`Invalid arguments for tool "%s": %s`, toolCall.Name, toolCall.ArgumentsError)
	}
	if tool.Parameters == nil {
		return toolCall.Arguments, nil
	}
	v, err := validatorFor(tool.Parameters)
	if err != nil {
		return nil, err
	}
	// Parse fails where pi's parse() throws (a malformed subschema compiled
	// on first use, a schema that applies itself to the arguments forever),
	// and so does validateToolArguments.
	coerced, errs, err := v.Parse(toSchemaValue(toolCall.OrderedArguments()))
	if err != nil {
		return nil, err
	}
	if errs == nil {
		obj, _ := fromSchemaValue(coerced).(map[string]any)
		return obj, nil
	}
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = fmt.Sprintf("  - %s: %s", validationPath(e.Path), e.Message)
	}
	return nil, fmt.Errorf("Validation failed for tool \"%s\":\n%s\n\nReceived arguments:\n%s",
		toolCall.Name, strings.Join(lines, "\n"), receivedArguments(toolCall))
}

// validationPath is a pi-schema error path (a JSON Pointer) as the dotted
// path the model sees, pi's formatValidationPath: "/edits/0/oldText" becomes
// "edits.0.oldText", and the value itself is "root".
func validationPath(pointer string) string {
	if pointer == "" {
		return "root"
	}
	segments := strings.Split(pointer[1:], "/")
	for i, s := range segments {
		segments[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return strings.Join(segments, ".")
}

// toSchemaValue is a decoded arguments value in package schema's value
// model: an OrderedObject becomes a schema.Object in the same order, at any
// depth. Copying is also what keeps coercion away from the call's arguments
// (pi's structuredClone).
func toSchemaValue(v any) any {
	switch x := v.(type) {
	case OrderedObject:
		obj := make(schema.Object, len(x))
		for i, f := range x {
			obj[i] = schema.Member{Key: f.Key, Value: toSchemaValue(f.Value)}
		}
		return obj
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = toSchemaValue(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toSchemaValue(e)
		}
		return out
	}
	return v
}

// fromSchemaValue is a coerced value back in encoding/json's shapes, the
// shape ValidateToolArguments has always returned.
func fromSchemaValue(v any) any {
	switch x := v.(type) {
	case schema.Object:
		m := make(map[string]any, len(x))
		for _, f := range x {
			m[f.Key] = fromSchemaValue(f.Value)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = fromSchemaValue(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = fromSchemaValue(e)
		}
		return out
	}
	return v
}

// ValidateToolCall finds a tool by name and validates the call's arguments.
func ValidateToolCall(tools []Tool, toolCall ToolCall) (map[string]any, error) {
	for _, t := range tools {
		if t.Name == toolCall.Name {
			return ValidateToolArguments(t, toolCall)
		}
	}
	return nil, fmt.Errorf("Tool \"%s\" not found", toolCall.Name)
}

// receivedArguments writes a call's arguments as pi's
// JSON.stringify(toolCall.arguments, null, 2) does: the keys in the order the
// model wrote them (OrderedArguments), JavaScript's string escapes and
// numbers (jstext.Stringify), indented by two spaces.
func receivedArguments(toolCall ToolCall) string {
	compact, err := jstext.Stringify(toolCall.OrderedArguments())
	if err != nil {
		return fmt.Sprint(toolCall.Arguments)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(compact), "", "  "); err != nil {
		return compact
	}
	return indented.String()
}
