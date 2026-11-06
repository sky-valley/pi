// Package schema mirrors upstream's packages/schema (@earendil-works/pi-schema)
// at 55a5c6914: JSON Schema validation, drafts 2020-12 and 7, compiled once
// into closures. Compile builds a Validator whose Check is the boolean fast
// path and whose Errors explains a failure as JSON Pointer paths, keywords
// and messages — text pi-ai hands to the model, so it, and its order, follow
// upstream exactly. Compiled with Options.Coerce, a Validator also coerces
// loosely typed input (Coerce, Parse) by upstream's three passes. The
// builders (Str, Obj, Union, …) return plain JSON Schema objects. The format
// checks are the schema/formats package.
//
// Schemas and values use the value model value.go describes: JSON values as
// Go holds them, objects as an Object (ordered members) or a
// map[string]any. Decode reads JSON text into it with JSON.parse's key
// order.
//
// Where the port differs from upstream:
//
//   - Regular expressions (`pattern`, `patternProperties`) are ECMAScript
//     with the `u` flag upstream; the port translates them to RE2 (see
//     translateRegex). A pattern RE2 cannot express — a lookaround, a
//     backreference, a repeat count over 1000, a Unicode property RE2 lacks,
//     half a surrogate pair — matches every string, as the port's earlier
//     tool validator treated such patterns. A pattern that is invalid
//     ECMAScript (a duplicate group name included) fails Compile with V8's
//     SyntaxError message, as upstream's compile throws. A pattern the `u`
//     flag rejects is read without it, by UTF-16 code unit, as upstream
//     does: it and the strings it tests are matched in unit space (see
//     unitRune), so `.` takes half of "😀" there too. With the `u` flag, half
//     a surrogate pair in a string (a lone "\ud83d" escape in its JSON) is
//     not one character to RE2 but three bytes that are not UTF-8, so `.`
//     or `[^a]` does not match it as upstream's does.
//   - A schema that applies itself to the same value (`{"$ref":"#"}`,
//     `{"allOf":[{"$ref":"#"}]}`) overflows V8's stack where a value reaches
//     the cycle, and upstream's check, errors and parse throw RangeError;
//     Go cannot recover from a stack overflow, so the port finds such cycles
//     (recursion.go): Errors and Parse return ErrStackOverflow there,
//     Check, which returns no error, reports the value invalid, and Coerce,
//     as upstream's coerce catches the throw, takes such a branch as not
//     passing. Such a schema's Check runs one call at a time. Recursion
//     into children is bounded by the value instead, and Go's stack outgrows
//     V8's: a value nested some thousands of levels deep under a recursive
//     schema validates in Go where upstream overflows.
//   - Where errors() or coerce() meets a malformed subschema the check never
//     compiled, every later call fails with the same error; upstream's cache
//     keeps the half-built check, and its later calls throw "slot.check is
//     not a function".
//   - A map[string]any has no key order; its keys are listed as a
//     JavaScript object would list keys added in sorted order (value.go).
//   - JavaScript's undefined is the Undefined value and a bigint a *big.Int
//     (value.go), which JSON never holds; a nil *big.Int is no bigint.
//   - The regular-expression cache keeps its entries, where upstream's holds
//     them weakly; the TypeScript type inference (Static, StaticObject) has
//     no Go form.
package schema
