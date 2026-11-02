package schema

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/sky-valley/pi/internal/jstext"
)

// The differential golden: testdata/golden/capture-6536f8012.json, written by
// testdata/golden/capture.mjs from upstream's src at 6536f8012. Each test
// replays one section and compares, as JSON text with key order kept, what
// the port returns with what upstream returned.

const goldenFile = "testdata/golden/capture-6536f8012.json"

// goldenFormats are capture.mjs's stand-in formats.
var goldenFormats = map[string]FormatCheck{
	"even":  func(s string) bool { return jstext.CodeUnitLength(s)%2 == 0 },
	"lower": func(s string) bool { return s == jstext.ToLower(s) },
}

func loadGolden(t *testing.T) Object {
	t.Helper()
	data, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return v.(Object)
}

func section(t *testing.T, golden Object, name string) []any {
	t.Helper()
	v, _ := golden.Get(name)
	list, _ := v.([]any)
	if len(list) == 0 {
		t.Fatalf("golden section %q is empty", name)
	}
	return list
}

func field(o any, key string) any {
	v, _ := getOwn(o, key)
	return v
}

// untag reads capture.mjs's tagged values back into the value model.
func untag(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = untag(e)
		}
		return out
	case Object:
		if len(x) > 0 && x[0].Key == "$" {
			switch x[0].Value {
			case "undefined":
				return undefined
			case "-0":
				return math.Copysign(0, -1)
			case "NaN":
				return math.NaN()
			case "Infinity":
				return math.Inf(1)
			case "-Infinity":
				return math.Inf(-1)
			case "bytes":
				items := field(x, "v").([]any)
				b := make([]byte, len(items))
				for i, n := range items {
					b[i] = byte(n.(float64))
				}
				return b
			case "object":
				var out Object
				for _, e := range field(x, "v").([]any) {
					pair := e.([]any)
					out = append(out, Member{pair[0].(string), untag(pair[1])})
				}
				return out
			}
			return unsupportedValue{}
		}
		out := make(Object, len(x))
		for i, m := range x {
			out[i] = Member{m.Key, untag(m.Value)}
		}
		return out
	}
	return v
}

// unsupportedValue is a tagged value the port has no form for (a bigint, a
// symbol).
type unsupportedValue struct{}

func hasUnsupported(v any) bool {
	switch x := v.(type) {
	case unsupportedValue:
		return true
	case []any:
		for _, e := range x {
			if hasUnsupported(e) {
				return true
			}
		}
	case Object:
		for _, m := range x {
			if hasUnsupported(m.Value) {
				return true
			}
		}
	}
	return false
}

// tag is capture.mjs's enc: v with what JSON cannot hold tagged.
func tag(v any) any {
	if f, ok := numberOf(v); ok {
		switch {
		case math.IsNaN(f):
			return Object{{"$", "NaN"}}
		case math.IsInf(f, 1):
			return Object{{"$", "Infinity"}}
		case math.IsInf(f, -1):
			return Object{{"$", "-Infinity"}}
		case f == 0 && math.Signbit(f):
			return Object{{"$", "-0"}}
		}
		return f
	}
	switch x := v.(type) {
	case undefinedValue:
		return Object{{"$", "undefined"}}
	case []byte:
		items := make([]any, len(x))
		for i, b := range x {
			items[i] = float64(b)
		}
		return Object{{"$", "bytes"}, {"v", items}}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = tag(e)
		}
		return out
	case Object, map[string]any:
		keys := ownKeys(x)
		if hasOwn(x, "$") {
			pairs := make([]any, len(keys))
			for i, k := range keys {
				pairs[i] = []any{k, tag(propOf(x, k))}
			}
			return Object{{"$", "object"}, {"v", pairs}}
		}
		out := make(Object, len(keys))
		for i, k := range keys {
			out[i] = Member{k, tag(propOf(x, k))}
		}
		return out
	}
	return v
}

func errorList(errs []ValidationError) []any {
	out := make([]any, len(errs))
	for i, e := range errs {
		out[i] = Object{{"path", e.Path}, {"keyword", e.Keyword}, {"message", e.Message}}
	}
	return out
}

func goldenOptions(o any) Options {
	var opts Options
	if d, ok := field(o, "draft").(string); ok {
		opts.Draft = d
	}
	opts.Coerce = field(o, "coerce") == true
	if u, ok := field(o, "unresolvedRefs").(string); ok {
		opts.UnresolvedRefs = u
	}
	if names, ok := field(o, "formats").([]any); ok {
		opts.Formats = map[string]FormatCheck{}
		for _, n := range names {
			opts.Formats[n.(string)] = goldenFormats[n.(string)]
		}
	}
	if s, ok := field(o, "schemas").(Object); ok {
		opts.Schemas = map[string]any{}
		for _, m := range s {
			opts.Schemas[m.Key] = untag(m.Value)
		}
	}
	return opts
}

func TestGoldenMatchesUpstream(t *testing.T) {
	rows := section(t, loadGolden(t), "rows")
	cases := 0
	for _, r := range rows {
		name := field(r, "name").(string)
		schema := untag(field(r, "schema"))
		opts := goldenOptions(field(r, "options"))
		v, err := Compile(schema, opts)
		if want, ok := field(r, "compileError").(string); ok {
			cases++
			if err == nil || err.Error() != want {
				t.Errorf("%s: compile error %v, want %q", name, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: compile: %v", name, err)
			continue
		}
		for i, c := range field(r, "cases").([]any) {
			cases++
			value := untag(field(c, "value"))
			got := Object{{"value", tag(value)}, {"check", v.Check(value)}, {"errors", errorList(v.Errors(value))}}
			if opts.Coerce {
				coerced := v.Coerce(value)
				got = append(got, Member{"coerce", tag(coerced)}, Member{"coerceSame", strictEquals(coerced, value)})
				parsed, errs := v.Parse(value)
				if errs == nil {
					got = append(got, Member{"parse", Object{{"ok", true}, {"value", tag(parsed)}}})
				} else {
					got = append(got, Member{"parse", Object{{"ok", false}, {"errors", errorList(errs)}}})
				}
			}
			if g, w := stringify(got), stringify(c); g != w {
				t.Errorf("%s case %d:\n got %s\nwant %s", name, i, g, w)
			}
		}
	}
	if cases < 500 {
		t.Fatalf("only %d golden cases", cases)
	}
}

func TestResolveURIMatchesUpstream(t *testing.T) {
	for _, u := range section(t, loadGolden(t), "uris") {
		row := u.([]any)
		ref, base, want := row[0].(string), row[1].(string), row[2].(string)
		if got := resolveURI(ref, base); got != want {
			t.Errorf("resolveURI(%q, %q) = %q, want %q", ref, base, got, want)
		}
	}
}

// re2Unsupported are the golden sources RE2 cannot express: each matches
// every string in the port, where upstream matches as the subjects record.
var re2Unsupported = map[string]bool{
	"(?<n>a)\\k<n>":     true, // backreference
	"(a)\\1":            true,
	"(?=a)a":            true, // lookaround
	"(?!a).":            true,
	"(?<=a)b":           true,
	"(?<!a)b":           true,
	"a{1001}":           true, // repeat count over RE2's 1000
	"^\\p{sc=Grek}$":    true, // script code; Go knows script names only
	"^\\p{Alphabetic}$": true, // binary property
	"^\\uD83D$":         true, // half a surrogate pair
}

func TestRegexMatchesUpstream(t *testing.T) {
	golden := loadGolden(t)
	subjectsValue, _ := golden.Get("subjects")
	subjects := subjectsValue.([]any)
	for _, r := range section(t, golden, "regexes") {
		source := field(r, "source").(string)
		re, err := sharedRegex(source)
		if want, ok := field(r, "error").(string); ok {
			if err == nil || err.Error() != want {
				t.Errorf("%q: error %v, want %q", source, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", source, err)
			continue
		}
		if re2Unsupported[source] {
			if re.re != nil {
				t.Errorf("%q: listed as unsupported by RE2 but compiled", source)
			}
			continue
		}
		if re.re == nil {
			t.Errorf("%q: unsupported, not listed in re2Unsupported", source)
			continue
		}
		for i, m := range field(r, "matches").([]any) {
			s := subjects[i].(string)
			if got := re.test(s); got != m.(bool) {
				t.Errorf("%q.test(%q) = %v, want %v (translated %s)", source, s, got, m, re.re)
			}
		}
	}
}

// TestTypeBoxConvertMatchesUpstream replays upstream's port of TypeBox
// 1.3.27's Value.Convert tests (test/coerce-typebox.test.ts): pass 2 alone,
// per Value.Convert call the file makes.
func TestTypeBoxConvertMatchesUpstream(t *testing.T) {
	golden := loadGolden(t)
	var replayed, unsupported int
	for _, r := range section(t, golden, "typebox") {
		name := field(r, "test").(string)
		schema := untag(field(r, "schema"))
		value := untag(field(r, "value"))
		want := untag(field(r, "result"))
		if hasUnsupported(schema) || hasUnsupported(value) || hasUnsupported(want) {
			unsupported++ // a bigint: no Go form
			continue
		}
		if strings.Contains(stringify(schema), `"format"`) {
			t.Fatalf("%s: the schema has a format; replay it with real formats", name)
		}
		c := newCompiler(toTree(schema), &treeOptions{draft: Draft2020}, false)
		run := &coerceRun{coercer: &coercer{c: c}}
		got := run.convertOnly(value)
		if g, w := stringify(tag(got)), stringify(tag(want)); g != w {
			t.Errorf("%s: Convert(%s) = %s, want %s", name, stringify(tag(value)), g, w)
		}
		replayed++
	}
	skipped, _ := golden.Get("typeboxSkipped")
	t.Logf("TypeBox Value.Convert: %d calls replayed, %d with a bigint skipped, %d tests upstream skips", replayed, unsupported, len(skipped.([]any)))
	if replayed < 200 {
		t.Fatalf("only %d TypeBox rows replayed", replayed)
	}
}
