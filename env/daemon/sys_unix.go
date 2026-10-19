//go:build darwin || linux

package daemon

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
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

// whichBash is the first line `which bash` prints, if that names a file.
func whichBash() string {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return ""
	}
	defer null.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return ""
	}
	defer r.Close()
	process, err := startProcess("which", []string{"which", "bash"}, os.Environ(), "", []*os.File{null, w, null}, false)
	w.Close()
	if err != nil {
		return ""
	}
	out, _ := io.ReadAll(r)
	if code, err := process.wait(); err != nil || code != 0 {
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

// searchStep is what execvp(3) does once a candidate failed to execute.
type searchStep int

const (
	// nextCandidate tries the next PATH entry.
	nextCandidate searchStep = iota
	// nextDenied tries the next, failing with EACCES if none runs.
	nextDenied
	// runThroughShell runs the file with the library's shell.
	runThroughShell
	// stopSearch fails with the candidate's error.
	stopSearch
)

// execvpRules are how a C library's execvp(3) searches PATH; the Rust
// daemon's Command runs programs through it (execvp in sys_*.go).
type execvpRules struct {
	// defaultPath is searched when the environment has no PATH.
	defaultPath string
	// emptyEntry is the candidate an empty PATH entry gives.
	emptyEntry func(name string) string
	// nameMax, if not 0, is the longest name searched (ENAMETOOLONG).
	nameMax int
	// shell runs a file the kernel does not recognize as an executable.
	shell string
	// step is the library's answer to a candidate's failure; resolved is the
	// candidate against the working directory.
	step func(err syscall.Errno, resolved string) searchStep
	// exhausted is the error once no candidate ran and none was denied.
	exhausted func(last syscall.Errno) syscall.Errno
}

// lookupEnv is getenv(3) of an environment: the first entry for key.
func lookupEnv(env []string, key string) (string, bool) {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value, true
		}
	}
	return "", false
}

// startProcess runs name as execvp(3) runs it in the Rust daemon's child: a
// name with a slash as given, otherwise each candidate from the child
// environment's PATH in turn, until one runs or fails in a way that ends the
// search. What execve(2) would say of a candidate is foreseen from its
// metadata (execFailure), and only a candidate expected to run is started:
// on macOS a session leader that dies with a controlling terminal revokes
// it, so a pty's failed attempt would spoil the next. The process gets argv,
// the environment env and the working directory dir, its standard streams
// files, and leads a new session (with ctty, with stdin as its controlling
// terminal).
func startProcess(name string, argv []string, env []string, dir string, files []*os.File, ctty bool) (child, error) {
	if slices.ContainsFunc(argv, hasNul) || slices.ContainsFunc(env, hasNul) {
		return child{}, errNulData
	}
	fds := make([]uintptr, len(files))
	for i, f := range files {
		fds[i] = f.Fd()
	}
	defer runtime.KeepAlive(files)
	attr := &syscall.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: fds,
		Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: ctty, Ctty: 0},
	}
	start := func(program string, argv []string) (child, error) {
		pid, err := startWithEmptySignalMask(program, argv, attr)
		if err != nil {
			return child{}, &os.PathError{Op: "fork/exec", Path: program, Err: err}
		}
		return child{pid: pid}, nil
	}
	fail := func(errno syscall.Errno) (child, error) {
		return child{}, &os.PathError{Op: "fork/exec", Path: name, Err: errno}
	}
	// The child enters its working directory before execvp(3) searches.
	if errno := chdirFailure(dir); errno != 0 {
		return fail(errno)
	}
	var candidates []string
	switch {
	case name == "":
		return fail(unix.ENOENT)
	case strings.Contains(name, "/"):
		candidates = []string{name}
	case execvp.nameMax > 0 && len(name) > execvp.nameMax:
		return fail(unix.ENAMETOOLONG)
	default:
		path, ok := lookupEnv(env, "PATH")
		if !ok {
			path = execvp.defaultPath
		}
		for _, entry := range strings.Split(path, ":") {
			if entry == "" {
				candidates = append(candidates, execvp.emptyEntry(name))
			} else {
				candidates = append(candidates, entry+"/"+name)
			}
		}
	}
	denied, last := false, unix.ENOENT
	for _, candidate := range candidates {
		resolved := candidate
		if dir != "" && !strings.HasPrefix(resolved, "/") {
			resolved = dir + "/" + resolved
		}
		errno := execFailure(resolved)
		if errno == 0 {
			// What fails now failed in the child (or execve found the format
			// unknown); the search ends there, as with a failure before execvp.
			process, err := start(candidate, argv)
			if err == nil || !errors.As(err, &errno) || errno != unix.ENOEXEC {
				return process, err
			}
		}
		switch execvp.step(errno, resolved) {
		case runThroughShell:
			return start(execvp.shell, append([]string{"sh", candidate}, argv[1:]...))
		case stopSearch:
			return fail(errno)
		case nextDenied:
			denied = true
		}
		last = errno
	}
	if denied {
		return fail(unix.EACCES)
	}
	return fail(execvp.exhausted(last))
}

// chdirFailure is the error chdir(2) is expected to give for dir, 0 if none.
func chdirFailure(dir string) syscall.Errno {
	if dir == "" {
		return 0
	}
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		if errno, ok := err.(syscall.Errno); ok {
			return errno
		}
		return unix.ENOENT
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.ENOTDIR
	}
	if unix.Access(dir, unix.X_OK) != nil {
		return unix.EACCES
	}
	return 0
}

// execFailure is the error execve(2) is expected to give for path: the
// error reaching it, EACCES for what is not an executable regular file, and
// ENOEXEC for a format the kernel does not run (where that is known, see
// knownFormat); 0 if it should run.
func execFailure(path string) syscall.Errno {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if errno, ok := err.(syscall.Errno); ok {
			return errno
		}
		return unix.ENOENT
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || unix.Access(path, unix.X_OK) != nil {
		return unix.EACCES
	}
	if !knownFormat(path) {
		return unix.ENOEXEC
	}
	return 0
}

// killTree kills the command's process group, or the process if the group is
// gone.
func killTree(pid int) {
	if unix.Kill(-pid, unix.SIGKILL) != nil {
		// Nothing to report if the process is gone too.
		_ = unix.Kill(pid, unix.SIGKILL)
	}
}

// child is a started command; its pid is also its process group's. Commands
// start through syscall.ForkExec and are reaped with wait4(2), not through
// os.StartProcess and os.Process: on Linux their first use probes pidfd
// support, and Android before 12 kills a process that calls pidfd_open
// (SIGSYS), which Go guards against for GOOS=android only, not in the
// android/amd64 daemon (a linux build with the pienv_android tag).
type child struct{ pid int }

// wait waits for the command to exit: its exit code, or 128 + the signal
// that killed it, as the shell reports it.
func (c child) wait() (int64, error) {
	var status syscall.WaitStatus
	err := retry(func() (err error) {
		_, err = syscall.Wait4(c.pid, &status, 0, nil)
		return err
	})
	if err != nil {
		return 0, os.NewSyscallError("wait", err)
	}
	if status.Signaled() {
		return 128 + int64(status.Signal()), nil
	}
	return int64(status.ExitStatus()), nil
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
func spawnProcess(program string, args []string, cwd string, env object, inheritEnv bool, files []*os.File) (child, error) {
	return startProcess(program, append([]string{program}, args...), childEnvironment(env, inheritEnv), cwd, files, false)
}

// irregularKind: Unix listings report no irregular entries.
func irregularKind(string) entryKind { return kindOther }
