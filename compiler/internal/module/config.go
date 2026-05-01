package module

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config represents the parsed contents of a promise.toml file.
type Config struct {
	Name    string            // module name
	Epoch   string            // catalog epoch, e.g. "2026.3"
	Require map[string]string // remote URL → commit hash
	Replace map[string]string // URL or catalog name → local path
	Dir     string            // directory containing promise.toml
}

// FindConfig walks up from dir until it finds a promise.toml file.
// Returns nil if no config file is found (single-file mode).
func FindConfig(dir string) (*Config, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		path := filepath.Join(dir, "promise.toml")
		if _, err := os.Stat(path); err == nil {
			cfg, err := ParseConfig(path)
			if err != nil {
				return nil, err
			}
			cfg.Dir = dir
			return cfg, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil // reached filesystem root
		}
		dir = parent
	}
}

// ParseConfig reads and parses a promise.toml file.
// Only supports the subset needed: [module], [require], [replace].
func ParseConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close()

	cfg := &Config{
		Require: make(map[string]string),
		Replace: make(map[string]string),
	}

	var section string
	scanner := bufio.NewScanner(f)
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
				return nil, fmt.Errorf("%s:%d: invalid section header: %s", path, lineNum, line)
			}
			section = line[1 : len(line)-1]
			continue
		}

		// Key = value
		key, val, err := parseTOMLLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNum, err)
		}

		switch section {
		case "module":
			switch key {
			case "name":
				cfg.Name = val
			case "epoch":
				cfg.Epoch = val
			default:
				// Forward compatibility: ignore unknown keys
			}
		case "require":
			cfg.Require[key] = val
		case "replace":
			cfg.Replace[key] = val
		default:
			// Forward compatibility: ignore unknown sections
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	if cfg.Name == "" {
		return nil, fmt.Errorf("%s: missing [module] name", path)
	}

	return cfg, nil
}

// parseTOMLLine parses a `key = "value"` or `key = value` line.
func parseTOMLLine(line string) (key, val string, err error) {
	idx := strings.Index(line, "=")
	if idx < 0 {
		return "", "", fmt.Errorf("expected key = value, got: %s", line)
	}
	key = strings.TrimSpace(line[:idx])
	val = strings.TrimSpace(line[idx+1:])

	// Strip quotes from key and value
	key = stripQuotes(key)
	val = stripQuotes(val)
	return key, val, nil
}

// stripQuotes removes surrounding double quotes from a string.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// IsCatalogImport returns true if the use declaration is a catalog import (no path).
func IsCatalogImport(path string) bool {
	return path == ""
}

// IsLocalPath returns true if the location string refers to a local module.
func IsLocalPath(location string) bool {
	if strings.HasPrefix(location, "./") || strings.HasPrefix(location, "../") {
		return true
	}
	if strings.HasPrefix(location, "/") {
		return true
	}
	// Windows drive letter: C:\, D:/, etc.
	if len(location) >= 2 && location[1] == ':' &&
		((location[0] >= 'A' && location[0] <= 'Z') || (location[0] >= 'a' && location[0] <= 'z')) {
		return true
	}
	return false
}
