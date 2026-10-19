package daemon

import (
	"encoding/binary"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS.

// execvp is macOS's execvP(3) (Libc gen/FreeBSD/exec.c): an empty entry is
// ".", a file that is not an executable runs through /bin/sh, and EACCES
// counts only for a candidate that exists (stat decides).
var execvp = execvpRules{
	defaultPath: "/usr/bin:/bin", // _PATH_DEFPATH
	emptyEntry:  func(name string) string { return "./" + name },
	shell:       "/bin/sh", // _PATH_BSHELL
	step: func(err syscall.Errno, resolved string) searchStep {
		switch err {
		case unix.ELOOP, unix.ENAMETOOLONG, unix.ENOENT, unix.ENOTDIR:
			return nextCandidate
		case unix.ENOEXEC:
			return runThroughShell
		case unix.E2BIG, unix.ENOMEM, unix.ETXTBSY:
			return stopSearch
		}
		var st unix.Stat_t
		switch {
		case unix.Stat(resolved, &st) != nil:
			return nextCandidate
		case err == unix.EACCES:
			return nextDenied
		}
		return stopSearch
	},
	exhausted: func(syscall.Errno) syscall.Errno { return unix.ENOENT },
}

// watchPerDirectory reports whether native watching needs a watch on every
// directory below a recursive target. The Rust daemon watches recursively
// through FSEvents; fsnotify watches through kqueue, which is not recursive.
const watchPerDirectory = true

// startWithEmptySignalMask starts a process whose signal mask is empty, as
// libuv and the Rust daemon set it: the forked child takes the mask of the
// thread that forks it, which __pthread_sigmask(2) clears for the moment (x/sys
// has no pthread_sigmask for macOS).
func startWithEmptySignalMask(program string, argv []string, attr *syscall.ProcAttr) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const sigSetmask = 3 // SIG_SETMASK
	var empty, previous uint32
	if _, _, errno := unix.RawSyscall(unix.SYS___PTHREAD_SIGMASK, sigSetmask, uintptr(unsafe.Pointer(&empty)), uintptr(unsafe.Pointer(&previous))); errno == 0 {
		defer unix.RawSyscall(unix.SYS___PTHREAD_SIGMASK, sigSetmask, uintptr(unsafe.Pointer(&previous)), 0)
	}
	return syscall.ForkExec(program, argv, attr)
}

// knownFormat reports whether the kernel can execute the file: a "#!"
// script or a Mach-O image, thin or fat; anything else fails with ENOEXEC.
// A file that cannot be read is left to the kernel.
func knownFormat(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	var head [4]byte
	if n, _ := io.ReadFull(f, head[:]); n >= 2 && head[0] == '#' && head[1] == '!' {
		return true
	} else if n < 4 {
		return false
	}
	switch binary.BigEndian.Uint32(head[:]) {
	case 0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe, // MH_MAGIC, MH_CIGAM, MH_MAGIC_64, MH_CIGAM_64
		0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca: // FAT_MAGIC, FAT_CIGAM, FAT_MAGIC_64, FAT_CIGAM_64
		return true
	}
	return false
}

// rootDevice is the device of "/", which realpath compares mount points with.
var rootDevice = sync.OnceValues(func() (int32, error) {
	var st unix.Stat_t
	err := unix.Stat("/", &st)
	return st.Dev, err
})

// realpathAttributes is the getattrlist(2) reply realpath(3) asks for:
// ATTR_CMN_NAME, ATTR_CMN_DEVID, ATTR_CMN_OBJTYPE and ATTR_CMN_OBJID, in that
// order, then the name's bytes.
type realpathAttributes struct {
	length     uint32
	nameOffset int32
	nameLength uint32
	dev        int32
	objType    uint32
	objNo      uint32
	objGen     uint32
	buf        [unix.PathMax]byte
}

const vlnk = 5 // vtype VLNK

// getattrlist is getattrlist(path, {NAME, DEVID, OBJTYPE, OBJID}, FSOPT_NOFOLLOW).
func getattrlist(path string) (*realpathAttributes, error) {
	name, err := unix.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	list := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_NAME | unix.ATTR_CMN_DEVID | unix.ATTR_CMN_OBJTYPE | unix.ATTR_CMN_OBJID,
	}
	attrs := new(realpathAttributes)
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Pointer(attrs)), unsafe.Sizeof(*attrs), unix.FSOPT_NOFOLLOW, 0)
	if errno != 0 {
		return nil, errno
	}
	return attrs, nil
}

// name is the object's name as the file system stores it.
func (a *realpathAttributes) name() string {
	start := 4 + int(a.nameOffset) // the offset counts from the attrreference
	raw := unsafe.Slice((*byte)(unsafe.Pointer(a)), unsafe.Sizeof(*a))
	if start < 0 || start >= len(raw) {
		return ""
	}
	return string(trimNul(raw[start:]))
}

// maxSymlinks is MAXSYMLINKS of <sys/param.h>.
const maxSymlinks = 32

// realpath is macOS's realpath(3): symbolic links resolved, "." and ".."
// removed, and each component spelled as the file system stores it (which
// matters on case-insensitive volumes), with mount points spelled as mounted.
func realpath(path string) (string, error) {
	if path == "" {
		return "", unix.ENOENT
	}
	rootDev, err := rootDevice()
	if err != nil {
		return "", err
	}
	var resolved, left string
	if path[0] == '/' {
		if path == "/" {
			return "/", nil
		}
		resolved, left = "/", path[1:]
	} else {
		wd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		resolved, left = wd, path
	}
	if len(left) >= unix.PathMax || len(resolved) >= unix.PathMax {
		return "", unix.ENAMETOOLONG
	}
	lastDev := rootDev
	if len(resolved) > 1 {
		var st unix.Stat_t
		if err := unix.Stat(resolved, &st); err != nil {
			return "", err
		}
		lastDev = st.Dev
	}
	symlinks := 0
	for left != "" {
		token, rest, more := strings.Cut(left, "/")
		left = rest
		if len(token) >= unix.PathMax {
			return "", unix.ENAMETOOLONG
		}
		if !strings.HasSuffix(resolved, "/") {
			if len(resolved)+1 >= unix.PathMax {
				return "", unix.ENAMETOOLONG
			}
			resolved += "/"
		}
		switch token {
		case "", ".":
			continue
		case "..":
			// Strip the last component, except from "/".
			if len(resolved) > 1 {
				resolved = resolved[:strings.LastIndexByte(resolved[:len(resolved)-1], '/')+1]
			}
			continue
		}
		saved := len(resolved)
		resolved += token
		if len(resolved) >= unix.PathMax {
			return "", unix.ENAMETOOLONG
		}
		var (
			useAttrs bool
			isLink   bool
			dev      int32
			inode    uint64
		)
		attrs, err := getattrlist(resolved)
		switch {
		case err == nil:
			useAttrs, isLink, dev, inode = true, attrs.objType == vlnk, attrs.dev, uint64(attrs.objNo)
		case err == unix.ENOTSUP || err == unix.EINVAL:
			var st unix.Stat_t
			if err := unix.Lstat(resolved, &st); err != nil {
				return "", err
			}
			isLink, dev, inode = st.Mode&unix.S_IFMT == unix.S_IFLNK, st.Dev, st.Ino
		default:
			return "", err
		}
		if dev != lastDev {
			// A mount point: its name from getattrlist may not be how it is
			// mounted, so the mount path is copied when it is reachable.
			lastDev = dev
			if mount, ok := mountPoint(resolved, dev, inode); ok {
				resolved = mount
				continue
			}
		}
		if isLink {
			if symlinks > maxSymlinks {
				return "", unix.ELOOP
			}
			symlinks++
			target, err := readlink(resolved)
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(target, "/") {
				resolved = "/"
				lastDev = rootDev
			} else if len(resolved) > 1 {
				resolved = resolved[:strings.LastIndexByte(resolved, '/')+1]
			}
			if more {
				if !strings.HasSuffix(target, "/") {
					target += "/"
				}
				target += left
			}
			left = target
		} else if useAttrs {
			resolved = resolved[:saved] + attrs.name()
		}
	}
	if len(resolved) > 1 && strings.HasSuffix(resolved, "/") {
		resolved = resolved[:len(resolved)-1]
	}
	return resolved, nil
}

// mountPoint is where the file system holding path is mounted, when that
// directory is the object (dev, inode) and every directory above it is a
// directory.
func mountPoint(path string, dev int32, inode uint64) (string, bool) {
	var fs unix.Statfs_t
	if unix.Statfs(path, &fs) != nil {
		return "", false
	}
	mount := string(trimNul(fs.Mntonname[:]))
	var st unix.Stat_t
	if unix.Lstat(mount, &st) != nil || st.Dev != dev || st.Ino != inode {
		return "", false
	}
	for dir := mount; ; {
		slash := strings.LastIndexByte(dir, '/')
		if slash <= 0 {
			if slash < 0 {
				return "", false
			}
			break
		}
		dir = dir[:slash]
		if unix.Lstat(dir, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return "", false
		}
	}
	return mount, true
}

func readlink(path string) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlink(path, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// processName is a process's name as proc_name(3) reports it: the long name
// when the system has one, else the command name.
func processName(pid int) (string, bool) {
	// struct proc_bsdinfo: twelve 32-bit fields, pbi_comm[16], pbi_name[32], ...
	var info [136]byte
	const (
		procInfoCallPidinfo = 2
		procPidTBSDInfo     = 3
	)
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPidinfo, uintptr(pid), procPidTBSDInfo, 0,
		uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if errno != 0 || n == 0 {
		return "", false
	}
	name := trimNul(info[64:96])
	if len(name) == 0 {
		name = trimNul(info[48:64])
	}
	if len(name) == 0 {
		return "", false
	}
	return decodeUTF8(name), true
}

// anyUnreliable is Linux only: macOS watches every file system natively.
func anyUnreliable([]watchTarget) bool { return false }

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
	if err := unix.IoctlSetInt(m, unix.TIOCPTYGRANT, 0); err != nil {
		return fail(err)
	}
	if err := unix.IoctlSetInt(m, unix.TIOCPTYUNLK, 0); err != nil {
		return fail(err)
	}
	var name [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(m), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		return fail(errno)
	}
	s, err := unix.Open(string(trimNul(name[:])), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	if err := unix.IoctlSetWinsize(s, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		unix.Close(s)
		return fail(err)
	}
	return os.NewFile(uintptr(m), "/dev/ptmx"), os.NewFile(uintptr(s), string(trimNul(name[:]))), nil
}

// syncFile is Rust's File::sync_all on Apple systems: fcntl(F_FULLFSYNC),
// which flushes the drive's cache too (without fsync's fallback that Go's
// File.Sync adds).
func syncFile(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var syncErr error
	if err := conn.Control(func(fd uintptr) {
		syncErr = retry(func() error {
			_, err := unix.FcntlInt(fd, unix.F_FULLFSYNC, 0)
			return err
		})
	}); err != nil {
		return err
	}
	return syncErr
}
