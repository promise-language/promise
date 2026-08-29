package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withBakedRoot sets the build-time root for one test, restoring it after.
// The stamp is stored encoded, so it is written the same way ./make writes it;
// an empty root means "this binary carries no stamp".
func withBakedRoot(t *testing.T, root string) {
	t.Helper()
	saved := bakedRoot
	if root == "" {
		bakedRoot = ""
	} else {
		bakedRoot = EncodeRoot(root)
	}
	t.Cleanup(func() { bakedRoot = saved })
}

// TestRootStampSurvivesAwkwardPaths pins the reason the stamp is encoded rather
// than written literally into -ldflags: a repo path with a space in it breaks
// the link outright, and one with a quote breaks any quoting used to fix that.
// Both are ordinary paths — C:\Users\John Doe\promise, /Users/o'brien/promise.
func TestRootStampSurvivesAwkwardPaths(t *testing.T) {
	for _, path := range []string{
		`/Users/John Doe/promise`,
		`/Users/o'brien/promise`,
		`/srv/say "hi"/promise`,
		`C:\Users\John Doe\promise`,
		`/tmp/tab	and space/promise`,
	} {
		encoded := EncodeRoot(path)
		if strings.ContainsAny(encoded, " \t\n'\"\\") {
			t.Errorf("EncodeRoot(%q) = %q, which -ldflags would mis-split", path, encoded)
		}
		saved := bakedRoot
		bakedRoot = encoded
		got := BakedRootValue()
		bakedRoot = saved
		if got != filepath.Clean(path) {
			t.Errorf("round trip of %q gave %q", path, got)
		}
	}
}

// TestFindRootUsesBakedRoot is the contract: a tool acts on the repository it
// was built for.
func TestFindRootUsesBakedRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "catalog.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	withBakedRoot(t, repo)

	got, err := FindRoot()
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	if got != repo {
		t.Errorf("FindRoot = %q, want the baked root %q", got, repo)
	}
}

// TestFindRootIgnoresCwd is the regression this design exists for (T1813): a
// directory that merely looks like a repository must not become one. Standing
// inside another tree — even one holding a catalog.toml — changes nothing.
func TestFindRootIgnoresCwd(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "catalog.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	withBakedRoot(t, repo)

	impostor := t.TempDir()
	if err := os.WriteFile(filepath.Join(impostor, "catalog.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(impostor)

	got, err := FindRoot()
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	if got == impostor {
		t.Fatal("FindRoot followed the working directory into another tree")
	}
	if got != repo {
		t.Errorf("FindRoot = %q, want the baked root %q", got, repo)
	}
}

// TestFindRootRejectsMovedRoot: a baked root that is no longer a repo is an
// error naming the rebuild, not a silent fallback to something nearby.
func TestFindRootRejectsMovedRoot(t *testing.T) {
	withBakedRoot(t, filepath.Join(t.TempDir(), "gone"))

	if _, err := FindRoot(); err == nil {
		t.Fatal("expected an error for a baked root with no catalog.toml")
	}
}

// TestFindRootUnstampedIgnoresCwd covers `go run` / `go test` binaries: the
// fallback is the tool's own place on disk, never the caller's.
func TestFindRootUnstampedIgnoresCwd(t *testing.T) {
	withBakedRoot(t, "")

	impostor := t.TempDir()
	if err := os.WriteFile(filepath.Join(impostor, "catalog.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(impostor)

	// The test binary does not live in <root>/bin, so this must fail rather than
	// answer with the directory it happens to be standing in.
	got, err := FindRoot()
	if err == nil && got == impostor {
		t.Fatalf("unstamped FindRoot resolved the working directory %q", impostor)
	}
}

// TestRootForTestsFindsRepo: the test-only helper resolves this checkout from
// its own compile-time source path.
func TestRootForTestsFindsRepo(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Fatalf("RootForTests: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "catalog.toml")); err != nil {
		t.Errorf("RootForTests = %q, which has no catalog.toml: %v", root, err)
	}
}
