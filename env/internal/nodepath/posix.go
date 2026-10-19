package nodepath

import "strings"

// PosixPath is Node's path.posix, reading Host where Node reads the process.
type PosixPath struct{ Host *Host }

func (PosixPath) Separator() string { return "/" }

// cwd is Node's posixCwd: on Windows, process.cwd() with forward slashes and
// without its drive.
func (p PosixPath) cwd() string {
	cwd := p.Host.Cwd()
	if !p.Host.Windows {
		return cwd
	}
	cwd = strings.ReplaceAll(cwd, "\\", "/")
	return jsSlice(cwd, strings.IndexByte(cwd, '/'), len(cwd))
}

// Resolve is path.posix.resolve.
func (p PosixPath) Resolve(paths ...string) string {
	if len(paths) == 0 || (len(paths) == 1 && (paths[0] == "" || paths[0] == ".")) {
		if cwd := p.cwd(); at(cwd, 0) == '/' {
			return cwd
		}
	}
	resolvedPath := ""
	resolvedAbsolute := false
	for i := len(paths) - 1; i >= 0 && !resolvedAbsolute; i-- {
		path := paths[i]
		if path == "" {
			continue
		}
		resolvedPath = path + "/" + resolvedPath
		resolvedAbsolute = path[0] == '/'
	}
	if !resolvedAbsolute {
		cwd := p.cwd()
		resolvedPath = cwd + "/" + resolvedPath
		resolvedAbsolute = at(cwd, 0) == '/'
	}
	resolvedPath = normalizeString(resolvedPath, !resolvedAbsolute, '/', isPosixPathSeparator)
	if resolvedAbsolute {
		return "/" + resolvedPath
	}
	if resolvedPath != "" {
		return resolvedPath
	}
	return "."
}

// Normalize is path.posix.normalize.
func (PosixPath) Normalize(path string) string {
	if path == "" {
		return "."
	}
	isAbsolute := path[0] == '/'
	trailingSeparator := path[len(path)-1] == '/'
	path = normalizeString(path, !isAbsolute, '/', isPosixPathSeparator)
	if path == "" {
		if isAbsolute {
			return "/"
		}
		if trailingSeparator {
			return "./"
		}
		return "."
	}
	if trailingSeparator {
		path += "/"
	}
	if isAbsolute {
		return "/" + path
	}
	return path
}

// IsAbsolute is path.posix.isAbsolute.
func (PosixPath) IsAbsolute(path string) bool { return path != "" && path[0] == '/' }

// Join is path.posix.join.
func (p PosixPath) Join(paths ...string) string {
	var parts []string
	for _, path := range paths {
		if path != "" {
			parts = append(parts, path)
		}
	}
	if len(parts) == 0 {
		return "."
	}
	return p.Normalize(strings.Join(parts, "/"))
}

// Basename is path.posix.basename without a suffix.
func (PosixPath) Basename(path string) string {
	start, end := 0, -1
	matchedSlash := true
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if !matchedSlash {
				start = i + 1
				break
			}
		} else if end == -1 {
			matchedSlash = false
			end = i + 1
		}
	}
	if end == -1 {
		return ""
	}
	return path[start:end]
}
