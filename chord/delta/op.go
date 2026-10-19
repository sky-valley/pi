package delta

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidOp is wrapped by every shape rejection — wrong arity, a payload of
// the wrong kind, a path that is not an array, an unknown verb. Path safety
// failures are *UnsafePathError instead; a path that does not resolve is
// *PathError.
var ErrInvalidOp = errors.New("invalid op")

// tuple is what Op and WireOp have in common: each value is one JSON tuple
// whose first element is its verb, and each can check its own constraints.
type tuple interface {
	json.Marshaler
	Validate() error
}

// Op is one decoded operation — the form a Prepared change carries and Apply
// accepts. Tuples are the form on the wire and on disk; in memory each verb is
// its own type:
//
//	Replace   ["r", value]                          replace the complete value
//	Set       ["s", path, value]                    set a property or element
//	Delete    ["d", path]                           delete an object property
//	Append    ["a", path, text]                     append to a string
//	Truncate  ["t", path, count]                    drop UTF-16 code units from a string's front
//	Splice    ["p", path, index, remove, items]     splice an array
//	Permute   ["m", path, permutation]              reorder an array: new[i] = old[permutation[i]]
//
// Replace is the ONLY op that replaces a whole value. Set, Delete, Append and
// Truncate cannot target the root: Validate forbids the empty path. Splice and
// Permute may, because a tracked value can itself be an array. Operation shape
// is not canonical: an array may be emptied by either a replacement or a root
// splice.
//
// Op knows nothing about the path dictionary. Interning, id references and
// omitted paths live in WireOp and exist only between encode and decode.
type Op interface {
	tuple
	// op seals the set to the seven decoded verbs.
	op()
}

// WireOp is what crosses a boundary. It adds two compressions to Op and
// nothing else:
//
//	Define      ["#", id, path]    defines an id, emitted on a path's SECOND use
//	a PathID    in place of a path references a previously defined id
//	a nil Ref   reuses the previous op's path in this batch; arity disambiguates
//
// Replace carries no path, so it encodes to itself — which is why IsBase works
// unchanged on either vocabulary. Never pass wire ops to an applier: a
// two-element ["s", value] would be read with the value as its path.
type WireOp interface {
	tuple
	// wireOp seals the set to Replace, Define and the six Wire* verbs.
	wireOp()
}

// ─── Decoded vocabulary ──────────────────────────────────────────────────────

// Replace is ["r", value]: the complete value. It opens every base batch and
// is the recovery point a reader replays from with a fresh decoder. Value is
// a JSON tree as encoding/json decodes one: nil, bool, float64, string,
// []any, map[string]any. It is adopted by an applier, never copied, and never
// inspected by a validator.
type Replace struct{ Value any }

// Set is ["s", path, value]: set an object property or array element. An
// index may address an existing element or the one slot past the end.
type Set struct {
	Path  Path
	Value any
}

// Delete is ["d", path]: delete an object property. On an array it removes
// the element, and the index must exist.
type Delete struct{ Path Path }

// Append is ["a", path, text]: append to a string.
type Append struct {
	Path Path
	Text string
}

// Truncate is ["t", path, count]: remove count UTF-16 code units from a
// string's front. The unit is the wire's, not Go's — a Go applier must count
// code units, not bytes or runes, or the two sides of a rolling window drift.
type Truncate struct {
	Path  Path
	Count int
}

// Splice is ["p", path, index, remove, items]: remove `remove` elements at
// index and insert items there, with Array.prototype.splice's clamping of a
// remove count past the end. Path may be empty: the root itself can be an
// array.
type Splice struct {
	Path          Path
	Index, Remove int
	Items         []any
}

// Permute is ["m", path, permutation]: reorder an array in place, so that
// new[i] = old[Permutation[i]]. Permutation must be a bijection on the
// array's indices, and the array must have exactly its length. Path may be
// empty: the root itself can be an array.
type Permute struct {
	Path        Path
	Permutation []int
}

func (Replace) op()  {}
func (Set) op()      {}
func (Delete) op()   {}
func (Append) op()   {}
func (Truncate) op() {}
func (Splice) op()   {}
func (Permute) op()  {}

// Validate always succeeds: a replacement has no path and its payload is not
// inspected.
func (Replace) Validate() error { return nil }

// Validate rejects the root and any unsafe segment.
func (op Set) Validate() error { return nonEmpty("s", op.Path) }

// Validate rejects the root and any unsafe segment.
func (op Delete) Validate() error { return nonEmpty("d", op.Path) }

// Validate rejects the root and any unsafe segment.
func (op Append) Validate() error { return nonEmpty("a", op.Path) }

// Validate rejects the root, any unsafe segment, and a negative count.
func (op Truncate) Validate() error {
	if err := nonEmpty("t", op.Path); err != nil {
		return err
	}
	return nonNegative("t", "count", op.Count)
}

// Validate rejects an unsafe segment and a negative index or remove count.
// The root path is legal here.
func (op Splice) Validate() error {
	if err := op.Path.Validate(); err != nil {
		return err
	}
	return spliceBounds(op.Index, op.Remove)
}

// Validate rejects an unsafe segment and a permutation that is not a
// bijection. The root path is legal here.
func (op Permute) Validate() error {
	if err := op.Path.Validate(); err != nil {
		return err
	}
	return bijection(op.Permutation)
}

func (op Replace) MarshalJSON() ([]byte, error) { return marshalJSON([]any{"r", op.Value}) }
func (op Set) MarshalJSON() ([]byte, error)     { return marshalJSON([]any{"s", op.Path, op.Value}) }
func (op Delete) MarshalJSON() ([]byte, error)  { return marshalJSON([]any{"d", op.Path}) }
func (op Append) MarshalJSON() ([]byte, error)  { return marshalJSON([]any{"a", op.Path, op.Text}) }
func (op Truncate) MarshalJSON() ([]byte, error) {
	return marshalJSON([]any{"t", op.Path, op.Count})
}
func (op Splice) MarshalJSON() ([]byte, error) {
	return marshalJSON([]any{"p", op.Path, op.Index, op.Remove, nonNilItems(op.Items)})
}
func (op Permute) MarshalJSON() ([]byte, error) {
	return marshalJSON([]any{"m", op.Path, nonNilPermutation(op.Permutation)})
}

// ─── Wire vocabulary ─────────────────────────────────────────────────────────

// Define is ["#", id, path]: bind id to path for the rest of the stream, until
// a Replace resets the dictionary.
type Define struct {
	ID   PathID
	Path Path
}

// WireSet is ["s", pathRef, value], or ["s", value] when Ref is nil.
type WireSet struct {
	Ref   PathRef
	Value any
}

// WireDelete is ["d", pathRef], or ["d"] when Ref is nil.
type WireDelete struct{ Ref PathRef }

// WireAppend is ["a", pathRef, text], or ["a", text] when Ref is nil.
type WireAppend struct {
	Ref  PathRef
	Text string
}

// WireTruncate is ["t", pathRef, count], or ["t", count] when Ref is nil.
type WireTruncate struct {
	Ref   PathRef
	Count int
}

// WireSplice is ["p", pathRef, index, remove, items], or
// ["p", index, remove, items] when Ref is nil.
type WireSplice struct {
	Ref           PathRef
	Index, Remove int
	Items         []any
}

// WirePermute is ["m", pathRef, permutation], or ["m", permutation] when Ref
// is nil.
type WirePermute struct {
	Ref         PathRef
	Permutation []int
}

func (Replace) wireOp()      {}
func (Define) wireOp()       {}
func (WireSet) wireOp()      {}
func (WireDelete) wireOp()   {}
func (WireAppend) wireOp()   {}
func (WireTruncate) wireOp() {}
func (WireSplice) wireOp()   {}
func (WirePermute) wireOp()  {}

// Validate rejects a bad id and an unsafe path.
func (op Define) Validate() error {
	if err := validateRef(op.ID); err != nil {
		return err
	}
	return op.Path.Validate()
}

// Validate rejects an unsafe path or a bad id. A nil Ref is the short form
// and is legal; whether a previous path exists is the decoder's check.
func (op WireSet) Validate() error { return validateRef(op.Ref) }

// Validate rejects an unsafe path or a bad id.
func (op WireDelete) Validate() error { return validateRef(op.Ref) }

// Validate rejects an unsafe path or a bad id.
func (op WireAppend) Validate() error { return validateRef(op.Ref) }

// Validate rejects an unsafe path, a bad id, and a negative count.
func (op WireTruncate) Validate() error {
	if err := validateRef(op.Ref); err != nil {
		return err
	}
	return nonNegative("t", "count", op.Count)
}

// Validate rejects an unsafe path, a bad id, and negative bounds.
func (op WireSplice) Validate() error {
	if err := validateRef(op.Ref); err != nil {
		return err
	}
	return spliceBounds(op.Index, op.Remove)
}

// Validate rejects an unsafe path, a bad id, and a permutation that is not a
// bijection.
func (op WirePermute) Validate() error {
	if err := validateRef(op.Ref); err != nil {
		return err
	}
	return bijection(op.Permutation)
}

func (op Define) MarshalJSON() ([]byte, error) { return marshalJSON([]any{"#", op.ID, op.Path}) }
func (op WireSet) MarshalJSON() ([]byte, error) {
	return marshalJSON(withRef("s", op.Ref, op.Value))
}
func (op WireDelete) MarshalJSON() ([]byte, error) { return marshalJSON(withRef("d", op.Ref)) }
func (op WireAppend) MarshalJSON() ([]byte, error) {
	return marshalJSON(withRef("a", op.Ref, op.Text))
}
func (op WireTruncate) MarshalJSON() ([]byte, error) {
	return marshalJSON(withRef("t", op.Ref, op.Count))
}
func (op WireSplice) MarshalJSON() ([]byte, error) {
	return marshalJSON(withRef("p", op.Ref, op.Index, op.Remove, nonNilItems(op.Items)))
}
func (op WirePermute) MarshalJSON() ([]byte, error) {
	return marshalJSON(withRef("m", op.Ref, nonNilPermutation(op.Permutation)))
}

// withRef builds the tuple, omitting the ref slot for the short form.
func withRef(verb string, ref PathRef, rest ...any) []any {
	out := make([]any, 0, 2+len(rest))
	out = append(out, verb)
	if ref != nil {
		out = append(out, ref)
	}
	return append(out, rest...)
}

// items never writes null: a nil slice is an empty splice payload.
func nonNilItems(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

// nonNilPermutation never writes null: a nil slice is the empty permutation.
func nonNilPermutation(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

// ─── Classification ──────────────────────────────────────────────────────────

// IsReplace reports whether op is a Replace, in either vocabulary.
func IsReplace[O tuple](op O) bool {
	switch any(op).(type) {
	case Replace, *Replace:
		return true
	}
	return false
}

// IsBase reports whether a batch begins with a replacement. DiffRevisions puts
// a Replace at index 0 or nowhere, so this is exact rather than a heuristic.
// It accepts either vocabulary because Replace encodes to itself.
func IsBase[O tuple](ops []O) bool {
	return len(ops) > 0 && IsReplace(ops[0])
}

// ─── Validators ──────────────────────────────────────────────────────────────

// ParseOp is upstream's assertValidOp: it checks verb, arity and payload shape
// of a DECODED op given as an any-tree (paths inline, no "#", no short forms)
// and returns it typed. A receiver runs it on every op it is handed; an
// applier's own checks run on the typed value.
//
// Validating an Op against the wire grammar would be laxer than the type: a
// two-element ["s", value] would pass, and apply would then read the value as a
// path. Each vocabulary gets the validator that matches it.
//
// Payloads are not inspected: a Replace, Set or Splice value is carried as-is.
func ParseOp(v any) (Op, error) {
	t, verb, err := tupleOf(v)
	if err != nil {
		return nil, err
	}
	var op Op
	switch verb {
	case "r":
		if err := arity(t, 2, `["r", value]`); err != nil {
			return nil, err
		}
		op = Replace{Value: t[1]}
	case "s":
		if err := arity(t, 3, `["s", path, value]`); err != nil {
			return nil, err
		}
		path, err := parsePath(t[1])
		if err != nil {
			return nil, err
		}
		op = Set{Path: path, Value: t[2]}
	case "d":
		if err := arity(t, 2, `["d", path]`); err != nil {
			return nil, err
		}
		path, err := parsePath(t[1])
		if err != nil {
			return nil, err
		}
		op = Delete{Path: path}
	case "a":
		if err := arity(t, 3, `["a", path, text]`); err != nil {
			return nil, err
		}
		path, err := parsePath(t[1])
		if err != nil {
			return nil, err
		}
		s, err := parseText(t[2])
		if err != nil {
			return nil, err
		}
		op = Append{Path: path, Text: s}
	case "t":
		if err := arity(t, 3, `["t", path, count]`); err != nil {
			return nil, err
		}
		path, err := parsePath(t[1])
		if err != nil {
			return nil, err
		}
		n, err := parseCount("t", "count", t[2])
		if err != nil {
			return nil, err
		}
		op = Truncate{Path: path, Count: n}
	case "p":
		if err := arity(t, 5, `["p", path, index, remove, items]`); err != nil {
			return nil, err
		}
		path, err := parsePath(t[1])
		if err != nil {
			return nil, err
		}
		index, remove, payload, err := spliceArgs(t[2], t[3], t[4])
		if err != nil {
			return nil, err
		}
		op = Splice{Path: path, Index: index, Remove: remove, Items: payload}
	case "m":
		if err := arity(t, 3, `["m", path, permutation]`); err != nil {
			return nil, err
		}
		path, err := parsePath(t[1])
		if err != nil {
			return nil, err
		}
		permutation, err := parsePermutation(t[2])
		if err != nil {
			return nil, err
		}
		op = Permute{Path: path, Permutation: permutation}
	default:
		// Silently skipping an unknown verb is how a newer producer's op vanishes.
		return nil, fmt.Errorf("%w: unknown op verb %s (decoded ops are r, s, d, a, t, p, m; run decode first if this came from the wire)", ErrInvalidOp, describe(t[0]))
	}
	if err := op.Validate(); err != nil {
		return nil, err
	}
	return op, nil
}

// ParseWireOp is upstream's assertValidWireOp: the same, for the wire grammar,
// where ids and short forms are legal. A decoder runs it on every incoming
// tuple before resolving paths.
func ParseWireOp(v any) (WireOp, error) {
	t, verb, err := tupleOf(v)
	if err != nil {
		return nil, err
	}
	var op WireOp
	switch verb {
	case "r":
		if err := arity(t, 2, `["r", value]`); err != nil {
			return nil, err
		}
		op = Replace{Value: t[1]}
	case "s":
		ref, rest, err := refArgs(t, 1, `["s", pathRef, value] or ["s", value]`)
		if err != nil {
			return nil, err
		}
		op = WireSet{Ref: ref, Value: rest[0]}
	case "d":
		ref, _, err := refArgs(t, 0, `["d", pathRef] or ["d"]`)
		if err != nil {
			return nil, err
		}
		op = WireDelete{Ref: ref}
	case "a":
		ref, rest, err := refArgs(t, 1, `["a", pathRef, text] or ["a", text]`)
		if err != nil {
			return nil, err
		}
		s, err := parseText(rest[0])
		if err != nil {
			return nil, err
		}
		op = WireAppend{Ref: ref, Text: s}
	case "t":
		ref, rest, err := refArgs(t, 1, `["t", pathRef, count] or ["t", count]`)
		if err != nil {
			return nil, err
		}
		n, err := parseCount("t", "count", rest[0])
		if err != nil {
			return nil, err
		}
		op = WireTruncate{Ref: ref, Count: n}
	case "p":
		ref, rest, err := refArgs(t, 3, `["p", pathRef, index, remove, items] or ["p", index, remove, items]`)
		if err != nil {
			return nil, err
		}
		index, remove, payload, err := spliceArgs(rest[0], rest[1], rest[2])
		if err != nil {
			return nil, err
		}
		op = WireSplice{Ref: ref, Index: index, Remove: remove, Items: payload}
	case "m":
		ref, rest, err := refArgs(t, 1, `["m", pathRef, permutation] or ["m", permutation]`)
		if err != nil {
			return nil, err
		}
		permutation, err := parsePermutation(rest[0])
		if err != nil {
			return nil, err
		}
		op = WirePermute{Ref: ref, Permutation: permutation}
	case "#":
		if err := arity(t, 3, `["#", id, path]`); err != nil {
			return nil, err
		}
		// An address, as the id a path ref names is: exact past 2^31 on every
		// platform, and Validate checks its range.
		id, ok := integer(t[1])
		if !ok {
			return nil, fmt.Errorf("%w: \"#\" id must be a non-negative integer, got %s", ErrInvalidOp, describe(t[1]))
		}
		path, err := parsePath(t[2])
		if err != nil {
			return nil, err
		}
		op = Define{ID: PathID(id), Path: path}
	default:
		// Silently skipping an unknown verb is how a newer producer's op vanishes.
		return nil, fmt.Errorf("%w: unknown wire op verb %s (wire ops are r, s, d, a, t, p, m, #)", ErrInvalidOp, describe(t[0]))
	}
	if err := op.Validate(); err != nil {
		return nil, err
	}
	return op, nil
}

// UnmarshalOp decodes one JSON tuple as a decoded op.
func UnmarshalOp(data []byte) (Op, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return ParseOp(v)
}

// UnmarshalWireOp decodes one JSON tuple as a wire op.
func UnmarshalWireOp(data []byte) (WireOp, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return ParseWireOp(v)
}

// tupleOf requires a non-empty array. The verb is returned as a string; a
// non-string verb is simply an unknown one and is reported by the caller.
func tupleOf(v any) ([]any, string, error) {
	t, ok := v.([]any)
	if !ok || len(t) == 0 {
		return nil, "", fmt.Errorf("%w: op is not a tuple: want a JSON array [verb, ...], got %s", ErrInvalidOp, describe(v))
	}
	verb, _ := t[0].(string)
	return t, verb, nil
}

func arity(t []any, want int, shape string) error {
	if len(t) != want {
		return fmt.Errorf("%w: %q op expects %s (%d elements), got %d", ErrInvalidOp, t[0], shape, want, len(t))
	}
	return nil
}

// refArgs splits a wire tuple into its optional path ref and the `n` trailing
// arguments. Arity tells whether the ref is present: the short form omits it.
func refArgs(t []any, n int, shape string) (PathRef, []any, error) {
	switch len(t) {
	case 1 + n:
		return nil, t[1:], nil
	case 2 + n:
		ref, err := parseRef(t[1])
		if err != nil {
			return nil, nil, err
		}
		return ref, t[2:], nil
	default:
		return nil, nil, fmt.Errorf("%w: %q op expects %s, got %d elements", ErrInvalidOp, t[0], shape, len(t))
	}
}

// parseRef reads a path ref: an integer id, or an inline path. Shape only —
// the id's range is validateRef's.
//
// A string is not a path. Unchecked, "a".slice(0, -1) is "", so it resolves to
// the ROOT and writes there — a path that is not a path, accepted.
func parseRef(v any) (PathRef, error) {
	if id, ok := v.(PathID); ok {
		return id, nil
	}
	if id, ok := integer(v); ok {
		return PathID(id), nil
	}
	if isNumber(v) {
		return nil, fmt.Errorf("%w: path id must be a non-negative integer, got %s", ErrInvalidOp, describe(v))
	}
	return parsePath(v)
}

// validateRef is the constraint on a path ref: a safe path, or an id within
// Number.MAX_SAFE_INTEGER — the ids an encoder assigns count up from 0, and a
// double cannot address one beyond that exactly. A nil ref is the short form
// and is legal; whether a previous path exists is the decoder's check.
func validateRef(ref PathRef) error {
	switch r := ref.(type) {
	case nil:
		return nil
	case Path:
		return r.Validate()
	case PathID:
		if r < 0 || r > maxSafeInteger {
			return fmt.Errorf("%w: path id must be a non-negative integer within Number.MAX_SAFE_INTEGER, got %d", ErrInvalidOp, r)
		}
		return nil
	default:
		return fmt.Errorf("%w: path ref must be a Path or PathID, got %T", ErrInvalidOp, ref)
	}
}

func parseText(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: \"a\" text must be a string, got %s", ErrInvalidOp, describe(v))
	}
	return s, nil
}

// parseCount reads an integer count or splice index: a quantity, never an
// address. Shape only: its sign is the op's Validate to check, and a magnitude
// past what an int holds saturates — pi's apply clamps a count against a
// length, so 1e300 and math.MaxInt truncate or remove the same.
func parseCount(verb, name string, v any) (int, error) {
	n, ok := integer(v)
	if !ok {
		return 0, fmt.Errorf("%w: %q %s must be a non-negative integer, got %s", ErrInvalidOp, verb, name, describe(v))
	}
	return clampInt(n), nil
}

func spliceArgs(i, r, itemsArg any) (index, remove int, items []any, err error) {
	if index, err = parseCount("p", "index", i); err != nil {
		return 0, 0, nil, err
	}
	if remove, err = parseCount("p", "remove", r); err != nil {
		return 0, 0, nil, err
	}
	items, ok := itemsArg.([]any)
	if !ok {
		// Unvalidated, an applier would spread a string: ["n","o","t","-","a",…]
		return 0, 0, nil, fmt.Errorf("%w: \"p\" items must be an array, got %s", ErrInvalidOp, describe(itemsArg))
	}
	return index, remove, items, nil
}

// parsePermutation reads an "m" permutation: an array of integers. Shape only —
// whether it is a bijection is the op's Validate to check. An element that is
// not an integer can be no index at all, so it is reported as upstream reports
// it: the permutation is not a bijection.
func parsePermutation(v any) ([]int, error) {
	xs, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: \"m\" permutation is not an array: want [index, ...] naming each index of the target array exactly once, got %s", ErrInvalidOp, describe(v))
	}
	out := make([]int, len(xs))
	for i, x := range xs {
		n, ok := integer(x)
		if !ok {
			return nil, fmt.Errorf("%w: \"m\" permutation is not a bijection: element %d is %s, want an integer index in [0, %d)", ErrInvalidOp, i, describe(x), len(xs))
		}
		out[i] = clampInt(n)
	}
	return out, nil
}

// bijection is upstream's assertPermutation: each of 0..len-1 exactly once.
// Unchecked, a duplicate would copy one element into two slots and drop
// another, and an index out of range would read past the array.
func bijection(permutation []int) error {
	seen := make([]bool, len(permutation))
	for i, n := range permutation {
		if n < 0 || n >= len(permutation) || seen[n] {
			return fmt.Errorf("%w: \"m\" permutation is not a bijection: element %d is %d, want each index in [0, %d) exactly once", ErrInvalidOp, i, n, len(permutation))
		}
		seen[n] = true
	}
	return nil
}

func spliceBounds(index, remove int) error {
	if err := nonNegative("p", "index", index); err != nil {
		return err
	}
	return nonNegative("p", "remove", remove)
}

func nonNegative(verb, name string, n int) error {
	if n < 0 {
		return fmt.Errorf("%w: %q %s must be a non-negative integer, got %d", ErrInvalidOp, verb, name, n)
	}
	return nil
}

// nonEmpty enforces NonEmptyPath: s, d, a and t cannot address the root.
func nonEmpty(verb string, p Path) error {
	if len(p) == 0 {
		return fmt.Errorf("%w: %q path is empty; only \"r\" replaces the root (and \"p\" and \"m\" may splice or permute a root array)", ErrInvalidOp, verb)
	}
	return p.Validate()
}
