package providers

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Ground truth for this file comes from pi 0.82.0
// (dist/api/constrained-sampling.js, run directly under node). Every expected
// string below is a verbatim capture of that run.

func grammarParams() *ai.Schema {
	return ai.Object(ai.Prop("query", ai.String()))
}

func jsonSchemaTool(name string, strict ai.ConstrainedSamplingStrictness) ai.Tool {
	return ai.Tool{
		Name: name, Description: "d", Parameters: grammarParams(),
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{
			Type: ai.ConstrainedSamplingJSONSchema, Strict: strict,
		},
	}
}

func grammarTool(name string, variants ai.GrammarVariants) ai.Tool {
	return ai.Tool{
		Name: name, Description: "d", Parameters: grammarParams(),
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{
			Type: ai.ConstrainedSamplingGrammar, Variants: variants,
		},
	}
}

func TestResolveJSONSchemaStrictSampling(t *testing.T) {
	plain := ai.Tool{Name: "plain", Description: "d", Parameters: grammarParams()}
	for _, tc := range []struct {
		name           string
		tool           ai.Tool
		supportsStrict bool
		want           bool
		wantErr        string
	}{
		{"no config", plain, true, false, ""},
		{"disabled config", ai.Tool{Name: "x", Parameters: grammarParams(), ConstrainedSampling: &ai.ConstrainedSamplingConfig{}}, true, false, ""},
		{"grammar config is not json_schema", grammarTool("g", ai.GrammarVariants{OpenAILark: "s"}), true, false, ""},
		{"prefer, supported", jsonSchemaTool("js_prefer", ai.ConstrainedSamplingPrefer), true, true, ""},
		{"prefer, unsupported", jsonSchemaTool("js_prefer", ai.ConstrainedSamplingPrefer), false, false, ""},
		{"require, supported", jsonSchemaTool("js_require", ai.ConstrainedSamplingRequire), true, true, ""},
		{
			"require, unsupported", jsonSchemaTool("js_require", ai.ConstrainedSamplingRequire), false, false,
			`Tool "js_require" requires JSON-schema constrained sampling, but strict tools are unsupported.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveJSONSchemaStrictSampling(tc.tool, tc.supportsStrict, nil)
			assertErrString(t, err, tc.wantErr)
			if err == nil && got != tc.want {
				t.Fatalf("strict = %v, want %v", got, tc.want)
			}
		})
	}
}

// Port of "derives strict provider schemas without changing tool definitions"
// (constrained-sampling.test.ts, upstream 7915cdac6): the strict conversion
// closes objects, requires every property in original property order, widens
// non-nullable optionals to anyOf-with-null, and never mutates its input.
func TestMakeStrictJSONSchema(t *testing.T) {
	parameters := ai.Object(
		ai.Prop("path", ai.String()),
		ai.Opt("offset", ai.Number()),
		ai.Prop("metadata", ai.Object(ai.Opt("enabled", ai.Boolean()))),
		ai.Opt("nullable", &ai.Schema{AnyOf: []*ai.Schema{{Type: "string"}, {Type: "null"}}}),
	)
	before, err := json.Marshal(parameters)
	if err != nil {
		t.Fatal(err)
	}

	strict, err := makeStrictJSONSchema(parameters, nil)
	if err != nil {
		t.Fatalf("makeStrictJSONSchema: %v", err)
	}

	if after, _ := json.Marshal(parameters); string(after) != string(before) {
		t.Fatalf("tool definition changed:\n got: %s\nwant: %s", after, before)
	}
	got, err := json.Marshal(strict)
	if err != nil {
		t.Fatal(err)
	}
	// pi-ai 0.99.2's makeStrictJsonSchema over the same TypeBox schema, byte
	// for byte: the root keeps TypeBox's {type, required, properties} order,
	// and the nested object, which had no required key, gains it after
	// properties, as a new JS key lands.
	want := `{"type":"object","required":["path","offset","metadata","nullable"],"properties":{` +
		`"path":{"type":"string"},` +
		`"offset":{"anyOf":[{"type":"number"},{"type":"null"}]},` +
		`"metadata":{"type":"object","properties":{"enabled":{"anyOf":[{"type":"boolean"},{"type":"null"}]}},"required":["enabled"],"additionalProperties":false},` +
		`"nullable":{"anyOf":[{"type":"string"},{"type":"null"}]}` +
		`},"additionalProperties":false}`
	if string(got) != want {
		t.Fatalf("strict schema mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// Zero-property objects: pi's makeJsonSchemaNodeStrict assigns
// required = Object.keys(properties) unconditionally, so a strict-converted
// empty object carries required: [] on the wire — and a source schema that
// never had a `properties` key must not grow one (upstream reads
// `schema.properties ?? {}` without assigning back).
func TestMakeStrictJSONSchemaEmptyObject(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "empty properties",
			src:  `{"type":"object","properties":{}}`,
			want: `{"type":"object","properties":{},"required":[],"additionalProperties":false}`,
		},
		{
			name: "no properties key",
			src:  `{"type":"object"}`,
			want: `{"type":"object","required":[],"additionalProperties":false}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var parameters ai.Schema
			if err := json.Unmarshal([]byte(tt.src), &parameters); err != nil {
				t.Fatal(err)
			}
			strict, err := makeStrictJSONSchema(&parameters, nil)
			if err != nil {
				t.Fatalf("makeStrictJSONSchema: %v", err)
			}
			got, err := json.Marshal(strict)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("strict schema mismatch:\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// Port of "falls back or rejects schemas that cannot be safely converted"
// (constrained-sampling.test.ts, upstream 7915cdac6): an inconvertible schema
// makes strict:"prefer" fall back to unconstrained sampling (strict:false with
// the original parameters on the wire) and makes strict:"require" fail the
// request. Error strings are byte-for-byte pi's.
func TestMakeStrictJSONSchemaUnsupported(t *testing.T) {
	openMetadata := ai.Object()
	openMetadata.AdditionalSchema = ai.String()
	var refSchema ai.Schema
	if err := json.Unmarshal([]byte(
		`{"type":"object","properties":{"child":{"$ref":"https://example.com/child.json"}},"required":["child"]}`,
	), &refSchema); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		parameters *ai.Schema
		reason     string
	}{
		{
			"schema-valued additionalProperties",
			ai.Object(ai.Prop("metadata", openMetadata)),
			"schema-valued or true additionalProperties is unsupported",
		},
		{
			"intersection",
			&ai.Schema{AllOf: []*ai.Schema{
				ai.Object(ai.Prop("a", ai.String())),
				ai.Object(ai.Prop("b", ai.Number())),
			}},
			"allOf schemas are unsupported",
		},
		{
			"object union",
			ai.Object(ai.Prop("value", &ai.Schema{AnyOf: []*ai.Schema{
				ai.Object(ai.Prop("nested", ai.String())),
				{Type: "null"},
			}})),
			"object and array unions are unsupported",
		},
		{
			"$ref property",
			&refSchema,
			"$ref schemas are unsupported",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := ai.Tool{
				Name: "cannot", Description: "d", Parameters: tc.parameters,
				ConstrainedSampling: &ai.ConstrainedSamplingConfig{
					Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingPrefer,
				},
			}

			if _, err := makeStrictJSONSchema(tc.parameters, nil); err == nil || err.Error() != tc.reason {
				t.Fatalf("makeStrictJSONSchema error = %v, want %q", err, tc.reason)
			}
			strict, err := resolveJSONSchemaStrictSampling(tool, true, nil)
			if err != nil || strict {
				t.Fatalf("prefer must fall back to unconstrained: strict=%v err=%v", strict, err)
			}

			converted, err := convertResponsesTools([]ai.Tool{tool}, responsesCompat{SupportsStrictMode: true}, false)
			if err != nil {
				t.Fatalf("convertResponsesTools: %v", err)
			}
			if converted[0]["strict"] != false {
				t.Fatalf("fallback tool must carry strict:false: %#v", converted[0])
			}
			raw, _ := json.Marshal(tc.parameters)
			var original any
			_ = json.Unmarshal(raw, &original)
			if !reflect.DeepEqual(converted[0]["parameters"], original) {
				t.Fatalf("fallback tool must keep its original parameters:\n got: %#v\nwant: %#v",
					converted[0]["parameters"], original)
			}

			tool.ConstrainedSampling = &ai.ConstrainedSamplingConfig{
				Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingRequire,
			}
			_, err = resolveJSONSchemaStrictSampling(tool, true, nil)
			assertErrString(t, err, `Tool "cannot" requires JSON-schema constrained sampling, but `+tc.reason+`.`)
		})
	}
}

func TestResolveGrammarSampling(t *testing.T) {
	twoRequired := grammarTool("gram_bad", ai.GrammarVariants{OpenAILark: "s"})
	twoRequired.Parameters = ai.Object(ai.Prop("a", ai.String()), ai.Prop("b", ai.String()))
	noProp := grammarTool("gram_noprop", ai.GrammarVariants{OpenAILark: "s"})
	noProp.Parameters = &ai.Schema{Type: "object", Required: []string{"query"}}
	notString := grammarTool("gram_notstr", ai.GrammarVariants{OpenAILark: "s"})
	notString.Parameters = ai.Object(ai.Prop("query", ai.Number()))
	notObject := grammarTool("gram_notobj", ai.GrammarVariants{OpenAILark: "s"})
	notObject.Parameters = ai.String()

	for _, tc := range []struct {
		name           string
		tool           ai.Tool
		supports       bool
		wantFormat     string
		wantDefinition string
		wantErr        string
	}{
		{"unsupported provider falls back", grammarTool("g", ai.GrammarVariants{OpenAILark: "s"}), false, "", "", ""},
		{"no config", ai.Tool{Name: "plain", Parameters: grammarParams()}, true, "", "", ""},
		{"lark", grammarTool("gram", ai.GrammarVariants{OpenAILark: "start: /.+/"}), true, "lark", "start: /.+/", ""},
		{"regex", grammarTool("gram_re", ai.GrammarVariants{OpenAIRegex: "[a-z]+"}), true, "regex", "[a-z]+", ""},
		{
			"blank lark falls through to regex, definition untrimmed",
			grammarTool("gram_both", ai.GrammarVariants{OpenAILark: "  ", OpenAIRegex: " [a-z]+ "}),
			true, "regex", " [a-z]+ ", "",
		},
		{
			"no variant", grammarTool("gram_none", ai.GrammarVariants{}), true, "", "",
			`Tool "gram_none" cannot use grammar constrained sampling: no supported grammar variant was provided.`,
		},
		{
			"two required properties", twoRequired, true, "", "",
			`Tool "gram_bad" cannot use grammar constrained sampling: grammar constrained sampling requires exactly one required string property.`,
		},
		{
			"missing properties entry", noProp, true, "", "",
			`Tool "gram_noprop" cannot use grammar constrained sampling: grammar constrained sampling requires a properties entry for query.`,
		},
		{
			"property is not a string", notString, true, "", "",
			`Tool "gram_notstr" cannot use grammar constrained sampling: grammar constrained sampling property query must have type string.`,
		},
		{
			"schema is not an object", notObject, true, "", "",
			`Tool "gram_notobj" cannot use grammar constrained sampling: grammar constrained sampling requires an object parameter schema.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveGrammarSampling(tc.tool, tc.supports)
			assertErrString(t, err, tc.wantErr)
			if err != nil {
				return
			}
			if tc.wantFormat == "" {
				if got != nil {
					t.Fatalf("expected no grammar, got %#v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected a grammar config, got nil")
			}
			if got.format != tc.wantFormat || got.definition != tc.wantDefinition || got.inputProperty != "query" {
				t.Fatalf("grammar = %#v", got)
			}
		})
	}
}

func TestGrammarToolInputProperties(t *testing.T) {
	tools := []ai.Tool{
		{Name: "plain", Parameters: grammarParams()},
		grammarTool("gram", ai.GrammarVariants{OpenAILark: "s"}),
	}
	props, err := grammarToolInputProperties(tools, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props["gram"] != "query" {
		t.Fatalf("props = %#v", props)
	}
	if props, err = grammarToolInputProperties(tools, false); err != nil || len(props) != 0 {
		t.Fatalf("grammar support off must yield no properties: %#v / %v", props, err)
	}
	if _, err := grammarToolInputProperties([]ai.Tool{grammarTool("gram_none", ai.GrammarVariants{})}, true); err == nil {
		t.Fatal("a bad grammar tool must propagate its error")
	}
}

func TestGrammarToolInput(t *testing.T) {
	got, err := grammarToolInput("gram", map[string]any{"query": "SELECT 1"}, "query")
	if err != nil || got != "SELECT 1" {
		t.Fatalf("got %q, %v", got, err)
	}
	_, err = grammarToolInput("gram", map[string]any{"query": 7}, "query")
	assertErrString(t, err, `Grammar tool call "gram" requires argument "query" to be a string.`)
	_, err = grammarToolInput("gram", map[string]any{}, "query")
	assertErrString(t, err, `Grammar tool call "gram" requires argument "query" to be a string.`)
}

// Sequences and expectations captured from pi's appendGrammarToolInputJsonDelta.
func TestGrammarInputBuffer(t *testing.T) {
	type step struct {
		next  string
		final bool
		// want is the emitted delta; wantNone means pi returned undefined.
		want     string
		wantNone bool
		wantErr  string
	}
	for _, tc := range []struct {
		name     string
		property string
		steps    []step
	}{
		{"streamed in three chunks", "query", []step{
			{next: "SEL", want: `{"query":"SEL`},
			{next: "SELECT ", want: "ECT "},
			{next: "SELECT 1", final: true, want: `1"}`},
		}},
		{"escapes the delta and the property name", "in put", []step{
			{next: `a"b`, want: `{"in put":"a\"b`},
			{next: `a"b\c`, want: `\\c`},
			{next: `a"b\c`, final: true, want: `"}`},
		}},
		{"empty input closes immediately", "q", []step{
			{next: "", final: true, want: `{"q":""}`},
		}},
		{"empty non-final delta emits nothing; re-close is a no-op", "q", []step{
			{next: "x", want: `{"q":"x`},
			{next: "x", wantNone: true},
			{next: "x", final: true, want: `"}`},
			{next: "x", final: true, wantNone: true},
		}},
		{"non-monotonic input", "q", []step{
			{next: "ab", want: `{"q":"ab`},
			{next: "a", wantErr: `grammar tool input for property "q" changed non-monotonically`},
		}},
		{"change after close", "q", []step{
			{next: "a", final: true, want: `{"q":"a"}`},
			// Only a repeated CLOSE with the same input is tolerated.
			{next: "a", wantErr: `grammar tool input for property "q" changed after it was closed`},
			{next: "a", final: true, wantNone: true},
			{next: "b", final: true, wantErr: `grammar tool input for property "q" changed after it was closed`},
		}},
		{"HTML-significant characters are not escaped", "<p>&", []step{
			{next: "<x>& ", final: true, want: `{"<p>&":"<x>& "}`},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := newGrammarInputBuffer(tc.property)
			for i, s := range tc.steps {
				delta, ok, err := buf.append(s.next, s.final)
				assertErrString(t, err, s.wantErr)
				if err != nil {
					continue
				}
				if s.wantNone {
					if ok {
						t.Fatalf("step %d: expected no delta, got %q", i, delta)
					}
					continue
				}
				if !ok || delta != s.want {
					t.Fatalf("step %d: delta = %q (ok=%v), want %q", i, delta, ok, s.want)
				}
			}
		})
	}
}

// jsQuote must match JavaScript's JSON.stringify, which differs from Go's
// encoding/json on <, >, &, U+2028/U+2029 and unpaired surrogates. Expectations
// are verbatim node output.
func TestJSQuoteMatchesJavaScript(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", `""`},
		{"plain", `"plain"`},
		{`he said "hi"`, `"he said \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"<script>&</script>", `"<script>&</script>"`},
		{"line\nbreak\ttab\r\b\f", `"line\nbreak\ttab\r\b\f"`},
		{"\x00\x01\x1f\x7f", "\"\\u0000\\u0001\\u001f\x7f\""},
		{"unicode é ü 漢字 🎉", `"unicode é ü 漢字 🎉"`},
		{"\u2028\u2029", "\"\u2028\u2029\""},
		{"slash / not escaped", `"slash / not escaped"`},
		// Lone UTF-16 surrogates (WTF-8 encoded) become \udXXX, like ES2019
		// well-formed JSON.stringify; a well-formed pair stays literal.
		{"\xed\xa0\x80", `"\ud800"`},
		{"a\xed\xbf\xbfb", `"a\udfffb"`},
		{"x\xed\xa0\x80\xed\xb0\x80y", `"x𐀀y"`},
	} {
		if got := jsQuote(tc.in); got != tc.want {
			t.Fatalf("jsQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func assertErrString(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("unexpected error: %v", err)
	case want != "" && err == nil:
		t.Fatalf("expected error %q, got nil", want)
	case want != "" && err.Error() != want:
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}
