package world

import "strings"

// Derived expressions are tiny: identifiers joined by && and ||, optional leading !, no parentheses.
// && binds tighter than ||.

func exprTokens(expr string) []string {
	var out []string
	for _, f := range strings.Fields(strings.NewReplacer("&&", " ", "||", " ", "!", " ").Replace(expr)) {
		out = append(out, f)
	}
	return out
}

// Eval evaluates expr against state. Missing keys and non-bools count as false.
func Eval(expr string, state map[string]any) bool {
	truthy := func(k string) bool {
		k = strings.TrimSpace(k)
		neg := false
		for strings.HasPrefix(k, "!") {
			neg = !neg
			k = strings.TrimSpace(k[1:])
		}
		v, _ := state[k].(bool)
		if neg {
			return !v
		}
		return v
	}
	for _, orPart := range strings.Split(expr, "||") {
		all := true
		for _, andPart := range strings.Split(orPart, "&&") {
			if !truthy(andPart) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
