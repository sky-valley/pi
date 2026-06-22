package providers

import (
	"bytes"
	"encoding/json"

	"github.com/sky-valley/pi/ai"
)

// chatTemplateKwargValue ports pi's ChatTemplateKwargValue (ai/types.ts): either
// a scalar (string | number | boolean | null) or a pi-controlled variable object
// {$var: "thinking.enabled" | "thinking.effort", omitWhenOff?: boolean}.
type chatTemplateKwargValue struct {
	isVar       bool
	scalar      any // string, float64, bool, or nil (only when !isVar)
	varName     string
	omitWhenOff bool
}

// chatTemplateKwarg is one ordered key/value entry. pi iterates
// Object.entries(compat.chatTemplateKwargs) in insertion order, so we preserve
// the model-config key order rather than using a Go map (which would sort keys).
type chatTemplateKwarg struct {
	key   string
	value chatTemplateKwargValue
}

// orderedJSONObject marshals key/value pairs in slice order, mirroring
// JSON.stringify of a JS object (insertion order) for byte-exact request bodies.
type orderedJSONObject []struct {
	Key   string
	Value any
}

func (o orderedJSONObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := json.Marshal(f.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// parseChatTemplateKwargs decodes the `chatTemplateKwargs` compat object,
// preserving key order. Returns nil for absent/invalid input (pi falls back to
// the detected default of {}, which emits nothing).
func parseChatTemplateKwargs(raw json.RawMessage) []chatTemplateKwarg {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	var out []chatTemplateKwarg
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil
		}
		var valRaw json.RawMessage
		if err := dec.Decode(&valRaw); err != nil {
			return nil
		}
		out = append(out, chatTemplateKwarg{key: key, value: parseChatTemplateKwargValue(valRaw)})
	}
	return out
}

// parseChatTemplateKwargValue mirrors pi's runtime check
// `typeof value !== "object" || value === null` — a JSON object (non-null) is the
// $var form, everything else (incl. null) is a scalar carried through verbatim.
func parseChatTemplateKwargValue(raw json.RawMessage) chatTemplateKwargValue {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj struct {
			Var         *string `json:"$var"`
			OmitWhenOff *bool   `json:"omitWhenOff"`
		}
		v := chatTemplateKwargValue{isVar: true}
		if json.Unmarshal(raw, &obj) == nil {
			if obj.Var != nil {
				v.varName = *obj.Var
			}
			if obj.OmitWhenOff != nil {
				v.omitWhenOff = *obj.OmitWhenOff
			}
		}
		return v
	}
	var scalar any
	_ = json.Unmarshal(raw, &scalar)
	return chatTemplateKwargValue{scalar: scalar}
}

// buildChatTemplateKwargs ports pi's buildChatTemplateKwargs (openai-completions.ts):
// resolve each configured kwarg against the model/level, dropping omitted ones,
// and return nil when nothing remains (pi returns undefined → no param emitted).
func buildChatTemplateKwargs(model *ai.Model, compat openAICompletionsCompat, level string) orderedJSONObject {
	var out orderedJSONObject
	for _, kw := range compat.ChatTemplateKwargs {
		if resolved, include := resolveChatTemplateKwargValue(model, level, kw.value); include {
			out = append(out, struct {
				Key   string
				Value any
			}{kw.key, resolved})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveChatTemplateKwargValue ports pi's resolveChatTemplateKwargValue. The
// bool return is pi's undefined sentinel (false = omit the key).
func resolveChatTemplateKwargValue(model *ai.Model, level string, v chatTemplateKwargValue) (any, bool) {
	if !v.isVar {
		// Scalar (incl. JSON null) is carried through as-is.
		return v.scalar, true
	}
	enabled := level != ""
	if !enabled && v.omitWhenOff {
		return nil, false
	}
	if v.varName == "thinking.enabled" {
		return enabled, true
	}
	// "thinking.effort": mappedValue = reasoningEffort ? tlm[effort] : tlm.off.
	mapKey := ai.ModelThinkingLevel(level)
	if !enabled {
		mapKey = "off"
	}
	mapped, present := lookupThinkingLevel(model, mapKey)
	if !present {
		// pi: undefined → reasoningEffort (the raw level when enabled, else undefined→omit).
		if enabled {
			return level, true
		}
		return nil, false
	}
	if mapped != nil {
		return *mapped, true
	}
	// present-null → undefined → omit.
	return nil, false
}

func lookupThinkingLevel(model *ai.Model, key ai.ModelThinkingLevel) (*string, bool) {
	if model.ThinkingLevelMap == nil {
		return nil, false
	}
	v, ok := model.ThinkingLevelMap[key]
	return v, ok
}
