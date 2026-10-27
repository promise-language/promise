package common

// The one invocation layer for the tools a person or an agent runs by hand —
// bin/gate, bin/run, bin/verify and ./make — so that each binds the rules of
// docs/org/cli-guide.md once, here, instead of four hand-rolled parsers drifting
// apart. A tool declares what it accepts (ToolSpec) and gets back either an
// Invocation or one *UsageError listing every problem with the command line.
//
// The rules this file holds, and why they are not left to each tool:
//
//   - One order. The command path comes first, then flags; a word that is not a
//     flag anywhere else is an error, never a positional. A parser that stopped
//     at the first non-flag and passed the rest on would silently reinterpret
//     `gate tested:go x -json`.
//   - Nothing is ignored. An unknown flag, a flag given twice, a boolean given a
//     value, a key in -json-input that no flag answers to — each is a usage
//     error, and every one of them is reported, so a caller fixes its command
//     line in one round instead of one problem per run.
//   - No precedence. A parameter given both in the -json-input file and on the
//     command line is refused rather than resolved: precedence is a fallback, and
//     the guide permits none.
//
// Usage errors exit 2 having written nothing on stdout (Fail); stdout carries
// only the result, and narration goes to stderr only when someone is watching
// (Narration).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// FlagType is the type a flag's definition carries, printed by -help.
type FlagType string

const (
	FlagBoolean     FlagType = "boolean"
	FlagPath        FlagType = "path"
	FlagEnumeration FlagType = "enumeration"
)

// FlagSpec is one flag a tool or command declares.
type FlagSpec struct {
	Name string
	// Type is the flag's type. Tools declare only booleans today (an empty Type
	// means boolean); path and enumeration are used by the built-ins.
	Type        FlagType
	Description string
	// FixesOutput marks a flag that puts the command into a mode whose output a
	// contract of its own fixes (bin/gate <name> --envelope, bin/run <gate> --verdict):
	// when it is asserted, -json/-human are a usage error ("… writes one form only").
	FixesOutput bool
}

// CommandSpec is a class of commands the tool answers but does not enumerate
// (bin/gate's "<gate>", bin/run's "<gate> | <command>"). The tool, not this
// layer, decides whether a given word is a member: the set lives with the tool,
// and -help points at the command that lists it rather than copying it.
type CommandSpec struct {
	Name         string // the pattern, e.g. "<gate>"
	Description  string
	EnumeratedBy string // e.g. "gate --list"
	Flags        []FlagSpec
}

// ToolSpec is everything a tool accepts on its command line.
type ToolSpec struct {
	Tool        string        // bare tool name used in messages: "gate", "run", "verify", "make"
	Description string        // one line
	Flags       []FlagSpec    // the root command's own flags (gate/run: list; make: rebuild; verify: none)
	Commands    []CommandSpec // at most one class entry; empty for verify and make
	// PassThrough, when non-nil and true for the command word, means everything
	// after that word is the command's own argv, handed over verbatim (bin/run <command> [args…]).
	PassThrough func(command string) bool
}

// OutputMode is the form a tool writes its result in.
type OutputMode int

const (
	ModeJSON OutputMode = iota
	ModeHuman
)

// Invocation is a command line that passed every rule.
type Invocation struct {
	Command    string          // "" when no command path was given
	Flags      map[string]bool // declared booleans: true for -x, false for -no-x; absent when not given
	Args       []string        // the pass-through argv of a PassThrough command
	Mode       OutputMode
	ModeForced bool // -json or -human given
	Narrate    bool
	Help       bool
	Version    bool
}

// The built-in parameters every invocation accepts. output-mode is one
// enumeration parameter with two spellings, -json and -human; it has a name of
// its own because the -json-input file and -help must call it something, and
// calling it "json" would make {"json": false} mean "human".
const (
	outputModeParam = "output-mode"
	narrateParam    = "narrate"
	helpParam       = "help"
	versionParam    = "version"
	jsonInputParam  = "json-input"
	argsKey         = "args"
)

// reservedNames may not be declared by a tool: each already means something on
// every invocation or in every -json-input file.
var reservedNames = map[string]bool{
	"json": true, "human": true, outputModeParam: true, narrateParam: true,
	helpParam: true, versionParam: true, jsonInputParam: true, argsKey: true,
}

// stdoutIsTerminal and stderrIsTerminal are variables so tests can decide what
// the streams are without needing a terminal.
var (
	stdoutIsTerminal = func() bool { return IsTerminal(os.Stdout) }
	stderrIsTerminal = func() bool { return IsTerminal(os.Stderr) }
)

// UsageError is a malformed invocation: exit 2, nothing done. It carries every
// problem found, not just the first.
type UsageError struct {
	Tool     string
	Problems []string
	// Raw marks Problems as preformatted lines, rendered verbatim without the
	// "<tool>: " prefix (BareInvocation's answer is a short page, not a list of
	// complaints).
	Raw bool
}

// Error renders one problem per line, each prefixed "<tool>: " unless Raw.
func (e *UsageError) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		if e.Raw {
			lines[i] = p
		} else {
			lines[i] = e.Tool + ": " + p
		}
	}
	return strings.Join(lines, "\n")
}

// ExitCode maps an error to the guide's exit codes: nil → 0, a malformed
// invocation → 2, an outcome already reported → the status it carries (a
// refusal's 3, a pass-through command's own), anything else (could not
// complete) → 1.
func ExitCode(err error) int {
	var ue *UsageError
	var reported *reportedExit
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		return 2
	case errors.As(err, &reported):
		return reported.code
	default:
		return 1
	}
}

// Fail prints err for a person on stderr and returns its exit code: a
// *UsageError prints its lines as they are; an outcome already reported prints
// nothing more; any other error prints "<tool>: <err>". Each tool's main ends
// os.Exit(common.Fail("gate", err)).
//
// It writes to os.Stderr itself, never to Narration(): an error is for whoever
// ran the tool even when nobody is watching the progress, and stdout is never
// touched, so a caller parsing the result reads nothing rather than a complaint.
func Fail(tool string, err error) int {
	if err == nil {
		return 0
	}
	var reported *reportedExit
	if errors.As(err, &reported) {
		return reported.code
	}
	var ue *UsageError
	msg := tool + ": " + err.Error()
	if errors.As(err, &ue) {
		msg = ue.Error()
	}
	fmt.Fprintln(os.Stderr, strings.TrimRight(msg, "\n"))
	return ExitCode(err)
}

// ParseInvocation applies every rule above to argv (os.Args[1:]). The error,
// when there is one, is a *UsageError naming every problem found.
func ParseInvocation(spec ToolSpec, argv []string) (Invocation, error) {
	validateSpec(spec)
	p := &invocationParser{spec: spec, given: map[string]origin{}}
	p.lex(argv)
	p.loadJSONInput()
	p.commandFromInput()
	for _, w := range p.words {
		p.resolveWord(w)
	}
	for _, e := range p.entries {
		p.resolveEntry(e)
	}
	p.crossCheck()
	var problems []string
	for _, pr := range slices.Concat(p.lineProblems, p.fileProblems, p.crossProblems) {
		if !slices.Contains(problems, pr) { // -list -list -list is one problem
			problems = append(problems, pr)
		}
	}
	if len(problems) > 0 {
		return Invocation{}, &UsageError{Tool: spec.Tool, Problems: problems}
	}
	return p.invocation(), nil
}

// ObservedFor reports whether anyone is watching this invocation's narration:
// it was asked for with -narrate, or stderr is a terminal. The tool passes the
// answer to SetObserved.
func ObservedFor(inv Invocation) bool {
	return inv.Narrate || stderrIsTerminal()
}

// cliWord is one word of the command line after the command path, before it
// is resolved against the flags that apply.
//
// Lexing and resolving are separate steps because which flags apply depends on
// whether there is a command, and the command may arrive in the -json-input
// file's "args" — which is only known once the file named on the command line
// has been read. Only -json-input takes a value, so where each word ends does
// not depend on the command.
type cliWord struct {
	text     string // the word as typed
	flag     bool
	spelling string // the flag as typed without any "=value": "--list"
	name     string // after the dashes: "list"
	value    string
	hasValue bool
}

// inputEntry is one key of the -json-input object, in file order.
type inputEntry struct {
	key   string
	value any
}

// origin is where a parameter was first given, and as what.
type origin struct {
	fromFile bool
	form     string // the normalized spelling: "json", "list", "no-list"
	spelling string // for messages: as typed on the command line, "-<form>" from the file
}

type invocationParser struct {
	spec ToolSpec

	command         string
	commandFromLine bool
	commandFromFile bool
	passThrough     bool
	args            []string

	words     []cliWord
	inputPath string // the -json-input path as typed; "" when none was given
	entries   []inputEntry

	given map[string]origin
	order []string // parameter names in the order first given

	// Problems in the order a person reads them: the command line, then the
	// file, then the ones that only exist between parameters.
	lineProblems, fileProblems, crossProblems []string
}

func (p *invocationParser) problem(fromFile bool, format string, a ...any) {
	if fromFile {
		p.fileProblems = append(p.fileProblems, fmt.Sprintf(format, a...))
	} else {
		p.lineProblems = append(p.lineProblems, fmt.Sprintf(format, a...))
	}
}

func (p *invocationParser) lex(argv []string) {
	i := 0
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		i = 1
		if p.lineCommand(argv[0]) && p.passThrough {
			p.args = slices.Clone(argv[1:])
			return
		}
	}
	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			rest := argv[i+1:]
			// After --, the first word is the command only when nothing came
			// before the marker: it is how a command beginning with "-" is given.
			if i == 0 && len(rest) > 0 {
				ok := p.lineCommand(rest[0])
				rest = rest[1:]
				if ok && p.passThrough {
					p.args = slices.Clone(rest)
					return
				}
			}
			for _, r := range rest {
				p.words = append(p.words, cliWord{text: r})
			}
			return
		}
		if !strings.HasPrefix(a, "-") {
			p.words = append(p.words, cliWord{text: a})
			continue
		}
		// -x and --x are one flag: strip at most two dashes, then match exactly.
		body := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		name, value, hasValue := strings.Cut(body, "=")
		w := cliWord{text: a, flag: true, spelling: a[:len(a)-len(body)] + name, name: name, value: value, hasValue: hasValue}
		if name == jsonInputParam && !hasValue && i+1 < len(argv) {
			i++
			w.value, w.hasValue = argv[i], true
		}
		p.words = append(p.words, w)
	}
}

// lineCommand takes c as the command path. A tool without commands, or an
// empty word, has no command to take: the word is kept as a positional, and
// reported where it stands.
func (p *invocationParser) lineCommand(c string) bool {
	if len(p.spec.Commands) == 0 || c == "" {
		p.words = append(p.words, cliWord{text: c})
		return false
	}
	p.command, p.commandFromLine = c, true
	p.passThrough = p.spec.PassThrough != nil && p.spec.PassThrough(c)
	return true
}

func (p *invocationParser) unexpected(arg, where string) string {
	if len(p.spec.Commands) == 0 {
		return fmt.Sprintf("unexpected argument %q%s: %s takes no command and no positional", arg, where, p.spec.Tool)
	}
	return fmt.Sprintf("unexpected argument %q%s: %s takes its command first and no other positional", arg, where, p.spec.Tool)
}

// loadJSONInput reads the first -json-input file named on the command line.
// The object is walked token by token rather than decoded into a map so that a
// key given twice is seen (a map keeps the last silently) and the problems come
// out in file order.
func (p *invocationParser) loadJSONInput() {
	for _, w := range p.words {
		if w.flag && w.name == jsonInputParam && w.hasValue && w.value != "" {
			p.inputPath = w.value
			break
		}
	}
	if p.inputPath == "" {
		return
	}
	path := p.inputPath
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	data, err := os.ReadFile(path)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		p.problem(true, "-json-input %s cannot be read: %v", p.inputPath, err)
		return
	}
	entries, err := decodeInputObject(data)
	if err != nil {
		p.problem(true, "-json-input %s %v", p.inputPath, err)
		return
	}
	p.entries = entries
}

var errNotOneObject = errors.New("must hold one JSON object")

func decodeInputObject(data []byte) ([]inputEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	invalid := func(err error) error { return fmt.Errorf("is not valid JSON: %v", err) }
	tok, err := dec.Token()
	if err == io.EOF {
		return nil, errNotOneObject
	}
	if err != nil {
		return nil, invalid(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotOneObject
	}
	var entries []inputEntry
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, invalid(err)
		}
		key, _ := tok.(string) // the decoder yields only strings in key position
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, invalid(err)
		}
		entries = append(entries, inputEntry{key: key, value: v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, invalid(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errNotOneObject
	}
	return entries, nil
}

// commandFromInput takes the command from the file's "args", whose first
// element is the command path and whose rest are positionals — refused unless
// the command passes them through.
func (p *invocationParser) commandFromInput() {
	for _, e := range p.entries {
		if e.key != argsKey {
			continue
		}
		list, ok := stringList(e.value)
		if !ok {
			p.problem(true, "-json-input %s: %q must be an array of strings", p.inputPath, argsKey)
			return
		}
		if len(list) == 0 {
			return
		}
		if p.commandFromLine {
			p.problem(true, "the command is given both in %s and on the command line", p.inputPath)
			return
		}
		where := " in " + p.inputPath
		if len(p.spec.Commands) == 0 || list[0] == "" {
			for _, a := range list {
				p.problem(true, "%s", p.unexpected(a, where))
			}
			return
		}
		p.command, p.commandFromFile = list[0], true
		p.passThrough = p.spec.PassThrough != nil && p.spec.PassThrough(list[0])
		if p.passThrough {
			p.args = slices.Clone(list[1:])
			return
		}
		for _, a := range list[1:] {
			p.problem(true, "%s", p.unexpected(a, where))
		}
		return // a second "args" is reported by resolveEntry
	}
}

func stringList(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	list := make([]string, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, false
		}
		list[i] = s
	}
	return list, true
}

// declared is the tool's own flags that apply: the class's after a command,
// the root's without one.
func (p *invocationParser) declared() []FlagSpec {
	if p.command != "" {
		return p.spec.Commands[0].Flags
	}
	return p.spec.Flags
}

func (p *invocationParser) lookup(name string) (FlagSpec, bool) {
	for _, f := range p.declared() {
		if f.Name == name {
			return f, true
		}
	}
	return FlagSpec{}, false
}

func (p *invocationParser) resolveWord(w cliWord) {
	if !w.flag {
		p.problem(false, "%s", p.unexpected(w.text, ""))
		return
	}
	name := w.name
	switch {
	case name == "json" || name == "human":
		if w.hasValue {
			p.problem(false, "%s takes no value: give -json or -human", w.spelling)
			return
		}
		p.set(outputModeParam, name, w.spelling, false)
	case name == narrateParam || name == helpParam || name == versionParam:
		if w.hasValue {
			p.problem(false, "%s takes no value: give -%s or leave it out", w.spelling, name)
			return
		}
		p.set(name, name, w.spelling, false)
	case name == jsonInputParam:
		if !w.hasValue || w.value == "" {
			p.problem(false, "%s needs a path: -json-input <path>", w.spelling)
			return
		}
		p.set(jsonInputParam, name, w.spelling, false)
	default:
		// No declared name begins "no-" (validateSpec), so this is the flag
		// whether name asserted or denied it.
		param := strings.TrimPrefix(name, "no-")
		if _, ok := p.lookup(param); !ok {
			p.unknown(w.spelling, name, false)
			return
		}
		if w.hasValue {
			p.problem(false, "%s takes no value: a boolean is -%s or -no-%s", w.spelling, param, param)
			return
		}
		p.set(param, name, w.spelling, false)
	}
}

func (p *invocationParser) resolveEntry(e inputEntry) {
	bad := func(want string) {
		p.problem(true, "-json-input %s: %q must be %s", p.inputPath, e.key, want)
	}
	switch key := e.key; {
	case key == argsKey:
		if _, seen := p.given[argsKey]; seen {
			p.problem(true, "%q is given twice in %s", argsKey, p.inputPath)
		}
		p.given[argsKey] = origin{fromFile: true} // not in p.order: the command is not a flag
	case key == outputModeParam:
		s, ok := e.value.(string)
		if !ok || (s != "json" && s != "human") {
			bad(`"json" or "human"`)
			return
		}
		p.set(outputModeParam, s, "-"+s, true)
	case key == narrateParam || key == helpParam || key == versionParam:
		b, ok := e.value.(bool)
		switch {
		case !ok:
			bad("true or false")
		case !b:
			// The file maps exactly onto the command line, and there is no
			// -no-narrate to map false onto.
			p.problem(true, "-json-input %s: %q can only be true: -%s has no -no- form, so leave the key out", p.inputPath, key, key)
		default:
			p.set(key, key, "-"+key, true)
		}
	default:
		if _, ok := p.lookup(key); !ok {
			p.unknown("", key, true)
			return
		}
		b, ok := e.value.(bool)
		if !ok {
			bad("true or false")
			return
		}
		form := key
		if !b {
			form = "no-" + key
		}
		p.set(key, form, "-"+form, true)
	}
}

// set records that param was given, as form, and reports it when it already
// was: there is one way to give each parameter, once.
func (p *invocationParser) set(param, form, spelling string, fromFile bool) {
	prev, seen := p.given[param]
	if !seen {
		p.given[param] = origin{fromFile: fromFile, form: form, spelling: spelling}
		p.order = append(p.order, param)
		return
	}
	switch {
	case prev.fromFile && fromFile:
		p.problem(true, "%q is given twice in %s", param, p.inputPath)
	case prev.fromFile || fromFile:
		line := spelling
		if fromFile {
			line = prev.spelling
		}
		p.problem(fromFile, "%s is given both in %s and on the command line", line, p.inputPath)
	case param == outputModeParam && prev.form != form:
		p.problem(false, "-json and -human choose one output mode; give one")
	case prev.form == form:
		p.problem(false, "-%s is given twice", form)
	default:
		p.problem(false, "-%s is given twice, as -%s and as -%s", param, prev.form, form)
	}
}

// unknown reports a flag nothing answers to, with the nearest one that does
// and where to find the rest — not the list itself, which would bury the one
// fact the caller needs.
func (p *invocationParser) unknown(spelling, name string, fromFile bool) {
	var msg string
	if fromFile {
		msg = fmt.Sprintf("unknown flag %q in %s", name, p.inputPath)
		if c := closestName(name, p.inputKeys()); c != "" {
			msg += fmt.Sprintf("; the closest is %q", c)
		}
	} else {
		msg = "unknown flag " + spelling
		if c := closestName(name, p.lineSpellings()); c != "" {
			msg += "; the closest is -" + c
		}
	}
	p.problem(fromFile, "%s; run `%s -help` for every flag", msg, p.spec.Tool)
}

// lineSpellings is every flag spelling the command line accepts here.
func (p *invocationParser) lineSpellings() []string {
	names := []string{"json", "human", narrateParam, helpParam, versionParam, jsonInputParam}
	for _, f := range p.declared() {
		names = append(names, f.Name, "no-"+f.Name)
	}
	return names
}

// inputKeys is every key the -json-input file accepts here.
func (p *invocationParser) inputKeys() []string {
	names := []string{argsKey, outputModeParam, narrateParam, helpParam, versionParam}
	for _, f := range p.declared() {
		names = append(names, f.Name)
	}
	return names
}

// closestName is the candidate nearest to name: within two edits, or sharing a
// prefix with it ("-h" → "-help", "-list:true" → "-list"). A leading "no-" is
// also tried off, so "-no-help" is pointed at "-help". "" when nothing is close.
func closestName(name string, candidates []string) string {
	best, bestDist := "", -1
	for _, typed := range []string{name, strings.TrimPrefix(name, "no-")} {
		for _, c := range candidates {
			d := levenshtein(typed, c)
			prefix := typed != "" && (strings.HasPrefix(typed, c) || strings.HasPrefix(c, typed))
			if d > 2 && !prefix {
				continue
			}
			if bestDist < 0 || d < bestDist {
				best, bestDist = c, d
			}
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// crossCheck holds the rules that are about parameters together rather than
// any one of them.
func (p *invocationParser) crossCheck() {
	cross := func(format string, a ...any) {
		p.crossProblems = append(p.crossProblems, fmt.Sprintf(format, a...))
	}
	// A pass-through command's own argv is everything after it; in the file
	// that is the rest of "args", so no flag of this tool can apply to it.
	if p.passThrough && p.commandFromFile {
		for _, param := range p.order {
			if param != jsonInputParam {
				cross("%s does not apply to %q, whose arguments are its own: put them in %q", p.given[param].spelling, p.command, argsKey)
			}
		}
	}
	// -help and -version answer about the tool, not about an invocation of it:
	// anything beside them would be silently ignored.
	_, help := p.given[helpParam]
	_, version := p.given[versionParam]
	if help || version {
		beside := (help && version) || p.command != ""
		for _, param := range p.order {
			switch param {
			case outputModeParam, jsonInputParam, helpParam, versionParam:
			default:
				beside = true
			}
		}
		if beside {
			which := "-help"
			if !help {
				which = "-version"
			}
			cross("%s takes nothing beside it but -json or -human", which)
		}
	}
	// A mode whose output its own contract fixes has one form; choosing a form
	// for it is a contradiction, not a preference.
	if mode, forced := p.given[outputModeParam]; forced {
		for _, param := range p.order {
			f, ok := p.lookup(param)
			if ok && f.FixesOutput && p.given[param].form == param {
				cross("%s writes one form only, fixed by its contract; drop %s", p.given[param].spelling, mode.spelling)
			}
		}
	}
}

func (p *invocationParser) invocation() Invocation {
	inv := Invocation{Command: p.command, Flags: map[string]bool{}, Args: p.args}
	for _, f := range p.declared() {
		if o, ok := p.given[f.Name]; ok {
			inv.Flags[f.Name] = o.form == f.Name
		}
	}
	_, inv.Narrate = p.given[narrateParam]
	_, inv.Help = p.given[helpParam]
	_, inv.Version = p.given[versionParam]
	if o, ok := p.given[outputModeParam]; ok {
		inv.ModeForced = true
		if o.form == "human" {
			inv.Mode = ModeHuman
		}
	} else if stdoutIsTerminal() {
		inv.Mode = ModeHuman
	}
	return inv
}

// validateSpec refuses a spec no command line could be parsed against: a flag
// that shadows a built-in, a name outside the guide's alphabet, a name whose
// -no- spelling would be another flag. It panics because the spec is code, and
// the first test of the tool declaring it fails.
func validateSpec(spec ToolSpec) {
	if len(spec.Commands) > 1 {
		panic(fmt.Sprintf("common: %s declares %d command classes; at most one is supported", spec.Tool, len(spec.Commands)))
	}
	check := func(where string, flags []FlagSpec) {
		seen := map[string]bool{}
		for _, f := range flags {
			if !validName(f.Name) || reservedNames[f.Name] || strings.HasPrefix(f.Name, "no-") || seen[f.Name] {
				panic(fmt.Sprintf("common: %s declares flag %q, which is not a valid, distinct flag name", where, f.Name))
			}
			if flagType(f) != FlagBoolean {
				panic(fmt.Sprintf("common: %s declares %s flag %q; only boolean flags can be declared", where, f.Type, f.Name))
			}
			seen[f.Name] = true
		}
	}
	check(spec.Tool, spec.Flags)
	for _, c := range spec.Commands {
		check(spec.Tool+" "+c.Name, c.Flags)
	}
}

// validName is the guide's alphabet: lowercase a–z and 0–9, "-" only between.
func validName(s string) bool {
	if s == "" || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func flagType(f FlagSpec) FlagType {
	if f.Type == "" {
		return FlagBoolean
	}
	return f.Type
}

// helpFlag, helpCommand and helpDocument are -help's JSON shape. The human form
// renders the same document, so the two cannot state different facts.
type helpFlag struct {
	Name        string   `json:"name"`
	Spellings   []string `json:"spellings,omitempty"`
	Type        FlagType `json:"type"`
	Description string   `json:"description"`
}

type helpCommand struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	EnumeratedBy string     `json:"enumerated-by"`
	Flags        []helpFlag `json:"flags"`
}

type helpDocument struct {
	// Project is the repository's name, never the tool's: -help and -version
	// say what the binary belongs to.
	Project     string        `json:"project"`
	Description string        `json:"description"`
	Flags       []helpFlag    `json:"flags"`
	Commands    []helpCommand `json:"commands"`
}

const projectName = "promise"

// builtinHelpFlags describes the flags every invocation accepts, in the order
// -help lists them.
func builtinHelpFlags() []helpFlag {
	return []helpFlag{
		{Name: outputModeParam, Spellings: []string{"json", "human"}, Type: FlagEnumeration,
			Description: "write the result as JSON or for a person; without either, for a person when stdout is a terminal and JSON when it is not"},
		{Name: narrateParam, Type: FlagBoolean,
			Description: "narrate progress on stderr even when stderr is not a terminal"},
		{Name: versionParam, Type: FlagBoolean,
			Description: "print the tools source hash this binary was built from, and nothing else"},
		{Name: helpParam, Type: FlagBoolean,
			Description: "print every command and flag, and nothing else"},
		{Name: jsonInputParam, Type: FlagPath,
			Description: `read the invocation from a JSON object: one key per flag, "output-mode" for -json/-human, and "args" for the command`},
	}
}

func declaredHelpFlags(flags []FlagSpec) []helpFlag {
	out := make([]helpFlag, 0, len(flags))
	for _, f := range flags {
		out = append(out, helpFlag{Name: f.Name, Spellings: []string{f.Name, "no-" + f.Name}, Type: flagType(f), Description: f.Description})
	}
	return out
}

// WriteHelp writes the tool's whole surface: every flag with its spellings,
// type and description, and the command class with the command that lists its
// members — never the members themselves, which only the tool knows.
func WriteHelp(w io.Writer, spec ToolSpec, mode OutputMode) error {
	validateSpec(spec)
	doc := helpDocument{
		Project:     projectName,
		Description: spec.Description,
		Flags:       append(declaredHelpFlags(spec.Flags), builtinHelpFlags()...),
		Commands:    []helpCommand{},
	}
	for _, c := range spec.Commands {
		doc.Commands = append(doc.Commands, helpCommand{Name: c.Name, Description: c.Description, EnumeratedBy: c.EnumeratedBy, Flags: declaredHelpFlags(c.Flags)})
	}
	var buf bytes.Buffer
	if mode == ModeJSON {
		if err := encodeLine(&buf, doc); err != nil {
			return err
		}
	} else {
		writeHumanHelp(&buf, spec.Tool, doc, len(spec.Flags))
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// writeHumanHelp renders doc for a person. The first own entries of doc.Flags
// are the root command's; the rest are the built-ins, which — unlike the
// root's — also follow a command, so the text keeps the two apart.
func writeHumanHelp(b *bytes.Buffer, tool string, doc helpDocument, own int) {
	fmt.Fprintf(b, "%s (%s): %s\n\nusage:\n", tool, doc.Project, doc.Description)
	for _, c := range doc.Commands {
		fmt.Fprintf(b, "  %s %s [flags]\n", tool, c.Name)
	}
	fmt.Fprintf(b, "  %s [flags]\n", tool)
	for _, c := range doc.Commands {
		fmt.Fprintf(b, "\ncommand %s\n    %s\n    listed by `%s`\n", c.Name, c.Description, c.EnumeratedBy)
		if len(c.Flags) > 0 {
			b.WriteString("    flags after it, beside those of every invocation:\n")
			for _, f := range c.Flags {
				writeHumanFlag(b, "      ", f)
			}
		}
	}
	if own > 0 {
		b.WriteString("\nflags without a command:\n")
		for _, f := range doc.Flags[:own] {
			writeHumanFlag(b, "  ", f)
		}
	}
	b.WriteString("\nflags of every invocation:\n")
	for _, f := range doc.Flags[own:] {
		writeHumanFlag(b, "  ", f)
	}
}

func writeHumanFlag(b *bytes.Buffer, indent string, f helpFlag) {
	spellings := f.Spellings
	if len(spellings) == 0 {
		spellings = []string{f.Name}
	}
	head := "-" + strings.Join(spellings, ", -")
	kind := string(f.Type)
	switch f.Type {
	case FlagPath:
		head += " <path>"
	case FlagEnumeration:
		kind += " " + f.Name
	}
	fmt.Fprintf(b, "%s%s  (%s)\n%s    %s\n", indent, head, kind, indent, f.Description)
}

// encodeLine writes v as one line of JSON. HTML escaping is off: "<gate>" is a
// command pattern, and <gate> would be the same string to a decoder
// and unreadable to everyone else.
func encodeLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// WriteVersion writes the one line -version answers. text is the caller's: the
// tools source hash — the stamped one for a tool, the one ./make computes — or
// "unknown".
func WriteVersion(w io.Writer, mode OutputMode, text string) error {
	if mode == ModeJSON {
		var buf bytes.Buffer
		if err := encodeLine(&buf, struct {
			Project string `json:"project"`
			Text    string `json:"text"`
		}{projectName, text}); err != nil {
			return err
		}
		_, err := w.Write(buf.Bytes())
		return err
	}
	_, err := io.WriteString(w, projectName+" "+text+"\n")
	return err
}

// BareInvocation is the answer to a tool that needs a command and was given
// nothing at all: what it expects, what it is, and where the rest is. It is a
// usage error (exit 2) because nothing was asked, but it reads as a short page
// rather than a complaint, so its lines carry no "<tool>: " prefix.
func BareInvocation(spec ToolSpec, versionText string) *UsageError {
	if len(spec.Commands) == 0 {
		panic(fmt.Sprintf("common: BareInvocation for %s, which declares no command class", spec.Tool))
	}
	c := spec.Commands[0]
	return &UsageError{Tool: spec.Tool, Raw: true, Problems: []string{
		fmt.Sprintf("expecting a subcommand: %s, %s — `%s` names them", c.Name, c.Description, c.EnumeratedBy),
		projectName + " " + versionText,
		spec.Description,
		fmt.Sprintf("run `%s -help` for all of them", spec.Tool),
	}}
}

// Narration. Progress is for someone watching; when no one is (stderr is not a
// terminal and -narrate was not given) it goes nowhere, so an unattended run's
// stderr holds only what went wrong. The zero state is observed, so a tool that
// never calls SetObserved narrates exactly as before.
var (
	unobserved  atomic.Bool
	devNullOnce sync.Once
	devNull     *os.File
)

// SetObserved records whether anyone is watching (see ObservedFor).
func SetObserved(observed bool) { unobserved.Store(!observed) }

// Observed reports what SetObserved last recorded; true before it is called.
func Observed() bool { return !unobserved.Load() }

// Narration is where narration goes: os.Stderr when observed, else the null
// device, opened once. It resolves os.Stderr at call time, so a test that swaps
// the variable sees the swap. Should the null device fail to open, narration
// falls back to stderr: noise nobody asked for beats a nil file a child
// process's Stderr would fail on.
func Narration() *os.File {
	if Observed() {
		return os.Stderr
	}
	devNullOnce.Do(func() {
		if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
			devNull = f
		}
	})
	if devNull == nil {
		return os.Stderr
	}
	return devNull
}
