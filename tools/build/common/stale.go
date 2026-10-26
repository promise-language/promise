package common

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CheckStale compares the compiled-in source hash against the current source
// hash. If they differ, it prints an error and exits. Call this at the start
// of every tool's main().
func CheckStale(compiledHash string) {
	if compiledHash == "dev" {
		// Running via "go run" — skip staleness check.
		return
	}
	root, err := FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	currentHash, err := ToolsSourceHash(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if compiledHash != currentHash {
		abs, relative := MakeCommands(root)
		fmt.Fprintf(os.Stderr, "tools source has changed — rebuild before continuing: %s (or %s from the repo root)\n", abs, relative)
		os.Exit(1)
	}
}

// The conditions a tool refuses on, from the closed set the tool contract
// names. A refusal is not a failure: a stale bin/gate has measured nothing, and
// a caller reading its exit as the tree's failure would blame a repository that
// was never put to the question.
const (
	RefusalStale                 = "stale"
	RefusalUnstamped             = "unstamped"
	RefusalRepositoryUnreachable = "repository-unreachable"
	RefusalUnbuilt               = "unbuilt"
)

// ExitRefused is the status a tool exits with when it declines to act.
const ExitRefused = 3

// Refusal is a tool declining to act: exit 3, and {condition, recovery} as its
// result. Detail is the human line's prose, never written to the object.
type Refusal struct {
	Condition string `json:"condition"`
	Recovery  string `json:"recovery"`
	Detail    string `json:"-"`
}

func (r *Refusal) Error() string { return r.Detail + " — run " + r.Recovery }

// toolHash is the stamped source hash of the running tool, recorded by
// RefuseUnlessFit so -version can answer with the hash the check compares.
var toolHash string

// ToolHash is the running tool's stamped source hash, or "" before
// RefuseUnlessFit has accepted it.
func ToolHash() string { return toolHash }

// RefuseUnlessFit is the first thing bin/gate, bin/run and bin/verify do: before
// the command line is read — -help and -version included — the binary must have
// been built by ./make from the tree it stands in. One that was not refuses
// (exit 3) rather than answering from a surface it no longer matches. protocol
// names the flags whose mode fixes stdout for another contract (--envelope,
// --verdict): asked that way, the refusal travels as the status and one stderr
// line, and stdout stays empty.
func RefuseUnlessFit(tool, compiledHash string, args []string, protocol ...string) {
	refusal := fitness(compiledHash)
	if refusal == nil {
		toolHash = compiledHash
		return
	}
	os.Exit(writeRefusal(os.Stdout, os.Stderr, tool, refusal, args, protocol))
}

// fitness is the refusal the running binary owes, or nil when it may act.
func fitness(compiledHash string) *Refusal {
	_, relative := MakeCommands(".")
	if compiledHash == "dev" || compiledHash == "" {
		return &Refusal{Condition: RefusalUnstamped, Recovery: relative,
			Detail: "this binary was not built by ./make, so it cannot tell whether it matches the tree"}
	}
	root, err := FindRoot()
	if err != nil {
		return &Refusal{Condition: RefusalRepositoryUnreachable, Recovery: relative,
			Detail: "the repository this binary was built for cannot be found: " + err.Error()}
	}
	current, err := ToolsSourceHash(root)
	if err != nil {
		return &Refusal{Condition: RefusalRepositoryUnreachable, Recovery: relative,
			Detail: "the tools source cannot be read: " + err.Error()}
	}
	if current != compiledHash {
		return &Refusal{Condition: RefusalStale, Recovery: relative,
			Detail: "the tools source has changed since this binary was built"}
	}
	return nil
}

// writeRefusal writes r where the mode in force puts a result and returns the
// refusal status. The mode is read from the raw argv — the command line has not
// been parsed, and a stale binary's parser is the thing not to trust: -json or
// -human where given, JSON in a protocol mode or when stdout is not a terminal,
// human otherwise. In a protocol mode stdout belongs to an envelope or a verdict
// and stays empty; the refusal is the status and one stderr line.
func writeRefusal(stdout, stderr io.Writer, tool string, r *Refusal, args, protocol []string) int {
	line := tool + ": " + r.Error()
	inProtocol := slices.ContainsFunc(protocol, func(p string) bool { return namesFlag(args, p) })
	switch {
	case inProtocol:
		fmt.Fprintln(stderr, line)
	case namesFlag(args, "json") || (!namesFlag(args, "human") && !stdoutIsTerminal()):
		body, _ := json.Marshal(r)
		fmt.Fprintf(stdout, "%s\n", body)
	default:
		fmt.Fprintln(stdout, line)
	}
	return ExitRefused
}

// namesFlag reports whether a word before `--` spells the flag name, with one
// dash or two, with or without a value.
func namesFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		body, ok := strings.CutPrefix(a, "-")
		if !ok {
			continue
		}
		body = strings.TrimPrefix(body, "-")
		if flag, _, _ := strings.Cut(body, "="); flag == name {
			return true
		}
	}
	return false
}

// MakeCommands returns the two spellings of this repository's bootstrap
// script: the absolute path, which resolves from any working directory, and
// the relative form, which only works at the repo root.
//
// Every staleness message names a rebuild command, and bin/guard *enforces*
// what it names — a message suggesting a command the same binary then refuses
// is what wedged a session in T1813. Defining the two spellings once means the
// guard's message and the tools' message cannot drift apart, and it is the only
// place that has to know Windows spells them differently.
func MakeCommands(root string) (abs, relative string) {
	if IsWindows() {
		return filepath.Join(root, "make.cmd"), `.\make.cmd`
	}
	return filepath.Join(root, "make"), "./make"
}
