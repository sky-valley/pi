package coding

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// The read tool's line selection (pi core/tools/read.ts, upstream
// 3a7bb6f48): from a whole file in memory, or from a file read in chunks
// when the operations can stream it, keeping only the lines that can be
// shown. Both give the same result for the same bytes.

// selectedLines are the lines a read shows, with the file's line count; line
// counts follow strings.Split(text, "\n"), pi's text.split("\n").
type selectedLines struct {
	totalFileLines int
	truncation     TruncationResult
	// firstLineBytes is the size of the first selected line, for the notice
	// when it alone exceeds the byte limit.
	firstLineBytes int
}

// readRange is the read tool's offset/limit: startLine is 0-based, and limit
// is only meaningful when hasLimit is set.
type readRange struct {
	startLine int
	limit     int
	hasLimit  bool
}

// isStreamableRange is pi's isStreamableRange: an integer offset and a
// non-negative integer limit, each when given. Other values keep the
// whole-file path, whose slicing defines what they select (a limit of -1 as
// all lines but the last).
func isStreamableRange(params map[string]any) bool {
	isInteger := func(key string) (float64, bool) {
		if _, present := params[key]; !present {
			return 0, true
		}
		v, ok := argFloat(params, key)
		return v, ok && v == math.Trunc(v)
	}
	_, offsetOK := isInteger("offset")
	limit, limitOK := isInteger("limit")
	return offsetOK && limitOK && limit >= 0
}

// selectLinesFromBuffer selects lines from a whole file in memory: for
// operations that cannot stream and for unusual ranges.
func selectLinesFromBuffer(data []byte, r readRange) selectedLines {
	allLines := strings.Split(jstext.DecodeUTF8(data), "\n") // Buffer.toString("utf-8")
	var selected string
	if r.hasLimit {
		// pi: allLines.slice(startLine, Math.min(startLine + limit, length))
		// with JS slice semantics: a negative end counts back from the array
		// end, and an end before the start yields an empty slice.
		end := min(r.startLine+r.limit, len(allLines))
		if end < 0 {
			end = max(len(allLines)+end, 0)
		}
		start := min(r.startLine, len(allLines))
		end = max(end, start)
		selected = strings.Join(allLines[start:end], "\n")
	} else if r.startLine < len(allLines) {
		selected = strings.Join(allLines[r.startLine:], "\n")
	}
	truncation := TruncateHead(selected, 0, 0)
	firstLineBytes := 0
	if truncation.FirstLineExceedsLimit {
		firstLineBytes = len(allLines[r.startLine])
	}
	return selectedLines{totalFileLines: len(allLines), truncation: truncation, firstLineBytes: firstLineBytes}
}

// errReadAborted is the read tool's error when its context ends mid-read
// (pi: "Operation aborted").
var errReadAborted = errors.New("Operation aborted")

// streamChunkSize is the size of each read from a stream.
const streamChunkSize = 64 * 1024

// selectLinesFromStream selects lines from a file read in chunks. Only lines
// TruncateHead can show are stored; the rest are counted, so memory does not
// grow with the file. It stops reading when ctx ends.
func selectLinesFromStream(ctx context.Context, stream io.Reader, r readRange) (selectedLines, error) {
	endLine := maxInt
	if r.hasLimit {
		endLine = r.startLine + r.limit
	}
	// Non-fatal UTF-8 that keeps a byte-order mark decodes like
	// Buffer.toString("utf-8"), the whole-file path's decode.
	decoder := jstext.TextDecoder{IgnoreBOM: true}
	var (
		stored      []string
		storedBytes int
		pastCaps    bool
		line        int
		// selected counts the selected lines, selectedBytes the size of
		// their text joined by "\n".
		selected      int
		selectedBytes int
		firstBytes    int
		lastEmpty     = true
		// The selected line being read: its text while it is stored (nil
		// once storing has stopped), its size, and whether it has any.
		piece    = &strings.Builder{}
		lineSize int
		hasText  bool
	)
	appendToLine := func(text string) {
		hasText = true
		if piece == nil {
			return
		}
		// A line over the byte cap is never shown, and a first line over it
		// is only measured: never store all of a 1 GB line.
		if lineSize <= DefaultMaxBytes {
			piece.WriteString(text)
		}
		lineSize += len(text)
	}
	finishLine := func() {
		if piece != nil {
			if selected == 0 {
				firstBytes = lineSize
			}
			// Storing continues one line past the caps: TruncateHead ignores
			// a trailing empty line, so the line after the cap must be there
			// for it to decide truncation as for the full text.
			if len(stored) > DefaultMaxLines || storedBytes > DefaultMaxBytes {
				pastCaps = true
			}
			text := piece.String()
			if len(stored) > 0 {
				storedBytes++
			}
			storedBytes += len(text)
			stored = append(stored, text)
			piece = nil
			if !pastCaps {
				piece = &strings.Builder{}
			}
		}
		selected++
		lastEmpty = !hasText
		lineSize, hasText = 0, false
	}
	scan := func(text string) {
		for {
			inRange := line >= r.startLine && line < endLine
			newline := strings.IndexByte(text, '\n')
			end := len(text)
			if newline >= 0 {
				end = newline
			}
			if inRange {
				if end > 0 {
					appendToLine(text[:end])
				}
				selectedBytes += end
			}
			if newline < 0 {
				return
			}
			if inRange {
				finishLine()
				if line+1 < endLine {
					selectedBytes++ // the newline joining it to the next selected line
				}
			}
			line++
			text = text[newline+1:]
		}
	}

	buf := make([]byte, streamChunkSize)
	for {
		if ctx.Err() != nil {
			return selectedLines{}, errReadAborted
		}
		n, err := stream.Read(buf)
		if n > 0 {
			scan(decoder.Decode(buf[:n]))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return selectedLines{}, err
		}
	}
	scan(decoder.Flush())
	if line >= r.startLine && line < endLine {
		finishLine() // the file's last line, which no newline ends
	}
	truncation := TruncateHead(strings.Join(stored, "\n"), 0, 0)
	// TruncateHead's counts for the joined selected lines, which drop a
	// trailing empty line.
	truncation.TotalBytes = selectedBytes
	truncation.TotalLines = 0
	if selectedBytes > 0 {
		truncation.TotalLines = selected
		if lastEmpty {
			truncation.TotalLines--
		}
	}
	return selectedLines{totalFileLines: line + 1, truncation: truncation, firstLineBytes: firstBytes}, nil
}

// openLocalReadStream streams a local file the way pi's readLocalFileChunks
// does: a regular file up to its size when opened, so text appended during
// the read is not counted and a growing file still ends; a file reporting
// size 0 (such as /proc files) and other file types until EOF. A directory
// fails as the whole-file read does.
func openLocalReadStream(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, errors.New("EISDIR: illegal operation on a directory, read")
	}
	if st.Mode().IsRegular() && st.Size() > 0 {
		return limitedFile{io.LimitReader(f, st.Size()), f}, nil
	}
	return f, nil
}

// limitedFile reads a prefix of a file and closes the file.
type limitedFile struct {
	io.Reader
	io.Closer
}
