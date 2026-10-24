package providers

import (
	"encoding/json"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// googleSchemaJSON converts one tool whose parameters are raw and returns its
// parametersJsonSchema as the bytes the request body carries.
func googleSchemaJSON(t *testing.T, raw string) string {
	t.Helper()
	var schema ai.Schema
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	before, _ := json.Marshal(&schema)
	tools, err := googleTools([]ai.Tool{{Name: "t", Description: "d", Parameters: &schema}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	decl := tools[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	got, err := json.Marshal(decl["parametersJsonSchema"])
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := json.Marshal(&schema); string(after) != string(before) {
		t.Fatalf("the tool's schema was mutated:\n got %s\nwant %s", after, before)
	}
	return string(got)
}

// Gemini ignores `const` in parametersJsonSchema but honors `enum`, so a
// string or number const is sent as a one-value enum (upstream 7314819f0),
// rewriting only subschemas, never literal data or property names
// (upstream a7b94a03f, #11308). Cases from google-shared-convert-tools.test.ts.
func TestGoogleToolsRewriteConstAsEnum(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"string and number consts become enums",
			`{"type":"object","properties":{"origin":{"type":"string","const":"zq-src-7"},"mode":{"anyOf":[{"type":"string","const":"a"},{"type":"string","const":"b"}]},"level":{"oneOf":[{"const":1},{"const":2}]},"flag":{"type":"boolean","const":true},"list":{"type":"array","items":{"const":"x"}},"const":{"type":"string"}},"required":["origin"]}`,
			`{"type":"object","properties":{"origin":{"type":"string","enum":["zq-src-7"]},"mode":{"anyOf":[{"type":"string","enum":["a"]},{"type":"string","enum":["b"]}]},"level":{"oneOf":[{"enum":[1]},{"enum":[2]}]},"flag":{"type":"boolean","const":true},"list":{"type":"array","items":{"enum":["x"]}},"const":{"type":"string"}},"required":["origin"]}`,
		},
		{
			"only schema-bearing keywords are rewritten",
			`{"type":"object","additionalProperties":{"const":"x"},"unevaluatedProperties":false,"propertyNames":{"const":"name"},"allOf":[{"if":{"const":"x"},"then":{"const":"y"},"else":{"not":{"const":"z"}}}],"properties":{"list":{"type":"array","items":[{"const":1}],"prefixItems":[{"const":2}],"additionalItems":{"const":3},"contains":{"const":4},"unevaluatedItems":{"const":5}}},"dependencies":{"list":["other"]}}`,
			`{"type":"object","additionalProperties":{"enum":["x"]},"unevaluatedProperties":false,"propertyNames":{"enum":["name"]},"allOf":[{"if":{"enum":["x"]},"then":{"enum":["y"]},"else":{"not":{"enum":["z"]}}}],"properties":{"list":{"type":"array","items":[{"enum":[1]}],"prefixItems":[{"enum":[2]}],"additionalItems":{"enum":[3]},"contains":{"enum":[4]},"unevaluatedItems":{"enum":[5]}}},"dependencies":{"list":["other"]}}`,
		},
		{
			"an existing enum wins over const",
			`{"type":"object","properties":{"v":{"const":"a","enum":["a","b"]}}}`,
			`{"type":"object","properties":{"v":{"const":"a","enum":["a","b"]}}}`,
		},
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"} {
		cases = append(cases, struct{ name, in, want string }{
			"names under " + key + " are kept",
			`{"type":"object","` + key + `":{"__proto__":{"const":"x"},"const":{"const":"y"},"properties":{"const":"z"}}}`,
			`{"type":"object","` + key + `":{"__proto__":{"enum":["x"]},"const":{"enum":["y"]},"properties":{"enum":["z"]}}}`,
		})
	}
	for _, key := range []string{"default", "examples", "enum", "const", "x-custom"} {
		literal := `{"const":"literal-data","properties":{"nested":{"const":1}}}`
		if key == "enum" || key == "examples" {
			literal = "[" + literal + "]"
		}
		in := `{"type":"object","properties":{"value":{"type":"object","` + key + `":` + literal + `}}}`
		cases = append(cases, struct{ name, in, want string }{"literal data under " + key + " is kept", in, in})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := googleSchemaJSON(t, tc.in); got != tc.want {
				t.Fatalf("parametersJsonSchema\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// The legacy `parameters` field (Cloud Code Assist with Claude) is untouched.
func TestGoogleToolsKeepConstInLegacyParameters(t *testing.T) {
	var schema ai.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"v":{"type":"string","const":"a"}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	tools, err := googleTools([]ai.Tool{{Name: "t", Description: "d", Parameters: &schema}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	decl := tools[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	got, _ := json.Marshal(decl["parameters"])
	if want := `{"properties":{"v":{"const":"a","type":"string"}},"type":"object"}`; string(got) != want {
		t.Fatalf("parameters = %s, want %s", got, want)
	}
}
