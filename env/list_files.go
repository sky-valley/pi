package env

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/sky-valley/pi/coding"
	"github.com/sky-valley/pi/internal/jstext"
)

// ListFilesOptions say what ListFiles lists (Durable's ListFilesOptions).
// Nil fields take the daemon's defaults.
type ListFilesOptions struct {
	// Hidden includes names starting with "." (on Windows also files with
	// the hidden attribute); nil is true. Entries named .git are never
	// listed.
	Hidden *bool
	// Gitignore skips, inside a git work tree, what git ignores; nil is
	// true. Outside work trees, .gitignore files have no effect.
	Gitignore *bool
	// Exclude names entries to skip with everything below them, such as
	// node_modules.
	Exclude []string
	// MaxEntries is the most entries to list; nil is 50 000.
	MaxEntries *int64
	// MaxBytes is the most bytes of the listing, each entry's UTF-8 path
	// plus two; nil is 4 MiB, at most 15.9 MiB.
	MaxBytes *int64
	// TimeoutMS is the time after which the listing stops; nil is 10 000.
	TimeoutMS *int64
}

// FileListEntry is one entry of a FileListing.
type FileListEntry struct {
	// Path is relative to the listed directory, with / separators on every
	// system.
	Path string
	// Kind is file, directory or symlink; a symbolic link is not followed.
	Kind coding.FileKind
}

// FileListing is the result of ListFiles.
type FileListing struct {
	// Entries are depth first, parents before their entries, siblings in
	// byte order of their names.
	Entries []FileListEntry
	// Truncated reports that a bound in ListFilesOptions ended the listing
	// early.
	Truncated bool
}

// listFileKinds are the kind bytes of listFiles records.
var listFileKinds = map[byte]coding.FileKind{'f': coding.FileKindFile, 'd': coding.FileKindDirectory, 'l': coding.FileKindSymlink}

// listFilesOver lists an absolute path through the daemon: it fails with
// not_supported if the daemon does not list files (a release before
// listFiles), so callers fall back to their own walk.
func listFilesOver(ctx context.Context, connection *Connection, path string, options *ListFilesOptions) (FileListing, error) {
	if options == nil {
		options = &ListFilesOptions{}
	}
	for _, bound := range []struct {
		name  string
		value *int64
	}{{"maxEntries", options.MaxEntries}, {"maxBytes", options.MaxBytes}, {"timeoutMs", options.TimeoutMS}} {
		if bound.value != nil && (*bound.value < 0 || *bound.value > maxSafeInteger) {
			return FileListing{}, &FileError{Code: FileInvalid, Message: bound.name + " must be a non-negative safe integer", Path: path}
		}
	}
	if failure := abortResult(ctx, path); failure != nil {
		return FileListing{}, failure
	}
	info, err := connection.Info(ctx)
	if err != nil {
		return FileListing{}, toFileError(err, path)
	}
	if !info.hasFeature("listFiles") {
		return FileListing{}, &FileError{Code: FileNotSupported, Message: "The pi-env daemon does not list files; update it", Path: path}
	}
	// Fields left out are not sent; the daemon applies the defaults.
	fields := object{}.with("path", path)
	if options.Hidden != nil {
		fields = fields.with("hidden", *options.Hidden)
	}
	if options.Gitignore != nil {
		fields = fields.with("gitignore", *options.Gitignore)
	}
	if options.Exclude != nil {
		fields = fields.with("exclude", options.Exclude)
	}
	if options.MaxEntries != nil {
		fields = fields.with("maxEntries", *options.MaxEntries)
	}
	if options.MaxBytes != nil {
		fields = fields.with("maxBytes", *options.MaxBytes)
	}
	if options.TimeoutMS != nil {
		fields = fields.with("timeoutMs", *options.TimeoutMS)
	}
	reply, err := connection.Request(ctx, "listFiles", fields, nil)
	if err != nil {
		return FileListing{}, toFileError(err, path)
	}
	if failure := abortResult(ctx, path); failure != nil {
		return FileListing{}, failure
	}
	// Records of a kind byte, the UTF-8 path, and a NUL.
	entries := []FileListEntry{}
	for records := reply.Payload; ; {
		end := bytes.IndexByte(records, 0)
		if end == -1 {
			break
		}
		if kind, ok := listFileKinds[records[0]]; ok && end > 0 {
			entries = append(entries, FileListEntry{Path: jstext.DecodeUTF8(records[1:end]), Kind: kind})
		}
		records = records[end+1:]
	}
	var result struct {
		Truncated bool `json:"truncated"`
	}
	_ = json.Unmarshal(reply.JSON, &result)
	return FileListing{Entries: entries, Truncated: result.Truncated}, nil
}

// LocalExecutionEnvOptions configure a LocalExecutionEnv.
type LocalExecutionEnvOptions struct {
	// Cwd is the working directory; empty is the process's.
	Cwd string
	// Connection is a connection to this machine's daemon, for ListFiles:
	// NewConnection(ConnectionOptions{Command: []string{daemonPath}}) with
	// the path LocalDaemon returns.
	Connection *Connection
}

// LocalExecutionEnv is the local environment (coding.LocalEnv) with
// ListFiles through a pi-env daemon on the same machine, so local and remote
// environments list files with the same code. The connection belongs to the
// caller, who closes it; several environments may share it.
type LocalExecutionEnv struct {
	*coding.LocalEnv
	connection *Connection
}

// NewLocalExecutionEnv returns a local environment listing files through
// options.Connection.
func NewLocalExecutionEnv(options LocalExecutionEnvOptions) *LocalExecutionEnv {
	return &LocalExecutionEnv{LocalEnv: coding.NewLocalEnv(options.Cwd), connection: options.Connection}
}

// ListFiles lists the tree below a directory through the daemon.
func (e *LocalExecutionEnv) ListFiles(ctx context.Context, path string, options *ListFilesOptions) (FileListing, error) {
	// NodeExecutionEnv's absolutePath does not check for an abort;
	// listFilesOver does.
	resolved, err := e.AbsolutePath(context.WithoutCancel(ctx), path)
	if err != nil {
		return FileListing{}, err
	}
	return listFilesOver(ctx, e.connection, resolved, options)
}
