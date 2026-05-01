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

// NormalizeURL canonicalizes a remote module URL for dedup and comparison.
// Strips scheme (https://, http://, git://, ssh://), trailing .git, trailing slashes,
// and lowercases the host portion. Path case is preserved.
func NormalizeURL(url string) string {
	s := url
	// Strip scheme
	for _, prefix := range []string{"https://", "http://", "git://", "ssh://"} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			s = s[len(prefix):]
			break
		}
	}
	// Strip trailing slashes first, then .git, then slashes again
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimRight(s, "/")
	// Lowercase the host portion (everything before the first /)
	if host, rest, ok := strings.Cut(s, "/"); ok {
		s = strings.ToLower(host) + "/" + rest
	} else {
		s = strings.ToLower(s)
	}
	return s
}

// SetRequire updates or adds a [require] entry in the promise.toml file at path.
// Preserves existing file content (comments, formatting) — only modifies the [require] section.
func SetRequire(path, url, commitHash string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	entry := fmt.Sprintf("%q = %q", url, commitHash)

	// Find [require] section and look for existing key
	requireStart := -1 // line index of [require] header
	requireEnd := -1   // line index of next section header (or EOF)
	existingLine := -1 // line index of existing entry for this URL

	normalizedURL := NormalizeURL(url)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "[require]" {
			requireStart = i
			continue
		}
		if requireStart >= 0 && requireEnd < 0 {
			// We're inside [require]
			if strings.HasPrefix(trimmed, "[") {
				requireEnd = i
				continue
			}
			// Check if this line is for the same URL
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				if key, _, err := parseTOMLLine(trimmed); err == nil {
					if NormalizeURL(key) == normalizedURL {
						existingLine = i
					}
				}
			}
		}
	}

	if existingLine >= 0 {
		// Replace existing entry
		lines[existingLine] = entry
	} else if requireStart >= 0 {
		// Append to existing [require] section
		insertAt := requireStart + 1
		if requireEnd >= 0 {
			insertAt = requireEnd
		} else {
			insertAt = len(lines)
		}
		// Find last non-empty line in [require] to insert after
		for j := insertAt - 1; j > requireStart; j-- {
			if strings.TrimSpace(lines[j]) != "" {
				insertAt = j + 1
				break
			}
		}
		lines = append(lines[:insertAt], append([]string{entry}, lines[insertAt:]...)...)
	} else {
		// No [require] section — add one
		// Find end of file, add section
		result := strings.TrimRight(string(data), "\n") + "\n\n[require]\n" + entry + "\n"
		return os.WriteFile(path, []byte(result), 0644)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
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
