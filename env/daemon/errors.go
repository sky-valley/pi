package daemon

import (
	"errors"
	"strings"
)

// failure is an error reply: a Node-style code such as ENOENT, or one of the
// execution codes (shell_unavailable, spawn_error, timeout, aborted,
// unknown), and a diagnostic message. Only codes are part of the contract.
type failure struct {
	code    string
	message string
	syscall string
	path    string
	hasPath bool
	// extra holds fields for the client, such as spillPath.
	extra object
}

func (f *failure) Error() string { return f.message }

func newFailure(code, message string) *failure {
	return &failure{code: code, message: message}
}

// withPath sets the error's path field.
func (f *failure) withPath(path string) *failure {
	f.path, f.hasPath = path, true
	return f
}

// withExtra adds fields to the error JSON, such as spillPath.
func (f *failure) withExtra(extra object) *failure {
	f.extra = extra
	return f
}

// ioFailure is an I/O error of syscall on path, coded with libuv's error name.
func ioFailure(err error, syscall, path string) *failure {
	return ioFailureBetween(err, syscall, path, "")
}

// ioFailureBetween is Node's message for an error of a two-path call:
// `CODE: description, syscall 'from' -> 'to'`.
func ioFailureBetween(err error, syscall, path, destination string) *failure {
	code := errorName(err)
	description, known := uvDescriptions[code]
	if !known {
		// Unknown to libuv's table: the system's own text.
		description = strings.ToLower(systemText(err))
	}
	target := "'" + path + "'"
	if destination != "" {
		target += " -> '" + destination + "'"
	}
	f := newFailure(code, code+": "+description+", "+syscall+" "+target)
	f.syscall = syscall
	return f.withPath(path)
}

func (f *failure) json() object {
	o := object{"code": f.code, "message": f.message}
	if f.syscall != "" {
		o["syscall"] = f.syscall
	}
	if f.hasPath {
		o["path"] = f.path
	}
	for key, value := range f.extra {
		o[key] = value
	}
	return o
}

// synthetic is an error code libuv reports where the system call itself
// reported another one, such as ENOTDIR from a recursive mkdir through a file,
// or EISDIR when Windows creates a file where a directory exists.
type synthetic string

func (s synthetic) Error() string { return string(s) }

// errorName is the libuv name of an error: a synthetic code, or the system's
// code translated per platform; UNKNOWN for errors without one.
func errorName(err error) string {
	var code synthetic
	if errors.As(err, &code) {
		return string(code)
	}
	return osErrorName(err)
}

// uvDescriptions are libuv's uv_strerror texts, which Node uses on every
// system.
var uvDescriptions = map[string]string{
	"EACCES":       "permission denied",
	"EAGAIN":       "resource temporarily unavailable",
	"EBADF":        "bad file descriptor",
	"EBUSY":        "resource busy or locked",
	"EEXIST":       "file already exists",
	"EFBIG":        "file too large",
	"EINVAL":       "invalid argument",
	"EIO":          "i/o error",
	"EISDIR":       "illegal operation on a directory",
	"ELOOP":        "too many symbolic links encountered",
	"EMFILE":       "too many open files",
	"EMLINK":       "too many links",
	"ENAMETOOLONG": "name too long",
	"ENFILE":       "file table overflow",
	"ENOENT":       "no such file or directory",
	"ENOMEM":       "not enough memory",
	"ENOSPC":       "no space left on device",
	"ENOSYS":       "function not implemented",
	"ENOTDIR":      "not a directory",
	"ENOTEMPTY":    "directory not empty",
	"ENOTSUP":      "operation not supported on socket",
	"EPERM":        "operation not permitted",
	"EROFS":        "read-only file system",
	"ETXTBSY":      "text file is busy",
	"EXDEV":        "cross-device link not permitted",
}
