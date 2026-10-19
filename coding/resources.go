package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// ConfigDirName is pi's per-project/user config directory name.
const ConfigDirName = ".pi"

// AgentDir returns the global agent config directory (~/.pi/agent).
// It returns "" when the home directory cannot be determined (no HOME — which
// is ordinary in containers, CI, systemd units and cron). It must NOT fall back
// to a relative path: a relative path resolves against the process working
// directory, i.e. whatever repository the agent happens to be run in, so
// `.pi/agent` would make a hostile repo's files look like the user's own global
// config. pi cannot reach that state — its getHomeDir is
// `process.env.HOME || homedir()` and Node's homedir() consults the passwd
// database — so failing closed is the faithful answer as well as the safe one.
// Every caller must treat "" as "there is no agent directory".
func AgentDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ConfigDirName, "agent")
}

// PackageDir returns the pi package root directory, mirroring pi's getPackageDir:
// honor PI_PACKAGE_DIR, else walk up from the executable until a package.json is
// found, else fall back to the executable's directory. A dist/ holding only a
// build's copied metadata resolves to the package root above it.
func PackageDir() string {
	if env := os.Getenv("PI_PACKAGE_DIR"); env != "" {
		return env
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return findNodePackageDir(filepath.Dir(exe))
}

// findNodePackageDir walks up from start to the first directory holding a
// package.json, mirroring pi's findNodePackageDir — "Node" is upstream's name
// for the non-Bun-binary arm of getPackageDir. Builds that embed binary
// metadata leave a package.json inside dist/ as well; asset paths resolve
// against the package root, so a dist/ whose parent is also a package yields
// the parent instead — otherwise dist-relative paths become dist/dist/. Like
// pi, the filesystem root is never probed: start is the fallback. start is
// expected absolute and cleaned; the walk is lexical, like pi's dirname, so
// symlinks are not resolved.
func findNodePackageDir(start string) string {
	for dir := start; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if !fileExists(filepath.Join(dir, "package.json")) {
			continue
		}
		if filepath.Base(dir) == "dist" {
			if parent := filepath.Dir(dir); fileExists(filepath.Join(parent, "package.json")) {
				return parent
			}
		}
		return dir
	}
	return start
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

// AGENTS.override.md replaces the directory's AGENTS.md/CLAUDE.md when present
// (pi 8ecf8a988).
var contextFileCandidates = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

func loadContextFileFromDir(dir string) (ContextFile, bool) {
	for _, name := range contextFileCandidates {
		p := filepath.Join(dir, name)
		if data, err := os.ReadFile(p); err == nil {
			// A BOM-prefixed AGENTS.md/CLAUDE.md would otherwise inject a stray
			// U+FEFF into the system prompt, which is emitted verbatim
			// (pi 1355cd36e resource-loader.ts loadContextFileFromDir).
			return ContextFile{Path: p, Content: stripBOM(string(data))}, true
		}
	}
	return ContextFile{}, false
}

// canonicalizePath resolves symlinks, falling back to the input when the path
// cannot be resolved (pi utils/paths.ts canonicalizePath).
func canonicalizePath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// gitPaths carries the git metadata locations the context-file shadow check
// consumes. pi's GitPaths also has headPath, for the footer's HEAD watcher —
// that consumer is not ported, so neither is the field.
type gitPaths struct {
	repoDir      string
	commonGitDir string
}

// findGitPaths walks up from cwd for a .git entry, mirroring pi's findGitPaths
// (footer-data-provider.ts, exported there by cced6a21). It handles a regular
// repo, where .git is a directory, and a linked worktree, where .git is a file
// holding `gitdir: <path>` whose commondir points back at the main repo's git
// dir. Reports false when no repo is found, when the located git dir has no
// HEAD, or when a .git entry exists but cannot be read.
//
// glob.go's findRepoRoot cannot be reused: it only Lstats .git, so it stops at
// a linked worktree without resolving the gitdir:/commondir chain this needs.
func findGitPaths(cwd string) (gitPaths, bool) {
	dir := cwd
	for {
		gitPath := filepath.Join(dir, ".git")
		st, err := os.Stat(gitPath)
		switch {
		case err != nil:
			// pi guards on existsSync, which swallows any error and keeps
			// climbing; its try/catch is only reachable once that returned true.
		case st.Mode().IsRegular():
			content, rerr := os.ReadFile(gitPath)
			if rerr != nil {
				return gitPaths{}, false
			}
			// A .git file that is not a gitdir pointer falls through to the
			// parent walk, as it does in pi.
			if rest, ok := strings.CutPrefix(jstext.Trim(string(content)), "gitdir: "); ok {
				gitDir := resolveFrom(dir, jstext.Trim(rest))
				if !fileExists(filepath.Join(gitDir, "HEAD")) {
					return gitPaths{}, false
				}
				commonGitDir := gitDir
				if data, cerr := os.ReadFile(filepath.Join(gitDir, "commondir")); cerr == nil {
					commonGitDir = resolveFrom(gitDir, jstext.Trim(string(data)))
				}
				return gitPaths{repoDir: dir, commonGitDir: commonGitDir}, true
			}
		case st.IsDir():
			if !fileExists(filepath.Join(gitPath, "HEAD")) {
				return gitPaths{}, false
			}
			return gitPaths{repoDir: dir, commonGitDir: gitPath}, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return gitPaths{}, false
		}
		dir = parent
	}
}

// resolveFrom is Node's path.resolve(base, p) for a single segment: an absolute
// p wins, otherwise it is joined onto base.
func resolveFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// findShadowedContextFile returns the main repo's context file that a nested
// linked worktree's own copy shadows: both occupy the same logical repository
// scope, so loading both applies that context twice. Reports false when
// nothing is shadowed, leaving normal ancestor inheritance alone.
//
// Paths are canonicalized, because `git worktree add` writes the .git file's
// `gitdir:` target in realpath form while cwd may still be symlinked (macOS
// /tmp -> /private/tmp).
func findShadowedContextFile(cwd string) (string, bool) {
	gp, ok := findGitPaths(cwd)
	if !ok {
		return "", false
	}
	commonGitDir := canonicalizePath(gp.commonGitDir)
	worktreeRoot := canonicalizePath(gp.repoDir)
	mainRepoRoot := filepath.Dir(commonGitDir)
	// False for an ordinary repo, where the two are the same dir, and for a
	// sibling worktree (`git worktree add ../feat`), whose main repo is not an
	// ancestor.
	if !strings.HasPrefix(worktreeRoot, mainRepoRoot+string(filepath.Separator)) {
		return "", false
	}
	// The parent of the common git dir is the main worktree root only when that
	// dir is itself checked out from the same repo. In a bare layout
	// (proj/.bare + proj/main) it is just the directory holding .bare, which
	// tracks nothing; a submodule's gitdir has no commondir, so it lands under
	// .git/modules.
	if canonicalizePath(filepath.Join(mainRepoRoot, ".git")) != commonGitDir {
		return "", false
	}
	// Selection must go through loadContextFileFromDir, not a cheaper
	// existence check: an unreadable candidate is skipped in favour of the next
	// one there and in pi, so a name-only probe would shadow the wrong file.
	cf, ok := loadContextFileFromDir(worktreeRoot)
	if !ok {
		return "", false
	}
	return filepath.Join(mainRepoRoot, filepath.Base(cf.Path)), true
}

// LoadProjectContextFiles discovers context files (AGENTS.override.md, else
// AGENTS.md/CLAUDE.md): the global one under agentDir first, then each ancestor
// directory of cwd from root down to cwd. Mirrors loadProjectContextFiles.
func LoadProjectContextFiles(cwd string) []ContextFile {
	cwd, _ = filepath.Abs(cwd)
	agentDir := AgentDir()

	var files []ContextFile
	seen := map[string]bool{}

	// An absent agent dir (no home) contributes no GLOBAL context file. Without
	// this the relative fallback used to read <cwd>/.pi/agent/AGENTS.md — the
	// repository's own file — as though it were the user's global one.
	if gc, ok := loadContextFileFromDir(agentDir); ok && agentDir != "" {
		files = append(files, gc)
		seen[gc.Path] = true
	}

	// A nested linked worktree's context file shadows the main repo's copy of
	// the same tracked file; skip the shadowed one (pi cced6a21). The check
	// compares the walked directory's real path plus the file name, not the
	// file's real path: a worktree AGENTS.md that symlinks to the main repo's
	// file resolves to the shadowed path itself (upstream f1b2e77f5).
	shadowed, hasShadowed := findShadowedContextFile(cwd)

	var ancestors []ContextFile
	current := cwd
	for {
		if cf, ok := loadContextFileFromDir(current); ok &&
			!(hasShadowed && filepath.Join(canonicalizePath(current), filepath.Base(cf.Path)) == shadowed) &&
			!seen[cf.Path] {
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

// skillDiscoveryMode selects which markdown files count as skills alongside the
// SKILL.md roots (pi SkillDiscoveryMode). The two modes are mirror images: pi's
// own directories load ROOT-level .md files, and the AGENTS-convention
// directories load NESTED ones instead.
type skillDiscoveryMode int

const (
	skillModePi skillDiscoveryMode = iota
	skillModeAgents
)

// AgentsSkillsDir returns the user's AGENTS-convention skills directory
// (~/.agents/skills), the sibling of pi's own ~/.pi/agent/skills.
// It returns "" when the home directory cannot be determined, for the same
// reason AgentDir does — see there.
func AgentsSkillsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "skills")
}

// LoadSkills discovers skills under the USER skill directories only
// (~/.pi/agent/skills and ~/.agents/skills). The project directory
// <cwd>/.pi/skills is NOT scanned.
//
// That omission is pi's behavior, not a gap. pi discovers the project skills
// dir only inside `if (projectTrusted)` (package-manager.ts:2417 at
// ccfe79ed2), and resolveProjectTrusted answers FALSE for a host with no UI
// to ask with (`if (!options.projectTrustContext.hasUI) return false`,
// project-trust.ts). This port is headless by construction — it is an SDK plus
// a non-interactive CLI, and it ships no trust prompt — so untrusted is the
// faithful default and scanning unconditionally was a parity INVERSION on a
// security default: a skill's name and description go into the system prompt
// (FormatSkillsForPrompt) together with an instruction to read the file when
// the task matches, so a hostile repo could put attacker-authored text in
// front of the model just by being the cwd.
//
// A host that has actually established trust calls LoadSkillsWithTrust, or
// sets SessionOptions.TrustProject.
//
// Note pi's ancestor <project>/.agents/skills directories are a SEPARATE
// discovery that this port does not implement at all (2026-08-18 ruling);
// passing projectTrusted does not enable them.
//
// Diagnostics are discarded; see LoadSkillsWithDiagnostics.
func LoadSkills(cwd string) []Skill {
	skills, _ := LoadSkillsWithDiagnostics(cwd)
	return skills
}

// LoadSkillsWithDiagnostics is LoadSkills but also returns validation
// diagnostics. Like LoadSkills it treats the project as UNTRUSTED.
func LoadSkillsWithDiagnostics(cwd string) ([]Skill, []SkillDiagnostic) {
	return LoadSkillsWithTrust(cwd, false)
}

// sessionSkills is the session's skill discovery: LoadSkillsWithTrust with the
// diagnostics dropped, since a session has nowhere to surface them (pi reports
// them through its resource UI, which is host surface and unported).
func sessionSkills(cwd string, projectTrusted bool) []Skill {
	skills, _ := LoadSkillsWithTrust(cwd, projectTrusted)
	return skills
}

// LoadSkillsWithTrust is LoadSkillsWithDiagnostics with the project-trust
// decision supplied by the caller. Passing true scans <cwd>/.pi/skills, which
// is what pi does once isProjectTrusted() holds; passing false is pi's
// headless answer. Only a host that has established trust — by prompting, or
// by an explicit operator opt-in — may pass true.
func LoadSkillsWithTrust(cwd string, projectTrusted bool) ([]Skill, []SkillDiagnostic) {
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
	// pi preserves discovery order (skills.ts loadSkills: a name-keyed Map in
	// insertion order — user dir first, then project, filesystem order within
	// each). No sorting.
	// A USER skill directory is absolute by definition. AgentDir() and
	// AgentsSkillsDir() fall back to a RELATIVE path when os.UserHomeDir()
	// fails (no HOME — containers, CI, systemd, cron), and a relative path
	// resolves against the process cwd, which is the untrusted repository. That
	// would read <cwd>/.pi/agent/skills and <cwd>/.agents/skills as though they
	// were the user's own, walking straight around the project-trust gate
	// below. pi cannot reach this state — its getHomeDir is
	// `process.env.HOME || homedir()` and Node's homedir() consults the passwd
	// database rather than yielding a relative path — so skipping is both the
	// safe answer and the faithful one: with no home there IS no user dir.
	if d := AgentDir(); d != "" {
		dir := filepath.Join(d, "skills")
		s1, d1 := loadSkillsFromDir(dir)
		add(s1, d1)
	}
	// pi reaches this directory only under `if (projectTrusted)`. Discovery
	// ORDER is preserved either way: skipping the project dir cannot change how
	// a name already claimed by the user dir resolves, and cannot promote a
	// later dir into its slot, because add() is first-wins over a shared map.
	if projectTrusted {
		s2, d2 := loadSkillsFromDir(filepath.Join(cwd, ConfigDirName, "skills"))
		add(s2, d2)
	}
	// The AGENTS-convention user directory comes last, where upstream also puts
	// it among its four discovery calls. Since names dedup first-wins, appending
	// can only ADD skills that no pi directory already defines — it cannot change
	// how an existing name resolves. pi's ancestor <project>/.agents/skills dirs
	// are NOT discovered here: they are gated on project trust, which is not
	// ported (2026-06-12 ruling; see the 2026-08-18 ruling for this split).
	if dir := AgentsSkillsDir(); dir != "" {
		s3, d3 := loadSkillsFromDirMode(dir, skillModeAgents)
		add(s3, d3)
	}
	return skills, diags
}

// loadSkillsFromDir scans a pi skills directory (port of loadSkillsFromDir).
// Discovery rules:
//   - a directory containing SKILL.md is a skill root (no further recursion);
//   - otherwise load direct .md children of the root, and recurse into
//     subdirectories looking for SKILL.md;
//   - honor .gitignore/.ignore/.fdignore, skip node_modules, follow symlinks but
//     realpath-dedup so a symlink loop or duplicate target is visited once.
func loadSkillsFromDir(dir string) ([]Skill, []SkillDiagnostic) {
	return loadSkillsFromDirMode(dir, skillModePi)
}

// loadSkillsFromDirMode is loadSkillsFromDir with the discovery mode chosen:
// agents mode loads NESTED .md files and ignores root-level ones, the mirror of
// pi mode (upstream 5e11f6586).
func loadSkillsFromDirMode(dir string, mode skillDiscoveryMode) ([]Skill, []SkillDiagnostic) {
	return loadSkillsFromDirInternal(dir, dir, mode, newSkillIgnore(), map[string]bool{})
}

func loadSkillsFromDirInternal(dir, root string, mode skillDiscoveryMode, ig *skillIgnore, visited map[string]bool) ([]Skill, []SkillDiagnostic) {
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
			s, d := loadSkillsFromDirInternal(full, root, mode, ig, visited)
			skills = append(skills, s...)
			diags = append(diags, d...)
			continue
		}

		// pi: (mode === "pi" && dir === root) || (mode === "agents" && dir !== root).
		atRoot := dir == root
		includeMarkdown := (mode == skillModePi && atRoot) || (mode == skillModeAgents && !atRoot)
		if !isFile || !includeMarkdown || !strings.HasSuffix(name, ".md") {
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
// missing entirely. Markdown files not declared as skills (basename other than
// SKILL.md) with no usable description are silently skipped — no skill, no
// diagnostics (pi 8c2529dae "dont load root mds as skills"). pi also routes
// YAML parse errors to a SKILL.md-only warning; the port's forgiving line
// parser never fails, so that branch has no Go home.
func loadSkillFromFile(filePath string) (*Skill, []SkillDiagnostic) {
	var diags []SkillDiagnostic
	isDeclaredSkill := filepath.Base(filePath) == "SKILL.md"
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, []SkillDiagnostic{{Type: "warning", Message: err.Error(), Path: filePath}}
	}
	fm, _ := parseFrontmatter(string(data))
	skillDir := filepath.Dir(filePath)

	// pi 8c2529dae: only string-typed frontmatter values count for description
	// and name (typeof checks over a real YAML parse). The forgiving parser
	// keeps every scalar as its source text, so YAML-core non-string plain
	// scalars are screened by literal shape instead (fmValue.isString).
	desc := ""
	if v := fm["description"]; v.isString() {
		desc = v.value
	}
	hasDescription := jstext.Trim(desc) != ""
	if !isDeclaredSkill && !hasDescription {
		return nil, diags
	}

	for _, e := range validateDescription(desc) {
		diags = append(diags, SkillDiagnostic{Type: "warning", Message: e, Path: filePath})
	}

	name := ""
	if v := fm["name"]; v.isString() {
		name = v.value
	}
	if name == "" {
		name = filepath.Base(skillDir)
	}
	for _, e := range validateName(name) {
		diags = append(diags, SkillDiagnostic{Type: "warning", Message: e, Path: filePath})
	}

	if !hasDescription {
		return nil, diags
	}
	return &Skill{
		Name:        name,
		Description: desc,
		FilePath:    filePath,
		BaseDir:     skillDir,
		// pi: frontmatter["disable-model-invocation"] === true after a real YAML
		// parse — only the YAML boolean enables it. A quoted "true" parses to a
		// string and does NOT (skills.ts:316).
		DisableModelInvocation: fm["disable-model-invocation"].isBoolTrue(),
	}, diags
}

// validateName ports pi's validateName (skills.ts:92-112). Lengths are JS
// String.length — UTF-16 code units — not bytes.
func validateName(name string) []string {
	var errs []string
	if n := utf16Len(name); n > maxSkillNameLength {
		errs = append(errs, fmt.Sprintf("name exceeds %d characters (%d)", maxSkillNameLength, n))
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
	if jstext.Trim(desc) == "" {
		errs = append(errs, "description is required")
	} else if n := utf16Len(desc); n > maxSkillDescriptionLength {
		// JS String.length (UTF-16 code units), like pi.
		errs = append(errs, fmt.Sprintf("description exceeds %d characters (%d)", maxSkillDescriptionLength, n))
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

// fmValue is a parsed frontmatter scalar. kind distinguishes plain scalars
// (which can carry YAML booleans) from quoted strings and block scalars.
type fmValue struct {
	value string
	kind  fmKind
}

type fmKind int

const (
	fmPlain  fmKind = iota // unquoted scalar (may be a YAML bool)
	fmQuoted               // single/double-quoted string
	fmBlock                // block scalar (| or >)
)

// isBoolTrue reports whether the value is the YAML boolean true: a plain
// (unquoted) scalar parsing to true under the YAML core schema, as pi's `yaml`
// package produces for `=== true` checks. Quoted "true" is a string.
func (v fmValue) isBoolTrue() bool {
	return v.kind == fmPlain && (v.value == "true" || v.value == "True" || v.value == "TRUE")
}

// YAML 1.2 core schema int and float literal shapes (the schema pi's `yaml`
// parse resolves plain scalars against).
var (
	yamlCoreIntRe   = regexp.MustCompile(`^([-+]?[0-9]+|0o[0-7]+|0x[0-9a-fA-F]+)$`)
	yamlCoreFloatRe = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
)

// isString reports whether the scalar is a string under the YAML core schema:
// quoted and block scalars always are; a plain scalar is unless it is a
// null/bool/int/float literal (pi 8c2529dae reads only string-typed
// frontmatter values — `typeof x === "string"` over a real YAML parse).
func (v fmValue) isString() bool {
	if v.kind != fmPlain {
		return true
	}
	switch v.value {
	case "", "~", "null", "Null", "NULL",
		"true", "True", "TRUE", "false", "False", "FALSE",
		".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF", "-.inf", "-.Inf", "-.INF",
		".nan", ".NaN", ".NAN":
		return false
	}
	return !yamlCoreIntRe.MatchString(v.value) && !yamlCoreFloatRe.MatchString(v.value)
}

// parseFrontmatter extracts a `--- ... ---` YAML header into a flat scalar map
// and returns the remaining body (port of utils/frontmatter.ts, which uses the
// real `yaml` parser; this is a minimal-but-correct subset for the flat
// key/scalar frontmatter skills use).
//
// Supported: `key: value` plain scalars (with ` #` comment stripping), single/
// double-quoted strings (with \\ \" \n \t escapes in double quotes), block
// scalars (|, >, with -/+ chomping) including multi-line folded descriptions
// (`description: >-`), and multi-line plain scalars folded across continuation
// lines. NOT supported (out of scope for skill frontmatter): nested mappings,
// sequences/lists, flow collections ({}/[]), anchors/aliases/tags, explicit
// block-scalar indentation indicators, and more-indented literal lines inside
// folded scalars (folded with spaces like regular lines).
func parseFrontmatter(content string) (map[string]fmValue, string) {
	// Strip the BOM before the "---" test below: without it a BOM-prefixed
	// SKILL.md fails HasPrefix and silently loses its whole frontmatter — name
	// and description included (pi 1355cd36e).
	//
	// The nesting mirrors upstream's normalizeNewlines(stripBom(content)), but
	// only as a parity note — it is observationally inert. The two transforms
	// commute for every input: newline normalization cannot touch a leading
	// U+FEFF and cannot produce one, so no test can (or does) pin the order.
	normalized := strings.ReplaceAll(strings.ReplaceAll(stripBOM(content), "\r\n", "\n"), "\r", "\n")
	fm := map[string]fmValue{}
	if !strings.HasPrefix(normalized, "---") {
		return fm, normalized
	}
	end := strings.Index(normalized[3:], "\n---")
	if end == -1 {
		return fm, normalized
	}
	yamlPart := normalized[4 : 3+end]
	body := jstext.Trim(normalized[3+end+4:])

	lines := strings.Split(yamlPart, "\n")
	inDocument := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !inDocument {
			// Until the document begins, the yaml lexer drops one U+FEFF
			// from the start of every line it reads.
			line = strings.TrimPrefix(line, "\ufeff")
		}
		trimmed := strings.Trim(line, yamlSpace)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		inDocument = true
		if line[0] == ' ' || line[0] == '\t' {
			continue // continuation lines are consumed by their key below
		}
		idx := strings.Index(line, ":")
		if idx == -1 {
			continue
		}
		key := strings.Trim(line[:idx], yamlSpace)
		rest := strings.Trim(line[idx+1:], yamlSpace)

		// Block scalar: | or > with optional chomping indicator.
		if isBlockIndicator(rest) {
			val, next := parseBlockScalar(rest, lines, i+1)
			fm[key] = fmValue{value: val, kind: fmBlock}
			i = next - 1
			continue
		}

		// Quoted scalar.
		if v, ok := parseQuotedScalar(rest); ok {
			fm[key] = fmValue{value: v, kind: fmQuoted}
			continue
		}

		// Plain scalar: strip trailing comment, fold continuation lines.
		val := stripPlainComment(rest)
		j := i + 1
		for ; j < len(lines); j++ {
			cont := lines[j]
			if cont == "" || (cont[0] != ' ' && cont[0] != '\t') {
				break
			}
			contTrimmed := strings.Trim(cont, yamlSpace)
			if contTrimmed == "" || strings.HasPrefix(contTrimmed, "#") {
				break
			}
			if val != "" {
				val += " "
			}
			val += stripPlainComment(contTrimmed)
		}
		i = j - 1
		fm[key] = fmValue{value: val, kind: fmPlain}
	}
	return fm, body
}

// yamlSpace is YAML's white space (s-white), what the yaml parser pi uses
// strips around keys and plain scalars. Any other padding — U+00A0, U+0085,
// U+FEFF — is scalar content to it. The body after the closing fence is not
// YAML and is trimmed as JavaScript trims.
const yamlSpace = " \t"

// isBlockIndicator reports whether a value is a YAML block scalar header:
// | or > optionally followed by a chomping indicator (- or +).
func isBlockIndicator(s string) bool {
	if s == "" || (s[0] != '|' && s[0] != '>') {
		return false
	}
	rest := s[1:]
	return rest == "" || rest == "-" || rest == "+"
}

// parseBlockScalar consumes the indented block following a | / > header,
// returning the scalar value and the index of the first unconsumed line.
func parseBlockScalar(header string, lines []string, start int) (string, int) {
	folded := header[0] == '>'
	chomp := byte(0) // 0 = clip (single trailing \n), '-' = strip, '+' = keep
	if len(header) > 1 {
		chomp = header[1]
	}

	// Collect the block: lines more indented than the key (or blank).
	var block []string
	indent := -1
	i := start
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.Trim(line, yamlSpace) == "" {
			block = append(block, "")
			continue
		}
		lineIndent := len(line) - len(strings.TrimLeft(line, " "))
		if lineIndent == 0 {
			break
		}
		if indent == -1 {
			indent = lineIndent
		}
		if lineIndent < indent {
			break
		}
		block = append(block, line[indent:])
	}
	// Drop trailing blank lines from the block (they belong to chomping).
	trailingBlanks := 0
	for len(block) > 0 && block[len(block)-1] == "" {
		block = block[:len(block)-1]
		trailingBlanks++
	}

	var val string
	if folded {
		// Fold: newlines between lines become spaces; blank lines become \n.
		var b strings.Builder
		prevBlank := true // suppress leading separator
		for _, l := range block {
			if l == "" {
				b.WriteString("\n")
				prevBlank = true
				continue
			}
			if !prevBlank {
				b.WriteString(" ")
			}
			b.WriteString(l)
			prevBlank = false
		}
		val = b.String()
	} else {
		val = strings.Join(block, "\n")
	}

	switch chomp {
	case '-':
		// strip: no trailing newline
	case '+':
		val += strings.Repeat("\n", trailingBlanks+1)
	default:
		if len(block) > 0 {
			val += "\n"
		}
	}
	return val, i
}

// parseQuotedScalar parses a fully single- or double-quoted scalar.
func parseQuotedScalar(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	q := s[0]
	if (q != '"' && q != '\'') || s[len(s)-1] != q {
		return "", false
	}
	inner := s[1 : len(s)-1]
	if q == '\'' {
		return strings.ReplaceAll(inner, "''", "'"), true
	}
	// Double quotes: minimal escape handling.
	r := strings.NewReplacer(`\\`, "\\", `\"`, `"`, `\n`, "\n", `\t`, "\t")
	return r.Replace(inner), true
}

// stripPlainComment removes a trailing ` #comment` from a plain scalar (YAML
// treats space-then-# as a comment in plain context).
func stripPlainComment(s string) string {
	if idx := strings.Index(s, " #"); idx >= 0 {
		return strings.TrimRight(s[:idx], " \t")
	}
	return s
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
	pattern  string // prefixed, slashes normalized, leading and trailing "/" stripped
	negated  bool
	dirOnly  bool
	anchored bool // matches from the root rather than at any depth
	matchAll bool // the body came out empty, which matches every path
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
			pattern, ok := prefixIgnorePattern(line, prefix)
			if !ok {
				continue
			}
			if rule, ok := newSkillIgnoreRule(pattern); ok {
				ig.rules = append(ig.rules, rule)
			}
		}
	}
}

// prefixIgnorePattern ports skills.ts prefixIgnorePattern: the pattern pi hands
// the ignore matcher for one ignore-file line, or false for a line that is
// blank or a comment as JavaScript trims it. The pattern itself is the line
// untrimmed, less a leading "!" (restored in front of the prefix) or the
// backslash of a leading "\!", less one leading "/", after the prefix. Like
// pi, that makes "\!foo" at the root the negated rule "!foo".
func prefixIgnorePattern(line, prefix string) (string, bool) {
	trimmed := jstext.Trim(line)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "\\#") {
		return "", false
	}

	pattern := line
	negated := false
	if strings.HasPrefix(pattern, "!") {
		negated = true
		pattern = pattern[1:]
	} else if strings.HasPrefix(pattern, "\\!") {
		pattern = pattern[1:]
	}
	pattern = prefix + strings.TrimPrefix(pattern, "/")
	if negated {
		return "!" + pattern, true
	}
	// pi filters the empty pattern out before the matcher sees it.
	return pattern, pattern != ""
}

// newSkillIgnoreRule compiles one pattern the way the ignore package pi's lock
// pins (7.0.8) does, as far as this matcher goes: checkPattern's rejections,
// createRule's negation, and the whitespace steps of its regex compiler (see
// ignoreRuleBody). createRule's unescaping of a leading "\!" or "\#" needs no
// step: the pattern matcher reads the backslash as an escape already. Whether the
// rule is anchored is decided on the body before the whitespace steps, so
// "a/ " is anchored though what remains of it, "a/", would not be.
func newSkillIgnoreRule(pattern string) (skillIgnoreRule, bool) {
	// checkPattern: a blank line is spaces only, a lone trailing backslash is
	// invalid, and "#" opens a comment.
	if strings.Trim(pattern, " ") == "" || hasLoneTrailingBackslash(pattern) || strings.HasPrefix(pattern, "#") {
		return skillIgnoreRule{}, false
	}
	var rule skillIgnoreRule
	body := pattern
	if strings.HasPrefix(body, "!") {
		rule.negated = true
		body = body[1:]
	}
	// A separator anywhere but at the very end anchors the pattern.
	rule.anchored = body != "" && strings.Contains(body[:len(body)-1], "/")

	body = ignoreRuleBody(body)
	if body == "" {
		rule.matchAll = true
		return rule, true
	}
	if strings.HasPrefix(body, "/") {
		rule.anchored = true
		body = body[1:]
	}
	rule.dirOnly = strings.HasSuffix(body, "/")
	rule.pattern = strings.TrimSuffix(body, "/")
	if rule.pattern == "" {
		// "/" alone (or "//") compiles to a pattern no relative path matches.
		return skillIgnoreRule{}, false
	}
	return rule, true
}

// hasLoneTrailingBackslash reports whether s ends in a backslash that is not
// preceded by another (checkPattern's /(?:[^\\]|^)\\$/).
func hasLoneTrailingBackslash(s string) bool {
	return strings.HasSuffix(s, "\\") && !strings.HasSuffix(s, "\\\\")
}

// ignoreRuleBody applies the whitespace steps of the ignore matcher's rule
// compiler, in its order: a leading U+FEFF is dropped; a trailing run of CR/LF
// is dropped, unless a backslash escapes its first character, which then stays;
// and a trailing run of spaces (spaces only: a tab, U+00A0 or U+3000 is pattern
// text) is dropped, unless a backslash escapes it, which leaves one literal
// space (/((?:\\\\)*?)(\\? +)$/).
func ignoreRuleBody(body string) string {
	body = strings.TrimPrefix(body, "\ufeff")

	if kept := strings.TrimRight(body, "\r\n"); kept != body {
		if endsInEscape(kept) {
			return body[:len(kept)+1]
		}
		body = kept
	}

	if kept := strings.TrimRight(body, " "); kept != body {
		if endsInEscape(kept) {
			return kept[:len(kept)-1] + " "
		}
		body = kept
	}
	return body
}

// endsInEscape reports whether s ends in an odd run of backslashes, whose last
// one escapes whatever follows.
func endsInEscape(s string) bool {
	return (len(s)-len(strings.TrimRight(s, "\\")))%2 == 1
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
		if r.matchAll || gitignoreMatchPath(r.pattern, r.anchored, relPosix) {
			ignored = !r.negated
		}
	}
	return ignored
}

// gitignoreMatchPath reports whether path (root-relative posix) matches a
// gitignore pattern. An unanchored pattern matches the basename of any path
// segment; an anchored one matches from the root. A directory pattern also
// matches descendants.
func gitignoreMatchPath(pattern string, anchored bool, path string) bool {
	if !anchored {
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
	return strings.HasPrefix(path, pattern+"/")
}

// SkillFileReadTool names the tool the skills block tells the model to use to
// load a skill file (pi skills.ts `fileReadTool: "read" | "bash" |
// "indirect"`). The zero value reads as SkillFileReadToolRead, pi's default
// argument.
type SkillFileReadTool string

const (
	// SkillFileReadToolRead points the model at the read tool.
	SkillFileReadToolRead SkillFileReadTool = "read"
	// SkillFileReadToolBash points the model at bash, for tool sets that have
	// no read tool (upstream 1d6dbf9e3).
	SkillFileReadToolBash SkillFileReadTool = "bash"
	// SkillFileReadToolIndirect names no tool, for a reader that is reachable
	// only through another tool (upstream c30840c2e).
	SkillFileReadToolIndirect SkillFileReadTool = "indirect"
)

// FormatSkillsForPrompt renders visible skills as the Agent Skills XML block,
// telling the model to load skill files with the read tool. It is
// FormatSkillsForPromptWithTool at pi's default fileReadTool.
func FormatSkillsForPrompt(skills []Skill) string {
	return FormatSkillsForPromptWithTool(skills, SkillFileReadToolRead)
}

// FormatSkillsForPromptWithTool renders visible skills as the Agent Skills XML
// block, naming fileReadTool as the way to load a skill's file. The zero value
// is pi's default argument, "read"; any value other than read and bash takes
// pi's last arm, the indirect hint that names no tool.
func FormatSkillsForPromptWithTool(skills []Skill, fileReadTool SkillFileReadTool) string {
	var visible []Skill
	for _, s := range skills {
		if !s.DisableModelInvocation {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}
	var loadInstruction string
	switch fileReadTool {
	case "", SkillFileReadToolRead:
		loadInstruction = "Use the read tool to load a skill's file when the task matches its description."
	case SkillFileReadToolBash:
		loadInstruction = "Use bash to load a skill's file when the task matches its description."
	default:
		loadInstruction = "Load a skill's file when the task matches its description."
	}
	lines := []string{
		"\n\nThe following skills provide specialized instructions for specific tasks.",
		loadInstruction,
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
