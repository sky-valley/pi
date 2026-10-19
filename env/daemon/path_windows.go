package daemon

import (
	"path/filepath"
	"strings"
)

// Rust's Path operations on Windows: "\" and "/" separate components, a
// prefix (C:, \\server\share, \\?\...) and a root separator come first, and
// "." components and repeated or trailing separators are not components.

func isSeparatorByte(c byte) bool { return c == '\\' || c == '/' }

// rootLength is the length of the path's prefix and root separator.
func rootLength(path string) int {
	n := len(filepath.VolumeName(path))
	if n < len(path) && isSeparatorByte(path[n]) {
		n++
	}
	return n
}

// trimEnd removes trailing separators and "." components, keeping the prefix
// and root (Rust's Components::trim_right).
func trimEnd(path string) string {
	root := rootLength(path)
	for len(path) > root {
		last := len(path) - 1
		switch {
		case isSeparatorByte(path[last]):
			path = path[:last]
		case path[last] == '.' && last > root && isSeparatorByte(path[last-1]):
			path = path[:last]
		default:
			return path
		}
	}
	return path
}

func lastComponent(path string) (before, last string, ok bool) {
	path = trimEnd(path)
	root := rootLength(path)
	if len(path) <= root {
		return "", "", false
	}
	i := strings.LastIndexAny(path[root:], `\/`)
	if i < 0 {
		return path[:root], path[root:], true
	}
	return path[:root+i+1], path[root+i+1:], true
}

// fileName is Path::file_name.
func fileName(path string) (string, bool) {
	_, last, ok := lastComponent(path)
	if !ok || last == "." || last == ".." {
		return "", false
	}
	return last, true
}

// parentPath is Path::parent.
func parentPath(path string) (string, bool) {
	before, _, ok := lastComponent(path)
	if !ok {
		return "", false
	}
	return trimEnd(before), true
}

// joinPath is Path::join on Windows: a path with a prefix replaces base, one
// with only a root keeps base's prefix, and "\" goes between them unless base
// ends with a separator or is a bare drive (C:).
func joinPath(base, name string) string {
	if filepath.VolumeName(name) != "" {
		return name
	}
	if name != "" && isSeparatorByte(name[0]) {
		return base[:len(filepath.VolumeName(base))] + name
	}
	if base == "" || isSeparatorByte(base[len(base)-1]) {
		return base + name
	}
	if volume := filepath.VolumeName(base); volume == base && len(volume) == 2 && volume[1] == ':' {
		return base + name
	}
	return base + `\` + name
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

func isAbsolute(path string) bool { return filepath.IsAbs(path) }

// isBareName is the ignore crate's is_file_name off Unix: a path whose parent
// is empty.
func isBareName(path string) bool {
	parent, ok := parentPath(path)
	return ok && parent == ""
}

// stripPathPrefix is Path::strip_prefix: prefix's components start path.
func stripPathPrefix(prefix, path string) (string, bool) {
	prefix = trimEnd(prefix)
	if path == prefix {
		return "", true
	}
	if !strings.HasPrefix(path, prefix) || prefix == "" {
		return "", false
	}
	if !isSeparatorByte(prefix[len(prefix)-1]) && !isSeparatorByte(path[len(prefix)]) {
		return "", false
	}
	return strings.TrimLeft(path[len(prefix):], `\/`), true
}

// normalizeCandidate is globset's normalize_path: "\" becomes "/".
func normalizeCandidate(path string) string { return strings.ReplaceAll(path, `\`, "/") }
