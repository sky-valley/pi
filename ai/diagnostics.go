package ai

// DiagnosticErrorInfo is the redacted error attached to a Diagnostic. It mirrors
// pi's DiagnosticErrorInfo (utils/diagnostics.ts). Only Message is required.
type DiagnosticErrorInfo struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	// Code is a string or number (pi: string | number). Kept as any so both
	// shapes round-trip; omitted when nil.
	Code any `json:"code,omitempty"`
}

// Diagnostic is a provider/runtime diagnostic attached to an AssistantMessage.
// It is the Go analogue of pi's AssistantMessageDiagnostic (utils/diagnostics.ts):
// {type, timestamp, error?, details?}.
type Diagnostic struct {
	Type      string               `json:"type"`
	Timestamp int64                `json:"timestamp"`
	Error     *DiagnosticErrorInfo `json:"error,omitempty"`
	Details   map[string]any       `json:"details,omitempty"`
}
