package delta

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ─── Revision diff ───────────────────────────────────────────────────────────
//
// Upstream's delta/diff.ts. A tracker records nothing while a draft is
// mutated; it diffs the committed revision against the candidate the draft
// produced, and the batch it publishes is this diff. Immutable revisions share
// every unchanged subtree, so identity — upstream's === — is the fast path
// everywhere: an unchanged branch is skipped without being walked, and an
// array's surviving elements are recognised by identity before anything is
// compared by value.

const (
	// defaultOverlapScan is how far back a string diff looks for a rolling
	// window, in UTF-16 code units.
	defaultOverlapScan = 65_536
	// maxDeltaOperations bounds one batch. Past it the diff gives up and
	// publishes the complete value.
	maxDeltaOperations = 4_096
	// maxIdentityCandidates bounds the identity-anchor search; past it the
	// anchors are chosen greedily.
	maxIdentityCandidates = 200_000
	// maxSemanticCells bounds the value-alignment table (before × after).
	maxSemanticCells = 65_536
	// snapshotThreshold is the batch size, in upstream's JSON cost units, below
	// which a delta is published without comparing it to a snapshot.
	snapshotThreshold = 65_536
)

// DiffRevisions computes a compact batch that turns before into after — the
// ops a tracker publishes for a change. It is exported, as upstream's
// diffRevisions is, for any two revisions: they need not come from a tracker.
// Identity is upstream's ===: a shared map, or a slice with the same backing
// array and length, is one container, which skips a subtree and anchors an
// array alignment, so it decides which ops describe the change, never the
// value they produce. An empty slice with no capacity — every [] encoding/json
// decodes — is identical to nothing; to share an empty array between two
// revisions, give it capacity.
//
// Strings become appends and front-truncations where they can, arrays splices
// and permutations anchored on the elements the revisions share, and objects
// sets and deletes of the members that differ. A batch that would exceed 4,096
// ops, or cost more than the complete value once it is large, is published as
// one Replace of after instead. Two deeply equal revisions yield an empty,
// non-nil batch.
//
// Payloads alias after: the ops share its containers, which must not be
// modified afterwards.
func DiffRevisions(before, after any) []Op {
	d := differ{ops: []Op{}}
	d.value(before, after, nil)
	if d.overflowed {
		return []Op{Replace{Value: after}}
	}
	if len(d.ops) == 0 {
		return d.ops
	}
	if _, ok := d.ops[0].(Replace); ok {
		return d.ops
	}
	deltaCost := 2
	for _, op := range d.ops {
		deltaCost += operationCost(op) + 1
	}
	if deltaCost < snapshotThreshold {
		return d.ops
	}
	if deltaCost >= jsonCost(after)+6 {
		return []Op{Replace{Value: after}}
	}
	return d.ops
}

// differ accumulates one batch. overflowed is upstream's overflowedBatches
// membership: once set, nothing more is emitted and the batch becomes a
// Replace.
type differ struct {
	ops        []Op
	overflowed bool
}

func (d *differ) emit(op Op) {
	if d.overflowed {
		return
	}
	if len(d.ops) >= maxDeltaOperations {
		d.overflowed = true
		return
	}
	d.ops = append(d.ops, op)
}

// set publishes value at path whole: a Replace at the root, a Set below it.
func (d *differ) set(path Path, value any) {
	if len(path) == 0 {
		d.emit(Replace{Value: value})
		return
	}
	d.emit(Set{Path: path, Value: value})
}

// child is path plus one segment, in storage of its own: each op keeps its
// path, as each upstream op holds its own array.
func child(path Path, seg Seg) Path {
	out := make(Path, len(path), len(path)+1)
	copy(out, path)
	return append(out, seg)
}

// value is upstream's diffValue.
func (d *differ) value(before, after any, path Path) {
	if same(before, after) || d.overflowed {
		return
	}
	if b, ok := before.(string); ok {
		if a, ok := after.(string); ok && len(path) > 0 {
			d.string(b, a, path)
			return
		}
	}
	switch b := before.(type) {
	case []any:
		if a, ok := after.([]any); ok {
			d.array(b, a, path)
			return
		}
	case map[string]any:
		if a, ok := after.(map[string]any); ok {
			d.object(b, a, path)
			return
		}
	}
	d.set(path, after)
}

// string is upstream's emitString: an append when after extends before; a
// front-truncation plus an append when a suffix of before is a prefix of after
// (a rolling window); a set otherwise. Counts are UTF-16 code units, the unit
// a "t" carries. An append's text is a copy of its own (upstream's
// ownedSlice, f715a15c1): a substring would pin all of after for as long as
// the op lives. Comparing two revisions reads both strings: a writer that
// knows what it appended records it with Draft.AppendText instead.
func (d *differ) string(before, after string, path Path) {
	if before == after {
		return
	}
	// A byte prefix is a code-unit prefix: before is a whole string, so it
	// ends on a rune boundary.
	if len(after) > len(before) && strings.HasPrefix(after, before) {
		d.emit(Append{Path: path, Text: strings.Clone(after[len(before):])})
		return
	}
	shared := Overlap(before, after, defaultOverlapScan)
	if shared == 0 {
		d.emit(Set{Path: path, Value: after})
		return
	}
	d.emit(Truncate{Path: path, Count: utf16Len(before) - shared})
	if rest := after[unitsOffset(after, shared):]; rest != "" {
		d.emit(Append{Path: path, Text: strings.Clone(rest)})
	}
}

// unitsOffset is the byte offset in s after its first units UTF-16 code
// units — s.slice(units), for a cut that lands on a rune boundary, which
// Overlap's answer always does.
func unitsOffset(s string, units int) int {
	i := 0
	for units > 0 && i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		units -= utf16.RuneLen(r)
		i += size
	}
	return i
}

// object is upstream's diffObject. An object holding a reserved key is set
// whole: its members are never addressed by path. Otherwise every member of
// after is diffed or set, then every member only before holds is deleted —
// both in keyOrder, where upstream follows insertion order.
func (d *differ) object(before, after map[string]any, path Path) {
	if hasReservedKey(before) || hasReservedKey(after) {
		if !jsonEqual(before, after) {
			d.set(path, after)
		}
		return
	}
	for _, key := range keyOrder(after) {
		if d.overflowed {
			return
		}
		if previous, ok := before[key]; ok {
			d.value(previous, after[key], child(path, Key(key)))
		} else {
			d.set(child(path, Key(key)), after[key])
		}
	}
	for _, key := range keyOrder(before) {
		if d.overflowed {
			return
		}
		if _, ok := after[key]; !ok {
			d.emit(Delete{Path: child(path, Key(key))})
		}
	}
}

func hasReservedKey(m map[string]any) bool {
	for key := range m {
		if ReservedSegments[key] {
			return true
		}
	}
	return false
}

// ─── Arrays ──────────────────────────────────────────────────────────────────

// match pairs an element of before with the element of after it survives as.
type match struct{ before, after int }

// sameValue is upstream's `left === right || equalJson(left, right)`.
func sameValue(a, b any) bool { return same(a, b) || jsonEqual(a, b) }

// array is upstream's diffArray: nothing for equal arrays; one "m" for a pure
// reorder whose first and last elements both moved; otherwise the region diff.
func (d *differ) array(before, after []any, path Path) {
	if same(before, after) || jsonEqual(before, after) {
		return
	}
	if len(before) == len(after) && len(before) > 1 &&
		!sameValue(before[0], after[0]) && !sameValue(before[len(before)-1], after[len(after)-1]) {
		if order := permutation(before, after); order != nil {
			d.emit(Permute{Path: path, Permutation: order})
			return
		}
	}
	d.region(before, after, path, 0, len(before), 0, len(after), 0)
}

// permutation is order such that after[i] is before[order[i]], matching
// repeated values in order of appearance, or nil when after is not a
// rearrangement of before. Values match by SameValueZero: scalars by value,
// containers by identity.
func permutation(before, after []any) []int {
	if len(before) != len(after) {
		return nil
	}
	type slot struct{ indices []int }
	positions := map[valueKey]*slot{}
	for i, v := range before {
		k, ok := keyOf(v)
		if !ok {
			return nil
		}
		if s := positions[k]; s != nil {
			s.indices = append(s.indices, i)
		} else {
			positions[k] = &slot{indices: []int{i}}
		}
	}
	order := make([]int, len(after))
	for i, v := range after {
		k, ok := keyOf(v)
		s := positions[k]
		if !ok || s == nil || len(s.indices) == 0 {
			return nil
		}
		order[i], s.indices = s.indices[0], s.indices[1:]
	}
	return order
}

// region is upstream's diffArrayRegion over before[bs:be] and after[as:ae],
// whose first element lands at index out of the array being built.
func (d *differ) region(before, after []any, path Path, bs, be, as, ae, out int) {
	if d.overflowed {
		return
	}
	for bs < be && as < ae && sameValue(before[bs], after[as]) {
		bs, as, out = bs+1, as+1, out+1
	}
	for bs < be && as < ae && sameValue(before[be-1], after[ae-1]) {
		be, ae = be-1, ae-1
	}
	nb, na := be-bs, ae-as
	if nb == 0 && na == 0 {
		return
	}
	if nb == 0 || na == 0 {
		d.emit(Splice{Path: path, Index: out, Remove: nb, Items: cloneArray(after[as:ae])})
		return
	}

	if nb == na {
		var positional []match
		for i := range nb {
			if sameValue(before[bs+i], after[as+i]) {
				positional = append(positional, match{bs + i, as + i})
			}
		}
		if len(positional) > 0 {
			d.matches(before, after, path, bs, be, as, ae, out, positional)
			return
		}
	}
	if sub := identitySubsequence(before, bs, be, after, as, ae); len(sub) > 0 {
		d.matches(before, after, path, bs, be, as, ae, out, sub)
		return
	}
	if anchors := identityAnchors(before, bs, be, after, as, ae); len(anchors) > 0 {
		d.matches(before, after, path, bs, be, as, ae, out, anchors)
		return
	}
	if semantic := lcsMatches(before[bs:be], after[as:ae], semanticallyAligned, maxSemanticCells); len(semantic) > 0 {
		for i := range semantic {
			semantic[i].before += bs
			semantic[i].after += as
		}
		d.matches(before, after, path, bs, be, as, ae, out, semantic)
		return
	}
	if nb == 1 && na == 1 {
		d.value(before[bs], after[as], child(path, Index(out)))
		return
	}
	d.emit(Splice{Path: path, Index: out, Remove: nb, Items: cloneArray(after[as:ae])})
}

// matches is upstream's processArrayMatches: the gaps between consecutive
// matches are regions of their own, and each matched pair that is not the
// same value is diffed in place.
func (d *differ) matches(before, after []any, path Path, bs, be, as, ae, out int, ms []match) {
	b, a := bs, as
	for _, m := range ms {
		if d.overflowed {
			return
		}
		d.region(before, after, path, b, m.before, a, m.after, out)
		out += m.after - a
		if !sameValue(before[m.before], after[m.after]) {
			d.value(before[m.before], after[m.after], child(path, Index(out)))
		}
		out++
		b, a = m.before+1, m.after+1
	}
	if !d.overflowed {
		d.region(before, after, path, b, be, a, ae, out)
	}
}

// identitySubsequence is upstream's: when one side is shorter, match each of
// its elements, in order, to the next identical element of the longer side;
// nil when that fails, or when the sides are the same length.
func identitySubsequence(before []any, bs, be int, after []any, as, ae int) []match {
	nb, na := be-bs, ae-as
	var ms []match
	switch {
	case na < nb:
		bi := bs
		for ai := as; ai < ae; ai++ {
			for bi < be && !same(before[bi], after[ai]) {
				bi++
			}
			if bi == be {
				return nil
			}
			ms = append(ms, match{bi, ai})
			bi++
		}
		return ms
	case nb < na:
		ai := as
		for bi := bs; bi < be; bi++ {
			for ai < ae && !same(before[bi], after[ai]) {
				ai++
			}
			if ai == ae {
				return nil
			}
			ms = append(ms, match{bi, ai})
			ai++
		}
		return ms
	}
	return nil
}

// identityAnchors is upstream's: the longest increasing run of identity
// matches between the regions (a patience-style longest increasing
// subsequence over every candidate pair), or a greedy left-to-right choice
// once the candidates exceed maxIdentityCandidates.
func identityAnchors(before []any, bs, be int, after []any, as, ae int) []match {
	positions := map[valueKey][]int{}
	for i := bs; i < be; i++ {
		if k, ok := keyOf(before[i]); ok {
			positions[k] = append(positions[k], i)
		}
	}
	lookup := func(v any) []int {
		if k, ok := keyOf(v); ok {
			return positions[k]
		}
		return nil
	}
	candidates := 0
	for i := as; i < ae; i++ {
		candidates += len(lookup(after[i]))
		if candidates > maxIdentityCandidates {
			return greedyIdentityAnchors(lookup, after, as, ae)
		}
	}
	if candidates == 0 {
		return nil
	}

	type candidate struct{ before, after, previous int }
	var (
		all        []candidate
		tails      []int
		tailValues []int
	)
	for ai := as; ai < ae; ai++ {
		bps := lookup(after[ai])
		for j := len(bps) - 1; j >= 0; j-- {
			bi := bps[j]
			at := lowerBound(tailValues, bi)
			previous := -1
			if at > 0 {
				previous = tails[at-1]
			}
			all = append(all, candidate{bi, ai, previous})
			if at == len(tails) {
				tails = append(tails, len(all)-1)
				tailValues = append(tailValues, bi)
			} else {
				tails[at] = len(all) - 1
				tailValues[at] = bi
			}
		}
	}
	var ms []match
	for c := tails[len(tails)-1]; c >= 0; c = all[c].previous {
		ms = append(ms, match{all[c].before, all[c].after})
	}
	for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
		ms[i], ms[j] = ms[j], ms[i]
	}
	return ms
}

// greedyIdentityAnchors takes, for each element of after in turn, the first
// identical element of before past the previous anchor.
func greedyIdentityAnchors(lookup func(any) []int, after []any, as, ae int) []match {
	var ms []match
	previous := -1
	for ai := as; ai < ae; ai++ {
		bps := lookup(after[ai])
		if bps == nil {
			continue
		}
		at := lowerBound(bps, previous+1)
		if at == len(bps) {
			continue
		}
		ms = append(ms, match{bps[at], ai})
		previous = bps[at]
	}
	return ms
}

// lowerBound is the first index of values (ascending) not less than value.
func lowerBound(values []int, value int) int {
	lo, hi := 0, len(values)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if values[mid] < value {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// lcsMatches is upstream's: a longest common subsequence of before and after
// under equal, as match pairs relative to the two slices; nil when the table
// would exceed maxCells.
func lcsMatches(before, after []any, equal func(a, b any) bool, maxCells int) []match {
	if len(before) == 0 || len(after) == 0 {
		return []match{}
	}
	if len(before)*len(after) > maxCells {
		return nil
	}
	width := len(after) + 1
	lengths := make([]uint32, (len(before)+1)*width)
	for l := len(before) - 1; l >= 0; l-- {
		for r := len(after) - 1; r >= 0; r-- {
			at := l*width + r
			if equal(before[l], after[r]) {
				lengths[at] = lengths[(l+1)*width+r+1] + 1
			} else {
				lengths[at] = max(lengths[(l+1)*width+r], lengths[l*width+r+1])
			}
		}
	}
	var ms []match
	l, r := 0, 0
	for l < len(before) && r < len(after) {
		switch {
		case equal(before[l], after[r]) && lengths[l*width+r] == lengths[(l+1)*width+r+1]+1:
			ms = append(ms, match{l, r})
			l, r = l+1, r+1
		case lengths[(l+1)*width+r] >= lengths[l*width+r+1]:
			l++
		default:
			r++
		}
	}
	return ms
}

// semanticallyAligned is upstream's: two values align when they are the same
// value, or containers of one kind that still share a child container by
// identity — an array at the same index, an object under the same key.
func semanticallyAligned(left, right any) bool {
	if sameValue(left, right) {
		return true
	}
	switch l := left.(type) {
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for i, v := range l {
			if isContainer(v) && same(v, r[i]) {
				return true
			}
		}
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range l {
			if w, ok := r[k]; ok && isContainer(v) && same(v, w) {
				return true
			}
		}
	}
	return false
}

// ─── Cost ────────────────────────────────────────────────────────────────────
//
// Upstream's jsonCost/pathCost/operationCost: an estimate of the serialized
// size, in UTF-16 code units, that decides between a delta and a snapshot.
// Strings count their length plus the quotes, not their escapes; numbers count
// their JavaScript spelling. The figures must be upstream's exactly, because
// which side of the threshold a batch lands on is wire-visible.

func jsonCost(v any) int {
	switch x := v.(type) {
	case nil:
		return 4
	case string:
		return utf16Len(x) + 2
	case bool:
		if x {
			return 4
		}
		return 5
	case []any:
		cost := 2
		for i, item := range x {
			cost += jsonCost(item)
			if i > 0 {
				cost++
			}
		}
		return cost
	case map[string]any:
		cost := 2
		i := 0
		for k, item := range x {
			cost += utf16Len(k) + 3 + jsonCost(item)
			if i > 0 {
				cost++
			}
			i++
		}
		return cost
	case []int:
		cost := 2
		for i, n := range x {
			cost += len(strconv.Itoa(n))
			if i > 0 {
				cost++
			}
		}
		return cost
	}
	if f, ok := number(v); ok {
		return len(jsNumber(f))
	}
	// Not a JSON value; upstream would have counted Object.keys of it. Count
	// what it would marshal to as null.
	return 4
}

func pathCost(path Path) int {
	cost := 2
	for i, seg := range path {
		switch s := seg.(type) {
		case Key:
			cost += utf16Len(string(s)) + 2
		case Index:
			cost += len(s.String())
		}
		if i > 0 {
			cost++
		}
	}
	return cost
}

func operationCost(op Op) int {
	switch op := op.(type) {
	case Replace:
		return 6 + jsonCost(op.Value)
	case Set:
		return 7 + pathCost(op.Path) + jsonCost(op.Value)
	case Delete:
		return 6 + pathCost(op.Path)
	case Append:
		return 7 + pathCost(op.Path) + utf16Len(op.Text) + 2
	case Truncate:
		return 7 + pathCost(op.Path) + len(strconv.Itoa(op.Count))
	case Splice:
		return 10 + pathCost(op.Path) + len(strconv.Itoa(op.Index)) + len(strconv.Itoa(op.Remove)) + jsonCost(op.Items)
	case Permute:
		return 7 + pathCost(op.Path) + jsonCost(op.Permutation)
	}
	return 0
}

// jsNumber is String(f) for a finite JavaScript number: the shortest digits
// that round-trip, in positional notation from 1e-6 up to (not including)
// 1e21 and in exponent notation outside it, with -0 spelled "0".
func jsNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	format := byte('f')
	if abs := math.Abs(f); abs < 1e-6 || abs >= 1e21 {
		format = 'e'
	}
	s := strconv.FormatFloat(f, format, -1, 64)
	if format == 'e' {
		// strconv writes a two-digit exponent ("1e-07"); JavaScript does not.
		if n := len(s); n >= 4 && s[n-4] == 'e' && s[n-2] == '0' {
			s = s[:n-2] + s[n-1:]
		}
	}
	return s
}
