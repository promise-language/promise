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
	Subdir string // repo-relative path to the module directory; empty means repo root
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
			case "subdir":
				namedReqEntry.Subdir = val
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

	// Validate flat require entries. Only the URL is checkable here — the value is
	// a commit hash and the key carries no subdir.
	for url := range cfg.Require {
		if err := CheckURLIdentitySafe(url); err != nil {
			return nil, fmt.Errorf("%s: [require] invalid url %q: %w", path, url, err)
		}
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
		if err := CheckURLIdentitySafe(entry.URL); err != nil {
			return nil, fmt.Errorf("%s: [require.%s] invalid 'url': %w", path, name, err)
		}
		sub, serr := NormalizeSubdir(entry.Subdir)
		if serr != nil {
			return nil, fmt.Errorf("%s: [require.%s] invalid 'subdir': %w", path, name, serr)
		}
		entry.Subdir = sub
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
	return setNamedRequireFields(path, name, [][2]string{{"commit", commitHash}}, false)
}

// SetNamedRequire creates or updates the [require.NAME] section of the
// promise.toml file at path, writing url, commit and subdir. An empty subdir
// writes no `subdir` line (and removes an existing one), so an entry re-added
// without a subdir addresses the repo root again.
func SetNamedRequire(path, name, url, commitHash, subdir string) error {
	return setNamedRequireFields(path, name, [][2]string{
		{"url", url}, {"commit", commitHash}, {"subdir", subdir},
	}, true)
}

// findNamedRequireSection locates the [require.NAME] section in lines. It returns
// the index of the header line and the index one past the section body — the next
// section header, or len(lines) when the section runs to EOF. start is -1 when
// there is no such section.
func findNamedRequireSection(lines []string, name string) (start, end int) {
	header := fmt.Sprintf("[require.%s]", name)
	start, end = -1, len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start < 0 {
			if trimmed == header {
				start = i
			}
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			return start, i
		}
	}
	return start, end
}

// RemoveNamedRequire deletes the whole [require.NAME] section — header, keys, and
// the blank lines that separated it from the next section — from the promise.toml
// at path. Reports whether a section was found; a missing section is a no-op, not
// an error, so removal is idempotent.
func RemoveNamedRequire(path, name string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("cannot read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	start, end := findNamedRequireSection(lines, name)
	if start < 0 {
		return false, nil
	}
	// end already sits on the next section header, so the blank line that
	// separated the two is inside [start, end) and goes with the section — the
	// blank line *above* the header stays and keeps separating what remains.
	out := append([]string{}, lines[:start]...)
	out = append(out, lines[end:]...)
	return true, os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644)
}

// setNamedRequireFields writes key/value pairs into the [require.NAME] section of
// the promise.toml at path, preserving all surrounding content (comments,
// formatting, other sections). A key already present is rewritten in place (the
// last assignment, matching ParseConfig's last-wins reading, with any earlier
// duplicates dropped); a missing key is appended to the end of the section; a pair
// with an empty value removes every assignment of that key rather than writing it.
// When create is true a missing section is appended to the file, otherwise a
// missing section is an error.
func setNamedRequireFields(path, name string, fields [][2]string, create bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	header := fmt.Sprintf("[require.%s]", name)
	start, end := findNamedRequireSection(lines, name)

	if start < 0 {
		if !create {
			return fmt.Errorf("%s: section [require.%s] not found", path, name)
		}
		var b strings.Builder
		b.WriteString(strings.TrimRight(string(data), "\n"))
		b.WriteString("\n\n")
		b.WriteString(header)
		b.WriteByte('\n')
		for _, f := range fields {
			if f[1] == "" {
				continue
			}
			fmt.Fprintf(&b, "%s = %q\n", f[0], f[1])
		}
		return os.WriteFile(path, []byte(b.String()), 0644)
	}

	section := append([]string{}, lines[start+1:end]...)
	for _, f := range fields {
		key, val := f[0], f[1]
		// Collect *every* line assigning this key. A hand-edited manifest can carry
		// a duplicate, and ParseConfig is last-wins — so rewriting only the first
		// occurrence would leave the read and the write disagreeing (an update that
		// reports success while the effective value never changed). Rewrite the last
		// occurrence and drop the earlier ones.
		var idxs []int
		for i, l := range section {
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if k, _, perr := parseTOMLLine(trimmed); perr == nil && k == key {
				idxs = append(idxs, i)
			}
		}
		// Deleting from the end keeps the not-yet-visited indices valid.
		dropFrom := len(idxs) - 1 // remove every occurrence
		if val != "" && len(idxs) > 0 {
			section[idxs[len(idxs)-1]] = fmt.Sprintf("%s = %q", key, val)
			dropFrom = len(idxs) - 2 // keep the rewritten one
		}
		for i := dropFrom; i >= 0; i-- {
			section = append(section[:idxs[i]], section[idxs[i]+1:]...)
		}
		if val == "" || len(idxs) > 0 {
			continue // removed, or rewritten in place
		}
		// New key: append after the last non-empty line so a trailing blank line
		// before the next section stays where it is.
		insertAt := 0
		for j := len(section) - 1; j >= 0; j-- {
			if strings.TrimSpace(section[j]) != "" {
				insertAt = j + 1
				break
			}
		}
		line := fmt.Sprintf("%s = %q", key, val)
		section = append(section[:insertAt], append([]string{line}, section[insertAt:]...)...)
	}

	out := append([]string{}, lines[:start+1]...)
	out = append(out, section...)
	out = append(out, lines[end:]...)
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644)
}

// IsLocalPath returns true if the location string refers to a local module.
func IsLocalPath(location string) bool {
	if strings.HasPrefix(location, "./") || strings.HasPrefix(location, "../") {
		return true
	}
	if strings.HasPrefix(location, "/") {
		return true
	}
	if hasWindowsDrive(location) {
		return true
	}
	return false
}

// hasWindowsDrive reports whether s starts with a Windows drive letter (C:\, d:/).
func hasWindowsDrive(s string) bool {
	return len(s) >= 2 && s[1] == ':' &&
		((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z'))
}

// IsImportName reports whether s can be used as the NAME of a [require.NAME]
// section — the identifier that a `use NAME;` declaration names.
func IsImportName(s string) bool {
	return isSimpleIdent(s)
}

// CheckURLIdentitySafe rejects a module URL that could not be told apart from a
// subdir-addressed module. GlobalIdentityForRemote joins the normalized URL and
// the subdir with "//", so a URL that itself normalizes to something containing
// "//" would let two distinct (url, subdir) pairs collapse to one identity — and
// therefore share a commit pin, a resolution slot and an IR prefix. Such a URL has
// an empty path component and is malformed anyway, so it is refused at parse time.
func CheckURLIdentitySafe(url string) error {
	if strings.Contains(NormalizeURL(url), "//") {
		return fmt.Errorf("contains an empty path component ('//'); a module in a repo subdirectory is addressed with the 'subdir' field of a [require.NAME] entry")
	}
	return nil
}

// NormalizeSubdir validates and canonicalizes a repo-relative subdirectory path —
// the `subdir` field on a [require.NAME] entry or a catalog entry, naming the
// directory inside the checkout that holds the module's promise.toml.
//
// "" means the repo root (the behavior of every manifest written before subdir
// existed) and is returned unchanged. Otherwise backslashes become '/', one
// leading "./" and one trailing "/" are stripped, and the result must be a
// sequence of non-empty components that are neither "." nor "..". Absolute paths
// (leading "/" or a Windows drive) and any escape above the repo root are
// rejected: a subdir must address a directory *inside* the checkout.
func NormalizeSubdir(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	orig := s
	s = strings.ReplaceAll(s, "\\", "/")
	if strings.HasPrefix(s, "/") || hasWindowsDrive(s) {
		return "", fmt.Errorf("must be repo-relative, got absolute path %q", orig)
	}
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return "", fmt.Errorf("empty path %q", orig)
	}
	parts := strings.Split(s, "/")
	for _, p := range parts {
		switch p {
		case "":
			return "", fmt.Errorf("empty path component in %q", orig)
		case ".", "..":
			return "", fmt.Errorf("path component %q is not allowed in %q", p, orig)
		}
	}
	return strings.Join(parts, "/"), nil
}
