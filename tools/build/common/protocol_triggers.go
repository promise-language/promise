package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProtocolTriggerEntry identifies a protocol method name reserved by an
// embedded catalog module.
type ProtocolTriggerEntry struct {
	Module string `json:"module"`
	// IsGetter is true when the abstract method is a getter (get name Type).
	IsGetter bool `json:"getter"`
	// IsSetter is true when the abstract method is a setter (set name(Type)).
	IsSetter bool `json:"setter"`
}

// ProtocolTriggerTable maps method name → list of trigger entries.
type ProtocolTriggerTable struct {
	Triggers map[string][]ProtocolTriggerEntry `json:"triggers"`
}

// GenerateProtocolTriggers scans embedded module sources for protocol-tagged
// interfaces and emits a compact name → owning module trigger table as JSON.
// std is excluded (always loaded). The table is consulted on every method
// declaration to decide whether an on-demand module load is needed for the
// protocol near-miss check (T1732).
func GenerateProtocolTriggers(root string) error {
	modulesDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "modules")
	if !Exists(modulesDir) {
		// No embedded modules — write empty table.
		return writeProtocolTriggers(root, &ProtocolTriggerTable{
			Triggers: map[string][]ProtocolTriggerEntry{},
		})
	}

	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return fmt.Errorf("read modules dir: %w", err)
	}

	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		modName := e.Name()
		// Skip std — always loaded via `use std as _`.
		if modName == "std" {
			continue
		}

		modDir := filepath.Join(modulesDir, modName)
		files, err := os.ReadDir(modDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".pr") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(modDir, f.Name()))
			if err != nil {
				continue
			}
			scanProtocolMethods(string(data), modName, table)
		}
	}

	return writeProtocolTriggers(root, table)
}

// scanProtocolMethods is a line-based scanner that finds protocol-tagged
// interfaces and their abstract methods in a .pr source file. It adds
// entries to the trigger table for each abstract method found.
//
// The scanner is reliable because:
// (a) the corpus is small and controlled (modules we ship),
// (b) the annotation format is rigid (`structural(protocol: true)),
// (c) abstract methods have a distinctive shape (`abstract at end of line).
func scanProtocolMethods(source, moduleName string, table *ProtocolTriggerTable) {
	lines := strings.Split(source, "\n")
	inProtocol := false
	braceDepth := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if !inProtocol {
			// Look for a type declaration with `structural(protocol: true).
			if isProtocolTypeDecl(trimmed) {
				inProtocol = true
				braceDepth = 0
				// Count braces on this line.
				braceDepth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
				if braceDepth <= 0 {
					// Single-line type (unlikely for protocols) — done.
					inProtocol = false
				}
			}
			continue
		}

		// Inside a protocol type body — track brace depth.
		braceDepth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")

		if braceDepth <= 0 {
			inProtocol = false
			continue
		}

		// Look for abstract method declarations.
		if !strings.Contains(trimmed, "`abstract") {
			continue
		}

		name, isGetter, isSetter := extractAbstractMethodName(trimmed)
		if name == "" {
			continue
		}

		entry := ProtocolTriggerEntry{
			Module:   moduleName,
			IsGetter: isGetter,
			IsSetter: isSetter,
		}
		table.Triggers[name] = appendUniqueTrigger(table.Triggers[name], entry)
	}
}

// isProtocolTypeDecl checks if a trimmed line declares a type with
// `structural(protocol: true).
func isProtocolTypeDecl(line string) bool {
	if !strings.HasPrefix(line, "type ") {
		return false
	}
	return strings.Contains(line, "`structural(protocol: true)")
}

// extractAbstractMethodName extracts the method name from a line that
// contains `abstract. Returns the name, isGetter, isSetter.
//
// Handles these forms:
//   - method!(this, ...) ...  → name = "method"
//   - method(this, ...) ...   → name = "method"
//   - get name Type ...       → name = "name", isGetter = true
//   - set name(Type) ...      → name = "name", isSetter = true
func extractAbstractMethodName(line string) (string, bool, bool) {
	// Strip leading whitespace.
	trimmed := strings.TrimSpace(line)

	// Check for getter: "get name Type `abstract"
	if strings.HasPrefix(trimmed, "get ") {
		rest := trimmed[4:]
		// Next token is the getter name.
		name := extractFirstWord(rest)
		if name != "" {
			return name, true, false
		}
		return "", false, false
	}

	// Check for setter: "set name(Type) `abstract"
	if strings.HasPrefix(trimmed, "set ") {
		rest := trimmed[4:]
		name := extractFirstWord(rest)
		if name != "" {
			return name, false, true
		}
		return "", false, false
	}

	// Regular method: "name!(this, ...) ..." or "name(this, ...) ..."
	// Extract name before '(' or '!'.
	idx := strings.IndexAny(trimmed, "(!")
	if idx <= 0 {
		return "", false, false
	}
	name := trimmed[:idx]
	// Validate: name should be a simple identifier (no spaces).
	if strings.ContainsAny(name, " \t") {
		return "", false, false
	}
	return name, false, false
}

// extractFirstWord returns the first whitespace/punctuation-delimited word.
func extractFirstWord(s string) string {
	s = strings.TrimSpace(s)
	for i, c := range s {
		if c == ' ' || c == '\t' || c == '(' || c == ')' || c == '!' {
			return s[:i]
		}
	}
	return s
}

// appendUniqueTrigger appends entry to slice if no identical entry exists.
func appendUniqueTrigger(slice []ProtocolTriggerEntry, entry ProtocolTriggerEntry) []ProtocolTriggerEntry {
	for _, e := range slice {
		if e.Module == entry.Module && e.IsGetter == entry.IsGetter && e.IsSetter == entry.IsSetter {
			return slice
		}
	}
	return append(slice, entry)
}

func writeProtocolTriggers(root string, table *ProtocolTriggerTable) error {
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal protocol triggers: %w", err)
	}
	data = append(data, '\n')
	dst := filepath.Join(root, "compiler", "cmd", "promise", "resources", "protocol_triggers.json")
	return os.WriteFile(dst, data, 0o644)
}
