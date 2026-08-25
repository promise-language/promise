package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// docsRepo creates a temp git repo, writes the given files (paths are
// slash-separated and relative to the repo root), and stages them all so
// `git ls-files` sees them.
func docsRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=1+test@users.noreply.github.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=1+test@users.noreply.github.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")

	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	return root
}

// --- checkDocLinks ---

func TestDocLinksValidRelativeLinkPasses(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"docs/index.md": "See [tags](tags.md) and [design](../DESIGN.md).\n",
		"docs/tags.md":  "# Tags\n",
		"DESIGN.md":     "# Design\n",
	})
	if err := checkDocLinks(root); err != nil {
		t.Fatalf("expected no findings, got: %v", err)
	}
}

func TestDocLinksDanglingLinkFails(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"docs/index.md": "See [tags](tracker-tags.md).\n",
		"docs/tags.md":  "# Tags\n",
	})
	err := checkDocLinks(root)
	if err == nil {
		t.Fatal("expected a dangling-link finding, got nil")
	}
	if !strings.Contains(err.Error(), "docs/index.md") || !strings.Contains(err.Error(), "tracker-tags.md") {
		t.Fatalf("finding should name both the file and the target, got: %v", err)
	}
}

func TestDocLinksSkipsAbsoluteAndMailtoAndAnchors(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"README.md": strings.Join([]string{
			"[a](https://example.com/nope.md)",
			"[b](http://example.com/nope.md)",
			"[c](mailto:someone@example.com)",
			"[d](#in-page-anchor)",
			"[e](/site/absolute.md)",
			"[f](not-markdown.txt)",
		}, "\n") + "\n",
	})
	if err := checkDocLinks(root); err != nil {
		t.Fatalf("expected no findings, got: %v", err)
	}
}

func TestDocLinksStripsAnchorBeforeExistenceCheck(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"a.md": "[x](b.md#some-heading)\n",
		"b.md": "# B\n",
	})
	if err := checkDocLinks(root); err != nil {
		t.Fatalf("anchor should be stripped, not validated; got: %v", err)
	}
}

func TestDocLinksAnchorOnMissingFileStillFails(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"a.md": "[x](b.md#some-heading)\n",
	})
	err := checkDocLinks(root)
	if err == nil {
		t.Fatal("expected a finding for a missing target carrying an anchor")
	}
	if !strings.Contains(err.Error(), "b.md#some-heading") {
		t.Fatalf("finding should quote the link as written, got: %v", err)
	}
}

func TestDocLinksIgnoresUntrackedFiles(t *testing.T) {
	// Mirrors why the check uses `git ls-files`: the generated
	// compiler/cmd/promise/resources/ tree is untracked and must not be
	// scanned, with no hand-maintained ignore list.
	root := docsRepo(t, map[string]string{"a.md": "# A\n"})
	generated := filepath.Join(root, "generated", "copy.md")
	if err := os.MkdirAll(filepath.Dir(generated), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generated, []byte("[x](does-not-exist.md)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkDocLinks(root); err != nil {
		t.Fatalf("untracked file must not be scanned, got: %v", err)
	}
}

// --- checkDocIndex ---

func TestDocIndexAllTopLevelDocsLinkedPasses(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"docs/index.md":           "- [tags](tags.md)\n- [design](language-design.md#types)\n- [contrib](../CONTRIBUTING.md)\n",
		"docs/tags.md":            "# Tags\n",
		"docs/language-design.md": "# Design\n",
		"CONTRIBUTING.md":         "# Contributing\n",
	})
	if err := checkDocIndex(root); err != nil {
		t.Fatalf("expected no findings, got: %v", err)
	}
}

func TestDocIndexUnlinkedDocFails(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"docs/index.md":      "- [tags](tags.md)\n",
		"docs/tags.md":       "# Tags\n",
		"docs/code-style.md": "# Style\n",
	})
	err := checkDocIndex(root)
	if err == nil {
		t.Fatal("expected a finding for a doc missing from the index")
	}
	if !strings.Contains(err.Error(), "docs/code-style.md") {
		t.Fatalf("finding should name the unlinked doc, got: %v", err)
	}
	if strings.Contains(err.Error(), "docs/tags.md") {
		t.Fatalf("a linked doc must not be flagged: %v", err)
	}
}

func TestDocIndexCoversSubdirectories(t *testing.T) {
	// Which folder a doc sits in is what says whether it binds — root is
	// normative, proposals/ is unratified, archive/ is superseded — and the
	// index is where that is written down. So a doc in a subdirectory that
	// nothing links to has no stated status at all, and must be flagged.
	root := docsRepo(t, map[string]string{
		"docs/index.md":            "- [tags](tags.md)\n",
		"docs/tags.md":             "# Tags\n",
		"docs/archive/stages.md":   "# Stages\n",
		"docs/proposals/ui.md":     "# UI\n",
		"docs/research/refined.md": "# Refinement types\n",
	})
	err := checkDocIndex(root)
	if err == nil {
		t.Fatal("unindexed docs in subdirectories must be flagged")
	}
	for _, want := range []string{"docs/archive/stages.md", "docs/proposals/ui.md", "docs/research/refined.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %s to be flagged, got: %v", want, err)
		}
	}
}

func TestDocIndexAcceptsLinkedSubdirectoryDocs(t *testing.T) {
	// The counterpart: once the index links them, subdirectory docs pass.
	root := docsRepo(t, map[string]string{
		"docs/index.md":          "- [tags](tags.md)\n- [stages](archive/stages.md)\n- [ui](proposals/ui.md)\n",
		"docs/tags.md":           "# Tags\n",
		"docs/archive/stages.md": "# Stages\n",
		"docs/proposals/ui.md":   "# UI\n",
	})
	if err := checkDocIndex(root); err != nil {
		t.Fatalf("linked subdirectory docs must pass, got: %v", err)
	}
}

func TestDocIndexBasenameMatchDoesNotSatisfy(t *testing.T) {
	// A link to ../CONTRIBUTING.md must not stand in for docs/CONTRIBUTING.md:
	// targets are resolved relative to docs/, not matched by basename.
	root := docsRepo(t, map[string]string{
		"docs/index.md":        "- [contrib](../CONTRIBUTING.md)\n",
		"docs/CONTRIBUTING.md": "# Doc copy\n",
		"CONTRIBUTING.md":      "# Root\n",
	})
	err := checkDocIndex(root)
	if err == nil {
		t.Fatal("expected a finding: the index links the root file, not the docs/ one")
	}
	if !strings.Contains(err.Error(), "docs/CONTRIBUTING.md") {
		t.Fatalf("finding should name the unlinked doc, got: %v", err)
	}
}

func TestDocIndexMissingIndexScopesItselfOut(t *testing.T) {
	// RunPreCommit also runs against bare temp repos with no docs/index.md.
	root := docsRepo(t, map[string]string{"a.md": "# A\n"})
	if err := checkDocIndex(root); err != nil {
		t.Fatalf("a tree with no docs/index.md must be a no-op, got: %v", err)
	}
}

// --- checkCatalogCoverage ---

// coverageRepo builds a repo with a catalog, the given module files, and the
// two inventory docs.
func coverageRepo(t *testing.T, catalog string, moduleFiles map[string]string, claudeMD, stdlibMD string) string {
	t.Helper()
	files := map[string]string{
		"catalog.toml":             catalog,
		"CLAUDE.md":                claudeMD,
		"docs/standard-library.md": stdlibMD,
	}
	for k, v := range moduleFiles {
		files[k] = v
	}
	return docsRepo(t, files)
}

func TestCatalogCoverageHappyPath(t *testing.T) {
	root := coverageRepo(t,
		"[modules.io]\ndescription = \"io\"\n",
		map[string]string{"modules/io/io.pr": "print_line(\"hi\");\n"},
		"see modules/io/io.pr\n",
		"see modules/io/io.pr\n")
	if err := checkCatalogCoverage(root); err != nil {
		t.Fatalf("expected no findings, got: %v", err)
	}
}

func TestCatalogCoverageModuleDirWithoutEntryFails(t *testing.T) {
	root := coverageRepo(t,
		"[modules.io]\ndescription = \"io\"\n",
		map[string]string{
			"modules/io/io.pr":    "print_line(\"hi\");\n",
			"modules/orphan/x.pr": "print_line(\"hi\");\n",
		},
		"see modules/io/io.pr\n",
		"see modules/io/io.pr\n")
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected a finding for a module dir with no catalog entry")
	}
	if !strings.Contains(err.Error(), "[modules.orphan]") {
		t.Fatalf("finding should name the missing catalog entry, got: %v", err)
	}
}

func TestCatalogCoverageRemoteEntryNeedsNoDirectory(t *testing.T) {
	root := coverageRepo(t,
		"[modules.wasi_preview_1]\nurl = \"https://example.com/w.git\"\ndescription = \"wasi\"\n",
		nil, "no modules here\n", "no modules here\n")
	if err := checkCatalogCoverage(root); err != nil {
		t.Fatalf("a url= entry has no local dir and must be exempt, got: %v", err)
	}
}

func TestCatalogCoverageMissingFromClaudeMDFails(t *testing.T) {
	root := coverageRepo(t,
		"[modules.gzip]\ndescription = \"gzip\"\n",
		map[string]string{"modules/gzip/gzip.pr": "deflate(data);\n"},
		"nothing here\n",
		"see modules/gzip/\n")
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected a finding for a shipped module absent from CLAUDE.md")
	}
	if !strings.Contains(err.Error(), "CLAUDE.md") || !strings.Contains(err.Error(), "modules/gzip/") {
		t.Fatalf("finding should name the doc and the module path, got: %v", err)
	}
	if strings.Contains(err.Error(), "docs/standard-library.md") {
		t.Fatalf("standard-library.md does mention the module; it must not be flagged: %v", err)
	}
}

func TestCatalogCoverageMissingFromStandardLibraryFails(t *testing.T) {
	root := coverageRepo(t,
		"[modules.gzip]\ndescription = \"gzip\"\n",
		map[string]string{"modules/gzip/gzip.pr": "deflate(data);\n"},
		"see modules/gzip/\n",
		"nothing here\n")
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected a finding for a shipped module absent from docs/standard-library.md")
	}
	if !strings.Contains(err.Error(), "docs/standard-library.md") {
		t.Fatalf("finding should name the doc, got: %v", err)
	}
}

func TestCatalogCoverageStubOnlyModuleNotRequiredInDocs(t *testing.T) {
	// Planned modules (toml, yaml, msgpack, markdown) keep a comment-only
	// .pr stub. They are not shipped and must not be demanded of the docs.
	root := coverageRepo(t,
		"[modules.toml]\ndescription = \"toml (planned)\"\n",
		map[string]string{"modules/toml/toml.pr": "// TOML 1.0.0 — planned.\n//\n\n"},
		"nothing here\n", "nothing here\n")
	if err := checkCatalogCoverage(root); err != nil {
		t.Fatalf("comment-only stub must not be required in the inventories, got: %v", err)
	}
}

func TestCatalogCoverageTestOnlyFileIsNotSource(t *testing.T) {
	root := coverageRepo(t,
		"[modules.thing]\ndescription = \"thing\"\n",
		map[string]string{"modules/thing/thing_test.pr": "assert(true);\n"},
		"nothing here\n", "nothing here\n")
	if err := checkCatalogCoverage(root); err != nil {
		t.Fatalf("a _test.pr file alone does not make a module shipped, got: %v", err)
	}
}

// --- parseCatalogModules ---

func TestParseCatalogModulesIgnoresNonModuleSections(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "catalog.toml")
	content := "[catalog]\nepoch = \"2026.8\"\nurl = \"ignored\"\n\n" +
		"[modules.std]\ndescription = \"std\"\n\n" +
		"[modules.remote]\nurl = \"https://example.com/r.git\"\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := parseCatalogModules(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 module entries, got %d: %v", len(entries), entries)
	}
	if entries["std"].remote {
		t.Error("std has no url and must not be remote")
	}
	if !entries["remote"].remote {
		t.Error("remote has a url and must be marked remote")
	}
	// The `url` under [catalog] must not leak into a module entry.
	if _, ok := entries["catalog"]; ok {
		t.Error("[catalog] is not a module section")
	}
}

func TestCatalogCoverageNoCatalogScopesItselfOut(t *testing.T) {
	// RunPreCommit is exercised against bare temp repos with no
	// catalog.toml; the check must scope out rather than error, or it
	// takes down the whole pre-commit hook outside this repo.
	root := docsRepo(t, map[string]string{"a.md": "# A\n"})
	if err := checkCatalogCoverage(root); err != nil {
		t.Fatalf("a tree with no catalog.toml must be a no-op, got: %v", err)
	}
}

func TestCatalogCoverageMissingInventoryDocIsAFinding(t *testing.T) {
	// A deleted inventory doc must not silently disable the check.
	root := docsRepo(t, map[string]string{
		"catalog.toml":     "[modules.io]\ndescription = \"io\"\n",
		"modules/io/io.pr": "print_line(\"hi\");\n",
		"CLAUDE.md":        "see modules/io/io.pr\n",
		// docs/standard-library.md deliberately absent.
	})
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected a finding for a missing inventory doc")
	}
	if !strings.Contains(err.Error(), "cannot read module inventory docs/standard-library.md") {
		t.Fatalf("finding should name the unreadable inventory, got: %v", err)
	}
	// Reported once, not once per module. (The path appears twice within
	// that single line — the label and the wrapped os error — so count
	// finding lines, not substrings.)
	if n := strings.Count(err.Error(), "cannot read module inventory"); n != 1 {
		t.Fatalf("unreadable inventory should yield exactly one finding, got %d: %v", n, err)
	}
}

// --- CheckDocs (aggregation) ---

// notAGitRepo guards the "git ls-files fails" tests: they assume the temp dir
// is outside any working tree, which is only true if TMPDIR is.
func notAGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Skip("TMPDIR is inside a git work tree; cannot exercise the ls-files failure path")
	}
}

func TestCheckDocsCleanTreePasses(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"catalog.toml":             "[modules.io]\ndescription = \"io\"\n",
		"modules/io/io.pr":         "print_line(\"hi\");\n",
		"CLAUDE.md":                "inventory: modules/io/io.pr — see [docs](docs/index.md)\n",
		"docs/index.md":            "- [stdlib](standard-library.md)\n",
		"docs/standard-library.md": "inventory: modules/io/io.pr\n",
	})
	if err := CheckDocs(root); err != nil {
		t.Fatalf("a coherent tree must produce no findings, got: %v", err)
	}
}

func TestCheckDocsReportsAllThreeChecksNotJustTheFirst(t *testing.T) {
	// The three checks are independent, so a tree that violates all three
	// must report all three. An early return here would silently disable
	// the later checks for anyone whose first violation is a dangling link
	// — exactly the failure mode this gate exists to prevent.
	root := docsRepo(t, map[string]string{
		"catalog.toml":             "[modules.io]\ndescription = \"io\"\n",
		"modules/io/io.pr":         "print_line(\"hi\");\n",
		"CLAUDE.md":                "no module inventory here\n",
		"docs/index.md":            "- [gone](tracker-tags.md)\n- [stdlib](standard-library.md)\n",
		"docs/standard-library.md": "no module inventory here\n",
		"docs/orphan.md":           "# Orphan\n",
	})
	err := CheckDocs(root)
	if err == nil {
		t.Fatal("expected findings from all three checks")
	}
	for _, want := range []string{
		"dangling markdown links",
		"docs not linked from docs/index.md",
		"catalog coverage",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregate error is missing the %q section, got:\n%v", want, err)
		}
	}
	// And the specifics survive aggregation, not just the headings.
	for _, want := range []string{"tracker-tags.md", "docs/orphan.md", "modules/io/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregate error lost the detail %q, got:\n%v", want, err)
		}
	}
}

func TestCheckDocsBareRepoIsNoOp(t *testing.T) {
	// RunPreCommit calls CheckDocs unconditionally, and its own tests run
	// against bare temp repos with no docs/, no index, and no catalog.
	root := docsRepo(t, map[string]string{"README.md": "# Hi\n"})
	if err := CheckDocs(root); err != nil {
		t.Fatalf("a tree with no docs/ or catalog.toml must be a no-op, got: %v", err)
	}
}

// --- checkDocLinks: error and skip paths ---

func TestDocLinksNonGitDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	notAGitRepo(t, root)
	err := checkDocLinks(root)
	if err == nil {
		t.Fatal("expected an error when the tree is not a git repository")
	}
	if !strings.Contains(err.Error(), "list tracked markdown files") {
		t.Fatalf("error should say which step failed, got: %v", err)
	}
}

func TestDocLinksSkipsStagedDeletionAndKeepsScanning(t *testing.T) {
	// A staged deletion is still in the index, so `git ls-files` returns a
	// path with no file behind it. That must be skipped rather than abort
	// the scan — the remaining files still need checking.
	root := docsRepo(t, map[string]string{
		"deleted.md": "# Gone\n",
		"kept.md":    "[x](does-not-exist.md)\n",
	})
	if err := os.Remove(filepath.Join(root, "deleted.md")); err != nil {
		t.Fatal(err)
	}
	err := checkDocLinks(root)
	if err == nil {
		t.Fatal("the surviving file's dangling link must still be reported")
	}
	if !strings.Contains(err.Error(), "kept.md") {
		t.Fatalf("scan stopped at the deleted file, got: %v", err)
	}
}

// --- checkDocIndex: error paths ---

func TestDocIndexNonGitDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	notAGitRepo(t, root)
	err := checkDocIndex(root)
	if err == nil {
		t.Fatal("expected an error when the tree is not a git repository")
	}
	if !strings.Contains(err.Error(), "list tracked docs") {
		t.Fatalf("error should say which step failed, got: %v", err)
	}
}

func TestDocIndexUnreadableIndexErrors(t *testing.T) {
	// Only a *missing* index scopes the check out. An index that exists but
	// cannot be read is a real failure and must not be mistaken for one.
	root := docsRepo(t, map[string]string{"docs/tags.md": "# Tags\n"})
	if err := os.Mkdir(filepath.Join(root, "docs", "index.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := checkDocIndex(root)
	if err == nil {
		t.Fatal("expected an error for an unreadable index")
	}
	if !strings.Contains(err.Error(), "read docs/index.md") {
		t.Fatalf("error should name the index, got: %v", err)
	}
}

func TestDocIndexExternalLinkDoesNotSatisfyCoverage(t *testing.T) {
	// The index routinely carries http(s) links; they are skipped when
	// building the linked set, so they can never stand in for a local doc.
	root := docsRepo(t, map[string]string{
		"docs/index.md":      "- [spec](https://example.com/code-style.md)\n- [tags](tags.md)\n",
		"docs/tags.md":       "# Tags\n",
		"docs/code-style.md": "# Style\n",
	})
	err := checkDocIndex(root)
	if err == nil {
		t.Fatal("an external URL must not count as linking the local doc")
	}
	if !strings.Contains(err.Error(), "docs/code-style.md") {
		t.Fatalf("finding should name the unlinked doc, got: %v", err)
	}
}

// --- checkCatalogCoverage: error paths ---

func TestCatalogCoverageUnreadableCatalogErrors(t *testing.T) {
	// A catalog that is absent scopes the check out; one that exists but
	// cannot be parsed must surface, not silently pass.
	root := docsRepo(t, map[string]string{"a.md": "# A\n"})
	if err := os.Mkdir(filepath.Join(root, "catalog.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected an error for an unreadable catalog.toml")
	}
	if !strings.Contains(err.Error(), "read catalog.toml") {
		t.Fatalf("error should name the catalog, got: %v", err)
	}
}

func TestCatalogCoverageModulesNotADirectoryErrors(t *testing.T) {
	root := docsRepo(t, map[string]string{
		"catalog.toml": "[modules.io]\ndescription = \"io\"\n",
		"modules":      "not a directory\n",
	})
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected an error when modules/ is not a directory")
	}
	if !strings.Contains(err.Error(), "read modules/") {
		t.Fatalf("error should name the step, got: %v", err)
	}
}

func TestCatalogCoverageIgnoresLooseFilesInModules(t *testing.T) {
	// A stray file directly under modules/ is not a module and must not be
	// reported as one missing a catalog entry.
	root := coverageRepo(t,
		"[modules.io]\ndescription = \"io\"\n",
		map[string]string{
			"modules/io/io.pr":  "print_line(\"hi\");\n",
			"modules/README.md": "# Modules\n",
		},
		"see modules/io/io.pr\n",
		"see modules/io/io.pr\n")
	if err := checkCatalogCoverage(root); err != nil {
		t.Fatalf("a loose file under modules/ must be ignored, got: %v", err)
	}
}

func TestCatalogCoverageEntryWithoutDirectoryIsNotShipped(t *testing.T) {
	// Local entries for modules that have not been created yet (no url, no
	// directory) ship nothing, so the inventories owe them nothing.
	root := coverageRepo(t,
		"[modules.io]\ndescription = \"io\"\n\n[modules.yaml]\ndescription = \"yaml (planned)\"\n",
		map[string]string{"modules/io/io.pr": "print_line(\"hi\");\n"},
		"see modules/io/io.pr\n",
		"see modules/io/io.pr\n")
	if err := checkCatalogCoverage(root); err != nil {
		t.Fatalf("an entry with no directory must not be required in the inventories, got: %v", err)
	}
}

func TestCatalogCoverageModulePathNotADirectoryErrors(t *testing.T) {
	root := coverageRepo(t,
		"[modules.broken]\ndescription = \"broken\"\n",
		map[string]string{"modules/broken": "this is a file, not a module directory\n"},
		"nothing\n", "nothing\n")
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected an error when a module path is not a directory")
	}
	if !strings.Contains(err.Error(), "modules/broken") {
		t.Fatalf("error should name the offending path, got: %v", err)
	}
}

func TestCatalogCoverageUnreadableSourceFileErrors(t *testing.T) {
	// A .pr entry that cannot be read must abort with a real error rather
	// than be silently treated as "not shipped", which would drop the
	// module out of the inventory check.
	root := coverageRepo(t,
		"[modules.io]\ndescription = \"io\"\n",
		map[string]string{"modules/io/keep.md": "# keep the directory\n"},
		"see modules/io/io.pr\n",
		"see modules/io/io.pr\n")
	link := filepath.Join(root, "modules", "io", "io.pr")
	if err := os.Symlink(filepath.Join(root, "modules", "io", "no-such-target.pr"), link); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}
	err := checkCatalogCoverage(root)
	if err == nil {
		t.Fatal("expected an error for an unreadable .pr file")
	}
	if !strings.Contains(err.Error(), "io.pr") {
		t.Fatalf("error should name the unreadable file, got: %v", err)
	}
}

// --- readDirIfExists ---

func TestReadDirIfExistsDistinguishesAbsentFromNotADirectory(t *testing.T) {
	// The whole reason this helper exists: os.ReadDir of a regular file on
	// Windows fails with ERROR_PATH_NOT_FOUND, which os.IsNotExist reports
	// as "absent". Callers must still be able to tell the two apart, or a
	// malformed tree silently scopes the check out.
	root := t.TempDir()

	entries, exists, err := readDirIfExists(filepath.Join(root, "missing"))
	if err != nil || exists || entries != nil {
		t.Fatalf("absent path: got (%v, %v, %v), want (nil, false, nil)", entries, exists, err)
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readDirIfExists(file); err == nil || !exists {
		t.Fatalf("regular file: got (exists=%v, err=%v), want (true, non-nil)", exists, err)
	}

	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entries, exists, err = readDirIfExists(dir)
	if err != nil || !exists || len(entries) != 1 {
		t.Fatalf("directory: got (%d entries, exists=%v, err=%v), want (1, true, nil)", len(entries), exists, err)
	}
}
