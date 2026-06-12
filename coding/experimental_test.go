package coding

import (
	"os"
	"testing"
)

// Port of upstream test/experimental.test.ts (66335d3a): only the exact value
// "1" enables experimental features.
func TestAreExperimentalFeaturesEnabled(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  bool
	}{
		{name: "unset", set: false, want: false},
		{name: "empty", set: true, value: "", want: false},
		{name: "one", set: true, value: "1", want: true},
		{name: "zero", set: true, value: "0", want: false},
		{name: "non-1 value", set: true, value: "true", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("PI_EXPERIMENTAL", tc.value)
			} else {
				// t.Setenv registers the restore, then the variable is removed
				// to exercise the truly-unset case.
				t.Setenv("PI_EXPERIMENTAL", "")
				if err := os.Unsetenv("PI_EXPERIMENTAL"); err != nil {
					t.Fatal(err)
				}
			}
			if got := AreExperimentalFeaturesEnabled(); got != tc.want {
				t.Fatalf("AreExperimentalFeaturesEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
