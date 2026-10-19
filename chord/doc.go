// Package chord is an application-composition runtime for systems assembled
// from plugins: facets, services, replicated state, and a pluggable
// remote-service boundary. It mirrors upstream's packages/chord and, like it,
// depends on nothing else in the monorepo — only internal/jsonstrict, the
// strict tree decoder, which is the Go analogue of the node: builtins
// upstream's boundary test permits.
//
// This file and its siblings hold the foundation layer: strict JSON
// ([IsValue]), service identities ([DefineService]), the error codes that may
// cross a service boundary ([RemoteServiceError]), and typed context keys
// ([Key]). Upstream's Context is a TypeScript reimplementation of Go's
// context package, so chord defines no Context type: operations take a
// [context.Context], cancellation is context.WithCancel, mandatory cleanup is
// context.WithoutCancel, and invocation-scoped values travel under a [Key].
package chord
