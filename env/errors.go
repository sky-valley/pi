package env

import (
	"context"
	"errors"
	"io/fs"

	"github.com/sky-valley/pi/coding"
)

// FileErrorCode is Durable's FileError code.
type FileErrorCode string

// The codes of FileError (Durable's FileErrorCode).
const (
	FileAborted          FileErrorCode = "aborted"
	FileNotFound         FileErrorCode = "not_found"
	FilePermissionDenied FileErrorCode = "permission_denied"
	FileNotDirectory     FileErrorCode = "not_directory"
	FileIsDirectory      FileErrorCode = "is_directory"
	FileInvalid          FileErrorCode = "invalid"
	FileNotSupported     FileErrorCode = "not_supported"
	FileUnknown          FileErrorCode = "unknown"
)

// FileError is a failed file operation of an environment: Durable's
// FileError, which pi-env's environments return. Its message is the one
// NodeExecutionEnv would give, and it reaches tools (and models) as is, so
// Error returns only the message.
//
// errors.Is matches not_found with fs.ErrNotExist and permission_denied with
// fs.ErrPermission, so callers written against the local file system keep
// working.
type FileError struct {
	Code    FileErrorCode
	Message string
	// Path is the path the failure is about, when there is one.
	Path  string
	Cause error
}

func (e *FileError) Error() string { return e.Message }

func (e *FileError) Unwrap() error { return e.Cause }

func (e *FileError) Is(target error) bool {
	switch target {
	case fs.ErrNotExist:
		return e.Code == FileNotFound
	case fs.ErrPermission:
		return e.Code == FilePermissionDenied
	}
	return false
}

// ExecutionErrorCode is Durable's ExecutionError code.
type ExecutionErrorCode string

// The codes of ExecutionError (Durable's ExecutionErrorCode).
const (
	ExecAborted          ExecutionErrorCode = "aborted"
	ExecTimeout          ExecutionErrorCode = "timeout"
	ExecShellUnavailable ExecutionErrorCode = "shell_unavailable"
	ExecSpawnError       ExecutionErrorCode = "spawn_error"
	ExecCallbackError    ExecutionErrorCode = "callback_error"
	ExecUnknown          ExecutionErrorCode = "unknown"
)

// ExecutionError is a command that could not run or was cut short: Durable's
// ExecutionError.
//
// For the coding tools' shell seam, an aborted command matches
// coding.ErrShellAborted (errors.Is) and a timed-out one converts to a
// *coding.ShellTimeoutError (errors.As), the two failures that seam
// classifies.
type ExecutionError struct {
	Code    ExecutionErrorCode
	Message string
	// SpillPath is the spill file of a command that timed out or was aborted
	// after its output crossed the spill thresholds.
	SpillPath string
	Cause     error
	// timeout is the command's timeout in seconds, for ShellTimeoutError.
	timeout float64
}

func (e *ExecutionError) Error() string { return e.Message }

func (e *ExecutionError) Unwrap() error { return e.Cause }

func (e *ExecutionError) Is(target error) bool {
	return target == coding.ErrShellAborted && e.Code == ExecAborted
}

func (e *ExecutionError) As(target any) bool {
	timeout, ok := target.(**coding.ShellTimeoutError)
	if !ok || e.Code != ExecTimeout || e.timeout <= 0 {
		return false
	}
	*timeout = &coding.ShellTimeoutError{Seconds: e.timeout}
	return true
}

// abortResult is the failure of an operation whose context ended.
func abortResult(ctx context.Context, path string) *FileError {
	if ctx.Err() != nil {
		return &FileError{Code: FileAborted, Message: "aborted", Path: path}
	}
	return nil
}

// toFileError is NodeExecutionEnv's mapping of Node error codes to FileError
// codes, for the daemon's errors.
func toFileError(err error, fallbackPath string) *FileError {
	var fileErr *FileError
	if errors.As(err, &fileErr) {
		return fileErr
	}
	var remote *RemoteError
	if !errors.As(err, &remote) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// The context ended while the connection was starting.
			return &FileError{Code: FileAborted, Message: "aborted", Path: fallbackPath, Cause: err}
		}
		return &FileError{Code: FileUnknown, Message: err.Error(), Path: fallbackPath, Cause: err}
	}
	path := remote.Path
	if path == "" {
		path = fallbackPath
	}
	code := FileUnknown
	switch remote.Code {
	case "aborted":
		code = FileAborted
	case "ENOENT":
		code = FileNotFound
	case "EACCES", "EPERM":
		code = FilePermissionDenied
	case "ENOTDIR":
		code = FileNotDirectory
	case "EISDIR":
		code = FileIsDirectory
	case "EINVAL", "SYMLINK", "NOT_REGULAR":
		code = FileInvalid
	}
	return &FileError{Code: code, Message: remote.Message, Path: path, Cause: remote}
}
