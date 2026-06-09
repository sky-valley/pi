package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigDirName is pi's per-project/user config directory name.
const ConfigDirName = ".pi"

// AgentDir returns the global agent config directory (~/.pi/agent).
func AgentDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(ConfigDirName, "agent")
	}
	return filepath.Join(home, ConfigDirName, "agent")
}

// PackageDir returns the pi package root directory, mirroring pi's getPackageDir:
// honor PI_PACKAGE_DIR, else walk up from the executable until a package.json is
// found, else fall back to the executable's directory.
func PackageDir() string {
	if env := os.Getenv("PI_PACKAGE_DIR"); env != "" {
		return env
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	for {
		if fileExists(filepath.Join(dir, "package.json")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Dir(exe)
}

// ReadmePath returns the absolute path to the pi package README.md.
func ReadmePath() string { p, _ := filepath.Abs(filepath.Join(PackageDir(), "README.md")); return p }

// DocsPath returns the absolute path to the pi package docs directory.
func DocsPath() string { p, _ := filepath.Abs(filepath.Join(PackageDir(), "docs")); return p }

// ExamplesPath returns the absolute path to the pi package examples directory.
func ExamplesPath() string {
	p, _ := filepath.Abs(filepath.Join(PackageDir(), "examples"))
	return p
}

var contextFileCandidates = []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

func loadContextFileFromDir(dir string) (ContextFile, bool) {
	for _, name := range contextFileCandidates {
		p := filepath.Join(dir, name)
		if data, err := os.ReadFile(p); err == nil {
			return ContextFile{Path: p, Content: string(data)}, true
		}
	}
	return ContextFile{}, false
}

// LoadProjectContextFiles discovers AGENTS.md/CLAUDE.md context files: the global
// one under agentDir first, then each ancestor directory of cwd from root down to
// cwd. Mirrors loadProjectContextFiles.
func LoadProjectContextFiles(cwd string) []ContextFile {
	cwd, _ = filepath.Abs(cwd)
	agentDir := AgentDir()

	var files []ContextFile
	seen := map[string]bool{}

	if gc, ok := loadContextFileFromDir(agentDir); ok {
		files = append(files, gc)
		seen[gc.Path] = true
	}

	var ancestors []ContextFile
	current := cwd
	for {
		if cf, ok := loadContextFileFromDir(current); ok && !seen[cf.Path] {
			ancestors = append([]ContextFile{cf}, ancestors...)
			seen[cf.Path] = true
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	files = append(files, ancestors...)
	return files
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

// Skill is a discovered Agent Skill (SKILL.md with frontmatter).
type Skill struct {
	Name                   string
	Description            string
	FilePath               string
	BaseDir                string
	DisableModelInvocation bool
}

// SkillDiagnostic mirrors pi's ResourceDiagnostic for skill loading: a validation
// warning (or error) with the offending file path.
type SkillDiagnostic struct {
	Type    string // "warning" | "error"
	Message string
	Path    string
}

// Max name/description lengths per the Agent Skills spec (skills.ts:11,14).
const (
	maxSkillNameLength        = 64
	maxSkillDescriptionLength = 1024
)

var skillIgnoreFileNames = []string{".gitignore", ".ignore", ".fdignore"}

// LoadSkills discovers skills under the user (~/.pi/agent/skills) and project
// (<cwd>/.pi/skills) skill directories. Diagnostics are discarded; see
// LoadSkillsWithDiagnostics.
func LoadSkills(cwd string) []Skill {
	skills, _ := LoadSkillsWithDiagnostics(cwd)
	return skills
}

// LoadSkillsWithDiagnostics is LoadSkills but also returns validation diagnostics.
func LoadSkillsWithDiagnostics(cwd string) ([]Skill, []SkillDiagnostic) {
	var skills []Skill
	var diags []SkillDiagnostic
	seen := map[string]bool{}
	add := func(found []Skill, d []SkillDiagnostic) {
		diags = append(diags, d...)
		for _, s := range found {
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			skills = append(skills, s)
		}
	}
	s1, d1 := loadSkillsFromDir(filepath.Join(AgentDir(), "skills"))
	add(s1, d1)
	s2, d2 := loadSkillsFromDir(filepath.Join(cwd, ConfigDirName, "skills"))
	add(s2, d2)
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, diags
}

// loadSkillsFromDir scans a directory for skills (port of loadSkillsFromDir).
// Discovery rules:
//   - a directory containing SKILL.md is a skill root (no further recursion);
//   - otherwise load direct .md children of the root, and recurse into
//     subdirectories looking for SKILL.md;
//   - honor .gitignore/.ignore/.fdignore, skip node_modules, follow symlinks but
//     realpath-dedup so a symlink loop or duplicate target is visited once.
func loadSkillsFromDir(dir string) ([]Skill, []SkillDiagnostic) {
	return loadSkillsFromDirInternal(dir, dir, true, newSkillIgnore(), map[string]bool{})
}

func loadSkillsFromDirInternal(dir, root string, includeRootFiles bool, ig *skillIgnore, visited map[string]bool) ([]Skill, []SkillDiagnostic) {
	var skills []Skill
	var diags []SkillDiagnostic

	if !dirExists(dir) {
		return skills, diags
	}
	// realpath-dedup: skip a directory whose canonical path was already visited
	// (guards symlink cycles and duplicate symlink targets).
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		if visited[real] {
			return skills, diags
		}
		visited[real] = true
	}

	ig.addRules(dir, root)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return skills, diags
	}

	// First pass: a SKILL.md in this dir makes it a skill root (stop recursion).
	for _, e := range entries {
		if e.Name() != "SKILL.md" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		isFile, ok := statIsFile(full, e)
		if !ok {
			continue
		}
		rel := toPosix(relPath(root, full))
		if !isFile || ig.ignores(rel, false) {
			continue
		}
		s, d := loadSkillFromFile(full)
		diags = append(diags, d...)
		if s != nil {
			skills = append(skills, *s)
		}
		return skills, diags
	}

	// Second pass: recurse into subdirs and (at the root) load direct .md files.
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		full := filepath.Join(dir, name)
		isDir, isFile := statIsDirFile(full, e)

		rel := toPosix(relPath(root, full))
		ignorePath := rel
		if isDir {
			ignorePath = rel + "/"
		}
		if ig.ignores(ignorePath, isDir) {
			continue
		}

		if isDir {
			s, d := loadSkillsFromDirInternal(full, root, false, ig, visited)
			skills = append(skills, s...)
			diags = append(diags, d...)
			continue
		}

		if !isFile || !includeRootFiles || !strings.HasSuffix(name, ".md") {
			continue
		}
		s, d := loadSkillFromFile(full)
		diags = append(diags, d...)
		if s != nil {
			skills = append(skills, *s)
		}
	}

	return skills, diags
}

// loadSkillFromFile parses one skill markdown file (port of loadSkillFromFile).
// The skill loads even with name/description warnings, except when description is
// missing entirely.
func loadSkillFromFile(filePath string) (*Skill, []SkillDiagnostic) {
	var diags []SkillDiagnostic
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, []SkillDiagnostic{{Type: "warning", Message: err.Error(), Path: filePath}}
	}
	fm, _ := parseFrontmatter(string(data))
	skillDir := filepath.Dir(filePath)

	desc := fm["description"]
	for _, e := range validateDescription(desc) {
		diags = append(diags, SkillDiagnostic{Type: "warning", Message: e, Path: filePath})
	}

	name := fm["name"]
	if name == "" {
		name = filepath.Base(skillDir)
	}
	for _, e := range validateName(name) {
		diags = append(diags, SkillDiagnostic{Type: "warning", Message: e, Path: filePath})
	}

	if strings.TrimSpace(desc) == "" {
		return nil, diags
	}
	return &Skill{
		Name:                   name,
		Description:            desc,
		FilePath:               filePath,
		BaseDir:                skillDir,
		DisableModelInvocation: fm["disable-model-invocation"] == "true",
	}, diags
}

// validateName ports pi's validateName (skills.ts:92-112).
func validateName(name string) []string {
	var errs []string
	if len(name) > maxSkillNameLength {
		errs = append(errs, fmt.Sprintf("name exceeds %d characters (%d)", maxSkillNameLength, len(name)))
	}
	if !isValidSkillName(name) {
		errs = append(errs, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errs = append(errs, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errs = append(errs, "name must not contain consecutive hyphens")
	}
	return errs
}

func isValidSkillName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

// validateDescription ports pi's validateDescription (skills.ts:117-127).
func validateDescription(desc string) []string {
	var errs []string
	if strings.TrimSpace(desc) == "" {
		errs = append(errs, "description is required")
	} else if len(desc) > maxSkillDescriptionLength {
		errs = append(errs, fmt.Sprintf("description exceeds %d characters (%d)", maxSkillDescriptionLength, len(desc)))
	}
	return errs
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func relPath(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return r
}

func toPosix(p string) string { return filepath.ToSlash(p) }

// statIsFile resolves whether full is a regular file, following symlinks.
func statIsFile(full string, e os.DirEntry) (isFile, ok bool) {
	if e.Type()&os.ModeSymlink != 0 {
		info, err := os.Stat(full)
		if err != nil {
			return false, false
		}
		return info.Mode().IsRegular(), true
	}
	return e.Type().IsRegular(), true
}

// statIsDirFile resolves dir/file-ness following symlinks. A broken symlink
// returns (false,false) so the caller skips it.
func statIsDirFile(full string, e os.DirEntry) (isDir, isFile bool) {
	if e.Type()&os.ModeSymlink != 0 {
		info, err := os.Stat(full)
		if err != nil {
			return false, false
		}
		return info.IsDir(), info.Mode().IsRegular()
	}
	return e.IsDir(), e.Type().IsRegular()
}

// parseFrontmatter extracts a YAML-ish `--- ... ---` header into a flat string
// map and returns the remaining body. It handles the simple scalar key: value
// frontmatter used by skills without a YAML dependency.
func parseFrontmatter(content string) (map[string]string, string) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	fm := map[string]string{}
	if !strings.HasPrefix(normalized, "---") {
		return fm, normalized
	}
	end := strings.Index(normalized[3:], "\n---")
	if end == -1 {
		return fm, normalized
	}
	yamlPart := normalized[4 : 3+end]
	body := strings.TrimSpace(normalized[3+end+4:])
	for _, line := range strings.Split(yamlPart, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx == -1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		fm[key] = val
	}
	return fm, body
}

// skillIgnore accumulates gitignore-style rules from .gitignore/.ignore/.fdignore
// files found while descending the skill tree (port of addIgnoreRules + the
// `ignore` npm matcher). Patterns are stored already prefixed with their
// directory's root-relative path, mirroring pi's prefixIgnorePattern.
type skillIgnore struct {
	rules []skillIgnoreRule
	seen  map[string]bool // dirs whose ignore files were already loaded
}

type skillIgnoreRule struct {
	pattern string // prefixed, slashes normalized, leading "/" stripped
	negated bool
	dirOnly bool
}

func newSkillIgnore() *skillIgnore {
	return &skillIgnore{seen: map[string]bool{}}
}

// addRules loads the ignore files in dir (if not already loaded), prefixing each
// pattern with dir's path relative to root.
func (ig *skillIgnore) addRules(dir, root string) {
	if ig.seen[dir] {
		return
	}
	ig.seen[dir] = true

	rel := relPath(root, dir)
	prefix := ""
	if rel != "." && rel != "" {
		prefix = toPosix(rel) + "/"
	}

	for _, fname := range skillIgnoreFileNames {
		p := filepath.Join(dir, fname)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			if rule, ok := prefixIgnorePattern(line, prefix); ok {
				ig.rules = append(ig.rules, rule)
			}
		}
	}
}

// prefixIgnorePattern ports skills.ts prefixIgnorePattern: trims comments/blank,
// handles "!"/"\!" negation and "\#" escapes, strips a leading "/", and prefixes
// the pattern with the directory prefix.
func prefixIgnorePattern(line, prefix string) (skillIgnoreRule, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return skillIgnoreRule{}, false
	}
	if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "\\#") {
		return skillIgnoreRule{}, false
	}

	pattern := line
	negated := false
	if strings.HasPrefix(pattern, "!") {
		negated = true
		pattern = pattern[1:]
	} else if strings.HasPrefix(pattern, "\\!") {
		pattern = pattern[1:]
	}
	if strings.HasPrefix(pattern, "/") {
		pattern = pattern[1:]
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return skillIgnoreRule{}, false
	}
	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")

	return skillIgnoreRule{pattern: prefix + pattern, negated: negated, dirOnly: dirOnly}, true
}

// ignores reports whether the root-relative posix path is ignored. The last
// matching rule wins; a negated match un-ignores.
func (ig *skillIgnore) ignores(relPosix string, isDir bool) bool {
	relPosix = strings.TrimSuffix(relPosix, "/")
	ignored := false
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if gitignoreMatchPath(r.pattern, relPosix) {
			ignored = !r.negated
		}
	}
	return ignored
}

// gitignoreMatchPath reports whether path (root-relative posix) matches a
// gitignore pattern. Patterns without a "/" match on any path component
// (basename); anchored patterns match from the root. A directory pattern also
// matches descendants.
func gitignoreMatchPath(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		// Unanchored: match the basename of any path segment.
		base := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base = path[i+1:]
		}
		if ok, _ := filepath.Match(pattern, base); ok {
			return true
		}
		// Also ignore everything beneath a matched directory segment.
		for _, seg := range strings.Split(path, "/") {
			if ok, _ := filepath.Match(pattern, seg); ok {
				return true
			}
		}
		return false
	}
	// Anchored: match the full path, or any ancestor directory of it.
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	if strings.HasPrefix(path, pattern+"/") {
		return true
	}
	return false
}

// FormatSkillsForPrompt renders visible skills as the Agent Skills XML block.
func FormatSkillsForPrompt(skills []Skill) string {
	var visible []Skill
	for _, s := range skills {
		if !s.DisableModelInvocation {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}
	lines := []string{
		"\n\nThe following skills provide specialized instructions for specific tasks.",
		"Use the read tool to load a skill's file when the task matches its description.",
		"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.",
		"",
		"<available_skills>",
	}
	for _, s := range visible {
		lines = append(lines,
			"  <skill>",
			"    <name>"+escapeXML(s.Name)+"</name>",
			"    <description>"+escapeXML(s.Description)+"</description>",
			"    <location>"+escapeXML(s.FilePath)+"</location>",
			"  </skill>",
		)
	}
	lines = append(lines, "</available_skills>")
	return strings.Join(lines, "\n")
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}
