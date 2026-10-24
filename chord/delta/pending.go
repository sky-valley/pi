package delta

import (
	"strconv"
	"strings"
)

// The pending queue: the ops a slow consumer of one state has not taken yet,
// folded while they wait. It mirrors upstream's delta/pending.ts (d8d99950a).

// MaxPendingOps is how many pending ops, after folding, a PendingOps keeps:
// past it only "replace with the current value" stays pending.
const MaxPendingOps = 10_000

// PendingTake is what PendingOps.Take hands over: the pending ops, folded, or
// Replace after an overflow, which the owner of the queue turns into a root
// replacement with the current value — []Op{Replace{Value: current}}. Ops is
// nil exactly when Replace is set, and never nil otherwise: an empty queue
// takes an empty batch.
type PendingTake struct {
	Replace bool
	Ops     []Op
}

type pendingState uint8

const (
	pendingEmpty pendingState = iota
	// One batch, kept by reference: a consumer that keeps up never pays for
	// folding.
	pendingBatch
	pendingFolding
	// Overflowed: nothing more accumulates until Take or a batch that starts
	// with a Replace.
	pendingReplace
)

// PendingOps holds the pending ops of one state for one consumer — a watch
// listener that is still running, a link whose window is full — in order and
// raw (never encoded while they wait), and folds them while they wait, so
// what it hands over replays to the same value with fewer ops:
//
//   - A Replace drops everything before it.
//   - A leaf op — an Append, or a Set of a scalar — folds into the last
//     pending leaf op on the same path: appends join, a Set replaces a Set or
//     an Append, an Append after a string Set appends into it. Any other op
//     clears that index, as it may change what a path names. Leaf ops on
//     different paths commute, except after a scalar Set of an ancestor,
//     after which the descendant cannot be written until a structural op
//     clears the index.
//   - Past MaxPendingOps pending ops after folding, only Replace stays.
//
// It folds the ops of a Tracker, DiffRevisions and Draft.AppendText; it does
// not validate or normalize hand-written ops. Received batches and their ops
// are never modified — they may feed other consumers too; a merge makes a new
// op. The zero value is an empty queue. A PendingOps is not safe for
// concurrent use.
type PendingOps struct {
	state pendingState
	// batch is the one batch pendingBatch holds, as received.
	batch []Op
	// ops is pendingFolding's: owned by the queue until Take hands it over,
	// never one of the received batches.
	ops []Op
	// leaves maps a path's key to the position in ops of the last pending
	// leaf op on that path.
	leaves map[string]int
	// joined holds, by position in ops, the text of a leaf op that appends
	// have folded into: ops keeps the op they folded into, and Take writes
	// the joined text into it. Upstream joins with +, which V8 makes a rope;
	// a Go string is flat, and joining a streamed text chunk by chunk with +
	// would copy all of it per chunk.
	joined map[int]*strings.Builder
}

// Push queues the ops of one commit.
func (q *PendingOps) Push(ops []Op) {
	if len(ops) == 0 {
		return
	}
	// A batch that starts with a Replace — the only place a tracker and
	// DiffRevisions put one — drops everything pending, an overflow too, and
	// is kept as received.
	if _, ok := ops[0].(Replace); ok {
		q.reset(pendingBatch, ops)
		return
	}
	switch q.state {
	case pendingEmpty:
		q.reset(pendingBatch, ops)
	case pendingBatch:
		first := q.batch
		q.state, q.batch = pendingFolding, nil
		q.ops, q.leaves = []Op{}, map[string]int{}
		if q.fold(first) {
			q.fold(ops)
		}
	case pendingFolding:
		q.fold(ops)
	}
}

// reset drops everything pending and enters state, holding batch.
func (q *PendingOps) reset(state pendingState, batch []Op) {
	*q = PendingOps{state: state, batch: batch}
}

// Size is how many ops are pending after folding; 1 once overflowed (the
// replacement the owner sends). One batch is counted as received, so a single
// commit larger than MaxPendingOps counts in full until a second one arrives
// and overflows.
func (q *PendingOps) Size() int {
	switch q.state {
	case pendingBatch:
		return len(q.batch)
	case pendingFolding:
		return len(q.ops)
	case pendingReplace:
		return 1
	}
	return 0
}

// Take hands over everything pending and leaves the queue empty.
func (q *PendingOps) Take() PendingTake {
	state, batch, ops, joined := q.state, q.batch, q.ops, q.joined
	q.reset(pendingEmpty, nil)
	switch state {
	case pendingBatch:
		return PendingTake{Ops: batch}
	case pendingFolding:
		for at, text := range joined {
			ops[at] = withText(ops[at], text.String())
		}
		return PendingTake{Ops: ops}
	case pendingReplace:
		return PendingTake{Replace: true}
	}
	return PendingTake{Ops: []Op{}}
}

// fold folds ops into the folding state; false once the queue overflowed
// into pendingReplace.
func (q *PendingOps) fold(ops []Op) bool {
	for _, op := range ops {
		switch o := op.(type) {
		case Replace:
			clear(q.ops)
			q.ops = q.ops[:0]
			clear(q.leaves)
			clear(q.joined)
		case Append:
			if q.mergeLeaf(o.Path, op) {
				continue
			}
		case Set:
			if !isContainer(o.Value) {
				if q.mergeLeaf(o.Path, op) {
					continue
				}
			} else {
				clear(q.leaves)
			}
		default:
			clear(q.leaves)
		}
		q.ops = append(q.ops, op)
		if len(q.ops) > MaxPendingOps {
			q.reset(pendingReplace, nil)
			return false
		}
	}
	return true
}

// mergeLeaf merges the leaf op at path into the last pending leaf op on that
// path, or indexes it as that op at the position it is about to be pushed
// to. true when merged.
func (q *PendingOps) mergeLeaf(path Path, op Op) bool {
	key := pathKey(path)
	if at, ok := q.leaves[key]; ok {
		// The index points at leaf ops only.
		if q.merge(at, op) {
			return true
		}
	}
	q.leaves[key] = len(q.ops)
	return false
}

// pathKey is the key of a path in the leaf index. Injective, as merging two
// distinct paths would lose a write: ["a.b"] and ["a", "b"] are different
// keys, and so are a key and the index it spells ("1" and 1), as upstream's
// JSON.stringify keeps them apart.
func pathKey(path Path) string {
	var b strings.Builder
	for i, seg := range path {
		if i > 0 {
			b.WriteByte(',')
		}
		switch s := seg.(type) {
		case Key:
			b.WriteString(strconv.Quote(string(s)))
		case Index:
			b.WriteString(strconv.Itoa(int(s)))
		}
	}
	return b.String()
}

// merge folds next into the leaf op at position at (same path); false when
// they do not fold. A Set replaces what is there; an Append joins an Append,
// or a Set of a string, and the text it makes is joined[at] until Take.
func (q *PendingOps) merge(at int, next Op) bool {
	a, ok := next.(Append)
	if !ok {
		q.ops[at] = next
		delete(q.joined, at)
		return true
	}
	if b := q.joined[at]; b != nil {
		b.WriteString(a.Text)
		return true
	}
	var text string
	switch p := q.ops[at].(type) {
	case Append:
		text = p.Text
	case Set:
		// Only a string set takes an append; the tracker never appends to
		// another scalar.
		s, ok := p.Value.(string)
		if !ok {
			return false
		}
		text = s
	default:
		return false
	}
	b := &strings.Builder{}
	b.Grow(len(text) + len(a.Text))
	b.WriteString(text)
	b.WriteString(a.Text)
	if q.joined == nil {
		q.joined = map[int]*strings.Builder{}
	}
	q.joined[at] = b
	return true
}

// withText is the leaf op op with its text replaced: upstream's merged
// Append of the joined text, or Set of the string.
func withText(op Op, text string) Op {
	switch o := op.(type) {
	case Append:
		return Append{Path: o.Path, Text: text}
	case Set:
		return Set{Path: o.Path, Value: text}
	}
	return op
}
