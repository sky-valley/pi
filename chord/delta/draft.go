package delta

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sky-valley/pi/chord/internal/jsonvalue"
	"github.com/sky-valley/pi/internal/jstext"
)

// ─── Drafts ──────────────────────────────────────────────────────────────────
//
// Upstream's delta/tracker.ts: a change's draft is an overlay on the committed
// revision. Every container the draft reads gets an overlay node, which records
// the writes, deletions and structural edits made through it and leaves the
// revision untouched; preparing the change emits the ops those records mean,
// directly, without diffing. Upstream hands out a Proxy per node; Go has no
// Proxy, so a Draft is a handle to one node and its methods are the traps (see
// doc.go for the mapping).
//
// A node belongs to one container of one slot: an object's member, an entry of
// the base array, or an entry a change inserted. Reading a member twice yields
// the same Draft, and a held Draft follows its entry through sorting, reversal
// and insertion — or out of the tree, after which its writes are ignored.

// parentKind is where a node's container sits in its parent.
type parentKind uint8

const (
	objectEntry parentKind = iota // a member of an object
	baseEntry                     // an entry of the base array
	insertEntry                   // an entry the change inserted
)

// node is upstream's OverlayNode.
type node struct {
	ctx    *overlayContext
	base   any // map[string]any or []any
	parent *node
	kind   parentKind
	key    string        // objectEntry: the member's key
	index  int           // baseEntry, insertEntry: the entry's source index
	source *insertSource // insertEntry: the insertion it came from
	// placement is upstream's parentPlacement: the container was placed by
	// this change (written or inserted) rather than read from the revision, so
	// its content publishes with the op that placed it.
	placement bool
	draft     *Draft

	// An object's overlay: written members, deleted ones, and base members
	// deleted and written again (which must be deleted on the replica first, so
	// that its key order follows the draft's).
	writes  orderedMap[string, any]
	deletes orderedMap[string, struct{}]
	readded orderedMap[string, struct{}]
	// appends holds the members whose only changes in the change are
	// AppendText calls, with the text appended so far: Prepare publishes each
	// as one append op without comparing the old and new strings.
	appends map[string]string

	array *arrayOverlay

	// The nodes of containers read from the revision, by slot: those are
	// found again by where they are, since the revision never changes under
	// them. A placed container is found by identity (overlayContext.placed).
	keyChildren   map[string]*node
	indexChildren map[int]*node

	dirty, subtreeDirty bool
	path                Path
	resolved            bool
}

// slot is where a container sits in a node: a member key, or an entry's
// source index and insertion.
type slot struct {
	kind   parentKind
	key    string
	index  int
	source *insertSource
}

func (n *node) isArray() bool {
	_, ok := n.base.([]any)
	return ok
}

func (n *node) object() map[string]any { return n.base.(map[string]any) }
func (n *node) elements() []any        { return n.base.([]any) }

// childFor is upstream's createNode for a member container: the node the
// container already has, or a new one.
func (n *node) childFor(s slot, value any, placement bool) *node {
	if c := n.existingChild(s, value, placement); c != nil {
		return c
	}
	c := n.ctx.newNode(value, n, s, placement)
	if placement {
		id, _ := identityOf(value)
		n.ctx.placed[id] = c
	} else if s.kind == objectEntry {
		if n.keyChildren == nil {
			n.keyChildren = map[string]*node{}
		}
		n.keyChildren[s.key] = c
	} else {
		if n.indexChildren == nil {
			n.indexChildren = map[int]*node{}
		}
		n.indexChildren[s.index] = c
	}
	return c
}

// existingChild is upstream's rawNodes lookup: the node of the container a
// slot holds, if one was made.
func (n *node) existingChild(s slot, value any, placement bool) *node {
	if placement {
		id, ok := identityOf(value)
		if !ok {
			return nil
		}
		return n.ctx.placed[id]
	}
	if s.kind == objectEntry {
		return n.keyChildren[s.key]
	}
	return n.indexChildren[s.index]
}

// holds reports whether the slot child sits in still holds child's container:
// a member not written over or deleted since, or an entry not overridden since
// — or, for a placed container, the same one.
func (n *node) holds(child *node, value any) bool {
	if !child.placement {
		switch child.kind {
		case objectEntry:
			return !n.hasWrite(child.key)
		case baseEntry:
			return !n.array.baseOverrides.has(child.index)
		}
		return false
	}
	return same(value, child.base)
}

// ─── Objects ─────────────────────────────────────────────────────────────────

func (n *node) hasWrite(key string) bool  { return n.writes.has(key) }
func (n *node) isDeleted(key string) bool { return n.deletes.has(key) }

// objectHas is upstream's: a member not deleted, and written or the base's own.
func (n *node) objectHas(key string) bool {
	if n.isDeleted(key) {
		return false
	}
	if n.hasWrite(key) {
		return true
	}
	_, ok := n.object()[key]
	return ok
}

// objectValue is upstream's: the member as written, else as the base holds it
// (even when deleted). ok is false when neither has it.
func (n *node) objectValue(key string) (any, bool) {
	if v, ok := n.writes.get(key); ok {
		return v, true
	}
	v, ok := n.object()[key]
	return v, ok
}

func (n *node) setWrite(key string, value any) { n.writes.set(key, value) }
func (n *node) setDeletion(key string)         { n.deletes.set(key, struct{}{}) }

// ownKeys is upstream's ownKeys trap: an object's members, integer-like first
// (ascending), then the rest in order — the base's (in keyOrder, where pi
// follows insertion order: D69), then the members this change added, in the
// order it added them.
func (n *node) ownKeys() []string {
	base := n.object()
	existingOnly := n.deletes.len() == 0 && n.readded.len() == 0
	if existingOnly {
		for k := range n.writes.keys() {
			if _, ok := base[k]; !ok {
				existingOnly = false
				break
			}
		}
	}
	if existingOnly {
		return keyOrder(base)
	}
	keys := make([]string, 0, len(base)+n.writes.len())
	seen := make(map[string]bool, len(base)+n.writes.len())
	for _, k := range keyOrder(base) {
		if !n.isDeleted(k) && !n.readded.has(k) {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for k := range n.writes.keys() {
		if !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var indices []int64
	strs := make([]string, 0, len(keys))
	for _, k := range keys {
		if i, ok := canonicalIndex(k); ok {
			indices = append(indices, i)
		} else {
			strs = append(strs, k)
		}
	}
	slices.Sort(indices)
	out := make([]string, 0, len(keys))
	for _, i := range indices {
		out = append(out, strconv.FormatInt(i, 10))
	}
	return append(out, strs...)
}

// objectLen is how many members ownKeys lists, counted without listing them:
// the base's own less those deleted, and the written ones the base lacks (a
// readded member is the base's, counted once).
func (n *node) objectLen() int {
	base := n.object()
	count := len(base)
	for k := range n.deletes.keys() {
		if _, own := base[k]; own {
			count--
		}
	}
	for k := range n.writes.keys() {
		if _, own := base[k]; !own {
			count++
		}
	}
	return count
}

// members yields an object's members as the draft holds them — each key, its
// value, and whether the change wrote it — in no particular order: for a copy
// into a Go map, which has none, where ownKeys would sort them for nothing.
func (n *node) members(yield func(key string, value any, written bool) bool) {
	base := n.object()
	for k, v := range base {
		if n.isDeleted(k) {
			continue
		}
		if w, ok := n.writes.get(k); ok {
			if !yield(k, w, true) {
				return
			}
			continue
		}
		if !yield(k, v, false) {
			return
		}
	}
	for k, w := range n.writes.all() {
		if _, own := base[k]; !own {
			if !yield(k, w, true) {
				return
			}
		}
	}
}

// markDirty is upstream's: the node joins the change's dirty list, once, and
// every ancestor learns that something below it changed.
func (n *node) markDirty() {
	if n.dirty {
		return
	}
	n.dirty = true
	n.ctx.dirty = append(n.ctx.dirty, n)
	for p := n.parent; p != nil; p = p.parent {
		p.subtreeDirty = true
	}
}

// ─── Arrays ──────────────────────────────────────────────────────────────────

func (n *node) arrayOverlay() *arrayOverlay {
	if n.array == nil {
		n.array = newArrayOverlay(len(n.elements()))
	}
	return n.array
}

// entryValue is upstream's entryValueAt: the entry as overridden, else as its
// base or insertion holds it.
func (n *node) entryValue(p *piece, sourceIndex int) any {
	a := n.array
	if p.base() {
		if v, ok := a.baseOverrides.get(sourceIndex); ok {
			return v
		}
		return n.elements()[sourceIndex]
	}
	if v, ok := a.insertOverrides[p.source][sourceIndex]; ok {
		return v
	}
	return p.source.refs[sourceIndex]
}

func (n *node) hasEntryOverride(p *piece, sourceIndex int) bool {
	if p.base() {
		return n.array.baseOverrides.has(sourceIndex)
	}
	_, ok := n.array.insertOverrides[p.source][sourceIndex]
	return ok
}

func entrySlot(p *piece, sourceIndex int) slot {
	if p.base() {
		return slot{kind: baseEntry, index: sourceIndex}
	}
	return slot{kind: insertEntry, index: sourceIndex, source: p.source}
}

// entry is upstream's getArrayIndex: the element at a logical index, a Draft
// for a container. nil past the end, which callers check first.
func (n *node) entry(index int) any {
	a := n.arrayOverlay()
	if index >= a.length() {
		return nil
	}
	p, offset := a.locate(index)
	sourceIndex := p.at(offset)
	v := n.entryValue(p, sourceIndex)
	if !isContainer(v) {
		return v
	}
	return n.childFor(entrySlot(p, sourceIndex), v, !p.base() || n.hasEntryOverride(p, sourceIndex)).draft
}

// replacePieceRange is upstream's: remove `remove` entries at index and insert
// the pieces there. An append of one fresh insertion to an array ending in the
// tail of another extends that insertion instead of adding a piece.
func (n *node) replacePieceRange(index, remove int, inserted []*piece) {
	if remove == 0 && len(inserted) == 0 {
		return
	}
	a := n.arrayOverlay()
	if remove == 0 && index == a.length() && len(inserted) == 1 && a.root != nil {
		addition := inserted[0]
		tail := rightmost(a.root).piece
		if !addition.base() && !tail.base() && tail.step == 1 && tail.start+tail.length == len(tail.source.refs) {
			for offset := range addition.length {
				tail.source.refs = append(tail.source.refs, addition.source.refs[addition.at(offset)])
			}
			extendRightmost(a.root, addition.length)
			a.invalidateCaches()
			a.structural = true
			a.generation++
			n.markDirty()
			return
		}
	}
	left, rest := a.split(a.root, index)
	_, right := a.split(rest, remove)
	middle := a.treeFrom(inserted)
	a.root = a.joinNormalized(a.joinNormalized(left, middle), right)
	a.invalidateCaches()
	a.structural = true
	a.generation++
	n.markDirty()
}

// insertPiece is upstream's: one piece of a new insertion holding values,
// which are already the change's own.
func insertPiece(values []any) []*piece {
	if len(values) == 0 {
		return nil
	}
	return []*piece{{source: &insertSource{refs: values}, start: 0, length: len(values), step: 1}}
}

// insertPlacementPiece is upstream's: items copied and checked, every one
// before any is inserted, as one new insertion.
func insertPlacementPiece(items []any) ([]*piece, error) {
	values := make([]any, len(items))
	for i, item := range items {
		v, err := clonePlacement(item)
		if err != nil {
			return nil, err
		}
		values[i] = v
	}
	return insertPiece(values), nil
}

// setEntry is upstream's setArrayIndex: an index up to the length, stored
// already copied. Writing the value an entry already has changes nothing, and
// writing a base entry's own scalar back drops its override.
func (n *node) setEntry(index int, stored any) {
	a := n.arrayOverlay()
	length := a.length()
	if index == length {
		n.replacePieceRange(length, 0, insertPiece([]any{stored}))
		return
	}
	p, offset := a.locate(index)
	sourceIndex := p.at(offset)
	current := n.entryValue(p, sourceIndex)
	if !isContainer(stored) && same(current, stored) {
		return
	}
	if p.base() {
		if !isContainer(stored) && same(stored, n.elements()[sourceIndex]) {
			a.baseOverrides.delete(sourceIndex)
		} else {
			a.baseOverrides.set(sourceIndex, stored)
		}
	} else {
		overrides := a.insertOverrides[p.source]
		if !isContainer(stored) && same(stored, p.source.refs[sourceIndex]) {
			delete(overrides, sourceIndex)
			if overrides != nil && len(overrides) == 0 {
				delete(a.insertOverrides, p.source)
			}
		} else {
			if overrides == nil {
				if a.insertOverrides == nil {
					a.insertOverrides = map[*insertSource]map[int]any{}
				}
				overrides = map[int]any{}
				a.insertOverrides[p.source] = overrides
			}
			overrides[sourceIndex] = stored
		}
	}
	n.markDirty()
}

// setLength is upstream's setArrayLength: shrinking removes entries, growing
// inserts nulls.
func (n *node) setLength(next int) {
	current := n.arrayOverlay().length()
	switch {
	case next == current:
	case next < current:
		n.replacePieceRange(next, current-next, nil)
	default:
		n.replacePieceRange(current, 0, insertPiece(make([]any, next-current)))
	}
}

// ─── Draft ───────────────────────────────────────────────────────────────────

// Draft is a mutable view of one object or array inside a change: the Go form
// of upstream's Draft<T> proxy. Change.State is the root; Get and At hand out
// the drafts of member containers, one handle per container, so a handle may
// be held and compared.
//
// Writes go to the change's overlay, never to the committed revision, and every
// value written is checked and deep-copied at once: a later change to the
// caller's value, or to a draft it was read from, does not reach it.
//
// Keys are given as any: a string is an object key, an int (or any integral Go
// number) an array index, and a Key or Index is taken as is. On an array a
// canonical numeric string is the index it spells; on an object an index is
// the key it spells — as JavaScript coerces both.
//
// A draft is usable until its change settles: it is prepared or aborted, or
// the tracker adopts another change first. After that a write returns
// ErrDraftSettled and a read panics with it: a read has no error to return,
// and using a settled draft is a bug in the caller, like a send on a closed
// channel. A nil *Draft — what At returns for a member that is not a container
// — reads as empty, and every write to it returns an error; so does a zero
// Draft, which no change handed out.
//
// A Draft is not safe for concurrent use.
type Draft struct{ n *node }

var (
	// ErrDraftSettled is returned by a write through a draft whose change has
	// settled, and is the value a read through one panics with.
	ErrDraftSettled = errors.New("delta: Cannot use a settled overlay (its change was prepared or aborted, or the tracker adopted a competing change; begin a new change and read the draft from its State)")
	// errReadOnly is upstream's assertWritable text for a change being
	// prepared, which no draft method can observe from Go.
	errReadOnly = errors.New("delta: Prepared overlays are read-only (the change is being prepared; begin a new change to write again)")
	errNilDraft = errors.New("delta: no draft here: the member is absent or not an object or array (check At's result, or Set the member first)")
	// errArrayHole is upstream's TypeError for a write that would leave an
	// array with a hole: deleting an element, or writing past the next index.
	errArrayHole = errors.New("delta: Overlay arrays cannot contain holes (write at an index up to the array's length, and remove elements with Splice, Pop or Shift rather than Delete)")
	// errArrayProperty is upstream's TypeError for a named property written on
	// an array, which a JSON array cannot hold.
	errArrayProperty = errors.New("delta: Only array indices and length can be written (an array draft holds elements at indices 0..length-1; write named members on an object draft)")
)

// readable is the node behind a draft being read, or why it cannot be.
func (d *Draft) readable() (*node, error) {
	if d == nil || d.n == nil {
		return nil, errNilDraft
	}
	if d.n.ctx.settled() {
		return nil, ErrDraftSettled
	}
	return d.n, nil
}

// live is the node behind a draft that may be written, or why it may not.
func (d *Draft) live() (*node, error) {
	n, err := d.readable()
	if err != nil {
		return nil, err
	}
	if n.ctx.status.value != statusOpen {
		return nil, errReadOnly
	}
	return n, nil
}

// read is the node behind a draft being read: nil for a nil or zero draft; a
// panic for a settled one.
func (d *Draft) read() *node {
	if d == nil || d.n == nil {
		return nil
	}
	if d.n.ctx.settled() {
		panic(ErrDraftSettled)
	}
	return d.n
}

// IsArray reports whether the draft is an array.
func (d *Draft) IsArray() bool {
	n := d.read()
	return n != nil && n.isArray()
}

// Len is an array's length or an object's member count.
func (d *Draft) Len() int {
	n := d.read()
	switch {
	case n == nil:
		return 0
	case n.isArray():
		return n.arrayOverlay().length()
	}
	return n.objectLen()
}

// Keys is Object.keys: an array's indices, as strings, or an object's members
// in the order ownKeys enumerates them.
func (d *Draft) Keys() []string {
	n := d.read()
	switch {
	case n == nil:
		return nil
	case n.isArray():
		length := n.arrayOverlay().length()
		keys := make([]string, length)
		for i := range keys {
			keys[i] = strconv.Itoa(i)
		}
		return keys
	}
	return n.ownKeys()
}

// Has reports whether the member exists: an object's own member, or an
// array's index below its length or its "length". Array.prototype's methods,
// which JavaScript's `in` also sees on an array, are the Draft's own methods
// here.
func (d *Draft) Has(key any) bool {
	n := d.read()
	if n == nil {
		return false
	}
	seg, err := parseSeg(key)
	if err != nil {
		return false
	}
	if n.isArray() {
		if k, ok := seg.(Key); ok && k == "length" {
			return true
		}
		i, ok := arrayIndexOf(seg)
		return ok && i < int64(n.arrayOverlay().length())
	}
	return n.objectHas(propertyKey(seg))
}

// Get is the member's value and whether it exists: a *Draft for an object or
// array, the value itself otherwise. An array's "length" is its length.
func (d *Draft) Get(key any) (any, bool) {
	n := d.read()
	if n == nil {
		return nil, false
	}
	seg, err := parseSeg(key)
	if err != nil {
		return nil, false
	}
	if n.isArray() {
		a := n.arrayOverlay()
		if k, ok := seg.(Key); ok && k == "length" {
			return float64(a.length()), true
		}
		i, ok := arrayIndexOf(seg)
		if !ok || i >= int64(a.length()) {
			return nil, false
		}
		return n.entry(int(i)), true
	}
	k := propertyKey(seg)
	if !n.objectHas(k) {
		return nil, false
	}
	v, _ := n.objectValue(k)
	if !isContainer(v) {
		return v, true
	}
	return n.childFor(slot{kind: objectEntry, key: k}, v, n.hasWrite(k)).draft, true
}

// At is the draft of a member object or array, or nil when the member is
// absent or a scalar. Writes to a nil draft return an error, so a chain like
// state.At("a").At("b").Set("c", 1) reports a missing link rather than
// panicking.
func (d *Draft) At(key any) *Draft {
	v, _ := d.Get(key)
	child, _ := v.(*Draft)
	return child
}

// Snapshot is copyJson of the draft: a detached strict-JSON copy of what it
// holds now, in the representation a decoded tree has — what pi's copyJson,
// JSON.stringify or structuredClone read through the proxy. chord.CopyJSON
// and chord.IsValue read a draft with it. It fails with ErrDraftSettled once
// the change has settled.
func (d *Draft) Snapshot() (any, error) {
	n, err := d.readable()
	if err != nil {
		return nil, err
	}
	return n.clonePlacementNode()
}

// arrayIndexOf is the index seg names on an array: an Index, or a Key that
// spells one canonically. false for anything else — a named property, which
// a JSON array cannot hold. It is an int64: an index past 2^31 is one on a
// 32-bit build too, past the end of every slice there.
func arrayIndexOf(seg Seg) (int64, bool) {
	switch s := seg.(type) {
	case Index:
		if s >= 0 && s <= maxArrayIndex {
			return int64(s), true
		}
	case Key:
		return canonicalIndex(string(s))
	}
	return 0, false
}

// Set assigns a member: an object property, or an array element at an index
// up to the length (the length itself appends). nil is JSON null; a member is
// removed with Delete. Assigning a scalar member the value it already has
// changes nothing; a container is always copied in, and assigning a *Draft
// copies what that draft holds now.
//
// On an array, "length" is SetLen, with value converted as JavaScript converts
// an assigned length: null is 0, a boolean 0 or 1, a string the number it
// spells, an array the number its elements' string spells ([2] is 2). Any
// other key that is not an index is refused, as is an index past the length.
func (d *Draft) Set(key any, value any) error {
	n, err := d.live()
	if err != nil {
		return err
	}
	seg, err := parseSeg(key)
	if err != nil {
		return err
	}
	if n.isArray() {
		if k, ok := seg.(Key); ok && k == "length" {
			length, err := toArrayLength(value)
			if err != nil {
				return err
			}
			n.setLength(length)
			return nil
		}
		i, ok := arrayIndexOf(seg)
		if !ok {
			return errArrayProperty
		}
		if i > int64(n.arrayOverlay().length()) {
			return errArrayHole
		}
		stored, err := clonePlacement(value)
		if err != nil {
			return err
		}
		n.setEntry(int(i), stored)
		return nil
	}
	k := propertyKey(seg)
	stored, err := clonePlacement(value)
	if err != nil {
		return err
	}
	current, present := n.objectValue(k)
	wasDeleted := n.isDeleted(k)
	if !wasDeleted && present && !isContainer(stored) && same(current, stored) {
		return nil
	}
	delete(n.appends, k)
	n.setWrite(k, stored)
	if _, own := n.object()[k]; wasDeleted && own {
		n.readded.set(k, struct{}{})
	}
	n.deletes.delete(k)
	n.markDirty()
	return nil
}

// AppendText is upstream's appendText: it appends text to the string member
// key, recorded as one append op — several appends to one key in a change
// join into one — that Prepare publishes as it is, without comparing the old
// string with the new. Setting the member to current+text publishes the same
// op, found by checking that the new string extends the old, which reads all
// of it; a Set or Delete of the key in the same change falls back to that
// check. Empty text changes nothing.
//
// Upstream's appendText is O(len(text)) because a V8 string append builds a
// rope. A Go string is flat, so the new value is a copy of the whole string
// here as it is for Set: what AppendText saves in Go is the comparison, not
// the copy. Upstream's StringKeys<T>, which limits the key at compile time to
// members typed exactly string, has no Go counterpart: a member that is not a
// string — absent, a container, an array draft's element — is refused at run
// time, with upstream's text.
func (d *Draft) AppendText(key string, text string) error {
	n, err := d.live()
	if err != nil {
		return err
	}
	current, ok := "", false
	if !n.isArray() && n.objectHas(key) {
		v, _ := n.objectValue(key)
		current, ok = v.(string)
	}
	if !ok {
		quoted, _ := marshalJSON(key)
		return fmt.Errorf("delta: appendText() needs a string at %s (AppendText extends a string member of an object draft; Set the member to a string first)", quoted)
	}
	if text == "" {
		return nil
	}
	// The op and the new value keep the text: a copy, so that neither keeps a
	// larger string the caller cut it from alive ("" + text would be text).
	owned := strings.Clone(text)
	appended, recorded := n.appends[key]
	if !recorded && !n.hasWrite(key) {
		appended, recorded = "", true
	}
	n.setWrite(key, current+owned)
	n.markDirty()
	if recorded {
		if n.appends == nil {
			n.appends = map[string]string{}
		}
		n.appends[key] = appended + owned
	}
	return nil
}

// Delete removes an object member; deleting one that is absent does nothing.
// An array element cannot be deleted — that would leave a hole — so every
// Delete on an array fails; use Splice, Pop or Shift.
func (d *Draft) Delete(key any) error {
	n, err := d.live()
	if err != nil {
		return err
	}
	seg, err := parseSeg(key)
	if err != nil {
		return err
	}
	if n.isArray() {
		return errArrayHole
	}
	k := propertyKey(seg)
	if !n.objectHas(k) {
		return nil
	}
	delete(n.appends, k)
	n.writes.delete(k)
	n.readded.delete(k)
	n.setDeletion(k)
	n.markDirty()
	return nil
}

// SetLen is `array.length = n`: it truncates, or grows with nulls.
func (d *Draft) SetLen(length int) error {
	n, err := d.live()
	if err != nil {
		return err
	}
	if !n.isArray() {
		return errors.New(`delta: SetLen on an object draft (SetLen sets an array's length; to write an object member named "length", use Set)`)
	}
	if length < 0 {
		return errArrayLength(length)
	}
	if int64(length) > maxArrayIndex+1 {
		return errArrayLength(length)
	}
	n.setLength(length)
	return nil
}

// errArrayLength is upstream's RangeError for a length that is not an integer
// from 0 to 2^32 - 1 once JavaScript has converted it.
func errArrayLength(v any) error {
	return fmt.Errorf("delta: Invalid array length: %s (a length is an integer from 0 to 4294967295, or a value JavaScript's Number() makes one)", describe(v))
}

// errArrayTooLong is growing an array past what a slice holds on this
// platform: only a 32-bit build reaches it (D73).
func errArrayTooLong(length float64) error {
	return fmt.Errorf("delta: an array of length %s is longer than a slice holds on this platform (at most %d elements); a 32-bit build cannot hold it, so keep the array shorter or use a 64-bit build", jsNumber(length), math.MaxInt)
}

// toArrayLength is upstream's: Number(v), which must be an integer from 0 to
// 2^32 - 1.
func toArrayLength(v any) (int, error) {
	f, err := jsToNumber(v)
	if err != nil {
		return 0, err
	}
	if !(f >= 0 && f < 1<<32 && f == math.Trunc(f)) {
		return 0, errArrayLength(v)
	}
	if f > math.MaxInt {
		return 0, errArrayTooLong(f)
	}
	return int(f), nil
}

// mutator is the start of every array method: upstream's mutatorNode.
func (d *Draft) mutator(method string) (*node, error) {
	n, err := d.live()
	if err != nil {
		return nil, err
	}
	if !n.isArray() {
		return nil, fmt.Errorf("delta: Array mutator called on incompatible receiver (%s applies to array drafts; this draft is an object)", method)
	}
	return n, nil
}

// Push appends items and returns the new length.
func (d *Draft) Push(items ...any) (int, error) {
	n, err := d.mutator("Push")
	if err != nil {
		return 0, err
	}
	length := n.arrayOverlay().length()
	pieces, err := insertPlacementPiece(items)
	if err != nil {
		return 0, err
	}
	n.replacePieceRange(length, 0, pieces)
	return length + len(items), nil
}

// Pop removes and returns the last element — as a *Draft when it is a
// container, which the removal detaches. ok is false when the array is empty.
func (d *Draft) Pop() (value any, ok bool, err error) {
	n, err := d.mutator("Pop")
	if err != nil {
		return nil, false, err
	}
	length := n.arrayOverlay().length()
	if length == 0 {
		return nil, false, nil
	}
	value = n.entry(length - 1)
	n.replacePieceRange(length-1, 1, nil)
	return value, true, nil
}

// Shift removes and returns the first element — as a *Draft when it is a
// container, which the removal detaches. ok is false when the array is empty.
func (d *Draft) Shift() (value any, ok bool, err error) {
	n, err := d.mutator("Shift")
	if err != nil {
		return nil, false, err
	}
	if n.arrayOverlay().length() == 0 {
		return nil, false, nil
	}
	value = n.entry(0)
	n.replacePieceRange(0, 1, nil)
	return value, true, nil
}

// Unshift inserts items at the front and returns the new length.
func (d *Draft) Unshift(items ...any) (int, error) {
	n, err := d.mutator("Unshift")
	if err != nil {
		return 0, err
	}
	pieces, err := insertPlacementPiece(items)
	if err != nil {
		return 0, err
	}
	n.replacePieceRange(0, 0, pieces)
	return n.arrayOverlay().length(), nil
}

// clampIndex is JavaScript's relative-index rule: negative counts from the
// end, and both directions clamp to [0, length].
func clampIndex(i, length int) int {
	if i < 0 {
		return max(0, length+i)
	}
	return min(i, length)
}

// Splice is Array.prototype.splice(start, deleteCount, ...items): it removes
// deleteCount elements at start and inserts items there, returning the
// removed elements (containers as drafts, detached from the tree). A negative
// start counts from the end; both bounds are clamped to the array.
func (d *Draft) Splice(start, deleteCount int, items ...any) ([]any, error) {
	n, err := d.mutator("Splice")
	if err != nil {
		return nil, err
	}
	length := n.arrayOverlay().length()
	start = clampIndex(start, length)
	remove := min(max(deleteCount, 0), length-start)
	removed := make([]any, remove)
	for offset := range removed {
		removed[offset] = n.entry(start + offset)
	}
	pieces, err := insertPlacementPiece(items)
	if err != nil {
		return nil, err
	}
	// Upstream then sets the length to length - remove + len(items), which the
	// splice has already made it.
	n.replacePieceRange(start, remove, pieces)
	return removed, nil
}

// Reverse reverses the array in place.
func (d *Draft) Reverse() error {
	n, err := d.mutator("Reverse")
	if err != nil {
		return err
	}
	a := n.arrayOverlay()
	if a.length() < 2 {
		return nil
	}
	pieces := slices.Clone(a.piecesOf())
	slices.Reverse(pieces)
	for _, p := range pieces {
		p.start = p.at(p.length - 1)
		p.step = -p.step
	}
	a.replaceAllPieces(pieces)
	a.structural = true
	a.generation++
	a.plan = nil
	n.markDirty()
	return nil
}

// Fill is Array.prototype.fill(value, start, end): each slot in [start, end)
// gets its own copy of value. Negative bounds count from the end; both clamp.
// Pass Len() as end to fill to the end. An empty range checks nothing.
func (d *Draft) Fill(value any, start, end int) error {
	n, err := d.mutator("Fill")
	if err != nil {
		return err
	}
	length := n.arrayOverlay().length()
	start, end = clampIndex(start, length), clampIndex(end, length)
	if end <= start {
		return nil
	}
	items := make([]any, end-start)
	for i := range items {
		v, err := clonePlacement(value)
		if err != nil {
			return err
		}
		items[i] = v
	}
	n.replacePieceRange(start, end-start, insertPiece(items))
	return nil
}

// CopyWithin is Array.prototype.copyWithin(target, start, end): each element
// of [start, end) is copied — as the draft holds it now — to the slot at the
// same offset from target. Negative bounds count from the end; all clamp.
// Pass Len() as end to copy to the end.
func (d *Draft) CopyWithin(target, start, end int) error {
	n, err := d.mutator("CopyWithin")
	if err != nil {
		return err
	}
	length := n.arrayOverlay().length()
	target, start, end = clampIndex(target, length), clampIndex(start, length), clampIndex(end, length)
	count := min(max(end-start, 0), length-target)
	values := make([]any, count)
	for offset := range values {
		v, err := clonePlacement(n.entry(start + offset))
		if err != nil {
			return err
		}
		values[offset] = v
	}
	n.replacePieceRange(target, count, insertPiece(values))
	return nil
}

// ─── Sort ────────────────────────────────────────────────────────────────────

// sortEntry is one entry of an array being sorted: where it comes from — a base
// index (source nil) or an insertion's — what the comparator sees of it, and,
// for the default order, its String().
type sortEntry struct {
	source *insertSource
	index  int
	value  any
	key    string
}

// Sort sorts the array in place, stably. cmp receives the elements as Get
// returns them — containers as drafts, which it may read and even write. A nil
// cmp is JavaScript's default order: by each element's String(), in UTF-16
// code units; it fails, leaving the array as it was, when an element has no
// string form (an object with a "toString" member — see jsString). Values the
// comparator writes over elements are put back afterwards, as upstream's sort
// restores its overrides; writes inside the elements stay.
func (d *Draft) Sort(cmp func(a, b any) int) error {
	n, err := d.mutator("Sort")
	if err != nil {
		return err
	}
	a := n.arrayOverlay()
	entries := make([]sortEntry, 0, a.length())
	for _, p := range a.piecesOf() {
		for offset := range p.length {
			e := sortEntry{source: p.source, index: p.at(offset)}
			e.value = n.sortValue(e)
			entries = append(entries, e)
		}
	}
	baseSnapshot := map[int]any{}
	for i, v := range a.baseOverrides.all() {
		baseSnapshot[i] = v
	}
	insertSnapshots := map[*insertSource]map[int]any{}
	for source, overrides := range a.insertOverrides {
		insertSnapshots[source] = maps.Clone(overrides)
	}
	generation := a.generation
	if cmp != nil {
		slices.SortStableFunc(entries, func(l, r sortEntry) int { return cmp(l.value, r.value) })
		// A comparator that prepares or aborts the change, or adopts a
		// competitor, settles it: upstream leaves such a comparator
		// unspecified, and the overlay it would go on to write is gone.
		if n.ctx.settled() {
			return ErrDraftSettled
		}
	} else if len(entries) > 1 {
		// Each String() is taken once, before anything moves: the default
		// order has no side effects, and with two or more elements V8 compares
		// every one, so an element without a string form fails the sort either
		// way (D74).
		for i := range entries {
			if entries[i].key, err = jsString(entries[i].value); err != nil {
				return err
			}
		}
		slices.SortStableFunc(entries, func(l, r sortEntry) int { return jstext.CompareUTF16(l.key, r.key) })
	}
	if len(baseSnapshot) > 0 || len(insertSnapshots) > 0 || a.baseOverrides.len() > 0 || len(a.insertOverrides) > 0 {
		for _, e := range entries {
			n.restoreSortOverride(e, baseSnapshot, insertSnapshots)
		}
	}
	comparatorWasStructural := a.generation != generation
	currentLength := a.length()
	samePrefix := currentLength >= len(entries)
	for i := 0; samePrefix && i < len(entries); i++ {
		samePrefix = a.holdsEntryAt(i, entries[i])
	}
	if !samePrefix {
		n.replacePieceRange(0, min(len(entries), currentLength), piecesFromSortOrder(entries))
	}
	if comparatorWasStructural {
		n.deduplicateEntries()
	}
	return nil
}

// sortValue is upstream's publicSortValue: an entry as the comparator sees it.
func (n *node) sortValue(e sortEntry) any {
	a := n.array
	var v any
	var overridden bool
	if e.source == nil {
		v, overridden = a.baseOverrides.get(e.index)
		if !overridden {
			v = n.elements()[e.index]
		}
	} else {
		v, overridden = a.insertOverrides[e.source][e.index]
		if !overridden {
			v = e.source.refs[e.index]
		}
	}
	if !isContainer(v) {
		return v
	}
	s := slot{kind: baseEntry, index: e.index}
	if e.source != nil {
		s = slot{kind: insertEntry, index: e.index, source: e.source}
	}
	return n.childFor(s, v, e.source != nil || a.baseOverrides.has(e.index)).draft
}

// restoreSortOverride is upstream's: an entry's override as it was before the
// comparator ran — restored, or dropped if it had none.
func (n *node) restoreSortOverride(e sortEntry, baseSnapshot map[int]any, insertSnapshots map[*insertSource]map[int]any) {
	a := n.array
	if e.source == nil {
		if v, ok := baseSnapshot[e.index]; ok {
			a.baseOverrides.set(e.index, v)
		} else {
			a.baseOverrides.delete(e.index)
		}
		return
	}
	overrides := a.insertOverrides[e.source]
	if v, ok := insertSnapshots[e.source][e.index]; ok {
		if overrides == nil {
			if a.insertOverrides == nil {
				a.insertOverrides = map[*insertSource]map[int]any{}
			}
			overrides = map[int]any{}
			a.insertOverrides[e.source] = overrides
		}
		overrides[e.index] = v
		return
	}
	delete(overrides, e.index)
	if overrides != nil && len(overrides) == 0 {
		delete(a.insertOverrides, e.source)
	}
}

// holdsEntryAt is upstream's sameSortTokenAt: whether the logical index
// already holds the entry.
func (a *arrayOverlay) holdsEntryAt(logicalIndex int, e sortEntry) bool {
	p, offset := a.locate(logicalIndex)
	return p.at(offset) == e.index && p.source == e.source
}

// piecesFromSortOrder is upstream's: the sorted entries as pieces, runs merged.
// Unlike appendMerged, a run of several entries is extended by an entry that
// continues it in its own step, whichever that is.
func piecesFromSortOrder(entries []sortEntry) []*piece {
	var pieces []*piece
	for _, e := range entries {
		if len(pieces) > 0 {
			previous := pieces[len(pieces)-1]
			if previous.source == e.source {
				if previous.length == 1 {
					if step := e.index - previous.start; step == 1 || step == -1 {
						previous.step = step
						previous.length = 2
						continue
					}
				} else if previous.at(previous.length) == e.index {
					previous.length++
					continue
				}
			}
		}
		pieces = append(pieces, &piece{source: e.source, start: e.index, length: 1, step: 1})
	}
	return pieces
}

// deduplicateEntries is upstream's deduplicateArrayEntries: after a comparator
// that edited the array's structure, an entry the sort placed twice keeps its
// first place and every later one becomes an inserted copy.
func (n *node) deduplicateEntries() {
	a := n.array
	seenBase := map[int]bool{}
	seenInsert := map[*insertSource]map[int]bool{}
	var next []*piece
	duplicated := false
	for _, p := range a.piecesOf() {
		for offset := range p.length {
			sourceIndex := p.at(offset)
			var seen bool
			if p.base() {
				seen = seenBase[sourceIndex]
				seenBase[sourceIndex] = true
			} else {
				indices := seenInsert[p.source]
				if indices == nil {
					indices = map[int]bool{}
					seenInsert[p.source] = indices
				}
				seen = indices[sourceIndex]
				indices[sourceIndex] = true
			}
			if seen {
				duplicated = true
				copied, err := n.clonePlacementStored(entrySlot(p, sourceIndex), n.entryValue(p, sourceIndex), !p.base() || n.hasEntryOverride(p, sourceIndex))
				if err != nil {
					// The entry is already the change's own, strict JSON.
					panic(err)
				}
				next = appendMerged(next, insertPiece([]any{copied})[0])
			} else {
				next = appendMerged(next, &piece{source: p.source, start: sourceIndex, length: 1, step: 1})
			}
		}
	}
	if !duplicated {
		return
	}
	a.replaceAllPieces(next)
	a.structural = true
	a.generation++
	a.plan = nil
	n.markDirty()
}

// ─── Placements ──────────────────────────────────────────────────────────────

// ValueError reports a value a draft cannot place: a cycle, a number that is
// not finite, or a Go type with no JSON form. Its Message is upstream's
// copyJson TypeError text, and its Value the offending Go value (nil for a
// cycle). chord.ValueError is the same type.
type ValueError = jsonvalue.Error

// clonePlacement is upstream's: a value placed into a draft, copied and
// checked by chord's copyJson. A draft — the value itself, or one inside it —
// is copied as it holds its content now.
func clonePlacement(v any) (any, error) {
	c, err := copyPlacement(v)
	var ve *ValueError
	if errors.As(err, &ve) {
		return nil, fmt.Errorf("delta: %w", err)
	}
	return c, err
}

// copyPlacement is clonePlacement below the top: its errors are wrapped once,
// by clonePlacement.
func copyPlacement(v any) (any, error) {
	return jsonvalue.Copy(v, copyDraft)
}

// copyDraft is copyJson's reading of a draft through its traps.
func copyDraft(v any) (any, bool, error) {
	d, ok := v.(*Draft)
	if !ok {
		return nil, false, nil
	}
	n, err := d.readable()
	if err != nil {
		return nil, true, err
	}
	c, err := n.clonePlacementNode()
	return c, true, err
}

// clonePlacementNode is upstream's: a node's content now, copied — through
// the nodes of its members, where they have one.
func (n *node) clonePlacementNode() (any, error) {
	if n.ctx.settled() {
		return nil, ErrDraftSettled
	}
	if n.isArray() {
		a := n.arrayOverlay()
		out := newArray(0)
		for _, p := range a.piecesOf() {
			for offset := range p.length {
				sourceIndex := p.at(offset)
				v, err := n.clonePlacementStored(entrySlot(p, sourceIndex), n.entryValue(p, sourceIndex), !p.base() || n.hasEntryOverride(p, sourceIndex))
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
		}
		return out, nil
	}
	out := make(map[string]any, n.objectLen())
	var err error
	n.members(func(k string, value any, written bool) bool {
		var v any
		if v, err = n.clonePlacementStored(slot{kind: objectEntry, key: k}, value, written); err != nil {
			return false
		}
		out[k] = v
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// clonePlacementStored is upstream's: a member's value copied — through its
// node when it has one, since that node may hold edits.
func (n *node) clonePlacementStored(s slot, value any, placement bool) (any, error) {
	if isContainer(value) {
		if c := n.existingChild(s, value, placement); c != nil {
			return c.clonePlacementNode()
		}
	}
	return copyPlacement(value)
}

// ─── JavaScript conversions ──────────────────────────────────────────────────

// errNoPrimitive is the TypeError JavaScript's ToPrimitive throws for a JSON
// object with an own "toString" member.
var errNoPrimitive = errors.New(`delta: Cannot convert object to primitive value (an object with a "toString" member has no string form in JavaScript, because the member is not a function; rename the member, or sort with a comparator)`)

// jsString is String(v) — ToPrimitive, then ToString — for a JSON value or a
// draft, the conversion JavaScript's default sort order and an assigned array
// length apply. A draft is converted as it holds its content now.
func jsString(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case string:
		return x, nil
	case *Draft:
		n, err := x.readable()
		if err != nil {
			return "", err
		}
		return n.string()
	case map[string]any:
		if _, ok := x["toString"]; ok {
			return "", errNoPrimitive
		}
		return "[object Object]", nil
	case []any:
		var b strings.Builder
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if item == nil {
				continue
			}
			s, err := jsString(item)
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		}
		return b.String(), nil
	}
	if f, ok := number(v); ok {
		return jsNumber(f), nil
	}
	// A Go value with no JSON form, inside a plain value handed to
	// Set("length", …): an object, to JavaScript's String().
	return "[object Object]", nil
}

// string is String() of a node's content: an array's elements' strings joined
// with commas (null as ""), or "[object Object]" — neither has a primitive
// value of its own, and a JSON object can shadow toString only with a member
// that is not a function, which leaves it no string form at all.
func (n *node) string() (string, error) {
	if !n.isArray() {
		if n.objectHas("toString") {
			return "", errNoPrimitive
		}
		return "[object Object]", nil
	}
	var b strings.Builder
	for i := range n.arrayOverlay().length() {
		if i > 0 {
			b.WriteByte(',')
		}
		item := n.entry(i)
		if item == nil {
			continue
		}
		s, err := jsString(item)
		if err != nil {
			return "", err
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// jsToNumber is ToNumber(v): null 0, booleans 0 and 1, a string by
// StringToNumber, and a container or draft by the number its string spells
// (ToPrimitive with hint number tries valueOf first, which never yields a
// primitive for JSON, then toString). Anything else is NaN.
func jsToNumber(v any) (float64, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return stringToNumber(x), nil
	case map[string]any, []any, *Draft:
		s, err := jsString(x)
		if err != nil {
			return 0, err
		}
		return stringToNumber(s), nil
	}
	if f, ok := number(v); ok {
		return f, nil
	}
	return math.NaN(), nil
}

// stringToNumber is JavaScript's StringToNumber: JavaScript's whitespace and
// line terminators trimmed, "" as 0, then an unsigned 0x, 0o or 0b integer, or
// a decimal literal with an optional sign ("Infinity" included). Anything
// else is NaN; so is every spelling Go's ParseFloat reads and JavaScript does
// not ("inf", "NaN", "0x1p1", "1_0").
func stringToNumber(s string) float64 {
	s = strings.TrimFunc(s, isJSWhitespace)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			digits := s[2:]
			for _, r := range digits {
				if d := digitValue(r); d < 0 || d >= base {
					return math.NaN()
				}
			}
			n, _ := new(big.Int).SetString(digits, base)
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}
	unsigned := s
	if s[0] == '+' || s[0] == '-' {
		unsigned = s[1:]
	}
	switch {
	case unsigned == "Infinity" && s[0] == '-':
		return math.Inf(-1)
	case unsigned == "Infinity":
		return math.Inf(1)
	case !isDecimalLiteral(unsigned):
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(s, 64) // out of range is ±Inf or ±0, as in JavaScript
	return f
}

// isDecimalLiteral is StrUnsignedDecimalLiteral without Infinity: digits with
// an optional fraction (either side of the point may be empty, not both) and
// an optional exponent.
func isDecimalLiteral(s string) bool {
	digits := func(s string) (int, string) {
		n := 0
		for n < len(s) && s[n] >= '0' && s[n] <= '9' {
			n++
		}
		return n, s[n:]
	}
	whole, rest := digits(s)
	fraction := 0
	if rest != "" && rest[0] == '.' {
		fraction, rest = digits(rest[1:])
	}
	if whole+fraction == 0 {
		return false
	}
	if rest != "" && (rest[0] == 'e' || rest[0] == 'E') {
		rest = rest[1:]
		if rest != "" && (rest[0] == '+' || rest[0] == '-') {
			rest = rest[1:]
		}
		exponent, after := digits(rest)
		if exponent == 0 {
			return false
		}
		rest = after
	}
	return rest == ""
}

// digitValue is r's value as a digit of base up to 16, or -1.
func digitValue(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	}
	return -1
}

// isJSWhitespace is StrWhiteSpaceChar: JavaScript's WhiteSpace (tab, vertical
// tab, form feed, the byte order mark and every space separator) and
// LineTerminator. U+0085, which Go's unicode.IsSpace includes, is neither.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\v', '\f', 0xFEFF, '\n', '\r', 0x2028, 0x2029:
		return true
	}
	return unicode.Is(unicode.Zs, r)
}
