package schema

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// explainer is explain.ts Explainer: the errors of a failed value, from a
// walk over the schema that reports each failing keyword at the value it
// failed on, using the compiler's checks for subschemas.
type explainer struct {
	c *compiler
	// rests holds each node less its unevaluated* keywords. Upstream builds
	// that rest object on every walk, and its compiler caches the annotating
	// check of each one for good; one per node is the same check without the
	// growth.
	rests map[*schemaObject]*schemaObject
}

func newExplainer(c *compiler) *explainer {
	return &explainer{c: c, rests: map[*schemaObject]*schemaObject{}}
}

// explainRun is the state of one errors() call: the errors of `$ref` targets
// for object values. A recursive union explains each branch to pick the one
// to blame, and every branch would explain the same children again,
// exponentially in the depth.
//
// walking is the walks running, by node, base and path (the path names the
// value). A walk that starts again on its own node and path would never end
// (upstream's errors() overflows V8's stack there); it fails with
// ErrStackOverflow. A check can stop before a cycle the walk meets: the walk
// explains every keyword where the check stops at the first failure.
type explainRun struct {
	*explainer
	memo    map[memoKey]memoEntry
	walking map[walkKey]bool
}

type walkKey struct {
	node       *schemaObject
	base, path string
}

type memoKey struct {
	node  *schemaObject
	value identity
}

type memoEntry struct {
	path   string
	errors []ValidationError
}

// errors is the errors of value, at least one (value failed the check).
func (e *explainer) errors(value any) []ValidationError {
	run := &explainRun{explainer: e}
	var out []ValidationError
	run.walk(e.c.schema, e.c.r.rootBase, value, "", &out)
	if len(out) == 0 {
		out = append(out, ValidationError{Path: "", Keyword: "schema", Message: "invalid"})
	}
	return out
}

// ok is whether v passes node: the compiler's check, compiled on first use
// where the check never needed it. A compile failure panics (a
// *compileError), upstream's throw out of errors(), as do the explainer's
// other throws (fail).
func (e *explainer) ok(node any, base string, v any) bool {
	return e.c.check(node, base)(v)
}

func (e *explainer) resolve(ref, base string) resolved {
	target, err := e.c.r.resolve(ref, base)
	if err != nil {
		fail(err)
	}
	return target
}

// walk explains node for v at path. For $dynamicRef schemas the checks run
// outside the original dynamic scope, so their errors are approximate.
func (e *explainRun) walk(v any, base string, value any, path string, out *[]ValidationError) {
	switch v {
	case true:
		return
	case false:
		*out = append(*out, ValidationError{path, "false", "no value is allowed here"})
		return
	}
	node, ok := v.(*schemaObject)
	if !ok {
		return
	}
	b := e.c.r.baseFor(node, base)
	if e.ok(node, b, value) {
		return
	}
	key := walkKey{node, b, path}
	if e.walking[key] {
		fail(ErrStackOverflow)
	}
	if e.walking == nil {
		e.walking = map[walkKey]bool{}
	}
	e.walking[key] = true
	defer delete(e.walking, key)
	start := len(*out)
	push := func(keyword, message string) {
		*out = append(*out, ValidationError{path, keyword, message})
	}
	if ref, ok := node.kw("$ref").(string); ok && e.c.draft == Draft7 {
		e.walkRef(ref, b, value, path, out)
		return
	}
	if node.has("type") {
		types := typesOf(node)
		if !typeCheck(types)(value) {
			push("type", "expected "+joinValues(types, " or ")+", got "+typeName(value))
			return
		}
	}
	if values, ok := node.kw("enum").([]any); ok && !enumCheck(values)(value) {
		push("enum", "expected one of "+listValues(values))
	}
	if c, ok := node.get("const"); ok && !constCheck(c)(value) {
		push("const", "expected "+stringify(c))
	}
	if s, ok := value.(string); ok && node.kw("type") != "bytes" {
		e.walkString(node, s, push)
	}
	if bs, ok := value.([]byte); ok && node.kw("type") == "bytes" {
		e.walkBytes(node, bs, push)
	}
	if f, ok := numberOf(value); ok {
		walkNumber(node, f, push)
	}
	if isObject(value) {
		e.walkObject(node, b, value, path, out, push)
	}
	if list, ok := value.([]any); ok {
		e.walkArray(node, b, list, path, out, push)
	}
	if list, ok := node.kw("allOf").([]any); ok {
		for _, s := range list {
			e.walk(s, b, value, path, out)
		}
	}
	if list, ok := node.kw("anyOf").([]any); ok {
		e.walkUnion(list, b, value, path, out, false)
	}
	if list, ok := node.kw("oneOf").([]any); ok {
		e.walkUnion(list, b, value, path, out, true)
	}
	if not, ok := node.get("not"); ok && e.ok(not, b, value) {
		push("not", "must not match the schema in not")
	}
	if cond, ok := node.get("if"); ok {
		key := "else"
		if e.ok(cond, b, value) {
			key = "then"
		}
		if branch, ok := node.get(key); ok {
			e.walk(branch, b, value, path, out)
		}
	}
	if ref, ok := node.kw("$ref").(string); ok {
		e.walkRef(ref, b, value, path, out)
	}
	if len(*out) == start && (node.has("unevaluatedProperties") || node.has("unevaluatedItems")) {
		e.walkUnevaluated(node, b, value, path, out)
	}
	if _, ok := node.kw("$dynamicRef").(string); ok && len(*out) == start {
		push("$dynamicRef", "does not match the dynamically referenced schema")
	}
}

func (e *explainRun) walkRef(ref, base string, value any, path string, out *[]ValidationError) {
	target := e.resolve(ref, base)
	if target.node == false && e.c.r.isUnresolved(ref, base) {
		*out = append(*out, ValidationError{path, "$ref", "unresolved reference " + ref})
		return
	}
	node, isNode := target.node.(*schemaObject)
	id, hasID := identityOf(value)
	if !isNode || !hasID || (id.n == 0 && id.kind != 'm' && id.kind != 's') {
		// A primitive (or an empty array or object, which has no identity of
		// its own here) is explained without the memo.
		e.walk(target.node, target.base, value, path, out)
		return
	}
	key := memoKey{node, id}
	if known, ok := e.memo[key]; ok && known.path == path {
		*out = append(*out, known.errors...)
		return
	}
	var errs []ValidationError
	e.walk(target.node, target.base, value, path, &errs)
	if e.memo == nil {
		e.memo = map[memoKey]memoEntry{}
	}
	e.memo[key] = memoEntry{path, errs}
	*out = append(*out, errs...)
}

func (e *explainRun) walkString(node *schemaObject, v string, push func(string, string)) {
	n := float64(codePoints(v))
	if lo, ok := numberKw(node, "minLength"); ok && n < lo {
		push("minLength", "expected at least "+jstext.NumberToString(lo)+" characters")
	}
	if hi, ok := numberKw(node, "maxLength"); ok && n > hi {
		push("maxLength", "expected at most "+jstext.NumberToString(hi)+" characters")
	}
	if source, ok := node.kw("pattern").(string); ok {
		if re, err := sharedRegex(source); err != nil {
			fail(err)
		} else if !re.test(v) {
			push("pattern", "expected to match "+source)
		}
	}
	if name, ok := node.kw("format").(string); ok && e.c.formats != nil {
		if format := e.c.formats[name]; format != nil && !format(v) {
			push("format", "expected format "+name)
		}
	}
}

func (e *explainRun) walkBytes(node *schemaObject, v []byte, push func(string, string)) {
	n := float64(len(v))
	if lo, ok := numberKw(node, "minLength"); ok && n < lo {
		push("minLength", "expected at least "+jstext.NumberToString(lo)+" bytes")
	}
	if hi, ok := numberKw(node, "maxLength"); ok && n > hi {
		push("maxLength", "expected at most "+jstext.NumberToString(hi)+" bytes")
	}
}

func walkNumber(node *schemaObject, v float64, push func(string, string)) {
	if x, ok := numberKw(node, "minimum"); ok && v < x {
		push("minimum", "expected >= "+jstext.NumberToString(x))
	}
	if x, ok := numberKw(node, "maximum"); ok && v > x {
		push("maximum", "expected <= "+jstext.NumberToString(x))
	}
	if x, ok := numberKw(node, "exclusiveMinimum"); ok && v <= x {
		push("exclusiveMinimum", "expected > "+jstext.NumberToString(x))
	}
	if x, ok := numberKw(node, "exclusiveMaximum"); ok && v >= x {
		push("exclusiveMaximum", "expected < "+jstext.NumberToString(x))
	}
	if x, ok := numberKw(node, "multipleOf"); ok && !isMultiple(v, x) {
		push("multipleOf", "expected a multiple of "+jstext.NumberToString(x))
	}
}

func (e *explainRun) walkObject(node *schemaObject, b string, v any, path string, out *[]ValidationError, push func(string, string)) {
	pushAt := func(at, keyword, message string) {
		*out = append(*out, ValidationError{at, keyword, message})
	}
	properties, _ := node.kw("properties").(*schemaObject)
	if properties == nil {
		properties = &schemaObject{}
	}
	if required, ok := node.kw("required").([]any); ok {
		for _, k := range required {
			if s, ok := k.(string); ok && !hasOwn(v, s) {
				pushAt(pointer(path, s), "required", "missing")
			}
		}
	}
	for _, m := range properties.members {
		if x, ok := getOwn(v, m.Key); ok {
			e.walk(m.Value, b, x, pointer(path, m.Key), out)
		}
	}
	var patterns []Member
	if p, ok := node.kw("patternProperties").(*schemaObject); ok {
		patterns = p.members
	}
	additional, hasAdditional := node.get("additionalProperties")
	propertyNames, hasPropertyNames := node.get("propertyNames")
	keys := ownKeys(v)
	for _, k := range keys {
		x, _ := getOwn(v, k)
		matched := properties.has(k)
		for _, p := range patterns {
			re, err := sharedRegex(p.Key)
			if err != nil {
				fail(err)
			}
			if re.test(k) {
				matched = true
				e.walk(p.Value, b, x, pointer(path, k), out)
			}
		}
		if !matched && hasAdditional {
			if additional == false {
				pushAt(pointer(path, k), "additionalProperties", "unknown field")
			} else {
				e.walk(additional, b, x, pointer(path, k), out)
			}
		}
		if hasPropertyNames && !e.ok(propertyNames, b, k) {
			pushAt(pointer(path, k), "propertyNames", "invalid property name")
		}
	}
	count := float64(len(keys))
	if lo, ok := numberKw(node, "minProperties"); ok && count < lo {
		push("minProperties", "expected at least "+jstext.NumberToString(lo)+" properties")
	}
	if hi, ok := numberKw(node, "maxProperties"); ok && count > hi {
		push("maxProperties", "expected at most "+jstext.NumberToString(hi)+" properties")
	}
	// `{...dependentRequired, ...dependencies}`: a key of both keeps its first
	// place and takes the dependencies value.
	var dependencies []Member
	merge := func(v any) {
		for _, m := range entries(v) {
			if i := slices.IndexFunc(dependencies, func(d Member) bool { return d.Key == m.Key }); i >= 0 {
				dependencies[i].Value = m.Value
			} else {
				dependencies = append(dependencies, m)
			}
		}
	}
	if e.c.draft != Draft7 {
		merge(node.kw("dependentRequired"))
	}
	merge(node.kw("dependencies"))
	for _, d := range dependencies {
		if !hasOwn(v, d.Key) {
			continue
		}
		list, ok := d.Value.([]any)
		if !ok {
			e.walk(d.Value, b, v, path, out)
			continue
		}
		for _, r := range list {
			if name := jsString(r); !hasOwn(v, name) {
				pushAt(pointer(path, name), "dependentRequired", "required when "+d.Key+" is present")
			}
		}
	}
	if e.c.draft != Draft7 {
		for _, m := range entries(node.kw("dependentSchemas")) {
			if hasOwn(v, m.Key) {
				e.walk(m.Value, b, v, path, out)
			}
		}
	}
}

func (e *explainRun) walkArray(node *schemaObject, b string, v []any, path string, out *[]ValidationError, push func(string, string)) {
	var prefix []any
	var rest any
	hasRest := false
	if items, ok := node.kw("items").([]any); ok {
		prefix = items
		rest, hasRest = node.get("additionalItems")
	} else {
		if list, ok := node.kw("prefixItems").([]any); ok && e.c.draft != Draft7 {
			prefix = list
		}
		rest, hasRest = node.get("items")
	}
	for i, x := range v {
		s, ok := rest, hasRest
		if i < len(prefix) {
			s, ok = prefix[i], true
		}
		if ok {
			e.walk(s, b, x, path+"/"+strconv.Itoa(i), out)
		}
	}
	n := float64(len(v))
	if lo, ok := numberKw(node, "minItems"); ok && n < lo {
		push("minItems", "expected at least "+jstext.NumberToString(lo)+" items")
	}
	if hi, ok := numberKw(node, "maxItems"); ok && n > hi {
		push("maxItems", "expected at most "+jstext.NumberToString(hi)+" items")
	}
	if node.kw("uniqueItems") == true && !uniqueCheck(v) {
		push("uniqueItems", "expected unique items")
	}
	if contains, ok := node.get("contains"); ok {
		lo, hi := 1.0, math.Inf(1)
		if e.c.draft != Draft7 {
			lo = numberOr(node.kw("minContains"), 1)
			hi = numberOr(node.kw("maxContains"), math.Inf(1))
		}
		if !containsCheck(e.c.check(contains, b), lo, hi)(v) {
			push("contains", containsMessage(lo, hi))
		}
	}
}

func (e *explainRun) walkUnion(branches []any, b string, v any, path string, out *[]ValidationError, exactlyOne bool) {
	matches := 0
	for _, s := range branches {
		if e.ok(s, b, v) {
			matches++
		}
	}
	if matches == 1 || (!exactlyOne && matches > 0) {
		return
	}
	keyword := "anyOf"
	if exactlyOne {
		keyword = "oneOf"
	}
	if matches > 1 {
		*out = append(*out, ValidationError{path, keyword, "matches more than one alternative"})
		return
	}
	// A tagged union reports the branch its tag selects; otherwise the branch
	// whose first error is deepest.
	if key, values, ok := e.tagOf(branches, b); ok && isObject(v) {
		tag := propOf(v, key)
		i := slices.IndexFunc(values, func(x any) bool { return strictEquals(x, tag) })
		if i < 0 {
			*out = append(*out, ValidationError{pointer(path, key), keyword, "expected one of " + listValues(values)})
		} else {
			e.walk(branches[i], b, v, path, out)
		}
		return
	}
	// An object is blamed on the branch that declares most of its
	// properties (the one it was most likely meant for), then on the branch
	// whose first error is deepest.
	var best []ValidationError
	bestDeclared := 0
	for _, s := range branches {
		var errs []ValidationError
		e.walk(s, b, v, path, &errs)
		if len(errs) == 0 {
			continue
		}
		declared := 0
		if isObject(v) {
			declared = e.declaredKeys(s, b, v)
		}
		if best == nil || declared > bestDeclared ||
			(declared == bestDeclared && jstext.CodeUnitLength(errs[0].Path) > jstext.CodeUnitLength(best[0].Path)) {
			best, bestDeclared = errs, declared
		}
	}
	if best == nil {
		best = []ValidationError{{path, keyword, "matches no alternative"}}
	}
	*out = append(*out, best...)
}

// throughRef is a branch, or the schema its `$ref` names.
func (e *explainRun) throughRef(s any, b string) any {
	if node, ok := s.(*schemaObject); ok {
		if ref, ok := node.kw("$ref").(string); ok {
			return e.resolve(ref, b).node
		}
	}
	return s
}

// declaredKeys is how many of the keys of v the branch s (through a `$ref`)
// declares in `properties`.
func (e *explainRun) declaredKeys(s any, b string, v any) int {
	node, _ := e.throughRef(s, b).(*schemaObject)
	var properties *schemaObject
	if node != nil {
		properties, _ = node.kw("properties").(*schemaObject)
	}
	n := 0
	for _, k := range ownKeys(v) {
		if properties != nil && properties.has(k) {
			n++
		}
	}
	return n
}

// tagOf is explain.ts tagOf: the key whose `const` in every branch's
// properties is a distinct primitive.
func (e *explainRun) tagOf(branches []any, b string) (key string, values []any, ok bool) {
	resolvedBranches := make([]any, len(branches))
	for i, s := range branches {
		resolvedBranches[i] = e.throughRef(s, b)
	}
	if len(resolvedBranches) == 0 {
		return "", nil, false
	}
	first, _ := resolvedBranches[0].(*schemaObject)
	if first == nil {
		return "", nil, false
	}
	properties, _ := first.kw("properties").(*schemaObject)
	if properties == nil {
		return "", nil, false
	}
	for _, m := range properties.members {
		values := make([]any, len(resolvedBranches))
		distinct := map[any]bool{}
		usable := true
		for i, s := range resolvedBranches {
			value := tagValue(s, m.Key)
			k, keyed := zeroKey(value)
			if value == undefined || !isPrimitive(value) || !keyed {
				usable = false
				break
			}
			distinct[k] = true
			values[i] = value
		}
		if usable && len(distinct) == len(values) {
			return m.Key, values, true
		}
	}
	return "", nil, false
}

// tagValue is `s.properties[key].const`, undefined where any step is missing.
func tagValue(s any, key string) any {
	node, ok := s.(*schemaObject)
	if !ok {
		return undefined
	}
	properties, ok := node.kw("properties").(*schemaObject)
	if !ok {
		return undefined
	}
	tag, ok := properties.kw(key).(*schemaObject)
	if !ok {
		return undefined
	}
	return propOf(tag, "const")
}

func (e *explainRun) walkUnevaluated(node *schemaObject, b string, v any, path string, out *[]ValidationError) {
	rest, ok := e.rests[node]
	if !ok {
		rest = node.without(func(k string) bool { return k == "unevaluatedProperties" || k == "unevaluatedItems" })
		e.rests[node] = rest
	}
	a := &annotations{}
	e.c.annotating(rest, b)(v, a)
	if unevaluated, ok := node.get("unevaluatedProperties"); ok && isObject(v) && !a.allProps {
		for _, k := range ownKeys(v) {
			if a.props[k] {
				continue
			}
			if unevaluated == false {
				*out = append(*out, ValidationError{pointer(path, k), "unevaluatedProperties", "unknown field"})
			} else {
				x, _ := getOwn(v, k)
				e.walk(unevaluated, b, x, pointer(path, k), out)
			}
		}
	}
	if unevaluated, ok := node.get("unevaluatedItems"); ok && !a.allItems {
		if list, isList := v.([]any); isList {
			for i := a.items; i < len(list); i++ {
				if a.indexes[i] {
					continue
				}
				at := path + "/" + strconv.Itoa(i)
				if unevaluated == false {
					*out = append(*out, ValidationError{at, "unevaluatedItems", "unexpected item"})
				} else {
					e.walk(unevaluated, b, list[i], at, out)
				}
			}
		}
	}
}

// listedValues is how many values an enum error names; a long list (an enum
// of thousands) is cut, so the message stays readable.
const listedValues = 20

// listValues is values as JSON, at most listedValues of them, then how many
// more there are.
func listValues(values []any) string {
	if len(values) <= listedValues {
		return stringify(values)
	}
	listed := stringify(values[:listedValues])
	return listed[:len(listed)-1] + ", ... and " + strconv.Itoa(len(values)-listedValues) + " more]"
}

func containsMessage(lo, hi float64) string {
	item := func(n float64) string {
		if n == 1 {
			return "item"
		}
		return "items"
	}
	switch {
	case math.IsInf(hi, 1):
		return "expected at least " + jstext.NumberToString(lo) + " matching " + item(lo)
	case lo == 0:
		return "expected at most " + jstext.NumberToString(hi) + " matching " + item(hi)
	}
	return "expected between " + jstext.NumberToString(lo) + " and " + jstext.NumberToString(hi) + " matching items"
}

// pointer is path extended by key as a JSON Pointer token.
func pointer(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// typeName is the type a `type` error names: "integer" for an integral
// number, else typeof.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case []byte:
		return "bytes"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	if f, ok := numberOf(v); ok {
		if isIntegral(f) {
			return "integer"
		}
		return "number"
	}
	return "object"
}
