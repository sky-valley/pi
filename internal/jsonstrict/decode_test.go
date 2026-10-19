package jsonstrict

import (
	"errors"
	"reflect"
	"testing"
)

type note struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Pin   bool   `json:"-"`
	Rank  int
}

// TestDecodeReadsTheConfiguredTag: the tag key is per decoder, not fixed to
// chord's `json`.
func TestDecodeReadsTheConfiguredTag(t *testing.T) {
	d := &Decoder{Tag: "json"}
	var got note
	// Rank is untagged and decodes under its Go name; Body is optional; Pin is
	// skipped, so sending it is an unknown property.
	if err := d.Decode(map[string]any{"title": "a", "Rank": float64(2)}, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := (note{Title: "a", Rank: 2}); got != want {
		t.Errorf("decoded %+v, want %+v", got, want)
	}
	err := d.Decode(map[string]any{"title": "a", "Rank": float64(2), "Pin": true}, &got)
	if err == nil || err.Error() != `value has unexpected property "Pin"` {
		t.Errorf("skipped field accepted: err = %v", err)
	}
	err = d.Decode(map[string]any{"Rank": float64(2)}, &got)
	if err == nil || err.Error() != `value is missing required property "title"` {
		t.Errorf("missing required: err = %v", err)
	}
}

// TestDecodeNamesTheRoot: Root is how the top-level value is described, so a
// protocol error says "message" where a generic one says "value".
func TestDecodeNamesTheRoot(t *testing.T) {
	var got note
	for _, tc := range []struct{ root, want string }{
		{"", "value must be an object"},
		{"message", "message must be an object"},
	} {
		d := &Decoder{Tag: "json", Root: tc.root}
		err := d.Decode("not an object", &got)
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("root %q: got %T, want *Error", tc.root, err)
		}
		if e.Msg != tc.want {
			t.Errorf("root %q: message = %q, want %q", tc.root, e.Msg, tc.want)
		}
	}
}

type shape interface{ shape() }
type circle struct {
	Kind   string  `json:"kind"`
	Radius float64 `json:"radius"`
}
type square struct {
	Kind string  `json:"kind"`
	Side float64 `json:"side"`
}

func (*circle) shape() {}
func (*square) shape() {}

func decodeShape(d *Decoder) func(any) (shape, error) {
	return func(value any) (shape, error) {
		tag, _, err := Discriminant(value, "kind")
		if err != nil {
			return nil, err
		}
		switch tag {
		case "circle":
			return DecodeMember[*circle](d, value)
		case "square":
			return DecodeMember[*square](d, value)
		default:
			return nil, Errorf("unknown shape %q", tag)
		}
	}
}

// TestUnionRegistryIsPerDecoder: a union registered on one decoder is unknown
// to another, which is what lets two packages own unrelated union sets.
func TestUnionRegistryIsPerDecoder(t *testing.T) {
	registered := &Decoder{Tag: "json"}
	RegisterUnion(registered, decodeShape(registered))
	bare := &Decoder{Tag: "json"}

	var target struct {
		Shape shape `json:"shape"`
	}
	value := map[string]any{"shape": map[string]any{"kind": "circle", "radius": float64(1.5)}}
	if err := registered.Decode(value, &target); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := (&circle{Kind: "circle", Radius: 1.5}); !reflect.DeepEqual(target.Shape, want) {
		t.Errorf("decoded %#v, want %#v", target.Shape, want)
	}

	err := bare.Decode(value, &target)
	if err == nil || err.Error() != "no decoder registered for union jsonstrict.shape at shape" {
		t.Errorf("bare decoder: err = %v", err)
	}

	err = registered.Decode(map[string]any{"shape": map[string]any{"kind": "blob"}}, &target)
	if err == nil || err.Error() != `shape: unknown shape "blob"` {
		t.Errorf("unknown tag: err = %v", err)
	}
}

// TestDecodeAcceptsJSONNumbersForIntegers: encoding/json hands every number
// over as float64, so an integer field must take an integral float and refuse
// a fractional one.
func TestDecodeAcceptsJSONNumbersForIntegers(t *testing.T) {
	d := &Decoder{Tag: "json"}
	var got struct {
		N int64 `json:"n"`
	}
	if err := d.Decode(map[string]any{"n": float64(7)}, &got); err != nil || got.N != 7 {
		t.Errorf("integral float: got %d, err %v", got.N, err)
	}
	if err := d.Decode(map[string]any{"n": 7.5}, &got); err == nil || err.Error() != "n must be an integer" {
		t.Errorf("fractional: err = %v", err)
	}
}
