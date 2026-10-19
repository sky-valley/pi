package delta

import (
	"slices"
	"sort"
)

// ─── Array overlays: the piece table ─────────────────────────────────────────
//
// Upstream's tracker.ts keeps a draft array as a sequence of pieces: runs of
// the base array's entries (in either direction) and runs of entries a change
// inserted. The sequence lives in a treap — split, merge, and a size per
// subtree — so an edit anywhere costs a logarithm of the piece count, not a
// copy of the array. Priorities come from a per-array xorshift generator with
// upstream's seed, so a given edit sequence builds the same tree on both sides
// (though nothing observable depends on its shape: only the in-order sequence
// of pieces reaches the draft or its ops).

// insertSource holds the entries a change inserted: a push, unshift, splice,
// fill, copyWithin or length growth. Pieces address it by index, and a held
// draft of one of its entries keeps its source and index across reordering.
type insertSource struct{ refs []any }

// piece is a run of entries: base entries start, start+step, … (source nil),
// or inserted ones from source. step is 1 or -1; reverse flips it. Pieces are
// shared and mutated in place exactly where upstream's are.
type piece struct {
	source *insertSource
	start  int
	length int
	step   int
}

func (p *piece) base() bool { return p.source == nil }

// at is the source index of the piece's offset-th entry.
func (p *piece) at(offset int) int { return p.start + p.step*offset }

type pieceNode struct {
	piece       *piece
	left, right *pieceNode
	priority    uint32
	elements    int
}

// pieceLocation is one piece of a flattened array with where it starts
// logically and the range of source indices it covers: what findEntryIndex
// searches to map a held entry back to its current index.
type pieceLocation struct {
	piece            *piece
	logicalStart     int
	minimum, maximum int
}

// arrayPlan is how a structural edit publishes: runs of base entries removed,
// from the right; the permutation of the retained base entries (nil when they
// keep their order); and runs of inserted pieces at their final indices.
type arrayPlan struct {
	removals    []span
	permutation []int
	insertions  []insertionRun
}

// span is a run of base indices.
type span struct{ start, length int }

// end is one past the run's last index.
func (s span) end() int { return s.start + s.length }

// insertionRun is a run of consecutive inserted pieces, pieces[first:end] of
// the flattened sequence, landing at logical index at.
type insertionRun struct{ at, first, end int }

// arrayOverlay is upstream's ArrayOverlay: an array draft's pieces, the values
// written over base or inserted entries, and caches of the flattened pieces and
// their locations.
type arrayOverlay struct {
	root            *pieceNode
	pieces          []*piece
	baseOverrides   orderedMap[int, any]
	insertOverrides map[*insertSource]map[int]any
	structural      bool
	generation      int
	plan            *arrayPlan
	seed            uint32
	baseLocations   []pieceLocation
	insertLocations map[*insertSource][]pieceLocation
}

func newArrayOverlay(length int) *arrayOverlay {
	a := &arrayOverlay{seed: 0x9e3779b9}
	if length > 0 {
		a.root = a.newPieceNode(&piece{start: 0, length: length, step: 1})
	}
	return a
}

// nextPriority is upstream's xorshift32. JavaScript does the shifts in int32
// and the >>> in uint32; the bits are the same in uint32 throughout.
func (a *arrayOverlay) nextPriority() uint32 {
	v := a.seed
	v ^= v << 13
	v ^= v >> 17
	v ^= v << 5
	a.seed = v
	return v
}

func (a *arrayOverlay) newPieceNode(p *piece) *pieceNode {
	return &pieceNode{piece: p, priority: a.nextPriority(), elements: p.length}
}

func elements(n *pieceNode) int {
	if n == nil {
		return 0
	}
	return n.elements
}

func (n *pieceNode) update() {
	n.elements = elements(n.left) + n.piece.length + elements(n.right)
}

func mergeTrees(left, right *pieceNode) *pieceNode {
	switch {
	case left == nil:
		return right
	case right == nil:
		return left
	case left.priority >= right.priority:
		left.right = mergeTrees(left.right, right)
		left.update()
		return left
	}
	right.left = mergeTrees(left, right.left)
	right.update()
	return right
}

// split cuts the tree before logical index, splitting the piece it falls in.
func (a *arrayOverlay) split(root *pieceNode, index int) (*pieceNode, *pieceNode) {
	if root == nil {
		return nil, nil
	}
	leftLength := elements(root.left)
	if index < leftLength {
		left, right := a.split(root.left, index)
		root.left = right
		root.update()
		return left, root
	}
	pieceEnd := leftLength + root.piece.length
	if index > pieceEnd {
		left, right := a.split(root.right, index-pieceEnd)
		root.right = left
		root.update()
		return root, right
	}
	if index == leftLength {
		left := root.left
		root.left = nil
		root.update()
		return left, root
	}
	if index == pieceEnd {
		right := root.right
		root.right = nil
		root.update()
		return root, right
	}
	offset := index - leftLength
	first := *root.piece
	first.length = offset
	second := *root.piece
	second.start = root.piece.at(offset)
	second.length = root.piece.length - offset
	return mergeTrees(root.left, a.newPieceNode(&first)), mergeTrees(a.newPieceNode(&second), root.right)
}

func leftmost(n *pieceNode) *pieceNode {
	for n.left != nil {
		n = n.left
	}
	return n
}

func rightmost(n *pieceNode) *pieceNode {
	for n.right != nil {
		n = n.right
	}
	return n
}

// mergeable reports whether right continues left: the same source, and either
// two single entries one apart (in either direction) or a run in one step.
func mergeable(left, right *piece) bool {
	if left.source != right.source {
		return false
	}
	if left.length == 1 && right.length == 1 {
		return abs(right.start-left.start) == 1
	}
	return left.step == right.step && left.at(left.length) == right.start
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// joinNormalized concatenates two trees, fusing the pieces that meet at the
// seam when one continues the other.
func (a *arrayOverlay) joinNormalized(left, right *pieceNode) *pieceNode {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	leftPiece := rightmost(left).piece
	rightPiece := leftmost(right).piece
	if !mergeable(leftPiece, rightPiece) {
		return mergeTrees(left, right)
	}
	leftRest, _ := a.split(left, elements(left)-leftPiece.length)
	_, rightRest := a.split(right, rightPiece.length)
	step := leftPiece.step
	if leftPiece.length == 1 {
		step = rightPiece.start - leftPiece.start
	}
	combined := &piece{source: leftPiece.source, start: leftPiece.start, length: leftPiece.length + rightPiece.length, step: step}
	return a.joinNormalized(a.joinNormalized(leftRest, a.newPieceNode(combined)), rightRest)
}

func flatten(n *pieceNode, out []*piece) []*piece {
	if n == nil {
		return out
	}
	out = flatten(n.left, out)
	out = append(out, n.piece)
	return flatten(n.right, out)
}

// piecesOf is the flattened piece sequence, cached until the next edit.
func (a *arrayOverlay) piecesOf() []*piece {
	if a.pieces == nil {
		a.pieces = flatten(a.root, make([]*piece, 0, 1))
	}
	return a.pieces
}

// treeFrom builds a tree from pieces, merging neighbours first.
func (a *arrayOverlay) treeFrom(pieces []*piece) *pieceNode {
	pieces = mergePieces(pieces)
	var root *pieceNode
	for _, p := range pieces {
		root = mergeTrees(root, a.newPieceNode(p))
	}
	return root
}

func (a *arrayOverlay) replaceAllPieces(pieces []*piece) {
	a.root = a.treeFrom(pieces)
	a.pieces = nil
	a.baseLocations = nil
	a.insertLocations = nil
}

func (a *arrayOverlay) length() int { return elements(a.root) }

// locate is the piece holding logical index, and the entry's offset in it.
func (a *arrayOverlay) locate(index int) (*piece, int) {
	n := a.root
	for n != nil {
		leftLength := elements(n.left)
		switch {
		case index < leftLength:
			n = n.left
		case index >= leftLength+n.piece.length:
			index -= leftLength + n.piece.length
			n = n.right
		default:
			return n.piece, index - leftLength
		}
	}
	panic("delta: array overlay index is out of range")
}

// extendRightmost lengthens the last piece by amount.
func extendRightmost(n *pieceNode, amount int) {
	if n.right != nil {
		extendRightmost(n.right, amount)
	} else {
		n.piece.length += amount
	}
	n.update()
}

func (a *arrayOverlay) invalidateCaches() {
	a.pieces = nil
	a.baseLocations = nil
	a.insertLocations = nil
	a.plan = nil
}

// mergePieces fuses each piece with the one before wherever it continues it,
// in place, as upstream's mergePieces does its array — by appendMerged's rule,
// which is the same.
func mergePieces(pieces []*piece) []*piece {
	merged := pieces[:0]
	for _, p := range pieces {
		merged = appendMerged(merged, p)
	}
	clear(pieces[len(merged):])
	return merged
}

// appendMerged is upstream's appendMergedPiece: add p to the end of pieces,
// extending the last one when p continues it.
func appendMerged(pieces []*piece, p *piece) []*piece {
	if len(pieces) > 0 {
		previous := pieces[len(pieces)-1]
		sameSource := previous.source == p.source
		if sameSource && previous.length == 1 && p.length == 1 {
			if step := p.start - previous.start; step == 1 || step == -1 {
				previous.step = step
				previous.length = 2
				return pieces
			}
		}
		if sameSource && previous.step == p.step && previous.at(previous.length) == p.start {
			previous.length += p.length
			return pieces
		}
	}
	return append(pieces, p)
}

// ensureLocations indexes the flattened pieces by source range.
func (a *arrayOverlay) ensureLocations() {
	if a.baseLocations != nil {
		return
	}
	a.baseLocations = make([]pieceLocation, 0, 1)
	a.insertLocations = map[*insertSource][]pieceLocation{}
	logicalStart := 0
	for _, p := range a.piecesOf() {
		last := p.at(p.length - 1)
		location := pieceLocation{piece: p, logicalStart: logicalStart, minimum: min(p.start, last), maximum: max(p.start, last)}
		if p.base() {
			a.baseLocations = append(a.baseLocations, location)
		} else {
			a.insertLocations[p.source] = append(a.insertLocations[p.source], location)
		}
		logicalStart += p.length
	}
	byMinimum := func(locations []pieceLocation) {
		// Stable, as Array.prototype.sort is.
		slices.SortStableFunc(locations, func(l, r pieceLocation) int { return l.minimum - r.minimum })
	}
	byMinimum(a.baseLocations)
	for _, locations := range a.insertLocations {
		byMinimum(locations)
	}
}

// findEntryIndex is the current logical index of an entry — a base one
// (source nil) or an inserted one — or false once it has been removed.
func (a *arrayOverlay) findEntryIndex(source *insertSource, sourceIndex int) (int, bool) {
	if source == nil && !a.structural {
		return sourceIndex, true
	}
	a.ensureLocations()
	locations := a.baseLocations
	if source != nil {
		locations = a.insertLocations[source]
	}
	// The last piece whose range starts at or before sourceIndex.
	low := sort.Search(len(locations), func(i int) bool { return locations[i].minimum > sourceIndex })
	if low == 0 {
		return 0, false
	}
	location := locations[low-1]
	if sourceIndex > location.maximum {
		return 0, false
	}
	offset := (sourceIndex - location.piece.start) / location.piece.step
	if offset < 0 || offset >= location.piece.length {
		return 0, false
	}
	return location.logicalStart + offset, true
}
