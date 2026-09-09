package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/tools/build/common"
)

// TestMain gives the test binary the build-time root that ./make stamps into
// the shipped guard. Without it the guard cannot scope itself to a project, and
// the checks that ask "is this inside my repository?" would have no repository
// — which is the state a real guard is never in.
func TestMain(m *testing.M) {
	root, err := common.RootForTests()
	if err != nil {
		fmt.Fprintf(os.Stderr, "guard tests: %v\n", err)
		os.Exit(1)
	}
	restore := func() {}
	common.SetRootForTest(cleanupFunc(func(f func()) { restore = f }), root)
	defer restore()
	os.Exit(m.Run())
}

// cleanupFunc adapts a plain function to the Cleanup interface SetRootForTest
// takes, since *testing.M has no Cleanup of its own.
type cleanupFunc func(func())

func (c cleanupFunc) Cleanup(f func()) { c(f) }

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

		// timeout: bare, with kill-after/signal flags, attached forms.
		{"timeout bare", []string{"timeout", "5", "git", "push"}, "git", 2},
		{"timeout -k", []string{"timeout", "-k", "10", "5", "git", "push"}, "git", 2},
		{"timeout --kill-after=", []string{"timeout", "--kill-after=10", "5", "git", "push"}, "git", 2},
		{"timeout --signal", []string{"timeout", "--signal", "KILL", "5", "rm", "-rf", "/"}, "rm", 3},
		{"timeout --preserve-status", []string{"timeout", "--preserve-status", "5", "rm", "-rf", "/"}, "rm", 3},

		// nice: -n form and legacy -N form.
		{"nice -n", []string{"nice", "-n", "10", "rm", "-rf", "/"}, "rm", 3},
		{"nice legacy", []string{"nice", "-10", "rm", "-rf", "/"}, "rm", 3},

		// ionice: attached short flags.
		{"ionice attached", []string{"ionice", "-c2", "-n7", "rm", "-rf", "/"}, "rm", 3},

		// stdbuf: attached mode.
		{"stdbuf -oL", []string{"stdbuf", "-oL", "rm", "-rf", "/"}, "rm", 3},

		// flock: mandatory lock file positional.
		{"flock", []string{"flock", "/tmp/lock", "rm", "-rf", "/"}, "rm", 3},

		// xargs: arg-taking flag.
		{"xargs -n", []string{"xargs", "-n", "1", "rm", "-rf"}, "rm", 2},

		// nohup/setsid: zero-arg / flag-only wrappers.
		{"nohup", []string{"nohup", "rm", "-rf", "/"}, "rm", 3},
		{"setsid", []string{"setsid", "rm", "-rf", "/"}, "rm", 3},

		// chained wrappers.
		{"chained", []string{"env", "FOO=1", "timeout", "5", "nice", "rm", "-rf", "/"}, "rm", 3},

		// sudo/command: flag-taking forms must still reach the wrapped program.
		{"sudo -u", []string{"sudo", "-u", "root", "rm", "-rf", "/"}, "rm", 3},
		{"sudo --user=", []string{"sudo", "--user=root", "rm", "-rf", "/"}, "rm", 3},
		{"sudo -n", []string{"sudo", "-n", "git", "push"}, "git", 2},
		{"command -p", []string{"command", "-p", "rm", "-rf", "/"}, "rm", 3},

		// env: remaining flag/prefix branches not covered above.
		{"env -i", []string{"env", "-i", "git", "push"}, "git", 2},
		{"env --unset", []string{"env", "--unset", "FOO", "git", "push"}, "git", 2},
		{"env --chdir=", []string{"env", "--chdir=/tmp", "git", "push"}, "git", 2},

		// nice: separate --adjustment form and its = variant.
		{"nice --adjustment", []string{"nice", "--adjustment", "10", "rm", "-rf", "/"}, "rm", 3},
		{"nice --adjustment=", []string{"nice", "--adjustment=10", "rm", "-rf", "/"}, "rm", 3},

		// ionice: no-value flag, separate-value flag, and its = variant.
		{"ionice -t", []string{"ionice", "-t", "rm", "-rf", "/"}, "rm", 3},
		{"ionice -c separate", []string{"ionice", "-c", "2", "rm", "-rf", "/"}, "rm", 3},
		{"ionice --class=", []string{"ionice", "--class=2", "rm", "-rf", "/"}, "rm", 3},

		// stdbuf: separate-value flag and its = variant (attached already covered).
		{"stdbuf -i separate", []string{"stdbuf", "-i", "0", "rm", "-rf", "/"}, "rm", 3},
		{"stdbuf --input=", []string{"stdbuf", "--input=0", "rm", "-rf", "/"}, "rm", 3},

		// time: currently untested entirely — bare, no-value flag, separate-value
		// flag, and its = variant.
		{"time bare", []string{"time", "rm", "-rf", "/"}, "rm", 3},
		{"time -p", []string{"time", "-p", "rm", "-rf", "/"}, "rm", 3},
		{"time -o separate", []string{"time", "-o", "/tmp/out", "rm", "-rf", "/"}, "rm", 3},
		{"time --output=", []string{"time", "--output=/tmp/out", "rm", "-rf", "/"}, "rm", 3},

		// setsid: no-value flags — bare setsid above never exercises the switch.
		{"setsid -c", []string{"setsid", "-c", "rm", "-rf", "/"}, "rm", 3},

		// xargs: no-value flag and its long-flag = variant.
		{"xargs -0", []string{"xargs", "-0", "rm", "-rf"}, "rm", 2},
		{"xargs --max-args=", []string{"xargs", "--max-args=1", "rm", "-rf"}, "rm", 2},

		// flock: no-value flag and separate-value flag plus its = variant.
		{"flock -n", []string{"flock", "-n", "/tmp/lock", "rm", "-rf", "/"}, "rm", 3},
		{"flock -w separate", []string{"flock", "-w", "10", "/tmp/lock", "rm", "-rf", "/"}, "rm", 3},
		{"flock --timeout=", []string{"flock", "--timeout=10", "/tmp/lock", "rm", "-rf", "/"}, "rm", 3},

		// Wrapper consumes every token (only its own flags, no wrapped
		// program) — the loop exits via i>=len(args) rather than hitting a
		// non-dash token. Nothing to dispatch on, so this is allowed (empty
		// remainder), not denied.
		{"command flags only, no program", []string{"command", "-p"}, "", 0},
		{"sudo flags only, no program", []string{"sudo", "-v"}, "", 0},
		{"env flags only, no program", []string{"env", "-i"}, "", 0},
		{"nice flags only, no program", []string{"nice", "-n", "5"}, "", 0},
		{"ionice flags only, no program", []string{"ionice", "-t"}, "", 0},
		{"stdbuf flags only, no program", []string{"stdbuf", "-oL"}, "", 0},
		{"time flags only, no program", []string{"time", "-p"}, "", 0},
		{"setsid flags only, no program", []string{"setsid", "-w"}, "", 0},
		{"xargs flags only, no program", []string{"xargs", "-0"}, "", 0},
	}
	for _, tt := range tests {
		got, denyReason := stripWrappers(tt.tokens)
		if denyReason != "" {
			t.Errorf("%s: unexpected deny: %s", tt.name, denyReason)
			continue
		}
		if len(got) != tt.wantN {
			t.Errorf("%s: len=%d (%v), want %d", tt.name, len(got), got, tt.wantN)
		}
		if len(got) > 0 && got[0] != tt.want0 {
			t.Errorf("%s: [0]=%q, want %q", tt.name, got[0], tt.want0)
		}
	}
}

// TestStripWrappersFailClosed covers known wrappers used with a flag shape
// stripWrappers doesn't model — these must deny, not silently pass the
// wrapper name through unstripped (T1624).
func TestStripWrappersFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		tokens []string
	}{
		{"timeout unrecognized flag", []string{"timeout", "--made-up-flag", "5", "rm", "-rf", "/"}},
		{"timeout no duration", []string{"timeout", "rm", "-rf", "/"}},
		{"flock -c opaque command", []string{"flock", "-c", "rm -rf /", "/tmp/lock"}},
		{"flock no lockfile", []string{"flock"}},
		{"env unrecognized flag", []string{"env", "--bogus", "git", "push"}},
		{"nice unrecognized flag", []string{"nice", "--bogus", "rm", "-rf", "/"}},
		{"sudo -e sudoedit", []string{"sudo", "-e", "/etc/passwd"}},
		{"sudo -l list", []string{"sudo", "-l", "rm", "-rf", "/"}},
		{"sudo -h ambiguous", []string{"sudo", "-h", "rm", "-rf", "/"}},
		{"sudo -u no value", []string{"sudo", "-u"}},
		{"command unrecognized flag", []string{"command", "--bogus", "rm", "-rf", "/"}},
		{"env -u no value", []string{"env", "-u"}},
		{"nice -n no value", []string{"nice", "-n"}},
		{"ionice -c no value", []string{"ionice", "-c"}},
		{"ionice unrecognized flag", []string{"ionice", "--bogus", "rm", "-rf", "/"}},
		{"stdbuf -i no value", []string{"stdbuf", "-i"}},
		{"stdbuf unrecognized flag", []string{"stdbuf", "--bogus", "rm", "-rf", "/"}},
		{"time -o no value", []string{"time", "-o"}},
		{"time unrecognized flag", []string{"time", "--bogus", "rm", "-rf", "/"}},
		{"setsid unrecognized flag", []string{"setsid", "--bogus", "rm", "-rf", "/"}},
		{"xargs -n no value", []string{"xargs", "-n"}},
		{"xargs unrecognized flag", []string{"xargs", "--bogus", "rm", "-rf"}},
		{"flock -w no value", []string{"flock", "-w"}},
		{"flock unrecognized flag", []string{"flock", "--bogus", "/tmp/lock"}},
		{"timeout -k no value", []string{"timeout", "-k"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, denyReason := stripWrappers(tt.tokens)
			if denyReason == "" {
				t.Errorf("expected a deny reason for %v", tt.tokens)
			}
		})
	}
}

// TestCheckSingleWrapperDenyPropagates covers the checkSingle integration
// point (main.go:499-501): a malformed known-wrapper invocation must be
// blocked by checkSingle itself, with stripWrappers's deny reason surfaced,
// not just by stripWrappers in isolation (T1624).
func TestCheckSingleWrapperDenyPropagates(t *testing.T) {
	reason := checkSingle("timeout --made-up-flag 5 rm -rf /", "")
	if reason == "" {
		t.Fatal("expected checkSingle to block a malformed timeout invocation")
	}
	if !strings.Contains(reason, "unrecognized option for wrapper") {
		t.Errorf("reason = %q, want it to mention the unrecognized wrapper option", reason)
	}

	// A wrapper invocation that consumes every token (only its own flags,
	// no wrapped program) leaves stripWrappers's remainder empty — nothing
	// to dispatch on, so checkSingle must allow it (main.go:503-505).
	if reason := checkSingle("command -p", ""); reason != "" {
		t.Errorf("expected wrapper-only invocation with no program to be allowed, got %q", reason)
	}
}

// TestWrapperBypassCoverage pairs every existing deny rule with a wrapped
// variant, so a future rule cannot be added without wrapper coverage (T1624).
func TestWrapperBypassCoverage(t *testing.T) {
	blocked := []string{
		"git push",
		"git reset --hard HEAD",
		"rm -rf /tmp/x",
		"curl http://x",
		"go build ./cmd/promise/",
		"npm install foo",
	}
	wrappers := []string{
		"timeout 5 %s",
		"nice %s",
		"env FOO=1 timeout 5 %s",
		"nohup %s",
		"setsid %s",
		"stdbuf -oL %s",
		"sudo -u root %s",
		"command -p %s",
	}
	for _, cmd := range blocked {
		for _, wrapper := range wrappers {
			wrapped := fmt.Sprintf(wrapper, cmd)
			t.Run(wrapped, func(t *testing.T) {
				if checkSingle(wrapped, "") == "" {
					t.Errorf("expected wrapped command %q to be blocked", wrapped)
				}
			})
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
		reason := checkGit(tt.tokens, "")
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
		"timeout 5 rm -rf /tmp/x",
		"timeout 300 go build ./cmd/promise/",
		"nice -n 10 git push",
	}
	for _, cmd := range blocked {
		if checkSingle(cmd, "") == "" {
			t.Errorf("expected %q to be blocked", cmd)
		}
	}

	allowed := []string{
		"git status",
		"ls -la",
		"go test ./...",
		"rm file.txt",
		"timeout 5 git status",
	}
	for _, cmd := range allowed {
		if reason := checkSingle(cmd, ""); reason != "" {
			t.Errorf("expected %q to be allowed, got: %s", cmd, reason)
		}
	}
}

func TestCheckAll(t *testing.T) {
	if checkAll("echo hi && git push", "") == "" {
		t.Error("expected chain with git push to be blocked")
	}
	if checkAll("git status && echo ok", "") != "" {
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
	if checkSingle(`bash -c "git push"`, "") == "" {
		t.Error("expected bash -c git push to be blocked")
	}
	if checkSingle(`sh -c "echo hello"`, "") != "" {
		t.Error("expected sh -c echo hello to be allowed")
	}
}

func TestCheckStaleAllowsMake(t *testing.T) {
	// Simulate stale binary by setting sourceHash to a known-bad value.
	old := sourceHash
	sourceHash = "stale-hash-for-test"
	defer func() { sourceHash = old }()

	root, err := common.RootForTests()
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

	// Wrapped forms at the root: rebuilding is rarely the whole command, and
	// refusing the wrapper leaves the named remedy as the only accepted
	// spelling of it (T1813).
	if reason := checkStale(mk("./make && go test ./...", root)); reason != "" {
		t.Errorf("expected './make && go test ./...' to be allowed when stale, got: %s", reason)
	}
	if reason := checkStale(mk("./make 2>&1 | tail", root)); reason != "" {
		t.Errorf("expected './make 2>&1 | tail' to be allowed when stale, got: %s", reason)
	}

	// Wrapping only lifts the *stale* block. Every sub-command still faces the
	// ordinary per-command checks, so a rebuild prefix is not a way to smuggle
	// a blocked command through.
	if reason := checkStale(mk("./make && git push", root)); reason != "" {
		t.Errorf("expected './make && git push' to clear the stale gate, got: %s", reason)
	}
	if checkAll("./make && git push", root) == "" {
		t.Error("expected './make && git push' to be blocked by the per-command checks")
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
		{"task by name", hookInput{ToolName: "Task"}, "task"},
		{"agent by name", hookInput{ToolName: "Agent"}, "agent"},
		{"mcp by name", hookInput{ToolName: "mcp__tracker__gate_run"}, "mcp"},

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

func TestCheckGateContext(t *testing.T) {
	skillInput := hookInput{ToolName: "Skill"}
	taskInput := hookInput{ToolName: "Task"}
	agentInput := hookInput{ToolName: "Agent"}
	mcpInput := hookInput{ToolName: "mcp__tracker__gate_run"}
	bashInput := func(cmd string) hookInput {
		h := hookInput{ToolName: "Bash"}
		h.ToolInput.Command = cmd
		return h
	}

	tests := []struct {
		name     string
		tool     string
		input    hookInput
		wantDeny bool
	}{
		{"skill denied", "skill", skillInput, true},
		{"task denied", "task", taskInput, true},
		{"agent denied", "agent", agentInput, true},
		{"mcp denied", "mcp", mcpInput, true},
		{"bash claude -p denied", "bash", bashInput("claude -p 'do something'"), true},
		{"bash bin/do denied", "bash", bashInput("bin/do resolve T1"), true},
		{"bash ./bin/flow denied", "bash", bashInput("./bin/flow status"), true},
		{"bash wrapped claude denied", "bash", bashInput("timeout 5s claude -p hi"), true},
		{"bash chained claude denied", "bash", bashInput("echo hi && claude -p hi"), true},
		{"bash unrelated allowed", "bash", bashInput("go test ./..."), false},
		{"bash -c wrapping claude denied", "bash", bashInput(`bash -c "claude -p hi"`), true},
		{"sh -c wrapping bin/do denied", "bash", bashInput(`sh -c "bin/do resolve T1"`), true},
		{"bash -c unrelated allowed", "bash", bashInput(`bash -c "go test ./..."`), false},
		{"bash wrapper deny reason propagates", "bash", bashInput("sudo -e claude -p hi"), true},
		{"bash empty segment then claude denied", "bash", bashInput("true &&  && claude -p hi"), true},
		{"bash wrapper with no trailing program allowed", "bash", bashInput("nohup"), false},
		{"bash empty command allowed", "bash", bashInput(""), false},
		{"edit not blocked by gate context", "edit", hookInput{ToolName: "Edit"}, false},
		{"write not blocked by gate context", "write", hookInput{ToolName: "Write"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkGateContext(tt.tool, tt.input)
			if (got != "") != tt.wantDeny {
				t.Errorf("checkGateContext(%q, ...) = %q, wantDeny=%v", tt.tool, got, tt.wantDeny)
			}
		})
	}
}

func TestIsAgentEntryPointProgram(t *testing.T) {
	tests := []struct {
		program string
		want    bool
	}{
		{"claude", true},
		{"/usr/local/bin/claude", true},
		{"bin/do", true},
		{"./bin/do", true},
		{"bin/flow", true},
		{"./bin/flow", true},
		{"do", false},
		{"flow", false},
		{"sudo", false},
		{"go", false},
		{"claudex", false},
	}
	for _, tt := range tests {
		t.Run(tt.program, func(t *testing.T) {
			if got := isAgentEntryPointProgram(tt.program); got != tt.want {
				t.Errorf("isAgentEntryPointProgram(%q) = %v, want %v", tt.program, got, tt.want)
			}
		})
	}
}

// TestGateContextOnlyAppliesWhenMarkerSet is the regression test proving the
// PROMISE_GATE dispatch in main() only fires when the marker is actually set
// — main() calls checkGateContext only inside `if gateContextActive() {...}`,
// so a Skill/Task/Agent/mcp call outside a gate run must reach its ordinary
// (allowing) path, not the gate-context deny.
func TestGateContextEnvGating(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv("PROMISE_GATE", "")
		os.Unsetenv("PROMISE_GATE")
		if gateContextActive() {
			t.Fatal("gateContextActive() = true with PROMISE_GATE unset")
		}
	})
	t.Run("set", func(t *testing.T) {
		t.Setenv("PROMISE_GATE", "1")
		if !gateContextActive() {
			t.Fatal("gateContextActive() = false with PROMISE_GATE=1")
		}
	})
}

// TestGateContextPrecedesSkillShortcut is a regression test for the dispatch
// order in main(): the gate-context check must run before the "skill"
// shortcut that would otherwise allow every Skill call unconditionally. This
// exercises checkGateContext directly rather than main() (which reads
// os.Stdin), asserting the same property main() relies on: a Skill tool is
// denied by checkGateContext, so main()'s ordering (checkGateContext before
// the skill shortcut) is what makes gate-context enforcement effective.
func TestGateContextPrecedesSkillShortcut(t *testing.T) {
	reason := checkGateContext("skill", hookInput{ToolName: "Skill"})
	if reason == "" {
		t.Fatal("checkGateContext(\"skill\", ...) allowed a Skill call — the skill shortcut in main() would let this through unconditionally if this check didn't run first")
	}
}

// TestSettingsJSONRoutesPromptToolsThroughGuard is the regression test for a
// gap that let checkGateContext's Task/Agent handling ship as dead code:
// checkGateContext only ever runs when Claude Code invokes bin/guard as a
// PreToolUse hook in the first place, and that only happens for a tool name
// matched by some PreToolUse entry's "matcher" regex in .claude/settings.json.
// Adding the "agent"/"task" cases to detectTool/checkGateContext (T1877) did
// nothing by itself, because the matcher shipped as "Skill|Task|mcp__.*" —
// missing "Agent", the name this environment's subagent-dispatch tool
// actually carries per detectTool's own comment. This parses the committed
// settings.json and asserts every prompt-invoking tool name bin/guard knows
// about is covered by at least one fail-closed ("|| exit 2") PreToolUse entry.
func TestSettingsJSONRoutesPromptToolsThroughGuard(t *testing.T) {
	root, err := common.FindRoot()
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}

	var parsed struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}

	// Every tool name checkGateContext can deny must be matched by some
	// fail-closed ("|| exit 2") PreToolUse entry — otherwise the guard binary
	// is never invoked for that tool and checkGateContext's handling of it is
	// unreachable.
	promptTools := []string{"Skill", "Task", "Agent", "mcp__tracker__gate_run"}
	for _, name := range promptTools {
		matched := false
		for _, entry := range parsed.Hooks.PreToolUse {
			re, err := regexp.Compile(entry.Matcher)
			if err != nil {
				t.Fatalf("PreToolUse matcher %q: %v", entry.Matcher, err)
			}
			if !re.MatchString(name) {
				continue
			}
			for _, h := range entry.Hooks {
				if strings.Contains(h.Command, "bin/guard") && strings.Contains(h.Command, "exit 2") {
					matched = true
				}
			}
		}
		if !matched {
			t.Errorf("no fail-closed PreToolUse entry in .claude/settings.json matches tool %q — bin/guard is never invoked for it, so gate-context enforcement (T1877) can't reach it", name)
		}
	}
}

func TestIsRepoMakeChain(t *testing.T) {
	// Use real, platform-native absolute dirs so the test holds on Windows
	// (where POSIX-absolute literals like "/repo" are not absolute).
	root := t.TempDir()
	elsewhere := t.TempDir()
	other := t.TempDir()
	sub := filepath.Join(root, "tools")
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
		{"cd root && ./make", "cd " + root + " && ./make", elsewhere, true},
		{"cd root && ./make --force", "cd " + root + " && ./make --force", elsewhere, true},
		// Allowed: absolute path to root/make.
		{"abs path", filepath.Join(root, "make"), elsewhere, true},
		{"abs path with args", filepath.Join(root, "make") + " --force", elsewhere, true},

		// Blocked: cwd has drifted, ./make resolves elsewhere.
		{"./make from subdir", "./make", sub, false},
		{"./make from other", "./make", other, false},
		// Blocked: cd into a non-root dir, then ./make.
		{"cd other && ./make", "cd " + other + " && ./make", root, false},
		{"cd subdir && ./make", "cd " + sub + " && ./make", root, false},
		// Blocked: bare 'make' (no ./) is not a recognized invocation.
		{"bare make", "make", root, false},
		// Blocked: a stray ./make that's not the repo's.
		{"./make in other via cd", "cd " + other + " && ./make foo", root, false},
		// Allowed: an absolute path to the repo's make script needs no cwd to
		// resolve, so an unknown cwd does not disqualify it (T1813).
		{"abs path, empty cwd", filepath.Join(root, "make"), "", true},
		{"abs path from foreign root", filepath.Join(root, "make"), other, true},
		{"cd root && ./make, empty cwd", "cd " + root + " && ./make", "", true},

		// Blocked: relative forms with an unknown cwd — nothing to resolve against.
		{"empty cwd", "./make", "", false},
		{"relative cd, empty cwd", "cd tools && ./make", "", false},
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

// TestCheckStaleFromForeignRoot is the T1813 scenario: the session's cwd is a
// scratch directory that itself looks like a Promise repo (it has a
// catalog.toml). The staleness verdict must still be about the repo the guard
// was built for, and — the load-bearing part — the recovery command the deny
// message names must be one this same binary accepts from that cwd. A message
// naming a command the guard then refuses is what wedged the session.
func TestCheckStaleFromForeignRoot(t *testing.T) {
	old := sourceHash
	sourceHash = "stale-hash-for-test"
	defer func() { sourceHash = old }()

	root, err := common.RootForTests()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	// A scratch tree that a cwd walk would mistake for the repository:
	// catalog.toml marker, an empty tools tree, a bin/.
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "catalog.toml"), []byte("# scratch\n"), 0o644); err != nil {
		t.Fatalf("write scratch catalog.toml: %v", err)
	}
	for _, dir := range []string{filepath.Join("tools", "build", "cmd"), "bin"} {
		if err := os.MkdirAll(filepath.Join(scratch, dir), 0o755); err != nil {
			t.Fatalf("mkdir scratch %s: %v", dir, err)
		}
	}

	// Put the process itself in that tree, not just the hook input. The
	// original failure came from the hook resolving its root by walking up
	// from its own cwd, which would find this scratch dir and then hash an
	// empty tools tree — permanently "stale", about the wrong repository.
	// Without the chdir the fixture above is decoration and a reintroduced
	// cwd walk would sail past this test.
	t.Chdir(scratch)

	mk := func(cmd string) hookInput {
		h := hookInput{ToolName: "Bash", CWD: scratch}
		h.ToolInput.Command = cmd
		return h
	}

	if reason := checkStale(mk("cd " + root + " && ./make")); reason != "" {
		t.Errorf("expected 'cd root && ./make' from a foreign root to be allowed, got: %s", reason)
	}
	if reason := checkStale(mk("./make")); reason == "" {
		t.Error("expected bare ./make inside the scratch dir to be blocked — it is not the repo's make")
	}

	reason := checkStale(mk("pwd"))
	if reason == "" {
		t.Fatal("expected an ordinary command to be blocked when stale")
	}

	// The message must name a cwd-independent remedy, and that remedy must be
	// one this same binary accepts from here. The spelling comes from
	// common.MakeCommands rather than being written out again, so a change on
	// the suggesting side that the accepting side does not follow fails here.
	// Matching the whole path rather than scanning tokens also keeps the test
	// working for a checkout whose path contains a space.
	absMake, _ := common.MakeCommands(root)
	if !strings.Contains(reason, absMake) {
		t.Fatalf("stale deny message names no cwd-independent remedy (%s): %s", absMake, reason)
	}
	if !isRepoMakeChain(absMake, scratch, root) {
		t.Fatalf("remedy %q named by the stale message is not one the guard accepts from %s", absMake, scratch)
	}
	if r := checkAll(absMake, scratch); r != "" {
		t.Errorf("recovery command %q named by the stale message is itself blocked: %s", absMake, r)
	}
}

// TestApplyCdUnknownCwd pins the contract both chain walkers share: a relative
// `cd` from an unknown cwd leaves it unknown. Joining against "" would name a
// directory relative to wherever the guard happens to be running, and git
// scoping would then read a wrong-directory answer as "outside the project"
// and waive branch hygiene.
func TestApplyCdUnknownCwd(t *testing.T) {
	tests := []struct {
		name   string
		tokens []string
		cwd    string
		want   string
		wantCd bool
	}{
		{"relative cd, unknown cwd", []string{"cd", "tools"}, "", "", true},
		{"absolute cd, unknown cwd", []string{"cd", "/tmp/x"}, "", filepath.Clean("/tmp/x"), true},
		{"relative cd, known cwd", []string{"cd", "tools"}, "/repo", filepath.Join("/repo", "tools"), true},
		{"not a cd", []string{"ls", "-l"}, "/repo", "/repo", false},
		{"bare cd", []string{"cd"}, "/repo", "/repo", false},
	}
	for _, tt := range tests {
		got, isCd := applyCd(tt.tokens, tt.cwd)
		if got != tt.want || isCd != tt.wantCd {
			t.Errorf("%s: applyCd(%v, %q) = (%q, %v), want (%q, %v)",
				tt.name, tt.tokens, tt.cwd, got, isCd, tt.want, tt.wantCd)
		}
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
// The stale edit/write hint is advice printed to the agent rather than a deny
// reason, so it is only observable this way.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = saved
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read end: %v", err)
	}
	return string(out)
}

// TestStaleMessageSpellingsAreAccepted holds the deny message to its own words.
// It offers two rebuild spellings and says where each applies; both claims are
// checked against the function that decides what the stale guard lets through,
// so neither the wording nor the acceptance can drift alone (T1813).
func TestStaleMessageSpellingsAreAccepted(t *testing.T) {
	old := sourceHash
	sourceHash = "stale-hash-for-test"
	defer func() { sourceHash = old }()

	root, err := common.RootForTests()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	absMake, relativeMake := common.MakeCommands(root)

	input := hookInput{ToolName: "Bash", CWD: filepath.Join(root, "tools", "build")}
	input.ToolInput.Command = "go test ./..."
	reason := checkStale(input)
	if reason == "" {
		t.Fatal("expected an ordinary command to be denied when stale")
	}
	if !strings.Contains(reason, absMake) || !strings.Contains(reason, relativeMake) {
		t.Fatalf("stale message names neither spelling (%s / %s): %s", absMake, relativeMake, reason)
	}

	// The absolute spelling is offered unconditionally, so it must be accepted
	// from anywhere — including a cwd the hook input never reported.
	for _, cwd := range []string{root, filepath.Join(root, "tools", "build"), t.TempDir(), ""} {
		if !isRepoMakeChain(absMake, cwd, root) {
			t.Errorf("absolute remedy %q from the stale message is refused with cwd %q", absMake, cwd)
		}
	}

	// The relative spelling is offered "from the repo root", and that
	// qualification is the whole of its contract: accepted there, refused
	// elsewhere, where it would name some other directory's script.
	if !isRepoMakeChain(relativeMake, root, root) {
		t.Errorf("relative remedy %q is refused at the repo root, where the message says to run it", relativeMake)
	}
	if isRepoMakeChain(relativeMake, filepath.Join(root, "tools"), root) {
		t.Errorf("relative remedy %q was accepted outside the repo root", relativeMake)
	}
}

// TestCheckStaleEditHintNamesAcceptedCommand: Edit/Write stay allowed while
// stale, and the hint printed alongside them names a rebuild command the same
// binary accepts — the identical trap as the deny message, in the branch that
// does not deny. The edit that fixes tools code is typically made from a cwd
// that is not the repo root, so a bare ./make would be wrong advice there.
func TestCheckStaleEditHintNamesAcceptedCommand(t *testing.T) {
	old := sourceHash
	sourceHash = "stale-hash-for-test"
	defer func() { sourceHash = old }()

	root, err := common.RootForTests()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	absMake, _ := common.MakeCommands(root)
	elsewhere := t.TempDir()

	for _, tool := range []string{"Edit", "Write"} {
		input := hookInput{ToolName: tool, CWD: elsewhere}
		input.ToolInput.FilePath = filepath.Join(root, "tools", "build", "common", "stale.go")
		input.ToolInput.NewString = "package common"
		input.ToolInput.Content = "package common"

		var reason string
		hint := captureStderr(t, func() { reason = checkStale(input) })
		if reason != "" {
			t.Fatalf("%s must stay allowed while stale, got: %s", tool, reason)
		}
		if !strings.Contains(hint, absMake) {
			t.Errorf("%s hint names no cwd-independent rebuild (%s): %s", tool, absMake, hint)
		}
		if !isRepoMakeChain(absMake, elsewhere, root) {
			t.Errorf("%s hint names %q, which the guard refuses from %s", tool, absMake, elsewhere)
		}
	}
}

// TestCheckStaleFreshBinaryAllows: the gate is about staleness and nothing
// else. With the compiled hash matching the tree, every tool goes through
// untouched — this is the path every real invocation takes.
func TestCheckStaleFreshBinaryAllows(t *testing.T) {
	root, err := common.RootForTests()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	hash, err := common.ToolsSourceHash(root)
	if err != nil {
		t.Fatalf("hash tools source: %v", err)
	}
	old := sourceHash
	sourceHash = hash
	defer func() { sourceHash = old }()

	bash := hookInput{ToolName: "Bash", CWD: t.TempDir()}
	bash.ToolInput.Command = "go test ./..."
	if reason := checkStale(bash); reason != "" {
		t.Errorf("a fresh guard must not block anything, got: %s", reason)
	}
	if reason := checkStale(hookInput{ToolName: "Read"}); reason != "" {
		t.Errorf("a fresh guard must not block a Read, got: %s", reason)
	}
}

// TestCheckStaleFailsOpenWhenRootUnusable: when the guard cannot work out
// which tree to hash, it has no staleness verdict to give and must not invent
// one. Denying instead would be the T1813 wedge in its purest form — every
// command refused over a comparison that never happened, and the rebuild the
// message named unable to fix it.
func TestCheckStaleFailsOpenWhenRootUnusable(t *testing.T) {
	old := sourceHash
	sourceHash = "stale-hash-for-test"
	defer func() { sourceHash = old }()

	// A repo-shaped scratch tree with no tools/build to hash — the exact shape
	// of the directory that wedged the session, reachable now only by stamping
	// it in, since the root no longer comes from the working directory.
	emptyRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(emptyRepo, "catalog.toml"), nil, 0o644); err != nil {
		t.Fatalf("write scratch catalog.toml: %v", err)
	}

	cases := []struct {
		name string
		root string
	}{
		{"stamped root is not a repo", filepath.Join(t.TempDir(), "gone")},
		{"stamped root has no tools tree", emptyRepo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			common.SetRootForTest(t, tc.root)

			input := hookInput{ToolName: "Bash", CWD: tc.root}
			input.ToolInput.Command = "pwd"
			if reason := checkStale(input); reason != "" {
				t.Errorf("expected no staleness verdict when the root is unusable, got: %s", reason)
			}
		})
	}
}

// TestCheckAllUnknownCwdKeepsGitHygiene is applyCd's other caller. A relative
// cd from an unknown cwd must leave it unknown rather than resolve against
// wherever the guard is running: a wrong-directory answer would read as "this
// git repo is outside the project" and quietly waive branch hygiene.
func TestCheckAllUnknownCwdKeepsGitHygiene(t *testing.T) {
	if reason := checkAll("cd tools && git switch -c feature", ""); reason == "" {
		t.Error("branch creation was allowed after a relative cd from an unknown cwd")
	}
	if reason := checkAll("cd tools && git push", ""); reason == "" {
		t.Error("push was allowed after a relative cd from an unknown cwd")
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
	tmp := os.TempDir()
	tests := []struct {
		name    string
		program string
		tokens  []string
		blocked bool
	}{
		{"cp to tmp", "cp", []string{"cp", "a.txt", filepath.Join(tmp, "a.txt")}, false},
		{"cp to outside", "cp", []string{"cp", "a.txt", "/etc/a.txt"}, true},
		{"mv to tmp", "mv", []string{"mv", "a.txt", filepath.Join(tmp, "a.txt")}, false},
		{"mv to outside", "mv", []string{"mv", "a.txt", "/usr/local/a.txt"}, true},
		{"cp no dest", "cp", []string{"cp", "a.txt"}, false},
		{"cp flags only", "cp", []string{"cp", "-r", "a/"}, false},
		{"cp with -t tmp", "cp", []string{"cp", "-t", tmp, "a.txt"}, false},
		{"cp with -t outside", "cp", []string{"cp", "-t", "/etc", "a.txt"}, true},
		{"cp with --target-directory=", "cp", []string{"cp", "--target-directory=" + tmp, "a.txt"}, false},
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
	tmp := os.TempDir()
	tests := []struct {
		dest string
		want bool
	}{
		{tmp, true},
		{filepath.Join(tmp, "foo"), true},
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

// TestIsAllowedCopyDestCwd covers the repo-directory (cwd) branch: a relative
// destination resolves under the current working directory and is allowed.
func TestIsAllowedCopyDestCwd(t *testing.T) {
	if !isAllowedCopyDest("guard_cwd_dest.txt") {
		t.Errorf("isAllowedCopyDest(relative path under cwd) = false, want true")
	}
}

// TestIsAllowedCopyDestTmpFallback covers the POSIX-only /tmp fallback: even
// when $TMPDIR (and thus os.TempDir()) points elsewhere, a literal /tmp
// destination is still accepted. On Windows /tmp is not special, so the
// fallback is skipped and the path is blocked.
func TestIsAllowedCopyDestTmpFallback(t *testing.T) {
	// Point os.TempDir() away from /tmp so the first allow-check misses and
	// the /tmp fallback branch is exercised.
	t.Setenv("TMPDIR", t.TempDir())

	got := isAllowedCopyDest("/tmp/promise_guard_fallback")
	want := runtime.GOOS != "windows"
	if got != want {
		t.Errorf("isAllowedCopyDest(/tmp/...) = %v, want %v (GOOS=%s)", got, want, runtime.GOOS)
	}
}

// TestCheckGitBranchTargets covers the string-level branch-hygiene decisions
// that short-circuit before any git invocation (creation flags, `--`, `-`,
// `main`, path-like targets, branch listing/deletion). The ancestry- and
// branch-resolution paths that shell out to git are covered by
// TestCheckGitCheckoutAncestry against a real repo.
func TestCheckGitBranchTargets(t *testing.T) {
	tests := []struct {
		name    string
		fn      func([]string, string) string
		tokens  []string
		blocked bool
	}{
		// switch: creation and non-main switching blocked; main allowed.
		{"switch -c", checkGitSwitch, []string{"git", "switch", "-c", "foo"}, true},
		{"switch --create", checkGitSwitch, []string{"git", "switch", "--create", "foo"}, true},
		{"switch --orphan", checkGitSwitch, []string{"git", "switch", "--orphan", "foo"}, true},
		{"switch -", checkGitSwitch, []string{"git", "switch", "-"}, true},
		{"switch main", checkGitSwitch, []string{"git", "switch", "main"}, false},
		{"switch origin/main", checkGitSwitch, []string{"git", "switch", "origin/main"}, false},
		// checkout: branch creation blocked; file/main/path forms allowed.
		{"checkout -b", checkGitCheckout, []string{"git", "checkout", "-b", "foo"}, true},
		{"checkout -B", checkGitCheckout, []string{"git", "checkout", "-B", "foo"}, true},
		{"checkout --orphan", checkGitCheckout, []string{"git", "checkout", "--orphan", "foo"}, true},
		{"checkout -", checkGitCheckout, []string{"git", "checkout", "-"}, true},
		{"checkout main", checkGitCheckout, []string{"git", "checkout", "main"}, false},
		{"checkout -- file", checkGitCheckout, []string{"git", "checkout", "--", "f.txt"}, false},
		{"checkout main -- file", checkGitCheckout, []string{"git", "checkout", "main", "--", "f.txt"}, false},
		{"checkout path-like", checkGitCheckout, []string{"git", "checkout", "src/main.go"}, false},
		{"checkout dotted path", checkGitCheckout, []string{"git", "checkout", "file.txt"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := tt.fn(tt.tokens, "")
			if tt.blocked && reason == "" {
				t.Errorf("expected blocked, got allowed")
			}
			if !tt.blocked && reason != "" {
				t.Errorf("expected allowed, got: %s", reason)
			}
		})
	}
}

// TestCheckGitBranchCreate covers `git branch`: listing/deletion/move are
// allowed, creating a non-main branch is blocked, (force-)moving main is
// allowed. None of these shell out to git.
func TestCheckGitBranchCreate(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []string
		blocked bool
	}{
		{"list", []string{"git", "branch"}, false},
		{"list -a", []string{"git", "branch", "-a"}, false},
		{"list -v", []string{"git", "branch", "-v"}, false},
		{"delete", []string{"git", "branch", "-d", "foo"}, false},
		{"force delete", []string{"git", "branch", "-D", "foo"}, false},
		{"rename", []string{"git", "branch", "-m", "a", "b"}, false},
		{"show-current", []string{"git", "branch", "--show-current"}, false},
		{"set main upstream", []string{"git", "branch", "-f", "main", "origin/main"}, false},
		{"create non-main", []string{"git", "branch", "feature"}, true},
		{"create main", []string{"git", "branch", "main"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := checkGitBranch(tt.tokens)
			if tt.blocked && reason == "" {
				t.Errorf("expected blocked, got allowed")
			}
			if !tt.blocked && reason != "" {
				t.Errorf("expected allowed, got: %s", reason)
			}
		})
	}
}

func TestEffectiveGitDir(t *testing.T) {
	cwd := filepath.Clean(string(filepath.Separator) + "repo") // "/repo" or "\repo"
	absArg, err := filepath.Abs(filepath.Join("abs", "sub"))   // platform-absolute
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		tokens []string
		want   string
	}{
		{[]string{"git", "status"}, cwd},
		{[]string{"git", "-C", "sub", "status"}, filepath.Join(cwd, "sub")},
		{[]string{"git", "-C", absArg, "status"}, absArg},
		{[]string{"git", "-c", "k=v", "checkout", "main"}, cwd},
		{[]string{"git", "--git-dir", "x", "status"}, cwd},
		// Generic flags (not -C/-c/--git-dir/--work-tree) are skipped without
		// consuming an argument, leaving the dir unchanged.
		{[]string{"git", "--no-pager", "status"}, cwd},
		{[]string{"git", "-C", "sub", "--no-pager", "status"}, filepath.Join(cwd, "sub")},
	}
	for _, tt := range tests {
		if got := effectiveGitDir(tt.tokens, cwd); got != tt.want {
			t.Errorf("effectiveGitDir(%v, %q) = %q, want %q", tt.tokens, cwd, got, tt.want)
		}
	}
}

func TestInManagedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root, err := common.RootForTests()
	if err != nil {
		t.Fatalf("find root: %v", err)
	}
	if !inManagedRepo(root) {
		t.Errorf("expected repo root %q to be managed", root)
	}
	if !inManagedRepo(filepath.Join(root, "tools", "build")) {
		t.Errorf("expected a subdir of the repo to be managed")
	}
	// A fresh non-git temp dir outside the project tree is exempt.
	// Reset TMPDIR before calling t.TempDir() because bin/verify sets it to
	// .promise-home/tmp/ (inside the repo), which would make t.TempDir() return
	// a path that inManagedRepo() correctly identifies as inside the repo.
	t.Setenv("TMPDIR", "")
	if tmp := t.TempDir(); inManagedRepo(tmp) {
		t.Errorf("expected non-repo temp dir %q to be exempt", tmp)
	}
}

// TestCheckGitCheckoutAncestry exercises the git-backed paths (local-branch
// detection and origin/HEAD ancestry) against a real repo with a remote: an
// ancestor commit may be detached onto, a commit ahead of origin/HEAD may not,
// a non-main branch switch is blocked, and file checkouts pass through.
func TestCheckGitCheckoutAncestry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	remote := t.TempDir()

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if out, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	git("init", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Tester")
	write("1")
	git("add", "-A")
	git("commit", "-m", "c1")
	c1 := git("rev-parse", "HEAD")
	write("2")
	git("add", "-A")
	git("commit", "-m", "c2")
	git("remote", "add", "origin", remote)
	git("push", "-u", "origin", "main")
	git("remote", "set-head", "origin", "main")
	// A commit ahead of origin/main, reachable only via a side branch.
	git("checkout", "-b", "feature")
	write("3")
	git("add", "-A")
	git("commit", "-m", "c3")
	c3 := git("rev-parse", "HEAD")
	git("checkout", "main")

	cases := []struct {
		name    string
		tokens  []string
		blocked bool
	}{
		{"ancestor sha allowed", []string{"git", "checkout", c1}, false},
		{"main allowed", []string{"git", "checkout", "main"}, false},
		{"ahead sha blocked", []string{"git", "checkout", c3}, true},
		{"feature branch blocked", []string{"git", "checkout", "feature"}, true},
		{"file checkout allowed", []string{"git", "checkout", "--", "f"}, false},
		{"file from main allowed", []string{"git", "checkout", "main", "--", "f"}, false},
		{"unknown ref blocked", []string{"git", "checkout", "no-such-thing"}, true},
		{"detach ancestor allowed", []string{"git", "switch", "--detach", c1}, false},
		{"detach ahead blocked", []string{"git", "switch", "--detach", c3}, true},
		{"switch feature blocked", []string{"git", "switch", "feature"}, true},
		{"switch main allowed", []string{"git", "switch", "main"}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var reason string
			switch tt.tokens[1] {
			case "checkout":
				reason = checkGitCheckout(tt.tokens, dir)
			case "switch":
				reason = checkGitSwitch(tt.tokens, dir)
			}
			if tt.blocked && reason == "" {
				t.Errorf("expected blocked, got allowed")
			}
			if !tt.blocked && reason != "" {
				t.Errorf("expected allowed, got: %s", reason)
			}
		})
	}
}

// TestIsWithinResolvesSymlinks covers the macOS shape: the stamped root and a
// path the caller supplies can spell one location two ways, because macOS
// resolves symlinks in cwd-derived paths and a build-time stamp keeps whatever
// form it was built with. /tmp -> /private/tmp is the everyday case there; this
// builds the same situation with an explicit symlink so it runs anywhere.
func TestIsWithinResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Base as the real path, target reached through the symlink.
	if !isWithin(real, filepath.Join(link, "sub", "file.txt")) {
		t.Error("a target reached through a symlink was judged outside its own base")
	}
	// And the reverse: base as the symlink, target as the real path.
	if !isWithin(link, filepath.Join(real, "sub", "file.txt")) {
		t.Error("a real target was judged outside a symlinked base")
	}
	// A genuinely unrelated path is still outside.
	if isWithin(real, filepath.Join(t.TempDir(), "elsewhere.txt")) {
		t.Error("an unrelated path was judged inside")
	}
}

// TestIsWithinIsCaseInsensitiveOnWindows covers the Windows shape: paths there
// are case-insensitive, so a stamped C:\Users\x and a caller's c:\users\x are
// one location. Byte comparison would call them two and deny a legitimate write
// inside the guard's own repo.
func TestIsWithinIsCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive path comparison is a Windows concern")
	}
	base := t.TempDir()
	if !isWithin(strings.ToUpper(base), filepath.Join(base, "file.txt")) {
		t.Error("differently-cased spellings of one directory were judged different")
	}
}
