package env

import (
	"bytes"
	"encoding/json"
)

// object is a JSON object whose keys keep their order, so requests carry
// their fields in the order upstream's object literals give them.
type object []field

type field struct {
	key   string
	value any
}

// with appends a field.
func (o object) with(key string, value any) object { return append(o, field{key, value}) }

func (o object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(f.key)
		buf.Write(key)
		buf.WriteByte(':')
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(f.value); err != nil {
			return nil, err
		}
		// Encode ends the value with a newline.
		buf.Truncate(buf.Len() - 1)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
