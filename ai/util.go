package ai

import "time"

// nowMillis returns the current Unix time in milliseconds.
func nowMillis() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}

// Clone returns a deep copy of an AssistantMessage, used when emitting partial
// snapshots so downstream consumers don't observe later mutations.
func (m *AssistantMessage) Clone() *AssistantMessage {
	if m == nil {
		return nil
	}
	cp := *m
	cp.Content = append(ContentList(nil), m.Content...)
	if m.Diagnostics != nil {
		cp.Diagnostics = append([]Diagnostic(nil), m.Diagnostics...)
	}
	return &cp
}
