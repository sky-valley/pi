package daemon

// Terminals are not supported on Windows yet.

// terminal is never registered on Windows; ptyWrite, ptyResize and
// ptyForeground find no terminal and fail with EBADF.
type terminal struct {
	input *inputQueue
	pid   int
}

func (t *terminal) setSize(uint16, uint16) error { return nil }

func (t *terminal) foreground() (int, bool, error) { return 0, false, nil }

func runPty(uint32, ptyRequest, *output, *control, *groups, *terminals) (object, *failure) {
	return nil, newFailure("ENOTSUP", "Terminals are not supported on this system yet")
}
