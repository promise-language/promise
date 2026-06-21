package common

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// setupGitRepo creates a temp git repo with an initial baselines.json commit,
// then stages a modified baselines.json. Returns the repo root.
func setupGitRepo(t *testing.T, headBaselines, stagedBaselines Baselines) string {
	t.Helper()
	root := t.TempDir()

	// Init git repo.
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	git("init")
	git("config", "user.name", "test")
	git("config", "user.email", "test@test")

	// Create baselines.json under the expected path and commit.
	gatesDir := filepath.Join(root, "tools", "gates")
	os.MkdirAll(gatesDir, 0o755)
	blPath := filepath.Join(gatesDir, "baselines.json")

	headData, _ := json.MarshalIndent(headBaselines, "", "  ")
	headData = append(headData, '\n')
	os.WriteFile(blPath, headData, 0o644)

	git("add", "tools/gates/baselines.json")
	git("commit", "-m", "initial baselines")

	// Write staged version.
	stagedData, _ := json.MarshalIndent(stagedBaselines, "", "  ")
	stagedData = append(stagedData, '\n')
	os.WriteFile(blPath, stagedData, 0o644)

	git("add", "tools/gates/baselines.json")

	return root
}

// initGitRepoWithStagedFile creates a temp git repo with one initial commit,
// then writes and stages a file at relPath. Returns the repo root.
func initGitRepoWithStagedFile(t *testing.T, relPath string) string {
	t.Helper()
	root := t.TempDir()

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	git("init")
	git("config", "user.name", "test")
	git("config", "user.email", "test@test")

	// Seed an initial commit so HEAD exists.
	os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644)
	git("add", "seed.txt")
	git("commit", "-m", "seed")

	full := filepath.Join(root, relPath)
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte("data\n"), 0o644)
	git("add", relPath)

	return root
}

func TestRunPreCommit_RejectsLogFile(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "Ctmpverify_t0925.log")
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for staged .log file, got nil")
	}
}

func TestRunPreCommit_RejectsLogFileInSubdir(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "logs/run.log")
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for staged .log file in subdir, got nil")
	}
}

func TestRunPreCommit_RejectsNonASCIIFilename(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "résumé.txt")
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for non-ASCII filename, got nil")
	}
}

func TestRunPreCommit_AllowsNormalFile(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "src/main.go")
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected no error for normal file, got: %v", err)
	}
}

func TestRunPreCommit_RejectsUnformattedGo(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "seed2.txt")
	// gofmt-violating Go file under compiler/ (bad indentation + spacing).
	bad := "package main\nfunc  main(){\nx:=1\n_=x}\n"
	full := filepath.Join(root, "compiler", "internal", "bad.go")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte(bad), 0o644)
	if err := RunPreCommit(root); err == nil {
		t.Fatal("expected error for unformatted Go file, got nil")
	}
}

func TestRunPreCommit_AllowsFormattedGo(t *testing.T) {
	root := initGitRepoWithStagedFile(t, "seed2.txt")
	good := "package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"
	full := filepath.Join(root, "compiler", "internal", "good.go")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte(good), 0o644)
	if err := RunPreCommit(root); err != nil {
		t.Fatalf("expected no error for formatted Go file, got: %v", err)
	}
}

func TestUnformattedGoFiles_DetectsAndSkips(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		full := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	mk("compiler/a.go", "package a\n\nfunc F() {}\n")      // formatted
	mk("compiler/b.go", "package b\nfunc  G(){}\n")        // unformatted
	mk("compiler/vendor/c.go", "package c\nfunc  H(){}\n") // skipped (vendor)
	mk("compiler/notes.txt", "func  not_go(){}\n")         // skipped (not .go)

	got, err := UnformattedGoFiles(root)
	if err != nil {
		t.Fatalf("UnformattedGoFiles: %v", err)
	}
	if len(got) != 1 || got[0] != filepath.Join("compiler", "b.go") {
		t.Fatalf("expected [compiler/b.go], got %v", got)
	}
}

func TestUnformattedPromiseFiles_SkipsWithoutCompiler(t *testing.T) {
	// No bin/promise in a temp repo → can't check Promise, must skip (not error).
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "modules"), 0o755)
	os.WriteFile(filepath.Join(root, "modules", "x.pr"), []byte("main(){}\n"), 0o644)
	got, err := UnformattedPromiseFiles(root)
	if err != nil {
		t.Fatalf("expected nil error when bin/promise absent, got: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil (skipped), got %v", got)
	}
}

func TestIsASCII(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"plain.txt", true},
		{"with space.txt", true},
		{"résumé.txt", false},
		{"café", false},
		{"", true},
	}
	for _, c := range cases {
		if got := isASCII(c.s); got != c.want {
			t.Errorf("isASCII(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestValidateBaselinesDiff_NoRegression(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(110), Direction: "up", Updated: "2026-04-11"},
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateBaselinesDiff_RegressionBlocked(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(90), Direction: "up", Updated: "2026-04-11"},
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected regression error, got nil")
	}
}

func TestValidateBaselinesDiff_InformationalSkipped(t *testing.T) {
	// Informational metrics (no Direction) in HEAD should not block removal.
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"binary_size":     {Type: "informational"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			// binary_size removed — should be allowed since it's informational
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err != nil {
		t.Fatalf("expected nil (informational removal allowed), got: %v", err)
	}
}

func TestValidateBaselinesDiff_PendingSkipped(t *testing.T) {
	// Pending metrics (Direction set but Value nil) in HEAD should not block.
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"coverage":        {Direction: "up"}, // Pending
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"coverage":        {Value: fp(85.0), Direction: "up", Updated: "2026-04-11"},
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err != nil {
		t.Fatalf("expected nil (pending populated), got: %v", err)
	}
}

func TestValidateBaselinesDiff_PlatformRemoved(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		// linux-amd64 removed entirely
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected error for platform removal, got nil")
	}
}

func TestValidateBaselinesDiff_EnforcedMetricRemoved(t *testing.T) {
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			// host_leak_count removed — should be blocked since it's enforced
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected error for enforced metric removal, got nil")
	}
}

func TestValidateBaselinesDiff_StagedValueNilTreatedAsZero(t *testing.T) {
	// If staged has a metric with nil Value but HEAD has it enforced,
	// the staged value should be treated as 0 for ratchet check.
	head := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	staged := Baselines{
		"linux-amd64": {
			"host_test_count": {Direction: "up"}, // Value nil → treated as 0 → regression
		},
	}
	root := setupGitRepo(t, head, staged)

	err := validateBaselinesDiff(root)
	if err == nil {
		t.Fatal("expected regression (nil staged value treated as 0 < 100), got nil")
	}
}
