// guard.go — Claude Code PreToolUse hook for blocking dangerous Bash commands.
//
// Reads hook JSON from stdin, checks the command against a set of dangerous
// patterns, and outputs a JSON allow/deny decision to stdout.
//
// Usage (via hook config):
//
//	"command": "go run tools/guard/guard.go || exit 2"
//
// The || exit 2 provides fail-closed behavior: if the guard crashes,
// exit 2 tells the hook system to block the command.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hookInput is the JSON structure Claude Code sends to PreToolUse hooks.
type hookInput struct {
	ToolInput struct {
		Command string `json:"command"`
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

func main() {
	var input hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		printDeny("guard: failed to parse hook input: " + err.Error())
		return
	}

	command := input.ToolInput.Command
	if command == "" {
		printDeny("guard: could not extract command from hook input")
		return
	}

	if reason := checkAll(command); reason != "" {
		printDeny(reason)
	} else {
		fmt.Println("{}")
	}
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

// ── Command splitting ───────────────────────────────────────────────────────

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
func checkAll(cmd string) string {
	for _, part := range splitCommands(cmd) {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			if reason := checkSingle(trimmed); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// ── Single command check ────────────────────────────────────────────────────

// checkSingle checks a single command (no shell operators) for dangerous patterns.
func checkSingle(cmd string) string {
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
		return checkAll(inner)
	}

	if program == "git" {
		return checkGit(args)
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
		"apt": true, "apt-get": true,
	}
	if pkgInstallers[program] && hasSubcommand(args, "install") {
		return fmt.Sprintf("blocked: '%s install' (unreviewed package installation)", program)
	}

	return ""
}

// ── Git checks ──────────────────────────────────────────────────────────────

func checkGit(tokens []string) string {
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

	return ""
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
	"Use ./build (Linux/macOS) or .\\build.ps1 (Windows) instead — " +
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
		} else if strings.HasPrefix(t, "--target-directory=") {
			targetDir = strings.TrimPrefix(t, "--target-directory=")
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
	for _, t := range tokens[1:] {
		if t == sub {
			return true
		}
	}
	return false
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
