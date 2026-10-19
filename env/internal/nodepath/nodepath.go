// Package nodepath is Node's path module for both of its flavours,
// path.posix and path.win32, and url.fileURLToPath, independent of the
// system this program runs on: the pi-env client resolves paths by the rules
// of the remote system, as upstream's RemoteExecutionEnv does with Node's
// `posix` and `win32` objects.
//
// The functions follow lib/path.js and lib/internal/url.js of Node v26.4.0
// line by line; testdata/node.json holds Node's own answers for a corpus of
// inputs (nodepath_test.go). Strings are walked byte by byte: every character
// the algorithms look at is ASCII, and they only cut at those characters, so
// UTF-8 input comes out as Node's UTF-16 would.
package nodepath

import (
	"os"
	"runtime"
	"strings"
)

// Host is what Node's path functions read from the process they run in:
// process.cwd(), process.env, and whether that process runs on Windows. Only
// the corner cases reach it, such as resolving a relative path against a
// relative base, as they would reach the local Node process upstream.
type Host struct {
	Cwd     func() string
	Getenv  func(string) string
	Windows bool
}

// Local is this process.
var Local = Host{
	Cwd: func() string {
		dir, err := os.Getwd()
		if err != nil {
			return ""
		}
		return dir
	},
	Getenv:  os.Getenv,
	Windows: runtime.GOOS == "windows",
}

// Flavor is one of Node's path objects.
type Flavor interface {
	Resolve(paths ...string) string
	Join(paths ...string) string
	Normalize(path string) string
	IsAbsolute(path string) bool
	Basename(path string) string
	// Separator is `sep`.
	Separator() string
}

// Posix is path.posix; Win32 is path.win32. Both read the local process.
var (
	Posix Flavor = PosixPath{Host: &Local}
	Win32 Flavor = Win32Path{Host: &Local}
)

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

// at is charCodeAt for an index that may be out of range (NaN there, which
// matches no character).
func at(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
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
