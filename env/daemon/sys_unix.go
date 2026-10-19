//go:build darwin || linux

package daemon

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Linux, macOS and Android.

const pathSeparator = "/"

// osErrorName is libuv's name for an OS error.
func osErrorName(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "UNKNOWN"
	}
	switch errno {
	case unix.EPERM:
		return "EPERM"
	case unix.ENOENT:
		return "ENOENT"
	case unix.EIO:
		return "EIO"
	case unix.EBADF:
		return "EBADF"
	case unix.EAGAIN:
		return "EAGAIN"
	case unix.ENOMEM:
		return "ENOMEM"
	case unix.EACCES:
		return "EACCES"
	case unix.EBUSY:
		return "EBUSY"
	case unix.EEXIST:
		return "EEXIST"
	case unix.EXDEV:
		return "EXDEV"
	case unix.ENOTDIR:
		return "ENOTDIR"
	case unix.EISDIR:
		return "EISDIR"
	case unix.EINVAL:
		return "EINVAL"
	case unix.ENFILE:
		return "ENFILE"
	case unix.EMFILE:
		return "EMFILE"
	case unix.ETXTBSY:
		return "ETXTBSY"
	case unix.EFBIG:
		return "EFBIG"
	case unix.ENOSPC:
		return "ENOSPC"
	case unix.EROFS:
		return "EROFS"
	case unix.EMLINK:
		return "EMLINK"
	case unix.ENAMETOOLONG:
		return "ENAMETOOLONG"
	case unix.ENOTEMPTY:
		return "ENOTEMPTY"
	case unix.ELOOP:
		return "ELOOP"
	case unix.ENOSYS:
		return "ENOSYS"
	case unix.ENOTSUP:
		return "ENOTSUP"
	}
	return "UNKNOWN"
}

// systemText is the system's description of an error.
func systemText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}

// fileStat is what the daemon reports of a file's metadata.
type fileStat struct {
	mode      uint32 // S_IFMT bits
	size      uint64
	mtimeSec  int64
	mtimeNsec int64
	dev       uint64
	ino       uint64
}

func statOf(st *unix.Stat_t) fileStat {
	return fileStat{
		mode:      uint32(st.Mode) & unix.S_IFMT,
		size:      uint64(st.Size),
		mtimeSec:  int64(st.Mtim.Sec),
		mtimeNsec: int64(st.Mtim.Nsec),
		// st_dev is a signed int on macOS; Rust's dev() widens it the same way.
		dev: uint64(st.Dev),
		ino: uint64(st.Ino),
	}
}

func (s fileStat) isFile() bool { return s.mode == unix.S_IFREG }

// identity is the device and inode, which identify a file across renames.
func (s fileStat) identity() (uint64, uint64) { return s.dev, s.ino }
func (s fileStat) isDir() bool                { return s.mode == unix.S_IFDIR }
func (s fileStat) isSymlink() bool            { return s.mode == unix.S_IFLNK }

// retry repeats a system call interrupted by a signal, as Rust's cvt_r does.
func retry(call func() error) error {
	for {
		err := call()
		if err != unix.EINTR {
			return err
		}
	}
}

func pathError(op, path string, err error) error {
	if err == nil {
		return nil
	}
	return &os.PathError{Op: op, Path: path, Err: err}
}

func lstatPath(path string) (fileStat, error) {
	var st unix.Stat_t
	err := retry(func() error { return unix.Lstat(path, &st) })
	return statOf(&st), pathError("lstat", path, err)
}

func statPath(path string) (fileStat, error) {
	var st unix.Stat_t
	err := retry(func() error { return unix.Stat(path, &st) })
	return statOf(&st), pathError("stat", path, err)
}

func fstatFile(f *os.File) (fileStat, error) {
	var st unix.Stat_t
	conn, err := f.SyscallConn()
	if err != nil {
		return fileStat{}, err
	}
	var statErr error
	if err := conn.Control(func(fd uintptr) {
		statErr = retry(func() error { return unix.Fstat(int(fd), &st) })
	}); err != nil {
		return fileStat{}, err
	}
	return statOf(&st), pathError("fstat", f.Name(), statErr)
}

// envVar is Rust's env::var: the value of a variable that is set and UTF-8.
func envVar(key string) (string, bool) {
	value, ok := os.LookupEnv(key)
	if !ok || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
}

// tmpdir is Node's os.tmpdir(); Termux's Node falls back to $PREFIX/tmp.
func tmpdir() string {
	path := ""
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if value, _ := envVar(key); value != "" {
			path = value
			break
		}
	}
	if path == "" {
		if onAndroid {
			prefix, ok := envVar("PREFIX")
			if !ok {
				prefix = "/data/data/com.termux/files/usr"
			}
			path = prefix + "/tmp"
		} else {
			path = "/tmp"
		}
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}
	return path
}

func home() string {
	home, _ := envVar("HOME")
	return home
}

// cwd is the daemon's working directory, the process.cwd() Node's
// path.resolve falls back to: getcwd(3), never $PWD.
func cwd() string {
	dir, err := unix.Getwd()
	if err != nil {
		return ""
	}
	return decodeUTF8([]byte(dir))
}

// tempCharacters are the characters mkdtemp(3) fills its template with.
const tempCharacters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// mkdtemp is mkdtemp(3) of prefix as libuv calls it: prefix plus six random
// characters from [A-Za-z0-9], a new directory with mode 0700.
func mkdtemp(prefix string) (string, error) {
	for attempt := 0; attempt < 238328; attempt++ {
		random := randomBytes()
		name := []byte(prefix)
		for i := 0; i < 6; i++ {
			name = append(name, tempCharacters[int(random[i])%len(tempCharacters)])
		}
		path := string(name)
		err := retry(func() error { return unix.Mkdir(path, 0o700) })
		if err == unix.EEXIST {
			continue
		}
		if err != nil {
			return "", pathError("mkdtemp", prefix+"XXXXXX", err)
		}
		return path, nil
	}
	return "", pathError("mkdtemp", prefix+"XXXXXX", unix.EEXIST)
}

// openFD opens path with flags and wraps the descriptor without registering
// it with Go's poller, so reads and writes are plain blocking system calls
// as in the Rust daemon.
func openFD(path string, flags int, perm uint32) (*os.File, error) {
	var fd int
	err := retry(func() (err error) {
		fd, err = unix.Open(path, flags|unix.O_CLOEXEC, perm)
		return err
	})
	if err != nil {
		return nil, pathError("open", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openReader opens a regular file for positional reads, without blocking on
// FIFOs and, with noFollow, without following a final symbolic link.
func openReader(path string, noFollow bool) (*os.File, *failure) {
	flags := unix.O_RDONLY | unix.O_NONBLOCK
	if noFollow {
		flags |= unix.O_NOFOLLOW
	}
	f, err := openFD(path, flags, 0)
	if err != nil {
		var errno syscall.Errno
		if noFollow && errors.As(err, &errno) && (errno == unix.ELOOP || errno == unix.EMLINK) {
			return nil, newFailure("SYMLINK", "Refusing to follow a symbolic link").withPath(path)
		}
		return nil, ioFailure(err, "open", path)
	}
	return f, nil
}

// openFile opens a file in one of Node's modes; mode 0666 as Rust creates
// files.
func openFile(path string, mode openMode) (*os.File, error) {
	var flags int
	switch mode {
	case modeRead:
		flags = unix.O_RDONLY
	case modeWrite:
		flags = unix.O_WRONLY | unix.O_CREAT | unix.O_TRUNC
	case modeAppend:
		flags = unix.O_WRONLY | unix.O_APPEND | unix.O_CREAT
	case modeReadWrite:
		flags = unix.O_RDWR
	}
	return openFD(path, flags, 0o666)
}

func rename(from, to string) error {
	return retry(func() error { return unix.Rename(from, to) })
}

func removeFile(path string) error {
	return retry(func() error { return unix.Unlink(path) })
}

// openDir opens a directory for reading its entries, failing at once on
// anything else, as opendir(3) does.
func openDir(path string) (*os.File, error) {
	return openFD(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
}

// rawName is the bytes of a file name, which libuv sorts directory listings by.
func rawName(name string) ([]byte, bool) { return []byte(name), true }

// driveCwds is Windows only: the per-drive working directories.
func driveCwds() object { return object{} }

// pathIdentity is the device and inode of path; its metadata already has them.
func pathIdentity(_ string, st fileStat, _ bool) (uint64, uint64) {
	return st.identity()
}

// isHiddenEntry is whether a listed entry counts as hidden: its name starts
// with a dot.
func isHiddenEntry(name, _ string) bool { return strings.HasPrefix(name, ".") }

func whichBash() string {
	command := exec.Command("which", "bash")
	command.Stdin = nil
	command.Stderr = nil
	out, err := command.Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if first == "" || !pathExists(first) {
		return ""
	}
	return first
}

// resolveShell is Node's shell resolution: a configured shell must exist;
// otherwise /bin/bash, `which bash`, then sh.
func resolveShell(shellPath string, configured bool) (shellConfig, *failure) {
	shell := func(program string) shellConfig {
		return shellConfig{program: program, args: []string{"-c"}}
	}
	if configured {
		if pathExists(shellPath) {
			return shell(shellPath), nil
		}
		return shellConfig{}, newFailure("shell_unavailable", "Custom shell path not found: "+shellPath)
	}
	if pathExists("/bin/bash") {
		return shell("/bin/bash"), nil
	}
	if found := whichBash(); found != "" {
		return shell(found), nil
	}
	return shell("sh"), nil
}

// resolveArgvProgram is the program an argv command runs; the search happens
// at spawn, as execvp does it.
func resolveArgvProgram(program, _ string, _ object) (string, *failure) {
	return program, nil
}

// childEnvironment is the environment Rust's Command gives a child: the
// daemon's, unless cleared, with the request's string values set over it.
// Once anything changed (a value set, or the environment cleared), Rust
// passes the environment it captured, sorted by name; otherwise the daemon's
// own, in its order.
func childEnvironment(env object, inheritEnv bool) []string {
	set := map[string]string{}
	for key, value := range env {
		if s, ok := asString(value); ok {
			set[key] = s
		}
	}
	if inheritEnv && len(set) == 0 {
		return os.Environ()
	}
	values := map[string]string{}
	if inheritEnv {
		for _, entry := range os.Environ() {
			if key, value, ok := strings.Cut(entry, "="); ok {
				values[key] = value
			}
		}
	}
	for key, value := range set {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

// lookPath finds program as execvp(3) does: a name with a slash as given,
// otherwise in each directory of the child environment's PATH (the system's
// default path when it has none, an empty entry meaning the working
// directory). EACCES from a candidate is remembered while the search goes on.
func lookPath(program string, env []string, dir string) (string, error) {
	if strings.Contains(program, "/") {
		return program, nil
	}
	if program == "" {
		return "", unix.ENOENT
	}
	path, found := "", false
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path, found = value, true
		}
	}
	if !found {
		path = defaultPath
	}
	denied := false
	for _, entry := range strings.Split(path, ":") {
		candidate := program
		if entry != "" {
			candidate = entry + "/" + program
		}
		check := candidate
		if !strings.HasPrefix(check, "/") {
			check = dir + "/" + check
		}
		var st unix.Stat_t
		if unix.Stat(check, &st) != nil || st.Mode&unix.S_IFMT == unix.S_IFDIR {
			continue
		}
		if unix.Access(check, unix.X_OK) != nil {
			denied = true
			continue
		}
		return candidate, nil
	}
	if denied {
		return "", unix.EACCES
	}
	return "", unix.ENOENT
}

// startProcess starts argv[0] (searched as execvp does) with argv, the
// environment env and the working directory dir, its standard streams
// files, as the leader of a new session (and with ctty, with stdin as its
// controlling terminal). A file the kernel does not recognize as an
// executable runs through /bin/sh, as execvp runs it.
func startProcess(path string, argv []string, env []string, dir string, files []*os.File, ctty bool) (*os.Process, error) {
	if slices.ContainsFunc(argv, hasNul) || slices.ContainsFunc(env, hasNul) {
		return nil, errNulData
	}
	program, err := lookPath(path, env, dir)
	if err != nil {
		return nil, &os.PathError{Op: "fork/exec", Path: path, Err: err}
	}
	attr := &os.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: files,
		Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: ctty, Ctty: 0},
	}
	process, err := startWithEmptySignalMask(program, argv, attr)
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == unix.ENOEXEC {
		shellArgv := append([]string{"sh", program}, argv[1:]...)
		return startWithEmptySignalMask("/bin/sh", shellArgv, attr)
	}
	return process, err
}

// killTree kills the command's process group, or the process if the group is
// gone.
func killTree(pid int) {
	if unix.Kill(-pid, unix.SIGKILL) != nil {
		// Nothing to report if the process is gone too.
		_ = unix.Kill(pid, unix.SIGKILL)
	}
}

// exitCode is the command's exit code; a process killed by a signal has none
// and reports 128 + the signal, as the shell does.
func exitCode(state *os.ProcessState) int64 {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return int64(state.ExitCode())
	}
	if status.Signaled() {
		return 128 + int64(status.Signal())
	}
	return int64(status.ExitStatus())
}

// nulError is how Rust refuses a path or argument with a NUL byte before any
// system call: an error without an OS code.
var nulError = errors.New("file name contained an unexpected NUL byte")

func hasNul(s string) bool { return strings.IndexByte(s, 0) >= 0 }

// readAt is one positional read.
func readAt(f *os.File, b []byte, offset int64) (int, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var readErr error
	if err := conn.Read(func(fd uintptr) bool {
		readErr = retry(func() (err error) {
			n, err = unix.Pread(int(fd), b, offset)
			return err
		})
		return true
	}); err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	return n, nil
}

// readOnce is one read at the current position.
func readOnce(f *os.File, b []byte) (int, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var readErr error
	if err := conn.Read(func(fd uintptr) bool {
		readErr = retry(func() (err error) {
			n, err = unix.Read(int(fd), b)
			return err
		})
		return true
	}); err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	return n, nil
}

// trimNul cuts a C string at its NUL.
func trimNul(b []byte) []byte {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i]
	}
	return b
}

// createDir is mkdir(path, 0777), Rust's fs::create_dir.
func createDir(path string) error {
	return pathError("mkdir", path, retry(func() error { return unix.Mkdir(path, 0o777) }))
}

// removeAll is Rust's fs::remove_dir_all: a symbolic link is removed itself;
// a directory has its tree removed depth first through descriptors, without
// following links, stopping at the first error other than an entry that
// vanished meanwhile.
func removeAll(path string) error {
	var st unix.Stat_t
	if err := retry(func() error { return unix.Lstat(path, &st) }); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return removeFile(path)
	}
	return removeTree(unix.AT_FDCWD, path, true)
}

func removeTree(parent int, name string, root bool) error {
	var fd int
	err := retry(func() (err error) {
		fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		return err
	})
	if err == unix.ENOTDIR || err == unix.ELOOP {
		// Not a directory: do not traverse, unlink it (unless it is the root).
		if root {
			return err
		}
		return retry(func() error { return unix.Unlinkat(parent, name, 0) })
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return pathErr.Err
		}
		return err
	}
	for _, entry := range entries {
		var err error
		if entry.IsDir() {
			err = removeTree(fd, entry.Name(), false)
		} else {
			err = retry(func() error { return unix.Unlinkat(fd, entry.Name(), 0) })
		}
		if err != nil && err != unix.ENOENT {
			return err
		}
	}
	at := parent
	if root {
		at = unix.AT_FDCWD
	}
	err = retry(func() error { return unix.Unlinkat(at, name, unix.AT_REMOVEDIR) })
	if err == unix.ENOENT {
		return nil
	}
	return err
}

// spawnProcess starts program with args (argv[0] is program as given), the
// environment Rust's Command builds, its working directory, and files as its
// standard streams.
func spawnProcess(program string, args []string, cwd string, env object, inheritEnv bool, files []*os.File) (*os.Process, error) {
	return startProcess(program, append([]string{program}, args...), childEnvironment(env, inheritEnv), cwd, files, false)
}

// irregularKind: Unix listings report no irregular entries.
func irregularKind(string) entryKind { return kindOther }
