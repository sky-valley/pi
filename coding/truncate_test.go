package coding

import (
	"strings"
	"testing"
)

// TestTruncateTailOverLongLineFollowedByBlankLines pins TruncateTail's
// edge case from upstream 9e9609216 directly: an over-long line followed only
// by blank lines keeps its end (lastLinePartial), within the bytes the blank
// lines leave. Every expected value was produced by pi's truncateTail at
// 9e9609216 (packages/coding-agent/src/core/tools/truncate.ts) with
// maxLines 100 and maxBytes 20; each case is truncated by bytes and none has
// firstLineExceedsLimit.
func TestTruncateTailOverLongLineFollowedByBlankLines(t *testing.T) {
	long := strings.Repeat("y", 30) + "END"
	cases := []struct {
		name, input, content                             string
		totalLines, totalBytes, outputLines, outputBytes int
		lastLinePartial                                  bool
	}{
		{"one blank line", long + "\n\n", strings.Repeat("y", 16) + "END\n", 2, 35, 2, 20, true},
		{"three blank lines", long + "\n\n\n\n", strings.Repeat("y", 14) + "END\n\n\n", 4, 37, 4, 20, true},
		{"whitespace-only line", long + "\n  \t\n", strings.Repeat("y", 13) + "END\n  \t", 2, 38, 2, 20, true},
		{"a non-blank line after it", long + "\nok\n", "ok", 2, 37, 1, 2, false},
		{"blank lines fill the budget", long + "\n" + strings.Repeat("\n", 25), strings.Repeat("\n", 20), 26, 59, 21, 20, false},
		{"one byte left", long + strings.Repeat("\n", 20), "D" + strings.Repeat("\n", 19), 20, 53, 20, 20, true},
		{"the long line is blank", strings.Repeat(" ", 30) + "\n\n", "", 2, 32, 1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateTail(tc.input, 100, 20)
			if got.Content != tc.content || !got.Truncated || got.TruncatedBy != "bytes" ||
				got.TotalLines != tc.totalLines || got.TotalBytes != tc.totalBytes ||
				got.OutputLines != tc.outputLines || got.OutputBytes != tc.outputBytes ||
				got.LastLinePartial != tc.lastLinePartial || got.FirstLineExceedsLimit {
				t.Fatalf("TruncateTail = %+v\nwant content %q, lines %d/%d, bytes %d/%d, lastLinePartial %v",
					got, tc.content, tc.outputLines, tc.totalLines, tc.outputBytes, tc.totalBytes, tc.lastLinePartial)
			}
		})
	}
}
