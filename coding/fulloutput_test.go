package coding

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// accumulatorSnapshot is what the oracle records of a display snapshot.
type accumulatorSnapshot struct {
	Content         string `json:"content"`
	Truncated       bool   `json:"truncated"`
	TruncatedBy     string `json:"truncatedBy"` // pi's null is ""
	TotalLines      int    `json:"totalLines"`
	TotalBytes      int    `json:"totalBytes"`
	OutputLines     int    `json:"outputLines"`
	OutputBytes     int    `json:"outputBytes"`
	LastLinePartial bool   `json:"lastLinePartial"`
	TempFile        bool   `json:"tempFile"`
	// TempFileBeforeFinish is whether a snapshot that does not persist names
	// a temp file before finish: the accumulator opens one as soon as the raw
	// bytes, the decoded bytes or the lines pass its limits.
	TempFileBeforeFinish bool `json:"tempFileBeforeFinish"`
}

// fullOutputRow is one row of testdata/fulloutput: what pi's OutputAccumulator
// made of the row's chunks (capture.mts).
type fullOutputRow struct {
	Name     string   `json:"name"`
	Chunks   []string `json:"chunks"`
	MaxLines int      `json:"maxLines"`
	MaxBytes int      `json:"maxBytes"`
	ReadMax  int      `json:"readMax"`
	// Unsaved rows run with the temp dir missing, so the file fails to open.
	Unsaved  bool                `json:"unsaved"`
	Snapshot accumulatorSnapshot `json:"snapshot"`
	// File is the temp file's kind after closeTempFile: "saved", "failed",
	// or "" for none (pi's null).
	File string `json:"file"`
	Full struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	} `json:"full"`
}

func loadFullOutputCapture(t *testing.T) (sha string, rows []fullOutputRow) {
	t.Helper()
	data, err := os.ReadFile("testdata/fulloutput/fulloutput-8209929fb.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		SHA  string          `json:"sha"`
		Rows []fullOutputRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) < 23 {
		t.Fatalf("only %d rows in the capture", len(capture.Rows))
	}
	return capture.SHA, capture.Rows
}

// replayRow appends a row's chunks to a fresh accumulator with the row's
// limits, in a temp dir that is missing for an unsaved row.
func replayRow(t *testing.T, row fullOutputRow) *outputAccumulator {
	t.Helper()
	tempDir := t.TempDir()
	if row.Unsaved {
		tempDir = filepath.Join(tempDir, "missing")
	}
	t.Setenv("TMPDIR", tempDir)
	acc := newOutputAccumulator(row.MaxLines, row.MaxBytes, "pi-capture")
	for _, chunk := range row.Chunks {
		b, err := hex.DecodeString(chunk)
		if err != nil {
			t.Fatal(err)
		}
		acc.append(b)
	}
	return acc
}

// TestOutputAccumulatorSnapshotMatchesPi replays each row's display snapshot,
// the text and truncation the model sees. pi decodes the stream with a
// TextDecoder as it arrives, so a byte-order mark before the first character
// is dropped, invalid bytes become U+FFFD per maximal subpart, and the limits
// count the decoded text's bytes, with the raw bytes also opening the temp
// file.
func TestOutputAccumulatorSnapshotMatchesPi(t *testing.T) {
	sha, rows := loadFullOutputCapture(t)
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			acc := replayRow(t, row)
			tempFileBeforeFinish := acc.snapshot(false).fullOutputFile != nil
			acc.finish()
			snap := acc.snapshot(true)
			acc.closeTempFile()
			tr := snap.truncation
			got := accumulatorSnapshot{
				Content: snap.content, Truncated: tr.Truncated, TruncatedBy: tr.TruncatedBy,
				TotalLines: tr.TotalLines, TotalBytes: tr.TotalBytes,
				OutputLines: tr.OutputLines, OutputBytes: tr.OutputBytes, LastLinePartial: tr.LastLinePartial,
				TempFile: snap.fullOutputFile != nil, TempFileBeforeFinish: tempFileBeforeFinish,
			}
			if got != row.Snapshot {
				t.Errorf("snapshot = %+v\n     pi at %s %+v", got, sha, row.Snapshot)
			}
			if file := fileKind(acc.snapshot(false).fullOutputFile); file != row.File {
				t.Errorf("file after closeTempFile = %q, pi at %s %q", file, sha, row.File)
			}
		})
	}
}

// fileKind is pi's OutputFileResult kind, "" for no file.
func fileKind(r *outputFileResult) string {
	switch {
	case r == nil:
		return ""
	case r.saved():
		return "saved"
	default:
		return "failed"
	}
}

// TestOutputAccumulatorReadFullOutputMatchesPi replays each row as bash.ts
// runs the accumulator: append, finish, snapshot (persistIfTruncated), close
// the temp file, then readFullOutput (upstream 1ff5b6fdd). An unsaved row
// reads the retained tail instead (upstream 943a99e3c).
func TestOutputAccumulatorReadFullOutputMatchesPi(t *testing.T) {
	sha, rows := loadFullOutputCapture(t)
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			acc := replayRow(t, row)
			acc.finish()
			acc.snapshot(true)
			acc.closeTempFile()
			content, truncated, err := acc.readFullOutput(row.ReadMax)
			if err != nil {
				t.Fatal(err)
			}
			if content != row.Full.Content || truncated != row.Full.Truncated {
				t.Errorf("readFullOutput(%d) = (%+q, %t), pi at %s (%+q, %t)", row.ReadMax, content, truncated, sha, row.Full.Content, row.Full.Truncated)
			}
		})
	}
}

// A temp file the output cannot be saved to no longer fails anything (upstream
// 943a99e3c): it shows up as failed, with the cause, and a file the accumulator
// created is removed, since a partial file would pass for the full output.
// pi's output-file-failures.test.ts 'createOutputFileStream' cases; the
// injected ENOSPC is a write to a closed file here.
func TestOutputFileStreamReportsFailures(t *testing.T) {
	t.Run("the file cannot be opened", func(t *testing.T) {
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
		file := createOutputFileStream("pi-test", ".log")
		path := file.path
		file.write([]byte("data"))
		if r := file.close(); r.saved() || !errors.Is(r.err, fs.ErrNotExist) || r.path != "" {
			t.Fatalf("close() = %+v, want failed with the open error (%v)", r, fs.ErrNotExist)
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s: %v", path, err)
		}
	})
	t.Run("a write fails", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		file := createOutputFileStream("pi-test", ".log")
		path := file.path
		file.write([]byte("data"))
		file.file.Close() // every later write fails
		file.write([]byte("more"))
		if r := file.result(); !errors.Is(r.err, os.ErrClosed) {
			t.Fatalf("result() = %+v, want failed with the write error (%v)", r, os.ErrClosed)
		}
		file.write([]byte("ignored"))
		if r := file.close(); !errors.Is(r.err, os.ErrClosed) {
			t.Fatalf("close() = %+v, want the write error", r)
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the partial file is still there: %v", err)
		}
	})
	// pi 8209929fb 'reports a failure while closing the file, not saved'
	// (delayed EDQUOT on quota and network file systems); here the descriptor
	// is closed underneath, so closing it fails.
	t.Run("closing fails", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		file := createOutputFileStream("pi-test", ".log")
		path := file.path
		file.write([]byte("data"))
		file.file.Close()
		if r := file.close(); !errors.Is(r.err, os.ErrClosed) {
			t.Fatalf("close() = %+v, want failed with the close error", r)
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the unsaved file is still there: %v", err)
		}
	})
	t.Run("a file it did not create stays", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "taken.log")
		if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		file := openOutputFileStream(path) // the exclusive create fails
		file.write([]byte("data"))
		if r := file.close(); !errors.Is(r.err, fs.ErrExist) {
			t.Fatalf("close() = %+v, want EEXIST", r)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "theirs" {
			t.Fatalf("a file the stream did not create changed: %q (%v)", data, err)
		}
	})
	t.Run("writing succeeds", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		file := createOutputFileStream("pi-test", ".log")
		file.write([]byte("data"))
		r := file.close()
		file.write([]byte("late")) // after close: ignored
		if !r.saved() || r.path != file.path || file.result() != r {
			t.Fatalf("close() = %+v, result() = %+v, want saved at %s", r, file.result(), file.path)
		}
		if data, err := os.ReadFile(r.path); err != nil || string(data) != "data" {
			t.Fatalf("file = %q (%v), want %q", data, err, "data")
		}
	})
}

// pi creates every output file readable only by the user and never through a
// link someone else placed at its path (utils/output-files.ts, upstream
// d677d0ee7): createWriteStream(path, {mode: 0o600, flags: "wx"}).
func TestOutputAccumulatorTempFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes carry no group or other permissions")
	}
	t.Setenv("TMPDIR", t.TempDir())
	acc := newOutputAccumulator(2, 1000, "pi-test")
	acc.append([]byte("1\n2\n3\n")) // three lines pass maxLines
	if acc.tempFile == nil || acc.tempFile.err != nil {
		t.Fatalf("precondition: three lines should open the temp file (%v)", acc.fullOutputFile())
	}
	defer acc.closeTempFile()
	info, err := os.Stat(acc.tempFile.path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("temp file mode = %o, want 600", mode)
	}
	if dir, base := filepath.Split(acc.tempFile.path); filepath.Clean(dir) != filepath.Clean(os.Getenv("TMPDIR")) ||
		!regexp.MustCompile(`^pi-test-[0-9a-f]{16}\.log$`).MatchString(base) {
		t.Fatalf("temp file path = %q, want <TMPDIR>/pi-test-<16 hex>.log", acc.tempFile.path)
	}
}
