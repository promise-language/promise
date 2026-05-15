package common

import "strings"

// NormalizeArgs canonicalizes CLI flag arguments so that both single-dash and
// double-dash prefixes resolve identically, and =/:  value separators are
// accepted alongside space separators.
//
// Transformations:
//   - "--flag" → "-flag"        (strip redundant dash)
//   - "-flag=value" → ["-flag", "value"]  (split on first =)
//   - "-flag:value" → ["-flag", "value"]  (split on first :)
//
// Bare "-" and "--" (end-of-flags sentinel) pass through unchanged.
// nil input returns nil (preserves nil checks in callers like RunBuild).
func NormalizeArgs(args []string) []string {
	if args == nil {
		return nil
	}
	var result []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			result = append(result, arg)
			continue
		}
		// Strip -- to -
		if strings.HasPrefix(arg, "--") {
			arg = "-" + arg[2:]
		}
		// Split on first = or : after the flag name
		if idx := strings.IndexAny(arg[1:], "=:"); idx >= 0 {
			result = append(result, arg[:idx+1], arg[idx+2:])
		} else {
			result = append(result, arg)
		}
	}
	return result
}
