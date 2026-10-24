package ai

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sky-valley/pi/internal/jstext"
)

// streamingJSONCapture is testdata/streaming-json/streaming-json-f715a15c1.json,
// recorded from pi's StreamingJsonParser and ToolCallArgumentsStream at
// f715a15c1 by capture.mts beside it (see its header for the corpus and the
// control).
type streamingJSONCapture struct {
	Source    string            `json:"source"`
	Control   string            `json:"control"`
	Documents []json.RawMessage `json:"documents"`
	Rows      []struct {
		Name   string            `json:"name"`
		Mode   string            `json:"mode"`
		Doc    *int              `json:"doc"`
		Size   int               `json:"size"`
		Cuts   []int             `json:"cuts"`
		Pieces []json.RawMessage `json:"pieces"`
		Steps  []struct {
			Changes  *string         `json:"changes"`
			Snapshot json.RawMessage `json:"snapshot"`
			Size     *int            `json:"size"`
			Same     bool            `json:"same"`
		} `json:"steps"`
	} `json:"rows"`
	Args []struct {
		Name string `json:"name"`
		Doc  *int   `json:"doc"`
		Ops  []struct {
			Op   string          `json:"op"`
			Text json.RawMessage `json:"text"`
		} `json:"ops"`
		Results []json.RawMessage `json:"results"`
	} `json:"args"`
}

func loadStreamingJSONCapture(t *testing.T) *streamingJSONCapture {
	t.Helper()
	data, err := os.ReadFile("testdata/streaming-json/streaming-json-f715a15c1.json")
	if err != nil {
		t.Fatal(err)
	}
	var c streamingJSONCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// codeUnits reads a capture text: a string, or the code units of one that
// holds a lone surrogate.
func codeUnits(t *testing.T, raw json.RawMessage) []uint16 {
	t.Helper()
	var units []uint16
	if json.Unmarshal(raw, &units) == nil {
		return units
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("capture text %s: %v", raw, err)
	}
	return utf16.Encode([]rune(s))
}

// wtf8 is the Go string of UTF-16 code units: pairs as one character, a lone
// surrogate as its WTF-8 triple.
func wtf8(units []uint16) string {
	var b strings.Builder
	for i := 0; i < len(units); i++ {
		u := rune(units[i])
		if utf16.IsSurrogate(u) && u <= 0xDBFF && i+1 < len(units) && rune(units[i+1]) >= 0xDC00 && rune(units[i+1]) <= 0xDFFF {
			b.WriteRune(utf16.DecodeRune(u, rune(units[i+1])))
			i++
			continue
		}
		b.WriteString(jstext.CodeUnit(u))
	}
	return b.String()
}

// jsStringify writes v as JSON.stringify does, lone surrogates escaped.
func jsStringify(v any) string {
	var b strings.Builder
	writeJS(&b, v)
	return b.String()
}

func writeJS(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		fmt.Fprint(b, t)
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			b.WriteString("null")
		} else {
			b.WriteString(jstext.NumberToString(t))
		}
	case int:
		fmt.Fprint(b, t)
	case string:
		writeJSString(b, t)
	case OrderedObject:
		b.WriteByte('{')
		for i, f := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSString(b, f.Key)
			b.WriteByte(':')
			writeJS(b, f.Value)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJS(b, e)
		}
		b.WriteByte(']')
	case JSONPath:
		writeJS(b, []any(t))
	case []JSONChange:
		b.WriteByte('[')
		for i, c := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"kind":`)
			writeJSString(b, string(c.Kind))
			b.WriteString(`,"path":`)
			writeJS(b, c.Path)
			if c.Kind == JSONChangeSet {
				b.WriteString(`,"value":`)
				writeJS(b, c.Value)
			} else {
				b.WriteString(`,"text":`)
				writeJSString(b, c.Text)
			}
			b.WriteByte('}')
		}
		b.WriteByte(']')
	default:
		panic(fmt.Sprintf("writeJS: %T", v))
	}
}

func writeJSString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		if u, ok := jstext.WTF8Surrogate(s[i:]); ok {
			fmt.Fprintf(b, `\u%04x`, u)
			i += 3
			continue
		}
		switch c := s[i]; {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
		i++
	}
	b.WriteByte('"')
}

// TestStreamingJSONParserLikePi feeds every capture row's pieces to one
// parser and checks, after each piece, the changes Append returned and the
// snapshot — its JSON text, and whether it is the very snapshot the previous
// call returned, which is how the copy-credit lag shows.
func TestStreamingJSONParserLikePi(t *testing.T) {
	c := loadStreamingJSONCapture(t)
	docs := make([][]uint16, len(c.Documents))
	for i, d := range c.Documents {
		docs[i] = codeUnits(t, d)
	}
	if len(c.Rows) < 5000 {
		t.Fatalf("%d rows: the capture is truncated", len(c.Rows))
	}
	for _, row := range c.Rows {
		var pieces []string
		switch {
		case row.Doc != nil && row.Size > 0:
			units := docs[*row.Doc]
			for i := 0; i < len(units); i += row.Size {
				pieces = append(pieces, wtf8(units[i:min(i+row.Size, len(units))]))
			}
			if len(pieces) == 0 {
				pieces = []string{""}
			}
		case row.Doc != nil:
			units := docs[*row.Doc]
			bounds := append(append([]int{0}, row.Cuts...), len(units))
			for i := 1; i < len(bounds); i++ {
				pieces = append(pieces, wtf8(units[bounds[i-1]:bounds[i]]))
			}
		default:
			for _, p := range row.Pieces {
				pieces = append(pieces, wtf8(codeUnits(t, p)))
			}
		}
		var parser StreamingJSONParser
		var prev jsonNode
		var hasPrev bool
		prevText := ""
		step := 0
		for i, piece := range pieces {
			changes := parser.Append(piece)
			snap, ok := parser.snapshot()
			same := i > 0 && sameSnapshot(snap, ok, prev, hasPrev)
			prev, hasPrev = snap, ok
			text := "null"
			if ok {
				text = jsStringify(snap.ord)
			}
			if row.Mode == "final" && i < len(pieces)-1 {
				continue
			}
			if step >= len(row.Steps) {
				t.Fatalf("%s: more steps than pi recorded", row.Name)
			}
			want := row.Steps[step]
			step++
			if same != want.Same {
				t.Errorf("%s piece %d: same snapshot = %v, pi %v", row.Name, i, same, want.Same)
			}
			if row.Mode == "size" {
				size := -1
				if ok {
					size = jstext.CodeUnitLength(text)
				}
				if size != *want.Size {
					t.Errorf("%s piece %d: snapshot length %d, pi %d", row.Name, i, size, *want.Size)
				}
				continue
			}
			wantText := prevText
			if want.Snapshot != nil {
				wantText = "null"
				if string(want.Snapshot) != "null" {
					if err := json.Unmarshal(want.Snapshot, &wantText); err != nil {
						t.Fatal(err)
					}
				}
			}
			prevText = wantText
			if !ok {
				text = "null"
			}
			if text != wantText {
				t.Errorf("%s piece %d %+q: snapshot\n got %s\n  pi %s", row.Name, i, piece, text, wantText)
			}
			if row.Mode == "all" {
				wantChanges := "[]"
				if want.Changes != nil {
					wantChanges = *want.Changes
				}
				if got := jsStringify(changes); got != wantChanges {
					t.Errorf("%s piece %d %+q: changes\n got %s\n  pi %s", row.Name, i, piece, got, wantChanges)
				}
			}
		}
		if step != len(row.Steps) {
			t.Errorf("%s: %d steps, pi %d", row.Name, step, len(row.Steps))
		}
	}
}

// argumentsJSON is JSON.stringify of a tool call's arguments as the port
// holds them: the ordered form, or the map when there is none.
func argumentsJSON(t *testing.T, args map[string]any, order OrderedObject) string {
	t.Helper()
	if order != nil {
		if !reflect.DeepEqual(order.Plain(), args) {
			t.Fatalf("ordered arguments %s disagree with the map %v", jsStringify(order), args)
		}
		return jsStringify(order)
	}
	if len(args) != 0 {
		t.Fatalf("arguments %v have no ordered form", args)
	}
	return "{}"
}

// A finished call keeps half a surrogate pair, as JSON.parse keeps the code
// unit (and as the streamed partials do), and writes it back as
// JSON.stringify does; an escaped pair, or an escape and a triple, join.
func TestParseToolCallArgumentsKeepsHalfASurrogatePair(t *testing.T) {
	bs := "\\"
	text := `{"s":"` + bs + `ud83dx","p":"` + bs + `ud83d` + bs + `ude00","m":"` + bs + `ud83d` + jstext.CodeUnit(0xde00) + `"}`
	_, order, argsErr := ParseToolCallArguments(text)
	if argsErr != "" {
		t.Fatal(argsErr)
	}
	if s, _ := order.Get("s"); s != jstext.CodeUnit(0xd83d)+"x" {
		t.Errorf("s = %+q, want the high surrogate's WTF-8 then x", s)
	}
	out, err := order.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"s":"` + bs + `ud83dx","p":"😀","m":"😀"}`; string(out) != want {
		t.Errorf("MarshalJSON = %s, want %s", out, want)
	}
}

// loneSurrogateEscape matches the escape JSON.stringify writes for half a
// surrogate pair.
var loneSurrogateEscape = regexp.MustCompile(`\\ud[89a-f][0-9a-f]{2}`)

func samePointer(a, b map[string]any) bool {
	return a != nil && b != nil && reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// TestToolCallArgumentsStreamLikePi drives a ToolCallArgumentsStream through
// each capture row's appends, replaces and finishes.
func TestToolCallArgumentsStreamLikePi(t *testing.T) {
	c := loadStreamingJSONCapture(t)
	// loneFinishes counts the finishes whose arguments keep half a surrogate
	// pair, as JSON.parse does, so the rows that pin it cannot go missing.
	loneFinishes := 0
	defer func() {
		if loneFinishes == 0 {
			t.Errorf("no finish row keeps half a surrogate pair; the capture lost its rows")
		}
	}()
	for _, row := range c.Args {
		type op struct{ op, text string }
		var ops []op
		if row.Doc != nil {
			units := codeUnits(t, c.Documents[*row.Doc])
			for i := range units {
				ops = append(ops, op{"append", wtf8(units[i : i+1])})
			}
			if len(units) == 0 {
				ops = append(ops, op{"append", ""})
			}
			ops = append(ops, op{op: "finish"})
		} else {
			for _, o := range row.Ops {
				var text string
				if o.Text != nil {
					text = wtf8(codeUnits(t, o.Text))
				}
				ops = append(ops, op{o.Op, text})
			}
		}
		if len(ops) != len(row.Results) {
			t.Fatalf("%s: %d ops, pi recorded %d results", row.Name, len(ops), len(row.Results))
		}
		var s ToolCallArgumentsStream
		var prev map[string]any
		for i, o := range ops {
			want := row.Results[i]
			switch o.op {
			case "append", "replace":
				var w struct {
					Arguments string  `json:"arguments"`
					Same      bool    `json:"same"`
					Delta     *string `json:"delta"`
					Text      string  `json:"text"`
				}
				if err := json.Unmarshal(want, &w); err != nil {
					t.Fatal(err)
				}
				var args map[string]any
				var order OrderedObject
				if o.op == "append" {
					args, order = s.Append(o.text)
				} else {
					delta, hasDelta, a, ord := s.Replace(o.text)
					args, order = a, ord
					if hasDelta != (w.Delta != nil) || (hasDelta && delta != *w.Delta) {
						t.Errorf("%s op %d: replace delta %q (%v), pi %v", row.Name, i, delta, hasDelta, w.Delta)
					}
					if s.Text() != w.Text {
						t.Errorf("%s op %d: text %q, pi %q", row.Name, i, s.Text(), w.Text)
					}
				}
				if got := argumentsJSON(t, args, order); got != w.Arguments {
					t.Errorf("%s op %d %+q: arguments\n got %s\n  pi %s", row.Name, i, o.text, got, w.Arguments)
				}
				if same := samePointer(args, prev); same != w.Same {
					t.Errorf("%s op %d %+q: same arguments = %v, pi %v", row.Name, i, o.text, same, w.Same)
				}
				prev = args
			case "finish":
				var wantText string
				if err := json.Unmarshal(want, &wantText); err != nil {
					t.Fatal(err)
				}
				args, order, argsErr := s.Finish()
				got := `{"arguments":` + argumentsJSON(t, args, order)
				if argsErr != "" {
					got += `,"argumentsError":` + jsStringify(argsErr)
				}
				got += "}"
				if loneSurrogateEscape.MatchString(wantText) {
					loneFinishes++
				}
				if got != wantText {
					t.Errorf("%s op %d: finish\n got %s\n  pi %s", row.Name, i, got, wantText)
				}
			}
		}
	}
}
