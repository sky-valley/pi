package daemon

import (
	"errors"
	"io"
	"os"
)

// File operations with Node's semantics where the system calls differ
// (docs/semantics.md).

// info is `{ name, kind, size, mtimeSec, mtimeNsec, dev, ino }` of lstat or
// fstat metadata.
func info(path string, st fileStat) object {
	kind := "other"
	switch {
	case st.isFile():
		kind = "file"
	case st.isDir():
		kind = "directory"
	case st.isSymlink():
		kind = "symlink"
	}
	name, _ := fileName(path)
	dev, ino := st.identity()
	return object{
		"name":      decodeUTF8([]byte(name)),
		"kind":      kind,
		"size":      st.size,
		"mtimeSec":  st.mtimeSec,
		"mtimeNsec": st.mtimeNsec,
		"dev":       dev,
		"ino":       ino,
	}
}

func lstatOp(path string) (object, *failure) {
	if hasNul(path) {
		return nil, ioFailure(nulError, "lstat", path)
	}
	st, err := lstatPath(path)
	if err != nil {
		return nil, ioFailure(err, "lstat", path)
	}
	return info(path, st), nil
}

func realpathOp(path string) (object, *failure) {
	if hasNul(path) {
		return nil, ioFailure(nulError, "realpath", path)
	}
	resolved, err := realpath(path)
	if err != nil {
		return nil, ioFailure(err, "realpath", path)
	}
	return object{"path": decodeUTF8([]byte(resolved))}, nil
}

// isNotExist and isExist read the error kind as Rust's io::ErrorKind does.
func isNotExist(err error) bool { return errorName(err) == "ENOENT" }
func isExist(err error) bool    { return errorName(err) == "EEXIST" }

func isDirectory(path string) bool {
	st, err := statPath(path)
	return err == nil && st.isDir()
}

// mkdirRecursive is Node's recursive mkdir: an existing directory is fine, an
// existing file is EEXIST, a file in the way of a parent is ENOTDIR.
func mkdirRecursive(path string) error {
	err := createDir(path)
	switch {
	case err == nil:
		return nil
	case isExist(err):
		if isDirectory(path) {
			return nil
		}
		return err
	case isNotExist(err):
		parent, ok := parentPath(path)
		if !ok || parent == "" {
			return err
		}
		if err := mkdirRecursive(parent); err != nil {
			if isExist(err) {
				return synthetic("ENOTDIR")
			}
			return err
		}
		if err := createDir(path); err != nil && !(isExist(err) && isDirectory(path)) {
			return err
		}
		return nil
	default:
		return err
	}
}

func mkdirOp(path string, recursive bool) (object, *failure) {
	var err error
	if hasNul(path) {
		err = nulError
	} else if recursive {
		err = mkdirRecursive(path)
	} else {
		err = createDir(path)
	}
	if err != nil {
		return nil, ioFailure(err, "mkdir", path)
	}
	return object{}, nil
}

// writeOp is writeFile/appendFile: create missing parents like Node's
// recursive mkdir, then write. It returns the open file for further chunks.
func writeOp(path string, appendMode, parents bool, content []byte) (*os.File, *failure) {
	if hasNul(path) {
		return nil, ioFailure(nulError, "open", path)
	}
	if parent, ok := parentPath(path); ok && parents && parent != "" {
		if err := mkdirRecursive(parent); err != nil {
			return nil, ioFailure(err, "mkdir", decodeUTF8([]byte(parent)))
		}
	}
	mode := modeWrite
	if appendMode {
		mode = modeAppend
	}
	f, err := openFile(path, mode)
	if err != nil {
		return nil, ioFailure(err, "open", path)
	}
	if err := writeAll(f, content); err != nil {
		f.Close()
		return nil, ioFailure(err, "write", path)
	}
	return f, nil
}

// writeAll writes all of b, as Rust's write_all.
func writeAll(f *os.File, b []byte) error {
	for len(b) > 0 {
		n, err := f.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func truncateOp(path string, size uint64) (object, *failure) {
	if hasNul(path) {
		return nil, ioFailure(nulError, "open", path)
	}
	f, err := openFile(path, modeReadWrite)
	if err != nil {
		return nil, ioFailure(err, "open", path)
	}
	defer f.Close()
	if size > 1<<63-1 {
		// Rust's set_len fails converting the size, before any system call.
		return nil, ioFailure(errors.New("out of range integral type conversion attempted"), "ftruncate", path)
	}
	if err := f.Truncate(int64(size)); err != nil {
		return nil, ioFailure(err, "ftruncate", path)
	}
	return object{}, nil
}

func fsyncOp(path string) (object, *failure) {
	if hasNul(path) {
		return nil, ioFailure(nulError, "open", path)
	}
	// POSIX refuses to open a directory for writing; check first so Windows
	// reports the same.
	if isDirectory(path) {
		return nil, newFailure("EISDIR", "EISDIR: illegal operation on a directory, open '"+path+"'").withPath(path)
	}
	f, err := openFile(path, modeReadWrite)
	if err != nil {
		return nil, ioFailure(err, "open", path)
	}
	defer f.Close()
	if err := syncFile(f); err != nil {
		return nil, ioFailure(err, "fsync", path)
	}
	return object{}, nil
}

func renameOp(from, to string) (object, *failure) {
	var err error
	if hasNul(from) || hasNul(to) {
		err = nulError
	} else {
		err = rename(from, to)
	}
	if err != nil {
		return nil, ioFailureBetween(err, "rename", from, to)
	}
	return object{}, nil
}

// rmOp is Node's fs.rm: missing paths fail unless force; a directory without
// recursive fails with ERR_FS_EISDIR.
func rmOp(path string, recursive, force bool) (object, *failure) {
	if hasNul(path) {
		return nil, ioFailure(nulError, "lstat", path)
	}
	st, err := lstatPath(path)
	if err != nil {
		if force && isNotExist(err) {
			return object{}, nil
		}
		return nil, ioFailure(err, "lstat", path)
	}
	if st.isDir() {
		if !recursive {
			return nil, newFailure("ERR_FS_EISDIR", "Path is a directory: rm returned EISDIR (is a directory) "+path).withPath(path)
		}
		err = removeAll(path)
	} else {
		err = removeFile(path)
	}
	if err != nil {
		return nil, ioFailure(err, "rm", path)
	}
	return object{}, nil
}

// mkdtempOp creates a directory named prefix plus six random characters, as
// libuv's mkdtemp creates it.
func mkdtempOp(prefix string) (object, *failure) {
	path, err := mkdtemp(prefix)
	if err != nil {
		return nil, ioFailure(err, "mkdtemp", prefix+"XXXXXX")
	}
	return object{"path": decodeUTF8([]byte(path))}, nil
}

// openReaderOp opens a regular file for positional reads; directories fail
// with EISDIR, other non-regular files with NOT_REGULAR, and with noFollow a
// final symbolic link with SYMLINK.
func openReaderOp(path string, noFollow bool) (*os.File, object, *failure) {
	if hasNul(path) {
		return nil, nil, ioFailure(nulError, "open", path)
	}
	f, fail := openReader(path, noFollow)
	if fail != nil {
		return nil, nil, fail
	}
	st, err := fstatFile(f)
	if err != nil {
		f.Close()
		return nil, nil, ioFailure(err, "fstat", path)
	}
	if st.isDir() {
		f.Close()
		return nil, nil, newFailure("EISDIR", "EISDIR: illegal operation on a directory, read").withPath(path)
	}
	if !st.isFile() {
		f.Close()
		return nil, nil, newFailure("NOT_REGULAR", "Not a regular file").withPath(path)
	}
	return f, info(path, st), nil
}

// pread is up to length bytes at offset; fewer only at the end of the file.
func pread(f *os.File, path string, offset uint64, length int) ([]byte, *failure) {
	buf := make([]byte, length)
	filled := 0
	for filled < length {
		n, err := readAt(f, buf[filled:], int64(offset)+int64(filled))
		if err != nil {
			return nil, ioFailure(err, "read", path)
		}
		if n == 0 {
			break
		}
		filled += n
	}
	return buf[:filled], nil
}

// readCurrent is up to length bytes from the current position: one read call,
// as Node's readFile makes for files of unknown size (FIFOs, devices).
func readCurrent(f *os.File, path string, length int) ([]byte, *failure) {
	buf := make([]byte, length)
	n, err := readOnce(f, buf)
	if err != nil {
		return nil, ioFailure(err, "read", path)
	}
	return buf[:n], nil
}

// dirEntry is one directory entry as Node lists it: the name decoded as UTF-8
// (invalid bytes replaced), and lstat of the directory joined with that
// decoded name, so a name that is not valid UTF-8 reports ENOENT as in Node.
// raw carries the original bytes of such a name, which libuv sorts by.
func dirEntry(dir, rawEntryName string) object {
	name := decodeUTF8([]byte(rawEntryName))
	entryPath := joinPath(dir, name)
	var entry object
	if st, err := lstatPath(entryPath); err != nil {
		entry = object{"name": name, "error": ioFailure(err, "lstat", decodeUTF8([]byte(entryPath))).json()}
	} else {
		entry = object{"name": name, "info": info(entryPath, st)}
	}
	if raw, ok := rawName(rawEntryName); ok && string(raw) != name {
		entry["raw"] = byteArray(raw)
	}
	return entry
}
