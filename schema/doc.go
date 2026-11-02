// Package schema mirrors upstream's packages/schema (@earendil-works/pi-schema)
// at 6536f8012: JSON Schema validation, drafts 2020-12 and 7, compiled once
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
//     ECMAScript fails Compile with V8's SyntaxError message, as upstream's
//     compile throws.
//   - A map[string]any has no key order; its keys are listed as a
//     JavaScript object would list keys added in sorted order (value.go).
//   - Go has no undefined: a property is either present or absent.
//   - The regular-expression cache keeps its entries, where upstream's holds
//     them weakly; the TypeScript type inference (Static, StaticObject) has
//     no Go form.
package schema
