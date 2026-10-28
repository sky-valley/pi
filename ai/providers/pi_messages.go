package providers

// pi-messages API implementation.
//
// Streams pi's own message protocol directly to a backend: the request is a
// single POST of `{ model, context, options }` to `<baseUrl>/messages`, the
// response is an SSE stream of serialized assistant-message events plus a
// terminal `done`/`error` event. This is the wire protocol spoken by the Radius
// gateway, but any backend implementing it can be used, e.g. via a models.json
// custom provider with `"api": "pi-messages"`.
//
// Port of packages/ai/src/api/pi-messages.ts.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// PiMessagesOptions are provider-native options for the pi-messages stream.
type PiMessagesOptions struct {
	ai.StreamOptions
	// Reasoning is the unified thinking level forwarded to the backend.
	Reasoning ai.ThinkingLevel
	// ToolChoice is "auto"|"none"|"required" or a
	// {type:"function",function:{name}} object (pi's union). Any non-nil value is
	// serialized verbatim into the request options; nil omits the field.
	ToolChoice any
	// Debug asks the backend for debug metadata (e.g. routing response headers).
	Debug bool
}

// piMessagesEvent is a serialized assistant-message event as sent by a
// pi-messages backend, read the way pi's createEventConverter reads the
// event's properties (piMessagesEventOf). It is the flattened union of every
// event variant; the converter reads only the fields its `type` implies. Port
// of PiMessagesEvent.
type piMessagesEvent struct {
	// Type is the event's type when it is a string; typ is the raw value,
	// nil when absent.
	Type string
	typ  json.RawMessage
	// primitive is the frame's value when it is neither an object nor an
	// array: a truthy number, string or true, which has no properties.
	primitive json.RawMessage
	// contentIndex is the raw value the converter checks and looks blocks up
	// by (see piMessagesConverter.blockIndex); nil when absent.
	contentIndex json.RawMessage
	// Delta is the delta the pushed event carries: the event's own when it is
	// a string. delta is the raw value, whose String() pi's `+=` appends (5 as
	// "5", an absent delta as "undefined").
	Delta string
	delta json.RawMessage
	// Content and Signature are what text_end and thinking_end assign, whether
	// or not the event carries them: absent, they clear the block's.
	Content   string
	Signature string
	Redacted  bool
	ID        string
	ToolName  string
	// toolCall is the raw toolCall toolcall_end assigns onto its block (see
	// assignPiMessagesToolCall); nil when absent.
	toolCall json.RawMessage
	Reason   ai.StopReason
	Usage    *ai.Usage
	// ResponseID is the backend's response id.
	ResponseID string
	// ResponseModel is the physical model the backend ran, e.g. behind a virtual
	// model such as Radius's "cheap" (upstream 14c811841). A pointer for the same
	// reason as ProviderThinkingLevel: pi copies it only when present.
	ResponseModel *string
	// ProviderThinkingLevel is the effort the upstream provider actually ran the
	// turn at, forwarded so the next request can replay it. A pointer because pi
	// only copies it when the field is present: an absent one leaves the
	// message's own level standing rather than blanking it.
	ProviderThinkingLevel *string
	ErrorMessage          string
	// Rewrite is the details of the pi_messages_rewrite diagnostic, nil when
	// the event carries no rewrite (see piMessagesRewriteDetails).
	Rewrite ai.OrderedObject
}

// piMessagesEventOf reads an object frame's members as pi's converter reads
// the event's properties: each by its exact name and on its own, so a member
// of an unexpected JSON type never drops the frame — pi converts the event
// whatever its members hold. A member pi stores as-is into a field Go types as
// string, bool or int reads as absent unless it is that type (a count accepts
// any finite number: the 10.0 a non-JavaScript backend writes is 10).
func piMessagesEventOf(o rawObject) piMessagesEvent {
	ev := piMessagesEvent{typ: o["type"], contentIndex: o["contentIndex"], delta: o["delta"], toolCall: o["toolCall"]}
	ev.Type, _ = rawString(o["type"])
	ev.Delta, _ = rawString(o["delta"])
	ev.Content, _ = rawString(o["content"])
	ev.Signature, _ = rawString(o["contentSignature"])
	ev.Redacted = jsonValueKind(o["redacted"]) == 't'
	ev.ID, _ = rawString(o["id"])
	ev.ToolName, _ = rawString(o["toolName"])
	if reason, ok := rawString(o["reason"]); ok {
		ev.Reason = ai.StopReason(reason)
	}
	ev.Usage = piMessagesUsage(o["usage"])
	ev.ResponseID, _ = rawString(o["responseId"])
	if model, ok := rawString(o["responseModel"]); ok {
		ev.ResponseModel = &model
	}
	if level, ok := rawString(o["providerThinkingLevel"]); ok {
		ev.ProviderThinkingLevel = &level
	}
	ev.ErrorMessage, _ = rawString(o["errorMessage"])
	ev.Rewrite = piMessagesRewriteDetails(o["rewrite"])
	return ev
}

// piMessagesUsage reads the usage a terminal event carries, which pi assigns
// to the message whole: its counts as finite numbers (rawCount) and its cost
// as finite numbers; nil when the event has no usage object.
func piMessagesUsage(raw json.RawMessage) *ai.Usage {
	o := rawOptional(raw)
	if o == nil {
		return nil
	}
	var u ai.Usage
	for key, dst := range map[string]*int{
		"input": &u.Input, "output": &u.Output, "cacheRead": &u.CacheRead, "cacheWrite": &u.CacheWrite,
		"cacheWrite1h": &u.CacheWrite1h, "reasoning": &u.Reasoning, "totalTokens": &u.TotalTokens,
	} {
		*dst, _ = rawCount(o[key])
	}
	cost := rawOptional(o["cost"])
	for key, dst := range map[string]*float64{
		"input": &u.Cost.Input, "output": &u.Cost.Output, "cacheRead": &u.Cost.CacheRead,
		"cacheWrite": &u.Cost.CacheWrite, "total": &u.Cost.Total,
	} {
		if f, ok := rawNumber(cost[key]); ok && !math.IsInf(f, 0) {
			*dst = f
		}
	}
	// pi assigns the backend's usage whole, so a backend running pi-ai passes
	// its totalFromProvider through (upstream 7c22a93d0).
	switch jsonValueKind(cost["totalFromProvider"]) {
	case 't':
		u.Cost.TotalFromProvider = ai.OptBoolTrue
	case 'f':
		u.Cost.TotalFromProvider = ai.OptBoolFalse
	}
	return &u
}

// piMessagesRewriteDetails is pi appendRewriteDiagnostic's `if (!rewrite)`
// and `{ ...rewrite }`: nil for a falsy rewrite (no diagnostic), else what the
// spread copies, in the order it copies it — an object's members, as
// JSON.parse lists them (ai.DecodeOrderedValue: numbers are JS numbers, a
// nested object keeps its order); an array's elements under their indices;
// a string's UTF-16 code units under theirs (a surrogate half, which a Go
// string cannot hold, as U+FFFD); nothing from a number or true, which spread
// into an empty object.
func piMessagesRewriteDetails(raw json.RawMessage) ai.OrderedObject {
	if !rawTruthy(raw) {
		return nil
	}
	details := ai.OrderedObject{}
	value, err := ai.DecodeOrderedValue(raw)
	if err != nil {
		return details // unreachable: raw is a member of a decoded document
	}
	switch v := value.(type) {
	case ai.OrderedObject:
		return v
	case []any:
		for i, e := range v {
			details = append(details, ai.OrderedField{Key: strconv.Itoa(i), Value: e})
		}
	case string:
		for i, unit := range utf16.Encode([]rune(v)) {
			details = append(details, ai.OrderedField{Key: strconv.Itoa(i), Value: string(rune(unit))})
		}
	}
	return details
}

// piMessagesResponseError is a non-2xx HTTP failure carrying redacted diagnostic
// details. Port of PiMessagesResponseError.
type piMessagesResponseError struct {
	message string
	// code is the error body's error.code when it is a string, else nil (pi's
	// undefined).
	code              any
	diagnosticDetails ai.OrderedObject
}

func (e *piMessagesResponseError) Error() string { return e.message }

// parsePiMessagesErrorBody is pi's JSON.parse of an error body and its guard:
// the body's `error` member when the body is an object and that member is an
// object too (not null, not an array); nil otherwise, a body that is not JSON
// included. The object is as JSON.parse builds it (ai.DecodeOrderedValue), so
// the diagnostic that carries it keeps its key order and numbers.
func parsePiMessagesErrorBody(body string) ai.OrderedObject {
	parsed, err := ai.DecodeOrderedValue([]byte(body))
	if err != nil {
		return nil
	}
	top, _ := parsed.(ai.OrderedObject)
	errObj, _ := top.Get("error")
	obj, _ := errObj.(ai.OrderedObject)
	return obj
}

// truncateDiagnosticString caps a raw body at 8192 UTF-16 code units, appending
// "…" when truncated (pi's value.length / value.slice(0, 8192)). The cut lands
// on a rune boundary; a surrogate pair straddling exactly 8192 is kept whole
// rather than split into a lone surrogate (a benign adaptation of JS slicing).
func truncateDiagnosticString(value string) string {
	const maxLength = 8192
	if jstext.UTF16Length(value) <= maxLength {
		return value
	}
	units := 0
	for i, r := range value {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > maxLength {
			return value[:i] + "…"
		}
		units += w
	}
	return value + "…"
}

// formatPiMessagesResponseError builds "<status> <statusText>: <message or
// body><(code)>". Port of formatPiMessagesResponseError.
func formatPiMessagesResponseError(status int, statusText, body string, errObj ai.OrderedObject) string {
	suffix := body
	if msg, _ := errObj.Get("message"); msg != nil {
		if s, ok := msg.(string); ok {
			suffix = s
		}
	}
	codeSuffix := ""
	if code, _ := errObj.Get("code"); code != nil {
		if s, ok := code.(string); ok && s != "" {
			codeSuffix = fmt.Sprintf(" (%s)", s)
		}
	}
	return fmt.Sprintf("%d %s: %s%s", status, statusText, suffix, codeSuffix)
}

// responseStatusText is fetch's Response.statusText for a response net/http
// read: the reason phrase of the status line as the server sent it — what
// follows the code's one space, spaces kept, nothing when there is none —
// decoded as UTF-8, as undici decodes it. net/http keeps that line in
// resp.Status. (HTTP/2 has no reason phrase, and net/http writes in the
// standard one there; pi's CLI speaks HTTP/1.1, D80.)
func responseStatusText(resp *http.Response) string {
	_, reason, _ := strings.Cut(resp.Status, " ")
	return jstext.DecodeUTF8([]byte(reason))
}

// createPiMessagesResponseError builds the error + its diagnostic details from a
// non-2xx response, the details in pi's key order. Port of
// createPiMessagesResponseError.
func createPiMessagesResponseError(model *ai.Model, url string, status int, statusText, body string) *piMessagesResponseError {
	errObj := parsePiMessagesErrorBody(body)
	var code any
	if c, _ := errObj.Get("code"); c != nil {
		if s, ok := c.(string); ok {
			code = s
		}
	}
	details := ai.OrderedObject{
		{Key: "version", Value: 1},
		{Key: "provider", Value: model.Provider},
		{Key: "model", Value: model.ID},
		{Key: "url", Value: url},
		{Key: "status", Value: status},
		{Key: "statusText", Value: statusText},
	}
	// pi spreads error:errorBody.error when there is an error body and
	// body:truncated when there is none: exactly one is present.
	if errObj != nil {
		details = append(details, ai.OrderedField{Key: "error", Value: errObj})
	} else {
		details = append(details, ai.OrderedField{Key: "body", Value: truncateDiagnosticString(body)})
	}
	details = append(details, ai.OrderedField{Key: "timestampMs", Value: nowMillis()})
	return &piMessagesResponseError{
		message:           formatPiMessagesResponseError(status, statusText, body, errObj),
		code:              code,
		diagnosticDetails: details,
	}
}

// appendPiMessagesRewriteDiagnostic mirrors pi appendRewriteDiagnostic: attach a
// "pi_messages_rewrite" diagnostic whose details are what the event's rewrite
// spreads into (piMessagesRewriteDetails); nil details attach nothing.
func appendPiMessagesRewriteDiagnostic(msg *ai.AssistantMessage, details ai.OrderedObject) {
	if details == nil {
		return
	}
	msg.Diagnostics = append(msg.Diagnostics, ai.Diagnostic{
		Type:      "pi_messages_rewrite",
		Timestamp: nowMillis(),
		Details:   details,
	})
}

// piMessagesConverter maintains a partial assistant message across the SSE
// stream, converting each backend event into a unified AssistantMessageEvent.
// Port of createEventConverter (1:1 on event semantics).
type piMessagesConverter struct {
	model   *ai.Model
	partial *ai.AssistantMessage
	// unindexed is the block pi stores for an event with no contentIndex:
	// `partial.content[undefined]` is the content array's ordinary property
	// "undefined", which is not content.
	unindexed ai.Content
	// toolArguments is pi's toolArguments Map: the arguments stream of each
	// tool call that started and has not ended (upstream f715a15c1), keyed by
	// its block index (piMessagesNoIndex for undefined).
	toolArguments map[int]*ai.ToolCallArgumentsStream
}

func newPiMessagesConverter(model *ai.Model) *piMessagesConverter {
	return &piMessagesConverter{
		model: model,
		partial: &ai.AssistantMessage{
			Content:    ai.ContentList{},
			Api:        model.Api,
			Provider:   model.Provider,
			Model:      model.ID,
			Usage:      ai.Usage{},
			StopReason: ai.StopPending,
			Timestamp:  nowMillis(),
		},
		toolArguments: map[int]*ai.ToolCallArgumentsStream{},
	}
}

// piMessagesNoIndex is the block index of an event with no contentIndex, and
// the contentIndex the port pushes for it, where pi's pushed event has none.
const piMessagesNoIndex = -1

// blockIndex is the check upstream 89c921d8b put ahead of the converter's
// switch, which every event passes through: an event carrying a contentIndex
// must hold an integer number from 0 to the last block — to the next block
// for a type ending in "_start" — since the index keys the content array,
// where "__proto__" would reach Array.prototype and a huge index make a
// sparse array of billions of holes. It returns the block the event
// addresses, or piMessagesNoIndex for an event without a contentIndex, which
// is not checked.
//
// Its errors fail the stream: pi's own for an index out of bounds, naming
// the index as JSON.stringify writes the wire value; V8's TypeError for a
// frame that is a primitive, which `"contentIndex" in event` throws on; and
// V8's TypeError for an event with a contentIndex and no string type, whose
// `event.type.endsWith` throws.
func (c *piMessagesConverter) blockIndex(ev piMessagesEvent) (int, error) {
	if ev.primitive != nil {
		name, _ := rawToString(ev.primitive) // a primitive always has one
		return 0, fmt.Errorf("Cannot use 'in' operator to search for 'contentIndex' in %s", name)
	}
	if ev.contentIndex == nil {
		return piMessagesNoIndex, nil
	}
	limit := len(c.partial.Content) - 1
	switch jsonValueKind(ev.typ) {
	case '"':
		if strings.HasSuffix(ev.Type, "_start") {
			limit++
		}
	case 0:
		return 0, readOfUndefined("endsWith")
	case 'n':
		return 0, errors.New("Cannot read properties of null (reading 'endsWith')")
	default:
		return 0, errors.New("event.type.endsWith is not a function")
	}
	// An index past float64's range reads as ±Infinity, which is no integer
	// and fails the bounds either way.
	if index, ok := rawNumber(ev.contentIndex); ok && index == math.Trunc(index) && index >= 0 && index <= float64(limit) {
		return int(index), nil
	}
	value, err := ai.DecodeOrderedValueKeepingSurrogates(ev.contentIndex)
	if err != nil {
		return 0, err // unreachable: the index is a member of a decoded frame
	}
	shown, _ := stringifyOrderedValue(value)
	return 0, fmt.Errorf("Invalid contentIndex %s in pi-messages %s event", shown, ev.Type)
}

// get reads the block at index, which blockIndex checked: nil only for an
// event with no contentIndex when none was started without one, where pi
// reads undefined.
func (c *piMessagesConverter) get(index int) ai.Content {
	if index == piMessagesNoIndex {
		return c.unindexed
	}
	return c.partial.Content[index]
}

// set replaces the block at index, which get found.
func (c *piMessagesConverter) set(index int, block ai.Content) {
	if index == piMessagesNoIndex {
		c.unindexed = block
		return
	}
	c.partial.Content[index] = block
}

// start stores a new block at index: pi's `partial.content[i] = block`,
// which appends the next block or replaces an existing one.
func (c *piMessagesConverter) start(index int, block ai.Content) {
	if index == len(c.partial.Content) {
		c.partial.Content = append(c.partial.Content, block)
		return
	}
	c.set(index, block)
}

// The TypeErrors V8 throws where pi's converter reaches a block that is not
// there, which only an event without a contentIndex can: `block.text +=
// delta` reads a property of it, and Object.assign converts it to an object.
func readOfUndefined(key string) error {
	return fmt.Errorf("Cannot read properties of undefined (reading '%s')", key)
}

var errAssignToUndefined = errors.New("Cannot convert undefined or null to object")

// assignPiMessagesToolCall is toolcall_end's `Object.assign(block, toolCall)`:
// each member the event's toolCall holds replaces the block's, and a member it
// lacks leaves the block's standing — the arguments its deltas built, for one.
// A null arguments is assigned as null. A toolCall that is not an object
// assigns nothing a tool call holds.
//
// pi assigns onto the block it has, whatever that is, and the block then
// carries the toolCall's type beside its own members. The port's block types
// cannot mix, so a text or thinking block becomes a tool call when the toolCall
// says `"type":"toolCall"`, and stays as it is otherwise. A member Go cannot
// hold in its field (a non-string id) is left as it was.
//
// argumentsError (upstream 87f9a3641) is assigned too, so a gateway's report of
// malformed final arguments fails validation here as it would have there. pi's
// validation reads it as `if (toolCall.argumentsError)` and writes it with a
// template literal, so a falsy one is held as "" (absent) and a truthy
// non-string as its JavaScript string; one with no string form is left as it
// was.
func assignPiMessagesToolCall(block ai.Content, raw json.RawMessage) ai.Content {
	o := rawOptional(raw)
	if o == nil {
		return block
	}
	tc, ok := block.(ai.ToolCall)
	if !ok {
		if typ, _ := rawString(o["type"]); typ != "toolCall" {
			return block
		}
	}
	if s, ok := rawString(o["id"]); ok {
		tc.ID = s
	}
	if s, ok := rawString(o["name"]); ok {
		tc.Name = s
	}
	switch args := o["arguments"]; jsonValueKind(args) {
	case '{':
		if m, order, err := ai.DecodeOrderedObject(args); err == nil {
			tc.Arguments, tc.ArgumentsOrder = m, order
		}
	case 'n':
		tc.Arguments, tc.ArgumentsOrder = nil, nil
	}
	if s, ok := rawString(o["thoughtSignature"]); ok {
		tc.ThoughtSignature = s
	}
	if s, ok := rawString(o["namespace"]); ok {
		tc.Namespace = s
	}
	if raw, present := o["argumentsError"]; present {
		if !rawTruthy(raw) {
			tc.ArgumentsError = ""
		} else if s, err := rawToString(raw); err == nil {
			tc.ArgumentsError = s
		}
	}
	return tc
}

// convert applies one event to the partial message and returns the event to
// push. Its error fails the stream: blockIndex's, or a TypeError pi's
// converter throws — a delta with no string form (see rawToString), or an
// event without a contentIndex when no block was started without one.
//
// An event for a block of another kind than its type implies (a text_delta
// into a thinking block) gives pi's block a stray property the port's block
// types cannot hold; the port leaves the block as it is.
func (c *piMessagesConverter) convert(ev piMessagesEvent) (ai.AssistantMessageEvent, error) {
	index, err := c.blockIndex(ev)
	if err != nil {
		return ai.AssistantMessageEvent{}, err
	}
	switch ev.Type {
	case "done":
		c.partial.StopReason = ev.Reason
		if ev.Usage != nil {
			c.partial.Usage = *ev.Usage
		}
		c.partial.ResponseID = ev.ResponseID
		if ev.ResponseModel != nil {
			c.partial.ResponseModel = *ev.ResponseModel
		}
		if ev.ProviderThinkingLevel != nil {
			c.partial.ProviderThinkingLevel = *ev.ProviderThinkingLevel
		}
		appendPiMessagesRewriteDiagnostic(c.partial, ev.Rewrite)
		return ai.AssistantMessageEvent{Type: ai.EventDone, Reason: ev.Reason, Message: c.partial}, nil
	case "error":
		c.partial.StopReason = ev.Reason
		if ev.Usage != nil {
			c.partial.Usage = *ev.Usage
		}
		c.partial.ErrorMessage = ev.ErrorMessage
		c.partial.ResponseID = ev.ResponseID
		if ev.ResponseModel != nil {
			c.partial.ResponseModel = *ev.ResponseModel
		}
		if ev.ProviderThinkingLevel != nil {
			c.partial.ProviderThinkingLevel = *ev.ProviderThinkingLevel
		}
		appendPiMessagesRewriteDiagnostic(c.partial, ev.Rewrite)
		return ai.AssistantMessageEvent{Type: ai.EventError, Reason: ev.Reason, Error: c.partial}, nil
	case "start":
		return ai.AssistantMessageEvent{Type: ai.EventStart, Partial: c.partial.Clone()}, nil
	case "text_start", "thinking_start":
		pushed := ai.EventTextStart
		var block ai.Content = ai.TextContent{Text: ""}
		if ev.Type == "thinking_start" {
			pushed, block = ai.EventThinkingStart, ai.ThinkingContent{Thinking: ""}
		}
		c.start(index, block)
		return ai.AssistantMessageEvent{Type: pushed, ContentIndex: index, Partial: c.partial.Clone()}, nil
	case "text_delta", "thinking_delta":
		// pi: `(partial.content[i] as {text}).text += event.delta` (thinking
		// alike): the block's property is read before the delta converts.
		pushed, member := ai.EventTextDelta, "text"
		if ev.Type == "thinking_delta" {
			pushed, member = ai.EventThinkingDelta, "thinking"
		}
		block := c.get(index)
		if block == nil {
			return ai.AssistantMessageEvent{}, readOfUndefined(member)
		}
		piece, err := rawToString(ev.delta)
		if err != nil {
			return ai.AssistantMessageEvent{}, err
		}
		switch b := block.(type) {
		case ai.TextContent:
			if member == "text" {
				b.Text += piece
				c.set(index, b)
			}
		case ai.ThinkingContent:
			if member == "thinking" {
				b.Thinking += piece
				c.set(index, b)
			}
		}
		return ai.AssistantMessageEvent{Type: pushed, ContentIndex: index, Delta: ev.Delta, Partial: c.partial.Clone()}, nil
	case "text_end":
		block := c.get(index)
		if block == nil {
			return ai.AssistantMessageEvent{}, errAssignToUndefined
		}
		if tc, ok := block.(ai.TextContent); ok {
			tc.Text, tc.TextSignature = ev.Content, ev.Signature
			c.set(index, tc)
		}
		return ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: index, Content: ev.Content, Partial: c.partial.Clone()}, nil
	case "thinking_end":
		block := c.get(index)
		if block == nil {
			return ai.AssistantMessageEvent{}, errAssignToUndefined
		}
		if tc, ok := block.(ai.ThinkingContent); ok {
			tc.Thinking, tc.ThinkingSignature, tc.Redacted = ev.Content, ev.Signature, ev.Redacted
			c.set(index, tc)
		}
		return ai.AssistantMessageEvent{Type: ai.EventThinkingEnd, ContentIndex: index, Content: ev.Content, Partial: c.partial.Clone()}, nil
	case "toolcall_start":
		c.start(index, ai.ToolCall{ID: ev.ID, Name: ev.ToolName, Arguments: map[string]any{}})
		c.toolArguments[index] = &ai.ToolCallArgumentsStream{}
		return ai.AssistantMessageEvent{Type: ai.EventToolCallStart, ContentIndex: index, Partial: c.partial.Clone()}, nil
	case "toolcall_delta":
		// pi: a delta for a call with no arguments stream (ended) changes
		// nothing; otherwise the stream reads the delta before
		// `partial.content[i].arguments` is set from it. A stream always has
		// its block: toolcall_start stores both under one index.
		// The delta is read keeping half a surrogate pair, so a pair cut
		// between two deltas joins again in the arguments, as in pi.
		pushed := ev.Delta
		value, verr := jsValueKeepingSurrogates(ev.delta)
		if text, isString := value.(string); isString && verr == nil {
			pushed = text
		}
		if args := c.toolArguments[index]; args != nil {
			if verr != nil {
				return ai.AssistantMessageEvent{}, verr
			}
			partial, order, delta, err := appendToolCallArguments(args, value)
			if err != nil {
				return ai.AssistantMessageEvent{}, err
			}
			pushed = delta
			if tc, ok := c.get(index).(ai.ToolCall); ok {
				tc.Arguments, tc.ArgumentsOrder = partial, order
				c.set(index, tc)
			}
		}
		return ai.AssistantMessageEvent{Type: ai.EventToolCallDelta, ContentIndex: index, Delta: pushed, Partial: c.partial.Clone()}, nil
	case "toolcall_end":
		block := c.get(index)
		if block == nil {
			return ai.AssistantMessageEvent{}, errAssignToUndefined
		}
		block = assignPiMessagesToolCall(block, ev.toolCall)
		c.set(index, block)
		delete(c.toolArguments, index)
		var tc *ai.ToolCall
		if v, ok := block.(ai.ToolCall); ok {
			tc = &v
		}
		return ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: index, ToolCall: tc, Partial: c.partial.Clone()}, nil
	}
	// Any other event — an unknown type, or an array frame, which has no type
	// — reaches pi's trailing `return { ...event, partial }`, as start and the
	// per-block cases do: the event pushed carries the frame's type ("" when
	// it has none) and the partial message.
	return ai.AssistantMessageEvent{Type: ai.EventType(ev.Type), Partial: c.partial.Clone()}, nil
}

// piMessagesFrameData is pi parsePiMessagesEvent's text: the first `data:`
// line of a frame, JS-trimmed. ok is false when the frame has none, or when it
// is empty or `[DONE]`, which pi passes over without parsing.
func piMessagesFrameData(frame string) (string, bool) {
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data:") {
			data := jstext.Trim(line[5:])
			return data, data != "" && data != "[DONE]"
		}
	}
	return "", false
}

// decodePiMessagesEvent is pi's JSON.parse of a frame's data plus
// readPiMessagesEvents' `if (event)`: yielded is false for a JS-falsy value
// (null, false, 0, -0, "" — and 1e-400, which parses to 0). An object's members
// are read into the typed event; an array converts as an event with no type,
// as pi's `{...event, partial}` of it has none, and any other truthy value is
// the event's primitive, which the converter fails on. err is the
// syntax error JSON.parse throws on data that is not JSON, which fails the
// stream.
//
// data is decoded once: an object into its members, which validates it on the
// way, anything else only validated. The value an observer receives is a
// separate decode (readPiMessagesEvents), made only when there is one.
func decodePiMessagesEvent(data string) (ev piMessagesEvent, yielded bool, err error) {
	if jsonValueKind(data) == '{' {
		o, err := decodeRawObject([]byte(data))
		if err != nil {
			return piMessagesEvent{}, false, err
		}
		return piMessagesEventOf(o), true, nil
	}
	if !json.Valid([]byte(data)) {
		var v any
		return piMessagesEvent{}, false, json.Unmarshal([]byte(data), &v)
	}
	if jsonValueKind(data) == '[' {
		return piMessagesEvent{}, true, nil
	}
	return piMessagesEvent{primitive: json.RawMessage(data)}, rawTruthy(json.RawMessage(data)), nil
}

// readPiMessagesEvents consumes the SSE body: frames are separated by "\n\n",
// CRLF is normalized to "\n", and a trailing non-terminal buffer is flushed.
// The body is text as pi's TextDecoder makes it: one byte-order mark at the
// very start is dropped, and invalid UTF-8 becomes U+FFFD per maximal subpart
// (jstext.DecodeUTF8); a frame ends at a line break, which never belongs to a
// multi-byte sequence, so decoding each frame is decoding the body.
// handle returns false to stop early (a terminal event was seen), and its
// error ends the read. A frame that is not JSON fails the read, as pi's
// JSON.parse throw does. Port of readPiMessagesEvents.
//
// pi never checks its signal here, and neither does this: an abort reaches
// the reading only through the body — the port's own client's body rejects
// the read the abort cuts short, or the next one, as undici's fetch does
// (fetchBody), even the one that would report a body already complete; a
// custom client's body goes on as it goes on. What an earlier read
// delivered is still framed and handled first.
//
// onEvent, when non-nil, observes every yielded frame's parsed value — objects
// as ai.OrderedObject, unknown fields and the terminal done/error included —
// before it is converted (pi's onProviderStreamEvent, upstream 002fc8385); its
// error ends the read. The value is a decode of its own, made only when
// onEvent is non-nil, so observing costs nothing when unset and a mutating
// observer cannot change what is converted. A body whose reads never progress
// fails with the port's guard (progressReader).
func readPiMessagesEvents(body io.Reader, onEvent func(any) error, handle func(piMessagesEvent) (bool, error)) error {
	body = &progressReader{r: body, provider: "pi-messages"}
	// emit handles one frame and reports whether to keep reading.
	emit := func(frame string) (bool, error) {
		if !utf8.ValidString(frame) {
			frame = jstext.DecodeUTF8([]byte(frame))
		}
		data, ok := piMessagesFrameData(frame)
		if !ok {
			return true, nil
		}
		ev, yielded, err := decodePiMessagesEvent(data)
		if err != nil {
			return false, err
		}
		if !yielded {
			return true, nil
		}
		if onEvent != nil {
			value, err := ai.DecodeOrderedValue([]byte(data))
			if err != nil {
				return false, err // unreachable: data parsed above
			}
			if err := onEvent(value); err != nil {
				return false, err
			}
		}
		return handle(ev)
	}
	buf := make([]byte, 32*1024)
	var pending string
	// atStart is whether pending still holds the body's first bytes, which
	// may be the byte-order mark the decoder drops.
	atStart := true
	for {
		n, readErr := body.Read(buf)
		chunk := string(buf[:n])
		if atStart {
			switch head := pending + chunk; {
			case strings.HasPrefix(head, piMessagesBOM):
				pending, chunk, atStart = "", head[len(piMessagesBOM):], false
			case readErr != nil || !strings.HasPrefix(piMessagesBOM, head):
				pending, chunk, atStart = "", head, false
			default:
				pending = head
				continue // too few bytes yet to tell
			}
		}
		if chunk != "" {
			// Normalize the whole accumulated buffer (pi: buffer.replace(/\r\n/g,
			// "\n") each read), so a "\r\n" split across two reads still collapses.
			// pending holds only an unframed remainder, so the re-scan stays cheap.
			pending = strings.ReplaceAll(pending+chunk, "\r\n", "\n")
			for {
				split := strings.Index(pending, "\n\n")
				if split == -1 {
					break
				}
				frame := pending[:split]
				pending = pending[split+2:]
				more, err := emit(frame)
				if err != nil {
					return err
				}
				if !more {
					return nil
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if jstext.Trim(jstext.DecodeUTF8([]byte(pending))) != "" {
		if _, err := emit(pending); err != nil {
			return err
		}
	}
	return nil
}

// piMessagesBOM is the UTF-8 byte-order mark a TextDecoder drops from the start
// of what it decodes.
const piMessagesBOM = "\xEF\xBB\xBF"

// createPiMessagesErrorEvent builds the terminal error event for a thrown
// failure, attaching the response-failure diagnostic for non-aborted
// PiMessagesResponseErrors. Port of createErrorEvent.
func createPiMessagesErrorEvent(model *ai.Model, err error, aborted bool) ai.AssistantMessageEvent {
	reason := ai.StopError
	if aborted {
		reason = ai.StopAborted
	}
	msg := ai.NewErrorMessage(model.Api, model.Provider, model.ID, reason, err.Error())
	if !aborted {
		if re, ok := err.(*piMessagesResponseError); ok {
			msg.Diagnostics = append(msg.Diagnostics, ai.Diagnostic{
				Type:      "pi_messages_response_failure",
				Timestamp: nowMillis(),
				Error:     &ai.DiagnosticErrorInfo{Name: "PiMessagesResponseError", Message: re.message, Code: re.code},
				Details:   re.diagnosticDetails,
			})
		}
	}
	return ai.AssistantMessageEvent{Type: ai.EventError, Reason: reason, Error: msg}
}

// resolvePiMessagesCacheRetention mirrors pi resolveCacheRetention: an explicit
// retention wins; otherwise the legacy PI_CACHE_RETENTION=long env opt-in maps
// to "long"; else backend defaults apply (empty).
func resolvePiMessagesCacheRetention(cacheRetention ai.CacheRetention, env map[string]string) ai.CacheRetention {
	if cacheRetention != "" {
		return cacheRetention
	}
	if getProviderEnvValue("PI_CACHE_RETENTION", env) == "long" {
		return ai.CacheLong
	}
	return ""
}

// StreamPiMessages streams from a pi-messages backend. Port of the `stream`
// export in pi-messages.ts.
func StreamPiMessages(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *PiMessagesOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &PiMessagesOptions{}
	}
	conv := newPiMessagesConverter(model)

	go func() {
		aborted := func() bool { return ctx != nil && ctx.Err() != nil }
		fail := func(err error) {
			stream.Push(createPiMessagesErrorEvent(model, err, aborted()))
			stream.End()
		}
		// Mirror pi's streaming-block try/catch: the converter returns the
		// TypeErrors pi's throws (a delta or end for a block never started), and
		// any panic past those still becomes a terminal error event, as pi's
		// throw is caught into createErrorEvent, rather than crashing the host
		// process.
		defer func() {
			if r := recover(); r != nil {
				fail(fmt.Errorf("%v", r))
			}
		}()

		apiKey := opts.APIKey
		if apiKey == "" {
			fail(fmt.Errorf("No API key provided for provider %q", model.Provider))
			return
		}

		// pi: `new URL(...)`, before the payload is built or onPayload runs;
		// fetch sends what that makes of it (requestURL).
		url, err := requestURL(strings.TrimRight(model.BaseURL, "/") + "/messages")
		if err != nil {
			fail(err)
			return
		}
		if opts.Debug {
			url += "?debug=1"
		}

		requestOptions := map[string]any{}
		if opts.Temperature != nil {
			requestOptions["temperature"] = *opts.Temperature
		}
		if opts.MaxTokens != nil {
			requestOptions["maxTokens"] = *opts.MaxTokens
		}
		if opts.Reasoning != "" {
			requestOptions["reasoning"] = opts.Reasoning
		}
		if cr := resolvePiMessagesCacheRetention(opts.CacheRetention, opts.Env); cr != "" {
			requestOptions["cacheRetention"] = cr
		}
		if opts.SessionID != "" {
			requestOptions["sessionId"] = opts.SessionID
		}
		if opts.ToolChoice != nil {
			requestOptions["toolChoice"] = opts.ToolChoice
		}

		var payload any = map[string]any{
			"model":   model.ID,
			"context": req,
			"options": requestOptions,
		}
		if opts.OnPayload != nil {
			next, perr := opts.OnPayload(payload, model)
			if perr != nil {
				fail(perr)
				return
			}
			if next != nil {
				payload = next
			}
		}
		body, err := json.Marshal(payload)
		if err != nil {
			fail(err)
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			fail(err)
			return
		}
		// pi builds one record of three sources (upstream adc5fa9f8):
		// providerHeadersToRecord(getIdentityHeaders(model, identity),
		// {authorization, accept, "content-type"}, options.headers), and no
		// model.headers. The record folds names case-insensitively, so a later
		// source's value replaces every earlier spelling of its name and a
		// marker deletes it — the three fixed headers included, which are part
		// of the record now. headerObject carries the merge order so a consumer
		// map holding two spellings of one name cannot let Go's map iteration
		// pick the winner.
		o := &headerObject{}
		setIdentityHeaders(o, model, opts.Identity)
		o.set("authorization", "Bearer "+apiKey)
		o.set("accept", "text/event-stream")
		o.set("content-type", "application/json")
		o.mergeRecord(opts.Headers)
		if err := o.applyAsFetchInit(httpReq.Header); err != nil {
			fail(err)
			return
		}

		// pi: `(options?.fetch ?? globalThis.fetch)(url, …)` — this provider calls
		// fetch directly rather than through an SDK, so no timeoutMs applies:
		// only fetch's own headersTimeout bounds the wait for the response.
		var client ai.HTTPDoer = sharedClient(undiciHeadersTimeoutMs)
		c, custom := customHTTPClient(opts.HTTPClient)
		if custom {
			client = c
		}
		var resp *http.Response
		if !custom {
			// undici's fetch refuses some requests before it looks at the
			// signal or sends anything.
			err = fetchRefusal(httpReq)
		}
		if err == nil {
			resp, err = client.Do(httpReq)
			// fetch rejects with undici's AbortError when the signal aborts
			// before the response arrives, however far the request got, and
			// with its TypeError "fetch failed" for any other failure; a
			// custom fetch rejects with whatever it rejects with, abort or
			// no abort.
			if err != nil && !custom {
				if aborted() {
					err = errOperationAborted
				} else {
					err = undiciFetchError(err)
				}
			}
		}
		if err != nil {
			fail(err)
			return
		}
		nullBody := nullResponseBody(resp)
		defer resp.Body.Close()
		var respBody io.Reader = resp.Body
		if !custom {
			respBody = fetchBody{ctx, resp.Body}
		}

		if opts.OnResponse != nil {
			// pi calls onResponse before the response.ok check, so error responses
			// still surface their headers; an awaited rejection propagates to the
			// catch (a terminal error event), so a non-nil error fails the stream.
			if rerr := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: responseHeadersRecord(resp)}, model); rerr != nil {
				fail(rerr)
				return
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// pi's response.text(): the body decoded as UTF-8, a leading
			// byte-order mark dropped and invalid bytes U+FFFD per maximal
			// subpart. A read that fails rejects it (fetchBody words a
			// failure of the port's own client as undici's), and that error,
			// which is no response error, is what the stream fails with.
			data, err := io.ReadAll(respBody)
			if err != nil {
				fail(err)
				return
			}
			fail(createPiMessagesResponseError(model, url, resp.StatusCode, responseStatusText(resp), jstext.DecodeUTF8(stripBOM(data))))
			return
		}
		// pi: `if (!response.body) throw new Error(`${model.provider} response has
		// no body`)`, before any event, so no start (nullResponseBody).
		if nullBody {
			fail(fmt.Errorf("%s response has no body", model.Provider))
			return
		}

		// pi awaits options.onProviderStreamEvent(piEvent, model) before
		// convertEvent, with the model the stream was called with.
		var onEvent func(any) error
		if opts.OnProviderStreamEvent != nil {
			onEvent = func(data any) error { return opts.OnProviderStreamEvent(data, model) }
		}
		terminal := false
		perr := readPiMessagesEvents(respBody, onEvent, func(ev piMessagesEvent) (bool, error) {
			out, err := conv.convert(ev)
			if err != nil {
				return false, err
			}
			stream.Push(out)
			if out.Type == ai.EventDone || out.Type == ai.EventError {
				terminal = true
				return false, nil
			}
			return true, nil
		})
		if perr != nil {
			fail(perr)
			return
		}
		if terminal {
			stream.End()
			return
		}
		fail(fmt.Errorf("%s stream ended without a terminal event", model.Provider))
	}()

	return stream
}

// StreamSimplePiMessages maps SimpleStreamOptions → the full stream, forwarding
// reasoning and the unified toolChoice. Port of the `streamSimple` export;
// upstream e5dde9a76 moved toolChoice off the provider-extra object onto the
// unified options, so it no longer depends on the caller passing native options.
func StreamSimplePiMessages(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	p := &PiMessagesOptions{}
	if opts != nil {
		p.StreamOptions = opts.StreamOptions
		p.Reasoning = opts.Reasoning
		if opts.ToolChoice != "" {
			p.ToolChoice = string(opts.ToolChoice)
		}
	}
	return StreamPiMessages(ctx, model, req, p)
}

// RegisterPiMessages registers the pi-messages api provider.
func RegisterPiMessages() {
	ai.RegisterApiProvider(piMessagesApiProvider())
}

// piMessagesApiProvider is the pi-messages api's built-in implementation.
func piMessagesApiProvider() ai.ApiProvider {
	return ai.ApiProvider{
		Api: ai.APIPiMessages,
		Stream: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
			p := &PiMessagesOptions{}
			if opts != nil {
				p.StreamOptions = *opts
			}
			return StreamPiMessages(ctx, model, req, p)
		},
		StreamSimple: StreamSimplePiMessages,
	}
}
