package common

// bin/run — the judging layer's command line (judge.go), and the one way to run
// a command this project builds.
//
// Two gate modes, and the difference is who ran the gate:
//
//   - `bin/run <gate>` measures (by executing bin/gate, the same process
//     boundary a runner crosses) and then judges, for a person at a terminal or
//     a script that wants both halves. No decision rests on it.
//   - `bin/run <gate> --verdict` judges an envelope it is GIVEN on stdin and
//     spawns nothing. This is what the flow SDK asks: it spawned the gate, so
//     it is the runner, and the runner may not come from the tree.
//
// Both reach the verdict through judge(), so they cannot disagree.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
)

// runSpec is everything bin/run accepts. A command it builds takes its own argv
// verbatim, so the spec needs the tree to know which words are commands.
func runSpec(root string) ToolSpec {
	return ToolSpec{
		Tool:        "run",
		Description: "judge a gate's measurement against this project's terms, or run a command this project builds",
		Flags:       []FlagSpec{{Name: "list", Description: "name every gate and command this project provides"}},
		Commands: []CommandSpec{{
			Name:         "<gate> | <command>",
			Description:  "a gate this project answers, or a command it builds",
			EnumeratedBy: "run --list",
			Flags: []FlagSpec{{
				Name:        "verdict",
				Description: "judge the envelope on stdin and write one verdict on stdout",
				FixesOutput: true,
			}},
		}},
		PassThrough: func(command string) bool { return slices.Contains(SupportedCommands(root), command) },
	}
}

// RunRun is the bin/run CLI entry. Every malformed command line is one
// *UsageError (exit 2) that has written nothing on stdout.
func RunRun(root string, args []string, stdin io.Reader, stdout io.Writer) error {
	spec := runSpec(root)
	inv, err := ParseInvocation(spec, args)
	if err != nil {
		return err
	}
	switch {
	case inv.Help:
		return WriteHelp(stdout, spec, inv.Mode)
	case inv.Version:
		return WriteVersion(stdout, inv.Mode, versionText(root))
	}
	SetObserved(ObservedFor(inv))

	if inv.Command == "" {
		if inv.Flags["list"] {
			return writeRunList(root, stdout, inv.Mode)
		}
		return BareInvocation(spec, versionText(root))
	}
	// A command is dispatched before a gate is looked for, because the two
	// share one namespace and a name cannot be both (CommandGateCollisions).
	if spec.PassThrough(inv.Command) {
		return runProjectCommand(root, inv.Command, inv.Args, inv.Mode, stdin, stdout)
	}
	if !IsContractGate(inv.Command) {
		return &UsageError{Tool: "run", Problems: []string{
			fmt.Sprintf("no gate or command named %q in this project; `run --list` names them", inv.Command),
		}}
	}
	if inv.Flags["verdict"] {
		return JudgeStdin(root, inv.Command, stdin, stdout)
	}
	return runOneGate(root, inv.Command, inv.Mode, stdout)
}

// versionText is what -version reports: the tools source hash this binary was
// stamped with, which is the hash its staleness check compares. A binary that
// got this far passed that check (RefuseUnlessFit), so the stamp IS the tree's
// hash, and it is read from the tree rather than carried in a variable — the
// same answer ./make -version gives.
func versionText(root string) string {
	h, err := ToolsSourceHash(root)
	if err != nil {
		return "unknown"
	}
	return h
}

// runOneGate is the by-hand path: execute bin/gate as a process — the same
// boundary a runner crosses, so a gate broken only across it (prints to stdout,
// exits without an envelope) is broken here too — then judge and print. For a
// person, each metric beside every term; in JSON, the envelope and the verdict
// together. Not acceptable, or nothing measured, exits 1.
func runOneGate(root, name string, mode OutputMode, stdout io.Writer) error {
	cmd := exec.Command(filepath.Join(root, "bin", "gate"+ExeSuffix()), name, "--envelope")
	cmd.Dir = root
	// The gate's progress goes straight to our stderr, not into a buffer
	// printed afterwards: a working gate and a wedged one look the same while
	// output is held. -narrate is passed on by being in force here.
	cmd.Stderr = os.Stderr
	if Observed() {
		cmd.Args = append(cmd.Args, "-narrate")
	}
	out, runErr := cmd.Output()
	var exit *exec.ExitError
	if errors.As(runErr, &exit) && exit.ExitCode() == ExitRefused {
		// The gate declined to act and said why on stderr; that refusal is
		// this run's, unchanged.
		return refuse(stdout, mode, gateRefusal(root, cmd.Path, name))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		if runErr != nil {
			return fmt.Errorf("%s did not measure anything: %w", name, runErr)
		}
		return fmt.Errorf("%s exited 0 and printed no envelope", name)
	}
	terms, err := LoadTerms(root)
	if err != nil {
		return err
	}
	env, err := readEnvelopeToJudge(root, name, out, terms)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	v := judge(root, env, terms)
	reportBlessing(root, env, v.Acceptable)
	if mode == ModeJSON {
		if err := encodeLine(stdout, struct {
			Envelope json.RawMessage `json:"envelope"`
			Verdict  Verdict         `json:"verdict"`
		}{bytes.TrimSpace(out), v}); err != nil {
			return err
		}
	} else {
		fmt.Fprint(stdout, renderVerdict(env, v))
	}
	if !v.Acceptable {
		return &reportedExit{code: 1}
	}
	return nil
}

// gateRefusal is the refusal a gate gave when asked for an envelope. In that
// protocol mode it says why on stderr only, so the object is asked for again
// where the gate writes it as its result — `-version -json`, which a gate unfit
// to act refuses before reading anything else — and relayed as the gate wrote
// it: an unstamped gate is not reported as a stale one. A gate that has become
// fit in between, or answers with something else, is reported as stale.
func gateRefusal(root, gate, name string) *Refusal {
	cmd := exec.Command(gate, "-version", "-json")
	cmd.Dir = root
	out, err := cmd.Output()
	var exit *exec.ExitError
	var r Refusal
	refused := errors.As(err, &exit) && exit.ExitCode() == ExitRefused
	if !refused || json.Unmarshal(out, &r) != nil || r.Condition == "" || r.Recovery == "" {
		_, relative := MakeCommands(root)
		r = Refusal{Condition: RefusalStale, Recovery: relative}
	}
	r.Detail = "bin/gate refused to measure " + name + " (" + r.Condition + ")"
	return &r
}

// reportedExit is an outcome already written on stdout: the process ends with
// code and says nothing more about it.
type reportedExit struct{ code int }

func (e *reportedExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// refuse writes a refusal as the result in mode and ends the run with the
// refusal status.
func refuse(stdout io.Writer, mode OutputMode, r *Refusal) error {
	if mode == ModeJSON {
		body, _ := json.Marshal(r)
		fmt.Fprintf(stdout, "%s\n", body)
	} else {
		fmt.Fprintln(stdout, "run: "+r.Error())
	}
	return &reportedExit{code: ExitRefused}
}

// metaBuilderName is the one cmd/ directory that is not a command: ./make
// builds the others and is not itself installed into bin/, so it can collide
// with nothing and belongs in no listing.
const metaBuilderName = "make"

// SupportedCommands lists the commands this project BUILDS: one directory per
// command under tools/build/cmd, minus the meta-builder. Every one of them,
// not a filtered subset.
//
// The workspace asks this list which names in bin/ belong to the project's own
// builder. It uses the answer to refuse a release that would install over one
// of them, and to decide which recorded names it may delete — so a project that
// under-reports gets its own tools silently overwritten by `workspace setup`,
// or removed by `workspace update` (workspace tool-contract.md §5).
//
// It reports what the project builds, not what is built: bin/ is empty in a
// fresh clone, and the claim has to exist before either builder has run.
func SupportedCommands(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, "tools", "build", "cmd"))
	if err != nil {
		// No command directory is a project with no commands. That is what the
		// caller needs to know, and it is not an error to report here.
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != metaBuilderName {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// CommandGateCollisions returns the names that are both a command this project
// builds and a gate it answers, sorted as SupportedCommands sorts.
//
// `bin/run <name>` dispatches both kinds, so such a name would mean one of two
// things. It is an error rather than a precedence rule: precedence would make
// the shadowed name silently unreachable while both still appeared in --list.
func CommandGateCollisions(root string) []string {
	gates := ContractGateNames()
	var both []string
	for _, c := range SupportedCommands(root) {
		if slices.Contains(gates, c) {
			both = append(both, c)
		}
	}
	return both
}

// commandSerialize is the exclusion each command declares it must hold while
// it runs, by command name; a command absent here declares none. Only the
// project can say what its commands cost, so the declaration is the project's
// (flow docs/gates-and-commands.md § Two scopes). verify holds the host-scope
// exclusion: a runner takes it before spawning verify and reports the queue as
// waiting, outside the run's allowance — and acquireHostScope joins that same
// exclusion rather than a lock of its own, so the runner's queue is the only
// one (#96).
var commandSerialize = map[string]string{"verify": serializeHost}

// commandRow is one command as `bin/run --list -json` writes it.
type commandRow struct {
	Name      string `json:"name"`
	Serialize string `json:"serialize,omitempty"`
}

// commandRows is the `commands` half of the JSON listing: one object per
// command, carrying the exclusion it declares. An absent list is an empty one
// rather than null: a reader asking what this project provides gets an answer.
func commandRows(commands []string) []commandRow {
	rows := make([]commandRow, 0, len(commands))
	for _, c := range commands {
		rows = append(rows, commandRow{Name: c, Serialize: commandSerialize[c]})
	}
	return rows
}

// writeRunList prints what this project provides: the gates — the same names
// `bin/gate --list` prints, from the same registry, so the two cannot drift —
// and the commands, which the gate entry point knows nothing about. A name that
// is both is an error rather than a listing that describes two things.
func writeRunList(root string, w io.Writer, mode OutputMode) error {
	if both := CommandGateCollisions(root); len(both) > 0 {
		return fmt.Errorf("%v are both a command this project builds and a gate it answers, so `bin/run <name>` would mean two things", both)
	}
	gates, commands := ContractGateNames(), SupportedCommands(root)
	if mode == ModeJSON {
		return writeJSONLine(w, struct {
			Commands []commandRow `json:"commands"`
			Gates    []string     `json:"gates"`
		}{Commands: commandRows(commands), Gates: gates})
	}
	var b bytes.Buffer
	b.WriteString("gates:\n")
	for _, g := range gates {
		fmt.Fprintf(&b, "  %s\n", g)
	}
	b.WriteString("commands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %s\n", c)
	}
	_, err := w.Write(b.Bytes())
	return err
}

// runProjectCommand execs bin/<name> with the argv given after it, and makes
// the child's exit status this process's. A command this project builds whose
// binary is absent is a refusal — the tool is not fit to run it — and the
// recovery is the build.
//
// It measures and judges NOTHING: no envelope is read and no terms are loaded.
// A `run` that interpreted a command's output would be deciding something no
// gate measured, which is the whole separation the gate/judge split exists for.
func runProjectCommand(root, name string, args []string, mode OutputMode, stdin io.Reader, stdout io.Writer) error {
	bin := filepath.Join(root, "bin", name+ExeSuffix())
	if !Exists(bin) {
		_, relative := MakeCommands(root)
		return refuse(stdout, mode, &Refusal{Condition: RefusalUnbuilt, Recovery: relative,
			Detail: fmt.Sprintf("this project builds %s, but bin/%s is not there", name, name+ExeSuffix())})
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.Exited() {
		return &reportedExit{code: exit.ExitCode()}
	}
	return err
}
