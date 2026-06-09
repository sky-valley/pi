package agent

import (
	"fmt"
	"time"
)

func nowMillis() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}

// panicMessage renders a recovered panic value into a message string, mirroring
// pi's `error instanceof Error ? error.message : String(error)`. An `error`
// value yields its message; anything else is formatted with %v.
func panicMessage(r any) string {
	if err, ok := r.(error); ok {
		return err.Error()
	}
	return fmt.Sprintf("%v", r)
}
