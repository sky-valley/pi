// Package render is the Go port of pi's trace renderer
// (@earendil-works/pi-telemetry/render, upstream d9025a5f5): stored spans as
// plain text, a subpath kept out of the telemetry package as pi keeps it out
// of the root entry.
package render

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
	"github.com/sky-valley/pi/telemetry"
)

// Trace renders spans as a chronological forest, one line per span, event
// and link (pi renderTrace):
//
//	name key=value (12ms) error Name: message
//	  · event key=value (+3ms)
//	  → link trace <trace> span <span> key=value
//	  child (4ms)
//
// Spans with the same ID are merged from several stores and rendered once,
// the last one at the first's place. A span whose parent is absent is a
// root. Roots and each span's children are in start order, ties keeping
// their input order; links are printed, never followed. Values are
// JSON.stringify's text, numbers JavaScript's. pi lists attributes in
// insertion order, which a Go map does not keep: they come out array-index
// keys first, ascending, then by UTF-16 code unit. The input is not
// modified, and a tree of any depth renders without recursion.
func Trace(spans []telemetry.StoredSpan) string {
	// pi's Map: a duplicate ID keeps its first position and its last value.
	var ordered []string
	byID := map[string]telemetry.StoredSpan{}
	for _, span := range spans {
		if _, seen := byID[span.ID]; !seen {
			ordered = append(ordered, span.ID)
		}
		byID[span.ID] = span
	}
	unique := make([]telemetry.StoredSpan, 0, len(ordered))
	for _, id := range ordered {
		unique = append(unique, byID[id])
	}
	slices.SortStableFunc(unique, func(left, right telemetry.StoredSpan) int {
		switch {
		case left.StartedMs < right.StartedMs:
			return -1
		case left.StartedMs > right.StartedMs:
			return 1
		}
		return 0
	})

	var roots []telemetry.StoredSpan
	children := map[string][]telemetry.StoredSpan{}
	for _, span := range unique {
		if _, hasParent := byID[span.ParentID]; span.ParentID == "" || !hasParent {
			roots = append(roots, span)
		} else {
			children[span.ParentID] = append(children[span.ParentID], span)
		}
	}

	type entry struct {
		span  telemetry.StoredSpan
		depth int
	}
	pending := make([]entry, 0, len(roots))
	for index := len(roots) - 1; index >= 0; index-- {
		pending = append(pending, entry{roots[index], 0})
	}
	var lines []string
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		span, indent := next.span, strings.Repeat("  ", next.depth)
		failure := ""
		if span.Status.Code == telemetry.StatusError {
			failure = " error"
			if span.Status.Error != nil {
				failure += fmt.Sprintf(" %s: %s", span.Status.Error.Name, span.Status.Error.Message)
			}
		}
		lines = append(lines, fmt.Sprintf("%s%s%s (%sms)%s", indent, span.Name, formatAttributes(span.Attributes), jstext.NumberToString(span.EndedMs-span.StartedMs), failure))
		for _, event := range span.Events {
			lines = append(lines, fmt.Sprintf("%s  · %s%s (+%sms)", indent, event.Name, formatAttributes(event.Attributes), jstext.NumberToString(event.TimeMs)))
		}
		for _, link := range span.Links {
			lines = append(lines, fmt.Sprintf("%s  → link trace %s span %s%s", indent, link.TraceID, link.SpanID, formatAttributes(link.Attributes)))
		}
		descendants := children[span.ID]
		for index := len(descendants) - 1; index >= 0; index-- {
			pending = append(pending, entry{descendants[index], next.depth + 1})
		}
	}
	return strings.Join(lines, "\n")
}

// formatAttributes writes each attribute as " key=<JSON.stringify(value)>".
func formatAttributes(attributes telemetry.SpanAttributes) string {
	var b strings.Builder
	for _, key := range objectKeyOrder(attributes) {
		b.WriteString(" " + key + "=" + stringify(attributes[key]))
	}
	return b.String()
}

// stringify is JSON.stringify of an attribute value; a nil is pi's
// undefined, which the template literal writes as "undefined".
func stringify(value telemetry.AttributeValue) string {
	if value == nil {
		return "undefined"
	}
	text, err := jstext.Stringify(value)
	if err != nil {
		// An attribute value outside the contract (a channel, a func):
		// unspecified, as pi's malformed input is. Show what it is.
		return fmt.Sprint(value)
	}
	return text
}

// objectKeyOrder lists attributes' keys in the order an object whose keys
// were inserted sorted enumerates them: array indices ascending, then the
// rest by UTF-16 code unit.
func objectKeyOrder(attributes telemetry.SpanAttributes) []string {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int {
		x, aIndex := jstext.ArrayIndexKey(a)
		y, bIndex := jstext.ArrayIndexKey(b)
		switch {
		case aIndex && bIndex:
			return cmp.Compare(x, y)
		case aIndex:
			return -1
		case bIndex:
			return 1
		}
		return jstext.CompareUTF16(a, b)
	})
	return keys
}
