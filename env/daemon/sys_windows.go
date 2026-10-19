package daemon

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows, following libuv (src/win/error.c, src/win/process.c,
// src/win/fs.c) and Node's child_process, as the Rust daemon does.

const pathSeparator = `\`

// watchPerDirectory: Windows always polls.
const watchPerDirectory = false

// osErrorName is libuv's uv_translate_sys_error for the errors file and
// process operations produce.
func osErrorName(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "UNKNOWN"
	}
	switch errno {
	case 1: // ERROR_INVALID_FUNCTION
		return "EISDIR"
	case 2, 3, 15, 123, 161, 4392: // FILE_NOT_FOUND, PATH_NOT_FOUND, INVALID_DRIVE, INVALID_NAME, BAD_PATHNAME, INVALID_REPARSE_DATA
		return "ENOENT"
	case 4: // ERROR_TOO_MANY_OPEN_FILES
		return "EMFILE"
	case 5, 1314: // ACCESS_DENIED, PRIVILEGE_NOT_HELD
		return "EPERM"
	case 6: // ERROR_INVALID_HANDLE
		return "EBADF"
	case 8, 14: // NOT_ENOUGH_MEMORY, OUTOFMEMORY
		return "ENOMEM"
	case 17: // ERROR_NOT_SAME_DEVICE
		return "EXDEV"
	case 19: // ERROR_WRITE_PROTECT
		return "EROFS"
	case 32, 33: // SHARING_VIOLATION, LOCK_VIOLATION
		return "EBUSY"
	case 39, 82, 112: // HANDLE_DISK_FULL, CANNOT_MAKE, DISK_FULL
		return "ENOSPC"
	case 50: // ERROR_NOT_SUPPORTED
		return "ENOTSUP"
	case 80, 183: // FILE_EXISTS, ALREADY_EXISTS
		return "EEXIST"
	case 13, 87, 122, 131, 1464: // INVALID_DATA, INVALID_PARAMETER, INSUFFICIENT_BUFFER, NEGATIVE_SEEK, SYMLINK_NOT_SUPPORTED
		return "EINVAL"
	case 126: // ERROR_MOD_NOT_FOUND
		return "ENOENT"
	case 145: // ERROR_DIR_NOT_EMPTY
		return "ENOTEMPTY"
	case 206: // ERROR_FILENAME_EXCED_RANGE
		return "ENAMETOOLONG"
	case 267: // ERROR_DIRECTORY (libuv maps it to ENOENT)
		return "ENOENT"
	case 998: // ERROR_NOACCESS
		return "EFAULT"
	case 740, 1920: // ELEVATION_REQUIRED, CANT_ACCESS_FILE
		return "EACCES"
	case 1921: // ERROR_CANT_RESOLVE_FILENAME
		return "ELOOP"
	}
	return "UNKNOWN"
}

// systemText is the system's description of an error.
func systemText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return strings.TrimRight(errno.Error(), "\r\n.")
	}
	return err.Error()
}

// fileStat is what the daemon reports of a file's metadata.
type fileStat struct {
	attributes uint32
	reparseTag uint32
	size       uint64
	mtimeSec   int64
	mtimeNsec  int64
	// creation is the creation time in 100 ns ticks since 1601, Rust's
	// creation_time().
	creation uint64
	volume   uint32
	index    uint64
	// hasIndex is whether volume and index came from a handle.
	hasIndex bool
}

// isSymlink is Rust's FileType::is_symlink on Windows: a reparse point whose
// tag is a name surrogate (symbolic links and junctions).
func (s fileStat) isSymlink() bool {
	return s.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && s.reparseTag&0x20000000 != 0
}

func (s fileStat) isDir() bool {
	return !s.isSymlink() && s.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}

func (s fileStat) isFile() bool {
	return !s.isSymlink() && s.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0
}

// identity is the dev and ino the daemon reports: file index numbers need a
// handle per file; the creation time tells a replaced file or directory apart
// instead.
func (s fileStat) identity() (uint64, uint64) { return 0, s.creation }

func filetimeTicks(t windows.Filetime) uint64 {
	return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime)
}

// unixTime is a FILETIME as seconds and nanoseconds since 1970; before 1970
// the seconds are negative.
func unixTime(t windows.Filetime) (int64, int64) {
	ticks := int64(filetimeTicks(t)) - 116444736000000000
	sec, rem := ticks/10_000_000, ticks%10_000_000
	if rem < 0 {
		sec--
		rem += 10_000_000
	}
	return sec, rem * 100
}

type fileAttributeTagInfo struct {
	attributes uint32
	reparseTag uint32
}

func statOfHandle(h windows.Handle) (fileStat, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fileStat{}, err
	}
	sec, nsec := unixTime(info.LastWriteTime)
	st := fileStat{
		attributes: info.FileAttributes,
		size:       uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow),
		mtimeSec:   sec,
		mtimeNsec:  nsec,
		creation:   filetimeTicks(info.CreationTime),
		volume:     info.VolumeSerialNumber,
		index:      uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		hasIndex:   true,
	}
	if st.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		var tag fileAttributeTagInfo
		if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err == nil {
			st.reparseTag = tag.reparseTag
		}
	}
	return st, nil
}

func wide(path string) (*uint16, error) {
	if hasNul(path) {
		return nil, nulError
	}
	return windows.UTF16PtrFromString(path)
}

const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// metadata is Rust's std metadata on Windows: the handle's information,
// opened without access (following a final link unless lstat), with a
// FindFirstFileEx fallback for files that cannot be opened.
func metadata(path string, follow bool) (fileStat, error) {
	name, err := wide(path)
	if err != nil {
		return fileStat{}, err
	}
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	if !follow {
		flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	h, err := windows.CreateFile(name, 0, shareAll, nil, windows.OPEN_EXISTING, flags, 0)
	if err == nil {
		defer windows.CloseHandle(h)
		return statOfHandle(h)
	}
	if err == windows.ERROR_SHARING_VIOLATION || err == windows.ERROR_ACCESS_DENIED {
		var data windows.Win32finddata
		if find, findErr := windows.FindFirstFile(name, &data); findErr == nil {
			windows.FindClose(find)
			sec, nsec := unixTime(data.LastWriteTime)
			st := fileStat{
				attributes: data.FileAttributes,
				size:       uint64(data.FileSizeHigh)<<32 | uint64(data.FileSizeLow),
				mtimeSec:   sec,
				mtimeNsec:  nsec,
				creation:   filetimeTicks(data.CreationTime),
			}
			if st.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				st.reparseTag = data.Reserved0
			}
			return st, nil
		}
	}
	return fileStat{}, err
}

func lstatPath(path string) (fileStat, error) { return metadata(path, false) }
func statPath(path string) (fileStat, error)  { return metadata(path, true) }

func fstatFile(f *os.File) (fileStat, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return fileStat{}, err
	}
	var st fileStat
	var statErr error
	if err := conn.Control(func(fd uintptr) { st, statErr = statOfHandle(windows.Handle(fd)) }); err != nil {
		return fileStat{}, err
	}
	return st, statErr
}

// tmpdir is Node's os.tmpdir() on Windows.
func tmpdir() string {
	variable := func(key string) string { return os.Getenv(key) }
	path := variable("TEMP")
	if path == "" {
		path = variable("TMP")
	}
	if path == "" {
		root := variable("SystemRoot")
		if root == "" {
			root = variable("windir")
		}
		path = root + `\temp`
	}
	if len(path) > 1 && strings.HasSuffix(path, `\`) && !strings.HasSuffix(path, `:\`) {
		path = path[:len(path)-1]
	}
	return path
}

// home is libuv's uv_os_homedir: USERPROFILE first.
func home() string { return os.Getenv("USERPROFILE") }

func cwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

const tempCharacters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// mkdtemp is libuv's fs__mkdtemp: prefix plus six characters from
// [a-zA-Z0-9], retried while the name exists.
func mkdtemp(prefix string) (string, error) {
	for range 1000 {
		random := randomBytes()
		name := []byte(prefix)
		for i := 0; i < 6; i++ {
			name = append(name, tempCharacters[int(random[i])%len(tempCharacters)])
		}
		path := string(name)
		err := createDir(path)
		if err == nil {
			return path, nil
		}
		if isExist(err) {
			continue
		}
		return "", err
	}
	return "", windows.ERROR_FILE_EXISTS
}

func createDir(path string) error {
	name, err := wide(path)
	if err != nil {
		return err
	}
	return windows.CreateDirectory(name, nil)
}

// openHandle is CreateFileW with every share mode, wrapped as a file.
func openHandle(path string, access, disposition, flags uint32) (*os.File, error) {
	name, err := wide(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, access, shareAll, nil, disposition, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// openReader: Windows cannot open a directory as a file and has no
// O_NOFOLLOW, so the path is checked first, as NodeExecutionEnv does there
// (not race-free).
func openReader(path string, noFollow bool) (*os.File, *failure) {
	if noFollow {
		if st, err := lstatPath(path); err == nil && st.isSymlink() {
			return nil, newFailure("SYMLINK", "Refusing to follow a symbolic link").withPath(path)
		}
	}
	if st, err := statPath(path); err == nil && st.isDir() {
		return nil, newFailure("EISDIR", "EISDIR: illegal operation on a directory, read").withPath(path)
	}
	f, err := openHandle(path, windows.GENERIC_READ, windows.OPEN_EXISTING, 0)
	if err != nil {
		return nil, ioFailure(err, "open", path)
	}
	return f, nil
}

// openFile is libuv's fs__open for Node's modes: CreateFileW with its access,
// sharing and disposition, always with FILE_FLAG_BACKUP_SEMANTICS, so a
// directory opens and later calls on it fail as in Node; creating a file
// where a directory exists reports EISDIR.
func openFile(path string, mode openMode) (*os.File, error) {
	const (
		genericRead  = windows.STANDARD_RIGHTS_READ | windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA | windows.SYNCHRONIZE
		genericWrite = windows.STANDARD_RIGHTS_WRITE | windows.FILE_WRITE_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.FILE_APPEND_DATA | windows.SYNCHRONIZE
	)
	var access, disposition uint32
	switch mode {
	case modeRead:
		access, disposition = genericRead, windows.OPEN_EXISTING
	case modeWrite:
		access, disposition = genericWrite, windows.CREATE_ALWAYS
	case modeAppend:
		access, disposition = (genericWrite&^windows.FILE_WRITE_DATA)|windows.FILE_APPEND_DATA, windows.OPEN_ALWAYS
	case modeReadWrite:
		access, disposition = genericRead|genericWrite, windows.OPEN_EXISTING
	}
	f, err := openHandle(path, access, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err == windows.ERROR_FILE_EXISTS && (mode == modeWrite || mode == modeAppend) {
		// ERROR_FILE_EXISTS while creating means the path is a directory.
		return nil, synthetic("EISDIR")
	}
	return f, err
}

// realpath is libuv's uv_fs_realpath: the final path without the \\?\ prefix.
func realpath(path string) (string, error) {
	f, err := openHandle(path, 0, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return "", err
	}
	defer f.Close()
	conn, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	var resolved string
	var finalErr error
	if err := conn.Control(func(fd uintptr) {
		buf := make([]uint16, 512)
		for {
			n, err := windows.GetFinalPathNameByHandle(windows.Handle(fd), &buf[0], uint32(len(buf)), 0)
			if err != nil {
				finalErr = err
				return
			}
			if int(n) < len(buf) {
				resolved = string(utf16.Decode(buf[:n]))
				return
			}
			buf = make([]uint16, n+1)
		}
	}); err != nil {
		return "", err
	}
	if finalErr != nil {
		return "", finalErr
	}
	if unc, ok := strings.CutPrefix(resolved, `\\?\UNC\`); ok {
		return `\\` + unc, nil
	}
	return strings.TrimPrefix(resolved, `\\?\`), nil
}

// rename is libuv's fs__rename: MoveFileExW replacing an existing
// destination.
func rename(from, to string) error {
	f, err := wide(from)
	if err != nil {
		return err
	}
	t, err := wide(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING)
}

// removeFile is Node's rm on Windows: a symbolic link or junction to a
// directory is removed as a directory entry (libuv's fs__unlink), and a
// read-only file is retried after making it writable.
func removeFile(path string) error {
	name, err := wide(path)
	if err != nil {
		return err
	}
	if st, err := lstatPath(path); err == nil && st.isSymlink() && st.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return windows.RemoveDirectory(name)
	}
	err = windows.DeleteFile(name)
	if err == windows.ERROR_ACCESS_DENIED {
		attributes, attrErr := windows.GetFileAttributes(name)
		if attrErr != nil {
			return attrErr
		}
		if err := windows.SetFileAttributes(name, attributes&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
			return err
		}
		return windows.DeleteFile(name)
	}
	return err
}

// removeAll removes a directory tree.
func removeAll(path string) error {
	return unwrapPathError(os.RemoveAll(path))
}

func syncFile(f *os.File) error { return f.Sync() }

// openDir opens a directory for reading its entries; libuv's scandir and
// opendir report a file as ENOTDIR.
func openDir(path string) (*os.File, error) {
	st, err := statPath(path)
	if err != nil {
		return nil, err
	}
	if !st.isDir() {
		return nil, synthetic("ENOTDIR")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, unwrapPathError(err)
	}
	return f, nil
}

// rawName: Windows listings keep the file system's order; nothing sorts by
// raw names.
func rawName(string) ([]byte, bool) { return nil, false }

// driveCwds are the per-drive working directories of the =C: variables, which
// Node's path.resolve uses for drive-relative paths.
func driveCwds() object {
	out := object{}
	for _, entry := range os.Environ() {
		if len(entry) < 4 || entry[0] != '=' || entry[2] != ':' || entry[3] != '=' {
			continue
		}
		drive := entry[1]
		if !('a' <= drive && drive <= 'z' || 'A' <= drive && drive <= 'Z') {
			continue
		}
		out[strings.ToUpper(entry[1:3])] = entry[4:]
	}
	return out
}

// pathIdentity is the volume serial number and file index of path, as libuv's
// stat reports dev and ino: the creation time is not enough, because NTFS
// gives a file created under a recently removed name the old one's creation
// time ("tunneling"). It falls back to the metadata when the path cannot be
// opened.
func pathIdentity(path string, st fileStat, follow bool) (uint64, uint64) {
	opened, err := metadata(path, follow)
	if err != nil || !opened.hasIndex {
		return st.identity()
	}
	return uint64(opened.volume), opened.index
}

// isHiddenEntry is whether a listed entry counts as hidden: the hidden
// attribute, or a name starting with a dot.
func isHiddenEntry(name, path string) bool {
	if st, err := lstatPath(path); err == nil && st.attributes&windows.FILE_ATTRIBUTE_HIDDEN != 0 {
		return true
	}
	return strings.HasPrefix(name, ".")
}

// isLegacyWSLBash is Node's isLegacyWslBashPath:
// ^[a-z]:\\windows\\(system32|sysnative)\\bash\.exe$, case-insensitive.
func isLegacyWSLBash(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	if len(normalized) < 2 || normalized[0] < 'a' || normalized[0] > 'z' {
		return false
	}
	rest := normalized[1:]
	return rest == `:\windows\system32\bash.exe` || rest == `:\windows\sysnative\bash.exe`
}

func bashConfig(program string) shellConfig {
	if isLegacyWSLBash(program) {
		return shellConfig{program: program, args: []string{"-s"}, commandOnStdin: true}
	}
	return shellConfig{program: program, args: []string{"-c"}}
}

func whereBash() string {
	command := exec.Command("where", "bash.exe")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := command.Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	first = strings.TrimSpace(first)
	if first == "" || !pathExists(first) {
		return ""
	}
	return first
}

// resolveShell is Node's shell resolution on Windows: a configured shell, Git
// Bash, `where bash.exe`, or shell_unavailable.
func resolveShell(shellPath string, configured bool) (shellConfig, *failure) {
	if configured {
		if pathExists(shellPath) {
			return bashConfig(shellPath), nil
		}
		return shellConfig{}, newFailure("shell_unavailable", "Custom shell path not found: "+shellPath)
	}
	var candidates []string
	for _, variable := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if directory, ok := os.LookupEnv(variable); ok {
			candidates = append(candidates, directory+`\Git\bin\bash.exe`)
		}
	}
	for _, candidate := range candidates {
		if pathExists(candidate) {
			return bashConfig(candidate), nil
		}
	}
	if found := whereBash(); found != "" {
		return bashConfig(found), nil
	}
	var searched strings.Builder
	for _, candidate := range candidates {
		searched.WriteString("  " + candidate + "\n")
	}
	return shellConfig{}, newFailure("shell_unavailable",
		"No bash shell found. Options:\n  1. Install Git for Windows: https://git-scm.com/download/win\n  2. Add your bash to PATH (Cygwin, MSYS2, etc.)\n  3. Configure an explicit shellPath\n\nSearched Git Bash in:\n"+strings.TrimRight(searched.String(), " \t\r\n"))
}

// extension is Rust's Path::extension of a file name.
func hasExtension(path string) bool {
	name, ok := fileName(path)
	if !ok {
		return false
	}
	dot := strings.LastIndexByte(name, '.')
	return dot > 0
}

// splitPaths is Rust's std::env::split_paths on Windows: entries separated by
// ";", double quotes letting an entry contain one.
func splitPaths(list string) []string {
	var out []string
	var current strings.Builder
	quoted := false
	for _, c := range list {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ';' && !quoted:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteRune(c)
		}
	}
	return append(out, current.String())
}

func isFilePath(path string) bool {
	st, err := statPath(path)
	return err == nil && st.isFile()
}

// resolveArgvProgram is libuv's search_path for an argv command: a name with
// a directory is tried as given, otherwise the working directory and then
// each PATH entry; a name without an extension also tries .com and .exe.
// Node refuses batch files without a shell.
func resolveArgvProgram(program, cwd string, env object) (string, *failure) {
	notFound := newFailure("spawn_error", "spawn "+program+" ENOENT")
	withExtension := hasExtension(program)
	candidates := func(base string) []string {
		var names []string
		if withExtension {
			names = append(names, base)
		}
		return append(names, base+".com", base+".exe")
	}
	found := ""
	if strings.ContainsAny(program, `\/:`) {
		for _, candidate := range candidates(joinPath(cwd, program)) {
			if isFilePath(candidate) {
				found = candidate
				break
			}
		}
	} else {
		pathVariable, ok := "", false
		for key, value := range env {
			if strings.EqualFold(key, "PATH") {
				pathVariable, ok = asString(value)
				break
			}
		}
		if !ok {
			pathVariable = os.Getenv("PATH")
		}
	search:
		for _, directory := range append([]string{cwd}, splitPaths(pathVariable)...) {
			for _, candidate := range candidates(joinPath(directory, program)) {
				if isFilePath(candidate) {
					found = candidate
					break search
				}
			}
		}
	}
	if found == "" {
		return "", notFound
	}
	lower := strings.ToLower(found)
	if strings.HasSuffix(lower, ".bat") || strings.HasSuffix(lower, ".cmd") {
		return "", newFailure("spawn_error", "spawn "+program+" EINVAL")
	}
	return found, nil
}

// quoteArgument is libuv's quote_cmd_arg: how one argument is written into
// the command line the child splits again.
func quoteArgument(argument string) string {
	if argument == "" {
		return `""`
	}
	if !strings.ContainsAny(argument, " \t\"") {
		return argument
	}
	if !strings.ContainsAny(argument, `"\`) {
		return `"` + argument + `"`
	}
	// Walk backwards: backslashes before a quote (or the closing quote) are
	// doubled, and quotes are escaped.
	runes := []rune(argument)
	var reversed []rune
	quoteHit := true
	for i := len(runes) - 1; i >= 0; i-- {
		c := runes[i]
		reversed = append(reversed, c)
		switch {
		case quoteHit && c == '\\':
			reversed = append(reversed, '\\')
		case c == '"':
			quoteHit = true
			reversed = append(reversed, '\\')
		default:
			quoteHit = false
		}
	}
	slices.Reverse(reversed)
	return `"` + string(reversed) + `"`
}

// requiredVariables are copied by libuv from the parent into every child
// environment that lacks them.
var requiredVariables = []string{
	"HOMEDRIVE", "HOMEPATH", "LOGONSERVER", "PATH", "SYSTEMDRIVE", "SYSTEMROOT",
	"TEMP", "USERDOMAIN", "USERNAME", "USERPROFILE", "WINDIR",
}

func compareUTF16Units(a, b string) int {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	return slices.Compare(ua, ub)
}

// childEnvironment is the child environment as Node builds it on Windows: the
// variables in object order, sorted by UTF-16 code units, keeping the first
// of names that differ only in case; then libuv adds required variables the
// result lacks. Rust's Command passes the block sorted case-insensitively.
func childEnvironment(env object, inheritEnv bool) []string {
	type variable struct{ key, value string }
	var entries []variable
	set := func(key, value string) {
		for i := range entries {
			if entries[i].key == key {
				entries[i].value = value
				return
			}
		}
		entries = append(entries, variable{key, value})
	}
	if inheritEnv {
		for _, entry := range os.Environ() {
			// A leading "=" belongs to the name (=C:=C:\dir).
			split := strings.IndexByte(entry[min(1, len(entry)):], '=')
			if split < 0 {
				continue
			}
			split += min(1, len(entry))
			set(entry[:split], entry[split+1:])
		}
	}
	for key, value := range env {
		if s, ok := asString(value); ok {
			set(key, s)
		}
	}
	slices.SortStableFunc(entries, func(a, b variable) int { return compareUTF16Units(a.key, b.key) })
	seen := map[string]bool{}
	kept := entries[:0]
	for _, entry := range entries {
		upper := strings.ToUpper(entry.key)
		if !seen[upper] {
			seen[upper] = true
			kept = append(kept, entry)
		}
	}
	entries = kept
	for _, required := range requiredVariables {
		if seen[required] {
			continue
		}
		if value, ok := os.LookupEnv(required); ok {
			entries = append(entries, variable{required, value})
			seen[required] = true
		}
	}
	slices.SortStableFunc(entries, func(a, b variable) int {
		return compareUTF16Units(strings.ToUpper(a.key), strings.ToUpper(b.key))
	})
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.key+"="+entry.value)
	}
	return out
}

// job is the job object libuv puts every child it starts into, which ends its
// processes when the daemon exits.
var job = sync.OnceValue(func() windows.Handle {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	var information windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK |
		windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK |
		windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION |
		windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, _ = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)))
	return h
})

// spawnProcess starts program with args quoted as libuv quotes them, Node's
// environment, and a hidden console, in the daemon's job.
func spawnProcess(program string, args []string, cwd string, env object, inheritEnv bool, files []*os.File) (child, error) {
	commandLine := `"` + program + `"`
	for _, arg := range args {
		if hasNul(arg) {
			return child{}, errNulData
		}
		commandLine += " " + quoteArgument(arg)
	}
	environment := childEnvironment(env, inheritEnv)
	if slices.ContainsFunc(environment, hasNul) {
		return child{}, errNulData
	}
	process, err := os.StartProcess(program, append([]string{program}, args...), &os.ProcAttr{
		Dir:   cwd,
		Env:   environment,
		Files: files,
		Sys:   &syscall.SysProcAttr{CmdLine: commandLine, CreationFlags: windows.CREATE_NO_WINDOW},
	})
	if err != nil {
		return child{}, err
	}
	if j := job(); j != 0 {
		if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid)); err == nil {
			_ = windows.AssignProcessToJobObject(j, h)
			windows.CloseHandle(h)
		}
	}
	return child{pid: process.Pid, process: process}, nil
}

// killTree is Node's killProcessTree on Windows: taskkill /F /T, not waited
// for.
func killTree(pid int) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	command := exec.Command(systemRoot+`\System32\taskkill.exe`, "/F", "/T", "/PID", strconv.Itoa(pid))
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if command.Start() == nil {
		go command.Wait()
	}
}

// child is a started command.
type child struct {
	pid     int
	process *os.Process
}

// wait waits for the command to exit: its exit code, an unsigned 32-bit value
// on Windows (an access violation is 3221225477).
func (c child) wait() (int64, error) {
	state, err := c.process.Wait()
	if err != nil {
		return 0, err
	}
	return int64(uint32(state.ExitCode())), nil
}

var nulError = errors.New("path contains a NUL byte")

func hasNul(s string) bool { return strings.IndexByte(s, 0) >= 0 }

// readAt is one positional read; 0 at the end of the file.
func readAt(f *os.File, b []byte, offset int64) (int, error) {
	n, err := f.ReadAt(b, offset)
	if err == io.EOF {
		err = nil
	}
	return n, unwrapPathError(err)
}

// readOnce is one read at the current position; 0 at the end of the file.
func readOnce(f *os.File, b []byte) (int, error) {
	n, err := f.Read(b)
	if err == io.EOF {
		err = nil
	}
	return n, unwrapPathError(err)
}

// trimNul cuts a C string at its NUL.
func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// processName: terminals are not supported on Windows.
func processName(int) (string, bool) { return "", false }

// anyUnreliable is Linux only; Windows polls anyway.
func anyUnreliable([]watchTarget) bool { return false }

// irregularKind is the kind Rust gives a reparse point Go calls irregular:
// junctions and other name surrogates are symbolic links (not followed), the
// rest are what their attributes say.
func irregularKind(path string) entryKind {
	st, err := lstatPath(path)
	switch {
	case err != nil:
		return kindOther
	case st.isSymlink():
		return kindSymlink
	case st.isDir():
		return kindDirectory
	default:
		return kindFile
	}
}
