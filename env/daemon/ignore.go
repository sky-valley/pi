package daemon

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Gitignore matching as the ignore crate (0.4.33) does it for listFiles, the
// crate fd and ripgrep use: the rules of a directory's .gitignore, of
// .git/info/exclude and of the global excludes file, the last matching rule
// of a file deciding, and the nearest file with a matching rule deciding
// between files.

type matchResult int

const (
	matchNone matchResult = iota
	matchIgnore
	matchWhitelist
)

type gitignoreRule struct {
	re        *regexp.Regexp
	whitelist bool
	onlyDir   bool
}

// gitignore is the rules of one or more gitignore files, matched relative to
// root.
type gitignore struct {
	root  string
	rules []gitignoreRule
}

// newGitignore reads the files at paths (missing ones are skipped) into a
// matcher for paths below root, as GitignoreBuilder::add does: a file's first
// line may start with a byte-order mark, and reading stops at the first line
// that is not UTF-8.
func newGitignore(root string, paths ...string) *gitignore {
	g := &gitignore{root: strings.TrimPrefix(root, "./")}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// BufRead::lines: lines end at "\n", which takes one "\r" before it along.
		for i := 0; len(data) > 0; i++ {
			line := data
			if end := bytes.IndexByte(data, '\n'); end >= 0 {
				line, data = bytes.TrimSuffix(data[:end], []byte("\r")), data[end+1:]
			} else {
				data = nil
			}
			if !utf8.Valid(line) {
				break
			}
			text := string(line)
			if i == 0 {
				for strings.HasPrefix(text, byteOrderMark) {
					text = text[len(byteOrderMark):]
				}
			}
			g.addLine(text)
		}
	}
	return g
}

// addLine is GitignoreBuilder::add_line; a line whose glob does not parse is
// skipped.
func (g *gitignore) addLine(line string) {
	if strings.HasPrefix(line, "#") {
		return
	}
	if !strings.HasSuffix(line, `\ `) {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	if line == "" {
		return
	}
	var rule gitignoreRule
	absolute := false
	if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
		absolute = strings.HasPrefix(line, "/")
	} else {
		if strings.HasPrefix(line, "!") {
			rule.whitelist = true
			line = line[1:]
		}
		if strings.HasPrefix(line, "/") {
			// Only matches at the start of the path: wildcards cannot match "/".
			line = line[1:]
			absolute = true
		}
	}
	// A trailing slash matches directories only and is not globbed.
	if strings.HasSuffix(line, "/") {
		rule.onlyDir = true
		line = line[:len(line)-1]
		line = strings.TrimSuffix(line, `\`)
	}
	actual := line
	// Without a slash, the glob matches anywhere.
	if !absolute && !strings.Contains(line, "/") {
		if !strings.HasPrefix(actual, "**/") && actual != "**" {
			actual = "**/" + actual
		}
	}
	// "/**" matches what is inside a directory, not the directory itself.
	if strings.HasSuffix(actual, "/**") {
		actual += "/*"
	}
	re, err := compileGlob(actual)
	if err != nil {
		return
	}
	rule.re = re
	g.rules = append(g.rules, rule)
}

// matched is Gitignore::matched: whether path (a file, or a directory with
// isDir) matches, decided by the last matching rule that applies.
func (g *gitignore) matched(path string, isDir bool) matchResult {
	if g == nil || len(g.rules) == 0 {
		return matchNone
	}
	candidate := latin1(normalizeCandidate(g.strip(path)))
	for i := len(g.rules) - 1; i >= 0; i-- {
		rule := g.rules[i]
		if rule.onlyDir && !isDir {
			continue
		}
		if rule.re.MatchString(candidate) {
			if rule.whitelist {
				return matchWhitelist
			}
			return matchIgnore
		}
	}
	return matchNone
}

// strip removes a leading "./" and the matcher's root from path, unless path
// is a bare name.
func (g *gitignore) strip(path string) string {
	path = strings.TrimPrefix(path, "./")
	if g.root != "." && !isBareName(path) {
		if rest, ok := stripPathPrefix(g.root, path); ok {
			path = strings.TrimPrefix(rest, "/")
		}
	}
	return path
}
