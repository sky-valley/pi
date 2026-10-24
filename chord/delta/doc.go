// Package delta synchronizes JSON values from an authoritative producer to an
// ordered replica. It mirrors @earendil-works/chord/delta
// (packages/chord/src/delta at 9e70c3d50) and depends on nothing else in the
// port: session storage, the runtime and the facet host consume it, and the
// arrows point that way.
//
// A change is an Op: a JSON tuple for replacing, setting, deleting, updating a
// string, splicing an array, or permuting an array. A producer keeps its value
// in a Tracker as immutable revisions and publishes the ops between one
// revision and the next; a replica applies them.
//
// # Operation vocabulary
//
// A path is an array of object keys and array indices, root first:
//
//	["operation", "message", "content", 0, "text"]
//
// Decoded ops (Op) carry complete inline paths:
//
//	["r", value]                        Replace the complete value.
//	["s", path, value]                  Set a property or array element.
//	["d", path]                         Delete an object property.
//	["a", path, text]                   Append to a string.
//	["t", path, count]                  Remove UTF-16 code units from a string's front.
//	["p", path, index, remove, items]   Splice an array.
//	["m", path, permutation]            Reorder an array so new[i] = old[permutation[i]].
//
// Except for "r", every decoded op carries its complete path. "s", "d", "a"
// and "t" cannot address the root; "p" and "m" may address a root array.
//
// Wire ops (WireOp) add path interning and omission, and nothing else:
//
//	["r", value]                           Identical to the decoded form.
//	["#", id, path]                        Define a numeric path id.
//	["s", pathRef, value]  ["s", value]    Inline or interned path; or the previous op's path.
//	["d", pathRef]         ["d"]
//	["a", pathRef, text]   ["a", text]
//	["t", pathRef, count]  ["t", count]
//	["p", pathRef, index, remove, items]   ["p", index, remove, items]
//	["m", pathRef, permutation]            ["m", permutation]
//
// The vocabulary is deliberately not RFC 6902. String append and front-truncate
// let a rolling output window ship as two small ops instead of a whole-value
// set; splice carries its items; a whole-value "r" doubles as the recovery
// point a reader replays from; and path interning is stateful across batches.
//
// Encoding is optional for local application. Never pass wire ops to an
// applier: the two grammars overlap, and a two-element ["s", value] read as a
// decoded op has the value as its path. Op and WireOp are distinct types with
// distinct validators (ParseOp, ParseWireOp) for exactly that reason.
//
// # Producing changes
//
// A Tracker holds one committed revision. A change is a transaction against
// it:
//
//	change, err := tracker.BeginChange()
//	state := change.State()                      // the root draft
//	err = state.Set("status", "running")
//	_, err = state.At("rows").Push(row)
//	prepared, err := change.Prepare()            // or change.Abort()
//	err = tracker.Adopt(prepared)                // commit
//	publish(prepared.Ops())
//
// A change is an overlay on the committed revision, which it never modifies:
// each container the draft reads gets a node that records the writes,
// deletions and structural edits made through it. Prepare emits the ops those
// records mean — in the order the change made them, shallowest first, each
// edit inside an array at its entry's final index — and materializes the
// revision they produce; writes that cancel out publish nothing and leave the
// committed revision's identity intact. Adopt commits exactly what was
// prepared, by swapping the root, so a runtime can persist the batch before it
// adopts, or drop it. PrepareReplace prepares a whole new value: no ops when it
// equals the committed revision, one Replace otherwise.
//
// Any number of changes may be open or prepared against one revision.
// Adopting one makes every other stale: an open change's drafts are settled
// and it cannot be prepared, and Adopt refuses a prepared one. Adopt also
// refuses a prepared change from another tracker, one already adopted, and one
// aborted.
//
// Strings publish as appends and front-truncations where they can; arrays as
// splices of the entries removed and inserted, a permutation of the entries
// kept, and sets of the entries written over; objects as sets and deletes.
// Many edits across one array fold into a splice of the region they cover, an
// edit below a reserved key into a set of its nearest safe ancestor, and a
// batch of more than 4,096 ops into one Replace. The op sequence is exact but
// not canonical: depend on the resulting value, never on the exact tuples.
//
// # Drafts
//
// Upstream's draft is a Proxy: plain JavaScript reads, writes and array method
// calls on the overlay. Go has no Proxy, so a *Draft is the handler the Proxy
// would call, and each trap is a method:
//
//	draft.key                        d.Get(key)  (value, present); a member object or array is a *Draft
//	draft.key (an object or array)   d.At(key)   the member's draft, or nil
//	draft.key = value                d.Set(key, value)
//	delete draft.key                 d.Delete(key)
//	key in draft                     d.Has(key)
//	Object.keys(draft)               d.Keys()
//	draft.length                     d.Len(); on an array, Get("length") too
//	draft.length = n                 d.SetLen(n); on an array, Set("length", v) too, v converted as JavaScript does
//	draft.push(...items)             d.Push(items...)
//	draft.pop() / draft.shift()      d.Pop() / d.Shift()   (value, present, err)
//	draft.unshift(...items)          d.Unshift(items...)
//	draft.splice(start, n, ...items) d.Splice(start, n, items...)
//	draft.sort(compare)              d.Sort(compare); nil is JavaScript's default string order
//	draft.reverse()                  d.Reverse()
//	draft.fill(v, start, end)        d.Fill(v, start, end)
//	draft.copyWithin(t, start, end)  d.CopyWithin(t, start, end)
//	appendText(draft, key, text)     d.AppendText(key, text)
//
// A draft belongs to a container, not a position: reading a member twice
// yields the same *Draft, and a held draft follows its entry through sorting,
// reversal and insertion — or out of the tree, after which writes through it
// are ignored. Every value placed — by Set, Push, Unshift, Splice, Fill or
// CopyWithin — is checked and deep-copied at once, and a refused one leaves the
// draft as it was; placing a *Draft copies what that draft holds now, and one
// value placed twice becomes two. A draft is usable until its change settles:
// after that a write returns ErrDraftSettled, and a read — which has no error
// to return — panics with it.
//
// Where upstream throws a TypeError from a trap, the Go method returns an
// error carrying upstream's text. Arrays stay dense: writing past the next
// index, deleting an element and writing a named property are refused;
// growing the length inserts nulls, and shrinking it removes elements.
//
// # Growing strings
//
// A draft sees only the finished new string, never the chunk, so
//
//	state.Set("text", current+chunk)
//
// publishes ["a", ["text"], chunk] by checking, in Prepare, that the new
// string extends the old one: a read of the whole string per change. A
// writer that knows what it appended hands Delta the chunk instead:
//
//	state.AppendText("text", chunk)  // records ["a", path, chunk], no compare
//
// AppendText takes the parent draft and the member's key, and refuses a member
// that is not a string. Several appends to one key in one change become one
// op; a Set or Delete of the key in that change falls back to the compare, as
// does an append after a Set. The text is copied, so neither the op nor the new
// value keeps a larger string alive. Use it wherever a string grows by appends
// across many changes — streamed answers, tool output, logs; a one-off
// assignment is fine with Set. DiffRevisions compares two revisions it did not
// write, so a derive producing a growing string pays the compare per delivery.
//
// The saving is smaller than pi's. pi measures 20,000 changes of 40
// characters, up to 800 KB, at 2.4 s with the compare and 1 ms without, because
// a V8 string append is a rope. A Go string is flat, so each change copies the
// whole new string either way, and AppendText saves only the compare: about a
// sixth of that same stream's time here.
//
// # Revisions
//
// Every committed revision — Tracker.Value, and the Base and Value of each
// Prepared — is immutable and shares its unchanged subtrees with the revisions
// around it; the ops' payloads may be the same containers as parts of Value.
// Immutability is an ownership contract, in pi as in Go: nothing is frozen or
// copied defensively, so never modify one — which includes never handing one
// to Apply (see Streams). Track and PrepareReplace take ownership of the root
// they are handed in O(1), without walking it: it must be alias-free strict
// JSON in the representation encoding/json decodes (no nil map or slice, which
// encoding/json writes as null), and the caller must not touch it again —
// chord.CopyJSON makes one from any Go value.
//
// Object identity is not replicated: a replica holds a distinct value at each
// path. Nor is object key order, entirely. JavaScript enumerates an object's
// keys integer-like first (ascending), then in insertion order; a Go map keeps
// none, so DiffRevisions visits a revision's members — and Draft.Keys lists
// them — in the order of an object whose keys were inserted sorted (the rest
// by UTF-16 code unit), and Draft.Keys lists the string keys a change added
// after them, in the order it added them. A tracker's batches follow the order
// of the change's edits, as pi's do. encoding/json marshals a payload's members
// in byte order instead. The replica is the same either way.
//
// # Paths and safety
//
// Path segments are Key and Index. Three keys are reserved as segments —
// ReservedSegments — because a replica in pi applies parent[key] = value, and
// a path is data: a Go producer emitting ["s", ["__proto__", "isAdmin"], true]
// hands a TypeScript replica a prototype-pollution primitive. Neither the
// tracker nor the diff ever emits one — an edit at or below a reserved key is
// published by setting the nearest safe ancestor whole — and wire validators
// and appliers refuse them. As VALUE keys they are fine; a value is written
// whole and never walked.
//
// # Streams
//
// An encoder and decoder are stateful. Use one pair per ordered stream: the
// encoder assigns ids to paths used across batches, the decoder remembers the
// definitions, and a Replace resets both dictionaries so replay can begin at
// that batch with a fresh decoder. A producer starts a stream — or starts it
// over — with a base batch: []Op{Replace{Value: tracker.Value()}}. A decode or
// apply error terminates the stream; discard its decoder and replica and
// recover from a later base batch.
//
// A tracker's batches are made of its revisions: the payloads of
// Prepared.Ops share Prepared.Value's containers, and a base batch carries
// the committed revision itself, and so does the encoder's output for them.
// Apply builds its replica out of what it is handed and writes into it, so
// in-process it would modify the tracker's revisions — silently, in pi as
// here — and the tracker would go on publishing batches against a revision its
// replicas no longer match. Apply a tracker's batches in-process with
// ApplyImmutable, as pi's own replicas do, or detach them first; a batch that
// was marshalled and decoded on the way is already detached.
//
// ApplyImmutable copies each container a batch touches once, so one batch can
// fan out to any number of immutable replicas; each must hold the batch's
// base. When only the final result of an ordered backlog is needed, replay it
// with ApplyImmutableBatches rather than joining the batches' ops: one
// copy-on-write scope spans the call, so no intermediate revision is exposed,
// or safe to retain. Call ApplyImmutable per batch when every revision is
// published or kept.
//
// # Slow consumers
//
// A PendingOps holds the batches one consumer has not taken yet — a watch
// listener still running, a link whose window is full — and folds them while
// they wait, so what it hands over replays to the same value with fewer ops:
//
//	pending.Push(prepared.Ops())  // for each adopted batch, while the consumer is busy
//	taken := pending.Take()       // when it is ready again
//	ops := taken.Ops
//	if taken.Replace {
//		ops = []Op{Replace{Value: tracker.Value()}}
//	}
//
// A leaf op (an append, or a set of a scalar) folds into the last pending leaf
// op on its path; any other op ends folding into the ops before it, as it may
// change what a path names, and a Replace drops them. Past MaxPendingOps
// pending ops after folding, only Replace stays. One batch is handed over as
// received, and received batches are never modified, so one batch can feed
// several consumers. It folds the ops of a tracker, DiffRevisions and
// AppendText; it does not validate or normalize hand-written ops.
//
// # JSON in Go
//
// A value is the tree encoding/json produces: nil, bool, float64, string,
// []any, map[string]any. Validators do not inspect payloads; they check the
// tuple's shape and the path. Every op marshals to its tuple; ParseOp and
// ParseWireOp classify an any-tree, and UnmarshalOp and UnmarshalWireOp do the
// same from bytes.
//
// A Tracker, its Changes and their drafts, an Encoder and a Decoder each
// belong to one goroutine at a time; none is safe for concurrent use.
package delta
