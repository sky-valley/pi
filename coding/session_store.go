package coding

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// CurrentSessionVersion matches pi's session file format version.
const CurrentSessionVersion = 3

// DefaultSessionDir returns the per-cwd session directory under the agent dir,
// using pi's safe-path encoding (--<cwd with separators as dashes>--).
func DefaultSessionDir(cwd string) string {
	resolved, _ := filepath.Abs(cwd)
	return filepath.Join(AgentDir(), "sessions", "--"+encodeCwdSafePath(resolved)+"--")
}

// encodeCwdSafePath mirrors pi's safe-path encoding (session-manager.ts:441):
// strip exactly ONE leading separator (replace(/^[/\\]/, "")), then replace
// every '/'/'\'/':' with '-'.
func encodeCwdSafePath(resolved string) string {
	if len(resolved) > 0 && (resolved[0] == '/' || resolved[0] == '\\') {
		resolved = resolved[1:]
	}
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(resolved)
}

func genID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// uuidv7 mirrors pi's uuidv7 (packages/agent/.../uuid.ts): a time-ordered v7
// UUID (version nibble 7, RFC-4122 variant) in canonical 8-4-4-4-12 form.
func uuidv7() string {
	var random [16]byte
	_, _ = rand.Read(random[:])
	ts := time.Now().UnixMilli()

	uuidMu.Lock()
	if ts > uuidLastTimestamp {
		uuidSequence = uint32(random[6])<<24 | uint32(random[7])<<16 | uint32(random[8])<<8 | uint32(random[9])
		uuidLastTimestamp = ts
	} else {
		uuidSequence++
		if uuidSequence == 0 {
			uuidLastTimestamp++
		}
	}
	seq := uuidSequence
	last := uuidLastTimestamp
	uuidMu.Unlock()

	var b [16]byte
	b[0] = byte(last >> 40)
	b[1] = byte(last >> 32)
	b[2] = byte(last >> 24)
	b[3] = byte(last >> 16)
	b[4] = byte(last >> 8)
	b[5] = byte(last)
	b[6] = 0x70 | byte((seq>>28)&0x0f)
	b[7] = byte((seq >> 20) & 0xff)
	b[8] = 0x80 | byte((seq>>14)&0x3f)
	b[9] = byte((seq >> 6) & 0xff)
	b[10] = byte((seq&0x3f)<<2) | (random[10] & 0x03)
	b[11] = random[11]
	b[12] = random[12]
	b[13] = random[13]
	b[14] = random[14]
	b[15] = random[15]

	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

var (
	uuidMu            sync.Mutex
	uuidLastTimestamp int64
	uuidSequence      uint32
)

// genEntryID returns an 8-hex-char entry id not already present in used (pi's
// generateId: randomUUID().slice(0,8), collision-checked, full-uuid fallback).
// pi slices a v4 UUID (fully random first 8 chars); genID() yields the same
// 8-random-hex-char shape, so we reuse it.
func genEntryID(used map[string]bool) string {
	for i := 0; i < 100; i++ {
		id := genID()
		if !used[id] {
			return id
		}
	}
	return uuidv7()
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// SessionInfo summarizes a stored session file.
type SessionInfo struct {
	Path      string
	ID        string
	Cwd       string
	Timestamp string
	Messages  int
}

// SessionRecorder appends an agent transcript to a JSONL session file, matching
// pi's append-only format (header + linear message/model/thinking entries).
//
// Writes are withheld until the first assistant message is recorded (pi
// _persist): a started-but-unused session leaves no file on disk. Pending
// entries are buffered and flushed atomically on the first assistant message.
type SessionRecorder struct {
	mu       sync.Mutex
	path     string
	id       string
	lastID   string
	file     *os.File
	byID     map[string]bool
	pending  []map[string]any
	flushed  bool
	hasAsst  bool
	createFn func() (*os.File, error)
}

// StartSession creates a new session for cwd and buffers the header plus an
// initial model entry. The session file is created lazily on the first recorded
// assistant message.
func StartSession(cwd string, model *ai.Model) (*SessionRecorder, error) {
	dir := DefaultSessionDir(cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	id := uuidv7()
	ts := isoNow()
	fileTS := strings.NewReplacer(":", "-", ".", "-").Replace(ts)
	path := filepath.Join(dir, fileTS+"_"+id+".jsonl")
	resolved, _ := filepath.Abs(cwd)
	r := &SessionRecorder{
		path: path,
		id:   id,
		byID: map[string]bool{},
		createFn: func() (*os.File, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		},
	}
	r.buffer(map[string]any{
		"type": "session", "version": CurrentSessionVersion, "id": id, "timestamp": ts, "cwd": resolved,
	})
	if model != nil {
		r.appendEntry(map[string]any{
			"type": "model_change", "provider": model.Provider, "modelId": model.ID,
		})
	}
	return r, nil
}

// Path returns the session file path.
func (r *SessionRecorder) Path() string { return r.path }

// ID returns the session id.
func (r *SessionRecorder) ID() string { return r.id }

func writeLine(f *os.File, entry map[string]any) {
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
}

// buffer records an entry and persists it per pi's _persist policy: writes are
// withheld until the buffered entries contain an assistant message; once that
// happens the whole buffer is flushed atomically and later entries append.
func (r *SessionRecorder) buffer(entry map[string]any) {
	r.pending = append(r.pending, entry)
	if t, _ := entry["type"].(string); t == "message" {
		if isAssistantEntry(entry) {
			r.hasAsst = true
		}
	}
	r.persist()
}

func (r *SessionRecorder) persist() {
	if !r.hasAsst {
		// No assistant message yet: nothing reaches disk (the file is not even
		// created). Subsequent flush will write every pending entry.
		r.flushed = false
		return
	}
	if !r.flushed {
		f, err := r.createFn()
		if err != nil {
			return
		}
		r.file = f
		for _, e := range r.pending {
			writeLine(f, e)
		}
		r.flushed = true
		return
	}
	// Already flushed: append just the most recent entry.
	if r.file != nil && len(r.pending) > 0 {
		writeLine(r.file, r.pending[len(r.pending)-1])
	}
}

func isAssistantEntry(entry map[string]any) bool {
	raw, ok := entry["message"].(json.RawMessage)
	if !ok {
		return false
	}
	var head struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return false
	}
	return head.Role == "assistant"
}

func (r *SessionRecorder) appendEntry(entry map[string]any) string {
	id := genEntryID(r.byID)
	r.byID[id] = true
	entry["id"] = id
	if r.lastID == "" {
		entry["parentId"] = nil
	} else {
		entry["parentId"] = r.lastID
	}
	entry["timestamp"] = isoNow()
	r.lastID = id
	r.buffer(entry)
	return id
}

// LastEntryID returns the id of the most recently written entry (a branch point).
func (r *SessionRecorder) LastEntryID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastID
}

// ForkFrom sets the parent for subsequent entries to entryID, so new entries
// branch off an earlier point in the tree instead of extending the latest leaf.
func (r *SessionRecorder) ForkFrom(entryID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastID = entryID
}

// RecordMessage appends a message entry for an agent transcript message and
// returns its entry id (usable as a fork point).
func (r *SessionRecorder) RecordMessage(m agent.AgentMessage) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return r.appendEntry(map[string]any{"type": "message", "message": json.RawMessage(raw)})
}

// RecordThinkingLevel appends a thinking-level-change entry.
func (r *SessionRecorder) RecordThinkingLevel(level string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendEntry(map[string]any{"type": "thinking_level_change", "thinkingLevel": level})
}

// RecordModelChange appends a model-change entry.
func (r *SessionRecorder) RecordModelChange(provider, modelID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendEntry(map[string]any{"type": "model_change", "provider": provider, "modelId": modelID})
}

// Close closes the session file.
func (r *SessionRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// ListSessions returns stored sessions for cwd, newest first.
func ListSessions(cwd string) []SessionInfo {
	dir := DefaultSessionDir(cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var infos []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if info, ok := readSessionInfo(filepath.Join(dir, e.Name())); ok {
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Timestamp > infos[j].Timestamp })
	return infos
}

func readSessionInfo(path string) (SessionInfo, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionInfo{}, false
	}
	info := SessionInfo{Path: path}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var head struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(line), &head) != nil {
			continue
		}
		switch head.Type {
		case "session":
			info.ID = head.ID
			info.Cwd = head.Cwd
			info.Timestamp = head.Timestamp
		case "message":
			info.Messages++
		}
	}
	return info, info.ID != ""
}

// LoadSessionMessages reconstructs the LLM message transcript from a session
// file for resume. It routes through SessionTree.BuildContext so compacted,
// branched, and custom-message sessions resume identically to pi (emitting the
// compaction summary in place of the pre-compaction turns) rather than naively
// concatenating every message entry.
func LoadSessionMessages(path string) ([]agent.AgentMessage, error) {
	tree, err := LoadSessionTree(path)
	if err != nil {
		return nil, err
	}
	return tree.BuildContext().Messages, nil
}

// LatestSession returns the most recent stored session for cwd, if any.
func LatestSession(cwd string) (SessionInfo, bool) {
	infos := ListSessions(cwd)
	if len(infos) == 0 {
		return SessionInfo{}, false
	}
	return infos[0], true
}
