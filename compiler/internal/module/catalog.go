package module

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// CatalogEntry describes a single module in the catalog.
type CatalogEntry struct {
	Name        string // module name (the TOML key, e.g., "json")
	URL         string // fetch-ready git URL (e.g., "https://github.com/promise-lang/json")
	Commit      string // pinned commit hash
	Description string // human-readable description
}

// Catalog is the parsed representation of the embedded catalog.toml.
type Catalog struct {
	Epoch   string                   // catalog epoch (e.g., "2026.3")
	Modules map[string]*CatalogEntry // name → entry
}

// Lookup returns the catalog entry for the given module name, or nil if not found.
func (c *Catalog) Lookup(name string) *CatalogEntry {
	if c == nil {
		return nil
	}
	return c.Modules[name]
}

// ParseCatalog parses a catalog.toml from bytes (typically from go:embed).
//
// Format:
//
//	[catalog]
//	epoch = "2026.3"
//
//	[modules.json]
//	url = "https://github.com/promise-lang/json"
//	commit = "a1b2c3d"
//	description = "JSON parsing and serialization"
func ParseCatalog(data []byte) (*Catalog, error) {
	cat := &Catalog{
		Modules: make(map[string]*CatalogEntry),
	}

	var section string // current section name (e.g., "catalog", "modules.json")
	var currentEntry *CatalogEntry
	var currentName string // module name extracted from "modules.NAME"

	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Section headers
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("catalog.toml:%d: invalid section header: %s", lineNum, line)
			}
			section = line[1 : len(line)-1]

			// Finish previous module entry
			currentEntry = nil
			currentName = ""

			// Check for [modules.NAME] pattern
			if strings.HasPrefix(section, "modules.") {
				currentName = section[len("modules."):]
				if currentName == "" {
					return nil, fmt.Errorf("catalog.toml:%d: empty module name in section [%s]", lineNum, section)
				}
				currentEntry = &CatalogEntry{Name: currentName}
				cat.Modules[currentName] = currentEntry
			}
			continue
		}

		// Key = value
		key, val, err := parseTOMLLine(line)
		if err != nil {
			return nil, fmt.Errorf("catalog.toml:%d: %w", lineNum, err)
		}

		switch {
		case section == "catalog":
			switch key {
			case "epoch":
				cat.Epoch = val
			}
		case currentEntry != nil:
			switch key {
			case "url":
				currentEntry.URL = val
			case "commit":
				currentEntry.Commit = val
			case "description":
				currentEntry.Description = val
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading catalog.toml: %w", err)
	}

	// Validate entries
	for name, entry := range cat.Modules {
		if entry.URL == "" {
			return nil, fmt.Errorf("catalog.toml: module '%s' is missing 'url'", name)
		}
		if entry.Commit == "" {
			return nil, fmt.Errorf("catalog.toml: module '%s' is missing 'commit'", name)
		}
	}

	return cat, nil
}
