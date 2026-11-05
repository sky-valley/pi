package jstext

import "math"

// MathRound is JavaScript's Math.round: the nearest integer, a tie going
// toward +Infinity (-2.5 rounds to -2, where math.Round gives -3). It is not
// floor(x+0.5), whose addition rounds 0.49999999999999994 up to 1. NaN,
// infinities and integers come back unchanged, and a negative x that rounds
// to zero gives -0.
func MathRound(x float64) float64 {
	r := math.Floor(x)
	if x-r >= 0.5 {
		r++
	}
	if r == 0 && math.Signbit(x) {
		return math.Copysign(0, -1) // -0.4 rounds to -0
	}
	return r
}
