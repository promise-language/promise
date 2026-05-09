package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// hintf records a hint message at the given position.
// Hints appear as separate "hint: ..." lines after the main error.
func (c *Checker) hintf(pos ast.Pos, format string, args ...any) {
	c.errorf(pos, "hint: "+format, args...)
}

// catalogTypes maps well-known public type/enum names from catalog modules
// to the module that defines them. Used for "did you mean?" suggestions
// when a user references a catalog type without importing the module.
var catalogTypes = map[string]string{
	// io
	"File":           "io",
	"Dir":            "io",
	"IoError":        "io",
	"BufferedReader": "io",
	"BufferedWriter": "io",
	// json
	"JsonEncoder": "json",
	"JsonDecoder": "json",
	"JsonValue":   "json",
	// os
	"Process":       "os",
	"ProcessResult": "os",
	"ProcessInput":  "os",
	"ProcessOutput": "os",
	"OsError":       "os",
	"Signal":        "os",
}

// catalogFuncs maps well-known public free functions from catalog modules.
var catalogFuncs = map[string]string{
	// io
	"read_line":  "io",
	"read_stdin": "io",
	// json
	"encode_string": "json",
	"decode_string": "json",
	"parse_value":   "json",
	// os
	"get_env_var":  "os",
	"exit_process": "os",
	"execute":      "os",
}

// suggestForUndefinedIdent emits hints after an "undefined: X" error.
// Checks catalog types/funcs first, then tries Levenshtein on scope names.
func (c *Checker) suggestForUndefinedIdent(pos ast.Pos, name string) {
	// Check catalog types
	if mod, ok := catalogTypes[name]; ok {
		c.hintf(pos, "%s is defined in module %s — add `use %s;` and use `%s.%s`", name, mod, mod, mod, name)
		return
	}
	// Check catalog functions
	if mod, ok := catalogFuncs[name]; ok {
		c.hintf(pos, "%s is defined in module %s — add `use %s;` and use `%s.%s()`", name, mod, mod, mod, name)
		return
	}
	// Try Levenshtein on all names in scope
	if suggestion := c.suggestName(name, nil); suggestion != "" {
		c.hintf(pos, "did you mean %s?", suggestion)
	}
}

// suggestForUndefinedType emits hints after an "undefined type: X" error.
// Checks catalog types first, then tries Levenshtein on type names in scope.
func (c *Checker) suggestForUndefinedType(pos ast.Pos, name string) {
	if mod, ok := catalogTypes[name]; ok {
		c.hintf(pos, "%s is defined in module %s — add `use %s;` and use `%s.%s`", name, mod, mod, mod, name)
		return
	}
	if suggestion := c.suggestTypeName(name); suggestion != "" {
		c.hintf(pos, "did you mean %s?", suggestion)
	}
}

// suggestForUndefinedModule emits hints after an "undefined module: X" error.
func (c *Checker) suggestForUndefinedModule(pos ast.Pos, name string) {
	// Check if it's a known catalog module name
	knownModules := []string{"io", "json", "os", "path", "math", "strings", "time", "http"}
	for _, mod := range knownModules {
		if name == mod {
			c.hintf(pos, "add `use %s;` at the top of the file to import the %s module", mod, mod)
			return
		}
	}
	// Try Levenshtein against imported module names and known catalog names
	candidates := c.collectModuleNames()
	candidates = append(candidates, knownModules...)
	if suggestion := closestMatch(name, candidates); suggestion != "" {
		c.hintf(pos, "did you mean %s?", suggestion)
	}
}

// suggestName finds the closest name in the current scope chain.
// If filter is non-nil, only names where filter returns true are considered.
func (c *Checker) suggestName(name string, filter func(types.Object) bool) string {
	var candidates []string
	for scope := c.scope; scope != nil; scope = scope.Parent() {
		for _, n := range scope.Names() {
			if filter != nil {
				if obj := scope.Lookup(n); obj != nil && !filter(obj) {
					continue
				}
			}
			candidates = append(candidates, n)
		}
	}
	return closestMatch(name, candidates)
}

// suggestTypeName finds the closest type name in the current scope chain.
func (c *Checker) suggestTypeName(name string) string {
	return c.suggestName(name, func(obj types.Object) bool {
		_, ok := obj.(*types.TypeName)
		return ok
	})
}

// collectModuleNames returns the names of all imported modules in scope.
func (c *Checker) collectModuleNames() []string {
	var names []string
	for scope := c.scope; scope != nil; scope = scope.Parent() {
		for _, n := range scope.Names() {
			if obj := scope.Lookup(n); obj != nil {
				if _, ok := obj.(*types.Module); ok {
					names = append(names, n)
				}
			}
		}
	}
	return names
}

// closestMatch returns the closest string from candidates within the edit
// distance threshold, or "" if no good match exists.
func closestMatch(name string, candidates []string) string {
	maxDist := levenshteinThreshold(len(name))
	best := ""
	bestDist := maxDist + 1
	for _, c := range candidates {
		if c == name {
			continue
		}
		// Quick length filter: if lengths differ by more than maxDist, skip
		diff := len(c) - len(name)
		if diff < 0 {
			diff = -diff
		}
		if diff > maxDist {
			continue
		}
		d := levenshtein(name, c)
		if d <= maxDist && d < bestDist {
			best = c
			bestDist = d
		}
	}
	return best
}

// levenshteinThreshold returns the max edit distance for a name of the given length.
func levenshteinThreshold(nameLen int) int {
	if nameLen <= 1 {
		return 0 // don't suggest for single-char names
	}
	if nameLen <= 3 {
		return 1
	}
	if nameLen <= 6 {
		return 2
	}
	return 3
}

// levenshtein computes the Levenshtein edit distance between two strings.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	if a == b {
		return 0
	}

	// Use single-row optimization
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr := make([]int, len(b)+1)
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			curr[j] = min3(ins, del, sub)
		}
		prev = curr
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
