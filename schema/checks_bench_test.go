package schema

import "testing"

func benchValidator(b *testing.B, schema Object) *Validator {
	b.Helper()
	v, err := Compile(schema, Options{})
	if err != nil {
		b.Fatal(err)
	}
	return v
}

// BenchmarkCheckNumberBounds: a number with plain float bounds, the common
// case the number keywords' fast path is for.
func BenchmarkCheckNumberBounds(b *testing.B) {
	v := benchValidator(b, Object{{"type", "number"}, {"minimum", 0.0}, {"maximum", 100.0}})
	for b.Loop() {
		if !v.Check(42.5) {
			b.Fatal("Check(42.5) = false")
		}
	}
}

// BenchmarkCheckBoundedIntegerArray: 1000 bounded integers as array items.
func BenchmarkCheckBoundedIntegerArray(b *testing.B) {
	v := benchValidator(b, Object{{"type", "array"}, {"items", Object{{"type", "integer"}, {"minimum", 0.0}, {"exclusiveMaximum", 1000.0}}}})
	items := make([]any, 1000)
	for i := range items {
		items[i] = float64(i)
	}
	for b.Loop() {
		if !v.Check(items) {
			b.Fatal("Check(items) = false")
		}
	}
}
