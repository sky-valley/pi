//go:build darwin || linux

package daemon

import "strings"

// Rust's Path operations on Unix, which differ from path/filepath's: a path's
// "." components and repeated or trailing separators are not components, and
// nothing is cleaned lexically (".." stays).

// trimEnd removes trailing separators and "." components, keeping a leading
// "/" or "." (Rust's Components::trim_right).
func trimEnd(path string) string {
	for {
		switch {
		case len(path) > 1 && strings.HasSuffix(path, "/"):
			path = path[:len(path)-1]
		case strings.HasSuffix(path, "/.") && len(path) > 2:
			path = path[:len(path)-2]
		case path == "/.":
			path = "/"
		default:
			return path
		}
	}
}

// lastComponent splits path into what comes before its last component and
// that component, after trimEnd; ok is false for "/", "" and ".".
func lastComponent(path string) (before, last string, ok bool) {
	path = trimEnd(path)
	if path == "/" || path == "" {
		return "", "", false
	}
	slash := strings.LastIndexByte(path, '/')
	if slash < 0 {
		return "", path, true
	}
	return path[:slash+1], path[slash+1:], true
}

// fileName is Path::file_name: the last component when it is a name, not
// "." or "..".
func fileName(path string) (string, bool) {
	_, last, ok := lastComponent(path)
	if !ok || last == "." || last == ".." {
		return "", false
	}
	return last, true
}

// parentPath is Path::parent: the path without its last component; false for
// a root or an empty path.
func parentPath(path string) (string, bool) {
	before, _, ok := lastComponent(path)
	if !ok {
		return "", false
	}
	if before == "/" {
		return "/", true
	}
	return trimEnd(strings.TrimSuffix(before, "/")), true
}

// joinPath is Path::join: name replaces base when absolute, and a separator
// goes between them unless base is empty or already ends with one.
func joinPath(base, name string) string {
	if strings.HasPrefix(name, "/") {
		return name
	}
	if base == "" || strings.HasSuffix(base, "/") {
		return base + name
	}
	return base + "/" + name
}

// ancestors is Path::ancestors without the path itself, nearest first.
func ancestors(path string) []string {
	var out []string
	for {
		parent, ok := parentPath(path)
		if !ok {
			return out
		}
		out = append(out, parent)
		path = parent
	}
}

// isAbsolute is Path::is_absolute.
func isAbsolute(path string) bool { return strings.HasPrefix(path, "/") }

// isBareName is the ignore crate's is_file_name: a path without a separator.
func isBareName(path string) bool { return !strings.Contains(path, "/") }

// stripPathPrefix is the ignore crate's strip_prefix on Unix: a byte prefix.
func stripPathPrefix(prefix, path string) (string, bool) {
	return strings.CutPrefix(path, prefix)
}

// normalizeCandidate is globset's normalize_path: nothing to do where "/" is
// the only separator.
func normalizeCandidate(path string) string { return path }
