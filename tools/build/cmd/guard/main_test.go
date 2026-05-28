package main

import (
	"path/filepath"
	"testing"

	"github.com/promise-language/promise/tools/build/common"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"git status", []string{"git", "status"}},
		{"git  status   -v", []string{"git", "status", "-v"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := tokenize(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestSplitCommands(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"echo hi && git push", 2},
		{"cmd1 || cmd2", 2},
		{"cmd1; cmd2", 2},
		{"echo x | grep y", 2},
		{"git status", 1},
	}
	for _, tt := range tests {
		got := splitCommands(tt.input)
		if len(got) != tt.want {
			t.Errorf("splitCommands(%q) = %d parts, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestStripWrappers(t *testing.T) {
	tests := []struct {
		name   string
		tokens []string
		want0  string
		wantN  int
	}{
		{"env", []string{"env", "git", "push"}, "git", 2},
		{"sudo", []string{"sudo", "rm", "-rf", "/"}, "rm", 3},
		{"var", []string{"VAR=1", "FOO=bar", "git", "push"}, "git", 2},
		{"combined", []string{"env", "VAR=1", "sudo", "git", "push"}, "git", 2},
	}
	for _, tt := range tests {
		got := stripWrappers(tt.tokens)
		if len(got) != tt.wantN {
			t.Errorf("%s: len=%d, want %d", tt.name, len(got), tt.wantN)
		}
		if len(got) > 0 && got[0] != tt.want0 {
			t.Errorf("%s: [0]=%q, want %q", tt.name, got[0], tt.want0)
		}
	}
}

func TestFindGitSubcommand(t *testing.T) {
	tests := []struct {
		tokens []string
		want   string
	}{
		{[]string{"git", "push"}, "push"},
		{[]string{"git", "-c", "x=y", "push"}, "push"},
		{[]string{"git", "-C", "/path", "status"}, "status"},
		{[]string{"git", "--no-pager", "log"}, "log"},
	}
	for _, tt := range tests {
		got := findGitSubcommand(tt.tokens)
		if got != tt.want {
			t.Errorf("findGitSubcommand(%v) = %q, want %q", tt.tokens, got, tt.want)
		}
	}
}

func TestCheckGit(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []string
		blocked bool
	}{
		{"push", []string{"git", "push"}, true},
		{"push --force", []string{"git", "push", "--force"}, true},
		{"push -f", []string{"git", "push", "-f", "origin", "main"}, true},
		{"reset --hard", []string{"git", "reset", "--hard", "HEAD"}, true},
		{"status", []string{"git", "status"}, false},
		{"reset --soft", []string{"git", "reset", "--soft", "HEAD~1"}, false},
	}
	for _, tt := range tests {
		reason := checkGit(tt.tokens)
		if tt.blocked && reason == "" {
			t.Errorf("%s: expected blocked", tt.name)
		}
		if !tt.blocked && reason != "" {
			t.Errorf("%s: unexpected block: %s", tt.name, reason)
		}
	}
}

func TestCheckRm(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []string
		blocked bool
	}{
		{"rf", []string{"rm", "-rf", "/"}, true},
		{"r f", []string{"rm", "-r", "-f", "/"}, true},
		{"fr", []string{"rm", "-fr", "/tmp/x"}, true},
		{"r only", []string{"rm", "-r", "dir/"}, false},
		{"simple", []string{"rm", "file.txt"}, false},
	}
	for _, tt := range tests {
		reason := checkRm(tt.tokens)
		if tt.blocked && reason == "" {
			t.Errorf("%s: expected blocked", tt.name)
		}
		if !tt.blocked && reason != "" {
			t.Errorf("%s: unexpected block: %s", tt.name, reason)
		}
	}
}

func TestCheckSingle(t *testing.T) {
	blocked := []string{
		"git push",
		"git -c x push",
		"env git push",
		"curl http://x",
		"wget http://x",
		"npm install foo",
		"go install github.com/x",
		"apt install vim",
	}
	for _, cmd := range blocked {
		if checkSingle(cmd) == "" {
			t.Errorf("expected %q to be blocked", cmd)
		}
	}

	allowed := []string{
		"git status",
		"ls -la",
		"go test ./...",
		"rm file.txt",
	}
	for _, cmd := range allowed {
		if reason := checkSingle(cmd); reason != "" {
			t.Errorf("expected %q to be allowed, got: %s", cmd, reason)
		}
	}
}

func TestCheckAll(t *testing.T) {
	if checkAll("echo hi && git push") == "" {
		t.Error("expected chain with git push to be blocked")
	}
	if checkAll("git status && echo ok") != "" {
		t.Error("expected safe chain to be allowed")
	}
}

func TestStripQuotes(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{`"hello"`, "hello"},
		{`'hello'`, "hello"},
		{"hello", "hello"},
	}
	for _, tt := range tests {
		if got := stripQuotes(tt.input); got != tt.want {
			t.Errorf("stripQuotes(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestHasSubcommand(t *testing.T) {
	if !hasSubcommand([]string{"npm", "install", "foo"}, "install") {
		t.Error("expected to find install")
	}
	if hasSubcommand([]string{"npm", "run", "build"}, "install") {
		t.Error("expected not to find install")
	}
}

func TestBashRecurse(t *testing.T) {
	if checkSingle(`bash -c "git push"`) == "" {
		t.Error("expected bash -c git push to be blocked")
	}
	if checkSingle(`sh -c "echo hello"`) != "" {
		t.Error("expected sh -c echo hello to be allowed")
	}
}

func TestCheckStaleAllowsMake(t *testing.T) {
	// Simulate stale binary by setting sourceHash to a known-bad value.
	old := sourceHash
	sourceHash = "stale-hash-for-test"
	defer func() { sourceHash = old }()

	root, err := common.FindRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	mk := func(cmd, cwd string) hookInput {
		h := hookInput{ToolName: "Bash", CWD: cwd}
		h.ToolInput.Command = cmd
		return h
	}

	if reason := checkStale(mk("./make", root)); reason != "" {
		t.Errorf("expected ./make at root to be allowed when stale, got: %s", reason)
	}
	if reason := checkStale(mk("./make.exe", root)); reason != "" {
		t.Errorf("expected ./make.exe at root to be allowed when stale, got: %s", reason)
	}
	if reason := checkStale(mk("./make --force", root)); reason != "" {
		t.Errorf("expected './make --force' at root to be allowed when stale, got: %s", reason)
	}
	if reason := checkStale(mk(`.\make.cmd`, root)); reason != "" {
		t.Errorf(`expected .\make.cmd at root to be allowed when stale, got: %s`, reason)
	}
	if reason := checkStale(mk(`.\make.cmd --force`, root)); reason != "" {
		t.Errorf(`expected '.\make.cmd --force' at root to be allowed when stale, got: %s`, reason)
	}

	// `cd <root> && ./make` from a drifted cwd must be allowed.
	if reason := checkStale(mk("cd "+root+" && ./make", filepath.Join(root, "tools", "build"))); reason != "" {
		t.Errorf("expected 'cd root && ./make' to be allowed when stale, got: %s", reason)
	}

	// ./make from outside the repo root must NOT be allowed (e.g. cwd has
	// drifted into a subdirectory that happens to contain a stray ./make).
	if reason := checkStale(mk("./make", filepath.Join(root, "tools", "build"))); reason == "" {
		t.Error("expected ./make from a non-root cwd to be blocked when stale")
	}

	// `cd /tmp && ./make` must NOT be allowed.
	if reason := checkStale(mk("cd /tmp && ./make", root)); reason == "" {
		t.Error("expected 'cd /tmp && ./make' to be blocked when stale")
	}

	if reason := checkStale(mk("git status", root)); reason == "" {
		t.Error("expected non-make command to be blocked when stale")
	}

	// Edit/Write are allowed when stale — gates are loaded from disk at
	// runtime, so the stale binary's enforcement is still correct (T0276).
	editInput := hookInput{ToolName: "Edit"}
	editInput.ToolInput.FilePath = "/tmp/foo.go"
	editInput.ToolInput.NewString = "hello"
	if reason := checkStale(editInput); reason != "" {
		t.Errorf("expected edit to be allowed when stale, got: %s", reason)
	}

	writeInput := hookInput{ToolName: "Write"}
	writeInput.ToolInput.FilePath = "/tmp/bar.go"
	writeInput.ToolInput.Content = "package main"
	if reason := checkStale(writeInput); reason != "" {
		t.Errorf("expected write to be allowed when stale, got: %s", reason)
	}
}

func TestDetectTool(t *testing.T) {
	tests := []struct {
		name  string
		input hookInput
		want  string
	}{
		// Explicit ToolName paths.
		{"bash by name", hookInput{ToolName: "Bash"}, "bash"},
		{"edit by name", hookInput{ToolName: "Edit"}, "edit"},
		{"write by name", hookInput{ToolName: "Write"}, "write"},
		{"skill by name", hookInput{ToolName: "Skill"}, "skill"},

		// Field-based fallback paths.
		{"bash by field", func() hookInput {
			h := hookInput{}
			h.ToolInput.Command = "ls"
			return h
		}(), "bash"},
		{"edit by field", func() hookInput {
			h := hookInput{}
			h.ToolInput.NewString = "x"
			return h
		}(), "edit"},
		{"edit by old_string", func() hookInput {
			h := hookInput{}
			h.ToolInput.OldString = "x"
			return h
		}(), "edit"},
		{"write by field", func() hookInput {
			h := hookInput{}
			h.ToolInput.FilePath = "/tmp/f"
			h.ToolInput.Content = "data"
			return h
		}(), "write"},
		{"skill by field", func() hookInput {
			h := hookInput{}
			h.ToolInput.Skill = "commit"
			return h
		}(), "skill"},
		{"unknown", hookInput{}, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectTool(tt.input); got != tt.want {
				t.Errorf("detectTool() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsRepoMakeChain(t *testing.T) {
	root := "/repo"
	tests := []struct {
		name string
		cmd  string
		cwd  string
		want bool
	}{
		// Allowed: cwd is the repo root and `./make` resolves to <root>/make.
		{"plain ./make at root", "./make", root, true},
		{"./make with args at root", "./make --force", root, true},
		{"./make in pipe at root", "./make 2>&1 | tail", root, true},
		{"./make.exe at root", "./make.exe", root, true},
		// Allowed: cd into root then make.
		{"cd root && ./make", "cd " + root + " && ./make", "/elsewhere", true},
		{"cd root && ./make --force", "cd " + root + " && ./make --force", "/elsewhere", true},
		// Allowed: absolute path to root/make.
		{"abs path", root + "/make", "/elsewhere", true},
		{"abs path with args", root + "/make --force", "/elsewhere", true},

		// Blocked: cwd has drifted, ./make resolves elsewhere.
		{"./make from subdir", "./make", root + "/tools", false},
		{"./make from /tmp", "./make", "/tmp", false},
		// Blocked: cd into a non-root dir, then ./make.
		{"cd /tmp && ./make", "cd /tmp && ./make", root, false},
		{"cd subdir && ./make", "cd " + root + "/tools && ./make", root, false},
		// Blocked: bare 'make' (no ./) is not a recognized invocation.
		{"bare make", "make", root, false},
		// Blocked: a stray ./make that's not the repo's.
		{"./make in /tmp via cd", "cd /tmp && ./make foo", root, false},
		// Blocked: empty cwd — can't verify.
		{"empty cwd", "./make", "", false},
		// Blocked: empty command.
		{"empty cmd", "", root, false},
		// Other commands without make in chain — false.
		{"git status", "git status", root, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRepoMakeChain(tt.cmd, tt.cwd, root)
			if got != tt.want {
				t.Errorf("isRepoMakeChain(%q, %q, %q) = %v, want %v",
					tt.cmd, tt.cwd, root, got, tt.want)
			}
		})
	}
}

func TestCheckStaleFieldBasedDetection(t *testing.T) {
	// Verify Edit/Write are allowed when stale even via field-based detection
	// (no explicit ToolName).
	old := sourceHash
	sourceHash = "stale-hash-for-test"
	defer func() { sourceHash = old }()

	editInput := hookInput{}
	editInput.ToolInput.FilePath = "/tmp/foo.go"
	editInput.ToolInput.OldString = "old"
	editInput.ToolInput.NewString = "new"
	if reason := checkStale(editInput); reason != "" {
		t.Errorf("expected field-detected edit to be allowed when stale, got: %s", reason)
	}

	writeInput := hookInput{}
	writeInput.ToolInput.FilePath = "/tmp/bar.go"
	writeInput.ToolInput.Content = "package main"
	if reason := checkStale(writeInput); reason != "" {
		t.Errorf("expected field-detected write to be allowed when stale, got: %s", reason)
	}

	// Unknown tool should still be blocked.
	unknownInput := hookInput{ToolName: "Read"}
	if reason := checkStale(unknownInput); reason == "" {
		t.Error("expected unknown tool to be blocked when stale")
	}
}

func TestCheckStaleDevHash(t *testing.T) {
	// sourceHash == "dev" means running via go run — skip stale check.
	old := sourceHash
	sourceHash = "dev"
	defer func() { sourceHash = old }()

	input := hookInput{ToolName: "Bash"}
	input.ToolInput.Command = "git status"
	if reason := checkStale(input); reason != "" {
		t.Errorf("expected dev hash to skip stale check, got: %s", reason)
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything.txt", true},
		{"*.pr", "test.pr", true},
		{"*.pr", "test.go", false},
		{"*.go", "main.go", true},
		{"*.go", "main.pr", false},
		{"[invalid", "foo", false}, // invalid glob pattern
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestCheckGo(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []string
		blocked bool
	}{
		{"go test", []string{"go", "test", "./..."}, false},
		{"go run", []string{"go", "run", "main.go"}, false},
		{"go install", []string{"go", "install", "github.com/x"}, true},
		{"go build promise", []string{"go", "build", "-o", "bin/promise", "./cmd/promise"}, true},
		{"go build bin/", []string{"go", "build", "-o", "bin/foo"}, true},
		{"go build ./bin/", []string{"go", "build", "-o", "./bin/foo"}, true},
		{"go build compiler/", []string{"go", "build", "./compiler/"}, true},
		{"go build other", []string{"go", "build", "-o", "/tmp/myapp", "./myapp"}, false},
		{"go alone", []string{"go"}, false},
		{"go build flags only", []string{"go", "build", "-v", "-race"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := checkGo(tt.tokens)
			if tt.blocked && reason == "" {
				t.Error("expected blocked")
			}
			if !tt.blocked && reason != "" {
				t.Errorf("unexpected block: %s", reason)
			}
		})
	}
}

func TestCheckCopy(t *testing.T) {
	tests := []struct {
		name    string
		program string
		tokens  []string
		blocked bool
	}{
		{"cp to /tmp", "cp", []string{"cp", "a.txt", "/tmp/a.txt"}, false},
		{"cp to outside", "cp", []string{"cp", "a.txt", "/etc/a.txt"}, true},
		{"mv to /tmp", "mv", []string{"mv", "a.txt", "/tmp/a.txt"}, false},
		{"mv to outside", "mv", []string{"mv", "a.txt", "/usr/local/a.txt"}, true},
		{"cp no dest", "cp", []string{"cp", "a.txt"}, false},
		{"cp flags only", "cp", []string{"cp", "-r", "a/"}, false},
		{"cp with -t /tmp", "cp", []string{"cp", "-t", "/tmp", "a.txt"}, false},
		{"cp with -t outside", "cp", []string{"cp", "-t", "/etc", "a.txt"}, true},
		{"cp with --target-directory=", "cp", []string{"cp", "--target-directory=/tmp", "a.txt"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := checkCopy(tt.program, tt.tokens)
			if tt.blocked && reason == "" {
				t.Error("expected blocked")
			}
			if !tt.blocked && reason != "" {
				t.Errorf("unexpected block: %s", reason)
			}
		})
	}
}

func TestContextFields(t *testing.T) {
	mk := func(skill, args, cmd, file string) hookInput {
		h := hookInput{}
		h.ToolInput.Skill = skill
		h.ToolInput.Args = args
		h.ToolInput.Command = cmd
		h.ToolInput.FilePath = file
		return h
	}
	tests := []struct {
		name      string
		input     hookInput
		tool      string
		wantKind  string
		wantName  string
		wantInput string
		wantOK    bool
	}{
		{"skill", mk("do", "B0042", "", ""), "skill", "skill", "do", "B0042", true},
		{"bash", mk("", "", "ls -la", ""), "bash", "tool", "Bash", "ls -la", true},
		{"edit", mk("", "", "", "/tmp/foo.go"), "edit", "tool", "Edit", "/tmp/foo.go", true},
		{"write", mk("", "", "", "/tmp/bar.go"), "write", "tool", "Write", "/tmp/bar.go", true},
		{"unknown", mk("", "", "", ""), "unknown", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, name, in, ok := contextFields(tt.input, tt.tool)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if in != tt.wantInput {
				t.Errorf("input = %q, want %q", in, tt.wantInput)
			}
		})
	}
}

func TestIsAllowedCopyDest(t *testing.T) {
	tests := []struct {
		dest string
		want bool
	}{
		{"/tmp", true},
		{"/tmp/foo", true},
		{"/etc/passwd", false},
		{"~/.promise", true},
		{"~/.promise/cache", true},
		{"~/Desktop/foo", false},
	}
	for _, tt := range tests {
		t.Run(tt.dest, func(t *testing.T) {
			if got := isAllowedCopyDest(tt.dest); got != tt.want {
				t.Errorf("isAllowedCopyDest(%q) = %v, want %v", tt.dest, got, tt.want)
			}
		})
	}
}
