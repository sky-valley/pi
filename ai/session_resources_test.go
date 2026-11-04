package ai

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// CleanupSessionResources runs every registered cleanup in registration order
// with the session id ("" for every session), keeps going past a failing one,
// and reports the failures together (pi cleanupSessionResources'
// AggregateError). An unregistered cleanup no longer runs.
func TestCleanupSessionResources(t *testing.T) {
	var calls []string
	first := RegisterSessionResourceCleanup(func(id string) error {
		calls = append(calls, "first:"+id)
		return errors.New("first failed")
	})
	defer first()
	second := RegisterSessionResourceCleanup(func(id string) error {
		calls = append(calls, "second:"+id)
		return nil
	})
	third := RegisterSessionResourceCleanup(func(id string) error {
		calls = append(calls, "third:"+id)
		return errors.New("third failed")
	})
	defer third()

	err := CleanupSessionResources("s1")
	if err == nil || !strings.HasPrefix(err.Error(), "Failed to cleanup session resources") ||
		!strings.Contains(err.Error(), "first failed") || !strings.Contains(err.Error(), "third failed") {
		t.Errorf("error %v", err)
	}
	second()
	second()                        // unregistering twice is harmless
	_ = CleanupSessionResources("") // the two failures are asserted above
	want := []string{"first:s1", "second:s1", "third:s1", "first:", "third:"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls %q, want %q", calls, want)
	}
}

func TestCleanupSessionResourcesWithoutFailures(t *testing.T) {
	unregister := RegisterSessionResourceCleanup(func(string) error { return nil })
	defer unregister()
	if err := CleanupSessionResources("s"); err != nil {
		t.Errorf("error %v", err)
	}
}
