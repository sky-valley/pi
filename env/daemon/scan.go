package daemon

import "bytes"

// lineScanner is Durable's LineScanner (env/line-scan.ts): it locates lines
// [startLine, endLine) of a file fed in order and measures their decoded
// UTF-8 size as `new TextDecoder().decode(file)` would produce it.
type lineScanner struct {
	startLine      uint64
	endLine        uint64
	hasEnd         bool
	position       uint64
	newlines       uint64
	lineStart      uint64
	start          *uint64
	end            *uint64
	firstLineEnd   *uint64
	lastLineStart  *uint64
	selectedBytes  uint64
	firstLineBytes uint64
	selection      *decoder
	firstLine      *decoder
	// head holds the first bytes until it is known whether they are a
	// byte-order mark; nil once released.
	head []byte
	bom  bool
}

func ptr(v uint64) *uint64 { return &v }

// newLineScanner scans lines [startLine, endLine); endLine, when hasEnd, must
// be greater than startLine.
func newLineScanner(startLine, endLine uint64, hasEnd bool) *lineScanner {
	s := &lineScanner{startLine: startLine, endLine: endLine, hasEnd: hasEnd, head: make([]byte, 0, 3)}
	if startLine == 0 {
		s.begin(0)
	}
	return s
}

func (s *lineScanner) isLastLine(line uint64) bool {
	return s.hasEnd && line+1 == s.endLine
}

func (s *lineScanner) push(chunk []byte) {
	if s.head != nil {
		take := min(3-len(s.head), len(chunk))
		s.head = append(s.head, chunk[:take]...)
		if len(s.head) < 3 {
			return
		}
		chunk = chunk[take:]
		s.releaseHead()
	}
	s.process(chunk)
}

func (s *lineScanner) releaseHead() {
	head := s.head
	s.head = nil
	s.bom = startsWithBOM(head)
	s.process(head)
}

func (s *lineScanner) process(chunk []byte) {
	base := s.position
	from := 0
	index := 0
	for {
		found := bytes.IndexByte(chunk[index:], '\n')
		if found < 0 {
			break
		}
		newline := index + found
		// The newline ends line s.newlines; it belongs to the selection between
		// selected lines only.
		s.feed(chunk, base, from, newline)
		line := s.newlines
		position := base + uint64(newline)
		if line == s.startLine {
			s.endFirstLine(position)
		}
		if s.isLastLine(line) {
			s.endSelection(position)
		}
		s.feed(chunk, base, newline, newline+1)
		from = newline + 1
		s.newlines++
		s.lineStart = position + 1
		if s.newlines == s.startLine {
			s.begin(s.lineStart)
		}
		if s.isLastLine(s.newlines) {
			s.lastLineStart = ptr(s.lineStart)
		}
		index = newline + 1
	}
	s.feed(chunk, base, from, len(chunk))
	s.position += uint64(len(chunk))
}

// finish is the LineScan result: where the selection starts and ends, where
// its first line ends and its last line starts, and decoded sizes.
func (s *lineScanner) finish() object {
	if s.head != nil {
		s.releaseHead()
	}
	size := s.position
	if s.start == nil {
		return object{
			"newlines": s.newlines, "start": size, "end": size, "firstLineEnd": size,
			"lastLineStart": size, "selectedBytes": uint64(0), "firstLineBytes": uint64(0),
		}
	}
	if s.firstLineEnd == nil {
		s.endFirstLine(size)
	}
	if s.end == nil {
		s.endSelection(size)
	}
	// A selection that reaches past the last line ends with the last line.
	lastLineStart := s.lineStart
	if s.lastLineStart != nil {
		lastLineStart = *s.lastLineStart
	}
	return object{
		"newlines":       s.newlines,
		"start":          *s.start,
		"end":            *s.end,
		"firstLineEnd":   *s.firstLineEnd,
		"lastLineStart":  lastLineStart,
		"selectedBytes":  s.selectedBytes,
		"firstLineBytes": s.firstLineBytes,
	}
}

func (s *lineScanner) begin(start uint64) {
	s.start = ptr(start)
	if s.isLastLine(s.startLine) {
		s.lastLineStart = ptr(start)
	}
	s.selection = newRangeDecoder()
	s.firstLine = newRangeDecoder()
}

func (s *lineScanner) endFirstLine(position uint64) {
	s.firstLineEnd = ptr(position)
	if s.firstLine != nil {
		s.firstLineBytes += uint64(len(s.firstLine.decode(nil, true)))
		s.firstLine = nil
	}
}

func (s *lineScanner) endSelection(position uint64) {
	s.end = ptr(position)
	if s.selection != nil {
		s.selectedBytes += uint64(len(s.selection.decode(nil, true)))
		s.selection = nil
	}
}

func (s *lineScanner) feed(chunk []byte, base uint64, from, to int) {
	// Decoding the whole file drops a leading byte-order mark.
	if s.bom && base+uint64(from) < 3 {
		from = min(to, int(3-base))
	}
	if to <= from {
		return
	}
	b := chunk[from:to]
	if s.selection != nil {
		s.selectedBytes += uint64(len(s.selection.decode(b, false)))
	}
	if s.firstLine != nil {
		s.firstLineBytes += uint64(len(s.firstLine.decode(b, false)))
	}
}
