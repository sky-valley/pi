package providers

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// Port of pi's packages/ai/src/api/constrained-sampling.ts. Every error message
// below reaches the user verbatim, so the literals are byte-for-byte pi's.

// unsupportedStrictJSONSchemaError marks a schema the strict subset cannot
// express (port of UnsupportedStrictJsonSchemaError). resolve falls back to
// unconstrained sampling on it unless the tool requires strict sampling.
type unsupportedStrictJSONSchemaError struct{ reason string }

func (e *unsupportedStrictJSONSchemaError) Error() string { return e.reason }

// unsupportedStrictSchemaKeys are the JSON Schema keywords the strict subset
// rejects, in pi's declaration order (the first present key names the error).
// allOf and oneOf are modeled Schema fields; the rest live in Schema.Extra.
var unsupportedStrictSchemaKeys = []string{
	"$ref",
	"$defs",
	"definitions",
	"allOf",
	"oneOf",
	"patternProperties",
	"dependentSchemas",
	"dependencies",
	"unevaluatedProperties",
	"propertyNames",
	"contains",
	"prefixItems",
	"not",
	"if",
	"then",
	"else",
}

// isStructuredSchema reports whether a schema describes an object or array
// (port of isStructuredSchema). pi reads the serialized JSON type, where a
// nullable object is ["object","null"] — its includes("object") still matches,
// so Nullable does not exclude a schema here.
func isStructuredSchema(schema *ai.Schema) bool {
	if schema == nil {
		return false
	}
	_, otherItems := schema.Extra["items"] // items that is not one schema
	return schema.Type == "object" || schema.Type == "array" ||
		schema.Properties != nil || schema.Items != nil || otherItems
}

// schemaAllowsNull reports whether a schema accepts null (port of
// schemaAllowsNull): a null type, const null, an enum containing null, or an
// anyOf variant that allows null.
func schemaAllowsNull(schema *ai.Schema) bool {
	if schema == nil {
		return false
	}
	if schema.Type == "null" || schema.Nullable {
		return true
	}
	if schema.HasConst && schema.Const == nil {
		return true
	}
	for _, v := range schema.Enum {
		if v == nil {
			return true
		}
	}
	for _, variant := range schema.AnyOf {
		if schemaAllowsNull(variant) {
			return true
		}
	}
	return false
}

// unsupportedStrictSchemaKeywordCheck reports whether a provider's strict mode
// rejects this schema keyword with this value (port of
// UnsupportedStrictSchemaKeywordCheck). A nil check accepts every keyword.
type unsupportedStrictSchemaKeywordCheck func(key string, value any) bool

// makeJSONSchemaNodeStrict rewrites one schema node (in place) into the strict
// subset, or reports why it cannot (port of makeJsonSchemaNodeStrict). Object
// schemas end up closed (additionalProperties:false) with every property
// required; a formerly-optional property that does not accept null is widened
// to {anyOf:[property,{type:"null"}]} so the model can still omit it by
// sampling null.
func makeJSONSchemaNodeStrict(schema *ai.Schema, isUnsupportedKeyword unsupportedStrictSchemaKeywordCheck) error {
	if schema == nil {
		// pi hits this for boolean (and null) schema nodes; ai.Schema can only
		// represent those as nil pointers.
		return &unsupportedStrictJSONSchemaError{"boolean schemas are unsupported"}
	}
	for _, key := range unsupportedStrictSchemaKeys {
		var present bool
		switch key {
		case "allOf":
			present = schema.AllOf != nil
		case "oneOf":
			present = schema.OneOf != nil
		default:
			_, present = schema.Extra[key]
		}
		if present {
			return &unsupportedStrictJSONSchemaError{key + " schemas are unsupported"}
		}
	}
	if isUnsupportedKeyword != nil {
		// Keywords walks the node in the order it serializes — the source's
		// for a decoded schema, TypeBox's for a built one — as pi walks
		// Object.entries, so the first offending keyword names the error.
		for key, value := range schema.Keywords() {
			if isUnsupportedKeyword(key, value) {
				rendered, _ := jstext.Stringify(value)
				return &unsupportedStrictJSONSchemaError{key + ": " + rendered + " is unsupported"}
			}
		}
	}

	if schema.AnyOf != nil {
		if len(schema.AnyOf) == 0 {
			return &unsupportedStrictJSONSchemaError{"anyOf must contain at least one schema"}
		}
		for _, variant := range schema.AnyOf {
			if isStructuredSchema(variant) {
				return &unsupportedStrictJSONSchemaError{"object and array unions are unsupported"}
			}
			if err := makeJSONSchemaNodeStrict(variant, isUnsupportedKeyword); err != nil {
				return err
			}
		}
	}

	if schema.Items != nil {
		if err := makeJSONSchemaNodeStrict(schema.Items, isUnsupportedKeyword); err != nil {
			return err
		}
	} else if items, ok := schema.Extra["items"]; ok {
		// ai.Schema keeps an items value that is not one schema in Extra. pi
		// refuses the tuple form, and recursing into anything else that is
		// not an object hits its boolean-schema refusal.
		if _, tuple := items.([]any); tuple {
			return &unsupportedStrictJSONSchemaError{"tuple schemas are unsupported"}
		}
		return &unsupportedStrictJSONSchemaError{"boolean schemas are unsupported"}
	}

	// pi compares the serialized type, so a nullable object (["object","null"])
	// is NOT an object schema here.
	isObjectSchema := schema.Type == "object" && !schema.Nullable
	if schema.Properties != nil && !isObjectSchema {
		return &unsupportedStrictJSONSchemaError{"properties require type object"}
	}
	if !isObjectSchema {
		return nil
	}
	if schema.AdditionalSchema != nil || (schema.AdditionalAllowed != nil && *schema.AdditionalAllowed) {
		return &unsupportedStrictJSONSchemaError{"schema-valued or true additionalProperties is unsupported"}
	}
	// pi's "object properties must be a schema map" / "object required must be
	// a string array" shape checks have no Go equivalents: Properties and
	// Required are typed.

	propertyNames := schema.OrderedProperties()
	required := make(map[string]bool, len(schema.Required))
	for _, key := range schema.Required {
		if _, known := schema.Properties[key]; !known {
			return &unsupportedStrictJSONSchemaError{"required contains an unknown property"}
		}
		required[key] = true
	}
	for _, key := range propertyNames {
		property := schema.Properties[key]
		if err := makeJSONSchemaNodeStrict(property, isUnsupportedKeyword); err != nil {
			return err
		}
		if !required[key] && !schemaAllowsNull(property) {
			schema.Properties[key] = &ai.Schema{AnyOf: []*ai.Schema{property, {Type: "null"}}}
		}
	}
	schema.Required = propertyNames
	closed := false
	schema.AdditionalAllowed = &closed
	return nil
}

// makeStrictJSONSchema converts a tool schema to the strict subset expected by
// provider constrained sampling (port of makeStrictJsonSchema). The input is
// deep-copied first — like pi's structuredClone, the tool's own schema is
// never touched.
func makeStrictJSONSchema(schema *ai.Schema, isUnsupportedKeyword unsupportedStrictSchemaKeywordCheck) (*ai.Schema, error) {
	cloned := schema.Clone()
	if cloned == nil {
		return nil, &unsupportedStrictJSONSchemaError{"root schema must have type object"}
	}
	if err := makeJSONSchemaNodeStrict(cloned, isUnsupportedKeyword); err != nil {
		return nil, err
	}
	if cloned.Type != "object" || cloned.Nullable {
		return nil, &unsupportedStrictJSONSchemaError{"root schema must have type object"}
	}
	return cloned, nil
}

// prefersJSONSchemaStrictSampling returns true for a tool that asks for strict
// JSON-schema sampling only when possible (`strict: "prefer"`). Port of
// prefersJsonSchemaStrictSampling (upstream 842c73170). Go's zero strictness
// is the documented default, prefer (ai.ConstrainedSamplingConfig.Strict), so
// it counts too.
func prefersJSONSchemaStrictSampling(tool ai.Tool) bool {
	config := tool.ConstrainedSampling
	return config != nil && config.Type == ai.ConstrainedSamplingJSONSchema && config.Strict != ai.ConstrainedSamplingRequire
}

// strictToolLimitErrorMessages: Claude rejects (on the Anthropic API and on
// Bedrock) a request whose strict tools exceed its limits with one of these
// messages. Observed limits: 20 strict tools, and 16 optional or union-typed
// parameters across all strict tools. Port of STRICT_TOOL_LIMIT_ERROR_MESSAGES.
var strictToolLimitErrorMessages = []string{
	"The compiled grammar is too large",
	"Too many strict tools (",
	"Schemas contains too many parameters with union types (",
}

// isStrictToolLimitErrorMessage returns true when a provider error message
// says the request's strict tools exceed the provider's limits. Port of
// isStrictToolLimitErrorMessage.
func isStrictToolLimitErrorMessage(message string) bool {
	return slices.ContainsFunc(strictToolLimitErrorMessages, func(limitMessage string) bool {
		return strings.Contains(message, limitMessage)
	})
}

// jsonSchemaToolParameters returns the schema a provider should send for a
// tool: the strict conversion when strict sampling resolved true, the tool's
// own parameters otherwise (port of getJsonSchemaToolParameters).
func jsonSchemaToolParameters(tool ai.Tool, strict bool) (*ai.Schema, error) {
	if strict {
		return makeStrictJSONSchema(tool.Parameters, nil)
	}
	return tool.Parameters, nil
}

// grammarSampling is a tool's resolved grammar-constrained-sampling config.
type grammarSampling struct {
	// format is the OpenAI grammar syntax: "lark" or "regex".
	format     string
	definition string
	// inputProperty is the single required string property of the tool schema
	// that the raw grammar output is stored under.
	inputProperty string
}

// grammarInputBuffer re-synthesizes `{"prop":"..."}`-shaped JSON deltas from the
// raw custom-tool input chunks a provider streams. It merges pi's
// GrammarToolInputJsonBuffer with the invariant `inputProperty` that pi passes
// alongside it on every call.
type grammarInputBuffer struct {
	property string
	input    strings.Builder
	started  bool
	closed   bool
}

func newGrammarInputBuffer(property string) *grammarInputBuffer {
	return &grammarInputBuffer{property: property}
}

// Input is the raw input streamed so far.
func (b *grammarInputBuffer) Input() string { return b.input.String() }

// appendDelta is pi's appendGrammarToolInputJsonDelta: it appends inputDelta
// to the input and returns the matching piece of the JSON arguments text, ok
// false for an empty delta (pi's undefined). It costs time linear in the
// delta, never in the input so far.
func (b *grammarInputBuffer) appendDelta(inputDelta string) (delta string, ok bool, err error) {
	if b.closed {
		return "", false, fmt.Errorf("grammar tool input for property %s changed after it was closed", quoteRaw(b.property))
	}
	if inputDelta == "" {
		return "", false, nil
	}
	b.input.WriteString(inputDelta)
	return b.jsonStart() + jsEscape(inputDelta), true, nil
}

// close is pi's closeGrammarToolInputJson: it closes the input and returns
// the rest of its JSON arguments text. finalInput is the complete input when
// the provider sends one (hasFinal); it must continue the input streamed so
// far. Closing again with the same input, or with none, returns ok false.
func (b *grammarInputBuffer) close(finalInput string, hasFinal bool) (delta string, ok bool, err error) {
	if b.closed {
		if !hasFinal || finalInput == b.input.String() {
			return "", false, nil
		}
		return "", false, fmt.Errorf("grammar tool input for property %s changed after it was closed", quoteRaw(b.property))
	}
	input := b.input.String()
	if hasFinal && !strings.HasPrefix(finalInput, input) {
		return "", false, fmt.Errorf("grammar tool input for property %s changed non-monotonically", quoteRaw(b.property))
	}
	rest := ""
	if hasFinal {
		rest = finalInput[len(input):]
		b.input.WriteString(rest)
	}
	b.closed = true
	return b.jsonStart() + jsEscape(rest) + `"}`, true, nil
}

// closeGrammarInput is close with the final input as an event holds it, a
// JSON value or undefined: a string is the final input and undefined none,
// while any other value throws V8's TypeError where pi calls startsWith on it
// — unless the buffer is closed already, which it is not equal to.
func closeGrammarInput(b *grammarInputBuffer, input any) (delta string, ok bool, err error) {
	switch v := input.(type) {
	case string:
		return b.close(v, true)
	case jsUndefinedValue:
		return b.close("", false)
	}
	switch {
	case b.closed:
		return "", false, fmt.Errorf("grammar tool input for property %s changed after it was closed", quoteRaw(b.property))
	case input == nil:
		return "", false, errors.New("Cannot read properties of null (reading 'startsWith')")
	default:
		return "", false, errors.New("finalInput.startsWith is not a function")
	}
}

// jsonStart is the opening of the JSON arguments text, once.
func (b *grammarInputBuffer) jsonStart() string {
	if b.started {
		return ""
	}
	b.started = true
	return "{" + jsQuote(b.property) + `:"`
}

// grammarToolInput extracts the raw grammar input from a replayed tool call's
// arguments (port of getGrammarToolInput).
func grammarToolInput(toolName string, arguments map[string]any, inputProperty string) (string, error) {
	input, ok := arguments[inputProperty].(string)
	if !ok {
		return "", fmt.Errorf("Grammar tool call %s requires argument %s to be a string.",
			quoteRaw(toolName), quoteRaw(inputProperty))
	}
	return input, nil
}

// inferGrammarInputProperty finds the single required string property that a
// grammar tool's raw output is carried in (port of inferGrammarInputProperty).
func inferGrammarInputProperty(tool ai.Tool) (string, error) {
	schema := tool.Parameters
	// pi reads the serialized JSON Schema, where a nullable type is an array and
	// therefore never equal to "object"/"string".
	if schema == nil || schema.Type != "object" || schema.Nullable {
		return "", errors.New("grammar constrained sampling requires an object parameter schema")
	}
	if len(schema.Required) != 1 {
		return "", errors.New("grammar constrained sampling requires exactly one required string property")
	}

	inputProperty := schema.Required[0]
	prop := schema.Properties[inputProperty]
	if prop == nil {
		return "", fmt.Errorf("grammar constrained sampling requires a properties entry for %s", inputProperty)
	}
	if prop.Type != "string" || prop.Nullable {
		return "", fmt.Errorf("grammar constrained sampling property %s must have type string", inputProperty)
	}
	return inputProperty, nil
}

// resolveJSONSchemaStrictSampling reports whether a tool should be sent with
// JSON-schema constrained sampling enabled (port of resolveJsonSchemaStrictSampling).
// pi returns true or undefined, never false, so a plain bool suffices.
// isUnsupportedKeyword lets a provider reject extra keywords its strict mode
// does not accept, so "prefer" tools fall back to non-strict.
func resolveJSONSchemaStrictSampling(tool ai.Tool, supportsStrictMode bool, isUnsupportedKeyword unsupportedStrictSchemaKeywordCheck) (bool, error) {
	config := tool.ConstrainedSampling
	if config == nil || config.Type != ai.ConstrainedSamplingJSONSchema {
		return false, nil
	}
	if supportsStrictMode {
		// Probe the conversion: a schema the strict subset cannot express falls
		// back to unconstrained sampling unless the tool requires strict.
		_, err := makeStrictJSONSchema(tool.Parameters, isUnsupportedKeyword)
		if err == nil {
			return true, nil
		}
		var unsupported *unsupportedStrictJSONSchemaError
		if !errors.As(err, &unsupported) {
			return false, err
		}
		if config.Strict != ai.ConstrainedSamplingRequire {
			return false, nil
		}
		return false, fmt.Errorf("Tool %s requires JSON-schema constrained sampling, but %s.",
			quoteRaw(tool.Name), unsupported.reason)
	}
	if config.Strict == ai.ConstrainedSamplingRequire {
		return false, fmt.Errorf("Tool %s requires JSON-schema constrained sampling, but strict tools are unsupported.",
			quoteRaw(tool.Name))
	}
	return false, nil
}

// resolveGrammarSampling resolves a tool's grammar-constrained-sampling config,
// returning nil when the tool has none or the provider cannot honor it
// (port of resolveGrammarConstrainedSampling).
func resolveGrammarSampling(tool ai.Tool, supportsOpenAIGrammarTools bool) (*grammarSampling, error) {
	config := tool.ConstrainedSampling
	if config == nil || config.Type != ai.ConstrainedSamplingGrammar {
		return nil, nil
	}
	if !supportsOpenAIGrammarTools {
		return nil, nil
	}

	lark := jstext.Trim(config.Variants.OpenAILark)
	regex := jstext.Trim(config.Variants.OpenAIRegex)
	if lark == "" && regex == "" {
		return nil, fmt.Errorf("Tool %s cannot use grammar constrained sampling: no supported grammar variant was provided.",
			quoteRaw(tool.Name))
	}

	// pi keeps the UNTRIMMED definition; the trim only decides which variant wins.
	format, definition := "lark", config.Variants.OpenAILark
	if lark == "" {
		format, definition = "regex", config.Variants.OpenAIRegex
	}
	inputProperty, err := inferGrammarInputProperty(tool)
	if err != nil {
		return nil, fmt.Errorf("Tool %s cannot use grammar constrained sampling: %w.", quoteRaw(tool.Name), err)
	}
	return &grammarSampling{format: format, definition: definition, inputProperty: inputProperty}, nil
}

// grammarToolInputProperties maps each grammar tool's name to the schema
// property its raw output is carried in (port of createGrammarToolInputProperties).
func grammarToolInputProperties(tools []ai.Tool, supportsOpenAIGrammarTools bool) (map[string]string, error) {
	var properties map[string]string
	for _, tool := range tools {
		grammar, err := resolveGrammarSampling(tool, supportsOpenAIGrammarTools)
		if err != nil {
			return nil, err
		}
		if grammar == nil {
			continue
		}
		if properties == nil {
			properties = map[string]string{}
		}
		properties[tool.Name] = grammar.inputProperty
	}
	return properties, nil
}

// quoteRaw wraps s in double quotes without escaping, matching a JS template
// literal `"${s}"` (Go's %q would escape the interpolated value).
func quoteRaw(s string) string { return `"` + s + `"` }
