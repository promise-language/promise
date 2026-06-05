// Command guard is a Claude Code PreToolUse hook that blocks dangerous operations.
//
// It handles three tool types:
//   - Bash: blocks dangerous shell commands (git push, rm -rf, etc.)
//   - Edit: blocks forbidden patterns in file edits (e.g., allow_leaks in .pr files)
//   - Write: blocks forbidden patterns in file writes
//
// Edit/Write gates are defined in tools/gates/edit_gates.json.
//
// Compiled by ./make into bin/guard. Invoked via hook config:
//
//	"command": "\"$CLAUDE_PROJECT_DIR/bin/guard\" || exit 2"
//
// $CLAUDE_PROJECT_DIR is set by Claude Code in every PreToolUse hook env,
// so the hook is immune to shell cwd drift (B0349). On a fresh clone
// bin/guard doesn't exist yet, so the hook fails closed and the user
// must run ./make once from a terminal (outside Claude Code) to bootstrap.
//
// The || exit 2 provides fail-closed behavior: if the guard crashes,
// exit 2 tells the hook system to block the command.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/promise-language/promise/tools/build/common"
	"github.com/promise-language/promise/tools/build/internal/context"
)

var sourceHash = "dev"

// hookInput is the JSON structure Claude Code sends to PreToolUse hooks.
// Fields vary by tool type — we decode all possible fields and detect the tool.
type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		// Bash
		Command string `json:"command"`
		// Edit
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		// Write
		Content string `json:"content"`
		// Skill
		Skill string `json:"skill"`
		Args  string `json:"args"`
	} `json:"tool_input"`
}

// hookOutput is the JSON structure the hook returns to Claude Code.
type hookOutput struct {
	HookSpecificOutput *hookDecision `json:"hookSpecificOutput,omitempty"`
}

type hookDecision struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// editGate defines a pattern-based gate for Edit/Write operations.
type editGate struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	Files   string `json:"files"`
	Reason  string `json:"reason"`
}

type editGatesConfig struct {
	Gates []editGate `json:"gates"`
}

func main() {
	var input hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		printDeny("guard: failed to parse hook input: " + err.Error())
		return
	}

	tool := detectTool(input)
	isPost := input.HookEventName == "PostToolUse"

	// PostToolUse hooks can't block — the tool already ran. Just notify
	// the tracker (pop the context frame) and exit.
	if isPost {
		notifyContext(input, tool, false)
		fmt.Println("{}")
		return
	}

	// Skill PreToolUse: skip the stale check (heartbeats / context updates
	// are best-effort and the settings.json entry uses `|| true` anyway).
	if tool == "skill" {
		notifyContext(input, tool, true)
		fmt.Println("{}")
		return
	}

	// Stale check: block most operations when tools source has changed,
	// but always allow ./make so the agent can rebuild.
	if reason := checkStale(input); reason != "" {
		printDeny(reason)
		return
	}

	// Detect tool type and dispatch.
	switch tool {
	case "bash":
		if input.ToolInput.Command == "" {
			printDeny("guard: could not extract command from hook input")
			return
		}
		if reason := checkAll(input.ToolInput.Command, input.CWD); reason != "" {
			printDeny(reason)
		} else {
			notifyContext(input, tool, true)
			fmt.Println("{}")
		}

	case "edit":
		if reason := checkEditGates(input.ToolInput.FilePath, input.ToolInput.NewString); reason != "" {
			printDeny(reason)
		} else {
			notifyContext(input, tool, true)
			fmt.Println("{}")
		}

	case "write":
		if reason := checkEditGates(input.ToolInput.FilePath, input.ToolInput.Content); reason != "" {
			printDeny(reason)
		} else {
			notifyContext(input, tool, true)
			fmt.Println("{}")
		}

	default:
		// Unknown tool type — allow (don't block what we don't understand).
		fmt.Println("{}")
	}
}

// notifyContext fires a context push (PreToolUse) or pop (PostToolUse) on
// the tracker.
func notifyContext(input hookInput, tool string, isPush bool) {
	kind, name, inputText, ok := contextFields(input, tool)
	if !ok {
		return
	}
	in := context.Input{
		HookEventName: input.HookEventName,
		CWD:           input.CWD,
		Kind:          kind,
		Name:          name,
		InputText:     inputText,
	}
	if isPush {
		context.Push(in)
	} else {
		context.Pop(in)
	}
}

// contextFields returns the (kind, name, input) tuple to forward to the
// tracker for a given dispatched tool. Returns ok=false for unknown tools.
func contextFields(input hookInput, tool string) (kind, name, inputText string, ok bool) {
	switch tool {
	case "skill":
		return "skill", input.ToolInput.Skill, input.ToolInput.Args, true
	case "bash":
		return "tool", "Bash", input.ToolInput.Command, true
	case "edit":
		return "tool", "Edit", input.ToolInput.FilePath, true
	case "write":
		return "tool", "Write", input.ToolInput.FilePath, true
	default:
		return "", "", "", false
	}
}

// checkStale returns a deny reason if the guard binary is stale and the
// command is not ./make (which must always be allowed so the agent can rebuild).
func checkStale(input hookInput) string {
	if sourceHash == "dev" {
		return ""
	}
	root, err := common.FindRoot()
	if err != nil {
		return ""
	}
	currentHash, err := common.ToolsSourceHash(root)
	if err != nil {
		return ""
	}
	if sourceHash == currentHash {
		return ""
	}

	// Stale — allow the agent to rebuild via the repo-root ./make, even when
	// wrapped (e.g. `cd repo && ./make`, `./make 2>&1 | tail`). Resolves each
	// subcommand's first token against the cwd it would run under and only
	// allows when that path is exactly <root>/make (or its .exe/.cmd sibling).
	// This prevents allowing a `./make` that happens to live in some other
	// directory the agent has cd'd into. Per-subcommand safety checks
	// (rm -rf, git push, etc.) still run downstream via checkAll.
	if detectTool(input) == "bash" && isRepoMakeChain(input.ToolInput.Command, input.CWD, root) {
		return ""
	}

	// Stale — allow Edit/Write through. Edit gates are loaded from disk
	// at runtime (tools/gates/edit_gates.json), so the stale binary's gate
	// enforcement is still correct. Blocking these creates a deadlock when
	// the agent needs to fix a compilation error in tools code (T0276).
	if tool := detectTool(input); tool == "edit" || tool == "write" {
		fmt.Fprintf(os.Stderr, "guard: stale binary — edit/write gates still enforced (run ./make to rebuild)\n")
		return ""
	}

	makeCmd := "./make"
	if runtime.GOOS == "windows" {
		makeCmd = ".\\make.cmd"
	}
	return "guard binary is stale — run " + makeCmd + " to rebuild tools before continuing"
}

// isRepoMakeChain reports whether the given command chain contains an
// invocation of the repo-root ./make script (or its .exe/.cmd sibling),
// resolving paths against the shell's cwd as it walks. Tracks `cd <path>`
// updates so cwd reflects what the make subcommand would actually run with.
//
// This is intentionally conservative: it only accepts `./make`, `./make.exe`,
// `.\make.cmd`, or the absolute path to the repo's make script. A bare
// `make` (no `./`) is not a Promise bootstrap invocation and is rejected.
//
// cwd may be empty if the hook input lacks CWD; in that case we can't verify
// resolution and refuse to whitelist.
func isRepoMakeChain(command, cwd, root string) bool {
	if cwd == "" {
		return false
	}
	expectedMake := filepath.Join(root, "make")
	expectedExe := filepath.Join(root, "make.exe")
	expectedCmd := filepath.Join(root, "make.cmd")

	for _, part := range splitCommands(command) {
		trimmed := strings.TrimSpace(part)
		tokens := tokenize(trimmed)
		if len(tokens) == 0 {
			continue
		}
		// `cd <path>` updates cwd for subsequent subcommands in the same chain.
		if tokens[0] == "cd" && len(tokens) >= 2 {
			target := stripQuotes(tokens[1])
			if !filepath.IsAbs(target) {
				target = filepath.Join(cwd, target)
			}
			cwd = filepath.Clean(target)
			continue
		}
		first := tokens[0]
		// Absolute path to the repo make script.
		if first == expectedMake || first == expectedExe || first == expectedCmd {
			return true
		}
		// Relative invocation — strip the leading `./` or `.\` and resolve
		// the bare basename against cwd. Done this way because filepath.Join
		// treats `\` as a literal character on Unix, so naive joining of
		// `.\\make.cmd` would not produce the expected path on non-Windows
		// platforms.
		var basename string
		switch first {
		case "./make", ".\\make":
			basename = "make"
		case "./make.exe", ".\\make.exe":
			basename = "make.exe"
		case "./make.cmd", ".\\make.cmd":
			basename = "make.cmd"
		}
		if basename != "" {
			resolved := filepath.Clean(filepath.Join(cwd, basename))
			if resolved == filepath.Join(root, basename) {
				return true
			}
		}
	}
	return false
}

// detectTool determines the tool type from the input fields.
func detectTool(input hookInput) string {
	// Prefer explicit tool_name if present.
	switch strings.ToLower(input.ToolName) {
	case "bash":
		return "bash"
	case "edit":
		return "edit"
	case "write":
		return "write"
	case "skill":
		return "skill"
	}

	// Fall back to field-based detection.
	if input.ToolInput.Skill != "" {
		return "skill"
	}
	if input.ToolInput.Command != "" {
		return "bash"
	}
	if input.ToolInput.OldString != "" || input.ToolInput.NewString != "" {
		return "edit"
	}
	if input.ToolInput.Content != "" && input.ToolInput.FilePath != "" {
		return "write"
	}
	return "unknown"
}

func printDeny(reason string) {
	out := hookOutput{
		HookSpecificOutput: &hookDecision{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: reason,
		},
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

// ── Edit/Write gate checking ────────────────────────────────────────────────

// loadEditGates loads gate definitions from tools/gates/edit_gates.json.
// Searches relative to the git repo root (walks up from cwd).
func loadEditGates() ([]editGate, error) {
	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "tools", "gates", "edit_gates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config editGatesConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return config.Gates, nil
}

// findRepoRoot locates the repo root by walking up from the guard binary's
// own location (not cwd), so Edit/Write gate loading keeps working even when
// the agent's shell cwd has drifted outside the git worktree (B0349).
//
// The guard binary lives at <root>/bin/guard, so we start from its resolved
// path and walk up until we find a .git entry. Falls back to walking up from
// cwd if os.Executable() isn't available (shouldn't happen on any supported
// platform, but stay defensive).
func findRepoRoot() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		// Resolve symlinks so `go run` or a symlinked bin/ still works.
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		for {
			if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	// Fallback: walk up from cwd.
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside a git repository")
		}
		dir = parent
	}
}

// checkEditGates checks file content against all edit gates.
// Returns the first deny reason, or "".
func checkEditGates(filePath, content string) string {
	if filePath == "" || content == "" {
		return ""
	}

	gates, err := loadEditGates()
	if err != nil {
		// Fail-closed: if we can't load gates, block with explanation.
		return fmt.Sprintf("guard: failed to load edit gates: %v", err)
	}

	fileName := filepath.Base(filePath)

	for _, gate := range gates {
		if !matchGlob(gate.Files, fileName) {
			continue
		}
		matched, err := regexp.MatchString(gate.Pattern, content)
		if err != nil {
			return fmt.Sprintf("guard: invalid regex in gate %q: %v", gate.ID, err)
		}
		if matched {
			return fmt.Sprintf("edit gate %q: %s", gate.ID, gate.Reason)
		}
	}
	return ""
}

// matchGlob checks if a filename matches a glob pattern.
// Supports "*" (match all) and "*.ext" patterns.
func matchGlob(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	matched, err := filepath.Match(pattern, name)
	if err != nil {
		return false
	}
	return matched
}

// ── Bash command splitting ──────────────────────────────────────────────────

// splitCommands splits a command string on shell operators (&&, ||, ;, |).
func splitCommands(cmd string) []string {
	s := cmd
	s = strings.ReplaceAll(s, " && ", "\n")
	s = strings.ReplaceAll(s, " || ", "\n")
	s = strings.ReplaceAll(s, "; ", "\n")
	s = strings.ReplaceAll(s, " | ", "\n")
	return strings.Split(s, "\n")
}

// checkAll checks all sub-commands. Returns the first deny reason, or "".
//
// cwd is the shell's working directory; a leading `cd <path>` in the chain
// updates it for subsequent sub-commands so per-command checks (e.g. git
// branch hygiene, which is scoped to the super/submodule the command runs in)
// see the directory the command would actually execute under.
func checkAll(cmd, cwd string) string {
	for _, part := range splitCommands(cmd) {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Track `cd <path>` so the next sub-command's cwd is correct (e.g.
		// `cd flow && git checkout <sha>` must resolve git in the submodule).
		tokens := tokenize(trimmed)
		if len(tokens) >= 2 && tokens[0] == "cd" {
			target := stripQuotes(tokens[1])
			if !filepath.IsAbs(target) {
				target = filepath.Join(cwd, target)
			}
			cwd = filepath.Clean(target)
		}
		if reason := checkSingle(trimmed, cwd); reason != "" {
			return reason
		}
	}
	return ""
}

// ── Single command check ────────────────────────────────────────────────────

// checkSingle checks a single command (no shell operators) for dangerous patterns.
func checkSingle(cmd, cwd string) string {
	tokens := tokenize(cmd)
	if len(tokens) == 0 {
		return ""
	}

	args := stripWrappers(tokens)
	if len(args) == 0 {
		return ""
	}

	program := args[0]

	// bash -c / sh -c: extract inner command and recurse.
	if (program == "bash" || program == "sh") && len(args) >= 3 && args[1] == "-c" {
		inner := strings.Join(args[2:], " ")
		inner = stripQuotes(inner)
		return checkAll(inner, cwd)
	}

	if program == "git" {
		return checkGit(args, cwd)
	}

	if program == "rm" {
		return checkRm(args)
	}

	if program == "cp" || program == "mv" {
		return checkCopy(program, args)
	}

	if program == "curl" || program == "wget" {
		return fmt.Sprintf("blocked: '%s' (unreviewed network access)", program)
	}

	if program == "go" {
		return checkGo(args)
	}

	// Package installers.
	pkgInstallers := map[string]bool{
		"npm": true, "pip": true, "pip3": true,
		"cargo": true,
		"apt":   true, "apt-get": true,
	}
	if pkgInstallers[program] && hasSubcommand(args, "install") {
		return fmt.Sprintf("blocked: '%s install' (unreviewed package installation)", program)
	}

	return ""
}

// ── Git checks ──────────────────────────────────────────────────────────────

func checkGit(tokens []string, cwd string) string {
	subcommand := findGitSubcommand(tokens)
	hasForce, hasHard, hasShortF := false, false, false

	for _, t := range tokens[1:] {
		switch t {
		case "--force", "--force-with-lease":
			hasForce = true
		case "--hard":
			hasHard = true
		case "-f":
			hasShortF = true
		}
	}

	if subcommand == "push" {
		if hasForce || hasShortF {
			return "blocked: 'git push --force' (can destroy remote history)"
		}
		return "blocked: 'git push' (requires explicit user approval)"
	}
	if subcommand == "reset" && hasHard {
		return "blocked: 'git reset --hard' (can destroy uncommitted work)"
	}

	// Branch hygiene: this project works ONLY on `main` (tracking origin/main).
	// Ad-hoc branches strand work that never reaches origin/main — when the flow
	// pushes `main`, anything committed on another branch is lost in the local
	// clone. So block branch creation and switching to any branch other than
	// `main`. A *detached* checkout is allowed only onto origin/HEAD or an
	// ancestor of it (e.g. positioning a submodule gitlink) — never onto a
	// commit ahead of / diverged from the pushed history. File checkouts
	// (`git checkout -- <file>` / `git checkout main -- <file>`), branch
	// listing, rebase, and worktrees are all still allowed.
	//
	// This only applies inside the superproject and its submodules — the clones
	// whose state the flow pushes. Temporary repos and worktrees outside the
	// project tree are exempt. When ancestry can't be verified we fail closed.
	switch subcommand {
	case "switch", "checkout", "branch":
		if !inManagedRepo(effectiveGitDir(tokens, cwd)) {
			return ""
		}
	}
	gitDir := effectiveGitDir(tokens, cwd)
	switch subcommand {
	case "switch":
		return checkGitSwitch(tokens, gitDir)
	case "checkout":
		return checkGitCheckout(tokens, gitDir)
	case "branch":
		return checkGitBranch(tokens)
	}

	return ""
}

// isMainRef reports whether a ref token names the one branch this project works
// on. `main` (local) and `origin/main` (its upstream) are the only allowed
// switch/branch targets.
func isMainRef(s string) bool { return s == "main" || s == "origin/main" }

// gitSubArgs returns the tokens that follow the git subcommand, skipping the
// `git` program token and any global flags (including the value-taking ones).
func gitSubArgs(tokens []string) []string {
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		switch t {
		case "-c", "-C", "--git-dir", "--work-tree":
			i += 2
			continue
		}
		if strings.HasPrefix(t, "-") {
			i++
			continue
		}
		return tokens[i+1:] // tokens after the subcommand itself
	}
	return nil
}

// checkGitSwitch blocks `git switch` that creates a branch (-c/-C/--orphan) or
// switches to any branch other than `main`. A detached switch (`--detach
// <commit>`) is allowed only onto origin/HEAD or an ancestor of it.
func checkGitSwitch(tokens []string, gitDir string) string {
	args := gitSubArgs(tokens)
	detach := false
	for _, a := range args {
		if a == "-c" || a == "-C" || a == "--create" || a == "--orphan" {
			return "blocked: 'git switch -c/--create' creates a branch. This project works only on main (tracking origin/main)."
		}
		if a == "-d" || a == "--detach" {
			detach = true
		}
	}
	for _, a := range args {
		if a == "-" { // switch to previous branch — not guaranteed to be main
			return "blocked: 'git switch -' switches off main; only 'main' is allowed (work stays on main tracking origin/main)."
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isMainRef(a) {
			return ""
		}
		if detach {
			return checkDetachTarget(gitDir, a)
		}
		return fmt.Sprintf("blocked: 'git switch %s' — only 'main' is allowed (work stays on main tracking origin/main).", a)
	}
	return ""
}

// checkGitCheckout blocks `git checkout` that creates a branch (-b/-B/--orphan)
// or switches to a non-main branch, while still allowing file checkouts and
// detaching onto origin/HEAD-or-ancestor commits. A `--` separator or a
// path-like target (contains '/', '.', or '~') is treated as a file operation
// and allowed. A bare token is resolved: a local branch other than `main` is a
// branch switch (blocked); otherwise it is treated as a commit-ish and allowed
// only if it is origin/HEAD or an ancestor of it.
func checkGitCheckout(tokens []string, gitDir string) string {
	args := gitSubArgs(tokens)
	for _, a := range args {
		if a == "-b" || a == "-B" || a == "--orphan" {
			return "blocked: 'git checkout -b/-B' creates a branch. This project works only on main (tracking origin/main)."
		}
		if a == "--" {
			return "" // explicit file mode
		}
	}
	for _, a := range args {
		if a == "-" {
			return "blocked: 'git checkout -' switches off main; only 'main' is allowed."
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isMainRef(a) {
			return "" // switching to main, or `git checkout main -- <file>`
		}
		if strings.ContainsAny(a, "/.~") {
			return "" // path-like → file checkout
		}
		if isLocalBranch(gitDir, a) {
			return fmt.Sprintf("blocked: 'git checkout %s' switches to a non-main branch. This project works only on main; commit on main so the flow can push it.", a)
		}
		// Not a local branch → treat as a commit-ish (sha/tag). Allow only if it
		// is origin/HEAD or an ancestor (e.g. positioning a submodule gitlink).
		return checkDetachTarget(gitDir, a)
	}
	return ""
}

// checkGitBranch blocks `git branch` that CREATES (or copies) a branch other
// than `main`. Listing (`git branch`, `-a`, `-v`, …), deletion (`-d`/`-D`),
// move/rename (`-m`/`-M`), and force-moving `main` itself (`git branch -f main
// origin/main`) are all allowed — only the creation of a non-main branch is
// blocked.
func checkGitBranch(tokens []string) string {
	args := gitSubArgs(tokens)
	for _, a := range args {
		switch {
		case a == "-d" || a == "-D" || a == "--delete" ||
			a == "-m" || a == "-M" || a == "--move" ||
			a == "--list" || a == "-a" || a == "--all" ||
			a == "-r" || a == "--remotes" || a == "--show-current" ||
			a == "--unset-upstream" || a == "--edit-description":
			return "" // not a plain create
		case strings.HasPrefix(a, "--contains") || strings.HasPrefix(a, "--merged") ||
			strings.HasPrefix(a, "--no-merged") || strings.HasPrefix(a, "--points-at") ||
			strings.HasPrefix(a, "--set-upstream-to") || strings.HasPrefix(a, "--sort") ||
			strings.HasPrefix(a, "--format"):
			return "" // informational/query form
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isMainRef(a) {
			return "" // creating/force-moving 'main' is allowed
		}
		return fmt.Sprintf("blocked: 'git branch %s' creates a branch. This project works only on main (tracking origin/main).", a)
	}
	return ""
}

// checkDetachTarget allows a detached checkout of ref only when ref resolves to
// origin/HEAD or an ancestor of it (keeping the super/submodule clone on the
// already-pushed history). Fails closed: an unresolvable ref or unknown
// origin/HEAD is blocked.
func checkDetachTarget(gitDir, ref string) string {
	allowed, resolved := ancestryVerdict(gitDir, ref)
	if allowed {
		return ""
	}
	if !resolved {
		return fmt.Sprintf("blocked: 'git checkout %s' — '%s' is not main, a verifiable commit, or an explicit file. For a file use 'git checkout -- %s'; to change branches use 'git switch main'.", ref, ref, ref)
	}
	return fmt.Sprintf("blocked: 'git checkout %s' — a detached checkout is only allowed onto origin/HEAD or an ancestor of it (keeps this super/submodule clone on the pushed history). '%s' is ahead of, or diverged from, origin/HEAD (or origin/HEAD could not be resolved).", ref, ref)
}

// effectiveGitDir resolves the directory the git command would run in: the
// value of a `-C <dir>` global flag (relative to cwd) if present, else cwd.
func effectiveGitDir(tokens []string, cwd string) string {
	dir := cwd
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		if t == "-C" && i+1 < len(tokens) {
			d := tokens[i+1]
			if !filepath.IsAbs(d) {
				d = filepath.Join(dir, d)
			}
			dir = filepath.Clean(d)
			i += 2
			continue
		}
		if (t == "-c" || t == "--git-dir" || t == "--work-tree") && i+1 < len(tokens) {
			i += 2
			continue
		}
		if strings.HasPrefix(t, "-") {
			i++
			continue
		}
		break
	}
	return dir
}

// guardProjectRoot returns the superproject working-tree root: $CLAUDE_PROJECT_DIR
// (set in every PreToolUse hook env) or, failing that, the root derived from the
// guard binary's own location. Returns "" when neither resolves.
func guardProjectRoot() string {
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		return filepath.Clean(d)
	}
	if r, err := common.FindRoot(); err == nil {
		return filepath.Clean(r)
	}
	return ""
}

// inManagedRepo reports whether gitDir belongs to the superproject or one of
// its submodules — the clones whose committed state the flow pushes to origin.
// Worktrees and throwaway clones outside the project tree are exempt. When the
// project root can't be determined we fail closed (enforce).
func inManagedRepo(gitDir string) bool {
	root := guardProjectRoot()
	if root == "" {
		return true // can't scope → enforce (fail closed)
	}
	top, ok := gitOutput(gitDir, "rev-parse", "--show-toplevel")
	if !ok {
		return false // not a git repo → nothing to strand → exempt
	}
	if filepath.Clean(top) == root {
		return true // anywhere inside the superproject working tree
	}
	if super, ok := gitOutput(gitDir, "rev-parse", "--show-superproject-working-tree"); ok && super != "" {
		return filepath.Clean(super) == root // a submodule of this project
	}
	return false
}

// isLocalBranch reports whether name is an existing local branch in gitDir.
func isLocalBranch(gitDir, name string) bool {
	return gitOK(gitDir, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
}

// originHead resolves the commit this repo's pushed default branch points at,
// trying origin/HEAD then the common default-branch upstreams. Returns
// ("", false) when none resolve (callers fail closed). Submodule clones often
// lack the origin/HEAD symref, so the explicit fallbacks matter there.
func originHead(gitDir string) (string, bool) {
	for _, ref := range []string{"origin/HEAD", "origin/main", "origin/master"} {
		if c, ok := gitOutput(gitDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); ok {
			return c, true
		}
	}
	return "", false
}

// ancestryVerdict resolves ref and reports whether it is origin/HEAD or an
// ancestor of it. resolved is false when ref does not name a commit at all
// (likely a file path); allowed is false (with resolved true) when origin/HEAD
// can't be determined — i.e. ancestry is unverifiable and we fail closed.
func ancestryVerdict(gitDir, ref string) (allowed, resolved bool) {
	target, ok := gitOutput(gitDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if !ok {
		return false, false
	}
	origin, ok := originHead(gitDir)
	if !ok {
		return false, true // can't verify ancestry → fail closed
	}
	return gitOK(gitDir, "merge-base", "--is-ancestor", target, origin), true
}

// gitOutput runs git in dir and returns trimmed stdout, ok=false on any error.
func gitOutput(dir string, args ...string) (string, bool) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// gitOK runs git in dir and reports whether it exited zero.
func gitOK(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd.Run() == nil
}

// findGitSubcommand skips global flags that take arguments.
func findGitSubcommand(tokens []string) string {
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		switch t {
		case "-c", "-C", "--git-dir", "--work-tree":
			i += 2
		default:
			if strings.HasPrefix(t, "-") {
				i++
			} else {
				return t
			}
		}
	}
	return ""
}

// ── go build checks ─────────────────────────────────────────────────────

const goBuildBlockMsg = "blocked: 'go build' for the Promise compiler. " +
	"Use bin/build (Linux/macOS) or bin\\build.exe (Windows) instead — " +
	"go build skips resource embedding and produces a broken binary."

func checkGo(tokens []string) string {
	if len(tokens) < 2 {
		return ""
	}

	sub := tokens[1]

	// go install: block package installation.
	if sub == "install" {
		return "blocked: 'go install' (unreviewed package installation)"
	}

	// Only check go build for compiler-building.
	if sub != "build" {
		return ""
	}

	// Walk args looking for -o value and non-flag positional args.
	skipNext := false
	for i := 2; i < len(tokens); i++ {
		if skipNext {
			skipNext = false
			// This token is the value of -o — check it.
			lower := strings.ToLower(tokens[i])
			if strings.Contains(lower, "promise") {
				return goBuildBlockMsg
			}
			// Block any -o targeting bin/ in the repo — only ./build should write there.
			if strings.HasPrefix(tokens[i], "bin/") || strings.HasPrefix(tokens[i], "./bin/") {
				return goBuildBlockMsg
			}
			continue
		}
		t := tokens[i]
		if t == "-o" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(t, "-") {
			continue
		}
		// Non-flag positional arg: check if it references the compiler.
		lower := strings.ToLower(t)
		if strings.Contains(lower, "promise") || strings.Contains(lower, "compiler/") {
			return goBuildBlockMsg
		}
	}

	return ""
}

// ── rm checks ───────────────────────────────────────────────────────────────

func checkRm(tokens []string) string {
	hasR, hasF := false, false
	for _, t := range tokens[1:] {
		switch t {
		case "-r", "-R", "--recursive":
			hasR = true
		case "-f", "--force":
			hasF = true
		default:
			if strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") {
				for _, c := range t[1:] {
					switch c {
					case 'r', 'R':
						hasR = true
					case 'f':
						hasF = true
					}
				}
			}
		}
	}
	if hasR && hasF {
		return "blocked: 'rm -rf' (recursive force delete)"
	}
	return ""
}

// ── cp/mv checks ────────────────────────────────────────────────────────

// checkCopy validates cp/mv destinations. Allows copies to the repo dir, /tmp, ~/.promise.
func checkCopy(program string, tokens []string) string {
	// Collect non-flag arguments (skip program name).
	// Handle -t/--target-directory which makes the *first* path-arg the destination.
	var paths []string
	targetDir := ""
	skipNext := false
	for i := 1; i < len(tokens); i++ {
		if skipNext {
			skipNext = false
			continue
		}
		t := tokens[i]
		if t == "-t" || t == "--target-directory" {
			if i+1 < len(tokens) {
				targetDir = tokens[i+1]
				skipNext = true
			}
		} else if after, ok := strings.CutPrefix(t, "--target-directory="); ok {
			targetDir = after
		} else if !strings.HasPrefix(t, "-") {
			paths = append(paths, t)
		}
	}

	// Determine destination.
	var dest string
	if targetDir != "" {
		dest = targetDir
	} else if len(paths) >= 2 {
		dest = paths[len(paths)-1]
	} else {
		return "" // can't determine destination, allow
	}

	if !isAllowedCopyDest(dest) {
		return fmt.Sprintf("blocked: '%s' to '%s' (destination outside repo, /tmp, ~/.promise)", program, dest)
	}
	return ""
}

// isAllowedCopyDest checks if a destination path is within the repo, /tmp, or ~/.promise.
func isAllowedCopyDest(dest string) bool {
	// Expand ~ prefix.
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(dest, "~/") {
		dest = filepath.Join(home, dest[2:])
	}

	abs, err := filepath.Abs(dest)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)

	// Allow /tmp.
	if strings.HasPrefix(abs, "/tmp/") || abs == "/tmp" {
		return true
	}

	// Allow ~/.promise.
	promiseDir := filepath.Join(home, ".promise")
	if strings.HasPrefix(abs, promiseDir+"/") || abs == promiseDir {
		return true
	}

	// Allow repo directory (cwd).
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	cwd = filepath.Clean(cwd)
	if strings.HasPrefix(abs, cwd+"/") || abs == cwd {
		return true
	}

	return false
}

// ── Token helpers ───────────────────────────────────────────────────────────

func tokenize(cmd string) []string {
	var result []string
	for _, t := range strings.Split(cmd, " ") {
		if t != "" {
			result = append(result, t)
		}
	}
	return result
}

func stripWrappers(tokens []string) []string {
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		if strings.Contains(t, "=") && !strings.HasPrefix(t, "-") {
			i++
		} else if t == "env" || t == "sudo" || t == "command" {
			i++
		} else {
			break
		}
	}
	return tokens[i:]
}

func hasSubcommand(tokens []string, sub string) bool {
	return slices.Contains(tokens[1:], sub)
}

func stripQuotes(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
