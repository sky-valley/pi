package coding

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
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
	// io.ReadFull so a short first Read (pipes, network FS) cannot truncate the
	// sniff window; EOF/ErrUnexpectedEOF just mean the file is small.
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return ""
	}
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
		PromptGuidelines: []string{
			"Use read to examine files instead of cat or sed.",
		},
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
				// pi's fs.readFile on a directory raises Node's EISDIR error text.
				return agent.AgentToolResult{}, fmt.Errorf("EISDIR: illegal operation on a directory, read")
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
			hasLimit := false
			userLimitedLines := 0
			if limit, ok := argInt(params, "limit"); ok {
				hasLimit = true
				// pi: endLine = Math.min(startLine + limit, allLines.length), then
				// allLines.slice(startLine, endLine) with JS slice semantics: a
				// negative end counts back from the array end, and an end before
				// start yields an empty slice (never a panic).
				endLine := startLine + limit
				if endLine > len(allLines) {
					endLine = len(allLines)
				}
				effEnd := endLine
				if effEnd < 0 {
					effEnd = len(allLines) + effEnd
					if effEnd < 0 {
						effEnd = 0
					}
				}
				if effEnd < startLine {
					effEnd = startLine
				}
				selected = strings.Join(allLines[startLine:effEnd], "\n")
				// pi keeps the raw arithmetic value (may be zero or negative) for
				// the continuation-footer math.
				userLimitedLines = endLine - startLine
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
			case hasLimit && startLine+userLimitedLines < len(allLines):
				remaining := len(allLines) - (startLine + userLimitedLines)
				nextOffset := startLine + userLimitedLines + 1
				out = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", tr.Content, remaining, nextOffset)
			default:
				out = tr.Content
			}
			res := textResult(out)
			if tr.Truncated {
				res.Details = map[string]any{"truncation": tr}
			}
			return res, nil
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
		PromptGuidelines: []string{
			"Use write only for new files or complete rewrites.",
		},
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
		PromptGuidelines: []string{
			"Use edit for precise changes (edits[].oldText must match exactly)",
			"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
			"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
			"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
		},
		Parameters: ai.Object(
			ai.Prop("path", ai.String("Path to the file to edit (relative or absolute)")),
			ai.Prop("edits", ai.ArrayOf(editObjSchema, "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.")),
		),
		// The harness runs PrepareArguments before schema validation (loop.go),
		// matching pi's prepareArguments hook (edit.ts:307).
		PrepareArguments: prepareEditArguments,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
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
					return agent.AgentToolResult{}, fmt.Errorf("Could not edit file: %s. %s.", path, fsErrorCode(err))
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

// fsErrorCode renders a filesystem error like pi's `Error code: ${error.code}`
// (Node errno codes), falling back to the raw error text like pi's String(error).
func fsErrorCode(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "Error code: ENOENT"
	case errors.Is(err, fs.ErrPermission):
		return "Error code: EACCES"
	case errors.Is(err, syscall.EISDIR):
		return "Error code: EISDIR"
	}
	return err.Error()
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
			shell, shellArgs, useStdin, err := getShellConfig()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if _, err := os.Stat(cwd); err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Working directory does not exist: %s\nCannot execute bash commands.", cwd)
			}
			runCtx := ctx
			var cancel context.CancelFunc
			timeout, hasTimeout := argFloat(params, "timeout")
			if hasTimeout && timeout > 0 {
				runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
				defer cancel()
			}
			// Legacy WSL bash takes the command on stdin (`bash -s`); otherwise it
			// is the final argv entry (`bash -c <command>`).
			var cmd *exec.Cmd
			if useStdin {
				cmd = exec.CommandContext(runCtx, shell, shellArgs...)
				cmd.Stdin = strings.NewReader(command)
			} else {
				cmd = exec.CommandContext(runCtx, shell, append(shellArgs, command)...)
			}
			cmd.Dir = cwd
			// Run in its own process group and, on cancel/timeout, kill the whole
			// tree so backgrounded grandchildren don't survive (port of pi).
			setProcessGroup(cmd)
			cmd.Cancel = func() error { return killProcessTree(cmd) }
			// WaitDelay backstops the manual drain: if killProcessTree fires on
			// cancel/timeout and a descendant still pins the pipe, os/exec force-
			// closes the inherited fds shortly after Wait returns so we never hang.
			cmd.WaitDelay = time.Second

			// Stream output through the rolling OutputAccumulator (bounded memory,
			// incremental temp-file writes) with throttled partial onUpdate emits
			// including a trailing-edge flush (port of bash.ts:291-348).
			u := newBashUpdater(onUpdate, newOutputAccumulator(0, 0, "pi-bash"))
			if onUpdate != nil {
				// pi emits an initial empty update before spawning (bash.ts:332-334).
				onUpdate(agent.AgentToolResult{Content: ai.ContentList{}, Details: nil})
			}
			runErr := runBashCommand(cmd, u)

			snap := u.finish()
			formatOutput := func(emptyText string) (string, map[string]any) {
				text := snap.content
				if text == "" {
					text = emptyText
				}
				var details map[string]any
				if snap.truncation.Truncated {
					details = map[string]any{"truncation": snap.truncation, "fullOutputPath": snap.fullOutputPath}
					tr := snap.truncation
					startLine := tr.TotalLines - tr.OutputLines + 1
					endLine := tr.TotalLines
					if tr.LastLinePartial {
						lastLineSize := FormatSize(u.acc.getLastLineBytes())
						text += fmt.Sprintf("\n\n[Showing last %s of line %d (line is %s). Full output: %s]",
							FormatSize(tr.OutputBytes), endLine, lastLineSize, snap.fullOutputPath)
					} else if tr.TruncatedBy == "lines" {
						text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]", startLine, endLine, tr.TotalLines, snap.fullOutputPath)
					} else {
						text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]", startLine, endLine, tr.TotalLines, FormatSize(DefaultMaxBytes), snap.fullOutputPath)
					}
				}
				return text, details
			}
			appendStatus := func(t, status string) string {
				if t != "" {
					return t + "\n\n" + status
				}
				return status
			}
			// Abort wins over timeout when both fired (bash.ts:112-117).
			if ctx.Err() != nil {
				text, _ := formatOutput("")
				return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, "Command aborted"))
			}
			if runCtx.Err() == context.DeadlineExceeded {
				text, _ := formatOutput("")
				// pi prints the raw timeout value (`timeout:${timeout}`), so 0.5
				// renders "0.5" and 2 renders "2".
				return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, fmt.Sprintf("Command timed out after %s seconds", strconv.FormatFloat(timeout, 'g', -1, 64))))
			}
			if runErr != nil {
				exitErr, ok := runErr.(*exec.ExitError)
				if !ok {
					return agent.AgentToolResult{}, runErr
				}
				// A signal-killed child has no exit code (pi: exitCode === null) and
				// is treated as success with whatever output was produced
				// (bash.ts:397 `exitCode !== 0 && exitCode !== null`).
				if code := exitErr.ExitCode(); code != -1 {
					text, _ := formatOutput("(no output)")
					return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, fmt.Sprintf("Command exited with code %d", code)))
				}
			}
			text, details := formatOutput("(no output)")
			res := textResult(text)
			if details != nil {
				res.Details = details
			}
			return res, nil
		},
	}
}

const bashUpdateThrottle = 100 * time.Millisecond

// bashExitStdioGrace is the idle window we keep reading merged stdout/stderr
// after the process exits. A detached descendant can hold the pipe open and keep
// writing past exit; we must not destroy the stream on a fixed deadline measured
// from exit or its tail is silently lost (port of pi#5303 / 3fa40956). Instead
// the timer is re-armed on every chunk, so an actively writing pipe keeps us
// reading while a quiet held-open handle still releases after the grace elapses.
const bashExitStdioGrace = 100 * time.Millisecond

// runBashCommand starts cmd with stdout+stderr merged onto a single pipe (2>&1
// interleaving, like pi's shared onData handler), streams the merged output into
// u, and waits for the process. After exit it drains the pipe on a re-arming
// idle grace rather than a fixed deadline so output a detached descendant writes
// past exit is captured (port of 3fa40956). It returns the same error cmd.Wait
// would: nil or *exec.ExitError on a non-zero/​signalled exit.
func runBashCommand(cmd *exec.Cmd, u *bashUpdater) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	// Same *os.File on both keeps the child on one pipe so stderr interleaves
	// with stdout in write order.
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	// The parent must drop its write end or pr never sees EOF.
	pw.Close()

	// The reader goroutine feeds u and reports each chunk (and final EOF) on
	// chunks. Closing pr unblocks a read parked on a pipe a descendant still
	// holds open.
	chunks := make(chan struct{}, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				u.Write(buf[:n])
			}
			select {
			case chunks <- struct{}{}:
			default:
			}
			if rerr != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()

	// Process has exited. Drain any output still arriving, re-arming the idle
	// grace per chunk; release on idle-grace OR pipe EOF (reader done).
	timer := time.NewTimer(bashExitStdioGrace)
	defer timer.Stop()
drain:
	for {
		select {
		case <-readDone:
			break drain
		case <-chunks:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(bashExitStdioGrace)
		case <-timer.C:
			break drain
		}
	}
	// Stop tracking the (possibly still-open) inherited handle and unblock the
	// reader. The reader appends only what it has already read, so output isn't
	// double-counted.
	pr.Close()
	<-readDone
	return waitErr
}

func argFloat(params map[string]any, key string) (float64, bool) {
	switch v := params[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

// getShellConfig ports pi's getShellConfig (shell.ts:57-110). pi never uses
// $SHELL or cmd: Unix resolves /bin/bash, then bash on PATH, then sh; Windows
// resolves Git Bash in known locations, then bash.exe on PATH, else errors.
var shellExists = pathExistsFS

// legacyWslBashRE matches the legacy Windows-bundled WSL launcher
// (C:\Windows\System32\bash.exe, or the WoW64 sysnative redirect) after path
// normalization. It mishandles `-c "<cmd>"` quoting, so commands go via stdin.
var legacyWslBashRE = regexp.MustCompile(`^[a-z]:\\windows\\(?:system32|sysnative)\\bash\.exe$`)

// isLegacyWslBashPath ports pi's isLegacyWslBashPath (shell.ts).
func isLegacyWslBashPath(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return legacyWslBashRE.MatchString(normalized)
}

// getBashShellConfig ports pi's getBashShellConfig: legacy WSL bash takes the
// command on stdin (`bash -s`); every other bash takes `bash -c <command>`.
func getBashShellConfig(shell string) (string, []string, bool) {
	if isLegacyWslBashPath(shell) {
		return shell, []string{"-s"}, true
	}
	return shell, []string{"-c"}, false
}

// getShellConfig returns the shell, its args, and whether the command must be
// delivered on stdin (legacy WSL bash) rather than appended to argv.
func getShellConfig() (shell string, args []string, useStdin bool, err error) {
	if runtime.GOOS == "windows" {
		var paths []string
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			paths = append(paths, pf+`\Git\bin\bash.exe`)
		}
		if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
			paths = append(paths, pf86+`\Git\bin\bash.exe`)
		}
		for _, p := range paths {
			if shellExists(p) {
				s, a, stdin := getBashShellConfig(p)
				return s, a, stdin, nil
			}
		}
		if p, err := exec.LookPath("bash.exe"); err == nil && p != "" {
			s, a, stdin := getBashShellConfig(p)
			return s, a, stdin, nil
		}
		var b strings.Builder
		b.WriteString("No bash shell found. Options:\n")
		b.WriteString("  1. Install Git for Windows: https://git-scm.com/download/win\n")
		b.WriteString("  2. Add your bash to PATH (Cygwin, MSYS2, etc.)\n")
		b.WriteString("  3. Set shellPath in settings.json\n\n")
		b.WriteString("Searched Git Bash in:\n")
		for i, p := range paths {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("  " + p)
		}
		return "", nil, false, errors.New(b.String())
	}

	// Unix: try /bin/bash, then bash on PATH, then fall back to sh.
	if shellExists("/bin/bash") {
		s, a, stdin := getBashShellConfig("/bin/bash")
		return s, a, stdin, nil
	}
	if p, err := exec.LookPath("bash"); err == nil && p != "" {
		s, a, stdin := getBashShellConfig(p)
		return s, a, stdin, nil
	}
	return "sh", []string{"-c"}, false, nil
}

// ---------------------------------------------------------------------------
// bash output accumulator (port of output-accumulator.ts)
// ---------------------------------------------------------------------------

type outputSnapshot struct {
	content        string
	truncation     TruncationResult
	fullOutputPath string
}

// outputAccumulator incrementally tracks streaming output with bounded memory:
// it keeps only a rolling tail (≤ 2× maxRollingBytes) for display snapshots
// and streams raw chunks to a temp file once the output exceeds the limits.
type outputAccumulator struct {
	maxLines        int
	maxBytes        int
	maxRollingBytes int
	prefix          string

	rawChunks             [][]byte
	tail                  []byte
	tailStartsAtLineBound bool
	totalBytes            int
	completedLines        int
	totalLines            int
	currentLineBytes      int
	hasOpenLine           bool
	finished              bool

	tempFilePath string
	tempFile     *os.File
}

func newOutputAccumulator(maxLines, maxBytes int, prefix string) *outputAccumulator {
	if maxLines == 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	rolling := maxBytes * 2
	if rolling < 1 {
		rolling = 1
	}
	if prefix == "" {
		prefix = "pi-output"
	}
	return &outputAccumulator{
		maxLines:              maxLines,
		maxBytes:              maxBytes,
		maxRollingBytes:       rolling,
		prefix:                prefix,
		tailStartsAtLineBound: true,
	}
}

func (a *outputAccumulator) append(data []byte) {
	if a.finished || len(data) == 0 {
		return
	}
	a.appendText(data)
	if a.tempFile != nil || a.shouldUseTempFile() {
		a.ensureTempFile()
		if a.tempFile != nil {
			_, _ = a.tempFile.Write(data)
		}
	} else {
		// Copy: os/exec reuses the write buffer.
		a.rawChunks = append(a.rawChunks, append([]byte(nil), data...))
	}
}

func (a *outputAccumulator) finish() {
	if a.finished {
		return
	}
	a.finished = true
	if a.shouldUseTempFile() {
		a.ensureTempFile()
	}
}

func (a *outputAccumulator) appendText(data []byte) {
	a.totalBytes += len(data)
	a.tail = append(a.tail, data...)
	if len(a.tail) > a.maxRollingBytes*2 {
		a.trimTail()
	}

	newlines := bytes.Count(data, []byte{'\n'})
	if newlines == 0 {
		a.currentLineBytes += len(data)
		a.hasOpenLine = true
	} else {
		a.completedLines += newlines
		lastNL := bytes.LastIndexByte(data, '\n')
		tailLen := len(data) - lastNL - 1
		a.currentLineBytes = tailLen
		a.hasOpenLine = tailLen > 0
	}
	a.totalLines = a.completedLines
	if a.hasOpenLine {
		a.totalLines++
	}
}

func (a *outputAccumulator) trimTail() {
	if len(a.tail) <= a.maxRollingBytes {
		return
	}
	start := len(a.tail) - a.maxRollingBytes
	for start < len(a.tail) && (a.tail[start]&0xc0) == 0x80 {
		start++
	}
	if start > 0 {
		a.tailStartsAtLineBound = a.tail[start-1] == '\n'
	}
	a.tail = append([]byte(nil), a.tail[start:]...)
}

func (a *outputAccumulator) snapshotText() string {
	if a.tailStartsAtLineBound {
		return string(a.tail)
	}
	if i := bytes.IndexByte(a.tail, '\n'); i != -1 {
		return string(a.tail[i+1:])
	}
	return string(a.tail)
}

func (a *outputAccumulator) snapshot(persistIfTruncated bool) outputSnapshot {
	tr := TruncateTail(a.snapshotText(), a.maxLines, a.maxBytes)
	truncated := a.totalLines > a.maxLines || a.totalBytes > a.maxBytes
	truncatedBy := ""
	if truncated {
		truncatedBy = tr.TruncatedBy
		if truncatedBy == "" {
			if a.totalBytes > a.maxBytes {
				truncatedBy = "bytes"
			} else {
				truncatedBy = "lines"
			}
		}
	}
	tr.Truncated = truncated
	tr.TruncatedBy = truncatedBy
	tr.TotalLines = a.totalLines
	tr.TotalBytes = a.totalBytes
	tr.MaxLines = a.maxLines
	tr.MaxBytes = a.maxBytes

	if persistIfTruncated && truncated {
		a.ensureTempFile()
	}
	return outputSnapshot{content: tr.Content, truncation: tr, fullOutputPath: a.tempFilePath}
}

func (a *outputAccumulator) closeTempFile() {
	if a.tempFile == nil {
		return
	}
	_ = a.tempFile.Close()
	a.tempFile = nil
}

func (a *outputAccumulator) getLastLineBytes() int { return a.currentLineBytes }

func (a *outputAccumulator) shouldUseTempFile() bool {
	return a.totalBytes > a.maxBytes || a.totalLines > a.maxLines
}

func (a *outputAccumulator) ensureTempFile() {
	if a.tempFilePath != "" {
		return
	}
	var rb [8]byte
	_, _ = rand.Read(rb[:])
	// pi: `${prefix}-${16 hex chars}.log` in the OS temp dir.
	a.tempFilePath = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%x.log", a.prefix, rb))
	f, err := os.Create(a.tempFilePath)
	if err != nil {
		return
	}
	a.tempFile = f
	for _, chunk := range a.rawChunks {
		_, _ = f.Write(chunk)
	}
	a.rawChunks = nil
}

// bashUpdater throttles partial onUpdate emits (leading + trailing edge) and
// serializes accumulator access across the exec pipe goroutines.
type bashUpdater struct {
	mu         sync.Mutex
	onUpdate   agent.ToolUpdateFunc
	acc        *outputAccumulator
	dirty      bool
	lastUpdate time.Time
	timer      *time.Timer
}

func newBashUpdater(onUpdate agent.ToolUpdateFunc, acc *outputAccumulator) *bashUpdater {
	return &bashUpdater{onUpdate: onUpdate, acc: acc}
}

// Write implements io.Writer for the child's stdout/stderr.
func (u *bashUpdater) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.acc.append(p)
	u.scheduleLocked()
	return len(p), nil
}

func (u *bashUpdater) emitLocked() {
	if u.onUpdate == nil || !u.dirty {
		return
	}
	u.dirty = false
	u.lastUpdate = time.Now()
	snap := u.acc.snapshot(true)
	details := map[string]any{}
	if snap.truncation.Truncated {
		details["truncation"] = snap.truncation
	}
	if snap.fullOutputPath != "" {
		details["fullOutputPath"] = snap.fullOutputPath
	}
	u.onUpdate(agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: snap.content}}, Details: details})
}

func (u *bashUpdater) scheduleLocked() {
	if u.onUpdate == nil {
		return
	}
	u.dirty = true
	delay := bashUpdateThrottle - time.Since(u.lastUpdate)
	if delay <= 0 {
		u.clearTimerLocked()
		u.emitLocked()
		return
	}
	if u.timer == nil {
		u.timer = time.AfterFunc(delay, func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.timer = nil
			u.emitLocked()
		})
	}
}

func (u *bashUpdater) clearTimerLocked() {
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
}

// finish flushes the trailing-edge update, finalizes the accumulator, and
// returns the final snapshot (port of bash.ts finishOutput).
func (u *bashUpdater) finish() outputSnapshot {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.acc.finish()
	u.clearTimerLocked()
	u.emitLocked()
	snap := u.acc.snapshot(true)
	u.acc.closeTempFile()
	return snap
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
			// pi sorts with a.toLowerCase().localeCompare(b.toLowerCase());
			// approximate localeCompare with a root-locale UCA collator so
			// punctuation/underscore order matches (e.g. _x < .gitignore < ax).
			coll := collate.New(language.Und)
			sort.SliceStable(names, func(a, b int) bool {
				return coll.CompareString(strings.ToLower(names[a]), strings.ToLower(names[b])) < 0
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
			details := map[string]any{}
			var notices []string
			if entryLimitReached {
				notices = append(notices, fmt.Sprintf("%d entries limit reached. Use limit=%d for more", limit, limit*2))
				details["entryLimitReached"] = limit
			}
			if tr.Truncated {
				notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
				details["truncation"] = tr
			}
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			res := textResult(output)
			if len(details) > 0 {
				res.Details = details
			}
			return res, nil
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
			// fd --max-results treats 0 as unlimited; never slice with a
			// non-positive limit.
			unlimited := limit <= 0
			if _, err := os.Stat(root); err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", root)
			}
			// fd: gitignore applies whether or not we are in a repo (the old
			// --no-require-git effect outside a repo). But inside a repo, fd's
			// default git-aware traversal stops parent .gitignore rules at nested
			// repository boundaries (upstream 756a4e8f), so request that here.
			ig := newIgnoreStack(root, false, true)
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
				if !unlimited && len(results) >= limit {
					return filepath.SkipAll
				}
				// fd matches directories as well as files.
				if matchFdGlob(pattern, rel, p) {
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
			// pi: resultLimitReached = relativized.length >= effectiveLimit (so a
			// limit of 0 still reports the notice, matching fd's 0-is-unlimited).
			resultLimitReached := limit >= 0 && len(results) >= limit
			if len(results) == 0 {
				return textResult("No files found matching pattern"), nil
			}
			rawOutput := strings.Join(results, "\n")
			tr := TruncateHead(rawOutput, maxInt, 0)
			output := tr.Content
			details := map[string]any{}
			var notices []string
			if resultLimitReached {
				notices = append(notices, fmt.Sprintf("%d results limit reached. Use limit=%d for more, or refine pattern", limit, limit*2))
				details["resultLimitReached"] = limit
			}
			if tr.Truncated {
				notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
				details["truncation"] = tr
			}
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			res := textResult(output)
			if len(details) > 0 {
				res.Details = details
			}
			return res, nil
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
			// pi: Math.max(1, limit ?? 100) — non-positive limits clamp to 1
			// (grep.ts:189).
			limit := grepDefaultLimit
			if l, ok := argInt(params, "limit"); ok {
				limit = l
			}
			if limit < 1 {
				limit = 1
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

			// searchFile scans one file; skipBinary mirrors rg's NUL sniff (a NUL
			// byte in the first 8KB marks the file binary; only applies during
			// directory traversal — explicitly-given files are always searched).
			searchFile := func(path, rel string, skipBinary bool) bool {
				data, err := os.ReadFile(path)
				if err != nil {
					return true
				}
				if skipBinary {
					window := data
					if len(window) > 8*1024 {
						window = window[:8*1024]
					}
					if bytes.IndexByte(window, 0) != -1 {
						return true
					}
				}
				// pi normalizes \r\n and bare \r to \n before splitting
				// (grep.ts getFileLines). Reading the whole file also avoids any
				// per-line length cap (rg has none).
				content := strings.ReplaceAll(string(data), "\r\n", "\n")
				content = strings.ReplaceAll(content, "\r", "\n")
				lines := strings.Split(content, "\n")
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
				_ = searchFile(root, filepath.Base(root), false)
			} else {
				// rg semantics: gitignore applies only inside a git repository.
				ig := newIgnoreStack(root, true, false)
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
					if globPat != "" && !matchRgGlob(globPat, rel) {
						return nil
					}
					if matchCount >= limit {
						matchLimitReached = true
						return filepath.SkipAll
					}
					if !searchFile(p, rel, true) {
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
			details := map[string]any{}
			var notices []string
			if matchLimitReached {
				notices = append(notices, fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", limit, limit*2))
				details["matchLimitReached"] = limit
			}
			if tr.Truncated {
				notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
				details["truncation"] = tr
			}
			if linesTruncated {
				notices = append(notices, fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", GrepMaxLineLength))
				details["linesTruncated"] = true
			}
			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, ". ") + "]"
			}
			res := textResult(output)
			if len(details) > 0 {
				res.Details = details
			}
			return res, nil
		},
	}
}
