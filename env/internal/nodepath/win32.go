package nodepath

import (
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// Win32Path is Node's path.win32, reading Host where Node reads the process.
type Win32Path struct{ Host *Host }

func (Win32Path) Separator() string { return "\\" }

// Resolve is path.win32.resolve.
func (p Win32Path) Resolve(paths ...string) string {
	resolvedDevice := ""
	resolvedTail := ""
	resolvedAbsolute := false
	for i := len(paths) - 1; i >= -1; i-- {
		var path string
		if i >= 0 {
			path = paths[i]
			if path == "" {
				continue
			}
		} else if resolvedDevice == "" {
			path = p.Host.Cwd()
			// Fast path for current directory.
			if len(paths) == 0 || (len(paths) == 1 && (paths[0] == "" || paths[0] == ".") && isPathSeparator(at(path, 0))) {
				if !p.Host.Windows {
					path = strings.ReplaceAll(path, "/", "\\")
				}
				return path
			}
		} else {
			// Windows' drive-specific working directories: the drive's, or the
			// process's when it is on that drive, else the drive's root.
			path = p.Host.Getenv("=" + resolvedDevice)
			if path == "" {
				path = p.Host.Cwd()
			}
			if jstext.ToLower(jsSlice(path, 0, 2)) != jstext.ToLower(resolvedDevice) && at(path, 2) == '\\' {
				path = resolvedDevice + "\\"
			}
		}

		rootEnd, device, isAbsolute := win32Root(path)
		if device != "" {
			if resolvedDevice != "" {
				if jstext.ToLower(device) != jstext.ToLower(resolvedDevice) {
					// This path points to another device, so it is not applicable.
					continue
				}
			} else {
				resolvedDevice = device
			}
		}
		if resolvedAbsolute {
			if resolvedDevice != "" {
				break
			}
		} else {
			resolvedTail = path[rootEnd:] + "\\" + resolvedTail
			resolvedAbsolute = isAbsolute
			if isAbsolute && resolvedDevice != "" {
				break
			}
		}
	}
	resolvedTail = normalizeString(resolvedTail, !resolvedAbsolute, '\\', isPathSeparator)
	if resolvedAbsolute {
		return resolvedDevice + "\\" + resolvedTail
	}
	if s := resolvedDevice + resolvedTail; s != "" {
		return s
	}
	return "."
}

// win32Root matches the root of path as resolve does: where it ends, its
// device (a drive, a UNC share, or \\. and \\?), and whether the path is
// absolute.
func win32Root(path string) (rootEnd int, device string, isAbsolute bool) {
	n := len(path)
	if n == 0 {
		return 0, "", false
	}
	code := path[0]
	if n == 1 {
		if isPathSeparator(code) {
			return 1, "", true
		}
		return 0, "", false
	}
	if isPathSeparator(code) {
		// Possible UNC root; a leading separator makes the path absolute.
		isAbsolute = true
		if !isPathSeparator(path[1]) {
			return 1, "", true
		}
		j := 2
		last := j
		for j < n && !isPathSeparator(path[j]) {
			j++
		}
		if j < n && j != last {
			firstPart := path[last:j]
			last = j
			for j < n && isPathSeparator(path[j]) {
				j++
			}
			if j < n && j != last {
				last = j
				for j < n && !isPathSeparator(path[j]) {
					j++
				}
				if j == n || j != last {
					if firstPart == "." || firstPart == "?" {
						// A device root, such as \\.\PHYSICALDRIVE0.
						return 4, "\\\\" + firstPart, true
					}
					return j, "\\\\" + firstPart + "\\" + path[last:j], true
				}
			}
		}
		return 0, "", true
	}
	if isWindowsDeviceRoot(code) && path[1] == ':' {
		if n > 2 && isPathSeparator(path[2]) {
			return 3, path[:2], true
		}
		return 2, path[:2], false
	}
	return 0, "", false
}

// Normalize is path.win32.normalize.
func (Win32Path) Normalize(path string) string {
	n := len(path)
	if n == 0 {
		return "."
	}
	rootEnd := 0
	device, hasDevice := "", false
	isAbsolute := false
	code := path[0]
	if n == 1 {
		if isPosixPathSeparator(code) {
			return "\\"
		}
		return path
	}
	if isPathSeparator(code) {
		isAbsolute = true
		if isPathSeparator(path[1]) {
			j := 2
			last := j
			for j < n && !isPathSeparator(path[j]) {
				j++
			}
			if j < n && j != last {
				firstPart := path[last:j]
				last = j
				for j < n && isPathSeparator(path[j]) {
					j++
				}
				if j < n && j != last {
					last = j
					for j < n && !isPathSeparator(path[j]) {
						j++
					}
					if j == n || j != last {
						if firstPart == "." || firstPart == "?" {
							device, hasDevice = "\\\\"+firstPart, true
							rootEnd = 4
							colonIndex := strings.IndexByte(path, ':')
							possibleDevice := jsSlice(path, 4, colonIndex+1)
							if isWindowsReservedName(possibleDevice, len(possibleDevice)-1) {
								device = "\\\\?\\" + possibleDevice
								rootEnd = 4 + len(possibleDevice)
							}
						} else if j == n {
							// Only a UNC root: nothing else to normalize.
							return "\\\\" + firstPart + "\\" + path[last:] + "\\"
						} else {
							device, hasDevice = "\\\\"+firstPart+"\\"+path[last:j], true
							rootEnd = j
						}
					}
				}
			}
		} else {
			rootEnd = 1
		}
	} else {
		colonIndex := strings.IndexByte(path, ':')
		if colonIndex > 0 {
			if isWindowsDeviceRoot(code) && colonIndex == 1 {
				device, hasDevice = path[:2], true
				rootEnd = 2
				if n > 2 && isPathSeparator(path[2]) {
					isAbsolute = true
					rootEnd = 3
				}
			} else if isWindowsReservedName(path, colonIndex) {
				device, hasDevice = path[:colonIndex+1], true
				rootEnd = colonIndex + 1
			}
		}
	}

	tail := ""
	if rootEnd < n {
		tail = normalizeString(path[rootEnd:], !isAbsolute, '\\', isPathSeparator)
	}
	if tail == "" && !isAbsolute {
		tail = "."
	}
	if tail != "" && isPathSeparator(path[n-1]) {
		tail += "\\"
	}
	if !isAbsolute && !hasDevice && strings.Contains(path, ":") {
		// A relative tail must not become something Windows reads as an
		// absolute path (CVE-2024-36139).
		if len(tail) >= 2 && isWindowsDeviceRoot(tail[0]) && tail[1] == ':' {
			return ".\\" + tail
		}
		for index := strings.IndexByte(path, ':'); index != -1; {
			if index == n-1 || isPathSeparator(path[index+1]) {
				return ".\\" + tail
			}
			next := strings.IndexByte(path[index+1:], ':')
			if next == -1 {
				break
			}
			index += 1 + next
		}
	}
	if isWindowsReservedName(path, strings.IndexByte(path, ':')) {
		return ".\\" + device + tail
	}
	if !hasDevice {
		if isAbsolute {
			return "\\" + tail
		}
		return tail
	}
	if isAbsolute {
		return device + "\\" + tail
	}
	return device + tail
}

// IsAbsolute is path.win32.isAbsolute.
func (Win32Path) IsAbsolute(path string) bool {
	if path == "" {
		return false
	}
	return isPathSeparator(path[0]) ||
		(len(path) > 2 && isWindowsDeviceRoot(path[0]) && path[1] == ':' && isPathSeparator(path[2]))
}

// Join is path.win32.join.
func (p Win32Path) Join(paths ...string) string {
	var parts []string
	for _, path := range paths {
		if path != "" {
			parts = append(parts, path)
		}
	}
	if len(parts) == 0 {
		return "."
	}
	firstPart := parts[0]
	joined := strings.Join(parts, "\\")

	// The joined path must not start with two slashes, which normalize would
	// take for a UNC path, unless the first part plainly is one: exactly two
	// slashes and more.
	needsReplace := true
	slashCount := 0
	if isPathSeparator(firstPart[0]) {
		slashCount++
		if len(firstPart) > 1 && isPathSeparator(firstPart[1]) {
			slashCount++
			if len(firstPart) > 2 {
				if isPathSeparator(firstPart[2]) {
					slashCount++
				} else {
					needsReplace = false
				}
			}
		}
	}
	if needsReplace {
		for slashCount < len(joined) && isPathSeparator(joined[slashCount]) {
			slashCount++
		}
		if slashCount >= 2 {
			joined = "\\" + joined[slashCount:]
		}
	}

	// Normalization is skipped when a part is a reserved device name.
	for _, part := range strings.FieldsFunc(joined, func(r rune) bool { return r == '\\' }) {
		if colonIndex := strings.IndexByte(part, ':'); colonIndex != -1 && isWindowsReservedName(part, colonIndex) {
			return strings.ReplaceAll(joined, "/", "\\")
		}
	}
	return p.Normalize(joined)
}

// Basename is path.win32.basename without a suffix.
func (Win32Path) Basename(path string) string {
	start, end := 0, -1
	matchedSlash := true
	// A drive letter's colon is not a separator to drop at the end.
	if len(path) >= 2 && isWindowsDeviceRoot(path[0]) && path[1] == ':' {
		start = 2
	}
	for i := len(path) - 1; i >= start; i-- {
		if isPathSeparator(path[i]) {
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
