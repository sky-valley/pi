package coding

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// Output files, ported from pi utils/output-files.ts (upstream d677d0ee7):
// files pi writes so the model can use output it was not shown in full, such
// as the full text of truncated tool output. Every output file is created
// here, so where they are stored can change in one place; today they go to
// the OS temp directory. pi's writeOutputFile, whose consumers are codemode's
// saved images and MCP's binary resources, lands with those (Scope rows 22
// and 21).

// outputFileMode keeps output files readable only by the user: output can
// carry private data.
const outputFileMode = 0o600

// createOutputFilePath is a new, unused path:
// <temp dir>/<prefix>-<16 hex chars><extension>, where extension includes the
// dot.
func createOutputFilePath(prefix, extension string) string {
	var id [8]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read never returns an error (Go 1.24)
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%x%s", prefix, id, extension))
}

// outputFileResult is where streamed output ended up (pi OutputFileResult,
// upstream 943a99e3c): a complete file at path, or, when err is set, why it
// could not be saved. A failed result carries no path.
type outputFileResult struct {
	path string
	err  error
}

func (r outputFileResult) saved() bool { return r.err == nil }

// outputFileStream is an output file written while output streams in (pi
// OutputFileStream). Open, write and close failures (full disk, quota,
// unwritable temp dir) never fail the caller: the first one drops the file
// and is reported by result and close.
type outputFileStream struct {
	path string
	// file is nil once the stream failed or closing started; writes after
	// that are ignored.
	file *os.File
	err  error
}

// createOutputFileStream opens a new output file for streamed output (pi
// createOutputFileStream). The file is created exclusively, as pi's "wx"
// flag does, so it never follows a link someone else placed at the path.
func createOutputFileStream(prefix, extension string) *outputFileStream {
	return openOutputFileStream(createOutputFilePath(prefix, extension))
}

func openOutputFileStream(path string) *outputFileStream {
	s := &outputFileStream{path: path}
	// A failed open leaves the path alone: whatever is there is not ours.
	s.file, s.err = os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, outputFileMode)
	return s
}

// result is saved while writing has not failed; the file is complete only
// once close returns.
func (s *outputFileStream) result() outputFileResult {
	if s.err != nil {
		return outputFileResult{err: s.err}
	}
	return outputFileResult{path: s.path}
}

func (s *outputFileStream) write(data []byte) {
	if s.file == nil {
		return
	}
	if _, err := s.file.Write(data); err != nil {
		s.fail(err)
	}
}

// close flushes and closes the file. A failure closing it counts as a failure
// to save it.
func (s *outputFileStream) close() outputFileResult {
	if f := s.file; f != nil {
		s.file = nil
		if err := f.Close(); err != nil {
			s.err = err
			_ = os.Remove(s.path)
		}
	}
	return s.result()
}

// fail drops the file after a write error: a partial file would pass for the
// full output.
func (s *outputFileStream) fail(err error) {
	s.err = err
	_ = s.file.Close()
	s.file = nil
	_ = os.Remove(s.path)
}
