package providers

import (
	"errors"
	"fmt"
	"unicode/utf16"

	"github.com/sky-valley/pi/internal/jstext"
)

// openAIFirstChoice is pi's `Array.isArray(chunk.choices) ? chunk.choices[0]
// : undefined`.
func openAIFirstChoice(chunk any) any {
	if choices, isArray := jsGet(chunk, "choices").([]any); isArray && len(choices) > 0 {
		return choices[0]
	}
	return jsUndefined
}

// appendValue is appendDelta for an input delta as an event holds it, a
// JSON value: pi appends any value, as `buffer.input += inputDelta` and
// `JSON.stringify(inputDelta).slice(1, -1)` make of it, so a number's JSON
// text loses its first and last digits (5 adds nothing, 123 adds "2"). The
// empty check reads `inputDelta.length`, which only a string and an array
// have, and which undefined and null throw on — after the closed check.
func (b *grammarInputBuffer) appendValue(inputDelta any) (delta string, ok bool, err error) {
	if text, isString := inputDelta.(string); isString {
		return b.appendDelta(text)
	}
	if b.closed {
		return "", false, fmt.Errorf("grammar tool input for property %s changed after it was closed", quoteRaw(b.property))
	}
	switch v := inputDelta.(type) {
	case jsUndefinedValue:
		return "", false, errors.New("Cannot read properties of undefined (reading 'length')")
	case nil:
		return "", false, errors.New("Cannot read properties of null (reading 'length')")
	case []any:
		if len(v) == 0 {
			return "", false, nil
		}
	}
	text, err := jsToString(inputDelta)
	if err != nil {
		return "", false, err
	}
	stringified, err := jstext.Stringify(inputDelta)
	if err != nil {
		return "", false, err
	}
	b.input.WriteString(text)
	units := utf16.Encode([]rune(stringified))
	inner := ""
	if len(units) >= 2 {
		inner = string(utf16.Decode(units[1 : len(units)-1]))
	}
	return b.jsonStart() + inner, true, nil
}
