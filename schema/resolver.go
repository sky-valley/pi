package schema

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// defaultBase is resolver.ts DEFAULT_BASE: the base URI of a root schema
// without `$id`; relative references resolve against it.
const defaultBase = "https://json-schema.invalid/root.json"

// resolved is a schema and the base URI its references resolve against.
type resolved struct {
	node any
	base string
}

// resolver is resolver.ts Resolver: reference resolution of one compile over
// the root schema and the registered documents. The schema is indexed on the
// first reference or the first `$id` a walker meets, so a schema with neither
// is never walked for identifiers.
type resolver struct {
	draft string
	// rootBase is the base URI the root schema is walked from: always the
	// default base. Walkers apply the root's own `$id` through baseFor, once
	// (upstream 2bd2c2d0a; applying it here as well put a relative `$id`
	// with a directory part in twice).
	rootBase string
	// baseOf is each indexed schema object's base URI.
	baseOf map[*schemaObject]string
	// dynamicAnchors is the schema of every `$dynamicAnchor`, by URI with a
	// `#name` fragment.
	dynamicAnchors map[string]*schemaObject

	root      any
	schemas   map[string]any // options.Schemas, converted
	registry  map[string]any
	reject    bool
	resources map[string]any
	anchors   map[string]*schemaObject
	// unresolved holds the references that named nothing and resolved to
	// false (with UnresolvedRefs "reject").
	unresolved map[string]bool
	indexed    bool
}

func newResolver(root any, draft string, schemas map[string]any, reject bool) *resolver {
	r := &resolver{
		draft:          draft,
		baseOf:         map[*schemaObject]string{},
		dynamicAnchors: map[string]*schemaObject{},
		root:           root,
		schemas:        schemas,
		registry:       map[string]any{},
		reject:         reject,
		resources:      map[string]any{},
		anchors:        map[string]*schemaObject{},
		unresolved:     map[string]bool{},
		rootBase:       defaultBase,
	}
	return r
}

// baseFor is the base URI of `node`, whose parent has `base`: its `$id`
// changes it, unless draft 7 makes the `$id` inert. Once the schema is
// indexed, the indexed base.
func (r *resolver) baseFor(node *schemaObject, base string) string {
	if indexed, ok := r.baseOf[node]; ok {
		return indexed
	}
	id, ok := node.kw("$id").(string)
	if !ok {
		return base
	}
	// Index before applying an `$id`: a caller's base may already include
	// it, and the index applies it once (upstream 2bd2c2d0a).
	if !r.indexed {
		r.ensureIndexed()
		if indexed, ok := r.baseOf[node]; ok {
			return indexed
		}
	}
	if _, ref := node.kw("$ref").(string); r.draft == Draft7 && (ref || strings.HasPrefix(id, "#")) {
		return base
	}
	return documentOf(resolveURI(id, base))
}

// ensureIndexed indexes the root schema and the registered documents' URIs,
// once.
func (r *resolver) ensureIndexed() {
	if r.indexed {
		return
	}
	r.indexed = true
	uris := make([]string, 0, len(r.schemas))
	for uri := range r.schemas {
		uris = append(uris, uri)
	}
	slices.SortFunc(uris, compareKeys) // a map has no order; see the value model
	for _, uri := range uris {
		r.registry[documentOf(resolveURI(uri, defaultBase))] = r.schemas[uri]
	}
	r.resources[defaultBase] = r.root
	r.index(r.root, defaultBase)
}

// resolve is the schema a reference names, with the base URI it compiles
// under. It fails if the reference names nothing, unless unresolved
// references are rejected: then it resolves to false, the schema no value
// passes.
func (r *resolver) resolve(ref, base string) (resolved, error) {
	r.ensureIndexed()
	target, err := r.find(ref, base)
	if err != nil && r.reject {
		r.unresolved[resolveURI(ref, base)] = true
		return resolved{node: false, base: base}, nil
	}
	return target, err
}

// isUnresolved reports whether `ref`, from `base`, named nothing and resolved
// to false.
func (r *resolver) isUnresolved(ref, base string) bool {
	return r.unresolved[resolveURI(ref, base)]
}

func (r *resolver) find(ref, base string) (resolved, error) {
	uri := resolveURI(ref, base)
	doc := documentOf(uri)
	fragment := ""
	if len(uri) > len(doc) {
		var err error
		if fragment, err = decodeURIComponent(uri[len(doc)+1:]); err != nil {
			return resolved{}, err
		}
	}
	root, ok := r.resources[doc]
	if !ok {
		root, ok = r.load(doc)
	}
	if !ok {
		return resolved{}, fmt.Errorf("Unresolved $ref %s (no schema registered for %s)", ref, doc)
	}
	if fragment == "" {
		return resolved{node: root, base: doc}, nil
	}
	if strings.HasPrefix(fragment, "/") {
		return r.pointer(root, doc, fragment)
	}
	target, ok := r.anchors[doc+"#"+fragment]
	if !ok {
		return resolved{}, fmt.Errorf("Unresolved $ref %s (no anchor %s in %s)", ref, fragment, doc)
	}
	if b, ok := r.baseOf[target]; ok {
		return resolved{node: target, base: b}, nil
	}
	return resolved{node: target, base: doc}, nil
}

// isResource reports whether `node` is the root of the schema resource at
// `base`.
func (r *resolver) isResource(node *schemaObject, base string) bool {
	r.ensureIndexed()
	s, ok := r.resources[base].(*schemaObject)
	return ok && s == node
}

// index walks only keywords that hold subschemas: an `$id` inside `enum`,
// `const` or an unknown keyword names nothing.
func (r *resolver) index(v any, base string) {
	node, ok := v.(*schemaObject)
	if !ok {
		return
	}
	if _, done := r.baseOf[node]; done {
		return
	}
	b := base
	// In draft 7 a `$ref` makes every sibling keyword, `$id` included, inert.
	_, hasRef := node.kw("$ref").(string)
	refOnly := r.draft == Draft7 && hasRef
	if id, ok := node.kw("$id").(string); ok && !refOnly {
		if r.draft == Draft7 && strings.HasPrefix(id, "#") {
			r.anchors[documentOf(b)+id] = node
		} else {
			uri := resolveURI(id, b)
			b = documentOf(uri)
			r.resources[b] = node
			if len(uri) > len(b)+1 {
				r.anchors[uri] = node
			}
		}
	}
	r.baseOf[node] = b
	if refOnly {
		return
	}
	if r.draft != Draft7 {
		if anchor, ok := node.kw("$anchor").(string); ok {
			r.anchors[b+"#"+anchor] = node
		}
		if anchor, ok := node.kw("$dynamicAnchor").(string); ok {
			r.anchors[b+"#"+anchor] = node
			r.dynamicAnchors[b+"#"+anchor] = node
		}
	}
	for _, key := range schemaKeywords {
		r.index(node.kw(key), b)
	}
	for _, key := range mapKeywords {
		if m, ok := node.kw(key).(*schemaObject); ok {
			for _, member := range m.members {
				r.index(member.Value, b)
			}
		}
	}
	for _, key := range arrayKeywords {
		if list, ok := node.kw(key).([]any); ok {
			for _, s := range list {
				r.index(s, b)
			}
		}
	}
}

func (r *resolver) load(doc string) (any, bool) {
	schema, ok := r.registry[doc]
	if !ok {
		return nil, false
	}
	r.resources[doc] = schema
	r.index(schema, doc)
	return schema, true
}

// pointer resolves a JSON Pointer fragment within `root`.
func (r *resolver) pointer(root any, base, fragment string) (resolved, error) {
	node := root
	b := base
	for _, raw := range strings.Split(fragment, "/")[1:] {
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		next, ok := pointerStep(node, key)
		if !ok {
			return resolved{}, fmt.Errorf("Unresolved $ref #%s in %s", fragment, base)
		}
		node = next
		if s, ok := node.(*schemaObject); ok {
			if nb, ok := r.baseOf[s]; ok {
				b = nb
			}
		}
	}
	return resolved{node: node, base: b}, nil
}

// pointerStep is `Object.hasOwn(node, key) ? node[key] : undefined` for an
// object or array node: an object's member, an array's item or its length.
func pointerStep(node any, key string) (any, bool) {
	switch n := node.(type) {
	case *schemaObject:
		return n.get(key)
	case []any:
		if key == "length" {
			return float64(len(n)), true
		}
		if i, ok := jstext.ArrayIndexKey(key); ok && int(i) < len(n) {
			return n[i], true
		}
	}
	return nil, false
}

var (
	schemaKeywords = []string{
		"additionalProperties", "propertyNames", "contains", "not", "if", "then", "else",
		"unevaluatedProperties", "unevaluatedItems", "additionalItems", "contentSchema", "items",
	}
	mapKeywords   = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"}
	arrayKeywords = []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"}
)
