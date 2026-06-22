package coding

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

func encodeBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// ---------------------------------------------------------------------------
// Glob primitives (shared by the fd-style find matcher, the rg-style grep
// matcher, and the gitignore engine).
// ---------------------------------------------------------------------------

// expandBraces expands {a,b} alternations (globset semantics, used by fd and
// rg patterns). Nested braces are expanded recursively. Patterns without
// braces are returned as-is.
func expandBraces(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start == -1 {
		return []string{pattern}
	}
	depth := 0
	end := -1
	for i := start; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		return []string{pattern}
	}
	inner := pattern[start+1 : end]
	var alts []string
	depth = 0
	last := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				alts = append(alts, inner[last:i])
				last = i + 1
			}
		}
	}
	alts = append(alts, inner[last:])
	var out []string
	for _, a := range alts {
		out = append(out, expandBraces(pattern[:start]+a+pattern[end+1:])...)
	}
	return out
}

// segMatch matches a single path segment against a glob segment.
// "[!x]" negated classes are translated to Go's "[^x]"; fold lowers both
// sides (smart-case support).
func segMatch(pat, seg string, fold bool) bool {
	pat = strings.ReplaceAll(pat, "[!", "[^")
	if fold {
		pat = strings.ToLower(pat)
		seg = strings.ToLower(seg)
	}
	ok, _ := filepath.Match(pat, seg)
	return ok
}

// globMatchPath matches a "/"-segmented glob (supporting "**" crossing
// slashes and {a,b} alternation) against a slash path.
func globMatchPath(pattern, name string, fold bool) bool {
	for _, p := range expandBraces(pattern) {
		if matchGlobOne(p, name, fold) {
			return true
		}
	}
	return false
}

func matchGlobOne(pattern, name string, fold bool) bool {
	if pattern == "**" {
		return true
	}
	return matchParts(strings.Split(pattern, "/"), strings.Split(name, "/"), fold)
}

func matchParts(pattern, name []string, fold bool) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				// A trailing "/**" requires at least one more component
				// ("a/**" matches "a/b" but not "a" itself, like git/globset).
				return len(name) >= 1
			}
			// "**" matches zero or more path segments.
			for i := 0; i <= len(name); i++ {
				if matchParts(pattern[1:], name[i:], fold) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if !segMatch(pattern[0], name[0], fold) {
			return false
		}
		pattern = pattern[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// patternHasUpper reports whether the pattern contains an uppercase letter
// (fd smart-case: all-lowercase patterns match case-insensitively).
func patternHasUpper(pattern string) bool {
	return strings.ContainsFunc(pattern, unicode.IsUpper)
}

// matchFdGlob reports whether a candidate matches a glob pattern using fd
// --glob semantics (find.ts:238-246):
//   - a pattern without "/" matches against the basename;
//   - a pattern with "/" puts fd in --full-path mode: it matches against the
//     absolute candidate path, and fd prepends "**/" unless the pattern starts
//     with "/", "**/", or is exactly "**";
//   - smart-case: an all-lowercase pattern matches case-insensitively;
//   - {a,b} alternation and [!x] classes are supported (globset).
func matchFdGlob(pattern, rel, abs string) bool {
	pattern = filepath.ToSlash(pattern)
	fold := !patternHasUpper(pattern)
	if !strings.Contains(pattern, "/") {
		return globMatchPath(pattern, filepath.Base(filepath.ToSlash(rel)), fold)
	}
	effective := pattern
	if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && pattern != "**" {
		effective = "**/" + pattern
	}
	return globMatchPath(effective, filepath.ToSlash(abs), fold)
}

// matchRgGlob reports whether a root-relative path matches a glob using
// ripgrep -g semantics: a pattern without "/" matches the basename; a pattern
// containing "/" is anchored to the search root (rg does NOT prepend "**/").
// rg -g globs are case-sensitive.
func matchRgGlob(pattern, rel string) bool {
	pattern = filepath.ToSlash(pattern)
	rel = filepath.ToSlash(rel)
	if !strings.Contains(pattern, "/") {
		return globMatchPath(pattern, filepath.Base(rel), false)
	}
	return globMatchPath(strings.TrimPrefix(pattern, "/"), rel, false)
}

// ---------------------------------------------------------------------------
// gitignore engine
// ---------------------------------------------------------------------------

// ignorePattern is a single parsed .gitignore rule.
type ignorePattern struct {
	pattern  string // pattern with slashes normalized, leading "/" stripped
	anchored bool   // pattern contained a non-trailing "/" → anchored to its base dir
	dirOnly  bool   // pattern ended with "/"
	negated  bool
}

// ignoreSource is a pattern list anchored at an absolute base directory
// (global excludes file, .git/info/exclude, or an ancestor .gitignore).
type ignoreSource struct {
	baseAbs string
	pats    []ignorePattern
	// boundaryExempt sources apply across nested-repo boundaries (the global
	// core.excludesFile, which git honors in every repo). Repo-specific sources
	// (.git/info/exclude, ancestor .gitignores) are not exempt: they belong to
	// the outer repo and must not leak into a nested repository.
	boundaryExempt bool
}

// ignoreStack applies hierarchical gitignore semantics in pure Go.
//
// Engine parity (tracker H4 empirics):
//   - find (fd --no-require-git): gitignore applies whether or not the root is
//     inside a git repository (requireGit=false);
//   - grep (rg): gitignore applies ONLY inside a git repository (requireGit=true);
//   - node_modules is NOT hard-ignored (only if gitignored);
//   - ".git" itself is always skipped;
//   - inside a repo, .git/info/exclude and the global core.excludesFile apply,
//     as do .gitignore files between the repo root and the search root.
type ignoreStack struct {
	root         string
	useGitignore bool
	repoRoot     string
	// boundaries, when set, makes outer-repo ignore rules stop at nested
	// repository boundaries (a descendant directory holding its own .git), so a
	// parent .gitignore no longer leaks into a checked-out sub-repo. fd does this
	// in its default git-aware mode; it is OFF under --no-require-git (outside any
	// repo), so this only activates when the search root is itself inside a repo.
	boundaries bool
	static     []ignoreSource             // global excludes, info/exclude, ancestor .gitignores (in precedence order)
	loaded     map[string][]ignorePattern // per root-relative dir .gitignore
	gitDir     map[string]bool            // abs dir → contains a .git entry (cached)
}

func newIgnoreStack(root string, requireGit bool, respectNestedRepos bool) *ignoreStack {
	s := &ignoreStack{root: root, loaded: map[string][]ignorePattern{}, gitDir: map[string]bool{}}
	s.repoRoot = findRepoRoot(root)
	s.useGitignore = !requireGit || s.repoRoot != ""
	// Nested-repo boundaries only apply in git-aware mode (search root inside a
	// repo). Under --no-require-git outside a repo, fd ignores boundaries.
	s.boundaries = respectNestedRepos && s.repoRoot != ""
	if s.repoRoot != "" && s.useGitignore {
		// Lowest precedence first; later sources win on conflicts.
		if p := globalExcludesPath(); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				s.static = append(s.static, ignoreSource{baseAbs: s.repoRoot, pats: parseGitignore(data), boundaryExempt: true})
			}
		}
		if data, err := os.ReadFile(filepath.Join(s.repoRoot, ".git", "info", "exclude")); err == nil {
			s.static = append(s.static, ignoreSource{baseAbs: s.repoRoot, pats: parseGitignore(data)})
		}
		// .gitignore files in ancestors of the search root (repo root downward).
		if s.root != s.repoRoot {
			var ancs []string
			for dir := filepath.Dir(s.root); ; dir = filepath.Dir(dir) {
				ancs = append(ancs, dir)
				if dir == s.repoRoot || filepath.Dir(dir) == dir {
					break
				}
			}
			for i := len(ancs) - 1; i >= 0; i-- {
				if data, err := os.ReadFile(filepath.Join(ancs[i], ".gitignore")); err == nil {
					s.static = append(s.static, ignoreSource{baseAbs: ancs[i], pats: parseGitignore(data)})
				}
			}
		}
	}
	return s
}

// findRepoRoot walks up from dir looking for a .git entry (dir or file).
func findRepoRoot(dir string) string {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// globalExcludesPath resolves git's global excludes file: core.excludesFile if
// configured, else $XDG_CONFIG_HOME/git/ignore, else ~/.config/git/ignore.
func globalExcludesPath() string {
	if out, err := exec.Command("git", "config", "--path", "--get", "core.excludesFile").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "git", "ignore")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "git", "ignore")
	}
	return ""
}

func parseGitignore(data []byte) []ignorePattern {
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
		anchored := strings.Contains(p, "/")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			continue
		}
		out = append(out, ignorePattern{pattern: p, anchored: anchored, dirOnly: dirOnly, negated: neg})
	}
	return out
}

// hasGitDir reports whether the root-relative dir contains a .git entry (a
// repository boundary). Results are cached.
func (s *ignoreStack) hasGitDir(relDir string) bool {
	if v, ok := s.gitDir[relDir]; ok {
		return v
	}
	abs := s.root
	if relDir != "" {
		abs = filepath.Join(s.root, filepath.FromSlash(relDir))
	}
	_, err := os.Lstat(filepath.Join(abs, ".git"))
	v := err == nil
	s.gitDir[relDir] = v
	return v
}

// crossesNestedBoundary reports whether a rule source rooted at root-relative
// dir baseRel is separated from path rel by a nested repository: i.e. some
// directory strictly below baseRel and at-or-above rel's own directory holds a
// .git. Such a source belongs to an outer repo and must not apply to rel.
func (s *ignoreStack) crossesNestedBoundary(baseRel, rel string) bool {
	for _, dir := range ancestorDirs(rel) {
		if dir == baseRel {
			continue
		}
		// strict descendant of baseRel?
		if baseRel == "" {
			if dir == "" {
				continue
			}
		} else if !strings.HasPrefix(dir, baseRel+"/") {
			continue
		}
		if s.hasGitDir(dir) {
			return true
		}
	}
	return false
}

// patternsFor loads (lazily) the .gitignore in the given root-relative dir.
func (s *ignoreStack) patternsFor(relDir string) []ignorePattern {
	if pats, ok := s.loaded[relDir]; ok {
		return pats
	}
	abs := s.root
	if relDir != "" {
		abs = filepath.Join(s.root, filepath.FromSlash(relDir))
	}
	pats := []ignorePattern{}
	if data, err := os.ReadFile(filepath.Join(abs, ".gitignore")); err == nil {
		pats = parseGitignore(data)
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

// ignored reports whether the path (abs absolute, rel root-relative) is ignored.
func (s *ignoreStack) ignored(abs, rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	// .git itself is always skipped.
	if filepath.Base(rel) == ".git" {
		return true
	}
	if !s.useGitignore {
		return false
	}

	result := false
	for _, src := range s.static {
		// Repo-specific outer sources stop at a nested-repo boundary; global
		// excludes (boundaryExempt) carry across, as git honors them everywhere.
		if s.boundaries && !src.boundaryExempt && s.crossesNestedBoundary("", rel) {
			continue
		}
		relToBase, err := filepath.Rel(src.baseAbs, abs)
		if err != nil {
			continue
		}
		rts := filepath.ToSlash(relToBase)
		for _, p := range src.pats {
			if gitignoreMatch(p, rts, isDir) {
				result = !p.negated
			}
		}
	}
	for _, dir := range ancestorDirs(rel) {
		// A .gitignore in an outer repo must not apply once a nested repository
		// begins below it (upstream 756a4e8f).
		if s.boundaries && s.crossesNestedBoundary(dir, rel) {
			continue
		}
		// Path relative to the gitignore's directory.
		relToDir := rel
		if dir != "" {
			relToDir = strings.TrimPrefix(rel, dir+"/")
		}
		for _, p := range s.patternsFor(dir) {
			if gitignoreMatch(p, relToDir, isDir) {
				result = !p.negated
			}
		}
	}
	return result
}

// gitignoreMatch reports whether a pattern matches relToDir (path relative to
// the pattern's base directory) per gitignore semantics:
//   - anchored patterns (containing a non-trailing "/") match the full relative
//     path; matching a parent directory ignores everything below it;
//   - unanchored patterns match any single path component;
//   - dir-only patterns ("x/") only match directories (or paths below a
//     matching directory) — never plain files;
//   - "**" crosses directory boundaries.
func gitignoreMatch(p ignorePattern, relToDir string, isDir bool) bool {
	relToDir = filepath.ToSlash(relToDir)
	if p.anchored {
		if globMatchPath(p.pattern, relToDir, false) {
			return !p.dirOnly || isDir
		}
		// A pattern matching an ancestor directory ignores everything below it.
		segs := strings.Split(relToDir, "/")
		prefix := ""
		for i := 0; i < len(segs)-1; i++ {
			if prefix == "" {
				prefix = segs[i]
			} else {
				prefix += "/" + segs[i]
			}
			if globMatchPath(p.pattern, prefix, false) {
				return true
			}
		}
		return false
	}
	// Unanchored: match against each path component; a hit on a non-final
	// component means the path is inside a matching directory.
	segs := strings.Split(relToDir, "/")
	for i, seg := range segs {
		if segMatch(p.pattern, seg, false) {
			if i < len(segs)-1 {
				return true
			}
			return !p.dirOnly || isDir
		}
	}
	return false
}
