package coding

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
)

func encodeBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// matchFdGlob reports whether a relative path matches a glob pattern using fd
// --glob semantics (find.ts:238-246):
//   - a pattern without "/" matches against the basename;
//   - a pattern with "/" matches against the full path; fd prepends "**/" unless
//     the pattern starts with "/", "**/", or is exactly "**".
func matchFdGlob(pattern, rel string) bool {
	pattern = filepath.ToSlash(pattern)
	rel = filepath.ToSlash(rel)

	if !strings.Contains(pattern, "/") {
		return matchGlobSegments(pattern, filepath.Base(rel))
	}

	effective := pattern
	if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && pattern != "**" {
		effective = "**/" + pattern
	}
	effective = strings.TrimPrefix(effective, "/")
	return matchGlobSegments(effective, rel)
}

// matchGlobSegments matches a "/"-segmented glob (supporting "**") against name.
func matchGlobSegments(pattern, name string) bool {
	if !strings.Contains(pattern, "**") {
		ok, _ := filepath.Match(pattern, name)
		return ok
	}
	return matchDoubleStar(pattern, name)
}

// matchDoubleStar matches a glob with "**" segments against a slash path.
func matchDoubleStar(pattern, name string) bool {
	pParts := strings.Split(pattern, "/")
	nParts := strings.Split(name, "/")
	return matchParts(pParts, nParts)
}

func matchParts(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			// "**" matches zero or more path segments.
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchParts(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := filepath.Match(pattern[0], name[0]); !ok {
			return false
		}
		pattern = pattern[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// ignorePattern is a single parsed .gitignore rule, anchored to the directory
// that declared it (dir is root-relative slash path, "" for the search root).
type ignorePattern struct {
	dir      string // root-relative directory the .gitignore lives in
	pattern  string // the raw pattern (slashes normalized), leading "/" stripped
	anchored bool   // pattern was anchored to dir (contained a non-trailing "/")
	dirOnly  bool   // pattern ended with "/"
	negated  bool
}

// ignoreStack applies hierarchical .gitignore semantics in pure Go: each
// directory's .gitignore is loaded lazily and applies to that directory's
// subtree. It always ignores .git and node_modules (fd default ignore set).
type ignoreStack struct {
	root   string
	loaded map[string][]ignorePattern // keyed by root-relative dir
}

func newIgnoreStack(root string) *ignoreStack {
	return &ignoreStack{root: root, loaded: map[string][]ignorePattern{}}
}

func parseGitignore(dir string, data []byte) []ignorePattern {
	var out []ignorePattern
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		neg := false
		if strings.HasPrefix(trimmed, "!") {
			neg = true
			trimmed = trimmed[1:]
		}
		dirOnly := strings.HasSuffix(trimmed, "/")
		trimmed = strings.TrimSuffix(trimmed, "/")
		p := filepath.ToSlash(trimmed)
		anchored := strings.HasPrefix(p, "/") || strings.Contains(strings.TrimSuffix(p, "/"), "/")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			continue
		}
		out = append(out, ignorePattern{dir: dir, pattern: p, anchored: anchored, dirOnly: dirOnly, negated: neg})
	}
	return out
}

// patternsFor loads (lazily) the .gitignore in the given root-relative dir.
func (s *ignoreStack) patternsFor(relDir string) []ignorePattern {
	if pats, ok := s.loaded[relDir]; ok {
		return pats
	}
	var abs string
	if relDir == "" {
		abs = s.root
	} else {
		abs = filepath.Join(s.root, filepath.FromSlash(relDir))
	}
	pats := []ignorePattern{}
	if data, err := os.ReadFile(filepath.Join(abs, ".gitignore")); err == nil {
		pats = parseGitignore(relDir, data)
	}
	s.loaded[relDir] = pats
	return pats
}

// ancestorDirs returns the chain of root-relative directories from root ("")
// down to (and including) the directory containing rel.
func ancestorDirs(rel string) []string {
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	dirs := []string{""}
	cur := ""
	// All path components except the last are directories that may hold .gitignore.
	for i := 0; i < len(parts)-1; i++ {
		if cur == "" {
			cur = parts[i]
		} else {
			cur = cur + "/" + parts[i]
		}
		dirs = append(dirs, cur)
	}
	return dirs
}

// ignored reports whether the root-relative path rel (a file or dir) is ignored.
// abs is the absolute path (unused today; kept for signature symmetry with a
// future absolute-path ignore source).
func (s *ignoreStack) ignored(_, rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	// fd default ignores.
	if base == ".git" || base == "node_modules" {
		return true
	}

	result := false
	for _, dir := range ancestorDirs(rel) {
		// Path relative to the gitignore's directory.
		var relToDir string
		if dir == "" {
			relToDir = rel
		} else {
			relToDir = strings.TrimPrefix(rel, dir+"/")
		}
		for _, p := range s.patternsFor(dir) {
			// Directory-only patterns only match directories (or ancestors of a
			// path, handled via the prefix check in gitignoreMatch).
			if p.dirOnly && !isDir && !strings.Contains(relToDir, "/") {
				continue
			}
			if gitignoreMatch(p, relToDir) {
				result = !p.negated
			}
		}
	}
	return result
}

// gitignoreMatch reports whether a pattern matches relToDir (path relative to
// the gitignore's own directory).
func gitignoreMatch(p ignorePattern, relToDir string) bool {
	relToDir = filepath.ToSlash(relToDir)
	if p.anchored {
		// Anchored: match the full relative path, or any ancestor dir prefix.
		if ok, _ := filepath.Match(p.pattern, relToDir); ok {
			return true
		}
		if strings.HasPrefix(relToDir+"/", p.pattern+"/") {
			return true
		}
		return false
	}
	// Unanchored: match against any path component / suffix.
	segments := strings.Split(relToDir, "/")
	for i := range segments {
		if ok, _ := filepath.Match(p.pattern, segments[i]); ok {
			return true
		}
		// Match a multi-segment suffix too (e.g. "a/b").
		suffix := strings.Join(segments[i:], "/")
		if ok, _ := filepath.Match(p.pattern, suffix); ok {
			return true
		}
	}
	return false
}
