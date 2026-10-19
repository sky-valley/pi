package daemon

import "testing"

func makeFIFO(t *testing.T, _ string) { t.Skip("no FIFOs on Windows") }
