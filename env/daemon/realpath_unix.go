//go:build darwin || linux

package daemon

import (
	"strings"

	"golang.org/x/sys/unix"
)

// eloopThreshold is glibc's __eloop_threshold: SYMLOOP_MAX, at least 40.
const eloopThreshold = 40

// realpathWalk is glibc's realpath(3): symbolic links resolved and "." and
// ".." removed, failing on a missing component, a file used as a directory,
// or too many links. It is Linux's realpath; it builds everywhere so that
// macOS can test it against its own realpath(3).
func realpathWalk(name string) (string, error) {
	if name == "" {
		return "", unix.ENOENT
	}
	dest := "/"
	if name[0] != '/' {
		wd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		dest = wd
	}
	links := 0
	for start := 0; start < len(name); {
		for start < len(name) && name[start] == '/' {
			start++
		}
		end := start
		for end < len(name) && name[end] != '/' {
			end++
		}
		if end == start {
			break
		}
		switch component := name[start:end]; component {
		case ".":
		case "..":
			if dest != "/" {
				dest = dest[:strings.LastIndexByte(dest, '/')]
				if dest == "" {
					dest = "/"
				}
			}
		default:
			next := dest + "/" + component
			if dest == "/" {
				next = "/" + component
			}
			target, err := readlinkAll(next)
			if err == nil {
				links++
				if links > eloopThreshold {
					return "", unix.ELOOP
				}
				name = target + name[end:]
				end = 0
				if strings.HasPrefix(target, "/") {
					dest = "/"
				}
			} else {
				if suffixRequiresDirCheck(name[end:]) {
					if err := unix.Access(next+"/", unix.F_OK); err != nil {
						return "", err
					}
				} else if err != unix.EINVAL {
					return "", err
				}
				dest = next
			}
		}
		start = end
	}
	return dest, nil
}

// suffixRequiresDirCheck is glibc's check of what follows a component: a
// trailing slash, or a later "." or "..", needs the component to be a
// directory.
func suffixRequiresDirCheck(end string) bool {
	for strings.HasPrefix(end, "/") {
		end = strings.TrimLeft(end, "/")
		if end == "" {
			return true
		}
		if end[0] != '.' {
			return false
		}
		end = end[1:]
		if end == "" || (end[0] == '.' && (len(end) == 1 || end[1] == '/')) {
			return true
		}
	}
	return false
}

// readlinkAll reads a link of any length.
func readlinkAll(path string) (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlink(path, buf)
		if err != nil {
			return "", err
		}
		if n < size-1 {
			return string(buf[:n]), nil
		}
	}
}
