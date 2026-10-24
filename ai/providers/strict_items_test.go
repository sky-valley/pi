package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// An items value that is not one schema — the tuple form or a boolean — is
// kept by ai.Schema, and strict conversion refuses it as pi's
// makeJsonSchemaNodeStrict does: an array is "tuple schemas are unsupported",
// anything else "boolean schemas are unsupported", and any items makes a
// union variant structured.
func TestStrictJSONSchemaRefusesNonSchemaItems(t *testing.T) {
	for _, tc := range []struct{ schema, want string }{
		{`{"type":"object","properties":{"t":{"type":"array","items":[{"type":"string"},{"const":"x"}]}},"required":["t"]}`, "tuple schemas are unsupported"},
		{`{"type":"object","properties":{"t":{"type":"array","items":false}},"required":["t"]}`, "boolean schemas are unsupported"},
		{`{"type":"object","properties":{"t":{"anyOf":[{"items":[{"type":"string"}]},{"type":"string"}]}},"required":["t"]}`, "object and array unions are unsupported"},
	} {
		var schema ai.Schema
		if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
			t.Fatal(err)
		}
		_, err := makeStrictJSONSchema(&schema, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.schema, err, tc.want)
		}
	}
}

// An enum ai.Schema's typed field cannot hold — empty, null, not an array —
// is still present, as in pi, so it is sent and keeps a const from being
// rewritten (pi tests result.enum === undefined).
func TestGoogleToolsKeepConstBesideAnyEnum(t *testing.T) {
	for _, enum := range []string{`[]`, `null`, `"x"`} {
		in := `{"type":"object","properties":{"v":{"const":"a","enum":` + enum + `}}}`
		if got := googleSchemaJSON(t, in); got != in {
			t.Errorf("parametersJsonSchema\n got %s\nwant %s", got, in)
		}
	}
}
