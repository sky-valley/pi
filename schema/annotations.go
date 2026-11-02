package schema

// Annotation tracking for `unevaluatedProperties` and `unevaluatedItems`
// (annotations.ts): annotating checks record which properties and items a
// schema evaluated. Only schemas that use those keywords compile into these
// closures.

// annotations is what a schema evaluated: property names (or all of them),
// the evaluated item prefix, items matched by `contains`.
type annotations struct {
	props    map[string]bool
	allProps bool
	items    int
	allItems bool
	indexes  map[int]bool
}

// annotatingCheck is a check that records what it evaluated into a when it
// passes.
type annotatingCheck func(v any, a *annotations) bool

func (a *annotations) merge(b *annotations) {
	for k := range b.props {
		a.addProp(k)
	}
	a.allProps = a.allProps || b.allProps
	a.items = max(a.items, b.items)
	a.allItems = a.allItems || b.allItems
	for i := range b.indexes {
		a.addIndex(i)
	}
}

func (a *annotations) addProp(key string) {
	if a.props == nil {
		a.props = map[string]bool{}
	}
	a.props[key] = true
}

func (a *annotations) addIndex(i int) {
	if a.indexes == nil {
		a.indexes = map[int]bool{}
	}
	a.indexes[i] = true
}

func annotatingTrue(any, *annotations) bool  { return true }
func annotatingFalse(any, *annotations) bool { return false }

// ignoreAnnotations is a plain check in annotating position: it evaluates
// nothing that unevaluated* sees.
func ignoreAnnotations(c check) annotatingCheck {
	return func(v any, _ *annotations) bool { return c(v) }
}

// withFreshAnnotations runs an annotating check as a plain check.
func withFreshAnnotations(c annotatingCheck) check {
	return func(v any) bool { return c(v, &annotations{}) }
}

// scopedAnnotating is c with uri pushed onto the dynamic scope while it runs.
func scopedAnnotating(scope *[]string, uri string, c annotatingCheck) annotatingCheck {
	return func(v any, a *annotations) bool {
		*scope = append(*scope, uri)
		ok := c(v, a)
		*scope = (*scope)[:len(*scope)-1]
		return ok
	}
}

// isolateAnnotations makes unevaluated* see only the annotations of its own
// schema, never those of its siblings in the parent.
func isolateAnnotations(c annotatingCheck) annotatingCheck {
	return func(v any, a *annotations) bool {
		b := &annotations{}
		if !c(v, b) {
			return false
		}
		a.merge(b)
		return true
	}
}

// annotateAll is the non-annotating keywords, then each annotating part in
// order.
func annotateAll(plain check, parts []annotatingCheck) annotatingCheck {
	return func(v any, a *annotations) bool {
		if !plain(v) {
			return false
		}
		for _, p := range parts {
			if !p(v, a) {
				return false
			}
		}
		return true
	}
}

// propertyCheck is one `properties` entry.
type propertyCheck struct {
	key   string
	check check
}

// annotateObject is properties, patternProperties and additionalProperties.
func annotateObject(props []propertyCheck, patterns []patternCheck, additional check) annotatingCheck {
	declaredKeys := make(map[string]bool, len(props))
	for _, p := range props {
		declaredKeys[p.key] = true
	}
	return func(v any, a *annotations) bool {
		if !isObject(v) {
			return true
		}
		var seen []string
		for _, p := range props {
			x, ok := getOwn(v, p.key)
			if !ok {
				continue
			}
			if !p.check(x) {
				return false
			}
			seen = append(seen, p.key)
		}
		for _, k := range ownKeys(v) {
			x, _ := getOwn(v, k)
			matched := declaredKeys[k]
			for _, p := range patterns {
				if p.re.test(k) {
					if !p.check(x) {
						return false
					}
					matched = true
					seen = append(seen, k)
				}
			}
			if !matched && additional != nil {
				if !additional(x) {
					return false
				}
				seen = append(seen, k)
			}
		}
		for _, k := range seen {
			a.addProp(k)
		}
		return true
	}
}

// annotateArray is prefixItems/items/additionalItems and contains with its
// bounds.
func annotateArray(prefix []check, rest, contains check, minContains, maxContains float64) annotatingCheck {
	return func(v any, a *annotations) bool {
		list, ok := v.([]any)
		if !ok {
			return true
		}
		m := min(len(list), len(prefix))
		for i := range m {
			if !prefix[i](list[i]) {
				return false
			}
		}
		if rest != nil {
			for i := len(prefix); i < len(list); i++ {
				if !rest(list[i]) {
					return false
				}
			}
			a.allItems = true
		}
		a.items = max(a.items, m)
		if contains != nil {
			n := 0.0
			for i, x := range list {
				if contains(x) {
					n++
					a.addIndex(i)
				}
			}
			if n < minContains || n > maxContains {
				return false
			}
		}
		return true
	}
}

// annotateAny is anyOf (or oneOf when exactlyOne): every passing branch
// contributes its annotations.
func annotateAny(branches []annotatingCheck, exactlyOne bool) annotatingCheck {
	return func(v any, a *annotations) bool {
		n := 0
		for _, branch := range branches {
			b := &annotations{}
			if branch(v, b) {
				n++
				a.merge(b)
			}
		}
		if exactlyOne {
			return n == 1
		}
		return n > 0
	}
}

// annotateIf is if, then and else: a passing if contributes its annotations
// even without then.
func annotateIf(cond, then, otherwise annotatingCheck) annotatingCheck {
	return func(v any, a *annotations) bool {
		b := &annotations{}
		if cond(v, b) {
			a.merge(b)
			return then == nil || then(v, a)
		}
		return otherwise == nil || otherwise(v, a)
	}
}

// annotateDependent is one dependentSchemas entry.
func annotateDependent(key string, c annotatingCheck) annotatingCheck {
	return func(v any, a *annotations) bool {
		return !isObject(v) || !hasOwn(v, key) || c(v, a)
	}
}

// unevaluatedPropertiesCheck is unevaluatedProperties: every property no
// other keyword evaluated passes c.
func unevaluatedPropertiesCheck(c check) annotatingCheck {
	return func(v any, a *annotations) bool {
		if !isObject(v) || a.allProps {
			return true
		}
		ok := true
		eachMember(v, func(k string, x any) bool {
			ok = a.props[k] || c(x)
			return ok
		})
		if !ok {
			return false
		}
		a.allProps = true
		return true
	}
}

// unevaluatedItemsCheck is unevaluatedItems: every item no other keyword
// evaluated passes c.
func unevaluatedItemsCheck(c check) annotatingCheck {
	return func(v any, a *annotations) bool {
		list, ok := v.([]any)
		if !ok || a.allItems {
			return true
		}
		for i := a.items; i < len(list); i++ {
			if !a.indexes[i] && !c(list[i]) {
				return false
			}
		}
		a.allItems = true
		return true
	}
}
