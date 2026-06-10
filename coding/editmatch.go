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
	base = normalizedContent
	if anyFuzzy {
		base = normalizeForFuzzyMatch(normalizedContent)
	}

	matched := make([]matchedEdit, 0, n)
	for i, e := range norm {
		m := fuzzyFindText(base, e.oldText)
		if !m.found {
			return "", "", notFoundError(path, i, n)
		}
		if occ := countFuzzyOccurrences(base, e.oldText); occ > 1 {
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

	result = base
	for i := len(matched) - 1; i >= 0; i-- {
		m := matched[i]
		result = result[:m.matchIndex] + m.newText + result[m.matchIndex+m.matchLength:]
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
