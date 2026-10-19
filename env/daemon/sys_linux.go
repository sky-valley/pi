package daemon

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Linux and Android.

// execvp is the execvp(3) of the C library the Rust daemon links: musl on
// Linux (its builds are static musl), bionic on Android.
var execvp = func() execvpRules {
	if onAndroid {
		return bionicExecvp
	}
	return muslExecvp
}()

// knownFormat: Linux runs formats registered at run time (binfmt_misc), so
// only execve(2) can tell; a file it refuses gives ENOEXEC from there.
func knownFormat(string) bool { return true }

// muslExecvp is musl's __execvpe (src/process/execvp.c): a name longer than
// NAME_MAX fails at once, an empty entry is the name itself, EACCES is
// remembered, ENOENT and ENOTDIR move on, anything else (ENOEXEC included:
// musl runs no shell) fails; with no EACCES, the last candidate's error.
var muslExecvp = execvpRules{
	defaultPath: "/usr/local/bin:/bin:/usr/bin",
	emptyEntry:  func(name string) string { return name },
	nameMax:     255,
	step: func(err syscall.Errno, _ string) searchStep {
		switch err {
		case unix.EACCES:
			return nextDenied
		case unix.ENOENT, unix.ENOTDIR:
			return nextCandidate
		}
		return stopSearch
	},
	exhausted: func(last syscall.Errno) syscall.Errno { return last },
}

// bionicExecvp is bionic's execvpe (libc/bionic/exec.cpp): an empty entry is
// ".", a file that is not an executable runs through /system/bin/sh, EACCES is
// remembered, EISDIR, ELOOP, ENAMETOOLONG, ENOENT and ENOTDIR move on,
// anything else fails; with no EACCES, the last candidate's error.
var bionicExecvp = execvpRules{
	defaultPath: "/product/bin:/apex/com.android.runtime/bin:/apex/com.android.art/bin:/system_ext/bin:/system/bin:/system/xbin:/odm/bin:/vendor/bin:/vendor/xbin",
	emptyEntry:  func(name string) string { return "./" + name },
	shell:       "/system/bin/sh",
	step: func(err syscall.Errno, _ string) searchStep {
		switch err {
		case unix.EACCES:
			return nextDenied
		case unix.EISDIR, unix.ELOOP, unix.ENAMETOOLONG, unix.ENOENT, unix.ENOTDIR:
			return nextCandidate
		case unix.ENOEXEC:
			return runThroughShell
		}
		return stopSearch
	},
	exhausted: func(last syscall.Errno) syscall.Errno { return last },
}

// watchPerDirectory reports whether native watching needs a watch on every
// directory below a recursive target: inotify is not recursive.
const watchPerDirectory = true

// startWithEmptySignalMask starts a process whose signal mask is empty, as
// libuv and the Rust daemon set it: the forked child takes the mask of the
// thread that forks it.
func startWithEmptySignalMask(program string, argv []string, attr *syscall.ProcAttr) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var empty, previous unix.Sigset_t
	if unix.PthreadSigmask(unix.SIG_SETMASK, &empty, &previous) == nil {
		defer unix.PthreadSigmask(unix.SIG_SETMASK, &previous, nil)
	}
	return syscall.ForkExec(program, argv, attr)
}

// realpath is the realpath(3) of the C library the Rust daemon links: musl's
// on Linux; bionic's on Android, taken to agree with glibc's walk (no Android
// oracle has been run).
func realpath(name string) (string, error) {
	if onAndroid {
		return realpathWalk(name)
	}
	return realpathMusl(name)
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
