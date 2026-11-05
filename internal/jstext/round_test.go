package jstext

import (
	"math"
	"testing"
)

// Expected values are what node's Math.round returns for each input.
func TestMathRound(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{2.5, 3}, {-2.5, -2}, {-2.6, -3}, {0.49999999999999994, 0}, {1.4, 1},
		{-0.4, math.Copysign(0, -1)}, {1e300, 1e300}, {308.4999, 308},
	}
	for _, c := range cases {
		if got := MathRound(c.in); got != c.want || math.Signbit(got) != math.Signbit(c.want) {
			t.Errorf("MathRound(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if got := MathRound(math.NaN()); !math.IsNaN(got) {
		t.Errorf("MathRound(NaN) = %v", got)
	}
}
