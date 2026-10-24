package delta

import (
	"slices"
	"strings"
)

// ─── Emitting a change ───────────────────────────────────────────────────────
//
// Upstream's emitOperations: the ops a prepared change publishes, read off its
// overlay. Each dirty node emits its own edits at the path it resolves to
// now, shallowest first, so that a structural edit to an array lands before
// the edits inside its entries, which address their final indices. A node
// whose container this change placed publishes whole with the op that placed
// it; a node below a reserved key folds into a set of its nearest safe
// ancestor; many edits across one array fold into region splices; and a batch
// of more than maxDeltaOperations ops is one Replace.

// maxSimpleObjectNodes is upstream's MAX_SIMPLE_OBJECT_NODES: past this many
// dirty objects the general emission orders them instead.
const maxSimpleObjectNodes = 128

// overlapScan is the window emitChangedValue searches for a rolling string.
const overlapScan = 65_536

// denseThreshold is how many edited entries of one array make a dense region.
const denseThreshold = 256

func (c *overlayContext) emitOperations() []Op {
	if ops, ok := c.emitSimpleObjectOperations(); ok {
		return ops
	}
	ops := []Op{}
	var forcedFolds, emissionPaths orderedMap[*node, Path]
	var denseIndices orderedMap[*node, *denseCandidates]
	var denseRegions orderedMap[*node, []denseRegion]
	for _, n := range c.dirty {
		recordDenseArrayPosition(n, &denseIndices)
		if n.isArray() {
			a := n.arrayOverlay()
			if !a.structural && a.baseOverrides.len() > 0 {
				candidates := candidatesFor(&denseIndices, n)
				for index := range a.baseOverrides.keys() {
					candidates.add(index)
				}
			}
		}
	}
	for array, candidates := range denseIndices.all() {
		path, ok := array.resolvePath()
		if !ok || reservedIndex(path) >= 0 {
			continue
		}
		if regions := candidates.regions(); len(regions) > 0 {
			denseRegions.set(array, regions)
		}
	}
	for _, n := range c.dirty {
		path, ok := n.resolvePath()
		if !ok || hasCoveringDenseRegion(n, path, &denseRegions) {
			continue
		}
		emissionPaths.set(n, path)
		if !n.isArray() && n.hasReservedMutation() {
			forcedFolds.set(n, path)
		}
		if reservedAt := reservedIndex(path); reservedAt >= 0 {
			ancestor := n
			for depth := len(path); depth > reservedAt; depth-- {
				ancestor = ancestor.parent
			}
			forcedFolds.set(ancestor, path[:reservedAt:reservedAt])
		}
	}
	for n, path := range forcedFolds.all() {
		emissionPaths.set(n, path)
	}
	for n := range denseRegions.keys() {
		if path, ok := n.resolvePath(); ok && !hasCoveringDenseRegion(n, path, &denseRegions) {
			emissionPaths.set(n, path)
		}
	}
	maxDepth := 0
	for _, path := range emissionPaths.all() {
		maxDepth = max(maxDepth, len(path))
	}
	buckets := make([][]*node, maxDepth+1)
	for n, path := range emissionPaths.all() {
		buckets[len(path)] = append(buckets[len(path)], n)
	}
	folded := map[*node]bool{}
	for _, bucket := range buckets {
		for _, n := range bucket {
			path, _ := emissionPaths.get(n)
			if n.hasPlacementAncestor() || hasFoldedAncestor(n, folded) {
				continue
			}
			if forcedFolds.has(n) {
				ops = emitSet(ops, path, n.cloneNode())
				folded[n] = true
				continue
			}
			if n.isArray() {
				regions, _ := denseRegions.get(n)
				ops = n.emitArrayOperations(path, ops, regions)
			} else {
				ops, _ = n.emitObjectOperations(path, ops)
			}
			if len(ops) > maxDeltaOperations {
				return []Op{Replace{Value: c.root.cloneNode()}}
			}
		}
	}
	return ops
}

// emitSimpleObjectOperations is upstream's fast path for a change that edited
// only objects below objects: at most maxSimpleObjectNodes of them, none below
// a placed container or a reserved key, emitted shallowest first. false when
// the change needs the general path.
func (c *overlayContext) emitSimpleObjectOperations() ([]Op, bool) {
	var nodes []*node
	for _, n := range c.dirty {
		if n.isArray() || n.hasReservedMutation() || n.hasPlacementAncestor() {
			return nil, false
		}
		for p := n.parent; p != nil; p = p.parent {
			if p.isArray() {
				return nil, false
			}
		}
		path, ok := n.resolvePath()
		if !ok {
			continue
		}
		if reservedIndex(path) >= 0 {
			return nil, false
		}
		nodes = append(nodes, n)
		if len(nodes) > maxSimpleObjectNodes {
			return nil, false
		}
	}
	slices.SortStableFunc(nodes, func(a, b *node) int { return len(a.path) - len(b.path) })
	ops := []Op{}
	direct := true
	for _, n := range nodes {
		var normalized bool
		ops, normalized = n.emitObjectOperations(n.path, ops)
		if normalized {
			direct = false
		}
		if len(ops) > maxDeltaOperations {
			return []Op{Replace{Value: c.root.cloneNode()}}, true
		}
	}
	c.simpleObjectMaterialization = direct
	return ops, true
}

// emitObjectOperations is upstream's: a readded member is deleted first, so
// that the replica's key order follows the draft's; then every write, then
// every deletion of a base member. normalized reports a container write that
// turned out deeply equal to the member it replaced, which the direct
// materialization cannot represent (the revision keeps the old container).
func (n *node) emitObjectOperations(path Path, ops []Op) (_ []Op, normalized bool) {
	base := n.object()
	for key := range n.readded.keys() {
		if _, own := base[key]; own {
			ops = append(ops, Delete{Path: child(path, Key(key))})
		}
	}
	for key, value := range n.writes.all() {
		if len(ops) > maxDeltaOperations {
			return ops, normalized
		}
		before, hasBefore := base[key]
		if n.readded.has(key) {
			before, hasBefore = nil, false
		}
		after := n.cloneStored(slot{kind: objectEntry, key: key}, value, true)
		var emitted bool
		ops, emitted = emitChangedValue(ops, child(path, Key(key)), before, hasBefore, after)
		if hasBefore && isContainer(before) && isContainer(after) && !emitted {
			normalized = true
		}
	}
	for key := range n.deletes.keys() {
		if len(ops) > maxDeltaOperations {
			return ops, normalized
		}
		if _, own := base[key]; own {
			ops = append(ops, Delete{Path: child(path, Key(key))})
		}
	}
	return ops, normalized
}

// emitArrayOperations is upstream's: the dense regions as splices; then, for a
// structural edit, the removal runs from the right, the permutation of the
// retained base entries, and the runs of inserted entries at their final
// indices; then each base entry written over, at its final index.
func (n *node) emitArrayOperations(path Path, ops []Op, regions []denseRegion) []Op {
	a := n.arrayOverlay()
	base := n.elements()
	for _, r := range regions {
		if len(ops) > maxDeltaOperations {
			return ops
		}
		ops = append(ops, Splice{Path: path, Index: r.start, Remove: r.length, Items: n.cloneRegion(r)})
	}
	if a.structural {
		plan := n.buildPlan()
		for _, r := range plan.removals {
			if len(ops) > maxDeltaOperations {
				return ops
			}
			ops = append(ops, Splice{Path: path, Index: r.start, Remove: r.length, Items: []any{}})
		}
		if plan.permutation != nil {
			ops = append(ops, Permute{Path: path, Permutation: plan.permutation})
		}
		pieces := a.piecesOf()
		for _, run := range plan.insertions {
			if len(ops) > maxDeltaOperations {
				return ops
			}
			items := []any{}
			for _, p := range pieces[run.first:run.end] {
				for offset := range p.length {
					sourceIndex := p.at(offset)
					items = append(items, n.cloneStored(entrySlot(p, sourceIndex), n.entryValue(p, sourceIndex), true))
				}
			}
			ops = append(ops, Splice{Path: path, Index: run.at, Remove: 0, Items: items})
		}
	}
	for baseIndex, value := range a.baseOverrides.all() {
		if len(ops) > maxDeltaOperations {
			return ops
		}
		index, ok := a.findEntryIndex(nil, baseIndex)
		if !ok || regionContaining(regions, index) {
			continue
		}
		after := n.cloneStored(slot{kind: baseEntry, index: baseIndex}, value, true)
		ops, _ = emitChangedValue(ops, child(path, Index(index)), base[baseIndex], true, after)
	}
	return ops
}

// buildPlan is upstream's buildArrayPlan: from the pieces alone when the
// retained base entries keep their order, else from a walk over every base
// entry.
func (n *node) buildPlan() *arrayPlan {
	a := n.array
	if a.plan != nil {
		return a.plan
	}
	baseLength := len(n.elements())
	pieces := a.piecesOf()
	plan := orderedArrayPlan(baseLength, pieces)
	if plan == nil {
		plan = elementArrayPlan(baseLength, pieces)
	}
	plan.insertions = insertionRuns(pieces)
	a.plan = plan
	return plan
}

// orderedArrayPlan is upstream's: the plan when the retained base entries keep
// their order, the common case of inserts and removals, computed from the
// pieces alone, so an append to a long array walks none of its entries. nil
// when base entries moved.
func orderedArrayPlan(baseLength int, pieces []*piece) *arrayPlan {
	// The retained base ranges in piece order; ordered means strictly
	// ascending without overlap.
	var kept []span
	for _, p := range pieces {
		if !p.base() || p.length == 0 {
			continue
		}
		if p.step != 1 && p.length > 1 {
			return nil
		}
		if len(kept) > 0 && p.start < kept[len(kept)-1].end() {
			return nil
		}
		kept = append(kept, span{start: p.start, length: p.length})
	}
	// The unretained runs from the end backwards: after the last range,
	// between ranges, before the first.
	var removals []span
	end := baseLength
	for i := len(kept); i >= 0; i-- {
		start := 0
		if i > 0 {
			start = kept[i-1].end()
		}
		if end > start {
			removals = append(removals, span{start: start, length: end - start})
		}
		if i > 0 {
			end = kept[i-1].start
		}
	}
	return &arrayPlan{removals: removals}
}

// elementArrayPlan is upstream's: the plan from a walk over every base entry,
// for pieces that moved base entries.
func elementArrayPlan(baseLength int, pieces []*piece) *arrayPlan {
	retained := make([]bool, baseLength)
	var targetBase []int
	for _, p := range pieces {
		if !p.base() {
			continue
		}
		for offset := range p.length {
			index := p.at(offset)
			retained[index] = true
			targetBase = append(targetBase, index)
		}
	}
	var removals []span
	for end := baseLength; end > 0; {
		if retained[end-1] {
			end--
			continue
		}
		start := end - 1
		for start > 0 && !retained[start-1] {
			start--
		}
		removals = append(removals, span{start: start, length: end - start})
		end = start
	}
	var retainedBase []int
	for index, kept := range retained {
		if kept {
			retainedBase = append(retainedBase, index)
		}
	}
	var permutation []int
	if !slices.Equal(targetBase, retainedBase) {
		positions := make([]int, baseLength)
		for i, index := range retainedBase {
			positions[index] = i
		}
		permutation = make([]int, len(targetBase))
		for i, index := range targetBase {
			permutation[i] = positions[index]
		}
	}
	return &arrayPlan{removals: removals, permutation: permutation}
}

// insertionRuns is upstream's insertRunsOf: the runs of consecutive inserted
// pieces, each at the logical index it lands on.
func insertionRuns(pieces []*piece) []insertionRun {
	var insertions []insertionRun
	logical := 0
	for i := 0; i < len(pieces); {
		if pieces[i].base() {
			logical += pieces[i].length
			i++
			continue
		}
		run := insertionRun{at: logical, first: i}
		for i < len(pieces) && !pieces[i].base() {
			logical += pieces[i].length
			i++
		}
		run.end = i
		insertions = append(insertions, run)
	}
	return insertions
}

// cloneRegion is upstream's cloneArrayRegion: a dense region's entries as the
// draft holds them.
func (n *node) cloneRegion(r denseRegion) []any {
	a := n.arrayOverlay()
	out := make([]any, 0, r.length)
	for index := r.start; index < r.start+r.length; index++ {
		p, offset := a.locate(index)
		sourceIndex := p.at(offset)
		out = append(out, n.cloneStored(entrySlot(p, sourceIndex), n.entryValue(p, sourceIndex), !p.base() || n.hasEntryOverride(p, sourceIndex)))
	}
	return out
}

// cloneNode is upstream's: a node's content as a new container, sharing every
// member container nothing below has touched.
func (n *node) cloneNode() any {
	if n.isArray() {
		a := n.arrayOverlay()
		out := newArray(0)
		for _, p := range a.piecesOf() {
			for offset := range p.length {
				sourceIndex := p.at(offset)
				out = append(out, n.cloneStored(entrySlot(p, sourceIndex), n.entryValue(p, sourceIndex), !p.base() || n.hasEntryOverride(p, sourceIndex)))
			}
		}
		return out
	}
	out := make(map[string]any, n.objectLen())
	n.members(func(key string, value any, written bool) bool {
		out[key] = n.cloneStored(slot{kind: objectEntry, key: key}, value, written)
		return true
	})
	return out
}

// cloneStored is upstream's: a member as the revision will hold it — the
// value itself, unless its node recorded edits below it.
func (n *node) cloneStored(s slot, value any, placement bool) any {
	if !isContainer(value) {
		return value
	}
	c := n.existingChild(s, value, placement)
	if c == nil || !c.dirty && !c.subtreeDirty {
		return value
	}
	return c.cloneNode()
}

// emitChangedValue is upstream's: nothing for an equal value; an append, or a
// front-truncation and an append, for a string that extends or rolls the one
// before (counts in UTF-16 code units, the unit a "t" carries); a set
// otherwise. hasBefore is false where upstream's before is undefined. emitted
// reports whether an op was published. An append's text is a copy of its own
// (upstream's ownedSlice): a substring would keep the whole of after alive in
// a delta chain or a watch backlog.
func emitChangedValue(ops []Op, path Path, before any, hasBefore bool, after any) (_ []Op, emitted bool) {
	if hasBefore {
		if !isContainer(after) && same(before, after) {
			return ops, false
		}
		if isContainer(before) && isContainer(after) && jsonEqual(before, after) {
			return ops, false
		}
		if b, ok := before.(string); ok {
			if a, ok := after.(string); ok {
				// A byte prefix is a code-unit prefix: before is a whole string, so
				// it ends on a rune boundary.
				if len(a) > len(b) && strings.HasPrefix(a, b) {
					return append(ops, Append{Path: path, Text: strings.Clone(a[len(b):])}), true
				}
				if shared := Overlap(b, a, overlapScan); shared > 0 {
					ops = append(ops, Truncate{Path: path, Count: utf16Len(b) - shared})
					if rest := a[unitsOffset(a, shared):]; rest != "" {
						ops = append(ops, Append{Path: path, Text: strings.Clone(rest)})
					}
					return ops, true
				}
			}
		}
	}
	return append(ops, Set{Path: path, Value: after}), true
}

// emitSet publishes value at path whole: a Replace at the root.
func emitSet(ops []Op, path Path, value any) []Op {
	if len(path) == 0 {
		return append(ops, Replace{Value: value})
	}
	return append(ops, Set{Path: path, Value: value})
}

// resolvePath is upstream's: the path a node's container sits at now, root
// first, or false once it has left the tree. Resolved paths are kept: nothing
// moves while a change is emitted.
func (n *node) resolvePath() (Path, bool) {
	if n.resolved {
		return n.path, true
	}
	p := n.parent
	if p == nil {
		n.path, n.resolved = Path{}, true
		return n.path, true
	}
	parentPath, ok := p.resolvePath()
	if !ok {
		return nil, false
	}
	if n.kind == objectEntry {
		if !p.objectHas(n.key) {
			return nil, false
		}
		v, _ := p.objectValue(n.key)
		if !p.holds(n, v) {
			return nil, false
		}
		n.path, n.resolved = child(parentPath, Key(n.key)), true
		return n.path, true
	}
	a := p.arrayOverlay()
	index, ok := a.findEntryIndex(n.source, n.index)
	if !ok {
		return nil, false
	}
	if piece, _ := a.locate(index); !p.holds(n, p.entryValue(piece, n.index)) {
		return nil, false
	}
	n.path, n.resolved = child(parentPath, Index(index)), true
	return n.path, true
}

// hasPlacementAncestor is upstream's: the node, or one above it, sits in a
// container this change placed, whose op publishes it whole.
func (n *node) hasPlacementAncestor() bool {
	for c := n; c != nil; c = c.parent {
		if c.parent != nil && c.placement {
			return true
		}
	}
	return false
}

func hasFoldedAncestor(n *node, folded map[*node]bool) bool {
	for p := n.parent; p != nil; p = p.parent {
		if folded[p] {
			return true
		}
	}
	return false
}

// hasReservedMutation is upstream's: the object wrote or deleted a reserved
// key, which no op may address, so it publishes whole.
func (n *node) hasReservedMutation() bool {
	for key := range n.writes.keys() {
		if ReservedSegments[key] {
			return true
		}
	}
	for key := range n.deletes.keys() {
		if ReservedSegments[key] {
			return true
		}
	}
	return false
}

// reservedIndex is the index of the first reserved key in path, or -1.
func reservedIndex(path Path) int {
	for i, seg := range path {
		if k, ok := seg.(Key); ok && ReservedSegments[string(k)] {
			return i
		}
	}
	return -1
}

// ─── Dense regions ───────────────────────────────────────────────────────────

type denseRegion struct{ start, length int }

// denseCandidates is upstream's DenseCandidates: the base indices of one
// array whose entries the change edited — listed, then, from denseThreshold
// on, marked in a bitmap.
type denseCandidates struct {
	indices []int
	bits    []bool
	length  int
}

func (c *denseCandidates) add(index int) {
	if c.bits != nil {
		c.bits[index] = true
		return
	}
	c.indices = append(c.indices, index)
	if len(c.indices) < denseThreshold {
		return
	}
	c.bits = make([]bool, c.length)
	for _, i := range c.indices {
		c.bits[i] = true
	}
	c.indices = nil
}

// regions is upstream's buildDenseRegions: the runs of marked entries (a
// single unmarked one does not break a run) holding at least denseThreshold
// marks, at least half the run.
func (c *denseCandidates) regions() []denseRegion {
	if c.bits == nil {
		return nil
	}
	var regions []denseRegion
	bits := c.bits
	for at := 0; at < len(bits); {
		for at < len(bits) && !bits[at] {
			at++
		}
		if at == len(bits) {
			break
		}
		start, end, count, gap := at, at, 0, 0
		for at < len(bits) {
			if bits[at] {
				count++
				end = at
				gap = 0
			} else if gap++; gap > 1 {
				break
			}
			at++
		}
		if length := end - start + 1; count >= denseThreshold && count*2 >= length {
			regions = append(regions, denseRegion{start: start, length: length})
		}
	}
	return regions
}

func candidatesFor(groups *orderedMap[*node, *denseCandidates], array *node) *denseCandidates {
	if c, ok := groups.get(array); ok {
		return c
	}
	c := &denseCandidates{length: len(array.elements())}
	groups.set(array, c)
	return c
}

// recordDenseArrayPosition is upstream's: a dirty node inside an entry of a
// non-structural array — through objects — counts as an edit of that entry.
func recordDenseArrayPosition(n *node, groups *orderedMap[*node, *denseCandidates]) {
	c := n
	for p := c.parent; p != nil; p = c.parent {
		if p.isArray() {
			a := p.arrayOverlay()
			if c.kind != baseEntry || a.structural {
				return
			}
			current, overridden := a.baseOverrides.get(c.index)
			if !overridden {
				current = p.elements()[c.index]
			}
			if !p.holds(c, current) {
				return
			}
			candidatesFor(groups, p).add(c.index)
			return
		}
		c = p
	}
}

// hasCoveringDenseRegion is upstream's: an array above the node publishes the
// entry it sits in as part of a region splice.
func hasCoveringDenseRegion(n *node, path Path, denseRegions *orderedMap[*node, []denseRegion]) bool {
	for p := n.parent; p != nil; p = p.parent {
		regions, ok := denseRegions.get(p)
		if !ok {
			continue
		}
		parentPath, ok := p.resolvePath()
		if !ok || len(path) <= len(parentPath) || !slices.Equal(path[:len(parentPath)], parentPath) {
			continue
		}
		if index, ok := path[len(parentPath)].(Index); ok && regionContaining(regions, int(index)) {
			return true
		}
	}
	return false
}

func regionContaining(regions []denseRegion, index int) bool {
	for _, r := range regions {
		if index >= r.start && index < r.start+r.length {
			return true
		}
	}
	return false
}
