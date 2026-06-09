package ai

import (
	"encoding/json"
	"testing"
)

// TestDiagnosticMarshalMatchesPi asserts the JSON shape matches pi's
// AssistantMessageDiagnostic (utils/diagnostics.ts): {type, timestamp, error?, details?}
// with error = {name?, message, stack?, code?}.
func TestDiagnosticMarshalMatchesPi(t *testing.T) {
	d := Diagnostic{
		Type:      "stream_error",
		Timestamp: 1717000000000,
		Error: &DiagnosticErrorInfo{
			Name:    "TypeError",
			Message: "boom",
			Stack:   "at foo",
			Code:    "ECONN",
		},
		Details: map[string]any{"attempt": float64(2)},
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"stream_error","timestamp":1717000000000,"error":{"name":"TypeError","message":"boom","stack":"at foo","code":"ECONN"},"details":{"attempt":2}}`
	if string(raw) != want {
		t.Fatalf("diagnostic JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestDiagnosticMarshalMinimal asserts optional fields drop and message is always
// present, matching pi's optional error/details and required message.
func TestDiagnosticMarshalMinimal(t *testing.T) {
	d := Diagnostic{
		Type:      "retry",
		Timestamp: 100,
		Error:     &DiagnosticErrorInfo{Message: "x"},
	}
	raw, _ := json.Marshal(d)
	want := `{"type":"retry","timestamp":100,"error":{"message":"x"}}`
	if string(raw) != want {
		t.Fatalf("minimal diagnostic JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestDiagnosticErrorCodeNumber confirms code round-trips as a number too
// (pi: code?: string | number).
func TestDiagnosticErrorCodeNumber(t *testing.T) {
	d := Diagnostic{
		Type:      "http",
		Timestamp: 1,
		Error:     &DiagnosticErrorInfo{Message: "bad", Code: float64(429)},
	}
	raw, _ := json.Marshal(d)
	want := `{"type":"http","timestamp":1,"error":{"message":"bad","code":429}}`
	if string(raw) != want {
		t.Fatalf("numeric code JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}
