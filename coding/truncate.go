// Package coding is the Go port of pi's coding agent (@earendil-works/pi-coding-agent):
// the built-in tools (read/write/edit/bash/ls/find/grep), system prompt, session
// runner, and CLI plumbing built on the agent and ai packages.
package coding

import (
	"fmt"
	"strings"
)

const (
	DefaultMaxLines   = 2000
	DefaultMaxBytes   = 50 * 1024 // 50KB
	GrepMaxLineLength = 500
	lsDefaultLimit    = 500
	findDefaultLimit  = 1000
	grepDefaultLimit  = 100
	// maxInt is used to disable the line limit (byte cap only), matching pi's
	// truncateHead({ maxLines: Number.MAX_SAFE_INTEGER }) for ls/find/grep.
	maxInt = int(^uint(0) >> 1)
)

// TruncationResult describes the outcome of a truncation operation. The JSON
// field names match pi's TruncationResult shape (truncate.ts) so it can be
// embedded in tool details payloads.
type TruncationResult struct {
	Content               string `json:"content"`
	Truncated             bool   `json:"truncated"`
	TruncatedBy           string `json:"truncatedBy"` // "lines" | "bytes" | "" (pi: null)
	TotalLines            int    `json:"totalLines"`
	TotalBytes            int    `json:"totalBytes"`
	OutputLines           int    `json:"outputLines"`
	OutputBytes           int    `json:"outputBytes"`
	LastLinePartial       bool   `json:"lastLinePartial"`
	FirstLineExceedsLimit bool   `json:"firstLineExceedsLimit"`
	MaxLines              int    `json:"maxLines"`
	MaxBytes              int    `json:"maxBytes"`
}

// FormatSize renders a byte count as a human-readable size.
func FormatSize(bytes int) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}

func splitLinesForCounting(content string) []string {
	if len(content) == 0 {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// TruncateHead keeps the first N lines/bytes (for file reads).
func TruncateHead(content string, maxLines, maxBytes int) TruncationResult {
	if maxLines == 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return TruncationResult{Content: content, TotalLines: totalLines, TotalBytes: totalBytes,
			OutputLines: totalLines, OutputBytes: totalBytes, MaxLines: maxLines, MaxBytes: maxBytes}
	}
	if len(lines) > 0 && len(lines[0]) > maxBytes {
		return TruncationResult{Content: "", Truncated: true, TruncatedBy: "bytes", TotalLines: totalLines,
			TotalBytes: totalBytes, FirstLineExceedsLimit: true, MaxLines: maxLines, MaxBytes: maxBytes}
	}

	var out []string
	outBytes := 0
	truncatedBy := "lines"
	for i := 0; i < len(lines) && i < maxLines; i++ {
		lineBytes := len(lines[i])
		if i > 0 {
			lineBytes++
		}
		if outBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			break
		}
		out = append(out, lines[i])
		outBytes += lineBytes
	}
	if len(out) >= maxLines && outBytes <= maxBytes {
		truncatedBy = "lines"
	}
	outputContent := strings.Join(out, "\n")
	return TruncationResult{Content: outputContent, Truncated: true, TruncatedBy: truncatedBy,
		TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: len(out), OutputBytes: len(outputContent),
		MaxLines: maxLines, MaxBytes: maxBytes}
}

// TruncateTail keeps the last N lines/bytes (for command output).
func TruncateTail(content string, maxLines, maxBytes int) TruncationResult {
	if maxLines == 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return TruncationResult{Content: content, TotalLines: totalLines, TotalBytes: totalBytes,
			OutputLines: totalLines, OutputBytes: totalBytes, MaxLines: maxLines, MaxBytes: maxBytes}
	}

	var out []string
	outBytes := 0
	truncatedBy := "lines"
	lastLinePartial := false
	for i := len(lines) - 1; i >= 0 && len(out) < maxLines; i-- {
		lineBytes := len(lines[i])
		if len(out) > 0 {
			lineBytes++
		}
		if outBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			if len(out) == 0 {
				truncated := truncateStringToBytesFromEnd(lines[i], maxBytes)
				out = append([]string{truncated}, out...)
				outBytes = len(truncated)
				lastLinePartial = true
			}
			break
		}
		out = append([]string{lines[i]}, out...)
		outBytes += lineBytes
	}
	if len(out) >= maxLines && outBytes <= maxBytes {
		truncatedBy = "lines"
	}
	outputContent := strings.Join(out, "\n")
	return TruncationResult{Content: outputContent, Truncated: true, TruncatedBy: truncatedBy,
		TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: len(out), OutputBytes: len(outputContent),
		LastLinePartial: lastLinePartial, MaxLines: maxLines, MaxBytes: maxBytes}
}

func truncateStringToBytesFromEnd(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && (s[start]&0xc0) == 0x80 {
		start++
	}
	return s[start:]
}

// TruncateLine truncates a single line to maxChars, appending a marker.
// maxChars counts UTF-16 code units like pi's `line.length`/`line.slice`
// (astral characters count as 2). A slice that would split a surrogate pair
// yields a lone high surrogate in JS, which serializes as U+FFFD.
func TruncateLine(line string, maxChars int) (string, bool) {
	if maxChars == 0 {
		maxChars = GrepMaxLineLength
	}
	if utf16Len(line) <= maxChars {
		return line, false
	}
	var b strings.Builder
	n := 0
	for _, r := range line {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if n+units > maxChars {
			if units == 2 && n+1 == maxChars {
				// JS slice cuts mid-pair, leaving a lone high surrogate.
				b.WriteRune('�')
			}
			break
		}
		b.WriteRune(r)
		n += units
		if n == maxChars {
			break
		}
	}
	return b.String() + "... [truncated]", true
}
