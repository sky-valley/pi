package providers

import "github.com/sky-valley/pi/ai"

// The built-in API providers register themselves when this package is
// imported, mirroring pi, where importing @earendil-works/pi-ai/compat runs
// registerBuiltInApiProviders() as a module-load side effect (compat.ts). The
// coding package imports this package, so SDK consumers get a populated
// registry without any manual wiring.
func init() {
	RegisterBuiltins()
}

// RegisterBuiltins is pi's registerBuiltInApiProviders: it registers each
// built-in api implementation whose api has no provider yet, without
// replacing one a host registered, and records what the registry holds for
// those apis as the built-ins compat's Stream routes through (see
// ai.RegisterBuiltInApiProviders). It runs via init() when this package is
// imported; running it again, after the registry changed, records the
// registry anew, as pi's does.
func RegisterBuiltins() {
	ai.RegisterBuiltInApiProviders(
		anthropicApiProvider(),
		openAICompletionsApiProvider(),
		openAIResponsesApiProvider(),
		googleApiProvider(),
		piMessagesApiProvider(),
	)
}
