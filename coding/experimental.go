package coding

import "os"

// AreExperimentalFeaturesEnabled ports pi's areExperimentalFeaturesEnabled
// (core/experimental.ts, upstream 66335d3a): the guard that lets users opt in
// to early features. It is true only when the PI_EXPERIMENTAL environment
// variable is exactly "1" — unset, empty, "0", "true", or any other value all
// leave experimental features disabled.
func AreExperimentalFeaturesEnabled() bool {
	return os.Getenv("PI_EXPERIMENTAL") == "1"
}
