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

	if program == "curl" || program == "wget" {
		return fmt.Sprintf("blocked: '%s' (unreviewed network access)", program)
	}

	// Package installers.
	pkgInstallers := map[string]bool{
		"npm": true, "pip": true, "pip3": true,
		"cargo": true, "go": true,
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
