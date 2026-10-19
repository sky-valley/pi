package daemon

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux and Android.

// defaultPath is the search path execvp(3) uses when PATH is unset: glibc's
// _CS_PATH, or bionic's _PATH_DEFPATH on Android.
var defaultPath = func() string {
	if onAndroid {
		return "/product/bin:/apex/com.android.runtime/bin:/apex/com.android.art/bin:/system_ext/bin:/system/bin:/system/xbin:/odm/bin:/vendor/bin:/vendor/xbin"
	}
	return "/bin:/usr/bin"
}()

// watchPerDirectory reports whether native watching needs a watch on every
// directory below a recursive target: inotify is not recursive.
const watchPerDirectory = true

// startWithEmptySignalMask starts a process whose signal mask is empty, as
// libuv and the Rust daemon set it: the forked child takes the mask of the
// thread that forks it.
func startWithEmptySignalMask(program string, argv []string, attr *os.ProcAttr) (*os.Process, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var empty, previous unix.Sigset_t
	if unix.PthreadSigmask(unix.SIG_SETMASK, &empty, &previous) == nil {
		defer unix.PthreadSigmask(unix.SIG_SETMASK, &previous, nil)
	}
	return os.StartProcess(program, argv, attr)
}

// eloopThreshold is glibc's __eloop_threshold: SYMLOOP_MAX, at least 40.
const eloopThreshold = 40

// realpath is glibc's realpath(3): symbolic links resolved and "." and ".."
// removed, failing on a missing component, a file used as a directory, or
// too many links.
func realpath(name string) (string, error) {
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
			target, err := readlink(next)
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

func readlink(path string) (string, error) {
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

// processName is a process's name from /proc, if it still runs.
func processName(pid int) (string, bool) {
	name, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return "", false
	}
	text := strings.TrimRight(decodeUTF8(name), " \t\n\r\v\f")
	return text, text != ""
}

// unreliableFileSystems are statfs magic numbers of file systems that accept
// watches but do not report changes made elsewhere.
var unreliableFileSystems = []uint64{
	0x6969,     // NFS
	0x517b,     // SMB
	0xff534d42, // CIFS
	0xfe534d42, // SMB2
	0x65735546, // FUSE (sshfs, Android shared storage)
	0x01021997, // 9P (WSL2 Windows drives)
	0x0bd00bd0, // Lustre
	0x47504653, // GPFS
	0x00c36400, // Ceph
	0x5346414f, // OpenAFS
	0x6b414653, // kAFS
	0x5dca2df5, // sdcardfs
}

// anyUnreliable reports whether any target, or its nearest existing ancestor,
// is on a file system that does not report remote changes.
func anyUnreliable(targets []watchTarget) bool {
	for _, target := range targets {
		for _, candidate := range append([]string{target.path}, ancestors(target.path)...) {
			if strings.IndexByte(candidate, 0) >= 0 {
				break
			}
			var info unix.Statfs_t
			if unix.Statfs(candidate, &info) != nil {
				continue
			}
			kind := uint64(info.Type) & 0xffffffff
			for _, unreliable := range unreliableFileSystems {
				if kind == unreliable {
					return true
				}
			}
			break
		}
	}
	return false
}

// openpty is openpty(3): a new pseudo-terminal of the given size.
func openpty(cols, rows uint16) (master, slave *os.File, err error) {
	m, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*os.File, *os.File, error) {
		unix.Close(m)
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(m, unix.TIOCSPTLCK, 0); err != nil {
		return fail(err)
	}
	n, err := unix.IoctlGetUint32(m, unix.TIOCGPTN)
	if err != nil {
		return fail(err)
	}
	name := "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
	s, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	if err := unix.IoctlSetWinsize(s, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		unix.Close(s)
		return fail(err)
	}
	return os.NewFile(uintptr(m), "/dev/ptmx"), os.NewFile(uintptr(s), name), nil
}

// syncFile is Rust's File::sync_all: fsync(2).
func syncFile(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var syncErr error
	if err := conn.Control(func(fd uintptr) {
		syncErr = retry(func() error { return unix.Fsync(int(fd)) })
	}); err != nil {
		return err
	}
	return syncErr
}
