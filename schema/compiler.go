package schema

import (
	"errors"
	"math"
	"slices"
	"strings"
)

// compiler is compiler.ts Compiler: one compile of a schema into check
// closures. Keywords group by the type they constrain, so a value is
// classified once per schema, and the common shapes (closed objects, arrays
// of one item schema, tagged unions, leaf schemas) get one closure each.
//
// It fails by panicking with a *compileError (or errRestart), which the
// package's entry points recover: the compile is a deep recursion whose every
// level would otherwise return an error it can only pass up, and upstream
// throws. A check that would recurse forever fails the same way at run time
// (recursion.go). No panic leaves the package but the programmer error
// Validator.Coerce documents.
type compiler struct {
	r       *resolver
	draft   string
	formats map[string]FormatCheck
	// dynamic is whether checks track the dynamic scope (the schema has a
	// bookended `$dynamicRef`).
	dynamic         bool
	schema          any
	scope           *[]string
	cache           map[*schemaObject]check
	annotatingCache map[*schemaObject]annotatingCheck

	// The in-place graph and its guards (recursion.go): the compile stack,
	// the edges and guards of the vertices built since the last search for
	// cycles, whether a cycle was found, and the guarded checks running.
	building []frame
	edges    map[vertex][]vertex
	fresh    []vertex
	guards   map[vertex]func()
	cyclic   bool
	active   []activation
}

// compileError is an error upstream throws: compile()'s, or one thrown out of
// errors() or coerce() by a subschema compiled on first use, or the
// RangeError of a check that would recurse forever (ErrStackOverflow).
type compileError struct{ err error }

// errRestart is compiler.ts RESTART: a compile without dynamic scope tracking
// met a bookended `$dynamicRef` and must compile again with it.
var errRestart = errors.New("restart")

func fail(err error) { panic(&compileError{err}) }

func newCompiler(schema any, opts *treeOptions, dynamic bool) *compiler {
	return &compiler{
		r:               newResolver(schema, opts.draft, opts.schemas, opts.reject),
		draft:           opts.draft,
		formats:         opts.formats,
		dynamic:         dynamic,
		schema:          schema,
		scope:           &[]string{},
		cache:           map[*schemaObject]check{},
		annotatingCache: map[*schemaObject]annotatingCheck{},
		edges:           map[vertex][]vertex{},
		guards:          map[vertex]func(){},
	}
}

// root is the check of the root schema.
func (c *compiler) root() check {
	return c.check(c.schema, c.r.rootBase)
}

// check is the check of a schema, cached by schema object; a recursive
// reference goes through an indirection, the slot, which is where a cycle
// on one value is guarded (recursion.go).
func (c *compiler) check(v any, base string) check {
	switch v {
	case true:
		return always
	case false:
		return never
	}
	node, ok := v.(*schemaObject)
	if !ok {
		fail(errors.New("A schema must be an object or a boolean, got " + stringifyNode(v)))
	}
	c.edge(vertex{node, false})
	if cached, ok := c.cache[node]; ok {
		return cached
	}
	if bare := bareType(node); bare != nil {
		return bare
	}
	var slot check
	c.cache[node] = func(v any) bool { return slot(v) }
	c.build(vertex{node, false},
		func() { slot = c.buildCheck(node, c.r.baseFor(node, base)) },
		func(p any) { slot = func(any) bool { panic(p) } },
		func() { slot = c.guard(node, slot) })
	c.cache[node] = slot
	return slot
}

// stringifyNode is JSON.stringify of a value that is not a schema.
func stringifyNode(v any) string {
	if _, ok := v.(undefinedValue); ok {
		return "undefined"
	}
	return stringify(v)
}

func (c *compiler) buildCheck(node *schemaObject, base string) check {
	if ref, ok := node.kw("$ref").(string); ok && c.draft == Draft7 {
		return c.ref(ref, base)
	}
	if c.draft != Draft7 && (node.has("unevaluatedProperties") || node.has("unevaluatedItems")) {
		return withFreshAnnotations(c.annotating(node, base))
	}
	plain := c.plain(node, base)
	if c.dynamic && c.r.isResource(node, base) {
		return scopedCheck(c.scope, base, plain)
	}
	return plain
}

// bareType is the shared check of a schema that is only a `type` (with
// annotations), always for one with no assertion at all; nil for every other
// schema. These are the most common subschemas and cost nothing to share.
func bareType(node *schemaObject) check {
	var typ any
	hasType := false
	for _, m := range node.members {
		if m.Key == "type" {
			typ, hasType = m.Value, true
		} else if !neutralKeywords[m.Key] {
			return nil
		}
	}
	if !hasType {
		return always
	}
	if name, ok := typ.(string); ok {
		return typeChecks[name]
	}
	return nil
}

// typesOf is the `type` keyword as a list, nil when absent.
func typesOf(node *schemaObject) []any {
	t, ok := node.get("type")
	if !ok {
		return nil
	}
	if list, ok := t.([]any); ok {
		return list
	}
	return []any{t}
}

// plain is every keyword of node, not tracking annotations.
func (c *compiler) plain(node *schemaObject, base string) check {
	var parts []check
	types := typesOf(node)
	var single any
	if len(types) == 1 {
		single = types[0]
	}
	typeInGroup := false
	if single == "bytes" {
		parts = append(parts, bytesCheck(numberOr(node.kw("minLength"), 0), numberOr(node.kw("maxLength"), math.Inf(1))))
		typeInGroup = true
	} else {
		str := c.stringGroup(node, single == "string")
		if str != nil {
			parts = append(parts, str)
		}
		numeric := single == "number" || single == "integer" || single == "bigint"
		typ := othersPass
		switch single {
		case "number":
			typ = numberOnly
		case "integer":
			typ = integerOnly
		case "bigint":
			typ = bigintOnly
		}
		num := numberGroup(node, typ)
		if num != nil {
			parts = append(parts, num)
		}
		obj := c.objectGroup(node, base, single == "object")
		if obj != nil {
			parts = append(parts, obj)
		}
		arr := c.arrayGroup(node, base, single == "array")
		if arr != nil {
			parts = append(parts, arr)
		}
		typeInGroup = (single == "string" && str != nil) || (numeric && num != nil) ||
			(single == "object" && obj != nil) || (single == "array" && arr != nil)
	}
	if types != nil && !typeInGroup {
		parts = slices.Insert(parts, 0, typeCheck(types))
	}
	return and(c.inPlace(node, base, parts))
}

// inPlace appends the keywords that apply to the value itself, whatever its
// type.
func (c *compiler) inPlace(node *schemaObject, base string, parts []check) []check {
	if values, ok := node.kw("enum").([]any); ok {
		parts = append(parts, enumCheck(values))
	}
	if value, ok := node.get("const"); ok {
		parts = append(parts, constCheck(value))
	}
	if list, ok := node.kw("allOf").([]any); ok {
		parts = append(parts, and(c.checks(list, base)))
	}
	if list, ok := node.kw("anyOf").([]any); ok {
		parts = append(parts, c.union(list, base, false))
	}
	if list, ok := node.kw("oneOf").([]any); ok {
		parts = append(parts, c.union(list, base, true))
	}
	if not, ok := node.get("not"); ok {
		parts = append(parts, notCheck(c.check(not, base)))
	}
	if cond, ok := node.get("if"); ok && (node.has("then") || node.has("else")) {
		then, otherwise := check(always), check(always)
		if s, ok := node.get("then"); ok {
			then = c.check(s, base)
		}
		if s, ok := node.get("else"); ok {
			otherwise = c.check(s, base)
		}
		parts = append(parts, ifCheck(c.check(cond, base), then, otherwise))
	}
	if ref, ok := node.kw("$ref").(string); ok {
		parts = append(parts, c.ref(ref, base))
	}
	if ref, ok := node.kw("$dynamicRef").(string); ok && c.draft != Draft7 {
		parts = append(parts, c.dynamicRef(ref, base))
	}
	return parts
}

func (c *compiler) checks(list []any, base string) []check {
	out := make([]check, len(list))
	for i, s := range list {
		out[i] = c.check(s, base)
	}
	return out
}

func (c *compiler) union(branches []any, base string, exactlyOne bool) check {
	if u := constUnion(branches, exactlyOne); u != nil {
		return u
	}
	if t := c.tagged(branches, base); t != nil {
		return t
	}
	if exactlyOne {
		return oneCheck(c.checks(branches, base))
	}
	return anyCheck(c.checks(branches, base))
}

// resolve is the resolver's resolve, failing the compile on an error.
func (c *compiler) resolve(ref, base string) resolved {
	target, err := c.r.resolve(ref, base)
	if err != nil {
		fail(err)
	}
	return target
}

func (c *compiler) ref(ref, base string) check {
	target := c.resolve(ref, base)
	chk := c.check(target.node, target.base)
	if c.dynamic && target.base != base {
		return scopedCheck(c.scope, target.base, chk)
	}
	return chk
}

func (c *compiler) dynamicRef(ref, base string) check {
	initial, name, ok := c.bookended(ref, base)
	if !ok {
		return c.ref(ref, base)
	}
	fallback := c.check(initial.node, initial.base)
	lookup := dynamicLookup(c, name, c.check)
	scope := c.scope
	// The schema the scope names is an edge the compile cannot see.
	return c.guard(new(dynamicSite), func(v any) bool {
		for _, uri := range *scope {
			if chk := lookup(uri); chk != nil {
				return chk(v)
			}
		}
		return fallback(v)
	})
}

// dynamicSite is a key of the guard of one `$dynamicRef` check (never
// zero-sized, so each is a distinct pointer).
type dynamicSite struct{ _ byte }

// bookended is the initial target and anchor name of a `$dynamicRef` that
// resolves dynamically; ok is false for one that is a plain reference.
func (c *compiler) bookended(ref, base string) (initial resolved, name string, ok bool) {
	initial = c.resolve(ref, base)
	uri := resolveURI(ref, base)
	name = uri[min(len(documentOf(uri))+1, len(uri)):]
	node, isObject := initial.node.(*schemaObject)
	anchored := isObject && node.kw("$dynamicAnchor") == name
	if name == "" || strings.HasPrefix(name, "/") || !anchored {
		return resolved{}, "", false
	}
	if !c.dynamic {
		panic(errRestart)
	}
	return initial, name, true
}

// dynamicLookup resolves an anchor name against the dynamic scope at run
// time, so a schema with a bookended `$dynamicRef` keeps its compiler.
func dynamicLookup[T any](c *compiler, name string, compile func(any, string) T) func(uri string) T {
	found := map[string]T{}
	missing := map[string]bool{}
	return func(uri string) T {
		key := uri + "#" + name
		if chk, ok := found[key]; ok {
			return chk
		}
		var zero T
		if missing[key] {
			return zero
		}
		target, ok := c.r.dynamicAnchors[key]
		if !ok {
			missing[key] = true
			return zero
		}
		b, ok := c.r.baseOf[target]
		if !ok {
			b = uri
		}
		chk := compile(target, b)
		found[key] = chk
		return chk
	}
}

// tagged is anyOf/oneOf whose branches are all objects with a distinct
// required const on one key: the tag picks the branch.
func (c *compiler) tagged(branches []any, base string) check {
	if len(branches) < 2 {
		return nil
	}
	resolvedBranches := make([]*schemaObject, len(branches))
	for i, branch := range branches {
		b, ok := branch.(*schemaObject)
		if !ok {
			return nil
		}
		node := any(b)
		if ref, ok := b.kw("$ref").(string); ok && len(b.members) == 1 {
			node = c.resolve(ref, c.r.baseFor(b, base)).node
		}
		if resolvedBranches[i], ok = node.(*schemaObject); !ok {
			return nil
		}
	}
	first, ok := resolvedBranches[0].kw("properties").(*schemaObject)
	if !ok {
		return nil
	}
	for _, m := range first.members {
		key := m.Key
		tags := make([]any, len(resolvedBranches))
		distinct := map[any]bool{}
		usable := true
		for i, s := range resolvedBranches {
			tag, ok := tagOf(s, key)
			k, keyed := zeroKey(tag)
			if !ok || !keyed || distinct[k] {
				usable = false
				break
			}
			distinct[k] = true
			tags[i] = tag
		}
		if usable {
			return taggedCheck(key, tags, c.checks(branches, base))
		}
	}
	return nil
}

// tagOf is the tag of a tagged-union branch: a primitive const on a required
// property of an object schema.
func tagOf(branch *schemaObject, key string) (any, bool) {
	required, ok := branch.kw("required").([]any)
	if branch.kw("type") != "object" || !ok || !slices.ContainsFunc(required, func(r any) bool { return sameValueZero(r, key) }) {
		return nil, false
	}
	props, ok := branch.kw("properties").(*schemaObject)
	if !ok {
		return nil, false
	}
	tag, ok := props.kw(key).(*schemaObject)
	if !ok {
		return nil, false
	}
	value, ok := tag.get("const")
	if !ok || !isPrimitive(value) {
		return nil, false
	}
	return value, true
}

// sameValueZero is Array.prototype.includes' equality.
func sameValueZero(a, b any) bool {
	ka, oka := zeroKey(a)
	kb, okb := zeroKey(b)
	return oka && okb && ka == kb
}

// constUnion is anyOf/oneOf whose branches are all `{ const: primitive }`: an
// enum (distinct values make oneOf the same).
func constUnion(branches []any, exactlyOne bool) check {
	values := make([]any, 0, len(branches))
	for _, branch := range branches {
		b, ok := branch.(*schemaObject)
		if !ok || !b.has("const") {
			return nil
		}
		for _, m := range b.members {
			if m.Key != "const" && !neutralKeywords[m.Key] {
				return nil
			}
		}
		value := b.kw("const")
		if !isPrimitive(value) {
			return nil
		}
		if exactlyOne && slices.ContainsFunc(values, func(x any) bool { return sameValueZero(x, value) }) {
			return nil
		}
		values = append(values, value)
	}
	return enumCheck(values)
}

func (c *compiler) stringGroup(node *schemaObject, strict bool) check {
	lo := numberOr(node.kw("minLength"), 0)
	hi := numberOr(node.kw("maxLength"), math.Inf(1))
	var re *pattern
	if source, ok := node.kw("pattern").(string); ok {
		re = c.regex(source)
	}
	var format FormatCheck
	if name, ok := node.kw("format").(string); ok && c.formats != nil {
		format = c.formats[name]
	}
	if lo == 0 && math.IsInf(hi, 1) && re == nil && format == nil {
		return nil
	}
	return stringCheck(strict, lo, hi, re, format)
}

// regex is sharedRegex, failing the compile on a SyntaxError.
func (c *compiler) regex(source string) *pattern {
	re, err := sharedRegex(source)
	if err != nil {
		fail(err)
	}
	return re
}

func numberGroup(node *schemaObject, typ numberType) check {
	lo, hasLo := boundKw(node, "minimum")
	hi, hasHi := boundKw(node, "maximum")
	xlo, hasXlo := boundKw(node, "exclusiveMinimum")
	xhi, hasXhi := boundKw(node, "exclusiveMaximum")
	m, hasMultiple := boundKw(node, "multipleOf")
	if !hasLo && !hasHi && !hasXlo && !hasXhi && !hasMultiple {
		return nil
	}
	inf := math.Inf(1)
	if !hasLo {
		lo = bound{f: -inf}
	}
	if !hasHi {
		hi = bound{f: inf}
	}
	if !hasXlo {
		xlo = bound{f: -inf}
	}
	if !hasXhi {
		xhi = bound{f: inf}
	}
	var multipleOf *bound
	if hasMultiple {
		multipleOf = &m
	}
	return numberCheck(typ, lo, hi, xlo, xhi, multipleOf)
}

func (c *compiler) objectGroup(node *schemaObject, base string, strict bool) check {
	onlyCore := !slices.ContainsFunc(objectExtras, node.has)
	g := guardNone
	if onlyCore {
		g = guardLoose
		if strict {
			g = guardStrict
		}
	}
	// A schema with only the core object keywords gets one closure that also
	// does the type guard.
	core := c.objectCore(node, base, g)
	if core != nil && onlyCore {
		return core
	}
	var parts []check
	if core != nil {
		parts = append(parts, core)
	}
	if s, ok := node.get("propertyNames"); ok {
		parts = append(parts, propertyNamesCheck(c.child(s, base)))
	}
	lo, hasLo := numberKw(node, "minProperties")
	hi, hasHi := numberKw(node, "maxProperties")
	if hasLo || hasHi {
		if !hasLo {
			lo = 0
		}
		if !hasHi {
			hi = math.Inf(1)
		}
		parts = append(parts, propertyCountCheck(lo, hi))
	}
	var required []dependency
	var schemas []dependentSchema
	if c.draft != Draft7 {
		for _, m := range entries(node.kw("dependentRequired")) {
			if list, ok := m.Value.([]any); ok {
				required = append(required, dependency{m.Key, list})
			}
		}
		for _, m := range entries(node.kw("dependentSchemas")) {
			schemas = append(schemas, dependentSchema{m.Key, c.check(m.Value, base)})
		}
	}
	for _, m := range entries(node.kw("dependencies")) {
		if list, ok := m.Value.([]any); ok {
			required = append(required, dependency{m.Key, list})
		} else {
			schemas = append(schemas, dependentSchema{m.Key, c.check(m.Value, base)})
		}
	}
	if len(required) > 0 {
		parts = append(parts, dependentRequiredCheck(required))
	}
	if len(schemas) > 0 {
		parts = append(parts, dependentSchemasCheck(schemas))
	}
	if len(parts) == 0 {
		if strict {
			return isObject
		}
		return nil
	}
	return guardObject(and(parts), strict)
}

func (c *compiler) objectCore(node *schemaObject, base string, g guard) check {
	properties, _ := node.kw("properties").(*schemaObject)
	if properties == nil {
		properties = &schemaObject{}
	}
	var required []string
	if list, ok := node.kw("required").([]any); ok {
		for _, k := range list {
			if s, ok := k.(string); ok {
				required = append(required, s)
			}
		}
	}
	patterns, _ := node.kw("patternProperties").(*schemaObject)
	if patterns == nil {
		patterns = &schemaObject{}
	}
	additional, hasAdditional := node.get("additionalProperties")
	openAdditional := !hasAdditional || additional == true
	if len(properties.members)+len(required)+len(patterns.members) == 0 && openAdditional {
		return nil
	}
	declaredEntries := make([]declared, len(properties.members))
	keys := make([]string, len(properties.members))
	requiredCount := 0
	for i, m := range properties.members {
		isRequired := slices.Contains(required, m.Key)
		if isRequired {
			requiredCount++
		}
		declaredEntries[i] = declared{m.Key, c.child(m.Value, base), isRequired}
		keys[i] = m.Key
	}
	var extraRequired []string
	for _, k := range required {
		if !properties.has(k) {
			extraRequired = append(extraRequired, k)
		}
	}
	pats := make([]patternCheck, len(patterns.members))
	for i, m := range patterns.members {
		pats[i] = patternCheck{c.regex(m.Key), c.child(m.Value, base)}
	}
	var extra check
	if !openAdditional {
		extra = c.child(additional, base)
	}
	if len(pats) == 0 && len(extraRequired) == 0 && (extra == nil || additional == false) {
		return declaredObject(declaredEntries, requiredCount, additional == false, g)
	}
	return openObject(declaredEntries, keys, extraRequired, pats, extra, g)
}

func (c *compiler) arrayGroup(node *schemaObject, base string, strict bool) check {
	var prefix []any
	var rest any
	var hasRest bool
	if items, ok := node.kw("items").([]any); ok {
		prefix = items
		rest, hasRest = node.get("additionalItems")
	} else {
		if list, ok := node.kw("prefixItems").([]any); ok && c.draft != Draft7 {
			prefix = list
		}
		rest, hasRest = node.get("items")
	}
	prefixChecks := c.children(prefix, base)
	var restCheck check
	if hasRest && rest != true {
		restCheck = c.child(rest, base)
	}
	lo, hasLo := numberKw(node, "minItems")
	hi, hasHi := numberKw(node, "maxItems")
	if !hasLo {
		lo = 0
	}
	if !hasHi {
		hi = math.Inf(1)
	}
	contains, hasContains := node.get("contains")
	if !hasContains && node.kw("uniqueItems") != true && (len(prefixChecks) > 0 || restCheck != nil) {
		// The common array: one closure for the guard, the length bounds and
		// the items.
		g := guardLoose
		if strict {
			g = guardStrict
		}
		return listCheck(g, lo, hi, prefixChecks, restCheck)
	}
	var parts []check
	if len(prefixChecks) > 0 || restCheck != nil {
		parts = append(parts, itemsCheck(prefixChecks, restCheck))
	}
	if hasLo || hasHi {
		parts = append(parts, lengthCheck(lo, hi))
	}
	if hasContains {
		minContains, maxContains := 1.0, math.Inf(1)
		if c.draft != Draft7 {
			minContains = numberOr(node.kw("minContains"), 1)
			maxContains = numberOr(node.kw("maxContains"), math.Inf(1))
		}
		parts = append(parts, containsCheck(c.child(contains, base), minContains, maxContains))
	}
	if node.kw("uniqueItems") == true {
		parts = append(parts, uniqueCheck)
	}
	if len(parts) == 0 {
		if strict {
			return isArray
		}
		return nil
	}
	return guardArray(and(parts), strict)
}

// annotating is the annotating check of a schema evaluated in place (only
// below unevaluatedProperties/unevaluatedItems).
func (c *compiler) annotating(v any, base string) annotatingCheck {
	switch v {
	case true:
		return annotatingTrue
	case false:
		return annotatingFalse
	}
	node, ok := v.(*schemaObject)
	if !ok {
		fail(errors.New("A schema must be an object or a boolean, got " + stringifyNode(v)))
	}
	c.edge(vertex{node, true})
	if cached, ok := c.annotatingCache[node]; ok {
		return cached
	}
	var slot annotatingCheck
	c.annotatingCache[node] = func(v any, a *annotations) bool { return slot(v, a) }
	c.build(vertex{node, true},
		func() { slot = c.buildAnnotating(node, c.r.baseFor(node, base)) },
		func(p any) { slot = func(any, *annotations) bool { panic(p) } },
		func() { slot = c.guardAnnotating(vertex{node, true}, slot) })
	c.annotatingCache[node] = slot
	return slot
}

func (c *compiler) buildAnnotating(node *schemaObject, base string) annotatingCheck {
	rest := node.without(func(k string) bool { return annotatingKeywords[k] })
	annotates := len(rest.members) < len(node.members)
	plain := c.plain(rest, base)
	var all annotatingCheck
	if annotates {
		if parts := c.annotatingParts(node, base); len(parts) > 0 {
			all = annotateAll(plain, parts)
		}
	}
	if all == nil {
		all = ignoreAnnotations(plain)
	}
	own := all
	if node.has("unevaluatedProperties") || node.has("unevaluatedItems") {
		own = isolateAnnotations(all)
	}
	if c.dynamic && c.r.isResource(node, base) {
		return scopedAnnotating(c.scope, base, own)
	}
	return own
}

func (c *compiler) annotatingParts(node *schemaObject, base string) []annotatingCheck {
	var parts []annotatingCheck
	if node.has("properties") || node.has("patternProperties") || node.has("additionalProperties") {
		var props []propertyCheck
		for _, m := range entries(node.kw("properties")) {
			props = append(props, propertyCheck{m.Key, c.child(m.Value, base)})
		}
		var pats []patternCheck
		for _, m := range entries(node.kw("patternProperties")) {
			pats = append(pats, patternCheck{c.regex(m.Key), c.child(m.Value, base)})
		}
		var extra check
		if s, ok := node.get("additionalProperties"); ok {
			extra = c.child(s, base)
		}
		parts = append(parts, annotateObject(props, pats, extra))
	}
	if node.has("prefixItems") || node.has("items") || node.has("contains") {
		prefix, ok := node.kw("prefixItems").([]any)
		if !ok {
			prefix, _ = node.kw("items").([]any)
		}
		rest, hasRest := node.get("items")
		if _, isList := rest.([]any); isList {
			rest, hasRest = node.get("additionalItems")
		}
		var restCheck, containsCheck check
		if hasRest {
			restCheck = c.child(rest, base)
		}
		if s, ok := node.get("contains"); ok {
			containsCheck = c.child(s, base)
		}
		parts = append(parts, annotateArray(c.children(prefix, base), restCheck, containsCheck,
			numberOr(node.kw("minContains"), 1), numberOr(node.kw("maxContains"), math.Inf(1))))
	}
	if list, ok := node.kw("allOf").([]any); ok {
		for _, s := range list {
			parts = append(parts, c.annotating(s, base))
		}
	}
	if list, ok := node.kw("anyOf").([]any); ok {
		parts = append(parts, annotateAny(c.annotatings(list, base), false))
	}
	if list, ok := node.kw("oneOf").([]any); ok {
		parts = append(parts, annotateAny(c.annotatings(list, base), true))
	}
	if cond, ok := node.get("if"); ok {
		var then, otherwise annotatingCheck
		if s, ok := node.get("then"); ok {
			then = c.annotating(s, base)
		}
		if s, ok := node.get("else"); ok {
			otherwise = c.annotating(s, base)
		}
		parts = append(parts, annotateIf(c.annotating(cond, base), then, otherwise))
	}
	if ref, ok := node.kw("$ref").(string); ok {
		parts = append(parts, c.annotatingRef(ref, base))
	}
	if ref, ok := node.kw("$dynamicRef").(string); ok {
		parts = append(parts, c.annotatingDynamicRef(ref, base))
	}
	for _, m := range entries(node.kw("dependentSchemas")) {
		parts = append(parts, annotateDependent(m.Key, c.annotating(m.Value, base)))
	}
	if s, ok := node.get("unevaluatedProperties"); ok {
		parts = append(parts, unevaluatedPropertiesCheck(c.child(s, base)))
	}
	if s, ok := node.get("unevaluatedItems"); ok {
		parts = append(parts, unevaluatedItemsCheck(c.child(s, base)))
	}
	return parts
}

func (c *compiler) annotatings(list []any, base string) []annotatingCheck {
	out := make([]annotatingCheck, len(list))
	for i, s := range list {
		out[i] = c.annotating(s, base)
	}
	return out
}

func (c *compiler) annotatingRef(ref, base string) annotatingCheck {
	target := c.resolve(ref, base)
	chk := c.annotating(target.node, target.base)
	if c.dynamic && target.base != base {
		return scopedAnnotating(c.scope, target.base, chk)
	}
	return chk
}

func (c *compiler) annotatingDynamicRef(ref, base string) annotatingCheck {
	initial, name, ok := c.bookended(ref, base)
	if !ok {
		return c.annotatingRef(ref, base)
	}
	fallback := c.annotating(initial.node, initial.base)
	lookup := dynamicLookup(c, name, c.annotating)
	scope := c.scope
	return c.guardAnnotating(new(dynamicSite), func(v any, a *annotations) bool {
		for _, uri := range *scope {
			if chk := lookup(uri); chk != nil {
				return chk(v, a)
			}
		}
		return fallback(v, a)
	})
}

// neutralKeywords never affect a check.
var neutralKeywords = setOf("$schema", "$comment", "title", "description", "default", "examples",
	"deprecated", "readOnly", "writeOnly", "$defs", "definitions", "contentEncoding", "contentMediaType",
	"minContains", "maxContains", "$vocabulary")

// objectExtras are the object keywords besides the core (properties,
// required, patternProperties, additionalProperties).
var objectExtras = []string{"propertyNames", "minProperties", "maxProperties", "dependentRequired",
	"dependentSchemas", "dependencies"}

// annotatingKeywords are the keywords an annotating check handles itself;
// every other keyword compiles into its plain part.
var annotatingKeywords = setOf("properties", "patternProperties", "additionalProperties", "prefixItems",
	"items", "additionalItems", "contains", "minContains", "maxContains", "allOf", "anyOf", "oneOf", "if",
	"then", "else", "$ref", "$dynamicRef", "dependentSchemas", "unevaluatedProperties", "unevaluatedItems")

func setOf(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// numberKw is compiler.ts number(node[key]): the keyword when it is a number.
func numberKw(node *schemaObject, key string) (float64, bool) {
	return numberOf(node.kw(key))
}

// boundKw is compiler.ts bound(node[key]): the keyword when it is a number
// or a bigint (TypeBox's Type.BigInt bounds).
func boundKw(node *schemaObject, key string) (bound, bool) {
	return boundOf(node.kw(key))
}

func numberOr(v any, fallback float64) float64 {
	if f, ok := numberOf(v); ok {
		return f
	}
	return fallback
}

// entries is Object.entries of a keyword that holds an object, none for any
// other value.
func entries(v any) []Member {
	if s, ok := v.(*schemaObject); ok {
		return s.members
	}
	return nil
}
