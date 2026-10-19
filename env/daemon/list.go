package daemon

import (
	"bufio"
	"math"
	"os"
	"os/user"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sky-valley/pi/env/internal/frame"
)

// listFiles: the files and directories below a directory, as `fd --hidden
// --exclude .git` lists them (without following symbolic links), skipping
// what git ignores, as the Rust daemon lists them with the ignore crate.

const (
	defaultMaxEntries = 50_000
	defaultMaxBytes   = 4 * 1024 * 1024
	defaultTimeoutMs  = 10_000
)

type listOptions struct {
	// hidden includes names starting with "." (and, on Windows, hidden files).
	hidden bool
	// gitignore skips what git ignores inside git work trees: .gitignore
	// files, .git/info/exclude, the global excludes file.
	gitignore bool
	// exclude names are skipped with everything below them, as is ".git".
	exclude []string
	// maxEntries and maxBytes bound the listing payload: per entry one kind
	// byte, the path, and a NUL.
	maxEntries uint64
	maxBytes   uint64
	timeout    time.Duration
}

func listOptionsFromJSON(json any) (listOptions, *failure) {
	count := func(key string, fallback uint64) (uint64, *failure) {
		value := get(json, key)
		if value == nil {
			return fallback, nil
		}
		n, ok := asU64(value)
		if !ok {
			return 0, newFailure("EINVAL", "invalid field "+key)
		}
		return n, nil
	}
	o := listOptions{hidden: true, gitignore: true}
	if exclude := get(json, "exclude"); exclude != nil {
		names, ok := asStrings(exclude)
		if !ok {
			return o, newFailure("EINVAL", "invalid field exclude")
		}
		o.exclude = names
	}
	if hidden, ok := asBool(get(json, "hidden")); ok {
		o.hidden = hidden
	}
	if gitignore, ok := asBool(get(json, "gitignore")); ok {
		o.gitignore = gitignore
	}
	var fail *failure
	if o.maxEntries, fail = count("maxEntries", defaultMaxEntries); fail != nil {
		return o, fail
	}
	if o.maxBytes, fail = count("maxBytes", defaultMaxBytes); fail != nil {
		return o, fail
	}
	o.maxBytes = min(o.maxBytes, frame.MaxPayload)
	timeoutMs, fail := count("timeoutMs", defaultTimeoutMs)
	if fail != nil {
		return o, fail
	}
	if timeoutMs > math.MaxInt64/uint64(time.Millisecond) {
		o.timeout = math.MaxInt64
	} else {
		o.timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	return o, nil
}

// ignoreDir is the ignore crate's Ignore: the rules of one directory, linked
// to its parent's. The chain runs from the directory being listed up through
// the listing root, then through the root's parents on the absolute path
// (absParent), to the top, which holds the global excludes.
type ignoreDir struct {
	parent    *ignoreDir
	dir       string
	absParent bool
	hasGit    bool
	gitignore *gitignore
	exclude   *gitignore
}

// ignorer holds what all of a listing's matchers share.
type ignorer struct {
	enabled bool
	global  *gitignore
	// absoluteBase is the canonical listing root, for the parents' rules;
	// empty when it could not be resolved.
	absoluteBase string
}

// child is Ignore::add_child_path: the rules of dir (its .gitignore, and
// .git/info/exclude when dir holds a repository) under parent's.
func (ig *ignorer) child(parent *ignoreDir, dir string) *ignoreDir {
	d := &ignoreDir{parent: parent, dir: dir}
	if !ig.enabled {
		return d
	}
	gitPath := joinPath(dir, ".git")
	gitStat, gitErr := statPath(gitPath)
	d.hasGit = gitErr == nil || pathExists(joinPath(dir, ".jj"))
	d.gitignore = newGitignore(dir, joinPath(dir, ".gitignore"))
	if commonDir, ok := gitCommonDir(dir, gitStat, gitErr == nil); ok {
		d.exclude = newGitignore(dir, joinPath(commonDir, "info/exclude"))
	}
	return d
}

// gitCommonDir is the ignore crate's resolve_git_commondir: where a work
// tree's info/exclude lives. A .git file (a linked work tree or submodule)
// names its git directory, whose commondir file names the common one.
func gitCommonDir(dir string, gitStat fileStat, exists bool) (string, bool) {
	gitDir := joinPath(dir, ".git")
	if !exists || !gitStat.isFile() {
		return gitDir, true
	}
	firstLine := func(path string) (string, bool) {
		f, err := os.Open(path)
		if err != nil {
			return "", false
		}
		defer f.Close()
		line, err := bufio.NewReader(f).ReadString('\n')
		if err != nil && line == "" {
			return "", false
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		return line, utf8.ValidString(line)
	}
	line, ok := firstLine(gitDir)
	if !ok {
		return "", false
	}
	realGitDir, ok := strings.CutPrefix(line, "gitdir: ")
	if !ok {
		return "", false
	}
	commonDir, ok := firstLine(joinPath(realGitDir, "commondir"))
	if !ok {
		return "", false
	}
	if strings.HasPrefix(commonDir, ".") {
		return joinPath(realGitDir, commonDir), true
	}
	return commonDir, true
}

// parents is Ignore::add_parents: the rules of every directory above the
// canonical listing root, which apply to the listing as git applies them.
func (ig *ignorer) parents(top *ignoreDir, root string) *ignoreDir {
	if !ig.enabled {
		return top
	}
	base, err := realpath(root)
	if err != nil {
		return top
	}
	ig.absoluteBase = base
	chain := ancestors(base)
	d := top
	for i := len(chain) - 1; i >= 0; i-- {
		d = ig.child(d, chain[i])
		d.absParent = true
		d.hasGit = pathExists(joinPath(chain[i], ".git")) || pathExists(joinPath(chain[i], ".jj"))
	}
	return d
}

// matched is Ignore::matched_ignore: the nearest .gitignore with a matching
// rule decides, then info/exclude, then the global excludes; rules of
// directories above the first one holding a repository do not apply, and none
// apply outside a repository.
func (ig *ignorer) matched(d *ignoreDir, path string, isDir bool) matchResult {
	if !ig.enabled {
		return matchNone
	}
	anyGit := false
	for p := d; p != nil; p = p.parent {
		if p.hasGit {
			anyGit = true
			break
		}
	}
	var mGitignore, mExclude matchResult
	sawGit := false
	consider := func(p *ignoreDir, path string) {
		if anyGit && !sawGit && mGitignore == matchNone {
			mGitignore = p.gitignore.matched(path, isDir)
		}
		if anyGit && !sawGit && mExclude == matchNone {
			mExclude = p.exclude.matched(path, isDir)
		}
		sawGit = sawGit || p.hasGit
	}
	p := d
	var rootDir *ignoreDir
	for ; p != nil && !p.absParent; p = p.parent {
		consider(p, path)
		rootDir = p
	}
	if ig.absoluteBase != "" {
		relative := path
		if rootDir != nil {
			relative = strings.TrimPrefix(path, strings.TrimPrefix(rootDir.dir, "./"))
			relative = strings.TrimPrefix(relative, "/")
		}
		absolute := joinPath(ig.absoluteBase, relative)
		for ; p != nil; p = p.parent {
			consider(p, absolute)
		}
	}
	var mGlobal matchResult
	if anyGit {
		mGlobal = ig.global.matched(path, isDir)
	}
	for _, m := range []matchResult{mGitignore, mExclude, mGlobal} {
		if m != matchNone {
			return m
		}
	}
	return matchNone
}

// excludesFileRule is the ignore crate's pattern for core.excludesFile.
var excludesFileRule = regexp.MustCompile(`(?im)^[\t\n\x0B\f\r ]*excludesfile[\t\n\x0B\f\r ]*=[\t\n\x0B\f\r ]*"?[\t\n\x0B\f\r ]*([^\t\n\x0B\f\r ]+?)[\t\n\x0B\f\r ]*"?[\t\n\x0B\f\r ]*$`)

// homeDir is Rust's std::env::home_dir: HOME, else the user database.
func homeDir() (string, bool) {
	if home, ok := os.LookupEnv("HOME"); ok {
		return home, true
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir, true
	}
	return "", false
}

// globalExcludesPath is the ignore crate's gitconfig_excludes_path: the
// core.excludesFile of GIT_CONFIG_GLOBAL, ~/.gitconfig, the XDG config or the
// system config, else the default $XDG_CONFIG_HOME/git/ignore.
func globalExcludesPath() (string, bool) {
	home, hasHome := homeDir()
	configHome := func() (string, bool) {
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return xdg, true
		}
		if hasHome {
			return joinPath(home, ".config"), true
		}
		return "", false
	}
	fromConfig := func(path string, ok bool) (string, bool) {
		if !ok {
			return "", false
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		match := excludesFileRule.FindStringSubmatch(latin1(string(data)))
		if match == nil {
			return "", false
		}
		value := fromLatin1(match[1])
		if !utf8.ValidString(value) {
			return "", false
		}
		if hasHome {
			value = strings.ReplaceAll(value, "~", home)
		}
		return value, true
	}
	if global := os.Getenv("GIT_CONFIG_GLOBAL"); global != "" {
		if path, ok := fromConfig(global, true); ok {
			return path, true
		}
	}
	if path, ok := fromConfig(joinPath(home, ".gitconfig"), hasHome); ok {
		return path, true
	}
	if dir, ok := configHome(); ok {
		if path, ok := fromConfig(joinPath(dir, "git/config"), true); ok {
			return path, true
		}
	}
	system := os.Getenv("GIT_CONFIG_SYSTEM")
	if system == "" {
		system = "/etc/gitconfig"
	}
	if path, ok := fromConfig(system, true); ok {
		return path, true
	}
	if dir, ok := configHome(); ok {
		return joinPath(dir, "git/ignore"), true
	}
	return "", false
}

// fromLatin1 undoes latin1.
func fromLatin1(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		out = append(out, byte(r))
	}
	return string(out)
}

type listing struct {
	payload   []byte
	count     uint64
	truncated bool
}

type lister struct {
	options listOptions
	ignorer *ignorer
	aborted func() bool
	started time.Time
	listing listing
}

// listFiles lists root depth first, siblings in byte order of their names.
// It stops with truncated at the first entry beyond a bound and fails with
// aborted once aborted() is true. Entries that cannot be read are skipped.
func listFiles(root string, options listOptions, aborted func() bool) (listing, *failure) {
	if hasNul(root) {
		return listing{}, ioFailure(nulError, "scandir", root)
	}
	st, err := statPath(root)
	if err != nil {
		return listing{}, ioFailure(err, "scandir", root)
	}
	if !st.isDir() {
		return listing{}, newFailure("ENOTDIR", "ENOTDIR: not a directory, scandir '"+root+"'").withPath(root)
	}
	ig := &ignorer{enabled: options.gitignore}
	if options.gitignore {
		// The global excludes file is matched relative to the listed
		// directory, not the daemon's.
		if path, ok := globalExcludesPath(); ok {
			if gst, err := statPath(path); err == nil && gst.isFile() {
				ig.global = newGitignore(root, path)
			}
		}
	}
	top := &ignoreDir{absParent: true}
	l := &lister{options: options, ignorer: ig, aborted: aborted, started: time.Now()}
	rootDir := ig.child(ig.parents(top, root), root)
	if fail := l.walk(root, "", rootDir); fail != nil {
		return listing{}, fail
	}
	return l.listing, nil
}

// walk lists dir's entries and recurses into its directories; it stops
// early, with listing.truncated, at the first entry beyond a bound.
func (l *lister) walk(dir, relativeDir string, ig *ignoreDir) *failure {
	entries, err := readDirSorted(dir)
	if err != nil {
		// walkdir reports the directory's error after the directory itself.
		if l.aborted() {
			return newFailure("aborted", "aborted")
		}
		return nil
	}
	for _, entry := range entries {
		path := joinPath(dir, entry.name)
		isDir := entry.kind == kindDirectory
		if l.skip(ig, path, entry, isDir) {
			continue
		}
		if l.aborted() {
			return newFailure("aborted", "aborted")
		}
		var child *ignoreDir
		if isDir {
			child = l.ignorer.child(ig, path)
		}
		var kind byte
		switch entry.kind {
		case kindSymlink:
			kind = 'l'
		case kindDirectory:
			kind = 'd'
		case kindFile:
			kind = 'f'
		default:
			continue
		}
		relative := decodeUTF8([]byte(entry.name))
		if relativeDir != "" {
			relative = relativeDir + "/" + relative
		}
		size := uint64(len(relative)) + 2
		if l.listing.count >= l.options.maxEntries ||
			uint64(len(l.listing.payload))+size > l.options.maxBytes ||
			time.Since(l.started) > l.options.timeout {
			l.listing.truncated = true
			return nil
		}
		l.listing.payload = append(l.listing.payload, kind)
		l.listing.payload = append(l.listing.payload, relative...)
		l.listing.payload = append(l.listing.payload, 0)
		l.listing.count++
		if isDir {
			if fail := l.walk(path, relative, child); fail != nil || l.listing.truncated {
				return fail
			}
		}
	}
	return nil
}

// skip is the walker's skip_entry: what the ignore rules ignore, hidden
// entries unless a rule whitelists them, ".git", and the excluded names.
func (l *lister) skip(ig *ignoreDir, path string, entry dirEntryInfo, isDir bool) bool {
	m := l.ignorer.matched(ig, path, isDir)
	if m == matchIgnore {
		return true
	}
	if m == matchNone && !l.options.hidden && isHiddenEntry(entry.name, path) {
		return true
	}
	return entry.name == ".git" || slices.Contains(l.options.exclude, entry.name)
}

type entryKind int

const (
	kindOther entryKind = iota // FIFOs, sockets, devices: not listed
	kindFile
	kindDirectory
	kindSymlink
)

// entryKindOf is the kind of a listed entry without following links, from
// the directory listing's type when it is plain.
func entryKindOf(dir string, entry os.DirEntry) entryKind {
	t := entry.Type()
	switch {
	case t&os.ModeSymlink != 0:
		return kindSymlink
	case t&os.ModeIrregular != 0:
		return irregularKind(joinPath(dir, entry.Name()))
	case t.IsDir():
		return kindDirectory
	case t.IsRegular():
		return kindFile
	}
	return kindOther
}

type dirEntryInfo struct {
	name string
	kind entryKind
}

// readDirSorted lists a directory's entries with their types (not following
// links), sorted by name bytes, as walkdir does with sort_by_file_name.
// Entries whose type cannot be read are left out, as walkdir reports them as
// errors.
func readDirSorted(dir string) ([]dirEntryInfo, error) {
	f, err := openDir(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	out := make([]dirEntryInfo, 0, len(entries))
	for _, entry := range entries {
		out = append(out, dirEntryInfo{name: entry.Name(), kind: entryKindOf(dir, entry)})
	}
	slices.SortFunc(out, func(a, b dirEntryInfo) int { return strings.Compare(a.name, b.name) })
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return out, nil
}

func listFilesOp(json any, aborted func() bool) (object, []byte, *failure) {
	root, ok := asString(get(json, "path"))
	if !ok {
		return nil, nil, newFailure("EINVAL", "missing field path")
	}
	options, fail := listOptionsFromJSON(json)
	if fail != nil {
		return nil, nil, fail
	}
	result, fail := listFiles(root, options, aborted)
	if fail != nil {
		return nil, nil, fail
	}
	return object{"count": result.count, "truncated": result.truncated}, result.payload, nil
}
