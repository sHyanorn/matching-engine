package book

import "strconv"

// Every enum in this package keeps its wire names in an array indexed by
// value, with "" for values that have no name. String and the Parse
// functions both read that one table, so each name is written exactly once.

// enumString returns the name of v, or "typeName(n)" if v has no name.
func enumString[E ~uint8](names []string, v E, typeName string) string {
	if int(v) < len(names) && names[v] != "" {
		return names[v]
	}
	return typeName + "(" + strconv.Itoa(int(v)) + ")"
}

// parseEnum returns the value whose name is exactly s. The empty string never
// matches, so unnamed values cannot be parsed.
func parseEnum[E ~uint8](names []string, s string) (E, bool) {
	if s == "" {
		return 0, false
	}
	for i, name := range names {
		if name == s {
			return E(i), true
		}
	}
	return 0, false
}
