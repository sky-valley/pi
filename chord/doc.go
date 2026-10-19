// Package chord mirrors upstream's packages/chord (@earendil-works/chord).
//
// This port carries chord's shared base: strict JSON ([IsValue], [CopyJSON]),
// typed context keys ([Key]), and the delta synchronization in chord/delta.
// Upstream's host runtime around that base — contracts, objects, cells,
// links and the host itself — has no Go counterpart yet.
//
// Upstream's Context is a TypeScript reimplementation of Go's context
// package, so chord defines no Context type: operations take a
// [context.Context], cancellation is context.WithCancel, mandatory cleanup is
// context.WithoutCancel, and invocation-scoped values travel under a [Key].
package chord
