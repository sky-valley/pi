package env

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/coding"
	"github.com/sky-valley/pi/env/internal/nodepath"
	"github.com/sky-valley/pi/internal/jstext"
)

const (
	maxTimeoutMS = 2_147_483_647
	// readChunk is the bytes per pread request of a binary reader; several
	// are in flight at once.
	readChunk = 256 * 1024
	readDepth = 8
	// writeChunk: Node's writeFile writes at most this much per call and
	// checks for an abort before each.
	writeChunk = 512 * 1024
	writeDepth = 8
	// Node's readFile: chunk of known-size reads, chunk of unknown-size
	// reads, and the largest file it reads.
	readFileChunk        = 512 * 1024
	readFileUnknownChunk = 64 * 1024
	readFileMax          = 1<<31 - 1
	// lineChunk is NodeTextLineReader's read size.
	lineChunk = 64 * 1024
	// maxSafeInteger is Number.MAX_SAFE_INTEGER, the bound of the sizes and
	// offsets JavaScript callers can pass.
	maxSafeInteger = 1<<53 - 1
)

// RemoteExecutionEnvOptions configure a RemoteExecutionEnv.
type RemoteExecutionEnvOptions struct {
	Connection *Connection
	// ID is the file namespace: equal ids see the same files, e.g.
	// "pi-env:<env name>".
	ID  string
	Cwd string
	// ShellPath is the shell string commands run with; empty is the daemon's
	// default.
	ShellPath string
	// ShellEnv is added to the remote environment of every command that
	// inherits it, like NodeExecutionEnv's shellEnv.
	ShellEnv map[string]string
	// Watch is how the daemon watches, like NodeExecutionEnv's watch option.
	Watch RemoteWatchOptions
}

// RemoteExecutionEnv is an execution environment on another machine, reached
// through a pi-env daemon. Results match NodeExecutionEnv running on that
// machine: the daemon performs the system calls, and this type applies Node's
// path, error, and result rules.
//
// It implements coding.ExecutionEnv. The rest of Durable's ExecutionEnv,
// which has no Go interface yet, is here as methods of the same names:
// OpenBinaryReader, OpenDirReader, ListFiles, Watch, TruncateFile, FlushFile,
// CreateDir, Remove, CreateTempDir, CreateTempFile, and Durable's exec as
// RunShell and RunProgram.
type RemoteExecutionEnv struct {
	connection   *Connection
	id           string
	shellPath    string
	shellEnv     map[string]string
	watchOptions RemoteWatchOptions

	mu  sync.Mutex
	cwd string
	// running are the commands this environment started, for Cleanup.
	running map[uint32]int
}

// NewRemoteExecutionEnv returns an environment over options.Connection.
func NewRemoteExecutionEnv(options RemoteExecutionEnvOptions) *RemoteExecutionEnv {
	return &RemoteExecutionEnv{
		connection:   options.Connection,
		id:           options.ID,
		cwd:          options.Cwd,
		shellPath:    options.ShellPath,
		shellEnv:     options.ShellEnv,
		watchOptions: options.Watch,
		running:      map[uint32]int{},
	}
}

var _ coding.ExecutionEnv = (*RemoteExecutionEnv)(nil)

// ID is the file namespace (FileSystem.id).
func (e *RemoteExecutionEnv) ID() string { return e.id }

// Connection is the environment's connection.
func (e *RemoteExecutionEnv) Connection() *Connection { return e.connection }

// Cwd implements coding.FileSystem.
func (e *RemoteExecutionEnv) Cwd() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

// SetCwd changes the working directory relative paths resolve against.
func (e *RemoteExecutionEnv) SetCwd(cwd string) {
	e.mu.Lock()
	e.cwd = cwd
	e.mu.Unlock()
}

// remotePath is Node's path flavor of the remote system. As upstream's, it
// waits for the daemon to start whether or not the operation was aborted:
// its callers check for aborts where upstream does.
func remotePath(connection *Connection) (nodepath.Flavor, error) {
	info, err := connection.Info(context.Background())
	if err != nil {
		return nil, err
	}
	return flavorOf(info), nil
}

func flavorOf(info RemoteInfo) nodepath.Flavor {
	if platform, _ := NodePlatform(info); platform == "win32" {
		return nodepath.Win32
	}
	return nodepath.Posix
}

// remoteFileInfo is an info record of the daemon.
type remoteFileInfo struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	MtimeSec  int64  `json:"mtimeSec"`
	MtimeNsec int64  `json:"mtimeNsec"`
}

type remoteDirEntry struct {
	Name  string          `json:"name"`
	Raw   []byte          `json:"-"`
	Info  *remoteFileInfo `json:"info"`
	Error map[string]any  `json:"error"`
}

func (d *remoteDirEntry) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name  string          `json:"name"`
		Raw   []int           `json:"raw"`
		Info  *remoteFileInfo `json:"info"`
		Error map[string]any  `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	d.Name, d.Info, d.Error = raw.Name, raw.Info, raw.Error
	if raw.Raw != nil {
		d.Raw = make([]byte, len(raw.Raw))
		for i, b := range raw.Raw {
			d.Raw[i] = byte(b)
		}
	}
	return nil
}

func toInfo(path string, remote remoteFileInfo, paths nodepath.Flavor) (coding.FileInfo, *FileError) {
	switch remote.Kind {
	case "file", "directory", "symlink":
	default:
		return coding.FileInfo{}, &FileError{Code: FileInvalid, Message: "Unsupported file type", Path: path}
	}
	return coding.FileInfo{
		Name:    paths.Basename(path),
		Path:    path,
		Kind:    coding.FileKind(remote.Kind),
		Size:    remote.Size,
		ModTime: time.Unix(remote.MtimeSec, remote.MtimeNsec),
	}, nil
}

// readRange reads length bytes from offset as consecutive reads of at most
// chunk bytes, several in flight. It ends early at the end of the file. A
// short read continues from where it ended, as sequential reads would.
// aborted is checked after each read; ok false means aborted.
func readRange(begin func(offset, length int64) (*call, error), offset, length, chunk int64, aborted func() bool) (data []byte, ok bool, err error) {
	type flight struct {
		length int64
		call   *call
		err    error
	}
	var inFlight []flight
	var out []byte
	total := int64(0)
	next := offset
	for total < length {
		for len(inFlight) < readDepth && next < offset+length {
			size := min(offset+length-next, chunk)
			c, err := begin(next, size)
			inFlight = append(inFlight, flight{size, c, err})
			next += size
		}
		head := inFlight[0]
		inFlight = inFlight[1:]
		if head.err != nil {
			return nil, false, head.err
		}
		reply, err := head.call.wait(context.Background())
		if err != nil {
			return nil, false, err
		}
		if aborted() {
			return nil, false, nil
		}
		if len(reply.Payload) == 0 {
			break
		}
		out = append(out, reply.Payload...)
		total += int64(len(reply.Payload))
		if int64(len(reply.Payload)) < head.length {
			// Reads already in flight assumed a full read; continue right
			// after this one.
			inFlight = nil
			next = offset + total
		}
	}
	return out, true, nil
}

// handle is a daemon handle: valid only in the session that opened it.
type handle struct {
	env     *RemoteExecutionEnv
	id      int64
	session int
	path    string
}

func (h *handle) fields(fields object) object { return fields.with("handle", h.id) }

func (h *handle) request(ctx context.Context, op string, fields object, payload []byte) (Reply, error) {
	return h.env.connection.Request(ctx, op, h.fields(fields), &RequestOptions{Session: h.session, Payload: payload})
}

// beginPread sends a pread; offset -1 reads sequentially.
func (h *handle) beginPread(offset, length int64) (*call, error) {
	fields := object{}
	if offset >= 0 {
		fields = fields.with("offset", offset)
	}
	return h.env.connection.begin(context.Background(), "pread", h.fields(fields.with("length", length)), &RequestOptions{Session: h.session})
}

func (h *handle) pread(offset, length int64) ([]byte, error) {
	c, err := h.beginPread(offset, length)
	if err != nil {
		return nil, err
	}
	reply, err := c.wait(context.Background())
	return reply.Payload, err
}

func (h *handle) close() {
	_, _ = h.request(context.Background(), "close", nil, nil)
}

// BinaryReader reads ranges of one opened regular file; all calls see the
// same file even if its path is renamed (Durable's BinaryReader).
type BinaryReader interface {
	// Info is the metadata of the opened file, not of whatever its path
	// names now.
	Info(ctx context.Context) (coding.FileInfo, error)
	// Read returns up to length bytes at offset; fewer only at the end of
	// the file.
	Read(ctx context.Context, offset, length int64) ([]byte, error)
	// ScanLines locates lines [StartLine, EndLine) in one pass over the file.
	ScanLines(ctx context.Context, lines LineRange) (LineScan, error)
	// Close releases the file; it is best-effort and idempotent.
	Close() error
}

// LineRange selects lines for ScanLines: 0-based, where line k starts after
// the k-th newline byte. EndLine 0 means to the end of the file (a given end
// is always above StartLine).
type LineRange struct {
	StartLine int64
	EndLine   int64
}

// LineScan is where lines of a file are, as ScanLines found them (Durable's
// LineScan).
type LineScan struct {
	// Newlines is the count of newline bytes in the whole file; it has
	// Newlines+1 lines.
	Newlines int64 `json:"newlines"`
	// Start and End are the byte range of the selected lines, without the
	// newline that ends the last.
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	// FirstLineEnd is where the first selected line ends.
	FirstLineEnd int64 `json:"firstLineEnd"`
	// LastLineStart is where the last selected line starts.
	LastLineStart int64 `json:"lastLineStart"`
	// SelectedBytes and FirstLineBytes are UTF-8 lengths of the decoded
	// selection and of its first line.
	SelectedBytes  int64 `json:"selectedBytes"`
	FirstLineBytes int64 `json:"firstLineBytes"`
}

type remoteBinaryReader struct {
	handle *handle
	mu     sync.Mutex
	closed bool
}

func (r *remoteBinaryReader) early(ctx context.Context) error {
	if failure := abortResult(ctx, r.handle.path); failure != nil {
		return failure
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return &FileError{Code: FileInvalid, Message: "Binary reader is closed", Path: r.handle.path}
	}
	return nil
}

func (r *remoteBinaryReader) Info(ctx context.Context) (coding.FileInfo, error) {
	if err := r.early(ctx); err != nil {
		return coding.FileInfo{}, err
	}
	reply, err := r.handle.request(context.Background(), "fstat", nil, nil)
	if err != nil {
		return coding.FileInfo{}, toFileError(err, r.handle.path)
	}
	var remote remoteFileInfo
	if err := json.Unmarshal(reply.JSON, &remote); err != nil {
		return coding.FileInfo{}, toFileError(err, r.handle.path)
	}
	paths, err := remotePath(r.handle.env.connection)
	if err != nil {
		return coding.FileInfo{}, toFileError(err, r.handle.path)
	}
	info, failure := toInfo(r.handle.path, remote, paths)
	if failure != nil {
		return coding.FileInfo{}, failure
	}
	return info, nil
}

func (r *remoteBinaryReader) Read(ctx context.Context, offset, length int64) ([]byte, error) {
	if err := r.early(ctx); err != nil {
		return nil, err
	}
	if offset < 0 || offset > maxSafeInteger || length < 0 || length > maxSafeInteger {
		return nil, &FileError{Code: FileInvalid, Message: "Offset and length must be non-negative safe integers", Path: r.handle.path}
	}
	data, ok, err := readRange(r.handle.beginPread, offset, length, readChunk, func() bool { return ctx.Err() != nil })
	if err != nil {
		return nil, toFileError(err, r.handle.path)
	}
	if !ok {
		return nil, &FileError{Code: FileAborted, Message: "aborted", Path: r.handle.path}
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

func (r *remoteBinaryReader) ScanLines(ctx context.Context, lines LineRange) (LineScan, error) {
	if err := r.early(ctx); err != nil {
		return LineScan{}, err
	}
	if lines.StartLine < 0 || lines.StartLine > maxSafeInteger ||
		(lines.EndLine != 0 && (lines.EndLine > maxSafeInteger || lines.EndLine <= lines.StartLine)) {
		return LineScan{}, &FileError{Code: FileInvalid, Message: "Invalid line range", Path: r.handle.path}
	}
	fields := object{}.with("startLine", lines.StartLine)
	if lines.EndLine != 0 {
		fields = fields.with("endLine", lines.EndLine)
	}
	reply, err := r.handle.request(ctx, "scanLines", fields, nil)
	if err != nil {
		return LineScan{}, toFileError(err, r.handle.path)
	}
	var scan LineScan
	if err := json.Unmarshal(reply.JSON, &scan); err != nil {
		return LineScan{}, toFileError(err, r.handle.path)
	}
	return scan, nil
}

func (r *remoteBinaryReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	r.handle.close()
	return nil
}

// DirReader pages through one directory's entries (Durable's DirReader).
type DirReader interface {
	// Next returns up to maxEntries entries in the order the file system
	// returns them, continuing where the previous call stopped; done marks
	// the end. Entries that disappear before their metadata is read are
	// skipped, as are entries of unsupported kinds.
	Next(ctx context.Context, maxEntries int) (entries []coding.FileInfo, done bool, err error)
	Close() error
}

type remoteDirReader struct {
	handle *handle
	mu     sync.Mutex
	done   bool
	closed bool
}

type readdirReply struct {
	Entries []remoteDirEntry `json:"entries"`
	Done    bool             `json:"done"`
}

func (r *remoteDirReader) Next(ctx context.Context, maxEntries int) ([]coding.FileInfo, bool, error) {
	path := r.handle.path
	if failure := abortResult(ctx, path); failure != nil {
		return nil, false, failure
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false, &FileError{Code: FileInvalid, Message: "Directory reader is closed", Path: path}
	}
	if maxEntries <= 0 || int64(maxEntries) > maxSafeInteger {
		return nil, false, &FileError{Code: FileInvalid, Message: "maxEntries must be a positive safe integer", Path: path}
	}
	entries := []coding.FileInfo{}
	paths, err := remotePath(r.handle.env.connection)
	if err != nil {
		return nil, false, toFileError(err, path)
	}
	// Like NodeDirReader, count only entries that are part of the listing.
	for !r.done && len(entries) < maxEntries {
		reply, err := r.handle.request(context.Background(), "readdir", object{}.with("max", maxEntries-len(entries)), nil)
		if err != nil {
			return nil, false, toFileError(err, path)
		}
		if failure := abortResult(ctx, path); failure != nil {
			return nil, false, failure
		}
		var page readdirReply
		if err := json.Unmarshal(reply.JSON, &page); err != nil {
			return nil, false, toFileError(err, path)
		}
		for _, entry := range page.Entries {
			entryPath := paths.Resolve(path, entry.Name)
			if entry.Error != nil {
				// Removed between enumeration and lstat: not part of the
				// listing any more.
				if entry.Error["code"] == "ENOENT" {
					continue
				}
				return nil, false, toFileError(newRemoteError(entry.Error), entryPath)
			}
			if entry.Info == nil {
				continue
			}
			if info, failure := toInfo(entryPath, *entry.Info, paths); failure == nil {
				entries = append(entries, info)
			}
		}
		r.done = page.Done
	}
	return entries, r.done, nil
}

func (r *remoteDirReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	r.handle.close()
	return nil
}

// remoteTextLineReader is NodeTextLineReader: positional reads of a file
// opened like open(path, "r").
type remoteTextLineReader struct {
	handle   *handle
	mu       sync.Mutex
	decoder  jstext.TextDecoder
	offset   int64
	buffered string
	ended    bool
	closed   bool
}

func (r *remoteTextLineReader) ReadLine(ctx context.Context) (coding.TextLine, error) {
	path := r.handle.path
	if failure := abortResult(ctx, path); failure != nil {
		return coding.TextLine{}, failure
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return coding.TextLine{}, &FileError{Code: FileInvalid, Message: "Text line reader is closed", Path: path}
	}
	for {
		if newline := strings.IndexByte(r.buffered, '\n'); newline != -1 {
			text := r.buffered[:newline]
			r.buffered = r.buffered[newline+1:]
			return coding.TextLine{Text: text, Terminated: true}, nil
		}
		if r.ended {
			if r.buffered == "" {
				return coding.TextLine{}, io.EOF
			}
			text := r.buffered
			r.buffered = ""
			return coding.TextLine{Text: text}, nil
		}
		data, err := r.handle.pread(r.offset, lineChunk)
		if err != nil {
			return coding.TextLine{}, toFileError(err, path)
		}
		if failure := abortResult(ctx, path); failure != nil {
			return coding.TextLine{}, failure
		}
		r.offset += int64(len(data))
		if len(data) == 0 {
			r.buffered += r.decoder.Flush()
			r.ended = true
		} else {
			r.buffered += r.decoder.Decode(data)
		}
	}
}

func (r *remoteTextLineReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.buffered = ""
	r.mu.Unlock()
	r.handle.close()
	return nil
}

var driveRelative = regexp.MustCompile(`^([a-zA-Z]:)([^\\/]|$)`)

// resolve is Node's resolvePath on the remote system, with its home
// directory and working directories.
func (e *RemoteExecutionEnv) resolve(ctx context.Context, path string) (string, error) {
	info, err := e.connection.Info(ctx)
	if err != nil {
		return "", err
	}
	platform, _ := NodePlatform(info)
	windows := platform == "win32"
	paths := flavorOf(info)
	cwd := e.Cwd()
	normalized := path
	switch {
	case normalized == "~":
		normalized = info.Home
	case strings.HasPrefix(normalized, "~/") || (windows && strings.HasPrefix(normalized, "~\\")):
		normalized = paths.Join(info.Home, normalized[2:])
	case strings.HasPrefix(normalized, "file://"):
		// Malformed URLs stay ordinary paths, as in Node.
		if converted, err := nodepath.FileURLToPath(normalized, windows); err == nil {
			normalized = converted
		}
	}
	if paths.IsAbsolute(normalized) {
		return paths.Resolve(normalized), nil
	}
	if windows {
		// A drive-relative path on another drive: Node on Windows resolves it
		// against that drive's working directory (=D:), else its own working
		// directory if on that drive, else the drive's root.
		if m := driveRelative.FindStringSubmatch(normalized); m != nil && jstext.ToLower(m[1]) != jstext.ToLower(jsSlice(cwd, 0, 2)) {
			base, ok := info.DriveCwds[strings.ToUpper(m[1])]
			if !ok {
				base = info.Cwd
			}
			if jstext.ToLower(jsSlice(base, 0, 2)) != jstext.ToLower(m[1]) && len(base) > 2 && base[2] == '\\' {
				base = m[1] + "\\"
			}
			return nodepath.Win32.Resolve(base, normalized), nil
		}
	}
	return paths.Resolve(cwd, normalized), nil
}

// jsSlice is String.prototype.slice for non-negative byte offsets.
func jsSlice(s string, start, end int) string {
	start, end = min(start, len(s)), min(end, len(s))
	if start >= end {
		return ""
	}
	return s[start:end]
}

// fileOp resolves path, checks for an abort, and runs op on the resolved
// path; checkAfter also checks once op is done.
func fileOp[T any](e *RemoteExecutionEnv, ctx context.Context, path string, checkAfter bool, op func(resolved string) (T, error)) (T, error) {
	var zero T
	resolved, err := e.resolve(ctx, path)
	if err != nil {
		return zero, toFileError(err, path)
	}
	if failure := abortResult(ctx, resolved); failure != nil {
		return zero, failure
	}
	value, err := op(resolved)
	if err != nil {
		return zero, toFileError(err, resolved)
	}
	if checkAfter {
		if failure := abortResult(ctx, resolved); failure != nil {
			return zero, failure
		}
	}
	return value, nil
}

type openReply struct {
	Handle    int64           `json:"handle"`
	Stat      *remoteFileInfo `json:"stat"`
	StatError map[string]any  `json:"statError"`
}

func (e *RemoteExecutionEnv) open(resolved string, fields object) (*handle, openReply, error) {
	reply, err := e.connection.Request(context.Background(), "open", append(object{}.with("path", resolved), fields...), nil)
	if err != nil {
		return nil, openReply{}, err
	}
	var opened openReply
	if err := json.Unmarshal(reply.JSON, &opened); err != nil {
		return nil, openReply{}, err
	}
	return &handle{env: e, id: opened.Handle, session: reply.Session, path: resolved}, opened, nil
}

// AbsolutePath implements coding.FileSystem. As upstream's absolutePath, it
// does not look at ctx.
func (e *RemoteExecutionEnv) AbsolutePath(ctx context.Context, path string) (string, error) {
	resolved, err := e.resolve(context.WithoutCancel(ctx), path)
	if err != nil {
		return "", toFileError(err, path)
	}
	return resolved, nil
}

// JoinPath implements coding.FileSystem. As upstream's joinPath, it does
// not look at ctx.
func (e *RemoteExecutionEnv) JoinPath(_ context.Context, parts []string) (string, error) {
	paths, err := remotePath(e.connection)
	if err != nil {
		return "", toFileError(err, "")
	}
	return paths.Join(parts...), nil
}

// readFile is Node's readFile, with or without UTF-8 decoding, over a file
// opened like open(path, "r").
func (e *RemoteExecutionEnv) readFile(ctx context.Context, path string, decode bool) ([]byte, error) {
	return fileOp(e, ctx, path, false, func(resolved string) ([]byte, error) {
		h, opened, err := e.open(resolved, object{}.with("mode", "read"))
		if err != nil {
			return nil, err
		}
		defer h.close()
		aborted := func() bool { return ctx.Err() != nil }
		abort := &FileError{Code: FileAborted, Message: "The operation was aborted", Path: resolved}
		if opened.StatError != nil {
			return nil, newRemoteError(opened.StatError)
		}
		if aborted() {
			return nil, abort
		}
		size := int64(0)
		if opened.Stat != nil && opened.Stat.Kind == "file" {
			size = opened.Stat.Size
		}
		if size > readFileMax {
			return nil, &FileError{Code: FileUnknown, Message: fmt.Sprintf("File size (%d) is greater than 2 GiB", size), Path: resolved}
		}
		if size == 0 {
			// Unknown size: sequential reads until the end, as for FIFOs and
			// devices.
			var data []byte
			for {
				if aborted() {
					return nil, abort
				}
				chunk, err := h.pread(-1, readFileUnknownChunk)
				if err != nil {
					return nil, err
				}
				if len(chunk) == 0 {
					break
				}
				data = append(data, chunk...)
			}
			return data, nil
		}
		if !decode {
			// A single read of the whole size, or reads of 512 KiB until the
			// size or the end of the file.
			if size <= readFileChunk {
				return h.pread(0, size)
			}
			data, ok, err := readRange(h.beginPread, 0, size, readFileChunk, aborted)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, abort
			}
			return data, nil
		}
		// Decoding: reads of min(size, 512 KiB) until the size, a short read,
		// or the end of the file. Node's StringDecoder over these reads gives
		// the text of decoding all the bytes at once.
		length := min(size, readFileChunk)
		var data []byte
		total := int64(0)
		var inFlight []*call
		var inFlightErr []error
		next := int64(0)
		for {
			for len(inFlight) < readDepth {
				c, err := h.beginPread(next, length)
				inFlight = append(inFlight, c)
				inFlightErr = append(inFlightErr, err)
				next += length
			}
			head, headErr := inFlight[0], inFlightErr[0]
			inFlight, inFlightErr = inFlight[1:], inFlightErr[1:]
			if headErr != nil {
				return nil, headErr
			}
			reply, err := head.wait(context.Background())
			if err != nil {
				return nil, err
			}
			if aborted() {
				return nil, abort
			}
			total += int64(len(reply.Payload))
			data = append(data, reply.Payload...)
			if len(reply.Payload) == 0 || total == size || int64(len(reply.Payload)) != length {
				return data, nil
			}
		}
	})
}

// ReadTextFile implements coding.FileSystem.
func (e *RemoteExecutionEnv) ReadTextFile(ctx context.Context, path string) (string, error) {
	data, err := e.readFile(ctx, path, true)
	if err != nil {
		return "", err
	}
	return jstext.DecodeUTF8(data), nil
}

// ReadBinaryFile implements coding.FileSystem.
func (e *RemoteExecutionEnv) ReadBinaryFile(ctx context.Context, path string) ([]byte, error) {
	data, err := e.readFile(ctx, path, false)
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

// BinaryReaderOptions are OpenBinaryReader's options.
type BinaryReaderOptions struct {
	// NoFollow refuses a symbolic link as the final path component (invalid)
	// instead of following it; earlier components are still resolved.
	NoFollow bool
}

// OpenBinaryReader opens a regular file for bounded positional reads. A
// directory fails with is_directory, other non-regular files with invalid.
func (e *RemoteExecutionEnv) OpenBinaryReader(ctx context.Context, path string, options *BinaryReaderOptions) (BinaryReader, error) {
	noFollow := options != nil && options.NoFollow
	h, err := fileOp(e, ctx, path, false, func(resolved string) (*handle, error) {
		h, _, err := e.open(resolved, object{}.with("noFollow", noFollow))
		return h, err
	})
	if err != nil {
		return nil, err
	}
	if failure := abortResult(ctx, h.path); failure != nil {
		h.close()
		return nil, failure
	}
	return &remoteBinaryReader{handle: h}, nil
}

// OpenTextLineReader implements coding.FileSystem.
func (e *RemoteExecutionEnv) OpenTextLineReader(ctx context.Context, path string) (coding.TextLineReader, error) {
	h, err := fileOp(e, ctx, path, false, func(resolved string) (*handle, error) {
		h, _, err := e.open(resolved, object{}.with("mode", "read"))
		return h, err
	})
	if err != nil {
		return nil, err
	}
	if failure := abortResult(ctx, h.path); failure != nil {
		h.close()
		return nil, failure
	}
	return &remoteTextLineReader{handle: h}, nil
}

// ReadTextLines implements coding.FileSystem: maxLines of 0 or less reads
// every line.
func (e *RemoteExecutionEnv) ReadTextLines(ctx context.Context, path string, maxLines int) ([]string, error) {
	reader, err := e.OpenTextLineReader(ctx, path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	lines := []string{}
	for maxLines <= 0 || len(lines) < maxLines {
		line, err := reader.ReadLine(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		lines = append(lines, line.Text)
	}
	return lines, nil
}

// write is Node's writeFile (abort checked before each 512 KiB write) or
// appendFile (no checks, one after): the first request creates parents,
// opens, and writes the first chunk; later chunks go to the same open file.
func (e *RemoteExecutionEnv) write(ctx context.Context, path string, content []byte, appending bool) error {
	aborted := func() bool { return !appending && ctx.Err() != nil }
	_, err := fileOp(e, ctx, path, appending, func(resolved string) (struct{}, error) {
		first := content[:min(len(content), writeChunk)]
		keep := len(content) > writeChunk
		fields := object{}.with("path", resolved).with("append", appending).with("keep", keep)
		reply, err := e.connection.Request(context.Background(), "write", fields, &RequestOptions{Payload: first})
		if err != nil || !keep {
			return struct{}{}, err
		}
		var opened openReply
		if err := json.Unmarshal(reply.JSON, &opened); err != nil {
			return struct{}{}, err
		}
		h := &handle{env: e, id: opened.Handle, session: reply.Session, path: resolved}
		var inFlight []*call
		var inFlightErr []error
		settle := func() error {
			var first error
			for i, c := range inFlight {
				err := inFlightErr[i]
				if err == nil {
					_, err = c.wait(context.Background())
				}
				if err != nil && first == nil {
					first = err
				}
			}
			inFlight, inFlightErr = nil, nil
			return first
		}
		defer h.close()
		for offset := writeChunk; offset < len(content); offset += writeChunk {
			if aborted() {
				// A chunk that failed before this check fails the write, as it
				// would have in sequence.
				if err := settle(); err != nil {
					return struct{}{}, err
				}
				return struct{}{}, &FileError{Code: FileAborted, Message: "The operation was aborted", Path: resolved}
			}
			c, err := e.connection.begin(context.Background(), "writeChunk", h.fields(nil), &RequestOptions{Session: h.session, Payload: content[offset:min(offset+writeChunk, len(content))]})
			inFlight = append(inFlight, c)
			inFlightErr = append(inFlightErr, err)
			if len(inFlight) >= writeDepth {
				head, headErr := inFlight[0], inFlightErr[0]
				inFlight, inFlightErr = inFlight[1:], inFlightErr[1:]
				if headErr == nil {
					_, headErr = head.wait(context.Background())
				}
				if headErr != nil {
					settle()
					return struct{}{}, headErr
				}
			}
		}
		// The first failure; later chunks are refused by the daemon, so the
		// file has no gaps.
		return struct{}{}, settle()
	})
	return err
}

// WriteFile implements coding.FileSystem.
func (e *RemoteExecutionEnv) WriteFile(ctx context.Context, path string, content []byte) error {
	return e.write(ctx, path, content, false)
}

// AppendFile implements coding.FileSystem.
func (e *RemoteExecutionEnv) AppendFile(ctx context.Context, path string, content []byte) error {
	return e.write(ctx, path, content, true)
}

// TruncateFile truncates or extends a file to exactly size bytes.
func (e *RemoteExecutionEnv) TruncateFile(ctx context.Context, path string, size int64) error {
	_, err := fileOp(e, ctx, path, true, func(resolved string) (struct{}, error) {
		if size < 0 || size > maxSafeInteger {
			return struct{}{}, &FileError{Code: FileInvalid, Message: "File size must be a non-negative safe integer", Path: resolved}
		}
		_, err := e.connection.Request(context.Background(), "truncate", object{}.with("path", resolved).with("size", size), nil)
		return struct{}{}, err
	})
	return err
}

// FlushFile flushes a file's contents and the metadata needed to retrieve
// them.
func (e *RemoteExecutionEnv) FlushFile(ctx context.Context, path string) error {
	_, err := fileOp(e, ctx, path, true, func(resolved string) (struct{}, error) {
		_, err := e.connection.Request(context.Background(), "fsync", object{}.with("path", resolved), nil)
		return struct{}{}, err
	})
	return err
}

// RenameFile implements coding.FileSystem.
func (e *RemoteExecutionEnv) RenameFile(ctx context.Context, sourcePath, destinationPath string) error {
	source, err := e.resolve(ctx, sourcePath)
	if err == nil {
		var destination string
		destination, err = e.resolve(ctx, destinationPath)
		if err == nil {
			if failure := abortResult(ctx, destination); failure != nil {
				return failure
			}
			_, err = e.connection.Request(context.Background(), "rename", object{}.with("path", source).with("to", destination), nil)
			if err != nil {
				return toFileError(err, source)
			}
			return nil
		}
	}
	return toFileError(err, sourcePath)
}

// Stat implements coding.FileSystem (Durable's fileInfo).
func (e *RemoteExecutionEnv) Stat(ctx context.Context, path string) (coding.FileInfo, error) {
	return fileOp(e, ctx, path, false, func(resolved string) (coding.FileInfo, error) {
		reply, err := e.connection.Request(context.Background(), "lstat", object{}.with("path", resolved), nil)
		if err != nil {
			return coding.FileInfo{}, err
		}
		var remote remoteFileInfo
		if err := json.Unmarshal(reply.JSON, &remote); err != nil {
			return coding.FileInfo{}, err
		}
		paths, err := remotePath(e.connection)
		if err != nil {
			return coding.FileInfo{}, err
		}
		info, failure := toInfo(resolved, remote, paths)
		if failure != nil {
			return coding.FileInfo{}, failure
		}
		return info, nil
	})
}

// ListDir implements coding.FileSystem.
func (e *RemoteExecutionEnv) ListDir(ctx context.Context, path string) ([]coding.FileInfo, error) {
	return fileOp(e, ctx, path, false, func(resolved string) ([]coding.FileInfo, error) {
		h, err := e.openDir(resolved)
		if err != nil {
			return nil, err
		}
		var entries []remoteDirEntry
		for done := false; !done; {
			reply, err := h.request(context.Background(), "readdir", object{}.with("max", 1000), nil)
			if err != nil {
				h.close()
				return nil, err
			}
			var page readdirReply
			if err := json.Unmarshal(reply.JSON, &page); err != nil {
				h.close()
				return nil, err
			}
			entries = append(entries, page.Entries...)
			done = page.Done
		}
		h.close()
		// Node's readdir fails on any entry it cannot lstat. libuv sorts
		// names by their bytes on POSIX; on Windows it keeps the file
		// system's order, which the daemon reports.
		paths, err := remotePath(e.connection)
		if err != nil {
			return nil, err
		}
		if paths == nodepath.Posix {
			key := func(entry remoteDirEntry) []byte {
				if entry.Raw != nil {
					return entry.Raw
				}
				return []byte(entry.Name)
			}
			sort.SliceStable(entries, func(i, j int) bool { return bytes.Compare(key(entries[i]), key(entries[j])) < 0 })
		}
		infos := []coding.FileInfo{}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return nil, &FileError{Code: FileAborted, Message: "aborted", Path: resolved}
			}
			entryPath := paths.Resolve(resolved, entry.Name)
			if entry.Error != nil {
				return nil, toFileError(newRemoteError(entry.Error), entryPath)
			}
			if entry.Info == nil {
				continue
			}
			if info, failure := toInfo(entryPath, *entry.Info, paths); failure == nil {
				infos = append(infos, info)
			}
		}
		return infos, nil
	})
}

// ListFiles lists the tree below a directory through the daemon; it fails
// with not_supported with a daemon released before listFiles, so callers
// fall back to their own walk.
func (e *RemoteExecutionEnv) ListFiles(ctx context.Context, path string, options *ListFilesOptions) (FileListing, error) {
	resolved, err := e.resolve(ctx, path)
	if err != nil {
		return FileListing{}, toFileError(err, path)
	}
	return listFilesOver(ctx, e.connection, resolved, options)
}

func (e *RemoteExecutionEnv) openDir(resolved string) (*handle, error) {
	reply, err := e.connection.Request(context.Background(), "opendir", object{}.with("path", resolved), nil)
	if err != nil {
		return nil, err
	}
	var opened openReply
	if err := json.Unmarshal(reply.JSON, &opened); err != nil {
		return nil, err
	}
	return &handle{env: e, id: opened.Handle, session: reply.Session, path: resolved}, nil
}

// OpenDirReader opens a directory for paged reading.
func (e *RemoteExecutionEnv) OpenDirReader(ctx context.Context, path string) (DirReader, error) {
	h, err := fileOp(e, ctx, path, false, e.openDir)
	if err != nil {
		return nil, err
	}
	if failure := abortResult(ctx, h.path); failure != nil {
		h.close()
		return nil, failure
	}
	return &remoteDirReader{handle: h}, nil
}

// CanonicalPath implements coding.FileSystem.
func (e *RemoteExecutionEnv) CanonicalPath(ctx context.Context, path string) (string, error) {
	return fileOp(e, ctx, path, false, func(resolved string) (string, error) {
		reply, err := e.connection.Request(context.Background(), "realpath", object{}.with("path", resolved), nil)
		if err != nil {
			return "", err
		}
		var real struct {
			Path string `json:"path"`
		}
		err = json.Unmarshal(reply.JSON, &real)
		return real.Path, err
	})
}

// Exists implements coding.FileSystem.
func (e *RemoteExecutionEnv) Exists(ctx context.Context, path string) (bool, error) {
	_, err := e.Stat(ctx, path)
	if err == nil {
		return true, nil
	}
	var failure *FileError
	if errors.As(err, &failure) && failure.Code == FileNotFound {
		return false, nil
	}
	return false, err
}

// CreateDirOptions are CreateDir's options.
type CreateDirOptions struct {
	// Recursive creates missing parents and accepts an existing directory;
	// nil is true.
	Recursive *bool
}

// CreateDir creates a directory.
func (e *RemoteExecutionEnv) CreateDir(ctx context.Context, path string, options *CreateDirOptions) error {
	recursive := options == nil || options.Recursive == nil || *options.Recursive
	_, err := fileOp(e, ctx, path, false, func(resolved string) (struct{}, error) {
		_, err := e.connection.Request(context.Background(), "mkdir", object{}.with("path", resolved).with("recursive", recursive), nil)
		return struct{}{}, err
	})
	return err
}

// RemoveOptions are Remove's options.
type RemoveOptions struct {
	Recursive bool
	Force     bool
}

// Remove removes a file or directory.
func (e *RemoteExecutionEnv) Remove(ctx context.Context, path string, options *RemoveOptions) error {
	if options == nil {
		options = &RemoveOptions{}
	}
	_, err := fileOp(e, ctx, path, false, func(resolved string) (struct{}, error) {
		fields := object{}.with("path", resolved).with("recursive", options.Recursive).with("force", options.Force)
		_, err := e.connection.Request(context.Background(), "rm", fields, nil)
		return struct{}{}, err
	})
	return err
}

// CreateTempDir creates a directory in the remote temporary directory; an
// empty prefix is "tmp-".
func (e *RemoteExecutionEnv) CreateTempDir(ctx context.Context, prefix string) (string, error) {
	if failure := abortResult(ctx, ""); failure != nil {
		return "", failure
	}
	if prefix == "" {
		prefix = "tmp-"
	}
	// Upstream checks for an abort before it waits for the daemon, not after.
	info, err := e.connection.Info(context.WithoutCancel(ctx))
	if err != nil {
		return "", toFileError(err, "")
	}
	reply, err := e.connection.Request(context.Background(), "mkdtemp", object{}.with("path", flavorOf(info).Join(info.Tmpdir, prefix)), nil)
	if err != nil {
		return "", toFileError(err, "")
	}
	var made struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(reply.JSON, &made); err != nil {
		return "", toFileError(err, "")
	}
	return made.Path, nil
}

// TempFileOptions are CreateTempFile's options.
type TempFileOptions struct {
	Prefix string
	Suffix string
}

// CreateTempFile creates an empty file in a new temporary directory.
func (e *RemoteExecutionEnv) CreateTempFile(ctx context.Context, options *TempFileOptions) (string, error) {
	if options == nil {
		options = &TempFileOptions{}
	}
	dir, err := e.CreateTempDir(ctx, "tmp-")
	if err != nil {
		return "", err
	}
	paths, err := remotePath(e.connection)
	if err != nil {
		return "", toFileError(err, "")
	}
	filePath := paths.Join(dir, options.Prefix+randomUUID()+options.Suffix)
	// Node's writeFile(filePath, ""), which does not create parents.
	fields := object{}.with("path", filePath).with("append", false).with("parents", false)
	if _, err := e.connection.Request(context.Background(), "write", fields, &RequestOptions{Payload: []byte{}}); err != nil {
		return "", toFileError(err, filePath)
	}
	return filePath, nil
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Watch reports changes to files and directories. When it returns, coverage
// is established: a host that watches before it loads cannot miss a change
// made during the load.
func (e *RemoteExecutionEnv) Watch(ctx context.Context, targets []WatchTarget, onChange func(FileChange)) (*RemoteWatcher, error) {
	if failure := abortResult(ctx, ""); failure != nil {
		return nil, failure
	}
	resolved := make([]WatchTarget, len(targets))
	for i, target := range targets {
		path, err := e.resolve(ctx, target.Path)
		if err != nil {
			return nil, toFileError(err, "")
		}
		target.Path = path
		resolved[i] = target
	}
	if failure := abortResult(ctx, ""); failure != nil {
		return nil, failure
	}
	watcher, err := OpenRemoteWatcher(e.connection, resolved, onChange, e.watchOptions)
	if err != nil {
		return nil, toFileError(err, "")
	}
	if failure := abortResult(ctx, ""); failure != nil {
		watcher.Stop()
		return nil, failure
	}
	return watcher, nil
}

// RunOptions are the options of RunShell and RunProgram (Durable's
// ShellExecOptions).
type RunOptions struct {
	// Cwd is the working directory; empty is the environment's.
	Cwd string
	Env map[string]string
	// InheritEnv inherits the daemon's environment and ShellEnv; nil is
	// true.
	InheritEnv *bool
	// Timeout in seconds; 0 is none. It must be finite, positive and at most
	// 2147483.647.
	Timeout float64
	// OnOutput receives every decoded chunk of stdout and stderr as it
	// arrives, in arrival order, with the stream it came from. An error
	// fails the command with callback_error and aborts it.
	OnOutput func(text string, info OutputInfo) error
	// Spill spills the complete output to a temporary file once it exceeds
	// either threshold.
	Spill *SpillOptions
	// Window is the tail of the output the caller keeps, so the daemon may
	// omit output outside it (OutputInfo.Skipped).
	Window *OutputWindow
}

// SpillOptions are the spill thresholds of a command's output.
type SpillOptions struct {
	AfterBytes int64 `json:"afterBytes"`
	// AfterLines counts complete or partial lines.
	AfterLines int64 `json:"afterLines"`
}

// OutputWindow is the tail of the combined output a caller keeps, and how
// often it samples it (Durable's ShellOutputWindow).
type OutputWindow struct {
	// MaxBytes of decoded text kept at the end of the output (UTF-8).
	MaxBytes int64 `json:"maxBytes"`
	// MaxLines kept at the end of the output.
	MaxLines int64 `json:"maxLines"`
	// MinIntervalMS is the minimum pause between the caller's samples.
	MinIntervalMS float64 `json:"minIntervalMs"`
	// BytesPerSecond paces each sample in proportion to its size.
	BytesPerSecond float64 `json:"bytesPerSecond"`
}

// OutputInfo describes one chunk of output (Durable's ShellOutputInfo).
type OutputInfo struct {
	// Stream is "stdout" or "stderr".
	Stream string
	// Skipped is output omitted immediately before this chunk, only with a
	// Window.
	Skipped *OutputSkip
}

// OutputSkip is output an environment omitted, measured on the decoded text
// OnOutput would have received.
type OutputSkip struct {
	Bytes           int64 `json:"bytes"`
	Newlines        int64 `json:"newlines"`
	EndsWithNewline bool  `json:"endsWithNewline"`
}

// RunResult is a finished command.
type RunResult struct {
	ExitCode int
	// SpillPath is the temporary file holding the complete raw output, when
	// the spill thresholds were exceeded.
	SpillPath string
}

// RunShell runs command through the environment's shell (Durable's exec of
// a string). Ending ctx or the timeout kills only this command's processes.
func (e *RemoteExecutionEnv) RunShell(ctx context.Context, command string, options *RunOptions) (RunResult, error) {
	return e.run(ctx, object{}.with("command", command), options)
}

// RunProgram runs argv[0] with the rest as its arguments, without a shell,
// so they reach the program unparsed (Durable's exec of an array).
func (e *RemoteExecutionEnv) RunProgram(ctx context.Context, argv []string, options *RunOptions) (RunResult, error) {
	if argv == nil {
		argv = []string{}
	}
	return e.run(ctx, object{}.with("argv", argv), options)
}

func (e *RemoteExecutionEnv) run(ctx context.Context, command object, options *RunOptions) (RunResult, error) {
	if options == nil {
		options = &RunOptions{}
	}
	if ctx.Err() != nil {
		return RunResult{}, &ExecutionError{Code: ExecAborted, Message: "aborted"}
	}
	timeout := options.Timeout
	if timeout != 0 {
		if math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
			return RunResult{}, &ExecutionError{Code: ExecTimeout, Message: "Invalid timeout: must be a finite number of seconds"}
		}
		if timeout*1000 > maxTimeoutMS {
			return RunResult{}, &ExecutionError{Code: ExecTimeout, Message: "Invalid timeout: maximum is " + jstext.NumberToString(maxTimeoutMS/1000.0) + " seconds"}
		}
	}
	inheritEnv := options.InheritEnv == nil || *options.InheritEnv
	cwd := e.Cwd()
	if options.Cwd != "" {
		resolved, err := e.resolve(ctx, options.Cwd)
		if err != nil {
			if ctx.Err() != nil {
				return RunResult{}, &ExecutionError{Code: ExecAborted, Message: "aborted"}
			}
			return RunResult{}, &ExecutionError{Code: ExecUnknown, Message: err.Error(), Cause: err}
		}
		cwd = resolved
	}
	if ctx.Err() != nil {
		return RunResult{}, &ExecutionError{Code: ExecAborted, Message: "aborted"}
	}
	env := map[string]string{}
	if inheritEnv {
		for key, value := range e.shellEnv {
			env[key] = value
		}
	}
	for key, value := range options.Env {
		env[key] = value
	}
	fields := append(command, object{}.with("cwd", cwd).with("env", env).with("inheritEnv", inheritEnv)...)
	if e.shellPath != "" {
		fields = fields.with("shellPath", e.shellPath)
	}
	if timeout != 0 {
		fields = fields.with("timeoutMs", timeout*1000)
	}
	if options.Spill != nil {
		fields = fields.with("spill", options.Spill)
	}
	if options.Window != nil {
		fields = fields.with("window", options.Window)
	}

	var callbackErr *ExecutionError
	settled := false
	var running uint32
	started := false
	// Aborting ctx or failing the callback cancels the request; it still
	// settles with the daemon's answer.
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reply, err := e.connection.Request(requestCtx, "exec", fields, &RequestOptions{
		OnStart: func(id uint32, session int) {
			running, started = id, true
			e.mu.Lock()
			e.running[id] = session
			e.mu.Unlock()
		},
		OnEvent: func(event json.RawMessage, payload []byte) {
			if settled || callbackErr != nil {
				return
			}
			var output struct {
				Kind    string      `json:"kind"`
				Stream  string      `json:"stream"`
				Skipped *OutputSkip `json:"skipped"`
			}
			if json.Unmarshal(event, &output) != nil || output.Kind != "output" {
				return
			}
			text := jstext.DecodeUTF8(payload)
			if text == "" || options.OnOutput == nil {
				return
			}
			info := OutputInfo{Stream: "stdout", Skipped: output.Skipped}
			if output.Stream == "stderr" {
				info.Stream = "stderr"
			}
			if err := options.OnOutput(text, info); err != nil {
				callbackErr = &ExecutionError{Code: ExecCallbackError, Message: err.Error(), Cause: err}
				cancel()
			}
		},
	})
	settled = true
	if started {
		e.mu.Lock()
		delete(e.running, running)
		e.mu.Unlock()
	}
	if callbackErr != nil {
		return RunResult{}, callbackErr
	}
	if err != nil {
		var remote *RemoteError
		if !errors.As(err, &remote) {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// The context ended while the connection was starting.
				return RunResult{}, &ExecutionError{Code: ExecAborted, Message: "aborted", Cause: err}
			}
			return RunResult{}, &ExecutionError{Code: ExecUnknown, Message: err.Error(), Cause: err}
		}
		failure := &ExecutionError{Code: ExecUnknown, Message: remote.Message, timeout: timeout}
		switch remote.Code {
		case "timeout":
			failure.Code, failure.Message = ExecTimeout, "timeout:"+jstext.NumberToString(timeout)
		case "aborted":
			failure.Code, failure.Message = ExecAborted, "aborted"
		case "shell_unavailable", "spawn_error":
			failure.Code = ExecutionErrorCode(remote.Code)
		}
		if spillPath, ok := remote.Fields["spillPath"].(string); ok {
			failure.SpillPath = spillPath
		}
		return RunResult{}, failure
	}
	var result struct {
		ExitCode  int     `json:"exitCode"`
		SpillPath *string `json:"spillPath"`
	}
	if err := json.Unmarshal(reply.JSON, &result); err != nil {
		return RunResult{}, &ExecutionError{Code: ExecUnknown, Message: err.Error(), Cause: err}
	}
	run := RunResult{ExitCode: result.ExitCode}
	if result.SpillPath != nil {
		run.SpillPath = *result.SpillPath
	}
	return run, nil
}

// Exec implements coding.Shell: the command runs through the environment's
// shell, its output streams to OnStdout and OnStderr and is collected per
// stream, and a non-zero exit is a result. Failures are *ExecutionError.
func (e *RemoteExecutionEnv) Exec(ctx context.Context, command string, options *coding.ShellExecOptions) (coding.ShellResult, error) {
	if options == nil {
		options = &coding.ShellExecOptions{}
	}
	var stdout, stderr strings.Builder
	run := &RunOptions{
		Cwd:        options.Cwd,
		Env:        options.Env,
		InheritEnv: options.InheritEnv,
		OnOutput: func(text string, info OutputInfo) error {
			if info.Stream == "stderr" {
				stderr.WriteString(text)
				if options.OnStderr != nil {
					options.OnStderr(text)
				}
			} else {
				stdout.WriteString(text)
				if options.OnStdout != nil {
					options.OnStdout(text)
				}
			}
			return nil
		},
	}
	if options.TimeoutSeconds > 0 {
		run.Timeout = float64(options.TimeoutSeconds)
	}
	result, err := e.RunShell(ctx, command, run)
	shell := coding.ShellResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: result.ExitCode}
	return shell, err
}

// Cleanup implements coding.Shell: it kills every command this environment
// still runs, without aborting them, so they settle with their killed
// status, as NodeExecutionEnv does.
func (e *RemoteExecutionEnv) Cleanup() error {
	e.mu.Lock()
	running := e.running
	e.running = map[uint32]int{}
	e.mu.Unlock()
	for id, session := range running {
		e.connection.Kill(id, session)
	}
	return nil
}
