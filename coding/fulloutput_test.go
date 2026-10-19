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
	Name     string              `json:"name"`
	Chunks   []string            `json:"chunks"`
	MaxLines int                 `json:"maxLines"`
	MaxBytes int                 `json:"maxBytes"`
	ReadMax  int                 `json:"readMax"`
	Snapshot accumulatorSnapshot `json:"snapshot"`
	Full     struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	} `json:"full"`
}

func loadFullOutputCapture(t *testing.T) (sha string, rows []fullOutputRow) {
	t.Helper()
	data, err := os.ReadFile("testdata/fulloutput/fulloutput-3dd803d7e.json")
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
	if len(capture.Rows) < 18 {
		t.Fatalf("only %d rows in the capture", len(capture.Rows))
	}
	return capture.SHA, capture.Rows
}

// replayRow appends a row's chunks to a fresh accumulator with the row's limits.
func replayRow(t *testing.T, row fullOutputRow) *outputAccumulator {
	t.Helper()
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
	t.Setenv("TMPDIR", t.TempDir())
	sha, rows := loadFullOutputCapture(t)
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			acc := replayRow(t, row)
			tempFileBeforeFinish := acc.snapshot(false).fullOutputPath != ""
			acc.finish()
			snap := acc.snapshot(true)
			if err := acc.closeTempFile(); err != nil {
				t.Fatal(err)
			}
			tr := snap.truncation
			got := accumulatorSnapshot{
				Content: snap.content, Truncated: tr.Truncated, TruncatedBy: tr.TruncatedBy,
				TotalLines: tr.TotalLines, TotalBytes: tr.TotalBytes,
				OutputLines: tr.OutputLines, OutputBytes: tr.OutputBytes, LastLinePartial: tr.LastLinePartial,
				TempFile: snap.fullOutputPath != "", TempFileBeforeFinish: tempFileBeforeFinish,
			}
			if got != row.Snapshot {
				t.Errorf("snapshot = %+v\n     pi at %s %+v", got, sha, row.Snapshot)
			}
		})
	}
}

// TestOutputAccumulatorReadFullOutputMatchesPi replays each row as bash.ts
// runs the accumulator: append, finish, snapshot (persistIfTruncated), close
// the temp file, then readFullOutput (upstream 1ff5b6fdd).
func TestOutputAccumulatorReadFullOutputMatchesPi(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	sha, rows := loadFullOutputCapture(t)
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			acc := replayRow(t, row)
			acc.finish()
			acc.snapshot(true)
			if err := acc.closeTempFile(); err != nil {
				t.Fatal(err)
			}
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

// A temp file the output cannot be saved to fails the call where pi's
// closeTempFile rejects with its stream's error (D90). Nothing is written after
// the first error.
func TestOutputAccumulatorReportsTempFileErrors(t *testing.T) {
	t.Run("the file cannot be created", func(t *testing.T) {
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
		acc := newOutputAccumulator(2, 1000, "pi-test")
		acc.append([]byte("1\n2\n3\n")) // three lines pass maxLines
		acc.finish()
		acc.snapshot(true)
		if err := acc.closeTempFile(); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("closeTempFile() = %v, want the create error (%v)", err, fs.ErrNotExist)
		}
	})
	t.Run("a write fails", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		acc := newOutputAccumulator(2, 1000, "pi-test")
		acc.append([]byte("1\n2\n3\n"))
		if acc.tempFile == nil {
			t.Fatal("precondition: three lines should open the temp file")
		}
		acc.tempFile.Close() // every later write fails
		acc.append([]byte("4\n"))
		acc.finish()
		acc.snapshot(true)
		if err := acc.closeTempFile(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("closeTempFile() = %v, want the write error (%v)", err, os.ErrClosed)
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
	if acc.tempFile == nil {
		t.Fatalf("precondition: three lines should open the temp file (%v)", acc.tempFileErr)
	}
	defer acc.closeTempFile()
	info, err := os.Stat(acc.tempFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("temp file mode = %o, want 600", mode)
	}
	if dir, base := filepath.Split(acc.tempFilePath); filepath.Clean(dir) != filepath.Clean(os.Getenv("TMPDIR")) ||
		!regexp.MustCompile(`^pi-test-[0-9a-f]{16}\.log$`).MatchString(base) {
		t.Fatalf("temp file path = %q, want <TMPDIR>/pi-test-<16 hex>.log", acc.tempFilePath)
	}
}
