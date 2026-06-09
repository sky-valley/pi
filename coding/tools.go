package coding

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"golang.org/x/text/unicode/norm"
)

// unicodeSpaces matches the unicode space variants pi folds to a regular space
// (paths.ts UNICODE_SPACES: U+00A0, U+2000–U+200A, U+202F, U+205F, U+3000).
var unicodeSpaces = regexp.MustCompile("[  -   　]")

// normalizePath ports pi's normalizePath with normalizeUnicodeSpaces + stripAtPrefix
// + tilde expansion (paths.ts). It does not trim by default.
func normalizePath(input string) string {
	normalized := unicodeSpaces.ReplaceAllString(input, " ")
	if strings.HasPrefix(normalized, "@") {
		normalized = normalized[1:]
	}
	// expandTilde (default true).
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if normalized == "~" {
			return home
		}
		if strings.HasPrefix(normalized, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(normalized, "~\\")) {
			return filepath.Join(home, normalized[2:])
		}
	}
	if strings.HasPrefix(normalized, "file://") {
		if u, err := url.Parse(normalized); err == nil {
			return u.Path
		}
	}
	return normalized
}

// resolveToCwd resolves a possibly-relative path against cwd, porting pi's
// resolvePath (normalizeUnicodeSpaces + stripAtPrefix + tilde + file:// expansion).
func resolveToCwd(path, cwd string) string {
	normalized := normalizePath(path)
	if filepath.IsAbs(normalized) {
		return filepath.Clean(normalized)
	}
	base := cwd
	// pi normalizes baseDir too (no @/unicode opts beyond the defaults).
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if base == "~" {
			base = home
		} else if strings.HasPrefix(base, "~/") {
			base = filepath.Join(home, base[2:])
		}
	}
	return filepath.Clean(filepath.Join(base, normalized))
}

const narrowNoBreakSpace = " "

func pathExistsFS(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// resolveReadPath resolves a path and tries pi's macOS filename fallbacks
// (path-utils.ts resolveReadPathAsync): narrow no-break space before AM/PM, NFD,
// curly quote, and combined NFD+curly variants.
func resolveReadPath(path, cwd string) string {
	resolved := resolveToCwd(path, cwd)
	if pathExistsFS(resolved) {
		return resolved
	}
	// macOS screenshot AM/PM variant: " AM."/" PM." → narrow-no-break-space variant.
	if v := macAMPMVariant(resolved); v != resolved && pathExistsFS(v) {
		return v
	}
	// NFD variant (macOS stores filenames decomposed).
	nfd := norm.NFD.String(resolved)
	if nfd != resolved && pathExistsFS(nfd) {
		return nfd
	}
	// Curly quote variant (U+2019 instead of straight apostrophe).
	if v := strings.ReplaceAll(resolved, "'", "’"); v != resolved && pathExistsFS(v) {
		return v
	}
	// Combined NFD + curly quote.
	if v := strings.ReplaceAll(nfd, "'", "’"); v != resolved && pathExistsFS(v) {
		return v
	}
	return resolved
}

var macAMPMRe = regexp.MustCompile(`(?i) (AM|PM)\.`)

func macAMPMVariant(p string) string {
	return macAMPMRe.ReplaceAllString(p, narrowNoBreakSpace+"$1.")
}

// utf16Len returns the number of UTF-16 code units in s, matching JS String
// `.length` (astral characters count as 2). Used where pi reports `.length`.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func textResult(text string) agent.AgentToolResult {
	return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}, Details: map[string]any{}}
}

// ToolNames are the built-in coding tool identifiers.
var ToolNames = []string{"read", "bash", "edit", "write", "grep", "find", "ls"}

// ToolSnippets are the one-line prompt snippets keyed by tool name.
var ToolSnippets = map[string]string{
	"read":      "Read file contents",
	"bash":      "Execute bash commands (ls, grep, find, etc.)",
	"edit":      "Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
	"write":     "Create or overwrite files",
	"grep":      "Search file contents for patterns (respects .gitignore)",
	"find":      "Find files by glob pattern (respects .gitignore)",
	"ls":        "List directory contents",
	"web_fetch": "Fetch a web URL and return readable text",
}

// CreateTool builds a single built-in tool by name, rooted at cwd.
func CreateTool(name, cwd string) (agent.AgentTool, error) {
	switch name {
	case "read":
		return readTool(cwd), nil
	case "bash":
		return bashTool(cwd), nil
	case "edit":
		return editTool(cwd), nil
	case "write":
		return writeTool(cwd), nil
	case "grep":
		return grepTool(cwd), nil
	case "find":
		return findTool(cwd), nil
	case "ls":
		return lsTool(cwd), nil
	case "web_fetch":
		return webFetchTool(cwd), nil
	default:
		return agent.AgentTool{}, fmt.Errorf("Unknown tool name: %s", name)
	}
}

// CreateCodingTools returns the default coding tool set [read, bash, edit, write].
func CreateCodingTools(cwd string) []agent.AgentTool {
	return []agent.AgentTool{readTool(cwd), bashTool(cwd), editTool(cwd), writeTool(cwd)}
}

// CreateAllTools returns all seven built-in tools.
func CreateAllTools(cwd string) []agent.AgentTool {
	return []agent.AgentTool{
		readTool(cwd), bashTool(cwd), editTool(cwd), writeTool(cwd),
		grepTool(cwd), findTool(cwd), lsTool(cwd),
	}
}

func argStr(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

func argInt(params map[string]any, key string) (int, bool) {
	switch v := params[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func argBool(params map[string]any, key string) bool {
	b, _ := params[key].(bool)
	return b
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

const imageTypeSniffBytes = 4100

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// detectSupportedImageMimeType sniffs magic bytes to identify a supported image
// type (port of utils/mime.ts detectSupportedImageMimeType). Returns "" for
// CMYK JPEG (ffd8fff7), animated PNG (acTL), and non-IHDR PNG.
func detectSupportedImageMimeType(buf []byte) string {
	if bytesStartWith(buf, []byte{0xff, 0xd8, 0xff}) {
		if len(buf) > 3 && buf[3] == 0xf7 {
			return ""
		}
		return "image/jpeg"
	}
	if bytesStartWith(buf, pngSignature) {
		if isPNG(buf) && !isAnimatedPNG(buf) {
			return "image/png"
		}
		return ""
	}
	if startsWithAscii(buf, 0, "GIF") {
		return "image/gif"
	}
	if startsWithAscii(buf, 0, "RIFF") && startsWithAscii(buf, 8, "WEBP") {
		return "image/webp"
	}
	return ""
}

// detectSupportedImageMimeTypeFromFile reads up to the sniff window from a file
// and identifies a supported image type (mime.ts detectSupportedImageMimeTypeFromFile).
func detectSupportedImageMimeTypeFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, imageTypeSniffBytes)
	n, _ := f.Read(buf)
	return detectSupportedImageMimeType(buf[:n])
}

func bytesStartWith(buf, prefix []byte) bool {
	if len(buf) < len(prefix) {
		return false
	}
	for i := range prefix {
		if buf[i] != prefix[i] {
			return false
		}
	}
	return true
}

func startsWithAscii(buf []byte, offset int, text string) bool {
	if len(buf) < offset+len(text) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if buf[offset+i] != text[i] {
			return false
		}
	}
	return true
}

func readUint32BE(buf []byte, offset int) int {
	b := func(i int) int {
		if i < len(buf) {
			return int(buf[i])
		}
		return 0
	}
	return b(offset)*0x1000000 + (b(offset+1) << 16) + (b(offset+2) << 8) + b(offset+3)
}

func isPNG(buf []byte) bool {
	return len(buf) >= 16 && readUint32BE(buf, len(pngSignature)) == 13 && startsWithAscii(buf, 12, "IHDR")
}

func isAnimatedPNG(buf []byte) bool {
	offset := len(pngSignature)
	for offset+8 <= len(buf) {
		chunkLength := readUint32BE(buf, offset)
		chunkTypeOffset := offset + 4
		if startsWithAscii(buf, chunkTypeOffset, "acTL") {
			return true
		}
		if startsWithAscii(buf, chunkTypeOffset, "IDAT") {
			return false
		}
		nextOffset := offset + 8 + chunkLength + 4
		if nextOffset <= offset || nextOffset > len(buf) {
			return false
		}
		offset = nextOffset
	}
	return false
}

func readTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "read",
		Label:       "read",
		Description: fmt.Sprintf("Read the contents of a file. Supports text files and images (jpg, png, gif, webp). Images are sent as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.", DefaultMaxLines, DefaultMaxBytes/1024),
		Parameters: ai.Object(
			ai.Prop("path", ai.String("Path to the file to read (relative or absolute)")),
			ai.Opt("offset", ai.Integer("Line number to start reading from (1-indexed)")),
			ai.Opt("limit", ai.Integer("Maximum number of lines to read")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			path := argStr(params, "path")
			abs := resolveReadPath(path, cwd)
			info, err := os.Stat(abs)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if info.IsDir() {
				return agent.AgentToolResult{}, fmt.Errorf("%s is a directory, not a file", path)
			}
			if mime := detectSupportedImageMimeTypeFromFile(abs); mime != "" {
				data, err := os.ReadFile(abs)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				// Downscale/re-encode to fit the model's inline image limits and
				// apply EXIF orientation (port of pi's resizeImage). The note text
				// matches pi's read tool exactly (utils/image-resize.ts).
				resized, fit := resizeImage(data, mime)
				if !fit {
					return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{
						Text: fmt.Sprintf("Read image file [%s]\n[Image omitted: could not be resized below the inline image size limit.]", mime),
					}}, Details: map[string]any{}}, nil
				}
				note := fmt.Sprintf("Read image file [%s]", resized.MimeType)
				if dn := formatDimensionNote(resized); dn != "" {
					note += "\n" + dn
				}
				return agent.AgentToolResult{Content: ai.ContentList{
					ai.TextContent{Text: note},
					ai.ImageContent{Data: encodeBase64(resized.Data), MimeType: resized.MimeType},
				}, Details: map[string]any{}}, nil
			}

			data, err := os.ReadFile(abs)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			allLines := strings.Split(string(data), "\n")
			totalFileLines := len(allLines)
			offset, hasOffset := argInt(params, "offset")
			startLine := 0
			if hasOffset && offset > 0 {
				startLine = offset - 1
			}
			startLineDisplay := startLine + 1
			if startLine >= len(allLines) {
				return agent.AgentToolResult{}, fmt.Errorf("Offset %d is beyond end of file (%d lines total)", offset, len(allLines))
			}
			var selected string
			userLimitedLines := -1
			if limit, ok := argInt(params, "limit"); ok {
				end := startLine + limit
				if end > len(allLines) {
					end = len(allLines)
				}
				selected = strings.Join(allLines[startLine:end], "\n")
				userLimitedLines = end - startLine
			} else {
				selected = strings.Join(allLines[startLine:], "\n")
			}

			tr := TruncateHead(selected, 0, 0)
			var out string
			switch {
			case tr.FirstLineExceedsLimit:
				firstLineSize := FormatSize(len(allLines[startLine]))
				out = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
					startLineDisplay, firstLineSize, FormatSize(DefaultMaxBytes), startLineDisplay, path, DefaultMaxBytes)
			case tr.Truncated:
				endLineDisplay := startLineDisplay + tr.OutputLines - 1
				nextOffset := endLineDisplay + 1
				out = tr.Content
				if tr.TruncatedBy == "lines" {
					out += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", startLineDisplay, endLineDisplay, totalFileLines, nextOffset)
				} else {
					out += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]", startLineDisplay, endLineDisplay, totalFileLines, FormatSize(DefaultMaxBytes), nextOffset)
				}
			case userLimitedLines >= 0 && startLine+userLimitedLines < len(allLines):
				remaining := len(allLines) - (startLine + userLimitedLines)
				nextOffset := startLine + userLimitedLines + 1
				out = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", tr.Content, remaining, nextOffset)
			default:
				out = tr.Content
			}
			return textResult(out), nil
		},
	}
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

func writeTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "write",
		Label:       "write",
		Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
		Parameters: ai.Object(
			ai.Prop("path", ai.String("Path to the file to write (relative or absolute)")),
			ai.Prop("content", ai.String("Content to write to the file")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			path := argStr(params, "path")
			content := argStr(params, "content")
			abs := resolveToCwd(path, cwd)
			return withFileMutationQueue(abs, func() (agent.AgentToolResult, error) {
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					return agent.AgentToolResult{}, err
				}
				if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
					return agent.AgentToolResult{}, err
				}
				// pi reports `content.length` — JS string length in UTF-16 code units
				// (mislabeled "bytes"); match it exactly, not the UTF-8 byte length.
				return textResult(fmt.Sprintf("Successfully wrote %d bytes to %s", utf16Len(content), path)), nil
			})
		},
	}
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

func editTool(cwd string) agent.AgentTool {
	editObjSchema := ai.Object(
		ai.Prop("oldText", ai.String("Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call.")),
		ai.Prop("newText", ai.String("Replacement text for this targeted edit.")),
	)
	return agent.AgentTool{
		Name:        "edit",
		Label:       "edit",
		Description: "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
		Parameters: ai.Object(
			ai.Prop("path", ai.String("Path to the file to edit (relative or absolute)")),
			ai.Prop("edits", ai.ArrayOf(editObjSchema, "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			params = prepareEditArguments(params)
			path := argStr(params, "path")
			rawEdits, _ := params["edits"].([]any)
			if len(rawEdits) == 0 {
				return agent.AgentToolResult{}, fmt.Errorf("Edit tool input is invalid. edits must contain at least one replacement.")
			}
			var edits []editEntry
			for _, re := range rawEdits {
				m, _ := re.(map[string]any)
				edits = append(edits, editEntry{oldText: fmt.Sprint(m["oldText"]), newText: fmt.Sprint(m["newText"])})
			}
			abs := resolveToCwd(path, cwd)
			// Serialize edits/writes to the same file (different files run in parallel).
			return withFileMutationQueue(abs, func() (agent.AgentToolResult, error) {
				data, err := os.ReadFile(abs)
				if err != nil {
					return agent.AgentToolResult{}, fmt.Errorf("Could not edit file: %s. %v.", path, err)
				}
				// Strip a leading BOM before matching (the model won't include it).
				bom, raw := stripBOM(string(data))
				ending := detectLineEnding(raw)
				normalized := normalizeToLF(raw)

				_, newContent, err := applyEditsToNormalizedContent(normalized, edits, path)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				final := bom + restoreLineEndings(newContent, ending)
				if err := os.WriteFile(abs, []byte(final), 0o644); err != nil {
					return agent.AgentToolResult{}, err
				}
				return textResult(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), path)), nil
			})
		},
	}
}

// prepareEditArguments ports pi's prepareEditArguments (edit.ts:94-118): when a
// model sends `edits` as a JSON string, parse it; and fold legacy top-level
// `oldText`/`newText` into the edits[] array.
func prepareEditArguments(input map[string]any) map[string]any {
	if input == nil {
		return input
	}
	// Copy to avoid mutating the caller's map.
	args := make(map[string]any, len(input))
	for k, v := range input {
		args[k] = v
	}

	// Some models send edits as a JSON string instead of an array.
	if s, ok := args["edits"].(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			if arr, ok := parsed.([]any); ok {
				args["edits"] = arr
			}
		}
	}

	oldText, oldOK := args["oldText"].(string)
	newText, newOK := args["newText"].(string)
	if !oldOK || !newOK {
		return args
	}

	var edits []any
	if existing, ok := args["edits"].([]any); ok {
		edits = append(edits, existing...)
	}
	edits = append(edits, map[string]any{"oldText": oldText, "newText": newText})
	args["edits"] = edits
	delete(args, "oldText")
	delete(args, "newText")
	return args
}

func normalizeToLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// detectLineEnding returns CRLF only if the first CRLF precedes the first bare
// LF (port of edit-diff.ts detectLineEnding).
func detectLineEnding(s string) string {
	crlfIdx := strings.Index(s, "\r\n")
	lfIdx := strings.Index(s, "\n")
	if lfIdx == -1 {
		return "\n"
	}
	if crlfIdx == -1 {
		return "\n"
	}
	if crlfIdx < lfIdx {
		return "\r\n"
	}
	return "\n"
}

func restoreLineEndings(s, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// ---------------------------------------------------------------------------
// bash
// ---------------------------------------------------------------------------

func bashTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "bash",
		Label:       "bash",
		Description: fmt.Sprintf("Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.", DefaultMaxLines, DefaultMaxBytes/1024),
		Parameters: ai.Object(
			ai.Prop("command", ai.String("Bash command to execute")),
			ai.Opt("timeout", ai.Number("Timeout in seconds (optional, no default timeout)")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			command := argStr(params, "command")
			if _, err := os.Stat(cwd); err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Working directory does not exist: %s\nCannot execute bash commands.", cwd)
			}
			runCtx := ctx
			var cancel context.CancelFunc
			timedOut := false
			if t, ok := argInt(params, "timeout"); ok && t > 0 {
				runCtx, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
				defer cancel()
			}
			shell, shellArgs := shellConfig()
			cmd := exec.CommandContext(runCtx, shell, append(shellArgs, command)...)
			cmd.Dir = cwd
			// Run in its own process group and, on cancel/timeout, kill the whole
			// tree so backgrounded grandchildren don't survive (port of pi).
			setProcessGroup(cmd)
			cmd.Cancel = func() error { return killProcessTree(cmd) }
			cmd.WaitDelay = 2 * time.Second

			// Stream stdout+stderr incrementally, emitting throttled partial output
			// via onUpdate so long-running commands surface progress (port of pi's
			// throttled OutputAccumulator).
			var outMu sync.Mutex
			var buf bytes.Buffer
			var lastEmit time.Time
			emit := func() {
				if onUpdate == nil {
					return
				}
				outMu.Lock()
				snapshot := buf.String()
				outMu.Unlock()
				tr := TruncateTail(snapshot, 0, 0)
				onUpdate(agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: tr.Content}}, Details: map[string]any{}})
			}
			w := writerFunc(func(p []byte) (int, error) {
				outMu.Lock()
				n, _ := buf.Write(p)
				now := time.Now()
				should := now.Sub(lastEmit) >= bashUpdateThrottle
				if should {
					lastEmit = now
				}
				outMu.Unlock()
				if should {
					emit()
				}
				return n, nil
			})
			cmd.Stdout = w
			cmd.Stderr = w
			err := cmd.Run()
			if runCtx.Err() == context.DeadlineExceeded {
				timedOut = true
			}
			emit() // flush final partial

			outMu.Lock()
			full := buf.String()
			outMu.Unlock()
			tr := TruncateTail(full, 0, 0)
			text := tr.Content
			if tr.Truncated {
				startLine := tr.TotalLines - tr.OutputLines + 1
				endLine := tr.TotalLines
				path, werr := writeTempOutput(full)
				fullOutput := ""
				if werr == nil {
					fullOutput = path
				}
				if tr.LastLinePartial {
					// Byte length of the full (untruncated) last line of output.
					lastLineSize := FormatSize(lastLineBytes(full))
					text += fmt.Sprintf("\n\n[Showing last %s of line %d (line is %s). Full output: %s]",
						FormatSize(tr.OutputBytes), endLine, lastLineSize, fullOutput)
				} else if tr.TruncatedBy == "lines" {
					text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]", startLine, endLine, tr.TotalLines, fullOutput)
				} else {
					text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]", startLine, endLine, tr.TotalLines, FormatSize(DefaultMaxBytes), fullOutput)
				}
			}
			appendStatus := func(t, status string) string {
				if t != "" {
					return t + "\n\n" + status
				}
				return status
			}
			if timedOut {
				t := argMustInt(params, "timeout")
				return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, fmt.Sprintf("Command timed out after %d seconds", t)))
			}
			if ctx.Err() != nil {
				return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, "Command aborted"))
			}
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, fmt.Sprintf("Command exited with code %d", exitErr.ExitCode())))
				}
				return agent.AgentToolResult{}, err
			}
			if text == "" {
				text = "(no output)"
			}
			return textResult(text), nil
		},
	}
}

const bashUpdateThrottle = 100 * time.Millisecond

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func argMustInt(params map[string]any, key string) int {
	v, _ := argInt(params, key)
	return v
}

func shellConfig() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c"}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	return shell, []string{"-c"}
}

// lastLineBytes returns the byte length of the full last line of content,
// dropping a single trailing newline (mirrors OutputAccumulator.getLastLineBytes).
func lastLineBytes(content string) int {
	s := content
	if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
	}
	if idx := strings.LastIndex(s, "\n"); idx != -1 {
		return len(s) - idx - 1
	}
	return len(s)
}

func writeTempOutput(content string) (string, error) {
	f, err := os.CreateTemp("", "pi-bash-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// ---------------------------------------------------------------------------
// ls
// ---------------------------------------------------------------------------

func lsTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "ls",
		Label:       "ls",
		Description: fmt.Sprintf("List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to %d entries or %dKB (whichever is hit first).", lsDefaultLimit, DefaultMaxBytes/1024),
		Parameters: ai.Object(
			ai.Opt("path", ai.String("Directory to list (default: current directory)")),
			ai.Opt("limit", ai.Integer("Maximum number of entries to return (default: 500)")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			dir := cwd
			if p := argStr(params, "path"); p != "" {
				dir = resolveToCwd(p, cwd)
			}
			limit := lsDefaultLimit
			if l, ok := argInt(params, "limit"); ok {
				limit = l
			}
			info, err := os.Stat(dir)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", dir)
			}
			if !info.IsDir() {
				return agent.AgentToolResult{}, fmt.Errorf("Not a directory: %s", dir)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Cannot read directory: %v", err)
			}
			// Collect bare names, sort case-insensitively on the bare name.
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			sort.SliceStable(names, func(a, b int) bool {
				return strings.ToLower(names[a]) < strings.ToLower(names[b])
			})

			var results []string
			entryLimitReached := false
			for _, name := range names {
				if len(results) >= limit {
					entryLimitReached = true
					break
				}
				// Stat (follows symlinks) to detect dir-ness, like pi.
				suffix := ""
				if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
					if st.IsDir() {
						suffix = "/"
					}
				} else {
					// Skip entries we cannot stat.
					continue
				}
				results = append(results, name+suffix)
			}

			if len(results) == 0 {
				return textResult("(empty directory)"), nil
			}
			rawOutput := strings.Join(results, "\n")
			tr := TruncateHead(rawOutput, maxInt, 0)
			output := tr.Content
			var notices []string
			if entryLimitReached {
				notices = append(notices, fmt.Sprintf("%d entries limit reached. Use limit=%d for more", limit, limit*2))
			}
			if tr.Truncated {
				notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
			}
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			return textResult(output), nil
		},
	}
}

// ---------------------------------------------------------------------------
// find (glob)
// ---------------------------------------------------------------------------

func findTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "find",
		Label:       "find",
		Description: fmt.Sprintf("Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to %d results or %dKB (whichever is hit first).", findDefaultLimit, DefaultMaxBytes/1024),
		Parameters: ai.Object(
			ai.Prop("pattern", ai.String("Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'")),
			ai.Opt("path", ai.String("Directory to search in (default: current directory)")),
			ai.Opt("limit", ai.Integer("Maximum number of results (default: 1000)")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			pattern := argStr(params, "pattern")
			root := cwd
			if p := argStr(params, "path"); p != "" {
				root = resolveToCwd(p, cwd)
			}
			limit := findDefaultLimit
			if l, ok := argInt(params, "limit"); ok {
				limit = l
			}
			if _, err := os.Stat(root); err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", root)
			}
			ig := newIgnoreStack(root)
			var results []string
			err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				rel, _ := filepath.Rel(root, p)
				if rel == "." {
					return nil
				}
				if ig.ignored(p, rel, d.IsDir()) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				if matchFdGlob(pattern, rel) {
					results = append(results, filepath.ToSlash(rel))
				}
				return nil
			})
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			// Deterministic, documented ordering: sort lexicographically. fd's native
			// traversal order is unspecified; we keep a stable sort for reproducible output.
			sort.Strings(results)
			resultLimitReached := len(results) >= limit
			if len(results) > limit {
				results = results[:limit]
			}
			if len(results) == 0 {
				return textResult("No files found matching pattern"), nil
			}
			rawOutput := strings.Join(results, "\n")
			tr := TruncateHead(rawOutput, maxInt, 0)
			output := tr.Content
			var notices []string
			if resultLimitReached {
				notices = append(notices, fmt.Sprintf("%d results limit reached. Use limit=%d for more, or refine pattern", limit, limit*2))
			}
			if tr.Truncated {
				notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
			}
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			return textResult(output), nil
		},
	}
}

// ---------------------------------------------------------------------------
// grep
// ---------------------------------------------------------------------------

func grepTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "grep",
		Label:       "grep",
		Description: fmt.Sprintf("Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to %d matches or %dKB (whichever is hit first). Long lines are truncated to %d chars.", grepDefaultLimit, DefaultMaxBytes/1024, GrepMaxLineLength),
		Parameters: ai.Object(
			ai.Prop("pattern", ai.String("Search pattern (regex or literal string)")),
			ai.Opt("path", ai.String("Directory or file to search (default: current directory)")),
			ai.Opt("glob", ai.String("Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'")),
			ai.Opt("ignoreCase", ai.Boolean("Case-insensitive search (default: false)")),
			ai.Opt("literal", ai.Boolean("Treat pattern as literal string instead of regex (default: false)")),
			ai.Opt("context", ai.Integer("Number of lines to show before and after each match (default: 0)")),
			ai.Opt("limit", ai.Integer("Maximum number of matches to return (default: 100)")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			patternStr := argStr(params, "pattern")
			root := cwd
			if p := argStr(params, "path"); p != "" {
				root = resolveToCwd(p, cwd)
			}
			globPat := argStr(params, "glob")
			limit := grepDefaultLimit
			if l, ok := argInt(params, "limit"); ok && l > 0 {
				limit = l
			}
			ctxLines := 0
			if c, ok := argInt(params, "context"); ok {
				ctxLines = c
			}

			flags := ""
			if argBool(params, "ignoreCase") {
				flags = "(?i)"
			}
			expr := patternStr
			if argBool(params, "literal") {
				expr = regexp.QuoteMeta(patternStr)
			}
			re, err := regexp.Compile(flags + expr)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("invalid regex: %v", err)
			}

			info, err := os.Stat(root)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", root)
			}
			isDir := info.IsDir()

			var matchLines []string
			matchCount := 0
			matchLimitReached := false
			linesTruncated := false

			searchFile := func(path, rel string) bool {
				f, err := os.Open(path)
				if err != nil {
					return true
				}
				defer f.Close()
				var lines []string
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
				for scanner.Scan() {
					lines = append(lines, scanner.Text())
				}
				for i, line := range lines {
					if matchCount >= limit {
						matchLimitReached = true
						return false
					}
					if re.MatchString(line) {
						matchCount++
						start := i - ctxLines
						if ctxLines <= 0 {
							start = i
						} else if start < 0 {
							start = 0
						}
						end := i + ctxLines
						if ctxLines <= 0 {
							end = i
						} else if end >= len(lines) {
							end = len(lines) - 1
						}
						for j := start; j <= end; j++ {
							text, was := TruncateLine(lines[j], 0)
							if was {
								linesTruncated = true
							}
							// Match line: "path:N: text". Context line: "path-N- text".
							if j == i {
								matchLines = append(matchLines, fmt.Sprintf("%s:%d: %s", rel, j+1, text))
							} else {
								matchLines = append(matchLines, fmt.Sprintf("%s-%d- %s", rel, j+1, text))
							}
						}
						if matchCount >= limit {
							matchLimitReached = true
							return false
						}
					}
				}
				return true
			}

			if !isDir {
				_ = searchFile(root, filepath.Base(root))
			} else {
				ig := newIgnoreStack(root)
				err = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return nil
					}
					rel, _ := filepath.Rel(root, p)
					if rel == "." {
						return nil
					}
					if ig.ignored(p, rel, d.IsDir()) {
						if d.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
					if d.IsDir() {
						return nil
					}
					if globPat != "" && !matchFdGlob(globPat, rel) {
						return nil
					}
					if matchCount >= limit {
						matchLimitReached = true
						return filepath.SkipAll
					}
					if !searchFile(p, rel) {
						return filepath.SkipAll
					}
					return nil
				})
				if err != nil {
					return agent.AgentToolResult{}, err
				}
			}

			if matchCount == 0 {
				return textResult("No matches found"), nil
			}

			rawOutput := strings.Join(matchLines, "\n")
			tr := TruncateHead(rawOutput, maxInt, 0)
			output := tr.Content
			var notices []string
			if matchLimitReached {
				notices = append(notices, fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", limit, limit*2))
			}
			if tr.Truncated {
				notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
			}
			if linesTruncated {
				notices = append(notices, fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", GrepMaxLineLength))
			}
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			return textResult(output), nil
		},
	}
}
