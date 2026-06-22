package coding

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// editEntry is a single oldText→newText replacement.
type editEntry struct {
	oldText string
	newText string
}

// isJSWhitespace reports whether r is in JS String.prototype.trimEnd's trim set
// (ECMAScript WhiteSpace ∪ LineTerminator): TAB VT FF SP NBSP ZWNBSP(U+FEFF),
// any Zs, LF CR LS PS. Unlike Go's unicode.IsSpace it includes U+FEFF and
// excludes U+0085 (NEL).
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\ufeff', '\u2028', '\u2029':
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// normalizeForFuzzyMatch normalizes text for whitespace/Unicode-tolerant matching
// (port of pi's normalizeForFuzzyMatch, edit-diff.ts:34). It applies Unicode NFKC,
// strips trailing per-line whitespace (JS trimEnd set), and folds smart quotes,
// dashes, and exotic spaces to ASCII.
func normalizeForFuzzyMatch(text string) string {
	text = norm.NFKC.String(text)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, isJSWhitespace)
	}
	joined := strings.Join(lines, "\n")
	return strings.Map(func(r rune) rune {
		switch r {
		case '‘', '’', '‚', '‛':
			return '\''
		case '“', '”', '„', '‟':
			return '"'
		case '‐', '‑', '‒', '–', '—', '―', '−':
			return '-'
		case ' ', ' ', ' ', ' ', ' ', ' ', ' ',
			' ', ' ', ' ', ' ', ' ', '　':
			return ' '
		}
		return r
	}, joined)
}

type fuzzyMatch struct {
	found       bool
	index       int // byte offset into the content used for replacement
	matchLength int // byte length of the match
	usedFuzzy   bool
}

// fuzzyFindText finds oldText in content, trying exact match then fuzzy
// (normalized) match (port of fuzzyFindText). When fuzzy, indices/lengths are in
// the normalized content space.
func fuzzyFindText(content, oldText string) fuzzyMatch {
	if i := strings.Index(content, oldText); i != -1 {
		return fuzzyMatch{found: true, index: i, matchLength: len(oldText), usedFuzzy: false}
	}
	fuzzyContent := normalizeForFuzzyMatch(content)
	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	if i := strings.Index(fuzzyContent, fuzzyOldText); i != -1 {
		return fuzzyMatch{found: true, index: i, matchLength: len(fuzzyOldText), usedFuzzy: true}
	}
	return fuzzyMatch{found: false, index: -1}
}

func countFuzzyOccurrences(content, oldText string) int {
	return strings.Count(normalizeForFuzzyMatch(content), normalizeForFuzzyMatch(oldText))
}

func stripBOM(content string) (bom, text string) {
	if strings.HasPrefix(content, "\ufeff") {
		return "\ufeff", content[len("\ufeff"):]
	}
	return "", content
}

type matchedEdit struct {
	editIndex   int
	matchIndex  int
	matchLength int
	newText     string
}

// splitLinesWithEndings splits content into lines that keep their trailing "\n"
// (port of pi's /[^\n]*\n|[^\n]+/g). A trailing "\n" yields no empty final
// element, and "" yields no elements — unlike strings.Split on "\n".
func splitLinesWithEndings(content string) []string {
	var out []string
	for i := 0; i < len(content); {
		if j := strings.IndexByte(content[i:], '\n'); j == -1 {
			out = append(out, content[i:])
			break
		} else {
			out = append(out, content[i:i+j+1])
			i += j + 1
		}
	}
	return out
}

type lineSpan struct{ start, end int }

// getLineSpans returns the byte [start,end) span of each line (with ending).
func getLineSpans(content string) []lineSpan {
	lines := splitLinesWithEndings(content)
	spans := make([]lineSpan, len(lines))
	offset := 0
	for i, line := range lines {
		spans[i] = lineSpan{start: offset, end: offset + len(line)}
		offset = spans[i].end
	}
	return spans
}

// getReplacementLineRange widens a replacement to the [startLine,endLine) lines
// it touches (port of pi's getReplacementLineRange). endLine is exclusive.
func getReplacementLineRange(lines []lineSpan, m matchedEdit) (startLine, endLine int, err error) {
	repStart, repEnd := m.matchIndex, m.matchIndex+m.matchLength
	startLine = -1
	for i, line := range lines {
		if repStart >= line.start && repStart < line.end {
			startLine = i
			break
		}
	}
	if startLine == -1 {
		return 0, 0, fmt.Errorf("Replacement range is outside the base content.")
	}
	endLine = startLine
	for endLine < len(lines) && lines[endLine].end < repEnd {
		endLine++
	}
	if endLine >= len(lines) {
		return 0, 0, fmt.Errorf("Replacement range is outside the base content.")
	}
	return startLine, endLine + 1, nil
}

// applyReplacements rewrites content with the given replacements, applied in
// reverse so earlier offsets stay valid (port of pi's applyReplacements). offset
// shifts each replacement's matchIndex into content-local coordinates.
func applyReplacements(content string, replacements []matchedEdit, offset int) string {
	result := content
	for i := len(replacements) - 1; i >= 0; i-- {
		r := replacements[i]
		mi := r.matchIndex - offset
		result = result[:mi] + r.newText + result[mi+r.matchLength:]
	}
	return result
}

// applyReplacementsPreservingUnchangedLines overlays line-level replacements
// matched against baseContent (a normalized view) onto originalContent, copying
// untouched lines back verbatim (port of pi's function of the same name). Touched
// line-blocks are rewritten from baseContent; the actual replacement ranges drive
// preservation so duplicate normalized lines can't align to the wrong occurrence.
func applyReplacementsPreservingUnchangedLines(originalContent, baseContent string, replacements []matchedEdit) (string, error) {
	originalLines := splitLinesWithEndings(originalContent)
	baseLines := getLineSpans(baseContent)
	if len(originalLines) != len(baseLines) {
		return "", fmt.Errorf("Cannot preserve unchanged lines because the base content has a different line count.")
	}

	sorted := append([]matchedEdit(nil), replacements...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].matchIndex < sorted[b].matchIndex })

	type group struct {
		startLine, endLine int
		replacements       []matchedEdit
	}
	var groups []group
	for _, r := range sorted {
		startLine, endLine, err := getReplacementLineRange(baseLines, r)
		if err != nil {
			return "", err
		}
		if n := len(groups); n > 0 && startLine < groups[n-1].endLine {
			if endLine > groups[n-1].endLine {
				groups[n-1].endLine = endLine
			}
			groups[n-1].replacements = append(groups[n-1].replacements, r)
			continue
		}
		groups = append(groups, group{startLine: startLine, endLine: endLine, replacements: []matchedEdit{r}})
	}

	var b strings.Builder
	originalLineIndex := 0
	for _, g := range groups {
		for _, line := range originalLines[originalLineIndex:g.startLine] {
			b.WriteString(line)
		}
		groupStartOffset := baseLines[g.startLine].start
		groupEndOffset := baseLines[g.endLine-1].end
		b.WriteString(applyReplacements(baseContent[groupStartOffset:groupEndOffset], g.replacements, groupStartOffset))
		originalLineIndex = g.endLine
	}
	for _, line := range originalLines[originalLineIndex:] {
		b.WriteString(line)
	}
	return b.String(), nil
}

// applyEditsToNormalizedContent applies edits to LF-normalized content using
// exact-then-fuzzy matching, with pi's uniqueness/overlap/no-change checks
// (port of applyEditsToNormalizedContent). Returns the base content used for
// matching and the resulting content.
func applyEditsToNormalizedContent(normalizedContent string, edits []editEntry, path string) (base, result string, err error) {
	n := len(edits)
	norm := make([]editEntry, n)
	for i, e := range edits {
		norm[i] = editEntry{oldText: normalizeToLF(e.oldText), newText: normalizeToLF(e.newText)}
		if norm[i].oldText == "" {
			return "", "", emptyOldTextError(path, i, n)
		}
	}

	anyFuzzy := false
	for _, e := range norm {
		if fuzzyFindText(normalizedContent, e.oldText).usedFuzzy {
			anyFuzzy = true
			break
		}
	}
	// Matching runs in fuzzy-normalized space when any edit needed fuzzy matching,
	// but unchanged lines are overlaid back from the original (pi 128330e3): the
	// returned base is always the LF-normalized original, not the fuzzy view.
	replacementBase := normalizedContent
	if anyFuzzy {
		replacementBase = normalizeForFuzzyMatch(normalizedContent)
	}

	matched := make([]matchedEdit, 0, n)
	for i, e := range norm {
		m := fuzzyFindText(replacementBase, e.oldText)
		if !m.found {
			return "", "", notFoundError(path, i, n)
		}
		if occ := countFuzzyOccurrences(replacementBase, e.oldText); occ > 1 {
			return "", "", duplicateError(path, i, n, occ)
		}
		matched = append(matched, matchedEdit{editIndex: i, matchIndex: m.index, matchLength: m.matchLength, newText: e.newText})
	}

	sort.Slice(matched, func(a, b int) bool { return matched[a].matchIndex < matched[b].matchIndex })
	for i := 1; i < len(matched); i++ {
		prev, cur := matched[i-1], matched[i]
		if prev.matchIndex+prev.matchLength > cur.matchIndex {
			return "", "", fmt.Errorf("edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions.", prev.editIndex, cur.editIndex, path)
		}
	}

	base = normalizedContent
	if anyFuzzy {
		result, err = applyReplacementsPreservingUnchangedLines(normalizedContent, replacementBase, matched)
		if err != nil {
			return "", "", err
		}
	} else {
		result = applyReplacements(replacementBase, matched, 0)
	}
	if base == result {
		return "", "", noChangeError(path, n)
	}
	return base, result, nil
}

func emptyOldTextError(path string, i, total int) error {
	if total == 1 {
		return fmt.Errorf("oldText must not be empty in %s.", path)
	}
	return fmt.Errorf("edits[%d].oldText must not be empty in %s.", i, path)
}

func notFoundError(path string, i, total int) error {
	if total == 1 {
		return fmt.Errorf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
	}
	return fmt.Errorf("Could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines.", i, path)
}

func duplicateError(path string, i, total, occ int) error {
	if total == 1 {
		return fmt.Errorf("Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", occ, path)
	}
	return fmt.Errorf("Found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique.", occ, i, path)
}

func noChangeError(path string, total int) error {
	if total == 1 {
		return fmt.Errorf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)
	}
	return fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
}
