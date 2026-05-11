package main

import "testing"

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

	makeInput := hookInput{ToolName: "Bash"}
	makeInput.ToolInput.Command = "./make"
	if reason := checkStale(makeInput); reason != "" {
		t.Errorf("expected ./make to be allowed when stale, got: %s", reason)
	}

	makeExeInput := hookInput{ToolName: "Bash"}
	makeExeInput.ToolInput.Command = "./make.exe"
	if reason := checkStale(makeExeInput); reason != "" {
		t.Errorf("expected ./make.exe to be allowed when stale, got: %s", reason)
	}

	makeArgsInput := hookInput{ToolName: "Bash"}
	makeArgsInput.ToolInput.Command = "./make --force"
	if reason := checkStale(makeArgsInput); reason != "" {
		t.Errorf("expected './make --force' to be allowed when stale, got: %s", reason)
	}

	otherInput := hookInput{ToolName: "Bash"}
	otherInput.ToolInput.Command = "git status"
	if reason := checkStale(otherInput); reason == "" {
		t.Error("expected non-make command to be blocked when stale")
	}

	editInput := hookInput{ToolName: "Edit"}
	editInput.ToolInput.FilePath = "/tmp/foo.go"
	editInput.ToolInput.NewString = "hello"
	if reason := checkStale(editInput); reason == "" {
		t.Error("expected edit to be blocked when stale")
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
