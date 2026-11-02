package schema

import (
	"maps"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// Coercion (coerce.ts): turns a loosely typed value (a model's tool call
// arguments, form input, query strings) into the value a schema most likely
// meant, before it is checked. Three passes, in this order:
//
//  1. Optional nulls: a null in an optional property whose schema rejects
//     null is dropped, as if omitted.
//  2. Conversion rules ported from TypeBox 1.3.27's Value.Convert, keyed on
//     JSON Schema structure (`$ref`, `const`, `enum`, `anyOf`/`oneOf`,
//     `allOf`, `type`, `items`/`prefixItems`, `properties`/
//     `patternProperties`): null becomes "null" for a string, "3.5" becomes 3
//     for an integer, a single value becomes a one-item array, and a union
//     takes the first branch whose conversion passes.
//  3. Primitive coercion by `type` (also type arrays), as in Ajv's
//     coerceTypes: what pass 2 left, for example a type array
//     ["integer", "null"], or "" for null.
//
// A value that cannot be coerced is left as it is; the check then reports it.
// Every pass is copy-on-write: an object or array is copied only when one of
// its members changes, so an unchanged value comes back as itself and the
// input is never modified. A copied object keeps its kind (Object or
// map[string]any) and its member order.

// coercer is coerce.ts Coercer for one compiled schema.
type coercer struct {
	c *compiler
}

// coerceRun is the state of one coerce() call.
type coerceRun struct {
	*coercer
	// entered is the (schema node, value) pairs pass 2 is converting through
	// a re-entry point, as a stack. Conversion can enter a node again with
	// the same value only through a `$ref` or by wrapping a single value into
	// an array: a recursive schema that meets a value no branch accepts would
	// convert it without end. A pair already on the stack leaves the value
	// unchanged.
	entered []enteredPair
	// cuts is how often a re-entry was cut short; results computed across
	// one are not memoized.
	cuts int
	// memo holds results of `$ref` targets by (node, value): a recursive
	// union whose branches all convert the same children (and all fail)
	// would convert them once per branch, exponentially in the depth.
	memo map[*schemaObject]map[any]any
}

type enteredPair struct {
	node  *schemaObject
	value any
}

// coerce is value coerced: a copy along the changed paths only, or value
// itself when nothing changes.
func (k *coercer) coerce(value any) any {
	run := &coerceRun{coercer: k}
	base := k.c.r.rootBase
	withoutNulls := run.dropOptionalNulls(value, k.c.schema, base)
	return run.coerceTypes(run.convertOnly(withoutNulls), k.c.schema, base)
}

// convertOnly is pass 2 alone, as TypeBox's Value.Convert.
func (r *coerceRun) convertOnly(value any) any {
	defer func() { r.memo, r.cuts = nil, 0 }()
	return r.convert(r.c.schema, r.c.r.rootBase, value)
}

// reenter is node converted for value at a re-entry point: unchanged when
// that pair is already being converted.
func (r *coerceRun) reenter(node *schemaObject, base string, value any, memoize bool) any {
	for _, p := range r.entered {
		if p.node == node && sameValue(p.value, value) {
			r.cuts++
			return value
		}
	}
	key, keyed := zeroKey(value)
	memoize = memoize && keyed
	if memoize {
		if result, ok := r.memo[node][key]; ok {
			return result
		}
	}
	cuts := r.cuts
	r.entered = append(r.entered, enteredPair{node, value})
	result := r.convert(node, base, value)
	r.entered = r.entered[:len(r.entered)-1]
	if memoize && r.cuts == cuts {
		if r.memo == nil {
			r.memo = map[*schemaObject]map[any]any{}
		}
		if r.memo[node] == nil {
			r.memo[node] = map[any]any{}
		}
		r.memo[node][key] = result
	}
	return result
}

// passes is whether value passes the schema node; ok is false when node
// cannot be compiled.
func (r *coerceRun) passes(node any, base string, value any) (pass, ok bool) {
	defer func() {
		if p := recover(); p != nil {
			if _, isCompile := p.(*compileError); !isCompile && p != errRestart {
				panic(p)
			}
			pass, ok = false, false
		}
	}()
	return r.c.check(node, r.baseOf(node, base))(value), true
}

// passesTrue is `passes(...) === true`.
func (r *coerceRun) passesTrue(node any, base string, value any) bool {
	pass, ok := r.passes(node, base, value)
	return ok && pass
}

func (r *coerceRun) baseOf(node any, base string) string {
	if s, ok := node.(*schemaObject); ok {
		if b, ok := r.c.r.baseOf[s]; ok {
			return b
		}
	}
	return base
}

// regex is sharedRegex where coercion meets a pattern; one that is not valid
// is upstream's throw out of coerce().
func regexOrPanic(source string) *pattern {
	re, err := sharedRegex(source)
	if err != nil {
		panic(err)
	}
	return re
}

// memberPatterns is a node's patternProperties, compiled.
func memberPatterns(node *schemaObject) []patternMember {
	var out []patternMember
	for _, m := range entries(node.kw("patternProperties")) {
		out = append(out, patternMember{regexOrPanic(m.Key), m.Value})
	}
	return out
}

type patternMember struct {
	re     *pattern
	schema any
}

// Pass 1: optional nulls.

func (r *coerceRun) dropOptionalNulls(value, schema any, base string) any {
	node, ok := schema.(*schemaObject)
	if !ok {
		return value
	}
	b := r.baseOf(node, base)
	if list, ok := value.([]any); ok {
		return mapItems(list, func(item any, i int) any {
			s, ok := itemSchemaAt(node, i)
			if !ok {
				return item
			}
			return r.dropOptionalNulls(item, s, b)
		})
	}
	if !isPlainObject(value) {
		return value
	}
	properties, hasProperties := node.kw("properties").(*schemaObject)
	if properties == nil {
		properties = &schemaObject{}
	}
	patterns := memberPatterns(node)
	additional, _ := node.kw("additionalProperties").(*schemaObject)
	if len(patterns) == 0 && additional == nil && !hasProperties {
		return value
	}
	required, _ := node.kw("required").([]any)
	isRequired := func(key string) bool {
		return slices.ContainsFunc(required, func(x any) bool { return sameValueZero(x, key) })
	}
	w := newObjectWriter(value)
	for _, m := range properties.members {
		x, ok := getOwn(value, m.Key)
		if !ok {
			continue
		}
		property, isNode := m.Value.(*schemaObject)
		_, isRef := property.kwIfNode("$ref").(string)
		isRef = isNode && isRef
		if x == nil && !isRequired(m.Key) && !isRef {
			if pass, ok := r.passes(m.Value, b, nil); ok && !pass {
				w.delete(m.Key)
				continue
			}
		}
		w.set(m.Key, r.dropOptionalNulls(x, m.Value, b))
	}
	// Values of pattern and additional properties are walked for their own
	// optional nulls; the keys themselves are not optional properties, so
	// they stay.
	if len(patterns) > 0 || additional != nil {
		for _, key := range ownKeys(value) {
			matched := properties.has(key)
			for _, p := range patterns {
				if !p.re.test(key) {
					continue
				}
				matched = true
				w.set(key, r.dropOptionalNulls(w.get(key), p.schema, b))
			}
			if !matched && additional != nil {
				w.set(key, r.dropOptionalNulls(propOf(value, key), additional, b))
			}
		}
	}
	return w.result()
}

// kwIfNode is kw on a node that may be nil.
func (s *schemaObject) kwIfNode(key string) any {
	if s == nil {
		return nil
	}
	return s.kw(key)
}

// Pass 2: TypeBox's conversion rules.

func (r *coerceRun) convert(v any, base string, value any) any {
	node, ok := v.(*schemaObject)
	if !ok {
		return value
	}
	b := r.baseOf(node, base)
	if ref, ok := node.kw("$ref").(string); ok {
		target, err := r.c.r.resolve(ref, b)
		if err != nil {
			return value
		}
		if t, ok := target.node.(*schemaObject); ok {
			return r.reenter(t, target.base, value, true)
		}
		return value
	}
	return r.convertNode(node, b, value)
}

// convertNode is one schema node's conversion: its first keyword among
// const, enum, the unions, allOf and type.
func (r *coerceRun) convertNode(node *schemaObject, b string, value any) any {
	if literal, ok := node.get("const"); ok {
		return convertLiteral(literal, value)
	}
	if values, ok := node.kw("enum").([]any); ok {
		return r.convertEnum(node, values, b, value)
	}
	// A union or intersection node may also declare members (plain JSON
	// Schema; TypeBox's never do): they convert after the branches.
	if list, ok := node.kw("anyOf").([]any); ok {
		return r.convertUnion(node, list, b, value)
	}
	if list, ok := node.kw("oneOf").([]any); ok {
		return r.convertUnion(node, list, b, value)
	}
	if list, ok := node.kw("allOf").([]any); ok {
		next := value
		for _, member := range list {
			next = r.convert(member, b, next)
		}
		return r.convertMembers(node, b, next)
	}
	typ, hasType := node.get("type")
	switch {
	case !hasType:
		if node.has("properties") || node.has("patternProperties") {
			return r.convertObject(node, b, value)
		}
		if node.has("items") || node.has("prefixItems") {
			return r.convertArray(node, b, value)
		}
		return value
	case typ == "string":
		if s, ok := tryString(value); ok {
			return s
		}
	case typ == "number":
		if isFiniteNumber(value) {
			return value
		}
		if f, ok := tryNumber(value); ok {
			return f
		}
	case typ == "integer":
		if f, ok := tryNumber(value); ok {
			return math.Trunc(f)
		}
	case typ == "boolean":
		if t, ok := tryBoolean(value); ok {
			return t
		}
	case typ == "null":
		if tryNull(value) {
			return nil
		}
	case typ == "array":
		return r.convertArray(node, b, value)
	case typ == "object":
		return r.convertObject(node, b, value)
	}
	return value
}

// convertEnum converts for an enum as for a union of its values: a member
// stays, else the first value the input converts to.
func (r *coerceRun) convertEnum(node *schemaObject, values []any, base string, value any) any {
	for _, member := range values {
		if strictEquals(member, value) {
			return value
		}
	}
	for _, member := range values {
		candidate := convertLiteral(member, value)
		if !strictEquals(candidate, value) && r.passesTrue(node, base, candidate) {
			return candidate
		}
	}
	return value
}

// convertUnion keeps a value one branch accepts; else it takes the first
// branch's conversion that the whole union accepts. Branches are converted
// in order and only until one is accepted, so a later recursive branch is not
// entered needlessly. A node that also declares members converts them into
// each candidate before it is checked.
func (r *coerceRun) convertUnion(node *schemaObject, branches []any, base string, value any) any {
	for _, branch := range branches {
		if r.passesTrue(branch, base, value) {
			return r.convertMembers(node, base, value)
		}
	}
	for _, branch := range branches {
		candidate := r.convertMembers(node, base, r.convert(branch, base, value))
		if r.passesTrue(node, base, candidate) {
			return candidate
		}
	}
	return r.convertMembers(node, base, value)
}

// convertMembers converts the object members of a union or intersection node,
// when it declares any.
func (r *coerceRun) convertMembers(node *schemaObject, base string, value any) any {
	if node.has("properties") || node.has("patternProperties") || node.has("additionalProperties") {
		return r.convertObject(node, base, value)
	}
	return value
}

func (r *coerceRun) convertArray(node *schemaObject, base string, value any) any {
	items, itemsIsList := node.kw("items").([]any)
	prefix := items
	if !itemsIsList {
		prefix, _ = node.kw("prefixItems").([]any)
	}
	if itemsIsList || prefix != nil {
		// A tuple converts its items and never wraps a single value.
		list, ok := value.([]any)
		if !ok {
			return value
		}
		rest, hasRest := node.get("items")
		if itemsIsList {
			hasRest = false
		}
		return mapItems(list, func(item any, i int) any {
			switch {
			case i < len(prefix):
				return r.convert(prefix[i], base, item)
			case hasRest:
				return r.convert(rest, base, item)
			}
			return item
		})
	}
	itemSchema, hasItems := node.get("items")
	if list, ok := value.([]any); ok {
		if !hasItems {
			return value
		}
		return mapItems(list, func(item any, _ int) any { return r.convert(itemSchema, base, item) })
	}
	// The wrapped single value is converted by the item schema with the same
	// value: a re-entry point.
	if s, ok := itemSchema.(*schemaObject); ok {
		return []any{r.reenter(s, base, value, false)}
	}
	return []any{value}
}

func (r *coerceRun) convertObject(node *schemaObject, base string, value any) any {
	if !isPlainObject(value) {
		return value
	}
	properties, _ := node.kw("properties").(*schemaObject)
	patterns := memberPatterns(node)
	additional, _ := node.kw("additionalProperties").(*schemaObject)
	w := newObjectWriter(value)
	for _, key := range ownKeys(value) {
		old, _ := getOwn(value, key)
		if old == undefined {
			continue // an undefined property counts as absent
		}
		next := old
		matched := properties != nil && properties.has(key)
		if matched {
			next = r.convert(properties.kw(key), base, next)
		}
		for _, p := range patterns {
			if !p.re.test(key) {
				continue
			}
			matched = true
			next = r.convert(p.schema, base, next)
		}
		if !matched && additional != nil {
			next = r.convert(additional, base, next)
		}
		w.set(key, next)
	}
	return w.result()
}

// Pass 3: primitive coercion by type.

func (r *coerceRun) coerceTypes(value, schema any, base string) any {
	node, ok := schema.(*schemaObject)
	if !ok {
		return value
	}
	b := r.baseOf(node, base)
	next := value
	if list, ok := node.kw("allOf").([]any); ok {
		for _, member := range list {
			next = r.coerceTypes(next, member, b)
		}
	}
	if list, ok := node.kw("anyOf").([]any); ok {
		next = r.coerceUnion(next, list, b)
	}
	if list, ok := node.kw("oneOf").([]any); ok {
		next = r.coerceUnion(next, list, b)
	}
	types := typeNames(node)
	matchesUnionMember := len(types) > 1 && slices.ContainsFunc(types, func(t string) bool { return matchesType(next, t) })
	if len(types) > 0 && !matchesUnionMember {
		for _, t := range types {
			if candidate := coercePrimitive(next, t); !strictEquals(candidate, next) {
				next = candidate
				break
			}
		}
	}
	if slices.Contains(types, "object") && isPlainObject(next) {
		next = r.coerceProperties(next, node, b)
	}
	if list, ok := next.([]any); ok && slices.Contains(types, "array") {
		next = r.coerceItems(list, node, b)
	}
	return next
}

func (r *coerceRun) coerceUnion(value any, branches []any, base string) any {
	for _, branch := range branches {
		if r.passesTrue(branch, base, value) {
			return value
		}
	}
	for _, branch := range branches {
		coerced := r.coerceTypes(value, branch, base)
		if r.passesTrue(branch, base, coerced) {
			return coerced
		}
	}
	return value
}

func (r *coerceRun) coerceProperties(value any, node *schemaObject, base string) any {
	properties, _ := node.kw("properties").(*schemaObject)
	if properties == nil {
		properties = &schemaObject{}
	}
	patterns := memberPatterns(node)
	additional, _ := node.kw("additionalProperties").(*schemaObject)
	w := newObjectWriter(value)
	for _, m := range properties.members {
		if x, ok := getOwn(value, m.Key); ok {
			w.set(m.Key, r.coerceTypes(x, m.Value, base))
		}
	}
	for _, key := range ownKeys(value) {
		matched := properties.has(key)
		for _, p := range patterns {
			if !p.re.test(key) {
				continue
			}
			matched = true
			w.set(key, r.coerceTypes(w.get(key), p.schema, base))
		}
		if !matched && additional != nil {
			w.set(key, r.coerceTypes(propOf(value, key), additional, base))
		}
	}
	return w.result()
}

func (r *coerceRun) coerceItems(value []any, node *schemaObject, base string) any {
	return mapItems(value, func(item any, i int) any {
		s, ok := itemSchemaAt(node, i)
		if _, isNode := s.(*schemaObject); !ok || !isNode {
			return item
		}
		return r.coerceTypes(item, s, base)
	})
}

// itemSchemaAt is the schema of an array's item at index: a tuple's
// (prefixItems, or a draft-7 items array) for its prefix, items after
// prefixItems, or items for every item; ok is false when nothing applies.
func itemSchemaAt(node *schemaObject, index int) (any, bool) {
	if items, ok := node.kw("items").([]any); ok {
		if index < len(items) {
			return items[index], true
		}
		return nil, false
	}
	if prefix, ok := node.kw("prefixItems").([]any); ok && index < len(prefix) {
		return prefix[index], true
	}
	return node.get("items")
}

// mapItems is items mapped by f; the same slice when every item maps to
// itself.
func mapItems(items []any, f func(item any, i int) any) []any {
	var out []any
	for i, item := range items {
		next := f(item, i)
		if strictEquals(next, item) {
			continue
		}
		if out == nil {
			out = slices.Clone(items)
		}
		out[i] = next
	}
	if out == nil {
		return items
	}
	return out
}

// objectWriter is the copy-on-write of one object: `out ??= { ...value }`,
// keeping the object's kind and member order.
type objectWriter struct {
	value any
	out   any
}

func newObjectWriter(value any) *objectWriter { return &objectWriter{value: value} }

func (w *objectWriter) current() any {
	if w.out != nil {
		return w.out
	}
	return w.value
}

// get is `(out ?? value)[key]`.
func (w *objectWriter) get(key string) any { return propOf(w.current(), key) }

func (w *objectWriter) copy() {
	if w.out != nil {
		return
	}
	switch o := w.value.(type) {
	case Object:
		w.out = slices.Clone(o)
	case map[string]any:
		w.out = maps.Clone(o)
	}
}

// set is the copy-on-write `out[key] = next`, a no-op when next is the value
// the key has.
func (w *objectWriter) set(key string, next any) {
	if strictEquals(next, w.get(key)) {
		return
	}
	w.copy()
	switch o := w.out.(type) {
	case Object:
		if i := slices.IndexFunc(o, func(m Member) bool { return m.Key == key }); i >= 0 {
			o[i].Value = next
		} else {
			w.out = append(o, Member{key, next})
		}
	case map[string]any:
		o[key] = next
	}
}

// delete is the copy-on-write `delete out[key]`.
func (w *objectWriter) delete(key string) {
	w.copy()
	switch o := w.out.(type) {
	case Object:
		w.out = slices.DeleteFunc(o, func(m Member) bool { return m.Key == key })
	case map[string]any:
		delete(o, key)
	}
}

func (w *objectWriter) result() any { return w.current() }

// isPlainObject is coerce.ts isPlainObject. Upstream's also takes a
// Uint8Array, whose index keys it would convert one by one; the port leaves
// bytes alone.
func isPlainObject(v any) bool {
	switch v.(type) {
	case Object, map[string]any:
		return true
	}
	return false
}

// TypeBox's conversions of one value (value/convert/try in TypeBox 1.3.27):
// a result, or ok false when there is none.

// convertLiteral is a literal (const, an enum value) taken from a value of
// another type that converts to exactly it.
func convertLiteral(literal, value any) any {
	if strictEquals(literal, value) {
		return value
	}
	var result any
	var ok bool
	switch {
	case isString(literal):
		result, ok = tryString(value)
	case isNumber(literal):
		if isFiniteNumber(value) {
			result, ok = value, true
		} else {
			result, ok = tryNumber(value)
		}
	case isBoolean(literal):
		result, ok = tryBoolean(value)
	}
	if ok && strictEquals(result, literal) {
		return result
	}
	return value
}

func tryString(value any) (string, bool) {
	switch x := value.(type) {
	case bool:
		return jsString(x), true
	case nil:
		return "null", true
	case string:
		return x, true
	case undefinedValue:
		return "", true
	}
	if f, ok := numberOf(value); ok && isFinite(f) {
		return jstext.NumberToString(f), true
	}
	return "", false
}

var (
	maxSafe = big.NewInt(1<<53 - 1)
	minSafe = big.NewInt(-(1<<53 - 1))
)

func tryNumber(value any) (float64, bool) {
	switch x := value.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil, undefinedValue:
		return 0, true
	case string:
		if coerced := jstext.StringToNumber(x); isFinite(coerced) {
			return coerced, true
		}
		switch jstext.ToLower(x) {
		case "false":
			return 0, true
		case "true":
			return 1, true
		}
		if n, ok := bigIntOfString(x); ok && n.Cmp(maxSafe) <= 0 && n.Cmp(minSafe) >= 0 {
			return float64(n.Int64()), true
		}
		return 0, false
	}
	if f, ok := numberOf(value); ok && isFinite(f) {
		return f, true
	}
	return 0, false
}

var (
	bigIntSuffixed = regexp.MustCompile(`^-?(0|[1-9][0-9]*)n$`)
	bigIntDecimal  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)\.[0-9]+$`)
	bigIntPlain    = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
)

// bigIntOfString is TypeBox's TryBigInt of a string: `12n`, `12.5`
// (truncated) or `12`.
func bigIntOfString(value string) (*big.Int, bool) {
	var digits string
	switch {
	case bigIntSuffixed.MatchString(value):
		digits = value[:len(value)-1]
	case bigIntDecimal.MatchString(value):
		digits, _, _ = strings.Cut(value, ".")
	case bigIntPlain.MatchString(value):
		digits = value
	default:
		return nil, false
	}
	return new(big.Int).SetString(digits, 10)
}

func tryBoolean(value any) (bool, bool) {
	switch x := value.(type) {
	case bool:
		return x, true
	case nil, undefinedValue:
		return false, true
	case string:
		lower := jstext.ToLower(x)
		switch {
		case lower == "false" || x == "0":
			return false, true
		case lower == "true" || x == "1":
			return true, true
		}
		return false, false
	}
	if f, ok := numberOf(value); ok && isFinite(f) {
		switch f {
		case 0:
			return false, true
		case 1:
			return true, true
		}
	}
	return false, false
}

// tryNull reports whether value converts to null.
func tryNull(value any) bool {
	switch x := value.(type) {
	case bool:
		return !x
	case nil, undefinedValue:
		return true
	case string:
		lower := jstext.ToLower(x)
		return lower == "undefined" || lower == "null" || x == "" || x == "0"
	}
	if f, ok := numberOf(value); ok && isFinite(f) {
		return f == 0
	}
	return false
}

func isFiniteNumber(v any) bool {
	f, ok := numberOf(v)
	return ok && isFinite(f)
}

// typeNames is the string members of `type`.
func typeNames(node *schemaObject) []string {
	switch t := node.kw("type").(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func matchesType(value any, typ string) bool {
	switch typ {
	case "number":
		return isNumber(value)
	case "integer":
		return isInteger(value)
	case "boolean":
		return isBoolean(value)
	case "string":
		return isString(value)
	case "null":
		return value == nil
	case "array":
		return isArray(value)
	case "object":
		return isPlainObject(value)
	}
	return false
}

func coercePrimitive(value any, typ string) any {
	switch typ {
	case "number", "integer":
		if value == nil {
			return 0.0
		}
		if s, ok := value.(string); ok && jstext.Trim(s) != "" {
			parsed := jstext.StringToNumber(s)
			if (typ == "number" && isFinite(parsed)) || (typ == "integer" && isIntegral(parsed)) {
				return parsed
			}
		}
		if b, ok := value.(bool); ok {
			if b {
				return 1.0
			}
			return 0.0
		}
	case "boolean":
		if value == nil {
			return false
		}
		if value == "true" || strictEquals(value, 1.0) {
			return true
		}
		if value == "false" || strictEquals(value, 0.0) {
			return false
		}
	case "string":
		if value == nil {
			return ""
		}
		if isNumber(value) || isBoolean(value) {
			return jsString(value)
		}
	case "null":
		if value == "" || strictEquals(value, 0.0) || value == false {
			return nil
		}
	}
	return value
}
