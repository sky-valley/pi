package schema

import "errors"

// ErrStackOverflow is the error of a value whose check would recurse
// forever: a schema that applies itself to the same value, as
// `{"$ref":"#"}` or `{"allOf":[{"$ref":"#"}]}` do, met a value that reaches
// that cycle. Such a schema compiles, and values the check decides before
// reaching the cycle keep their results; upstream's check, errors and parse
// overflow V8's stack there and throw RangeError with this message. Errors
// and Parse return it, and Check reports such a value invalid. Coerce, as
// upstream's coerce catches the throw of a check, takes such a branch as
// failing; it returns ErrStackOverflow only where its own walk would recur
// forever, on a schema object built in Go that holds itself.
var ErrStackOverflow = errors.New("Maximum call stack size exceeded")

// Recursion on one value.
//
// A check recurses into its subschemas, on the value itself (allOf, anyOf,
// oneOf, not, if/then/else, $ref, $dynamicRef, dependentSchemas) or on a
// child of it (properties, items, contains, propertyNames, unevaluated*, …).
// A chain of the first kind that comes back to its start is a cycle that
// never ends: the checks are pure functions of the schema node, the value
// and the dynamic scope, so the same node met again on the same value, with
// no new URI in the scope, does what it did the first time. Go cannot
// recover from a stack overflow, so the compiler finds those cycles and the
// checks on them stop one:
//
//   - While compiling, it records the in-place graph: an edge from each
//     check (or annotating check) being built to each subschema check it
//     applies to the same value. Steps into a child value push a step frame,
//     which no edge crosses.
//   - When a compile returns to the top, it searches the vertices it built
//     for strongly connected components (Tarjan) and guards each vertex of a
//     cycle. A recursive reference to a check still being built goes through
//     its forwarder (the slot in compiler.check), and every cycle has such an
//     edge: into the vertex of the cycle whose build started first, from its
//     predecessor, built inside that build. So guarding the forwarder, which
//     the slot is, stops every cycle; the closures that hold the finished
//     check directly pay nothing.
//   - A guard keeps the running guarded checks on the compiler's stack
//     (active) and fails with ErrStackOverflow when its node starts again on
//     the value it is already running on, with no new URI in the dynamic
//     scope since. The values a check runs on below one value are that value
//     or its descendants, so "the same value" is the same container, or any
//     value under a leaf (a leaf has no descendants).
//   - In a schema that tracks the dynamic scope, a `$dynamicRef` reaches the
//     schema its scope names at run time, an edge the compile cannot see, so
//     each bookended `$dynamicRef` check is guarded too.
//
// The guards share state per compiler, so a Validator whose check has a
// cycle checks one value at a time, as a dynamic-scope one does; every other
// Validator's Check stays lock-free. The explainer's walk has its own guard
// (explainRun.walking), the coercer its own (coerceRun.inPlace).
//
// Recursion that steps into children is bounded by the value's depth, and
// Go's stacks grow far beyond V8's: a value nested a few thousand levels
// deep under a recursive schema validates in Go, where upstream overflows.

// vertex is a schema node's check, or its annotating check, in the in-place
// graph.
type vertex struct {
	node       *schemaObject
	annotating bool
}

// frame is one entry of the compile stack: a vertex being built, or a step
// into a child value.
type frame struct {
	v    vertex
	step bool
}

// activation is a guarded check running on value, with the length of the
// dynamic scope when it started.
type activation struct {
	key   any
	value any
	scope int
}

// edge records that the check on top of the compile stack applies `to` to
// the same value.
func (c *compiler) edge(to vertex) {
	if n := len(c.building); n > 0 && !c.building[n-1].step {
		from := c.building[n-1].v
		c.edges[from] = append(c.edges[from], to)
	}
}

// build runs fn, which builds the check of v, on the compile stack. guard
// guards v's forwarder. If fn panics, failed makes the forwarder raise the
// same panic, so every later call fails as the first did, not on a missing
// check. Back at the top of the stack, the vertices built since the last
// search are searched for cycles.
func (c *compiler) build(v vertex, fn func(), failed func(p any), guard func()) {
	c.building = append(c.building, frame{v: v})
	c.fresh = append(c.fresh, v)
	c.guards[v] = guard
	defer func() {
		c.building = c.building[:len(c.building)-1]
		p := recover()
		if p != nil {
			failed(p)
		}
		if len(c.building) == 0 {
			c.guardCycles()
		}
		if p != nil {
			panic(p)
		}
	}()
	fn()
}

// child is the check of a schema that applies to a child value.
func (c *compiler) child(s any, base string) check {
	c.building = append(c.building, frame{step: true})
	defer func() { c.building = c.building[:len(c.building)-1] }()
	return c.check(s, base)
}

func (c *compiler) children(list []any, base string) []check {
	out := make([]check, len(list))
	for i, s := range list {
		out[i] = c.child(s, base)
	}
	return out
}

// guardCycles guards every fresh vertex on a cycle of the in-place graph
// (Tarjan's strongly connected components). Older vertices never reach the
// fresh ones: their checks were built before them.
func (c *compiler) guardCycles() {
	fresh := c.fresh
	c.fresh = nil
	isFresh := make(map[vertex]bool, len(fresh))
	for _, v := range fresh {
		isFresh[v] = true
	}
	index := map[vertex]int{}
	low := map[vertex]int{}
	onStack := map[vertex]bool{}
	var stack []vertex
	var visit func(v vertex)
	visit = func(v vertex) {
		index[v] = len(index)
		low[v] = index[v]
		stack = append(stack, v)
		onStack[v] = true
		selfLoop := false
		for _, w := range c.edges[v] {
			if !isFresh[w] {
				continue
			}
			if w == v {
				selfLoop = true
			}
			if _, seen := index[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		i := len(stack) - 1
		for stack[i] != v {
			i--
		}
		component := stack[i:]
		stack = stack[:i]
		for _, w := range component {
			onStack[w] = false
		}
		if len(component) > 1 || selfLoop {
			c.cyclic = true
			for _, w := range component {
				c.guards[w]()
			}
		}
	}
	for _, v := range fresh {
		if _, seen := index[v]; !seen {
			visit(v)
		}
	}
	for _, v := range fresh {
		delete(c.edges, v)
		delete(c.guards, v)
	}
}

// guard is inner stopped where it starts again on the value it is running
// on (see "Recursion on one value").
func (c *compiler) guard(key any, inner check) check {
	return func(v any) bool {
		c.enter(key, v)
		ok := inner(v)
		c.active = c.active[:len(c.active)-1]
		return ok
	}
}

func (c *compiler) guardAnnotating(key any, inner annotatingCheck) annotatingCheck {
	return func(v any, a *annotations) bool {
		c.enter(key, v)
		ok := inner(v, a)
		c.active = c.active[:len(c.active)-1]
		return ok
	}
}

// enter pushes the activation of key on v, failing with ErrStackOverflow when
// it repeats the innermost one of key.
func (c *compiler) enter(key, v any) {
	for i := len(c.active) - 1; i >= 0; i-- {
		if a := c.active[i]; a.key == key {
			if sameOrAncestor(a.value, v) && !c.scopeGrew(a.scope) {
				fail(ErrStackOverflow)
			}
			break
		}
	}
	c.active = append(c.active, activation{key, v, len(*c.scope)})
}

// sameOrAncestor is whether w, which is u or a descendant of u, is u: always
// when u has no descendants, else when w is the same container.
func sameOrAncestor(u, w any) bool {
	switch x := u.(type) {
	case Object:
		if len(x) == 0 {
			return true
		}
	case []any:
		if len(x) == 0 {
			return true
		}
	case map[string]any:
		if len(x) == 0 {
			return true
		}
	case *schemaObject:
		if len(x.members) == 0 {
			return true
		}
	default:
		return true
	}
	a, _ := identityOf(u)
	b, _ := identityOf(w)
	return a == b
}

// scopeGrew is whether the dynamic scope gained a URI it did not hold at
// length n: only the first place of each URI decides a `$dynamicRef`.
func (c *compiler) scopeGrew(n int) bool {
	scope := *c.scope
	if len(scope) == n {
		return false
	}
	held := make(map[string]bool, n)
	for _, uri := range scope[:n] {
		held[uri] = true
	}
	for _, uri := range scope[n:] {
		if !held[uri] {
			return true
		}
	}
	return false
}

// try runs f, returning the error it throws upstream: a compile failure of a
// subschema compiled on first use, or ErrStackOverflow. The dynamic scope
// and the guards' stack are restored, which a throw leaves unwound. Any
// other panic is a bug and goes on.
func (c *compiler) try(f func()) (err error) {
	scope, active := len(*c.scope), len(c.active)
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		*c.scope = (*c.scope)[:scope]
		c.active = c.active[:active]
		if ce, ok := p.(*compileError); ok {
			err = ce.err
			return
		}
		if p == errRestart {
			err = errRestart
			return
		}
		panic(p)
	}()
	f()
	return nil
}
