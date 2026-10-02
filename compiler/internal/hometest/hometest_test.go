package hometest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDamage is the verdict Pin acts on: silence when the listing held, and a
// report naming both readings when it moved — including the shared home being
// created from nothing, which is what a first write to ~/.promise looks like.
func TestDamage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		damaged       bool
	}{
		{"absent and still absent", "absent", "absent", false},
		{"unchanged listing", "bin cache", "bin cache", false},
		{"created", "absent", "cache", true},
		{"gained a subtree", "bin cache", "bin cache epochs", true},
		{"lost a subtree", "bin cache", "cache", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := damage(tc.before, tc.after)
			if (report != "") != tc.damaged {
				t.Fatalf("damage(%q, %q) = %q, want damaged=%v", tc.before, tc.after, report, tc.damaged)
			}
			if tc.damaged && (!strings.Contains(report, tc.before) || !strings.Contains(report, tc.after)) {
				t.Errorf("report %q does not name both readings", report)
			}
		})
	}
}

// TestSharedHomeListing pins what the guard reads: the sorted top-level names
// of ~/.promise, "absent" without one, and "unavailable" on a host that cannot
// name its user home.
func TestSharedHomeListing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	if got := sharedHomeListing(); got != "absent" {
		t.Errorf("no ~/.promise reads %q, want %q", got, "absent")
	}
	for _, dir := range []string{"cache", "bin"} {
		if err := os.MkdirAll(filepath.Join(home, ".promise", dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := sharedHomeListing(), "bin cache"; got != want {
		t.Errorf("listing = %q, want %q", got, want)
	}

	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if got := sharedHomeListing(); got != "unavailable" {
		t.Errorf("an unnameable home reads %q, want %q", got, "unavailable")
	}
}

// fakeM stands in for *testing.M: it records the environment its tests would
// have run under, and can act like a test that writes the shared home.
type fakeM struct {
	run    func()
	code   int
	home   string
	pinned string // Home() as the package's tests would see it
	ran    bool
}

func (f *fakeM) Run() int {
	f.ran = true
	f.home = os.Getenv("PROMISE_HOME")
	f.pinned = Home()
	if f.run != nil {
		f.run()
	}
	return f.code
}

// TestPin covers both outcomes: a package that stays in its own home keeps its
// exit code and leaves nothing behind, and one whose test writes ~/.promise
// fails even though every test passed.
func TestPin(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	t.Setenv("PROMISE_HOME", "")
	t.Setenv("PROMISE_CACHE", filepath.Join(t.TempDir(), "inherited"))

	clean := &fakeM{run: func() {
		if v, ok := os.LookupEnv("PROMISE_CACHE"); ok {
			t.Errorf("PROMISE_CACHE = %q under Pin, want unset", v)
		}
	}}
	if code := Pin(clean); code != 0 {
		t.Errorf("a package that stayed in its own home exited %d, want 0", code)
	}
	if clean.home == "" || strings.HasPrefix(clean.home, filepath.Join(userHome, ".promise")) {
		t.Errorf("the package ran with PROMISE_HOME=%q, want a private temp home", clean.home)
	}
	if _, err := os.Stat(clean.home); !os.IsNotExist(err) {
		t.Errorf("the private home %s outlived the package: %v", clean.home, err)
	}

	leaky := &fakeM{run: func() {
		if err := os.MkdirAll(filepath.Join(userHome, ".promise", "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
	}}
	if code := Pin(leaky); code != 1 {
		t.Errorf("a package whose test wrote ~/.promise exited %d, want 1", code)
	}
}

// TestPinReplacesAnAmbientHome: the private home is the package's whatever the
// caller exported — under bin/test that is the worktree's .promise-home, and
// under a bare `go test` it may be ~/.promise spelled out. The caller's home is
// left untouched, and Home() names the private one only while the package runs.
func TestPinReplacesAnAmbientHome(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	ambient := t.TempDir()
	t.Setenv("PROMISE_HOME", ambient)

	m := &fakeM{run: func() {
		if err := os.WriteFile(filepath.Join(os.Getenv("PROMISE_HOME"), "written"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	if code := Pin(m); code != 0 {
		t.Fatalf("Pin exited %d, want 0", code)
	}
	if m.home == ambient {
		t.Errorf("the package ran in the caller's home %s, want a private one", ambient)
	}
	if m.pinned != m.home {
		t.Errorf("Home() = %q while the package ran, want its PROMISE_HOME %q", m.pinned, m.home)
	}
	if got := Home(); got != "" {
		t.Errorf("Home() = %q after Pin returned, want \"\"", got)
	}
	if entries, _ := os.ReadDir(ambient); len(entries) != 0 {
		t.Errorf("the caller's home %s was written", ambient)
	}
}

// TestPinWithoutAPrivateHomeRunsNothing: a package that cannot be given a home
// of its own fails without running a single test, rather than running them in
// the home it would otherwise inherit.
func TestPinWithoutAPrivateHomeRunsNothing(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} { // os.TempDir, per platform
		t.Setenv(name, absent)
	}
	m := &fakeM{}
	if code := Pin(m); code != 1 {
		t.Errorf("Pin exited %d with no temp directory to make a home in, want 1", code)
	}
	if m.ran {
		t.Error("the package's tests ran without a private home")
	}
}
