package providers

// RegisterBuiltins registers all built-in real API providers. Call once at
// startup before streaming.
func RegisterBuiltins() {
	RegisterAnthropic()
	RegisterOpenAICompletions()
	RegisterOpenAIResponses()
	RegisterGoogle()
}
