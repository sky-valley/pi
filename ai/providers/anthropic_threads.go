package providers

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/sky-valley/pi/ai"
)

// Message threads (pi anthropic-messages.ts, upstream a758f3ae4): the server
// keeps the conversation of a thread, so a request can continue from the
// previous reply and send only the new messages. The full request is still
// built; prepare compares it with the session's previous request and sends
// only the new part when nothing earlier changed.

const (
	messageThreadsBeta = "message-threads-2026-08-12"
	// maxAnthropicThreadSessions bounds the per-session state (pi
	// MAX_THREAD_STATE_ENTRIES). State is normally freed by
	// ai.CleanupSessionResources when a session ends; the cap bounds memory for
	// callers that never call it, for example a server passing a new session id
	// per conversation. Dropping the least recently updated session is cheap:
	// its thread has most likely expired on the server already, and otherwise
	// its next request just starts a new thread.
	maxAnthropicThreadSessions = 10_000
)

// anthropicNewThreadErrors are the rejections of a `continue` that a new
// thread fixes: the thread expired, raced, or a fixed setting changed (pi
// NEW_THREAD_ERRORS).
var anthropicNewThreadErrors = map[string]bool{
	"thread_not_found":            true,
	"thread_already_continued":    true,
	"thread_continue_in_flight":   true,
	"thread_fingerprint_mismatch": true,
}

// anthropicThreadParam is the request's `thread` field. A struct rather than
// a map so its keys go out in pi's literal order: type, then
// previous_message_id.
type anthropicThreadParam struct {
	Type              string `json:"type"`
	PreviousMessageID string `json:"previous_message_id,omitempty"`
}

// anthropicThreadState is where a session's next request can continue, or off
// once the endpoint rejected threads for it.
type anthropicThreadState struct {
	off bool
	// prefixCount is the number of messages in the last full request as sent
	// plus the converted reply.
	prefixCount int
	// prefixHash is hashAnthropicThreadPrefix of that request's settings and
	// those messages.
	prefixHash [sha256.Size]byte
	// replyID is the id of the reply to continue from.
	replyID string
}

var anthropicThreadOff = &anthropicThreadState{off: true}

// anthropicThreadStore holds the thread state per session id, evicting the
// least recently updated session past max.
type anthropicThreadStore struct {
	mu    sync.Mutex
	max   int
	order *list.List // of *anthropicThreadSlot, least recently updated first
	slots map[string]*list.Element
}

type anthropicThreadSlot struct {
	sessionID string
	state     *anthropicThreadState
}

func newAnthropicThreadStore(max int) *anthropicThreadStore {
	return &anthropicThreadStore{max: max, order: list.New(), slots: map[string]*list.Element{}}
}

// anthropicThreads is the process-wide store, as pi's module-level Map is.
var anthropicThreads = newAnthropicThreadStore(maxAnthropicThreadSessions)

func init() {
	ai.RegisterSessionResourceCleanup(func(sessionID string) error {
		anthropicThreads.cleanup(sessionID)
		return nil
	})
}

func (s *anthropicThreadStore) get(sessionID string) *anthropicThreadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.slots[sessionID]; ok {
		return e.Value.(*anthropicThreadSlot).state
	}
	return nil
}

// set records a session's state as its most recent update.
func (s *anthropicThreadStore) set(sessionID string, state *anthropicThreadState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.slots[sessionID]; ok {
		e.Value.(*anthropicThreadSlot).state = state
		s.order.MoveToBack(e)
	} else {
		s.slots[sessionID] = s.order.PushBack(&anthropicThreadSlot{sessionID: sessionID, state: state})
	}
	if s.order.Len() > s.max {
		oldest := s.order.Remove(s.order.Front()).(*anthropicThreadSlot)
		delete(s.slots, oldest.sessionID)
	}
}

// take removes the session's state if it is still state, and reports whether
// it did. Each reply can be continued once; taking it also keeps a concurrent
// request of the same session from continuing it too.
func (s *anthropicThreadStore) take(sessionID string, state *anthropicThreadState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.slots[sessionID]
	if !ok || e.Value.(*anthropicThreadSlot).state != state {
		return false
	}
	s.order.Remove(e)
	delete(s.slots, sessionID)
	return true
}

// cleanup forgets one session, or every session when sessionID is empty.
func (s *anthropicThreadStore) cleanup(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID == "" {
		s.order.Init()
		clear(s.slots)
		return
	}
	if e, ok := s.slots[sessionID]; ok {
		s.order.Remove(e)
		delete(s.slots, sessionID)
	}
}

// anthropicThreadMessages splits the request's messages into their JSON
// elements, which can be sliced, hashed and sent again as they are. It
// reports false when they are not a list, which only an onPayload hook can
// cause; such a request is sent as a new thread and never recorded.
func anthropicThreadMessages(params map[string]any) ([]json.RawMessage, bool) {
	data, err := json.Marshal(params["messages"])
	if err != nil {
		return nil, false
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(data, &messages); err != nil || messages == nil {
		return nil, false
	}
	return messages, true
}

// hashAnthropicThreadPrefix hashes everything a `continue` builds on: the
// settings that must stay the same within a thread and the messages the
// server already holds (pi hashThreadPrefix).
//
//   - Settings, as sent: the endpoint (a thread exists on one endpoint only)
//     and the fixed request fields. A `continue` omits `system` and `tools` and
//     the server keeps the thread's own, so only this check can notice a change
//     to them, cache markers included. The server rejects a change to the
//     other fields with thread_fingerprint_mismatch; checking them here saves
//     that rejected request.
//   - Messages, normalized so messages that mean the same to the API hash the
//     same: cache markers are dropped (pi moves its marker to the newest
//     message, and extensions may add more), and string content counts as one
//     text block (the newest user message's string goes out as a block to
//     carry that marker, and as a string once it is older).
//
// The digest is internal: only whether two requests hash the same matters. pi
// uses its shortHash; SHA-256 makes a false match, which would send a
// `continue` the server does not hold the start of, out of reach.
func hashAnthropicThreadPrefix(model *ai.Model, params map[string]any, messages []json.RawMessage) ([sha256.Size]byte, error) {
	settings, err := json.Marshal([]any{
		model.Provider, model.BaseURL, params["model"], params["system"], params["tools"],
		params["thinking"], params["output_config"], params["tool_choice"], params["betas"], params["fallbacks"],
	})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	conversation := make([]any, len(messages))
	for i, raw := range messages {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&conversation[i]); err != nil {
			return [sha256.Size]byte{}, err
		}
		conversation[i] = normalizeAnthropicThreadValue("", conversation[i])
	}
	normalized, err := json.Marshal(conversation)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	h := sha256.New()
	h.Write(settings)
	h.Write(normalized)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum, nil
}

// normalizeAnthropicThreadValue is pi's hashThreadPrefix replacer, applied at
// every depth: it drops `cache_control` members and turns a string `content`
// into one text block.
func normalizeAnthropicThreadValue(key string, value any) any {
	switch v := value.(type) {
	case string:
		if key == "content" {
			return []any{map[string]any{"type": "text", "text": v}}
		}
	case map[string]any:
		delete(v, "cache_control")
		for k, member := range v {
			v[k] = normalizeAnthropicThreadValue(k, member)
		}
	case []any:
		for i, element := range v {
			v[i] = normalizeAnthropicThreadValue("", element)
		}
	}
	return value
}

// anthropicThreadRequest is a request that carries `thread`: its session and
// the body to send.
type anthropicThreadRequest struct {
	sessionID string
	body      map[string]any
}

func newAnthropicThread(sessionID string, params map[string]any) *anthropicThreadRequest {
	body := maps.Clone(params)
	body["thread"] = anthropicThreadParam{Type: "create"}
	return &anthropicThreadRequest{sessionID: sessionID, body: body}
}

// prepare is pi's prepareThreadRequest: a `continue` with only the new
// messages when the session's previous request and reply are an unchanged
// prefix of params, a new thread otherwise, and nil when threads are off for
// the session.
func (s *anthropicThreadStore) prepare(model *ai.Model, sessionID string, params map[string]any) *anthropicThreadRequest {
	state := s.get(sessionID)
	if state == anthropicThreadOff {
		return nil
	}
	if state != nil {
		if messages, ok := anthropicThreadMessages(params); ok && len(messages) > state.prefixCount {
			hash, err := hashAnthropicThreadPrefix(model, params, messages[:state.prefixCount])
			if err == nil && hash == state.prefixHash && s.take(sessionID, state) {
				body := maps.Clone(params)
				delete(body, "system")
				delete(body, "tools")
				body["messages"] = messages[state.prefixCount:]
				body["thread"] = anthropicThreadParam{Type: "continue", PreviousMessageID: state.replyID}
				return &anthropicThreadRequest{sessionID: sessionID, body: body}
			}
		}
	}
	return newAnthropicThread(sessionID, params)
}

// record is pi's recordThreadReply: it notes where the session's next request
// can continue — after output, the clean reply to thread, whose full request
// was params.
func (s *anthropicThreadStore) record(model *ai.Model, thread *anthropicThreadRequest, params map[string]any, output *ai.AssistantMessage, oauth, allowEmptySignature bool) {
	// The response model is set only when another model answered, such as a
	// server-side fallback; the server does not store those replies, so they
	// cannot be continued.
	if output.ResponseID == "" || output.ResponseModel != "" {
		return
	}
	messages, ok := anthropicThreadMessages(params)
	if !ok {
		return
	}
	// The reply is converted as the next request will convert it, keeping only
	// the assistant message (transformMessages adds placeholder results for
	// trailing tool calls).
	converted, _, err := convertAnthropicMessages(
		transformMessages([]ai.Message{output}, model, normalizeToolCallID), oauth, nil, allowEmptySignature, "", nil)
	if err != nil {
		return
	}
	prefix := messages
	for _, message := range converted {
		if message["role"] != "assistant" {
			continue
		}
		raw, err := json.Marshal(message)
		if err != nil {
			return
		}
		prefix = append(prefix, raw)
	}
	hash, err := hashAnthropicThreadPrefix(model, params, prefix)
	if err != nil {
		return
	}
	s.set(thread.sessionID, &anthropicThreadState{prefixCount: len(prefix), prefixHash: hash, replyID: output.ResponseID})
}

// anthropicAPIError is a request Anthropic rejected with a non-2xx status:
// the SDK's APIError, with its message as pi surfaces it.
type anthropicAPIError struct {
	status  int
	body    []byte
	message string
}

func (e *anthropicAPIError) Error() string { return e.message }

// anthropicThreadErrorCode is the `error_code` Anthropic puts in
// `error.details` of thread rejections (pi getThreadErrorCode). ok is false
// when it is absent or not a string.
func anthropicThreadErrorCode(body []byte) (code string, ok bool) {
	var parsed struct {
		Error struct {
			Details struct {
				ErrorCode json.RawMessage `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		// A body whose `error` or `details` is not an object still parses in
		// pi; it just has no code.
		return "", false
	}
	return rawString(parsed.Error.Details.ErrorCode)
}

// send is pi's sendThreadRequest: it sends thread, and resends
// a rejection about the thread (never a started stream) once in full — as a
// new thread when the old one cannot be continued, or without `thread` when
// the endpoint cannot use threads, which also turns threads off for the
// session. Other errors are returned as they are. It returns the response and
// the thread request that produced it, nil when none did.
func (s *anthropicThreadStore) send(ctx context.Context, thread *anthropicThreadRequest, params map[string]any,
	send func(body map[string]any) (*http.Response, error), output *ai.AssistantMessage,
) (*http.Response, *anthropicThreadRequest, error) {
	resp, err := send(thread.body)
	if err == nil {
		return resp, thread, nil
	}
	var rejected *anthropicAPIError
	if (ctx != nil && ctx.Err() != nil) || !errors.As(err, &rejected) {
		return nil, nil, err
	}
	code, hasCode := anthropicThreadErrorCode(rejected.body)
	startNewThread := hasCode && anthropicNewThreadErrors[code]
	// The endpoint cannot use threads: it cannot thread this request, or it
	// does not know the `thread` field and rejects it without a code ("thread:
	// Extra inputs are not permitted"), e.g. with an anthropic-beta header that
	// lacks the threads beta.
	threadsUnsupported := (hasCode && code == "thread_unsupported_request") ||
		(!hasCode && rejected.status == http.StatusBadRequest && strings.Contains(rejected.message, "thread"))
	// Any other error (prompt too long, unknown model, strict tool limits, ...)
	// is not about the thread, and resending would only fail again.
	if !startNewThread && !threadsUnsupported {
		return nil, nil, err
	}
	reason := code
	if !hasCode {
		reason = strconv.Itoa(rejected.status)
	}
	// Recorded before the resend, so a failed resend keeps it.
	output.Diagnostics = append(output.Diagnostics, ai.Diagnostic{
		Type:      "anthropic_thread_fallback",
		Timestamp: nowMillis(),
		Error:     &ai.DiagnosticErrorInfo{Message: rejected.message},
		Details:   ai.OrderedObject{{Key: "reason", Value: reason}},
	})
	if startNewThread {
		created := newAnthropicThread(thread.sessionID, params)
		resp, err := send(created.body)
		if err != nil {
			return nil, nil, err
		}
		return resp, created, nil
	}
	// With the threads beta, a request without `thread` is a normal request.
	if resp, err = send(params); err != nil {
		return nil, nil, err
	}
	s.set(thread.sessionID, anthropicThreadOff)
	return resp, nil, nil
}
