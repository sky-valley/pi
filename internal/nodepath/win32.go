// Package nodepath is the part of Node's path.win32 the root module needs —
// normalize and join — independent of the system this program runs on, so
// Windows path arithmetic is the same (and testable) on every host.
//
// It is a copy of the same functions in the env module's internal/nodepath,
// which follow lib/path.js of Node v26.4.0 line by line; the env module
// builds against a released root module, so it cannot import this package
// until it next requires one that has it. nodepath_test.go runs both against
// that package's corpus of Node's own answers.
package nodepath

import "strings"

func isPathSeparator(c byte) bool      { return c == '/' || c == '\\' }
func isPosixPathSeparator(c byte) bool { return c == '/' }

func isWindowsDeviceRoot(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

var windowsReservedNames = []string{
	"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
	"COM¹", "COM²", "COM³",
	"LPT¹", "LPT²", "LPT³",
}

// isWindowsReservedName is Node's check of path.slice(0, colonIndex); a
// colonIndex of -1 slices to the last character, as JavaScript's slice does.
func isWindowsReservedName(path string, colonIndex int) bool {
	device := strings.ToUpper(jsSlice(path, 0, colonIndex))
	for _, name := range windowsReservedNames {
		if device == name {
			return true
		}
	}
	return false
}

// jsSlice is String.prototype.slice(start, end) for byte offsets: negative
// offsets count from the end.
func jsSlice(s string, start, end int) string {
	if start < 0 {
		start = max(len(s)+start, 0)
	}
	if end < 0 {
		end = max(len(s)+end, 0)
	}
	start, end = min(start, len(s)), min(end, len(s))
	if start >= end {
		return ""
	}
	return s[start:end]
}

// normalizeString resolves . and .. elements in a path with directory names.
func normalizeString(path string, allowAboveRoot bool, separator byte, isSeparator func(byte) bool) string {
	res := ""
	lastSegmentLength := 0
	lastSlash := -1
	dots := 0
	var code byte
	for i := 0; i <= len(path); i++ {
		if i < len(path) {
			code = path[i]
		} else if isSeparator(code) {
			break
		} else {
			code = '/'
		}
		if isSeparator(code) {
			if lastSlash == i-1 || dots == 1 {
				// NOOP
			} else if dots == 2 {
				if len(res) < 2 || lastSegmentLength != 2 || res[len(res)-1] != '.' || res[len(res)-2] != '.' {
					if len(res) > 2 {
						lastSlashIndex := len(res) - lastSegmentLength - 1
						if lastSlashIndex == -1 {
							res = ""
							lastSegmentLength = 0
						} else {
							res = res[:lastSlashIndex]
							lastSegmentLength = len(res) - 1 - strings.LastIndexByte(res, separator)
						}
						lastSlash = i
						dots = 0
						continue
					} else if len(res) != 0 {
						res = ""
						lastSegmentLength = 0
						lastSlash = i
						dots = 0
						continue
					}
				}
				if allowAboveRoot {
					if len(res) > 0 {
						res += string(separator) + ".."
					} else {
						res = ".."
					}
					lastSegmentLength = 2
				}
			} else {
				if len(res) > 0 {
					res += string(separator) + path[lastSlash+1:i]
				} else {
					res = path[lastSlash+1 : i]
				}
				lastSegmentLength = i - lastSlash - 1
			}
			lastSlash = i
			dots = 0
		} else if code == '.' && dots != -1 {
			dots++
		} else {
			dots = -1
		}
	}
	return res
}

// Win32Normalize is path.win32.normalize.
func Win32Normalize(path string) string {
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

// Win32Join is path.win32.join.
func Win32Join(paths ...string) string {
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
	return Win32Normalize(joined)
}
