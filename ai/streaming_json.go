package ai

// Incremental, tolerant parsing of JSON text that arrives in pieces, such as
// streamed tool-call arguments (pi's utils/streaming-json.ts).
//
// StreamingJSONParser.Append reads each piece once, so a stream costs time and
// memory linear in its length, and Snapshot returns the value of the text read
// so far. Snapshots are plain JSON values in DecodeOrderedValue's shapes
// (OrderedObject, []any, float64, string, bool, nil) and never change once
// returned: the parser copies the containers that are still open (the "open
// path") into every new snapshot and shares completed containers and strings.
// The rules for incomplete text follow pi's parseStreamingJson():
//
//   - An unterminated string shows the characters read so far; an
//     unterminated escape is held back until complete.
//   - An unterminated container shows the members and elements read so far.
//   - A member whose key or value has not started is omitted.
//   - An unterminated literal shows its value once it is a prefix of `true`,
//     `false` or `null`; an unterminated number shows its value when the
//     digits read so far form a number (an exponent without digits is
//     ignored).
//
// Malformed text is read leniently, the way RepairJSON and parseStreamingJson
// treat it: raw control characters in strings are kept, an invalid escape
// such as `\d` stays literal text, missing commas and colons are tolerated,
// values in invalid positions and unknown words are dropped, and text after
// the root value is ignored. The parser never fails. Callers that need the
// exact value of the finished text parse it once, e.g. with
// ParseToolCallArguments.
//
// Differences from parseStreamingJson on incomplete text: trailing whitespace
// inside an unterminated string is kept (it was trimmed), a string that
// contains a raw control character or an invalid escape stays visible while
// it streams (it was dropped until the document was complete), and `NaN` and
// `Infinity` words are dropped.
//
// Append also returns the changes the piece made, for a writer that keeps its
// own copy of the value and applies them instead of re-assigning snapshots:
// `set` when a key, an array item or a finished value appears (a new string
// is set once with its first text, a container empty), `append` when the
// piece extended an open string. Unterminated numbers and literals are not
// reported until they end. A container nested more than 32 levels deep is
// reported as one `set` of its whole value when it closes, so paths stay
// short.
//
// Cost: copying the open path costs its member and element count. Each Append
// earns copy credit proportional to its length plus a constant; Snapshot
// spends it and returns the previous snapshot while the open path costs more
// than the credit. So a document that keeps a very large container open
// (thousands of elements) shows a preview that lags a little behind the text,
// and no input makes the total work grow faster than the text.
//
// Text is measured as pi measures it, in UTF-16 code units. A piece may end
// inside a surrogate pair: a lone surrogate is held as its WTF-8 byte triple,
// and the two halves join into one character when the second arrives, as JS
// string concatenation joins them.

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/sky-valley/pi/internal/jstext"
)

// JSONPath is a path from the root value: member keys (string) and array
// indices (int).
type JSONPath []any

// JSONChangeKind names what a JSONChange does.
type JSONChangeKind string

const (
	// JSONChangeSet sets the value at Path to Value.
	JSONChangeSet JSONChangeKind = "set"
	// JSONChangeAppend appends Text to the string at Path.
	JSONChangeAppend JSONChangeKind = "append"
)

// JSONChange is one change a piece of text made to the value; see
// StreamingJSONParser. Value is set for JSONChangeSet (in DecodeOrderedValue's
// shapes, and never changed afterwards), Text for JSONChangeAppend.
type JSONChange struct {
	Kind  JSONChangeKind
	Path  JSONPath
	Value any
	Text  string
}

type jsonSlotKind uint8

const (
	slotRoot jsonSlotKind = iota
	slotMember
	slotElement
)

// jsonSlot is where a value sits in its parent; a nil *jsonSlot is a value in
// an invalid position, which is read but dropped.
type jsonSlot struct {
	kind  jsonSlotKind
	key   string
	index int
}

var rootSlot = &jsonSlot{kind: slotRoot}

// containerIDs numbers container snapshots, across parsers, so two are the
// same only when they are the same value.
var containerIDs atomic.Uint64

// jsonNode is a value in the forms the port hands out: ord, in
// DecodeOrderedValue's shapes (an object is an OrderedObject), and plain, in
// encoding/json's (an object is a map[string]any), so a tool call gets its
// Arguments and ArgumentsOrder without converting a whole tree per snapshot.
// A number past float64's range is ±Inf in ord, as Snapshot shows pi's value,
// and nil in plain and in tool — the ordered form a tool call holds, which
// the port writes as JSON.stringify writes Infinity. tool is set only on a
// value that holds such a number (nonFinite); elsewhere it is ord, so the
// two share every container that holds none.
// A container's id tells snapshots apart as JS object identity does; a
// scalar has none. open is the frame of a container that is still open,
// whose snapshots are copies.
type jsonNode struct {
	ord, plain, tool any
	nonFinite        bool
	id               uint64
	open             *jsonFrame
}

// toolOrd is the ordered form a tool call holds.
func (n jsonNode) toolOrd() any {
	if n.nonFinite {
		return n.tool
	}
	return n.ord
}

type jsonPhase uint8

const (
	phaseKey jsonPhase = iota
	phaseColon
	phaseValue
	phaseComma
)

// jsonFrame is an open object or array. Its members are never handed out
// while open; snapshots get copies.
type jsonFrame struct {
	isArray bool
	elems   []jsonNode
	// An object's members: the keys that are array indices, listed ascending
	// (sorted when a copy needs them, after indexUnsorted), then the others in
	// creation order, as a JS object lists its own keys.
	indexKeys     []indexKey
	indexUnsorted bool
	nameKeys      []string
	members       map[string]jsonNode
	slot          *jsonSlot
	// path is where the container sits, when its changes are reported
	// (hasPath).
	path    JSONPath
	hasPath bool
	// reportsInside is whether changes inside it are reported; when not, its
	// whole value is reported as it closes.
	reportsInside bool
	// size is the member or element count: what a copy of the container costs.
	size int
	// key is an object's key read last, where the member value goes (hasKey).
	key    string
	hasKey bool
	phase  jsonPhase
}

// indexKey is a member key that is an array index, and its value.
type indexKey struct {
	n   uint32
	key string
}

// openString is an unterminated string.
type openString struct {
	isKey bool
	slot  *jsonSlot
	text  *strings.Builder
	// escape is the escape read so far: "" for none, otherwise a backslash and
	// up to five more characters.
	escape string
	// path is where a value string sits, when its changes are reported
	// (hasPath).
	path    JSONPath
	hasPath bool
	// change is the index of the change this string extends while it is the
	// newest change of the piece; -1 for none. Its text is text from
	// changeStart, after changePrefix (half a surrogate pair whose other half
	// the text already held), and is copied into the change once the change
	// stops growing (flushChange), so a piece with many escapes costs time
	// linear in its length.
	change       int
	changeStart  int
	changePrefix string
}

// Changes are reported at most this deep, so a path costs O(1) to build.
const maxReportedDepth = 32

// Copy credit per append and per appended character. The floor keeps small
// documents fresh at every snapshot.
const (
	creditPerAppend    = 256
	creditPerCharacter = 8
)

// An unterminated number longer than this is not shown until it ends, so
// Snapshot stays cheap.
const maxPendingScalarLength = 64

var (
	jsonNumber                 = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	jsonNumberWithoutExpDigits = regexp.MustCompile(`^(-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?)[eE][+-]?$`)
	jsonLiterals               = [...]struct {
		text  string
		value any
	}{{"true", true}, {"false", false}, {"null", nil}}
)

// StreamingJSONParser is pi's StreamingJsonParser: an incremental, tolerant
// JSON parser; the comment at the top of streaming_json.go gives its rules
// and costs. The zero value is ready to use. A parser is not safe for
// concurrent use. The values Snapshot and Append return share their closed
// containers and strings with later snapshots: they are read-only.
type StreamingJSONParser struct {
	root    jsonNode
	hasRoot bool
	done    bool
	stack   []*jsonFrame
	str     *openString
	scalar  []byte
	// scalarSlot is where the unterminated scalar goes; nil for an invalid
	// position.
	scalarSlot *jsonSlot
	// pathCost is the sum of size+1 over the open path: what a fresh snapshot
	// copies.
	pathCost int
	credit   int
	dirty    bool
	snap     jsonNode
	hasSnap  bool
	// changes are the changes of the piece being read.
	changes []JSONChange
	// growing is the open string whose change, the newest, is not filled in
	// yet; nil for none.
	growing *openString
}

// Append reads the next piece of text and returns the changes it made, in
// order. It costs time linear in the length of text.
func (p *StreamingJSONParser) Append(text string) []JSONChange {
	p.credit += creditPerAppend + creditPerCharacter*jstext.CodeUnitLength(text)
	p.changes = nil
	if p.str != nil {
		p.str.change = -1
	}
	index := 0
	for index < len(text) && !p.done {
		if open := p.str; open != nil {
			index = p.readString(open, text, index)
		} else if len(p.scalar) > 0 {
			index = p.readScalar(text, index)
		} else {
			p.structural(text[index])
			index++
		}
	}
	p.flushChange()
	return p.changes
}

// Snapshot returns the value of the text read so far, and false before a
// value starts. The value never changes afterwards. It is the previous
// snapshot while the text read since did not change the value, or while a
// fresh copy of the open path is not yet paid for (see StreamingJSONParser).
func (p *StreamingJSONParser) Snapshot() (any, bool) {
	node, ok := p.snapshot()
	return node.ord, ok
}

func (p *StreamingJSONParser) snapshot() (jsonNode, bool) {
	if !p.dirty {
		return p.snap, p.hasSnap
	}
	pending, hasPending := p.pendingScalar()
	cost := p.pathCost
	if hasPending {
		cost++
	}
	if cost > p.credit {
		return p.snap, p.hasSnap
	}
	p.credit -= cost
	p.dirty = false
	p.snap, p.hasSnap = p.copyOpenPath(pending, hasPending)
	return p.snap, p.hasSnap
}

// readString reads string characters from start until the string ends or
// the text does; it returns the next index.
func (p *StreamingJSONParser) readString(open *openString, text string, start int) int {
	runStart := start
	for index := start; index < len(text); index++ {
		c := text[index]
		if open.escape != "" {
			if p.continueEscape(open, c) {
				runStart = index + 1
				continue
			}
			// Not part of the escape: read it as a normal string character below.
			runStart = index
		}
		if c == '"' {
			if index > runStart {
				p.appendString(open, text[runStart:index])
			}
			p.endString(open)
			return index + 1
		}
		if c == '\\' {
			if index > runStart {
				p.appendString(open, text[runStart:index])
			}
			open.escape = "\\"
			runStart = index + 1
		}
	}
	if len(text) > runStart {
		p.appendString(open, text[runStart:])
	}
	return len(text)
}

// readScalar reads number or literal characters from start up to the next
// delimiter; it returns the delimiter's index.
func (p *StreamingJSONParser) readScalar(text string, start int) int {
	end := start
	for end < len(text) && !isJSONDelimiter(text[end]) {
		end++
	}
	if end > start {
		p.scalar = append(p.scalar, text[start:end]...)
		if p.scalarSlot != nil {
			p.dirty = true
		}
	}
	if end < len(text) {
		p.endScalar()
	}
	return end
}

// structural handles one character outside strings.
func (p *StreamingJSONParser) structural(c byte) {
	switch c {
	case ' ', '\t', '\n', '\r':
		return
	case '"':
		p.startString()
	case '{', '[':
		p.openContainer(c == '[')
	case '}', ']':
		p.closeContainer()
	case ':':
		if frame := p.top(); frame != nil && !frame.isArray && frame.phase == phaseColon {
			frame.phase = phaseValue
		}
	case ',':
		if frame := p.top(); frame != nil {
			if frame.isArray {
				frame.phase = phaseValue
			} else {
				frame.phase = phaseKey
			}
		}
	default:
		p.scalar = append(p.scalar[:0], c)
		p.scalarSlot = p.valueSlot()
		if p.scalarSlot != nil {
			p.dirty = true
		}
	}
}

func (p *StreamingJSONParser) top() *jsonFrame {
	if len(p.stack) == 0 {
		return nil
	}
	return p.stack[len(p.stack)-1]
}

// valueSlot is where a value starting now goes, or nil when this position
// takes no value.
func (p *StreamingJSONParser) valueSlot() *jsonSlot {
	frame := p.top()
	if frame == nil {
		if !p.hasRoot {
			return rootSlot
		}
		return nil
	}
	if frame.isArray {
		return &jsonSlot{kind: slotElement, index: len(frame.elems)}
	}
	// A missing colon is tolerated: `{"a" 1}` reads as `{"a": 1}`.
	if (frame.phase == phaseValue || frame.phase == phaseColon) && frame.hasKey {
		return &jsonSlot{kind: slotMember, key: frame.key}
	}
	return nil
}

func (p *StreamingJSONParser) startString() {
	// A missing comma is tolerated: `{"a": 1 "b": 2}` reads the second member.
	if frame := p.top(); frame != nil && !frame.isArray && (frame.phase == phaseKey || frame.phase == phaseComma) {
		p.str = &openString{isKey: true, change: -1}
		return
	}
	slot := p.valueSlot()
	path, hasPath := p.reportedPath(slot)
	p.str = &openString{slot: slot, path: path, hasPath: hasPath, change: -1}
	p.place(slot, scalarNode(""))
	if hasPath {
		p.str.change = p.report(JSONChange{Kind: JSONChangeSet, Path: path, Value: ""})
		p.growing = p.str
	}
}

// reportedPath is the path of a value at slot in the innermost open
// container, when its changes are reported.
func (p *StreamingJSONParser) reportedPath(slot *jsonSlot) (JSONPath, bool) {
	if slot == nil {
		return nil, false
	}
	if slot.kind == slotRoot {
		return JSONPath{}, true
	}
	frame := p.top()
	if !frame.reportsInside {
		return nil, false
	}
	path := make(JSONPath, len(frame.path), len(frame.path)+1)
	copy(path, frame.path)
	if slot.kind == slotMember {
		return append(path, slot.key), true
	}
	return append(path, slot.index), true
}

// report records a change of the piece and returns its index.
func (p *StreamingJSONParser) report(change JSONChange) int {
	p.flushChange()
	p.changes = append(p.changes, change)
	return len(p.changes) - 1
}

// flushChange fills in the text of the growing string's change, which stops
// growing now.
func (p *StreamingJSONParser) flushChange() {
	open := p.growing
	if open == nil {
		return
	}
	p.growing = nil
	text := open.changePrefix + builderText(open.text)[open.changeStart:]
	if change := &p.changes[open.change]; change.Kind == JSONChangeSet {
		change.Value = text
	} else {
		change.Text = text
	}
}

// simpleJSONEscapes decodes the one-character escapes.
var simpleJSONEscapes = [256]string{
	'"': `"`, '\\': `\`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t",
}

// continueEscape continues an escape with c; it returns false when c is not
// part of it and must be read normally.
func (p *StreamingJSONParser) continueEscape(open *openString, c byte) bool {
	if open.escape == "\\" {
		if decoded := simpleJSONEscapes[c]; decoded != "" {
			open.escape = ""
			p.appendString(open, decoded)
			return true
		}
		if c == 'u' {
			open.escape = "\\u"
			return true
		}
		// Invalid escape: keep the backslash as text and read the character
		// normally, as RepairJSON does.
		open.escape = ""
		p.appendString(open, "\\")
		return false
	}
	if isHexDigit(c) {
		open.escape += string(c)
		if len(open.escape) < 6 {
			return true
		}
		var unit rune
		for _, h := range []byte(open.escape[2:]) {
			unit = unit<<4 | rune(hexValue(h))
		}
		open.escape = ""
		p.appendString(open, jstext.CodeUnit(unit))
		return true
	}
	// `\u` without four hex digits: keep it as text and read the character
	// normally, as RepairJSON does.
	literal := open.escape
	open.escape = ""
	p.appendString(open, literal)
	return false
}

func (p *StreamingJSONParser) appendString(open *openString, text string) {
	before := builderText(open.text)
	_, joined := jstext.JoinsSurrogates(before, text)
	open.text = appendUTF16(open.text, text)
	if open.isKey {
		return
	}
	p.place(open.slot, scalarNode(open.text.String()))
	if !open.hasPath {
		return
	}
	// Extend this string's newest change while nothing came after it: its text
	// is already in open.text. Otherwise report an append, whose text starts
	// here — with the low half of a pair whose high half came in an earlier
	// change, which the text now holds joined.
	if open.change >= 0 && open.change == len(p.changes)-1 {
		return
	}
	open.change = p.report(JSONChange{Kind: JSONChangeAppend, Path: open.path})
	open.changeStart, open.changePrefix = len(before), ""
	if joined {
		open.changeStart, open.changePrefix = len(before)+1, text[:3]
	}
	p.growing = open
}

func (p *StreamingJSONParser) endString(open *openString) {
	p.str = nil
	frame := p.top()
	if open.isKey {
		if frame == nil {
			return
		}
		frame.key, frame.hasKey = builderText(open.text), true
		frame.phase = phaseColon
		return
	}
	p.valueEnded(frame)
}

func (p *StreamingJSONParser) endScalar() {
	token := string(p.scalar)
	slot := p.scalarSlot
	p.scalar = p.scalar[:0]
	p.scalarSlot = nil
	if value, ok := p.scalarValue(token); ok {
		p.place(slot, scalarNode(value))
		if path, ok := p.reportedPath(slot); ok {
			p.report(JSONChange{Kind: JSONChangeSet, Path: path, Value: value})
		}
	}
	p.valueEnded(p.top())
}

type pendingScalar struct {
	slot  *jsonSlot
	value any
}

// pendingScalar is the value of the unterminated scalar at the end of the
// text, if it shows.
func (p *StreamingJSONParser) pendingScalar() (pendingScalar, bool) {
	slot := p.scalarSlot
	// A token past the length limit, or holding anything but ASCII, shows
	// nothing, so its UTF-16 length need not be measured: only ASCII tokens
	// have a value, and their bytes are their code units.
	if slot == nil || len(p.scalar) == 0 || len(p.scalar) > maxPendingScalarLength {
		return pendingScalar{}, false
	}
	value, ok := p.pendingScalarValue(string(p.scalar))
	if !ok {
		return pendingScalar{}, false
	}
	return pendingScalar{slot: slot, value: value}, true
}

func (p *StreamingJSONParser) openContainer(isArray bool) {
	slot := p.valueSlot()
	path, hasPath := p.reportedPath(slot)
	frame := &jsonFrame{
		isArray:       isArray,
		slot:          slot,
		path:          path,
		hasPath:       hasPath,
		reportsInside: hasPath && len(path) < maxReportedDepth,
		phase:         phaseKey,
	}
	if isArray {
		frame.phase = phaseValue
	} else {
		frame.members = map[string]jsonNode{}
	}
	p.place(slot, jsonNode{open: frame})
	p.stack = append(p.stack, frame)
	p.pathCost++
	p.dirty = true
	// A fresh container: the working one keeps changing, and a change never
	// does.
	if hasPath {
		var empty any = OrderedObject{}
		if isArray {
			empty = []any{}
		}
		p.report(JSONChange{Kind: JSONChangeSet, Path: path, Value: empty})
	}
}

func (p *StreamingJSONParser) closeContainer() {
	frame := p.top()
	// A stray closer outside any container is ignored.
	if frame == nil {
		return
	}
	p.stack[len(p.stack)-1] = nil
	p.stack = p.stack[:len(p.stack)-1]
	p.pathCost -= frame.size + 1
	p.dirty = true
	// The container is final now: its parent holds its value from here on.
	closed := p.copyFrame(frame, nil)
	p.replace(frame.slot, closed)
	// Too deep to report inside: the whole value, final now that it is closed.
	if frame.hasPath && !frame.reportsInside {
		p.report(JSONChange{Kind: JSONChangeSet, Path: frame.path, Value: closed.ord})
	}
	p.valueEnded(p.top())
}

// valueEnded ends a value in frame, or the root value when frame is nil.
func (p *StreamingJSONParser) valueEnded(frame *jsonFrame) {
	if frame == nil {
		// Text after the root value is ignored, but only once a root value exists.
		if p.hasRoot {
			p.done = true
		}
		return
	}
	frame.phase = phaseComma
}

// place sets the value at slot in the open path; nothing for a value in an
// invalid position.
func (p *StreamingJSONParser) place(slot *jsonSlot, value jsonNode) {
	if slot == nil {
		return
	}
	p.dirty = true
	if slot.kind == slotRoot {
		p.root, p.hasRoot = value, true
		return
	}
	frame := p.top()
	if slot.kind == slotElement {
		if slot.index == len(frame.elems) {
			frame.size++
			p.pathCost++
			frame.elems = append(frame.elems, value)
			return
		}
		frame.elems[slot.index] = value
		return
	}
	if _, exists := frame.members[slot.key]; !exists {
		frame.size++
		p.pathCost++
		frame.addKey(slot.key)
	}
	frame.members[slot.key] = value
}

// replace swaps the closed value of the container that sat at slot into its
// parent (the innermost open container now) or the root, where the open
// container stood; nothing for a container in an invalid position.
func (p *StreamingJSONParser) replace(slot *jsonSlot, value jsonNode) {
	switch {
	case slot == nil:
	case slot.kind == slotRoot:
		p.root = value
	case slot.kind == slotElement:
		p.top().elems[slot.index] = value
	default:
		p.top().members[slot.key] = value
	}
}

// addKey records a new member key in the order a JS object lists it.
func (f *jsonFrame) addKey(key string) {
	if n, ok := jstext.ArrayIndexKey(key); ok {
		if len(f.indexKeys) > 0 && f.indexKeys[len(f.indexKeys)-1].n > n {
			f.indexUnsorted = true
		}
		f.indexKeys = append(f.indexKeys, indexKey{n: n, key: key})
		return
	}
	f.nameKeys = append(f.nameKeys, key)
}

// sortedIndexKeys is the index keys, ascending. Sorting when a copy needs
// them, not on every insertion, keeps keys that arrive in descending order
// from costing a shift of every key read so far.
func (f *jsonFrame) sortedIndexKeys() []indexKey {
	if f.indexUnsorted {
		slices.SortFunc(f.indexKeys, func(a, b indexKey) int { return cmp.Compare(a.n, b.n) })
		f.indexUnsorted = false
	}
	return f.indexKeys
}

// copyOpenPath makes a fresh snapshot: copies of the open containers,
// innermost first, each holding the copy of its open child.
func (p *StreamingJSONParser) copyOpenPath(pending pendingScalar, hasPending bool) (jsonNode, bool) {
	if len(p.stack) == 0 {
		if hasPending && pending.slot.kind == slotRoot {
			return scalarNode(pending.value), true
		}
		return p.root, p.hasRoot
	}
	var child jsonNode
	var childSlot *jsonSlot
	for depth := len(p.stack) - 1; depth >= 0; depth-- {
		frame := p.stack[depth]
		var overrides []slotValue
		if depth == len(p.stack)-1 && hasPending {
			overrides = append(overrides, slotValue{pending.slot, scalarNode(pending.value)})
		}
		if depth < len(p.stack)-1 && childSlot != nil {
			overrides = append(overrides, slotValue{childSlot, child})
		}
		child = p.copyFrame(frame, overrides)
		childSlot = frame.slot
	}
	// The outermost open container is the root unless it sits in an invalid
	// position.
	if childSlot != nil && childSlot.kind == slotRoot {
		return child, true
	}
	return p.root, p.hasRoot
}

type slotValue struct {
	slot  *jsonSlot
	value jsonNode
}

// copyFrame copies the container of frame, with each override set at its
// slot as JS sets it on the copy (`copy[key] = value`).
func (p *StreamingJSONParser) copyFrame(frame *jsonFrame, overrides []slotValue) jsonNode {
	id := containerIDs.Add(1)
	if frame.isArray {
		n := len(frame.elems)
		for _, o := range overrides {
			n = max(n, o.slot.index+1)
		}
		elem := func(i int) jsonNode {
			for _, o := range overrides {
				if o.slot.index == i {
					return o.value
				}
			}
			return frame.elems[i]
		}
		ord, plain := make([]any, n), make([]any, n)
		nonFinite := false
		for i := range n {
			e := elem(i)
			ord[i], plain[i] = e.ord, e.plain
			nonFinite = nonFinite || e.nonFinite
		}
		node := jsonNode{ord: ord, plain: plain, id: id, nonFinite: nonFinite}
		if nonFinite {
			tool := make([]any, n)
			for i := range n {
				tool[i] = elem(i).toolOrd()
			}
			node.tool = tool
		}
		return node
	}
	indexKeys, nameKeys := frame.sortedIndexKeys(), frame.nameKeys
	for _, o := range overrides {
		if _, exists := frame.members[o.slot.key]; exists {
			continue
		}
		// A new key: the pending scalar's member, which the working container
		// does not hold yet.
		if n, ok := jstext.ArrayIndexKey(o.slot.key); ok {
			at, _ := slices.BinarySearchFunc(indexKeys, n, func(k indexKey, target uint32) int { return cmp.Compare(k.n, target) })
			indexKeys = slices.Insert(slices.Clip(indexKeys), at, indexKey{n: n, key: o.slot.key})
		} else {
			nameKeys = append(slices.Clip(nameKeys), o.slot.key)
		}
	}
	value := func(key string) jsonNode {
		for _, o := range overrides {
			if o.slot.key == key {
				return o.value
			}
		}
		return frame.members[key]
	}
	size := len(indexKeys) + len(nameKeys)
	ord := make(OrderedObject, 0, size)
	plain := make(map[string]any, size)
	nonFinite := false
	add := func(key string) {
		v := value(key)
		ord = append(ord, OrderedField{Key: key, Value: v.ord})
		plain[key] = v.plain
		nonFinite = nonFinite || v.nonFinite
	}
	for _, k := range indexKeys {
		add(k.key)
	}
	for _, key := range nameKeys {
		add(key)
	}
	node := jsonNode{ord: ord, plain: plain, id: id, nonFinite: nonFinite}
	if nonFinite {
		tool := make(OrderedObject, size)
		for i, f := range ord {
			tool[i] = OrderedField{Key: f.Key, Value: value(f.Key).toolOrd()}
		}
		node.tool = tool
	}
	return node
}

// scalarNode is a number, string, boolean or null: a number past float64's
// range as ±Inf in ord and nil in the tool-facing forms.
func scalarNode(v any) jsonNode {
	if f, ok := v.(float64); ok && math.IsInf(f, 0) {
		return jsonNode{ord: f, nonFinite: true}
	}
	return jsonNode{ord: v, plain: v}
}

// sameSnapshot is `a === b` for two snapshots: a container is itself only,
// a scalar any equal scalar, and no value (undefined) only itself.
func sameSnapshot(a jsonNode, hasA bool, b jsonNode, hasB bool) bool {
	if !hasA || !hasB {
		return hasA == hasB
	}
	if a.id != 0 || b.id != 0 {
		return a.id == b.id
	}
	return a.ord == b.ord
}

func isJSONDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', ':', '{', '}', '[', ']', '"':
		return true
	}
	return false
}

func hexValue(h byte) byte {
	switch {
	case h <= '9':
		return h - '0'
	case h >= 'a':
		return h - 'a' + 10
	default:
		return h - 'A' + 10
	}
}

// scalarValue is the value of a complete number or literal token; false for
// anything else, which is dropped.
func (p *StreamingJSONParser) scalarValue(token string) (any, bool) {
	for _, l := range jsonLiterals {
		if token == l.text {
			return l.value, true
		}
	}
	if jsonNumber.MatchString(token) {
		return p.number(token), true
	}
	return nil, false
}

// pendingScalarValue is the value an unterminated number or literal token
// shows.
func (p *StreamingJSONParser) pendingScalarValue(token string) (any, bool) {
	for _, l := range jsonLiterals {
		if strings.HasPrefix(l.text, token) {
			return l.value, true
		}
	}
	if jsonNumber.MatchString(token) {
		return p.number(token), true
	}
	if m := jsonNumberWithoutExpDigits.FindStringSubmatch(token); m != nil {
		return p.number(m[1]), true
	}
	return nil, false
}

// number is Number(token) for a token that is a JSON number: the nearest
// float64, ±Inf past its range.
func (p *StreamingJSONParser) number(token string) float64 {
	return jstext.StringToNumber(token)
}

// appendUTF16 appends text to b as JS `+` joins it (jstext.ConcatCodeUnits)
// and returns the
// builder that holds the result. Joining rewrites the end of the text, which
// a strings.Builder cannot do in place, so it starts a new one: the old one's
// strings stay as they were handed out. A nil b is an empty text.
func appendUTF16(b *strings.Builder, text string) *strings.Builder {
	if b == nil {
		b = &strings.Builder{}
	}
	s := b.String()
	if r, ok := jstext.JoinsSurrogates(s, text); ok {
		fresh := &strings.Builder{}
		fresh.Grow(len(s) + len(text) + 1)
		fresh.WriteString(s[:len(s)-3])
		fresh.WriteRune(r)
		fresh.WriteString(text[3:])
		return fresh
	}
	b.WriteString(text)
	return b
}

// ToolCallArgumentsStream is pi's ToolCallArgumentsStream: the arguments of
// one streaming tool call — the raw text, for the one full parse when the
// call ends, and a StreamingJSONParser for the partial arguments in between.
// Providers keep one per call in their own stream state, never on the
// content block, so the raw text never reaches partial messages or
// transcripts. The zero value is ready to use. A stream is not safe for
// concurrent use, and the arguments it returns share their closed members
// with later partial arguments: they are read-only.
type ToolCallArgumentsStream struct {
	text    *strings.Builder
	parser  *StreamingJSONParser
	snap    jsonNode
	hasSnap bool
	// args and order are the partial arguments last returned; nil args is
	// pi's initial {}.
	args  map[string]any
	order OrderedObject
}

// Text returns the raw argument text received so far.
func (s *ToolCallArgumentsStream) Text() string { return builderText(s.text) }

// builderText is the text of b; "" for a nil b.
func builderText(b *strings.Builder) string {
	if b == nil {
		return ""
	}
	return b.String()
}

// Append appends a piece of argument text and returns the partial arguments
// (ToolCall.Arguments and ToolCall.ArgumentsOrder): new values whenever they
// changed, the previous ones otherwise. They are never changed afterwards.
func (s *ToolCallArgumentsStream) Append(delta string) (map[string]any, OrderedObject) {
	s.text = appendUTF16(s.text, delta)
	if s.parser == nil {
		s.parser = &StreamingJSONParser{}
	}
	s.parser.Append(delta)
	return s.partial()
}

// Replace takes the provider's complete argument text. When it continues the
// text so far, it returns the missing piece as delta (hasDelta true);
// otherwise it starts over with text. Either way it returns the partial
// arguments of text.
func (s *ToolCallArgumentsStream) Replace(text string) (delta string, hasDelta bool, args map[string]any, order OrderedObject) {
	if current := s.Text(); strings.HasPrefix(text, current) {
		delta = text[len(current):]
		if delta == "" {
			args, order = s.partial()
		} else {
			args, order = s.Append(delta)
		}
		return delta, true, args, order
	}
	s.text = nil
	s.parser = &StreamingJSONParser{}
	args, order = s.Append(text)
	return "", false, args, order
}

// Finish returns the final arguments: one full parse of the complete text
// (ParseToolCallArguments), which reports malformed arguments in its third
// return, the call's ArgumentsError.
func (s *ToolCallArgumentsStream) Finish() (map[string]any, OrderedObject, string) {
	return ParseToolCallArguments(s.Text())
}

func (s *ToolCallArgumentsStream) partial() (map[string]any, OrderedObject) {
	if s.parser == nil {
		s.parser = &StreamingJSONParser{}
	}
	if s.args == nil {
		s.args = map[string]any{}
	}
	snapshot, ok := s.parser.snapshot()
	if sameSnapshot(snapshot, ok, s.snap, s.hasSnap) {
		return s.args, s.order
	}
	s.snap, s.hasSnap = snapshot, ok
	// Tool arguments are an object; any other partial value shows as no
	// arguments yet.
	if _, isObject := snapshot.ord.(OrderedObject); !ok || !isObject {
		s.args, s.order = map[string]any{}, nil
		return s.args, s.order
	}
	// A number past float64's range is null in both forms, as JSON.stringify
	// writes pi's Infinity (ParseToolCallArguments holds it so too).
	s.args, s.order = snapshot.plain.(map[string]any), snapshot.toolOrd().(OrderedObject)
	return s.args, s.order
}
