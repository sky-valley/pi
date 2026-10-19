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

// createOutputFile opens a new output file for streamed output (pi
// createOutputFileStream). The file is created exclusively, as pi's "wx"
// flag does, so it never follows a link someone else placed at the path. pi
// reports a failed open on the stream; here it is the error, with the path
// the file would have had.
func createOutputFile(prefix, extension string) (path string, file *os.File, err error) {
	path = createOutputFilePath(prefix, extension)
	file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, outputFileMode)
	return path, file, err
}
