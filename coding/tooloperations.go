package coding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Pluggable operations for the built-in tools, ported from pi's per-tool
// `*Operations` seams (packages/coding-agent/src/core/tools/*.ts at ccfe79ed2).
// pi's own comment on ReadOperations states the intent: "Override these to
// delegate file reading to remote systems (for example SSH)."
//
// These are STRUCTS OF FUNCTIONS rather than Go interfaces, deliberately.
// pi's seams are object literals whose members are individually overridable and
// in one case optional (`detectImageMimeType?`), and callers override one or
// two members while inheriting the rest. A Go interface would force every
// implementer to supply all members and could not express the optional one; a
// struct of funcs maps pi's shape exactly and lets a nil field mean "use the
// default", which is what pi's `?` and its spread-over-defaults do.
//
// Every tool keeps its existing env-free constructor, which uses the local
// defaults, so none of this changes behavior until a host injects something.
//
// ONE RECORDED DIVERGENCE, in GrepOperations.ReadFile. In pi that member is
// NOT the primary scan: pi matches with ripgrep and calls readFile only to
// fetch CONTEXT LINES around a hit (grep.ts:210). This port has no ripgrep
// dependency — it walks and matches in Go — so the same member IS the primary
// read here, and a custom implementation therefore controls strictly more than
// it would upstream: it decides what grep can see at all, not just how context
// is rendered. That is a consequence of the port's own grep architecture rather
// than a porting choice, it cannot be avoided without taking a ripgrep
// dependency, and it is stated on the member so nobody has to rediscover it.
//
// Note these are the CODING-AGENT seams. pi-durable's separate tools
// (packages/durable/src/tools) take a single broad ExecutionEnv instead (see
// execenv.go); the port carries both, with EnvReadOperations and friends
// bridging one to the other.

// ReadOperations are the file operations the read tool performs.
type ReadOperations struct {
	// ReadFile reads a file's contents. Required.
	ReadFile func(ctx context.Context, absolutePath string) ([]byte, error)
	// Access reports whether the file is readable, returning an error if not.
	// Required.
	Access func(ctx context.Context, absolutePath string) error
	// DetectImageMimeType returns the image MIME type for a path, or "" for a
	// non-image. Optional — nil uses the built-in detector, matching pi's `?`.
	DetectImageMimeType func(ctx context.Context, absolutePath string) string
}

// EditOperations are the file operations the edit tool performs.
type EditOperations struct {
	ReadFile  func(ctx context.Context, absolutePath string) ([]byte, error)
	WriteFile func(ctx context.Context, absolutePath, content string) error
	// Access reports whether the file is readable AND writable (pi passes
	// R_OK|W_OK here, where the read tool passes R_OK alone).
	Access func(ctx context.Context, absolutePath string) error
}

// WriteOperations are the file operations the write tool performs.
type WriteOperations struct {
	WriteFile func(ctx context.Context, absolutePath, content string) error
	// Mkdir creates a directory and its parents.
	Mkdir func(ctx context.Context, dir string) error
}

// ErrShellAborted is what a shell operation returns when the context ended the
// command. It is pi's `throw new Error("aborted")` from ops.exec (bash.ts:461),
// rendered as a sentinel so the tool can classify it with errors.Is.
var ErrShellAborted = errors.New("aborted")

// ShellTimeoutError is what a shell operation returns when the command outran
// its timeout — pi's `throw new Error("timeout:<seconds>")` (bash.ts:464),
// rendered as a typed error so the tool can recover the seconds with errors.As
// instead of parsing a message.
type ShellTimeoutError struct{ Seconds float64 }

func (e *ShellTimeoutError) Error() string {
	return "timeout:" + strconv.FormatFloat(e.Seconds, 'g', -1, 64)
}

// BashExecOptions are the per-command controls pi passes to BashOperations.exec.
type BashExecOptions struct {
	// OnData receives interleaved stdout/stderr as it is produced.
	OnData func(data []byte)
	// TimeoutSeconds bounds the command; zero or less means no timeout.
	TimeoutSeconds float64
	// Env is the child environment, already assembled.
	Env []string
}

// BashOperations is the process execution the shell tools perform. pi shares
// one interface between bash and powershell (`PowerShellOperations =
// BashOperations`), so the port does too.
type BashOperations struct {
	// Exec runs a command, streaming output through OnData, and reports the
	// exit code. A signal termination is reported as 128 + the signal number,
	// pi's shell convention. A NIL exit code with a nil error is pi's
	// `exitCode: null`, which only a custom implementation can produce and which
	// the tool treats as a FAILED command. Abort and timeout come back as
	// ErrShellAborted and *ShellTimeoutError.
	Exec func(ctx context.Context, command, cwd string, options BashExecOptions) (exitCode *int, err error)
}

// LocalBashOptions are pi's createLocalBashOperations options.
type LocalBashOptions struct {
	// ShellPath is an explicit shell (pi's shellPath setting). Empty resolves
	// the platform default; a missing path fails each command.
	ShellPath string
}

// LocalBashOperations runs commands on the local machine through bash,
// porting pi's createLocalBashOperations (bash.ts): the shell is resolved
// per command with GetShellConfig(options.ShellPath).
func LocalBashOperations(options LocalBashOptions) BashOperations {
	return localShellOperations("bash", func() (ShellConfig, error) { return GetShellConfig(options.ShellPath) })
}

// LocalPowerShellOperations runs commands on the local machine through
// PowerShell, porting pi's createLocalPowerShellOperations (powershell.ts):
// GetPowerShellConfig per command, every command prefixed with
// UTF8_OUTPUT_PREFIX.
func LocalPowerShellOperations() PowerShellOperations {
	return withUTF8OutputPrefix(localShellOperations("PowerShell", GetPowerShellConfig))
}

// withUTF8OutputPrefix is createLocalPowerShellOperations' wrapper: the
// prefix belongs to the local operations, so custom PowerShell operations and
// the spawn hook see the command as the tool built it.
func withUTF8OutputPrefix(operations BashOperations) BashOperations {
	return BashOperations{
		Exec: func(ctx context.Context, command, cwd string, options BashExecOptions) (*int, error) {
			return operations.Exec(ctx, utf8OutputPrefix+command, cwd, options)
		},
	}
}

// localShellOperations ports pi's createLocalShellOperations (bash.ts), the
// process execution both built-in shells share; shellName goes into the
// missing-working-directory error.
func localShellOperations(shellName string, resolveShellConfig func() (ShellConfig, error)) BashOperations {
	return BashOperations{
		Exec: func(ctx context.Context, command, cwd string, options BashExecOptions) (*int, error) {
			shellConfig, err := resolveShellConfig()
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(cwd); err != nil {
				return nil, fmt.Errorf("Working directory does not exist: %s\nCannot execute %s commands.", cwd, shellName)
			}
			runCtx := ctx
			if options.TimeoutSeconds > 0 {
				var cancel context.CancelFunc
				runCtx, cancel = context.WithTimeout(ctx, time.Duration(options.TimeoutSeconds*float64(time.Second)))
				defer cancel()
			}
			// Legacy WSL bash takes the command on stdin (`bash -s`); otherwise it
			// is the final argv entry (`bash -c <command>`).
			var cmd *exec.Cmd
			if shellConfig.commandFromStdin() {
				cmd = exec.CommandContext(runCtx, shellConfig.Shell, shellConfig.Args...)
				cmd.Stdin = strings.NewReader(command)
			} else {
				cmd = exec.CommandContext(runCtx, shellConfig.Shell, append(slices.Clone(shellConfig.Args), command)...)
			}
			cmd.Dir = cwd
			cmd.Env = options.Env
			// Own process group, and on cancel/timeout kill the whole tree so
			// backgrounded grandchildren don't survive (port of pi).
			setProcessGroup(cmd)
			cmd.Cancel = func() error { return killProcessTree(cmd) }
			// WaitDelay backstops the manual drain: if killProcessTree fires and a
			// descendant still pins the pipe, os/exec force-closes the inherited
			// fds shortly after Wait returns so we never hang.
			cmd.WaitDelay = time.Second

			runErr := runBashCommand(cmd, onDataWriter(options.OnData))

			return classifyShellRun(ctx.Err(), runCtx.Err(), runErr, options.TimeoutSeconds)
		},
	}
}

// classifyShellRun turns a finished command into pi's `{exitCode}` / thrown
// error pair. It is a pure function of the three signals rather than inline
// code, because its ORDER is the contract and the order is not reachable from a
// live process test: getting a real command to have both aborted AND timed out
// is a race, so a test written that way passes whichever way the branches are
// written. See TestClassifyShellRunPrecedence.
//
//   - Abort wins over timeout when both fired (bash.ts:112-117).
//   - Both are checked BEFORE the exit status, as pi's try/catch is.
//   - A signal-killed child takes 128 + the signal number (pi a8b3dd199,
//     bash.ts:139 `exitCode ?? (signalCode ? 128 + signals[signalCode] : 1)`),
//     so the termination is never mistaken for a clean exit.
func classifyShellRun(ctxErr, runCtxErr, runErr error, timeoutSeconds float64) (*int, error) {
	if ctxErr != nil {
		return nil, ErrShellAborted
	}
	if runCtxErr == context.DeadlineExceeded {
		return nil, &ShellTimeoutError{Seconds: timeoutSeconds}
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return nil, runErr
		}
		code := exitCode(exitErr.ProcessState)
		return &code, nil
	}
	zero := 0
	return &zero, nil
}

// onDataWriter adapts pi's onData callback to the io.Writer runBashCommand
// feeds. A nil callback discards, which keeps Exec usable without streaming.
func onDataWriter(onData func([]byte)) io.Writer {
	if onData == nil {
		return io.Discard
	}
	return writerFunc(func(p []byte) (int, error) { onData(p); return len(p), nil })
}

type GrepOperations struct {
	// IsDirectory reports whether a path is a directory, erroring if it does
	// not exist.
	IsDirectory func(ctx context.Context, absolutePath string) (bool, error)
	// ReadFile reads a file's contents. NOTE the divergence recorded at the top
	// of this file: here this is the PRIMARY scan read, not pi's context-line
	// fetch, because the port matches in Go rather than shelling out to
	// ripgrep. Whatever this returns is what grep can match.
	ReadFile func(ctx context.Context, absolutePath string) ([]byte, error)
}

// DefaultGrepOperations reads the local filesystem.
func DefaultGrepOperations() GrepOperations {
	return GrepOperations{
		IsDirectory: func(_ context.Context, p string) (bool, error) {
			st, err := os.Stat(p)
			if err != nil {
				return false, err
			}
			return st.IsDir(), nil
		},
		ReadFile: func(_ context.Context, p string) ([]byte, error) { return os.ReadFile(p) },
	}
}

func resolveGrepOperations(ops *GrepOperations) GrepOperations {
	if ops == nil {
		return DefaultGrepOperations()
	}
	return *ops
}

// EnvGrepOperations backs the grep tool with an ExecutionEnv.
func EnvGrepOperations(env ExecutionEnv) GrepOperations {
	return GrepOperations{
		IsDirectory: func(ctx context.Context, p string) (bool, error) {
			info, err := env.Stat(ctx, p)
			if err != nil {
				return false, err
			}
			return info.Kind == FileKindDirectory, nil
		},
		ReadFile: env.ReadBinaryFile,
	}
}

// EnvBashOperations backs the shell tools with an ExecutionEnv's Shell half.
func EnvBashOperations(env ExecutionEnv) BashOperations {
	return BashOperations{
		Exec: func(ctx context.Context, command, cwd string, options BashExecOptions) (*int, error) {
			emit := func(chunk string) {
				if options.OnData != nil {
					options.OnData([]byte(chunk))
				}
			}
			res, err := env.Exec(ctx, command, &ShellExecOptions{
				Cwd:            cwd,
				TimeoutSeconds: int(options.TimeoutSeconds),
				OnStdout:       emit,
				OnStderr:       emit,
			})
			if err != nil {
				return nil, err
			}
			code := res.ExitCode
			return &code, nil
		},
	}
}

// FindOperations are the file operations the find tool performs.
type FindOperations struct {
	Exists func(ctx context.Context, absolutePath string) (bool, error)
	// Glob returns paths matching a pattern. Nil keeps pi's behavior, where the
	// default is a placeholder and real matching happens in the tool via fd.
	Glob func(ctx context.Context, pattern, cwd string, ignore []string, limit int) ([]string, error)
}

// LsOperations are the file operations the ls tool performs.
type LsOperations struct {
	Exists func(ctx context.Context, absolutePath string) (bool, error)
	// Stat reports whether a path is a directory, erroring if not found.
	Stat func(ctx context.Context, absolutePath string) (isDir bool, err error)
	// Readdir lists a directory's entry names.
	Readdir func(ctx context.Context, absolutePath string) ([]string, error)
}

// --- local defaults (pi's default*Operations) ---

// DefaultReadOperations reads from the local filesystem.
func DefaultReadOperations() ReadOperations {
	return ReadOperations{
		ReadFile: func(_ context.Context, p string) ([]byte, error) { return localReadFile(p) },
		Access:   func(_ context.Context, p string) error { return accessReadable(p) },
		DetectImageMimeType: func(_ context.Context, p string) string {
			return DetectSupportedImageMimeTypeFromFile(p)
		},
	}
}

// DefaultEditOperations reads and writes the local filesystem.
func DefaultEditOperations() EditOperations {
	return EditOperations{
		ReadFile:  func(_ context.Context, p string) ([]byte, error) { return os.ReadFile(p) },
		WriteFile: func(_ context.Context, p, content string) error { return os.WriteFile(p, []byte(content), 0o644) },
		Access:    func(_ context.Context, p string) error { return accessReadWritable(p) },
	}
}

// DefaultWriteOperations writes the local filesystem.
func DefaultWriteOperations() WriteOperations {
	return WriteOperations{
		WriteFile: func(_ context.Context, p, content string) error { return os.WriteFile(p, []byte(content), 0o644) },
		Mkdir:     func(_ context.Context, dir string) error { return os.MkdirAll(dir, 0o755) },
	}
}

// DefaultFindOperations checks the local filesystem. Glob is nil, matching pi,
// whose default is a placeholder because real matching happens in the tool.
func DefaultFindOperations() FindOperations {
	return FindOperations{
		Exists: func(_ context.Context, p string) (bool, error) { return pathExistsErr(p) },
		Glob:   nil,
	}
}

// DefaultLsOperations lists the local filesystem.
func DefaultLsOperations() LsOperations {
	return LsOperations{
		Exists: func(_ context.Context, p string) (bool, error) { return pathExistsErr(p) },
		Stat: func(_ context.Context, p string) (bool, error) {
			st, err := os.Stat(p)
			if err != nil {
				return false, err
			}
			return st.IsDir(), nil
		},
		Readdir: func(_ context.Context, p string) ([]string, error) {
			entries, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			return names, nil
		},
	}
}

// --- resolution: pi is `options?.operations ?? defaultOperations`, i.e.
// WHOLE-OBJECT replacement, not a per-member merge ---
//
// A caller who supplies operations supplies ALL of them; there is no
// spread-over-defaults anywhere in pi's tools. That is why these take a
// POINTER: nil is pi's absent `operations?`, and a non-nil value is used as
// given. Getting this wrong would silently give an injected remote filesystem a
// local fallback for any member its author left out — the opposite of what the
// seam is for.
//
// The one optional member, ReadOperations.DetectImageMimeType, is handled at
// its call site the way pi does it (`ops.detectImageMimeType ? ... : undefined`,
// read.ts:250) rather than by filling.

func resolveReadOperations(ops *ReadOperations) ReadOperations {
	if ops == nil {
		return DefaultReadOperations()
	}
	return *ops
}

func resolveEditOperations(ops *EditOperations) EditOperations {
	if ops == nil {
		return DefaultEditOperations()
	}
	return *ops
}

func resolveWriteOperations(ops *WriteOperations) WriteOperations {
	if ops == nil {
		return DefaultWriteOperations()
	}
	return *ops
}

func resolveFindOperations(ops *FindOperations) FindOperations {
	if ops == nil {
		return DefaultFindOperations()
	}
	return *ops
}

func resolveLsOperations(ops *LsOperations) LsOperations {
	if ops == nil {
		return DefaultLsOperations()
	}
	return *ops
}

// --- the bridge: an ExecutionEnv can back the coding-agent seams ---
//
// pi has two injection designs for the same tools — coding-agent's narrow
// per-tool Operations and pi-durable's broad ExecutionEnv — and they meet
// here rather than being duplicated into two tool trees.
//
// NOTE: under the mirror ruling ExecutionEnv belongs with its upstream home,
// packages/durable/src/env, so it moves out of package coding when the Go
// durable package lands. These seven Env*Operations bridges are what absorbs
// that break.

// EnvReadOperations backs the read tool with an ExecutionEnv.
func EnvReadOperations(env ExecutionEnv) ReadOperations {
	return ReadOperations{
		ReadFile: env.ReadBinaryFile,
		Access: func(ctx context.Context, p string) error {
			_, err := env.Stat(ctx, p)
			return err
		},
	}
}

// EnvEditOperations backs the edit tool with an ExecutionEnv.
func EnvEditOperations(env ExecutionEnv) EditOperations {
	return EditOperations{
		ReadFile: env.ReadBinaryFile,
		WriteFile: func(ctx context.Context, p, content string) error {
			return env.WriteFile(ctx, p, []byte(content))
		},
		Access: func(ctx context.Context, p string) error {
			_, err := env.Stat(ctx, p)
			return err
		},
	}
}

// EnvWriteOperations backs the write tool with an ExecutionEnv. Mkdir is a
// no-op because ExecutionEnv.WriteFile already creates parent directories,
// which is pi's own contract for it ("creating parent directories when
// supported").
func EnvWriteOperations(env ExecutionEnv) WriteOperations {
	return WriteOperations{
		WriteFile: func(ctx context.Context, p, content string) error {
			return env.WriteFile(ctx, p, []byte(content))
		},
		Mkdir: func(context.Context, string) error { return nil },
	}
}

// EnvLsOperations backs the ls tool with an ExecutionEnv.
func EnvLsOperations(env ExecutionEnv) LsOperations {
	return LsOperations{
		Exists: env.Exists,
		Stat: func(ctx context.Context, p string) (bool, error) {
			info, err := env.Stat(ctx, p)
			if err != nil {
				return false, err
			}
			return info.Kind == FileKindDirectory, nil
		},
		Readdir: func(ctx context.Context, p string) ([]string, error) {
			entries, err := env.ListDir(ctx, p)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, filepath.Base(e.Name))
			}
			return names, nil
		},
	}
}

// EnvFindOperations backs the find tool with an ExecutionEnv. Glob stays nil,
// as in pi's default: an env exposes no glob, so matching stays with the tool.
func EnvFindOperations(env ExecutionEnv) FindOperations {
	return FindOperations{Exists: env.Exists}
}

// localReadFile is os.ReadFile plus Node's EISDIR text. Node's fs.readFile
// raises `EISDIR: illegal operation on a directory, read` for a directory and
// Go's os.ReadFile reports something else, so the local filesystem
// implementation is where that difference is reconciled — which is also where
// Node keeps it.
func localReadFile(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, errors.New("EISDIR: illegal operation on a directory, read")
	}
	return os.ReadFile(path)
}

// accessReadable mirrors pi's fsAccess(path, R_OK).
func accessReadable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// accessReadWritable mirrors pi's fsAccess(path, R_OK|W_OK).
func accessReadWritable(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// pathExistsErr reports existence, distinguishing "absent" from a real failure.
func pathExistsErr(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
