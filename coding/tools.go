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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
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

// textResult is a tool result that is just text. Details is left nil: pi spells
// "no details" as `details: undefined` (write.ts:88, ls.ts:135, find.ts:137,
// grep.ts:258), which JSON.stringify drops from the session entry. An empty map
// would survive `omitempty` — omitempty elides a nil interface, not a non-nil
// empty map — and put a `"details":{}` key on the wire that pi never writes.
func textResult(text string) agent.AgentToolResult {
	return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}}
}

// ToolNames are the built-in coding tool identifiers.
var ToolNames = []string{"read", "bash", "powershell", "edit", "write", "grep", "find", "ls"}

// ToolSnippets are the one-line prompt snippets keyed by tool name.
var ToolSnippets = map[string]string{
	"read":       "Read file contents",
	"bash":       "Execute bash commands (ls, grep, find, etc.)",
	"powershell": "Execute PowerShell commands",
	"edit":       "Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
	"write":      "Create or overwrite files",
	"grep":       "Search file contents for patterns (respects .gitignore)",
	"find":       "Find files by glob pattern (respects .gitignore)",
	"ls":         "List directory contents",
	"web_fetch":  "Fetch a web URL and return readable text",
}

// preferStrictToolSampling is pi's inlined `{type: "json_schema", strict:
// "prefer"}` on the built-in read/bash/powershell/edit/write definitions
// (upstream fcff255b0). pi dropped PREFER_STRICT_TOOL_SAMPLING and the
// PI_EXPERIMENTAL gate that fed it, making strict sampling the default; the
// port keeps one shared value rather than repeating the literal at each site,
// which is the same bytes on the wire. Nothing mutates it.
var preferStrictToolSampling = &ai.ConstrainedSamplingConfig{
	Type:   ai.ConstrainedSamplingJSONSchema,
	Strict: ai.ConstrainedSamplingPrefer,
}

// CreateTool builds a single built-in tool by name, rooted at cwd. The bash
// tool built this way exposes no PI_* session metadata: there is no session to
// read it from, matching pi's `exposeSessionEnvironment && ctx` guard when the
// tool runs without an extension context.
func CreateTool(name, cwd string) (agent.AgentTool, error) {
	return createTool(name, cwd, nil, nil)
}

// createTool is CreateTool with the session-metadata provider the coding
// session threads into the bash tool (nil for standalone construction).
func createTool(name, cwd string, sessionEnv sessionEnvFn, resize imageResizeFn) (agent.AgentTool, error) {
	switch name {
	case "read":
		return readToolOps(cwd, nil, resize), nil
	case "bash":
		return bashTool(cwd, sessionEnv), nil
	case "powershell":
		return powershellTool(cwd, sessionEnv), nil
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
	return []agent.AgentTool{readTool(cwd), bashTool(cwd, nil), editTool(cwd), writeTool(cwd)}
}

// CreateAllTools returns all eight built-in tools, in pi's createAllTools order
// (powershell sits after bash). powershell is included even off Windows: like
// pi, the shell is resolved inside Execute, so constructing the tool is safe
// everywhere and only running it reports the platform error.
func CreateAllTools(cwd string) []agent.AgentTool {
	return []agent.AgentTool{
		readTool(cwd), bashTool(cwd, nil), powershellTool(cwd, nil), editTool(cwd), writeTool(cwd),
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
	if startsWithAscii(buf, 0, "GIF87a") || startsWithAscii(buf, 0, "GIF89a") {
		return "image/gif"
	}
	if startsWithAscii(buf, 0, "RIFF") && startsWithAscii(buf, 8, "WEBP") {
		return "image/webp"
	}
	if startsWithAscii(buf, 0, "BM") && isBmp(buf) {
		return "image/bmp"
	}
	return ""
}

// isBmp validates the BMP magic + DIB header (port of utils/mime.ts isBmp).
// It requires colorPlanes==1 and bitsPerPixel in {1,4,8,16,24,32}, and applies
// the declaredFileSize/pixelDataOffset sanity checks.
func isBmp(buf []byte) bool {
	if len(buf) < 26 {
		return false
	}
	declaredFileSize := readUint32LE(buf, 2)
	pixelDataOffset := readUint32LE(buf, 10)
	dibHeaderSize := readUint32LE(buf, 14)
	if declaredFileSize != 0 && declaredFileSize < 26 {
		return false
	}
	if pixelDataOffset < 14+dibHeaderSize {
		return false
	}
	if declaredFileSize != 0 && pixelDataOffset >= declaredFileSize {
		return false
	}

	var colorPlanes, bitsPerPixel int
	if dibHeaderSize == 12 {
		colorPlanes = readUint16LE(buf, 22)
		bitsPerPixel = readUint16LE(buf, 24)
	} else if dibHeaderSize >= 40 && dibHeaderSize <= 124 {
		if len(buf) < 30 {
			return false
		}
		colorPlanes = readUint16LE(buf, 26)
		bitsPerPixel = readUint16LE(buf, 28)
	} else {
		return false
	}
	if colorPlanes != 1 {
		return false
	}
	switch bitsPerPixel {
	case 1, 4, 8, 16, 24, 32:
		return true
	default:
		return false
	}
}

func readUint16LE(buf []byte, offset int) int {
	b := func(i int) int {
		if i < len(buf) {
			return int(buf[i])
		}
		return 0
	}
	return b(offset) + (b(offset+1) << 8)
}

func readUint32LE(buf []byte, offset int) int {
	b := func(i int) int {
		if i < len(buf) {
			return int(buf[i])
		}
		return 0
	}
	return b(offset) + (b(offset+1) << 8) + (b(offset+2) << 16) + b(offset+3)*0x1000000
}

// DetectSupportedImageMimeTypeFromFile reads up to the sniff window from a file
// and identifies a supported image type (mime.ts
// detectSupportedImageMimeTypeFromFile), returning "" when the file is not an
// image pi can attach — including when it cannot be opened, matching pi's null.
// Upstream de82e5367 promoted it to published SDK surface; the buffer variant it
// delegates to is not published there and stays unexported here.
func DetectSupportedImageMimeTypeFromFile(path string) string {
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

// readTool builds the read tool against the local filesystem — the form every
// existing caller uses. readToolOps is the injection seam.
func readTool(cwd string) agent.AgentTool { return readToolOps(cwd, nil, nil) }

// readToolOps builds the read tool against injectable file operations, porting
// pi's ReadOperations seam (core/tools/read.ts:49). pi's own words: "Override
// these to delegate file reading to remote systems (for example SSH)." A nil
// member falls back to the local default, which is pi's spread-over-defaults.
func readToolOps(cwd string, custom *ReadOperations, resize imageResizeFn) agent.AgentTool {
	ops := resolveReadOperations(custom)
	return agent.AgentTool{
		Name:        "read",
		Label:       "read",
		Description: fmt.Sprintf("Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.", DefaultMaxLines, DefaultMaxBytes/1024),
		PromptGuidelines: []string{
			"Use read to examine files instead of cat or sed.",
		},
		Parameters: ai.Object(
			ai.Prop("path", ai.String("Path to the file to read (relative or absolute)")),
			ai.Opt("offset", ai.Number("Line number to start reading from (1-indexed)")),
			ai.Opt("limit", ai.Number("Maximum number of lines to read")),
		),
		ConstrainedSampling: preferStrictToolSampling,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			path := argStr(params, "path")
			abs := resolveReadPath(path, cwd)
			// pi calls ops.access before reading (read.ts:248). The EISDIR a
			// directory produces is NOT checked here: Node's fs.readFile raises it
			// on its own, so it belongs to the filesystem implementation — see
			// DefaultReadOperations. Checking it here would stat the LOCAL disk
			// even when a host has injected a remote filesystem.
			if err := ops.Access(ctx, abs); err != nil {
				return agent.AgentToolResult{}, err
			}
			mime := ""
			if ops.DetectImageMimeType != nil {
				mime = ops.DetectImageMimeType(ctx, abs)
			}
			if mime != "" {
				data, err := ops.ReadFile(ctx, abs)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				// Normalize (convert BMP→PNG), downscale/re-encode to fit the
				// model's inline image limits, and apply EXIF orientation (port of
				// pi's processImage). The note text matches pi's read tool exactly
				// (utils/image-process.ts).
				processed := processImage(data, mime, true, resize.get())
				if !processed.Ok {
					return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{
						Text: fmt.Sprintf("Read image file [%s]\n%s", mime, processed.Message),
					}}}, nil
				}
				note := fmt.Sprintf("Read image file [%s]", processed.MimeType)
				if len(processed.Hints) > 0 {
					note += "\n" + strings.Join(processed.Hints, "\n")
				}
				// pi's read never assigns `details` on the image branches
				// (read.ts:108-131), so it stays undefined and off the wire.
				return agent.AgentToolResult{Content: ai.ContentList{
					ai.TextContent{Text: note},
					ai.ImageContent{Data: encodeBase64(processed.Data), MimeType: processed.MimeType},
				}}, nil
			}

			data, err := ops.ReadFile(ctx, abs)
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

// writeTool builds the write tool against the local filesystem.
func writeTool(cwd string) agent.AgentTool { return writeToolOps(cwd, nil) }

// writeToolOps builds the write tool against injectable file operations,
// porting pi's WriteOperations seam (core/tools/write.ts:31).
func writeToolOps(cwd string, custom *WriteOperations) agent.AgentTool {
	ops := resolveWriteOperations(custom)
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
		ConstrainedSampling: preferStrictToolSampling,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			path := argStr(params, "path")
			content := argStr(params, "content")
			abs := resolveToCwd(path, cwd)
			return withFileMutationQueue(abs, func() (agent.AgentToolResult, error) {
				if err := ops.Mkdir(ctx, filepath.Dir(abs)); err != nil {
					return agent.AgentToolResult{}, err
				}
				if err := ops.WriteFile(ctx, abs, content); err != nil {
					return agent.AgentToolResult{}, err
				}
				// pi dropped the count from this message (write.ts:87): it was
				// reporting `content.length` — UTF-16 code units — as bytes.
				return textResult("Successfully wrote to " + path), nil
			})
		},
	}
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

// editTool builds the edit tool against the local filesystem.
func editTool(cwd string) agent.AgentTool { return editToolOps(cwd, nil) }

// editToolOps builds the edit tool against injectable file operations, porting
// pi's EditOperations seam (core/tools/edit.ts:96). Note pi's edit Access
// checks R_OK|W_OK where read's checks R_OK alone.
func editToolOps(cwd string, custom *EditOperations) agent.AgentTool {
	ops := resolveEditOperations(custom)
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
		ConstrainedSampling: preferStrictToolSampling,
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
				data, err := ops.ReadFile(ctx, abs)
				if err != nil {
					return agent.AgentToolResult{}, fmt.Errorf("Could not edit file: %s. %s.", path, fsErrorCode(err))
				}
				// Strip a leading BOM before matching (the model won't include it).
				bom, raw := splitBOM(string(data))
				ending := detectLineEnding(raw)
				normalized := normalizeToLF(raw)

				_, newContent, err := applyEditsToNormalizedContent(normalized, edits, path)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				final := bom + restoreLineEndings(newContent, ending)
				if err := ops.WriteFile(ctx, abs, final); err != nil {
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

	// Some models send edits as a JSON string instead of an array. Others send
	// a single edit object instead of a one-element edits array (upstream
	// ca21c1686); both forms wrap to [edit].
	if s, ok := args["edits"].(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			if arr, ok := parsed.([]any); ok {
				args["edits"] = arr
			} else if isSingleEditInput(parsed) {
				args["edits"] = []any{parsed}
			}
		}
	} else if isSingleEditInput(args["edits"]) {
		args["edits"] = []any{args["edits"]}
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

// isSingleEditInput reports whether value is a bare {oldText, newText} edit
// object (pi edit.ts isSingleEditInput): a non-array object whose oldText and
// newText are both strings.
func isSingleEditInput(value any) bool {
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, oldOK := m["oldText"].(string)
	_, newOK := m["newText"].(string)
	return oldOK && newOK
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

// pi MAX_TIMEOUT_MS (INT32_MAX): the bash tool rejects any timeout that would
// exceed this many milliseconds (bash.ts resolveTimeoutMs).
const maxBashTimeoutMs = 2147483647

// maxBashTimeoutSeconds renders pi's MAX_TIMEOUT_SECONDS (2147483.647) for the
// rejection message, byte-identical to JS `${MAX_TIMEOUT_MS / 1000}`.
var maxBashTimeoutSeconds = strconv.FormatFloat(float64(maxBashTimeoutMs)/1000, 'f', -1, 64)

// sessionEnvFn supplies the PI_* session metadata exposed to bash commands.
// It is called per execution so a mid-session /model or thinking-level change
// is reflected, mirroring pi reading them off the live ExtensionContext.
type sessionEnvFn func() map[string]string

// imageResizeFn supplies the resize profile of the model a request is being
// built for. pi reads it straight off the tool execution context
// (`ctx?.model?.inputLimits?.images?.resize`, read.ts) and falls back to a
// construction-time option when the context carries no model. The port's
// AgentTool.Execute has no model, so this getter — installed by NewSession,
// the same shape as sessionEnvFn — is that seam, and reading it per call keeps
// a mid-session SetModel from leaving a stale profile behind. Scope queue entry
// 18 covers giving the execution context a model outright.
type imageResizeFn func() *ai.ModelImageResizeOptions

// get resolves the profile, treating a nil getter as "no model metadata" — pi's
// undefined ctx.model, which falls through to the pipeline defaults.
func (f imageResizeFn) get() *ai.ModelImageResizeOptions {
	if f == nil {
		return nil
	}
	return f()
}

// piSessionEnvKeys are the session metadata variables pi manages for bash
// commands. They are stripped from the inherited environment before every run
// so a parent process's stale values never leak into a child (pi bb3d7d39).
var piSessionEnvKeys = []string{
	"PI_SESSION_ID",
	"PI_SESSION_FILE",
	"PI_PROVIDER",
	"PI_MODEL",
	"PI_REASONING_LEVEL",
}

// bashCommandEnv builds the child environment: the inherited environment minus
// the PI_* session keys, plus whatever the session currently provides.
func bashCommandEnv(sessionEnv sessionEnvFn) []string {
	environ := os.Environ()
	env := make([]string, 0, len(environ)+len(piSessionEnvKeys))
	for _, kv := range environ {
		// Exact-match, like pi's `delete env.PI_SESSION_ID`. On Windows a
		// differently-cased inherited key survives this, but os/exec dedupes the
		// child environment case-insensitively keeping the last entry, so the
		// value appended below still wins.
		if name, _, ok := strings.Cut(kv, "="); ok && slices.Contains(piSessionEnvKeys, name) {
			continue
		}
		env = append(env, kv)
	}
	if sessionEnv == nil {
		return env
	}
	// pi assigns in a fixed order; keep it stable for readable diffs.
	provided := sessionEnv()
	for _, k := range piSessionEnvKeys {
		if v, ok := provided[k]; ok && v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// shellToolConfig describes one built-in shell tool. pi factors bash and
// powershell out of a shared createShellToolDefinition (bash.ts ShellToolConfig,
// upstream 80e62761f); the same split here keeps the two tools byte-identical
// apart from these fields. pi's label always equals its name for both shells,
// and its `prompt`/`promptSnippet` fields drive TUI rendering and the prompt
// snippet map, neither of which lives on the tool in this port. pi's
// `promptGuidelines` is likewise not a field here: both shells contribute the
// same single guideline, so a knob with one value would only alias one backing
// array across every tool built from these package-level configs.
type shellToolConfig struct {
	name           string
	shellName      string
	tempFilePrefix string
	// commandPrefix is prepended verbatim to every command before it reaches the
	// shell. pi applies it inside the operations wrapper, ahead of any
	// spawn-hook prefix (powershell.ts createLocalPowerShellOperations).
	commandPrefix string
	// resolveShell resolves the shell binary, its args, and whether the command
	// must be delivered on stdin rather than appended to argv.
	resolveShell func() (shell string, args []string, useStdin bool, err error)
}

// utf8OutputPrefix opts PowerShell's console into UTF-8 so non-ASCII output
// survives the pipe. Byte-exact from powershell.ts UTF8_OUTPUT_PREFIX,
// including the trailing newline that separates it from the model's command.
const utf8OutputPrefix = "try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}\n"

// piSessionEnvGuideline is the PI_* advertisement both shell tools contribute
// to the system prompt (pi bb3d7d39, shared by powershell.ts since 80e62761f).
const piSessionEnvGuideline = "You can inspect PI_* environment variables for current model and session details."

var bashShellConfig = shellToolConfig{
	name:           "bash",
	shellName:      "bash",
	tempFilePrefix: "pi-bash",
	resolveShell:   getShellConfig,
}

var powershellShellConfig = shellToolConfig{
	name:           "powershell",
	shellName:      "PowerShell",
	tempFilePrefix: "pi-powershell",
	commandPrefix:  utf8OutputPrefix,
	resolveShell:   getPowerShellConfig,
}

func bashTool(cwd string, sessionEnv sessionEnvFn) agent.AgentTool {
	return shellTool(cwd, bashShellConfig, sessionEnv)
}

func powershellTool(cwd string, sessionEnv sessionEnvFn) agent.AgentTool {
	return shellTool(cwd, powershellShellConfig, sessionEnv)
}

func shellTool(cwd string, config shellToolConfig, sessionEnv sessionEnvFn) agent.AgentTool {
	return shellToolOps(cwd, config, sessionEnv, nil)
}

// shellToolOps builds a shell tool against injectable process execution,
// porting pi's BashOperations seam (core/tools/bash.ts:63), which pi shares
// between bash and powershell. Only the SPAWN is injectable: the output
// accumulator, truncation, temp-file overflow and status formatting stay in the
// tool, exactly as they do upstream.
func shellToolOps(cwd string, config shellToolConfig, sessionEnv sessionEnvFn, custom *BashOperations) agent.AgentTool {
	ops := resolveBashOperations(custom, config)
	return agent.AgentTool{
		Name:             config.name,
		Label:            config.name,
		Description:      fmt.Sprintf("Execute a %s command in the current working directory. Returns stdout and stderr. Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.", config.shellName, DefaultMaxLines, DefaultMaxBytes/1024),
		PromptGuidelines: []string{piSessionEnvGuideline},
		Parameters: ai.Object(
			ai.Prop("command", ai.String("Shell command to execute")),
			ai.Opt("timeout", ai.Number("Timeout in seconds (optional, no default timeout)")),
		),
		ConstrainedSampling: preferStrictToolSampling,
		OutputSchema:        bashOutputSchema(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			command := config.commandPrefix + argStr(params, "command")
			// pi resolveTimeoutMs (bash.ts) validates the timeout before spawning:
			// reject a non-positive value and cap it at INT32_MAX ms, surfacing the
			// raw error as the tool result. JSON numbers are always finite, so pi's
			// !Number.isFinite guard collapses into the non-positive rejection here.
			timeout, hasTimeout := argFloat(params, "timeout")
			if hasTimeout {
				if timeout <= 0 {
					return agent.AgentToolResult{}, fmt.Errorf("Invalid timeout: must be a finite number of seconds")
				}
				if timeout*1000 > maxBashTimeoutMs {
					return agent.AgentToolResult{}, fmt.Errorf("Invalid timeout: maximum is %s seconds", maxBashTimeoutSeconds)
				}
			}
			// Stream output through the rolling OutputAccumulator (bounded memory,
			// incremental temp-file writes) with throttled partial onUpdate emits
			// including a trailing-edge flush (port of bash.ts:291-348).
			u := newBashUpdater(onUpdate, newOutputAccumulator(0, 0, config.tempFilePrefix))
			if onUpdate != nil {
				// pi emits an initial empty update before spawning (bash.ts:332-334).
				onUpdate(agent.AgentToolResult{Content: ai.ContentList{}, Details: nil})
			}
			execTimeout := 0.0
			if hasTimeout {
				execTimeout = timeout
			}
			startedAt := time.Now()
			exitCode, execErr := ops.Exec(ctx, command, cwd, BashExecOptions{
				OnData:         func(p []byte) { u.Write(p) },
				TimeoutSeconds: execTimeout,
				Env:            bashCommandEnv(sessionEnv),
			})

			snap, err := u.finish()
			if err != nil {
				// D90: pi's finishOutput awaits closeTempFile before it looks at
				// the command's fate, so a temp file that failed replaces an
				// abort or a timeout too. pi fails the call only when the error
				// comes after closeTempFile attached its listener; before that,
				// its write stream's unhandled error crashes the process or leaves
				// the call waiting. The port always fails the call, with the cause
				// and how to fix it.
				return agent.AgentToolResult{}, fmt.Errorf("could not save the full command output: %w; point TMPDIR (TMP on Windows) at a writable directory with free space", err)
			}
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
			// pi classifies the spawn's failure before looking at the exit status,
			// and abort wins over timeout when both fired (bash.ts:112-117, 457-467).
			if execErr != nil {
				if errors.Is(execErr, ErrShellAborted) {
					text, _ := formatOutput("")
					return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, "Command aborted"))
				}
				var timeoutErr *ShellTimeoutError
				if errors.As(execErr, &timeoutErr) {
					text, _ := formatOutput("")
					// pi prints the raw timeout value (`timeout:${timeout}`), so 0.5
					// renders "0.5" and 2 renders "2".
					return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, fmt.Sprintf("Command timed out after %s seconds", strconv.FormatFloat(timeoutErr.Seconds, 'g', -1, 64))))
				}
				return agent.AgentToolResult{}, execErr
			}
			// pi a8b3dd199 (#9577): a command with no exit code at all fails
			// rather than reporting partial output as a success. The local shell
			// no longer produces one — a signal termination arrives as
			// 128 + signal — so this is reachable only through custom
			// BashOperations, e.g. a remote runner that lost the status
			// (bash.ts:366-371).
			if exitCode == nil {
				text, _ := formatOutput("(no output)")
				return agent.AgentToolResult{}, fmt.Errorf("%s", appendStatus(text, "Command terminated without an exit code"))
			}
			text, details := formatOutput("(no output)")
			// The wall time is taken before the full output is read, as pi
			// takes it.
			wallTimeSeconds := bashWallTimeSeconds(time.Since(startedAt))
			fullOutput, fullTruncated, err := u.acc.readFullOutput(bashStructuredOutputMaxBytes)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			structured := BashToolOutput{
				Output:          fullOutput,
				Truncated:       fullTruncated,
				ExitCode:        *exitCode,
				WallTimeSeconds: wallTimeSeconds,
			}
			if fullTruncated {
				structured.FullOutputPath = snap.fullOutputPath
			}
			// Upstream 8562bcf66: a non-zero exit is an error result, not a
			// thrown error, so it keeps its details and structured content. pi's
			// details stay undefined unless the output was truncated.
			res := textResult(text)
			if *exitCode != 0 {
				res = textResult(appendStatus(text, fmt.Sprintf("Command exited with code %d", *exitCode)))
				res.IsError = true
			}
			if details != nil {
				res.Details = details
			}
			res.StructuredContent = structured
			return res, nil
		},
	}
}

// BashToolOutput is a shell tool's structured content (pi bash.ts
// BashToolOutput, upstream 8562bcf66): the result for programmatic callers
// such as a script that calls tools. A non-zero exit is an error result for
// the model, but a script still gets this value.
type BashToolOutput struct {
	// Output is the combined stdout and stderr, without the status line and
	// not cut to the model-facing limits: up to 1 MiB, and longer output keeps
	// its first and last 512 KiB around an omission marker (upstream
	// 1ff5b6fdd). A caller decides how much of it reaches the model.
	Output string `json:"output"`
	// Truncated is whether Output omits part of the command output.
	Truncated bool `json:"truncated"`
	// FullOutputPath is the temp file with the full output, set only when
	// Output is truncated.
	FullOutputPath  string  `json:"full_output_path,omitempty"`
	ExitCode        int     `json:"exit_code"`
	WallTimeSeconds float64 `json:"wall_time_seconds"`
}

// bashStructuredOutputMaxBytes limits BashToolOutput.Output (pi
// STRUCTURED_OUTPUT_MAX_BYTES).
const bashStructuredOutputMaxBytes = 1024 * 1024

// bashOutputSchema is the shell tools' OutputSchema (pi bashOutputSchema),
// built per tool like Parameters, since a Schema can be modified in place.
func bashOutputSchema() *ai.Schema {
	return ai.Object(
		ai.Prop("output", ai.String("Combined stdout and stderr, up to 1 MiB. Longer output keeps its first and last 512 KiB around an omission marker.")),
		ai.Prop("truncated", ai.Boolean("Whether `output` omits part of the command output")),
		ai.Opt("full_output_path", ai.String("Temp file with the full output, when truncated")),
		ai.Prop("exit_code", ai.Number()),
		ai.Prop("wall_time_seconds", ai.Number()),
	)
}

// bashWallTimeSeconds is pi's Math.round(elapsedMs / 100) / 10: the wall
// time in tenths of a second.
func bashWallTimeSeconds(elapsed time.Duration) float64 {
	elapsedMs := float64(elapsed) / float64(time.Millisecond)
	return float64(jsRound(elapsedMs/100)) / 10
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
func runBashCommand(cmd *exec.Cmd, w io.Writer) error {
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

	// The reader goroutine feeds w and reports each chunk (and final EOF) on
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
				w.Write(buf[:n])
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

// powershellArgs ports pi's POWERSHELL_ARGS (shell.ts, upstream 80e62761f):
// no profile, no interactive prompts, and a process-scoped execution-policy
// bypass so the machine policy is left alone.
var powershellArgs = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}

// getPowerShellConfig ports pi's getPowerShellConfig: Windows only, preferring
// PowerShell 7 (pwsh) over Windows PowerShell. pi throws on both failures; the
// Go shell resolvers report failure as an error, which the tool surfaces to the
// model verbatim, so the strings are byte-exact.
func getPowerShellConfig() (shell string, args []string, useStdin bool, err error) {
	if runtime.GOOS != "windows" {
		return "", nil, false, errors.New("The powershell tool is only available on Windows.")
	}
	for _, executable := range []string{"pwsh.exe", "powershell.exe"} {
		if p, lookErr := exec.LookPath(executable); lookErr == nil && p != "" {
			return p, slices.Clone(powershellArgs), false, nil
		}
	}
	return "", nil, false, errors.New("No PowerShell executable found. Install PowerShell or add powershell.exe/pwsh.exe to PATH.")
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
// it decodes the chunks as pi's streaming TextDecoder does, keeps only a
// rolling tail of the text (≤ 2× maxRollingBytes) for display snapshots, and
// streams the raw chunks to a temp file once the output exceeds the limits.
type outputAccumulator struct {
	maxLines        int
	maxBytes        int
	maxRollingBytes int
	prefix          string

	decoder               jstext.TextDecoder
	rawChunks             [][]byte
	tail                  []byte // decoded text
	tailStartsAtLineBound bool
	totalRawBytes         int
	totalDecodedBytes     int
	completedLines        int
	totalLines            int
	currentLineBytes      int
	hasOpenLine           bool
	finished              bool

	tempFilePath string
	tempFile     *os.File
	// tempFileErr is the first error creating, writing or closing the temp
	// file. Nothing is written after it, and closeTempFile returns it.
	tempFileErr error
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
	a.totalRawBytes += len(data)
	a.appendDecodedText(a.decoder.Decode(data))
	if a.tempFile != nil || a.shouldUseTempFile() {
		a.ensureTempFile()
		a.writeTempFile(data)
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
	a.appendDecodedText(a.decoder.Flush())
	if a.shouldUseTempFile() {
		a.ensureTempFile()
	}
}

// appendDecodedText counts text the decoder produced: its UTF-8 bytes, which
// the limits measure, and its lines.
func (a *outputAccumulator) appendDecodedText(text string) {
	if text == "" {
		return
	}
	a.totalDecodedBytes += len(text)
	a.tail = append(a.tail, text...)
	if len(a.tail) > a.maxRollingBytes*2 {
		a.trimTail()
	}

	newlines := strings.Count(text, "\n")
	if newlines == 0 {
		a.currentLineBytes += len(text)
		a.hasOpenLine = true
	} else {
		a.completedLines += newlines
		tailLen := len(text) - strings.LastIndexByte(text, '\n') - 1
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
	truncated := a.totalLines > a.maxLines || a.totalDecodedBytes > a.maxBytes
	truncatedBy := ""
	if truncated {
		truncatedBy = tr.TruncatedBy
		if truncatedBy == "" {
			if a.totalDecodedBytes > a.maxBytes {
				truncatedBy = "bytes"
			} else {
				truncatedBy = "lines"
			}
		}
	}
	tr.Truncated = truncated
	tr.TruncatedBy = truncatedBy
	tr.TotalLines = a.totalLines
	tr.TotalBytes = a.totalDecodedBytes
	tr.MaxLines = a.maxLines
	tr.MaxBytes = a.maxBytes

	if persistIfTruncated && truncated {
		a.ensureTempFile()
	}
	return outputSnapshot{content: tr.Content, truncation: tr, fullOutputPath: a.tempFilePath}
}

// closeTempFile closes the temp file and returns the first error creating,
// writing or closing it, as pi's closeTempFile rejects with its write stream's
// error.
func (a *outputAccumulator) closeTempFile() error {
	if a.tempFile != nil {
		if err := a.tempFile.Close(); err != nil && a.tempFileErr == nil {
			a.tempFileErr = err
		}
		a.tempFile = nil
	}
	return a.tempFileErr
}

// readFullOutput is the complete output, for callers that can take more than
// the display snapshot (pi readFullOutput, upstream 1ff5b6fdd). Call it after
// finish and closeTempFile. Output longer than maxBytes raw bytes keeps its
// first and last maxBytes/2 bytes around an omission marker. Each part is
// decoded as pi's TextDecoder decodes it: the head in stream mode, so a
// character the cut splits is dropped, and the tail after the continuation
// bytes the cut left at its start.
func (a *outputAccumulator) readFullOutput(maxBytes int) (content string, truncated bool, err error) {
	if a.tempFilePath == "" {
		return jstext.DecodeText(bytes.Join(a.rawChunks, nil)), false, nil
	}
	f, err := os.Open(a.tempFilePath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	size := info.Size()
	if size <= int64(maxBytes) {
		data, err := io.ReadAll(f)
		if err != nil {
			return "", false, err
		}
		return jstext.DecodeText(data), false, nil
	}
	head := make([]byte, maxBytes/2)
	tail := make([]byte, maxBytes-len(head))
	if n, err := f.ReadAt(head, 0); n < len(head) {
		return "", false, err
	}
	if n, err := f.ReadAt(tail, size-int64(len(tail))); n < len(tail) {
		return "", false, err
	}
	tailStart := 0
	for tailStart < len(tail) && tail[tailStart]&0xc0 == 0x80 {
		tailStart++
	}
	// The head is decoded in stream mode and never flushed, so a character
	// the cut splits is held back and dropped.
	var headDecoder jstext.TextDecoder
	omitted := size - int64(len(head)) - int64(len(tail))
	return fmt.Sprintf("%s\n\n[... %d bytes omitted ...]\n\n%s",
		headDecoder.Decode(head), omitted, jstext.DecodeText(tail[tailStart:])), true, nil
}

func (a *outputAccumulator) getLastLineBytes() int { return a.currentLineBytes }

func (a *outputAccumulator) shouldUseTempFile() bool {
	return a.totalRawBytes > a.maxBytes || a.totalDecodedBytes > a.maxBytes || a.totalLines > a.maxLines
}

func (a *outputAccumulator) ensureTempFile() {
	if a.tempFilePath != "" {
		return
	}
	var rb [8]byte
	_, _ = rand.Read(rb[:]) // crypto/rand.Read never returns an error (Go 1.24)
	// pi: `${prefix}-${16 hex chars}.log` in the OS temp dir.
	a.tempFilePath = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%x.log", a.prefix, rb))
	f, err := os.Create(a.tempFilePath)
	if err != nil {
		a.tempFileErr = err
		return
	}
	a.tempFile = f
	for _, chunk := range a.rawChunks {
		a.writeTempFile(chunk)
	}
	a.rawChunks = nil
}

// writeTempFile writes data to the temp file until a write fails.
func (a *outputAccumulator) writeTempFile(data []byte) {
	if a.tempFile == nil || a.tempFileErr != nil {
		return
	}
	if _, err := a.tempFile.Write(data); err != nil {
		a.tempFileErr = err
	}
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
func (u *bashUpdater) finish() (outputSnapshot, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.acc.finish()
	u.clearTimerLocked()
	u.emitLocked()
	snap := u.acc.snapshot(true)
	err := u.acc.closeTempFile()
	return snap, err
}

// ---------------------------------------------------------------------------
// ls
// ---------------------------------------------------------------------------

// lsTool builds the ls tool against the local filesystem.
func lsTool(cwd string) agent.AgentTool { return lsToolOps(cwd, nil) }

// lsToolOps builds the ls tool against injectable file operations, porting pi's
// LsOperations seam (core/tools/ls.ts:37).
func lsToolOps(cwd string, custom *LsOperations) agent.AgentTool {
	ops := resolveLsOperations(custom)
	return agent.AgentTool{
		Name:        "ls",
		Label:       "ls",
		Description: fmt.Sprintf("List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to %d entries or %dKB (whichever is hit first).", lsDefaultLimit, DefaultMaxBytes/1024),
		Parameters: ai.Object(
			ai.Opt("path", ai.String("Directory to list (default: current directory)")),
			ai.Opt("limit", ai.Number("Maximum number of entries to return (default: 500)")),
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
			isDir, err := ops.Stat(ctx, dir)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", dir)
			}
			if !isDir {
				return agent.AgentToolResult{}, fmt.Errorf("Not a directory: %s", dir)
			}
			// Bare names, sorted case-insensitively below.
			names, err := ops.Readdir(ctx, dir)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Cannot read directory: %v", err)
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
				if entryIsDir, err := ops.Stat(ctx, filepath.Join(dir, name)); err == nil {
					if entryIsDir {
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

// relativizeFindResultPath relativizes a find result against the search root and
// normalizes it to posix separators (pi's relativizeFindResultPath). pi had to
// special-case results that are absolute but not prefixed by the search root,
// and searching a filesystem root ("/" or a Windows drive root), where its
// prefix slice ate the first character of the first segment (#6104);
// filepath.Rel handles both.
//
// A trailing separator on the input is carried across relativization, because
// fd marks directory results with one and pi's output keeps it. filepath.Rel
// (like pi's path.relative) drops it, so it is reattached here.
func relativizeFindResultPath(resultPath, searchPath string) string {
	hadTrailingSeparator := strings.HasSuffix(resultPath, "/") || strings.HasSuffix(resultPath, `\`)
	rel, err := filepath.Rel(searchPath, resultPath)
	if err != nil {
		rel = resultPath
	}
	rel = filepath.ToSlash(rel)
	if hadTrailingSeparator && !strings.HasSuffix(rel, "/") {
		rel += "/"
	}
	return rel
}

// findTool builds the find tool against the local filesystem.
func findTool(cwd string) agent.AgentTool { return findToolOps(cwd, nil) }

// findToolOps builds the find tool against injectable file operations, porting
// pi's FindOperations seam (core/tools/find.ts:55). pi branches on whether the
// CUSTOM operations supplied glob (`if (customOps?.glob)`), falling back to fd
// otherwise — so the custom value is kept alongside the resolved one.
func findToolOps(cwd string, custom *FindOperations) agent.AgentTool {
	ops := resolveFindOperations(custom)
	return agent.AgentTool{
		Name:        "find",
		Label:       "find",
		Description: fmt.Sprintf("Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to %d results or %dKB (whichever is hit first).", findDefaultLimit, DefaultMaxBytes/1024),
		Parameters: ai.Object(
			ai.Prop("pattern", ai.String("Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'")),
			ai.Opt("path", ai.String("Directory to search in (default: current directory)")),
			ai.Opt("limit", ai.Number("Maximum number of results (default: 1000)")),
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
			if ok, err := ops.Exists(ctx, root); err != nil || !ok {
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
				// fd matches directories as well as files, and marks them with a
				// trailing separator in its output.
				if matchFdGlob(pattern, rel, p) {
					result := p
					if d.IsDir() {
						result += string(filepath.Separator)
					}
					results = append(results, relativizeFindResultPath(result, root))
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

// grepTool builds the grep tool against the local filesystem.
func grepTool(cwd string) agent.AgentTool { return grepToolOps(cwd, nil) }

// grepToolOps builds the grep tool against injectable file operations, porting
// pi's GrepOperations seam (core/tools/grep.ts:56). See the divergence note on
// GrepOperations.ReadFile: here it is the primary scan read.
func grepToolOps(cwd string, custom *GrepOperations) agent.AgentTool {
	ops := resolveGrepOperations(custom)
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
			ai.Opt("context", ai.Number("Number of lines to show before and after each match (default: 0)")),
			ai.Opt("limit", ai.Number("Maximum number of matches to return (default: 100)")),
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

			rootIsDir, err := ops.IsDirectory(ctx, root)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", root)
			}
			isDir := rootIsDir

			var matchLines []string
			matchCount := 0
			matchLimitReached := false
			linesTruncated := false

			// searchFile scans one file; skipBinary mirrors rg's NUL sniff (a NUL
			// byte in the first 8KB marks the file binary; only applies during
			// directory traversal — explicitly-given files are always searched).
			searchFile := func(path, rel string, skipBinary bool) bool {
				data, err := ops.ReadFile(ctx, path)
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
