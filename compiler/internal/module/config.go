package module

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RequireEntry describes a named dependency in [require.NAME] sections.
type RequireEntry struct {
	URL    string // git URL or archive URL
	Commit string // pinned commit hash (git only)
	SHA256 string // optional content hash (non-git sources)
}

// sha256Hex matches exactly 64 lowercase hex characters.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Config represents the parsed contents of a promise.toml file.
type Config struct {
	Name         string                   // module name
	Epoch        string                   // catalog epoch, e.g. "2026.0"
	Require      map[string]string        // remote URL → commit hash
	NamedRequire map[string]*RequireEntry // local import name → {url, commit}
	Replace      map[string]string        // URL or catalog name → local path
	Dir          string                   // directory containing promise.toml
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
		Require:      make(map[string]string),
		NamedRequire: make(map[string]*RequireEntry),
		Replace:      make(map[string]string),
	}

	var section string
	var namedReqName string // current [require.NAME] entry name
	var namedReqEntry *RequireEntry
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
			namedReqName = ""
			namedReqEntry = nil

			// Check for [require.NAME] pattern
			if strings.HasPrefix(section, "require.") {
				namedReqName = section[len("require."):]
				if namedReqName == "" {
					return nil, fmt.Errorf("%s:%d: empty name in section [%s]", path, lineNum, section)
				}
				namedReqEntry = &RequireEntry{}
				cfg.NamedRequire[namedReqName] = namedReqEntry
			}
			continue
		}

		// Key = value
		key, val, err := parseTOMLLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNum, err)
		}

		switch {
		case namedReqEntry != nil:
			switch key {
			case "url":
				namedReqEntry.URL = val
			case "commit":
				namedReqEntry.Commit = val
			case "sha256":
				namedReqEntry.SHA256 = val
			}
		case section == "module":
			switch key {
			case "name":
				cfg.Name = val
			case "epoch":
				// A project pins a numeric epoch ("YYYY.N"). "next" is a
				// toolchain release channel (§4.3), not a project epoch — it
				// selects which compiler you run, never participates in
				// `epoch-X ≤ E` module resolution (§9.8). Reject it here so the
				// gate fires at parse time with a clear message.
				if val == ChannelNext {
					return nil, fmt.Errorf("%s:%d: [module] epoch = \"next\" is not allowed — projects pin a numeric epoch (e.g. \"2026.1\"); \"next\" is a toolchain channel, not a project epoch", path, lineNum)
				}
				cfg.Epoch = val
			default:
				// Forward compatibility: ignore unknown keys
			}
		case section == "require":
			cfg.Require[key] = val
		case section == "replace":
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

	// Validate named require entries.
	for name, entry := range cfg.NamedRequire {
		if entry.URL == "" && entry.Commit == "" && entry.SHA256 == "" {
			return nil, fmt.Errorf("%s: [require.%s] missing 'url' and 'commit'", path, name)
		}
		if entry.URL == "" {
			return nil, fmt.Errorf("%s: [require.%s] missing 'url'", path, name)
		}
		// sha256 and commit are mutually exclusive — git sources use commit SHA
		// for integrity; sha256 is for non-git sources (tarballs, archives).
		if entry.Commit != "" && entry.SHA256 != "" {
			return nil, fmt.Errorf("%s: [require.%s] cannot have both 'commit' and 'sha256' — use 'commit' for git sources, 'sha256' for non-git sources", path, name)
		}
		if entry.Commit == "" && entry.SHA256 == "" {
			return nil, fmt.Errorf("%s: [require.%s] has 'url' but missing 'commit' (or 'sha256' for non-git sources)", path, name)
		}
		// Validate sha256 hex format.
		if entry.SHA256 != "" && !sha256Hex.MatchString(entry.SHA256) {
			return nil, fmt.Errorf("%s: [require.%s] invalid 'sha256': must be exactly 64 lowercase hex characters", path, name)
		}
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

// FindProjectMain looks for a promise.toml in dir and returns the value of the
// "main" field under [module], if present. Returns "" if no promise.toml exists
// or if it has no "main" field. Unlike ParseConfig, does not require [module] name.
func FindProjectMain(dir string) (string, error) {
	path := filepath.Join(dir, "promise.toml")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	var section string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if section == "module" {
			if key, val, err := parseTOMLLine(line); err == nil && key == "main" {
				return val, nil
			}
		}
	}
	return "", scanner.Err()
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

// RemoveRequire removes a [require] entry from the promise.toml file at path.
// No-op (returns nil) if the entry is absent.
func RemoveRequire(path, url string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	normalizedURL := NormalizeURL(url)

	requireStart := -1
	targetLine := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "[require]" {
			requireStart = i
			continue
		}
		if requireStart >= 0 {
			if strings.HasPrefix(trimmed, "[") {
				break
			}
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				if key, _, err := parseTOMLLine(trimmed); err == nil {
					if NormalizeURL(key) == normalizedURL {
						targetLine = i
					}
				}
			}
		}
	}

	if targetLine < 0 {
		return nil // not found, no-op
	}

	// Remove the line
	lines = append(lines[:targetLine], lines[targetLine+1:]...)

	// If [require] section is now empty, remove trailing blank line inside it
	// (locate end again after removal)
	sectionEnd := len(lines)
	for i := requireStart + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			sectionEnd = i
			break
		}
	}
	allBlank := true
	for i := requireStart + 1; i < sectionEnd; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			allBlank = false
			break
		}
	}
	if allBlank && sectionEnd > requireStart+1 {
		// Remove the blank lines between [require] header and next section
		lines = append(lines[:requireStart+1], lines[sectionEnd:]...)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// SetNamedRequireCommit updates the commit field in a [require.NAME] section
// of the promise.toml file at path. Returns an error if the section doesn't exist.
func SetNamedRequireCommit(path, name, commitHash string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	sectionHeader := fmt.Sprintf("[require.%s]", name)

	// Find the [require.NAME] section
	sectionStart := -1
	sectionEnd := -1
	commitLine := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == sectionHeader {
			sectionStart = i
			continue
		}
		if sectionStart >= 0 && sectionEnd < 0 {
			if strings.HasPrefix(trimmed, "[") {
				sectionEnd = i
				break
			}
			if key, _, err := parseTOMLLine(trimmed); err == nil && key == "commit" {
				commitLine = i
			}
		}
	}

	if sectionStart < 0 {
		return fmt.Errorf("%s: section [require.%s] not found", path, name)
	}

	if commitLine >= 0 {
		lines[commitLine] = fmt.Sprintf("commit = %q", commitHash)
	} else {
		// No commit line found — append after last key in section
		insertAt := len(lines)
		if sectionEnd >= 0 {
			insertAt = sectionEnd
		}
		for j := insertAt - 1; j > sectionStart; j-- {
			if strings.TrimSpace(lines[j]) != "" {
				insertAt = j + 1
				break
			}
		}
		newLine := fmt.Sprintf("commit = %q", commitHash)
		lines = append(lines[:insertAt], append([]string{newLine}, lines[insertAt:]...)...)
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
