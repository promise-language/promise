package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"

	"github.com/promise-language/promise/compiler/internal/module"
)

func TestParseAddFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args       []string
		wantPos    []string
		wantSubdir string
		wantName   string
	}{
		{[]string{"url"}, []string{"url"}, "", ""},
		{[]string{"--subdir", "proto/wire", "url"}, []string{"url"}, "proto/wire", ""},
		{[]string{"url", "--subdir=proto/wire"}, []string{"url"}, "proto/wire", ""},
		{[]string{"--name", "w", "--subdir", "a/b", "url", "v1"}, []string{"url", "v1"}, "a/b", "w"},
		{[]string{"--name=w", "url"}, []string{"url"}, "", "w"},
	}
	for _, tc := range cases {
		pos, subdir, name, err := parseAddFlags(tc.args)
		if err != nil {
			t.Errorf("parseAddFlags(%v): %v", tc.args, err)
			continue
		}
		if strings.Join(pos, ",") != strings.Join(tc.wantPos, ",") || subdir != tc.wantSubdir || name != tc.wantName {
			t.Errorf("parseAddFlags(%v) = %v, %q, %q; want %v, %q, %q",
				tc.args, pos, subdir, name, tc.wantPos, tc.wantSubdir, tc.wantName)
		}
	}

	for _, args := range [][]string{{"--subdir"}, {"url", "--name"}} {
		if _, _, _, err := parseAddFlags(args); err == nil {
			t.Errorf("parseAddFlags(%v): expected a missing-value error", args)
		}
	}
}

// T1611: two aliased catalog imports in different files must each resolve to
// their own IR prefix, not to their alias. (Sema makes aliases project-unique, so
// the ambiguous same-alias case cannot arise — see TestSameAliasTwoModulesRejected.)
func TestPackageRemoveNamedEntries(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	manifest := `[module]
name = "app"
epoch = "2026.0"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/wire"

[require.types]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/types"

[require.other]
url = "https://github.com/acme/other"
commit = "def456"
`
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	// By import name — removes just that entry.
	out := captureStdout(t, func() { captureStderr(func() { runPackageRemove([]string{"wire"}) }) })
	if !strings.Contains(out, "[require.wire]") {
		t.Errorf("expected a removal message naming the entry, got: %s", out)
	}
	cfg, err := module.ParseConfig(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if _, ok := cfg.NamedRequire["wire"]; ok {
		t.Error("[require.wire] survived removal by name")
	}
	if _, ok := cfg.NamedRequire["types"]; !ok {
		t.Error("removing 'wire' by name also removed its sibling")
	}

	// By URL — removes every named entry addressed in that repo.
	captureStdout(t, func() {
		captureStderr(func() { runPackageRemove([]string{"https://github.com/acme/base"}) })
	})
	cfg, err = module.ParseConfig(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig after URL remove: %v", err)
	}
	if _, ok := cfg.NamedRequire["types"]; ok {
		t.Error("[require.types] survived removal by repo URL")
	}
	if _, ok := cfg.NamedRequire["other"]; !ok {
		t.Error("an unrelated named entry was removed")
	}
}

// T1524: a URL carrying the `repo//subdir` spelling of another ecosystem must be
// refused by `package add`/`package pin` rather than written into a manifest that
// ParseConfig then rejects.
func TestAddRejectsDoubleSlashURL(t *testing.T) {
	if os.Getenv("TEST_ADD_DOUBLE_SLASH") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
		runAdd([]string{"https://github.com/acme/base//proto/wire"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAddRejectsDoubleSlashURL")
	cmd.Env = append(os.Environ(), "TEST_ADD_DOUBLE_SLASH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a '//' URL, got:\n%s", out)
	}
	if !strings.Contains(string(out), "subdir") {
		t.Errorf("expected the error to point at the 'subdir' field, got:\n%s", out)
	}
}

// A subdir module with no matching pin must be reported against the module, not
// the bare repo: the hint has to name the [require.NAME] form, since a flat
// [require] line cannot carry a subdir at all (T1524).
func TestLoadRemoteSubdirNoPinHint(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &module.Config{
		Name:    "app",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		Replace: map[string]string{},
	}
	loader := testModuleLoaderWithConfig(projectDir, cfg)

	_, err := loader.loadRemote("https://github.com/acme/base", "proto/wire", "wire")
	if err == nil {
		t.Fatal("expected a no-pin error")
	}
	msg := err.Error()
	for _, want := range []string{"no pin", "proto/wire", "[require.wire]", "subdir"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}
	// The flat-[require] hint would be actively wrong here — a flat key is the URL
	// and has nowhere to put the subdir.
	if strings.Contains(msg, "package pin") {
		t.Errorf("error %q suggests `package pin`, which cannot express a subdir", msg)
	}
}

// A pin recorded for one subdir module must not satisfy its sibling in the same
// repo: identities differ, so each [require.NAME] entry needs its own pin.
func TestSubdirPinsAreNotSharedAcrossSiblings(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &module.Config{
		Name:    "app",
		Epoch:   "2026.0",
		Dir:     projectDir,
		Require: map[string]string{},
		Replace: map[string]string{},
		NamedRequire: map[string]*module.RequireEntry{
			"wire": {URL: "https://github.com/acme/base", Commit: "abc123", Subdir: "proto/wire"},
		},
	}
	loader := testModuleLoaderWithConfig(projectDir, cfg)

	// The sibling has no entry of its own — resolution must stop at the pin
	// lookup rather than silently reusing 'wire''s commit.
	_, err := loader.loadRemote("https://github.com/acme/base", "proto/types", "types")
	if err == nil {
		t.Fatal("expected a no-pin error for the unpinned sibling")
	}
	if !strings.Contains(err.Error(), "no pin") {
		t.Errorf("expected a no-pin error, got: %v", err)
	}
	// And the repo root, addressed with no subdir, is a third identity.
	_, err = loader.loadRemote("https://github.com/acme/base", "", "base")
	if err == nil {
		t.Fatal("expected a no-pin error for the repo root")
	}
	if !strings.Contains(err.Error(), "no pin") {
		t.Errorf("expected a no-pin error, got: %v", err)
	}
}

// runAdd rejects every unusable --subdir/--name combination before it touches the
// manifest. Each case runs in a subprocess because the failures call os.Exit(1).
func TestAddSubdirFlagValidation(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantText string
	}{
		// A catalog name resolves to a URL the user does not control, and its
		// import name is fixed by the catalog.
		{"catalog_name_with_subdir", []string{"--subdir", "proto/wire", "json"}, "require a git URL"},
		{"catalog_name_with_name", []string{"--name", "w", "json"}, "require a git URL"},
		// An escaping subdir must never reach the manifest.
		{"escaping_subdir", []string{"--subdir", "../outside", "https://github.com/acme/base"}, "invalid --subdir"},
		{"absolute_subdir", []string{"--subdir", "/etc", "https://github.com/acme/base"}, "invalid --subdir"},
		// The derived import name is the last subdir component; if that is not an
		// identifier, `use NAME;` could never name it.
		{"underivable_name", []string{"--subdir", "proto/my-wire", "https://github.com/acme/base"}, "not a usable import name"},
		{"explicit_bad_name", []string{"--subdir", "proto/wire", "--name", "9wire", "https://github.com/acme/base"}, "not a usable import name"},
		// A [require.NAME] entry is shadowed by a catalog module of the same name.
		{"catalog_name_collision", []string{"--subdir", "proto/json", "https://github.com/acme/base"}, "conflicts with catalog module"},
		// A flag with no value.
		{"missing_flag_value", []string{"https://github.com/acme/base", "--subdir"}, "requires a value"},
	}

	if idx := os.Getenv("TEST_ADD_FLAG_CASE"); idx != "" {
		for _, tc := range cases {
			if tc.name != idx {
				continue
			}
			dir := t.TempDir()
			os.Chdir(dir)
			os.WriteFile(filepath.Join(dir, "promise.toml"),
				[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
			runAdd(tc.args)
			return
		}
		os.Exit(9) // unknown case name — fail loudly
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestAddSubdirFlagValidation")
			cmd.Env = append(os.Environ(), "TEST_ADD_FLAG_CASE="+tc.name)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected a non-zero exit, got:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantText) {
				t.Errorf("expected %q in output, got:\n%s", tc.wantText, out)
			}
			// Nothing may have been written to the manifest.
			if strings.Contains(string(out), "Added ") {
				t.Errorf("a rejected add still reported success:\n%s", out)
			}
		})
	}
}

// `promise package pin` writes a flat [require] key, which cannot carry a subdir —
// so the `repo//subdir` spelling has to be refused there too, not just in `add`.
func TestPinRejectsDoubleSlashURL(t *testing.T) {
	if os.Getenv("TEST_PIN_DOUBLE_SLASH") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
		runPin([]string{"https://github.com/acme/base//proto/wire"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestPinRejectsDoubleSlashURL")
	cmd.Env = append(os.Environ(), "TEST_PIN_DOUBLE_SLASH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a '//' URL, got:\n%s", out)
	}
	if !strings.Contains(string(out), "subdir") {
		t.Errorf("expected the error to point at the 'subdir' field, got:\n%s", out)
	}
}

// A catalog entry may also address a subdirectory (catalog.toml grew a `subdir`
// key alongside url/commit), so loadCatalog must resolve into it rather than the
// repo root. Exercised with a synthetic catalog over a local bare repo — the
// embedded catalog ships no subdir entry today, so nothing else covers this path.
func TestLoadCatalogSubdirEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo, commit := clitest.MakeSubdirRepo(t, map[string]string{"wire": "proto/wire"})

	saved := embeddedCatalog
	// std must stay in the catalog — it is auto-imported into every file.
	embeddedCatalog = []byte("[catalog]\nepoch = \"2026.0\"\n\n" +
		"[modules.std]\ndescription = \"standard library\"\n\n" +
		"[modules.wire]\nurl = \"" + repo + "\"\n" +
		"commit = \"" + commit + "\"\nsubdir = \"proto/wire\"\ndescription = \"wire types\"\n")
	defer func() { embeddedCatalog = saved }()

	projectDir := t.TempDir()
	loader := testModuleLoaderWithConfig(projectDir, &module.Config{
		Name: "app", Epoch: "2026.0", Dir: projectDir,
		Require: map[string]string{}, Replace: map[string]string{},
	})
	if loader.catalog == nil {
		t.Fatal("synthetic catalog did not parse")
	}
	if e := loader.catalog.Lookup("wire"); e == nil || e.Subdir != "proto/wire" {
		t.Fatalf("catalog entry = %+v, want subdir proto/wire", e)
	}

	mi, err := loader.loadCatalog("wire")
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if mi == nil {
		t.Fatal("loadCatalog returned no module info")
	}
	if filepath.Base(mi.AbsDir) != "wire" {
		t.Errorf("resolved to %q, want the proto/wire subdirectory", mi.AbsDir)
	}
	if _, err := os.Stat(filepath.Join(mi.AbsDir, "promise.toml")); err != nil {
		t.Errorf("resolved directory has no manifest: %v", err)
	}
	// A catalog module keeps its catalog identity — the subdir affects where the
	// sources come from, not how the module is named in IR.
	if mi.GlobalIdentity != module.GlobalIdentityForCatalog("wire") {
		t.Errorf("identity = %q, want the catalog identity", mi.GlobalIdentity)
	}
}

// The CLI argument guard does not cover a URL the *catalog* supplies. ParseConfig
// rejects a '//' URL, so writing one would produce a manifest the next command
// cannot read — `add` has to refuse it instead (T1524).
func TestAddRejectsCatalogSuppliedDoubleSlashURL(t *testing.T) {
	if os.Getenv("TEST_ADD_CATALOG_DOUBLE_SLASH") == "1" {
		embeddedCatalog = []byte("[catalog]\nepoch = \"2026.0\"\n\n" +
			"[modules.wire]\nurl = \"https://github.com/acme/base//proto/wire\"\n" +
			"commit = \"abc123\"\ndescription = \"bad url\"\n")
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644)
		runAdd([]string{"wire"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAddRejectsCatalogSuppliedDoubleSlashURL")
	cmd.Env = append(os.Environ(), "TEST_ADD_CATALOG_DOUBLE_SLASH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a catalog '//' URL, got:\n%s", out)
	}
	if !strings.Contains(string(out), "catalog url") {
		t.Errorf("expected the error to name the catalog URL, got:\n%s", out)
	}
}

// One URL can appear in both tables at once: a flat [require] key from before the
// module moved into a subdirectory, plus the [require.NAME] entries that replaced
// it. `remove <url>` must clear all of them in a single pass, leaving a manifest
// that still parses.
func TestPackageRemoveMixedFlatAndNamed(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	manifest := `[module]
name = "app"
epoch = "2026.0"

[require]
"https://github.com/acme/base" = "abc123"
"https://github.com/acme/keep" = "999999"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/wire"
`
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		captureStderr(func() { runPackageRemove([]string{"github.com/acme/base"}) })
	})
	if !strings.Contains(out, "[require.wire]") {
		t.Errorf("expected the named entry to be reported removed, got: %s", out)
	}

	cfg, err := module.ParseConfig(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig after removal: %v", err)
	}
	for url := range cfg.Require {
		if module.NormalizeURL(url) == module.NormalizeURL("https://github.com/acme/base") {
			t.Errorf("the flat [require] key survived removal: %q", url)
		}
	}
	if _, ok := cfg.NamedRequire["wire"]; ok {
		t.Error("[require.wire] survived removal by its repo URL")
	}
	if cfg.Require["https://github.com/acme/keep"] != "999999" {
		t.Errorf("an unrelated flat entry was disturbed: %v", cfg.Require)
	}
}
