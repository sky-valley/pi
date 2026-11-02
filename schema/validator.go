package schema

import (
	"errors"
	"sync"
)

// The drafts Options.Draft names.
const (
	Draft2020 = "2020-12"
	Draft7    = "7"
)

// What a `$ref` that names nothing does (Options.UnresolvedRefs).
const (
	UnresolvedThrow  = "throw"
	UnresolvedReject = "reject"
)

// FormatCheck is a format check: whether value (a string) has the format. It
// is an alias, so a table of plain funcs is a Formats table as it is.
type FormatCheck = func(value string) bool

// Options are compile()'s options (types.ts CompileOptions). The zero value
// compiles draft 2020-12, with `format` an annotation and an unresolved
// `$ref` a compile error.
type Options struct {
	// Draft is the draft whose semantics apply: Draft2020 (the default; any
	// value but Draft7) or Draft7.
	Draft string
	// Formats are the format checks by name. Without one, `format` is an
	// annotation, as draft 2020-12 specifies.
	Formats map[string]FormatCheck
	// Schemas are schema documents by URI, for `$ref`s to other documents.
	Schemas map[string]any
	// Coerce adds Coerce and Parse to the validator, for loosely typed input.
	Coerce bool
	// UnresolvedRefs is what a `$ref` that names nothing (a missing
	// definition, a document not in Schemas) does: UnresolvedThrow (the
	// default; any value but UnresolvedReject) makes Compile fail;
	// UnresolvedReject compiles it as false, so only values at that place
	// fail, with the error "unresolved reference".
	UnresolvedRefs string
}

// treeOptions are Options with the schemas converted once for every compile
// of one Validator.
type treeOptions struct {
	draft   string
	formats map[string]FormatCheck
	schemas map[string]any
	reject  bool
}

// ValidationError is one failure of a value: where it is (a JSON Pointer, ""
// for the value itself), the failing keyword and a message.
type ValidationError struct {
	Path    string `json:"path"`
	Keyword string `json:"keyword"`
	Message string `json:"message"`
}

// Validator is a compiled schema (validator.ts Validator, and its
// CoercingValidator when compiled with Options.Coerce).
//
// Coerce and Parse are methods of the one type, guarded by Options.Coerce,
// where upstream returns a subclass: compile() picks the class by an
// overload on its options, and Go has no overloading on a value, so a second
// type would need a second constructor or a type assertion at every call
// site for no checking gained.
//
// A Validator is safe for concurrent use. Check runs lock-free unless the
// schema tracks a dynamic scope (a bookended `$dynamicRef`), whose scope
// stack the closures share, or applies itself to the same value (a cycle,
// recursion.go), whose guards share theirs; Errors and Coerce build their
// compiles on first use and run one call at a time.
type Validator struct {
	schema  any
	opts    *treeOptions
	coerce  bool
	c       *compiler
	check   check
	serial  bool       // the check shares state: dynamic scope or guards
	checkMu sync.Mutex // serializes check when serial

	explainOnce sync.Once
	explainMu   sync.Mutex
	explainer   *explainer

	coerceOnce sync.Once
	coerceMu   sync.Mutex
	coercer    *coercer
}

// Compile compiles schema into a Validator (validator.ts compile). schema is
// true, false or a schema object in the value model (an Object, a
// map[string]any, or what the builders return); it is read, never modified.
// It fails, with upstream's message, if the schema is malformed, a pattern is
// not a valid regular expression, or a `$ref` names nothing and
// Options.UnresolvedRefs is not UnresolvedReject (register other documents
// with Options.Schemas).
func Compile(schema any, opts Options) (*Validator, error) {
	tree := &treeOptions{
		draft:   Draft2020,
		formats: opts.Formats,
		reject:  opts.UnresolvedRefs == UnresolvedReject,
	}
	if opts.Draft == Draft7 {
		tree.draft = Draft7
	}
	if len(opts.Schemas) > 0 {
		tree.schemas = make(map[string]any, len(opts.Schemas))
		for uri, s := range opts.Schemas {
			tree.schemas[uri] = toTree(s)
		}
	}
	root := toTree(schema)
	c, chk, err := compileRoot(root, tree)
	if err != nil {
		return nil, err
	}
	return &Validator{schema: root, opts: tree, coerce: opts.Coerce, c: c, check: chk, serial: c.dynamic || c.cyclic}, nil
}

// compileRoot is validator.ts compiler(): most schemas compile without
// tracking the dynamic scope; one with a bookended `$dynamicRef` compiles
// again with it.
func compileRoot(schema any, opts *treeOptions) (c *compiler, chk check, err error) {
	c = newCompiler(schema, opts, false)
	chk, err = compileWith(c)
	if errors.Is(err, errRestart) {
		c = newCompiler(schema, opts, true)
		chk, err = compileWith(c)
	}
	return c, chk, err
}

func compileWith(c *compiler) (chk check, err error) {
	defer func() {
		if p := recover(); p != nil {
			if p == errRestart {
				err = errRestart
				return
			}
			if ce, ok := p.(*compileError); ok {
				err = ce.err
				return
			}
			panic(p)
		}
	}()
	return c.root(), nil
}

// mustCompiler is a compiler of the validator's schema for explaining or
// coercing; the schema compiled once already, so it compiles again.
func (v *Validator) mustCompiler() *compiler {
	c, _, err := compileRoot(v.schema, v.opts)
	if err != nil {
		panic("schema: a schema that compiled once failed to compile again: " + err.Error())
	}
	return c
}

// Check reports whether value is valid. A value Errors fails with an error
// (upstream's check throws) is not valid.
func (v *Validator) Check(value any) bool {
	if !v.serial {
		return v.check(value)
	}
	v.checkMu.Lock()
	defer v.checkMu.Unlock()
	ok := false
	err := v.c.try(func() { ok = v.check(value) })
	return err == nil && ok
}

// Errors is why value is invalid: one error per failing keyword, in the
// order upstream reports them, nil when it is valid. The explainer is built
// on the first failure and kept, so a validator whose values always pass
// never pays for it.
//
// The error is upstream's throw out of errors(), and errs is nil with it:
// ErrStackOverflow for a schema that applies itself to the value forever, or
// the compile error of a malformed subschema the check never needed (an `if`
// without `then` or `else`), which explaining compiles on first use.
func (v *Validator) Errors(value any) (errs []ValidationError, err error) {
	if v.Check(value) {
		return nil, nil
	}
	v.explainOnce.Do(func() { v.explainer = newExplainer(v.mustCompiler()) })
	v.explainMu.Lock()
	defer v.explainMu.Unlock()
	if err := v.explainer.c.try(func() { errs = v.explainer.errors(value) }); err != nil {
		return nil, err
	}
	return errs, nil
}

// Coerce is value coerced towards the schema: numeric strings to numbers,
// "true" to true, a single value to a one-item array, an optional null
// dropped, the first union branch the value converts to (coerce.ts lists
// every rule). Only the changed paths are copied, keeping each object's kind
// and member order; value comes back itself when nothing changes, and is
// never modified. Values that cannot be coerced stay as they are. The error
// is upstream's throw out of coerce(): a pattern that is not a valid
// regular expression in a subschema the check never compiled, or
// ErrStackOverflow where a schema object built in Go holds itself in place
// (`allOf` itself).
//
// It panics unless the validator was compiled with Options.Coerce, as
// calling coerce() on upstream's plain Validator throws.
func (v *Validator) Coerce(value any) (any, error) {
	if !v.coerce {
		panic("schema: Coerce needs a Validator compiled with Options.Coerce")
	}
	v.coerceOnce.Do(func() { v.coercer = &coercer{c: v.mustCompiler()} })
	v.coerceMu.Lock()
	defer v.coerceMu.Unlock()
	var coerced any
	if err := v.coercer.c.try(func() { coerced = v.coercer.coerce(value) }); err != nil {
		return nil, err
	}
	return coerced, nil
}

// Parse coerces value, then checks it: the coerced value, and nil errs when
// it is valid, else the errors of the coerced value (upstream's ParseResult,
// whose `ok` is len(errs) == 0). err is an error Coerce or Errors returns,
// upstream's throw out of parse(); coerced and errs are nil with it. It
// panics as Coerce does.
func (v *Validator) Parse(value any) (coerced any, errs []ValidationError, err error) {
	if coerced, err = v.Coerce(value); err != nil {
		return nil, nil, err
	}
	if errs, err = v.Errors(coerced); err != nil {
		return nil, nil, err
	}
	return coerced, errs, nil
}
