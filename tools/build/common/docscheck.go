package common

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// CheckDocs runs the mechanical documentation checks: every relative Markdown
// link resolves to a file that exists, every top-level doc is reachable from
// docs/index.md, and the catalog stays in sync with what is actually on disk
// and what the two module-inventory docs claim.
//
// These are deliberately narrow. They verify link *targets*, index reachability,
// and module-name *presence* — nothing about whether the surrounding prose is
// accurate. Doc
// staleness of the kind T1675 swept up (wrong API names, "planned" modules that
// shipped) is not detectable this way and still needs human review.
//
// Coverage limitation worth knowing before extending this: README.md is
// intentionally excluded from the catalog-coverage check. Its module list lives
// inside an ASCII box-drawing repo tree using bare directory names, so a
// substring matcher robust enough to read it would produce false positives.
// README stays human-reviewed.
func CheckDocs(root string) error {
	var problems []string
	if err := checkDocLinks(root); err != nil {
		problems = append(problems, err.Error())
	}
	if err := checkDocIndex(root); err != nil {
		problems = append(problems, err.Error())
	}
	if err := checkCatalogCoverage(root); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}

// markdownLink matches the target of an inline Markdown link: the `foo.md#bar`
// in `[text](foo.md#bar)`. Targets containing whitespace or a closing paren are
// not matched, which keeps reference-style and image syntax out of scope.
var markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// checkDocLinks reports every relative link to a `.md` file that does not exist
// on disk.
//
// The file set comes from `git ls-files`, not a filesystem walk: that skips
// build output (.promise-home/, dist/) and, importantly, the generated-but-
// untracked compiler/cmd/promise/resources/ tree — with no ignore list to keep
// in sync.
//
// Only the link target is validated. Anchors are stripped and never checked,
// which is what keeps the false-positive rate at zero.
func checkDocLinks(root string) error {
	out, err := RunOutputIn(root, "git", "ls-files", "-z", "*.md")
	if err != nil {
		return fmt.Errorf("list tracked markdown files: %w", err)
	}

	var dangling []string
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// Tracked but absent from the worktree (e.g. a staged
			// deletion). Nothing to scan.
			continue
		}
		dir := filepath.Dir(rel)
		for _, m := range markdownLink.FindAllStringSubmatch(string(data), -1) {
			target := m[1]
			if !isRelativeMarkdownLink(target) {
				continue
			}
			if i := strings.IndexByte(target, '#'); i >= 0 {
				target = target[:i]
			}
			// Defensive: unreachable today, since a target that is
			// empty once its anchor is stripped must have begun with
			// '#', which isRelativeMarkdownLink already rejects.
			if target == "" {
				continue
			}
			resolved := filepath.Join(root, dir, filepath.FromSlash(target))
			if _, err := os.Stat(resolved); err != nil {
				dangling = append(dangling, fmt.Sprintf("  %s -> %s", rel, m[1]))
			}
		}
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		return fmt.Errorf("dangling markdown links (target does not exist):\n%s", strings.Join(dangling, "\n"))
	}
	return nil
}

// isRelativeMarkdownLink reports whether target is a relative link to a .md
// file — the only kind this check can resolve. Absolute URLs, mailto:, and
// pure in-page anchors are out of scope.
func isRelativeMarkdownLink(target string) bool {
	lower := strings.ToLower(target)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return false
	case strings.HasPrefix(lower, "mailto:"), strings.HasPrefix(lower, "#"):
		return false
	case strings.HasPrefix(target, "/"):
		// Site-absolute; there is no doc root to resolve against.
		return false
	}
	base := lower
	if i := strings.IndexByte(base, '#'); i >= 0 {
		base = base[:i]
	}
	return strings.HasSuffix(base, ".md")
}

// docIndex is the table of contents every top-level doc must be reachable from.
const docIndex = "docs/index.md"

// checkDocIndex reports every tracked doc under docs/ that docs/index.md does
// not link to. The index is the entry point a reader (or an agent) is pointed
// at, so a doc missing from it is effectively unpublished — the second half of
// the T1675 drift, alongside the dangling link checkDocLinks catches.
//
// Scope is all of docs/, subdirectories included. Which folder a doc sits in is
// what says whether it binds — docs/ root is normative, docs/proposals/ is not
// yet ratified, docs/archive/ is superseded, docs/research/ is background — and
// the index is where that distinction is written down. A doc that is not indexed
// therefore has no stated status at all, which is worse for an archived or
// proposed doc than for a root one, not better. Nested module plan.md files stay
// out of scope: they belong to their module, not to the index — the `docs/*.md`
// pathspec matches nested paths recursively, so it already covers them and they
// are excluded by living outside docs/ rather than by a filter here.
func checkDocIndex(root string) error {
	out, err := RunOutputIn(root, "git", "ls-files", "-z", "docs/*.md")
	if err != nil {
		return fmt.Errorf("list tracked docs: %w", err)
	}
	index, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(docIndex)))
	if err != nil {
		if os.IsNotExist(err) {
			// Not a Promise checkout (or the index was deliberately
			// removed) — nothing to reconcile against.
			return nil
		}
		return fmt.Errorf("read %s: %w", docIndex, err)
	}

	// Resolve each link relative to docs/ and record the repo-relative path,
	// so a link to ../CONTRIBUTING.md cannot stand in for docs/CONTRIBUTING.md.
	linked := map[string]bool{}
	for _, m := range markdownLink.FindAllStringSubmatch(string(index), -1) {
		target := m[1]
		if !isRelativeMarkdownLink(target) {
			continue
		}
		if i := strings.IndexByte(target, '#'); i >= 0 {
			target = target[:i]
		}
		linked[path.Join(path.Dir(docIndex), target)] = true
	}

	var missing []string
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" || rel == docIndex {
			continue
		}
		if !linked[rel] {
			missing = append(missing, "  "+rel)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("docs not linked from %s:\n%s", docIndex, strings.Join(missing, "\n"))
	}
	return nil
}

// catalogCoverageDocs are the docs required to mention every shipped module.
// Both use full `modules/<name>/…` paths in their inventory tables, so a
// literal substring match on `modules/<name>/` is exact — no Markdown table
// parsing, and no false hit from the bare word "io" or "time".
var catalogCoverageDocs = []string{"CLAUDE.md", "docs/standard-library.md"}

// checkCatalogCoverage asserts two directions of catalog/disk/doc agreement:
//
//  1. Every directory under modules/ has a [modules.<name>] entry in
//     catalog.toml. (The reverse does not hold: entries carrying a `url` key
//     are remote modules with no local directory.)
//  2. Every catalog entry whose local directory actually ships source is
//     mentioned by the literal string `modules/<name>/` in each of
//     catalogCoverageDocs — so a new module cannot land without appearing in
//     the two inventories agents read.
//
// A tree with no catalog.toml is not a Promise checkout (RunPreCommit is also
// exercised against bare temp repos), so the check scopes itself out entirely
// rather than erroring.
func checkCatalogCoverage(root string) error {
	catalogPath := filepath.Join(root, "catalog.toml")
	if _, err := os.Stat(catalogPath); os.IsNotExist(err) {
		return nil
	}
	entries, err := parseCatalogModules(catalogPath)
	if err != nil {
		return err
	}

	// A tree with no modules/ directory at all has nothing to reconcile;
	// that is not a finding. A modules/ that exists but is not a directory
	// is one — see readDirIfExists.
	dirs, _, err := readDirIfExists(filepath.Join(root, "modules"))
	if err != nil {
		return fmt.Errorf("read modules/: %w", err)
	}

	var problems []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if _, ok := entries[d.Name()]; !ok {
			problems = append(problems, fmt.Sprintf(
				"  modules/%s/ has no [modules.%s] entry in catalog.toml", d.Name(), d.Name()))
		}
	}

	// A missing inventory doc is itself a finding: it would otherwise
	// silently disable half the check.
	docs := map[string]string{}
	for _, rel := range catalogCoverageDocs {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("  cannot read module inventory %s: %v", rel, err))
			continue
		}
		docs[rel] = string(data)
	}

	var names []string
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if entries[name].remote {
			continue
		}
		shipped, err := moduleShipsSource(filepath.Join(root, "modules", name))
		if err != nil {
			return err
		}
		if !shipped {
			continue
		}
		needle := "modules/" + name + "/"
		for _, rel := range catalogCoverageDocs {
			// An unreadable doc was already reported once; don't
			// repeat it per module.
			content, ok := docs[rel]
			if !ok {
				continue
			}
			if !strings.Contains(content, needle) {
				problems = append(problems, fmt.Sprintf(
					"  module %q ships source but %s never mentions %q", name, rel, needle))
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("catalog coverage:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

// catalogEntry is what the coverage check needs to know about a catalog module.
type catalogEntry struct {
	// remote is true when the entry has a `url` key — it is fetched from a
	// git remote and has no directory under modules/.
	remote bool
}

// parseCatalogModules extracts the [modules.<name>] sections of catalog.toml.
// This is a deliberately minimal scan rather than a TOML parse: the file is
// hand-maintained and flat, and the check only needs section names plus the
// presence of a `url` key.
func parseCatalogModules(path string) (map[string]catalogEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog.toml: %w", err)
	}
	entries := map[string]catalogEntry{}
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = ""
			if name, ok := strings.CutPrefix(strings.TrimSuffix(line, "]"), "[modules."); ok {
				current = name
				entries[name] = catalogEntry{}
			}
			continue
		}
		if current == "" {
			continue
		}
		if key, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(key) == "url" {
			entries[current] = catalogEntry{remote: true}
		}
	}
	return entries, nil
}

// moduleShipsSource reports whether a module directory contains real Promise
// source — at least one non-test .pr file with a line that is neither blank nor
// a comment. Planned modules keep a comment-only stub .pr as a design
// placeholder; those are not expected in the inventory docs yet.
// readDirIfExists reads dir, reporting separately whether it exists at all.
//
// The separate flag is the point: os.IsNotExist cannot distinguish "absent"
// from "present but not a directory" on Windows, where os.ReadDir of a regular
// file fails with ERROR_PATH_NOT_FOUND — which satisfies both os.IsNotExist and
// errors.Is(err, syscall.ENOTDIR). Callers that scope themselves out on an
// absent path would therefore silently scope themselves out on a *malformed*
// one too, disabling the very check they exist to run. An explicit Lstat tells
// the two apart on every platform.
func readDirIfExists(dir string) ([]os.DirEntry, bool, error) {
	entries, err := os.ReadDir(dir)
	if err == nil {
		return entries, true, nil
	}
	if _, statErr := os.Lstat(dir); statErr != nil && os.IsNotExist(statErr) {
		return nil, false, nil
	}
	return nil, true, err
}

func moduleShipsSource(dir string) (bool, error) {
	files, exists, err := readDirIfExists(dir)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", filepath.ToSlash(dir), err)
	}
	if !exists {
		return false, nil
	}
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".pr") || strings.HasSuffix(name, "_test.pr") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return false, fmt.Errorf("read %s: %w", filepath.Join(dir, name), err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "//") {
				return true, nil
			}
		}
	}
	return false, nil
}
