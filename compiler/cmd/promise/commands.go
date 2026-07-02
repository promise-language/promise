package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdNode is a node in the command tree — the single source of truth for the
// concise command index, the hierarchical help tree (`-h`/`-help`/`--help` on
// every node plus `promise help <path...>`), and the help-coverage tests.
// (T1006)
type cmdNode struct {
	name    string
	summary string            // one-line description for the index and synthesized help
	group   string            // group key (top-level nodes); drives index ordering
	hidden  bool              // excluded from the index (deprecated aliases; fetch/warm pending T1008, gc removed T1009)
	subs    []*cmdNode        // subcommands
	help    func(w io.Writer) // optional rich help renderer; nil → synthesized from summary/subs
}

// groupOrder is the deterministic order groups are rendered in the index. It
// mirrors docs/language-design.md §2.
var groupOrder = []string{
	"Compile & run",
	"Documentation & discovery",
	"Project & dependencies",
	"Toolchain & cache",
	"Tooling",
	"Compiler debugging",
}

// commandTree is the registry of every top-level command (and its
// subcommands). Order within a group follows docs/language-design.md §2.
// Removed verbs (fetch/warm/gc) and deprecated aliases are registered as
// `hidden` so routing and the coverage test still see them, but the index does
// not advertise them (§5).
var commandTree = []*cmdNode{
	// Compile & run
	{name: "build", group: "Compile & run", summary: "Compile a Promise source file or project to an executable"},
	{name: "run", group: "Compile & run", summary: "Compile and run a Promise source file or project"},
	{name: "exec", group: "Compile & run", summary: "Execute inline Promise code (auto-wraps in failable main)"},
	{name: "test", group: "Compile & run", summary: "Discover and run test functions"},
	{name: "check", group: "Compile & run", summary: "Run semantic analysis (type checking) only"},

	// Documentation & discovery
	{name: "help", group: "Documentation & discovery", summary: "Show language overview and quick-start guide"},
	{name: "guide", group: "Documentation & discovery", summary: "Print the full language reference", help: func(w io.Writer) { printGuideUsage(w) }},
	{name: "examples", group: "Documentation & discovery", summary: "Browse and run example programs", help: func(w io.Writer) { printExamplesUsage(w) }},
	{name: "doc", group: "Documentation & discovery", summary: "Document a file/module; list modules with no args", help: func(w io.Writer) { printDocUsage(w) }},
	{name: "targets", group: "Documentation & discovery", summary: "List supported compile targets", help: func(w io.Writer) { printTargetsUsage(w) }},
	{name: "catalog", group: "Documentation & discovery", summary: "Catalog operations", subs: []*cmdNode{
		{name: "list", summary: "List all available catalog modules"},
	}},

	// Project & dependencies
	{name: "init", group: "Project & dependencies", summary: "Initialize a new Promise project or module (creates promise.toml)", help: func(w io.Writer) { printInitUsage(w) }},
	{name: "package", group: "Project & dependencies", summary: "Manage project dependencies", subs: []*cmdNode{
		{name: "add", summary: "Add an external dependency to promise.toml"},
		{name: "remove", summary: "Remove a dependency from promise.toml"},
		{name: "update", summary: "Update dependency pins to latest commits"},
		{name: "search", summary: "Search the catalog for available modules"},
		{name: "pin", summary: "Resolve a remote ref to a commit SHA and pin it"},
		{name: "check-upgrade", summary: "Preview which dependencies support a target epoch"},
		{name: "check-epoch", summary: "Verify THIS module against an epoch (owner self-check)"},
		{name: "build-index", summary: "(CI) verify catalog modules + write the compat index"},
	}},

	// Toolchain & cache
	{name: "install", group: "Toolchain & cache", summary: "Install Promise to PROMISE_HOME (default: ~/.promise/)"},
	{name: "use", group: "Toolchain & cache", summary: "Activate an epoch (downloads from releases if not installed)"},
	{name: "epochs", group: "Toolchain & cache", summary: "List installed epochs"},
	{name: "remove", group: "Toolchain & cache", summary: "Remove an installed epoch (reclaims its exclusive cached blobs)"},
	{name: "update", group: "Toolchain & cache", summary: "Update Promise (follow the release channel)", subs: []*cmdNode{
		{name: "channel", summary: "Show or set the update channel (stable|next)"},
		{name: "check", summary: "Report whether an update is available (no changes)"},
	}},
	{name: "doctor", group: "Toolchain & cache", summary: "Check the local Promise environment for issues"},
	{name: "clean", group: "Toolchain & cache", summary: "Remove build cache (-global also clears module cache)"},

	// Tooling
	{name: "format", group: "Tooling", summary: "Format Promise source files"},
	{name: "bind", group: "Tooling", summary: "Generate Promise bindings from WIT or WebIDL definitions", help: func(w io.Writer) { printBindUsage(w) }},
	{name: "version", group: "Tooling", summary: "Print compiler version", help: func(w io.Writer) { printVersionUsage(w) }},

	// Compiler debugging
	{name: "ast", group: "Compiler debugging", summary: "Print Promise AST"},
	{name: "emit-ir", group: "Compiler debugging", summary: "Print LLVM IR to stdout"},

	// Hidden — registered for routing/coverage but omitted from the index.
	// fetch/warm still dispatch until T1008 removes them; gc now dispatches a
	// removal notice (T1009) but stays routable for muscle-memory redirects (§5).
	{name: "fetch", hidden: true, summary: "(deprecated — folded into install; T1008)"},
	{name: "warm", hidden: true, summary: "(deprecated — folded into install; T1008)"},
	{name: "gc", hidden: true, summary: "(removed — cache reclamation is automatic; see doctor --repair; T1009)"},
	{name: "pkg", hidden: true, summary: "Deprecated alias for package"},
	{name: "add", hidden: true, summary: "Deprecated alias for package add"},
	{name: "search", hidden: true, summary: "Deprecated alias for package search"},
	{name: "pin", hidden: true, summary: "Deprecated alias for package pin"},
}

// indexNameWidth returns the column width for the visible command names.
func indexNameWidth() int {
	width := 0
	for _, n := range commandTree {
		if n.hidden {
			continue
		}
		if len(n.name) > width {
			width = len(n.name)
		}
	}
	return width
}

// printIndex writes the concise, grouped command index to w (the naked-`promise`
// and root-`--help` output). Hidden nodes are omitted (§5).
func printIndex(w io.Writer) {
	fmt.Fprintln(w, "Promise — statically-typed language with Go-like concurrency and Rust-like ownership.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: promise <command> [options] [file.pr]")
	fmt.Fprintln(w)

	width := indexNameWidth()
	for _, g := range groupOrder {
		printed := false
		for _, n := range commandTree {
			if n.hidden || n.group != g {
				continue
			}
			if !printed {
				fmt.Fprintf(w, "%s\n", g)
				printed = true
			}
			fmt.Fprintf(w, "  %-*s  %s\n", width, n.name, n.summary)
		}
		if printed {
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w, "Run 'promise help' for an overview, or 'promise <command> --help' for command details.")
}

// findNode walks the command tree by positional tokens, returning the deepest
// matched node and the path consumed. ok is false when the first token names no
// command. Extra tokens that match no subcommand stop the walk (matched is
// shorter than path).
func findNode(path []string) (node *cmdNode, matched []string, ok bool) {
	if len(path) == 0 {
		return nil, nil, false
	}
	var cur *cmdNode
	for _, n := range commandTree {
		if n.name == path[0] {
			cur = n
			break
		}
	}
	if cur == nil {
		return nil, nil, false
	}
	matched = []string{path[0]}
	for _, tok := range path[1:] {
		var next *cmdNode
		for _, s := range cur.subs {
			if s.name == tok {
				next = s
				break
			}
		}
		if next == nil {
			break
		}
		cur = next
		matched = append(matched, tok)
	}
	return cur, matched, true
}

// printNodeHelp renders help for a node to w: its rich renderer when present,
// otherwise a synthesized synopsis (group → list of subcommands; leaf → usage
// line + summary).
func printNodeHelp(w io.Writer, node *cmdNode, path []string) {
	if node.help != nil {
		node.help(w)
		return
	}
	full := strings.Join(path, " ")
	if len(node.subs) > 0 {
		fmt.Fprintf(w, "Usage: promise %s <subcommand>\n\n", full)
		if node.summary != "" {
			fmt.Fprintf(w, "%s\n\n", node.summary)
		}
		fmt.Fprintln(w, "Subcommands:")
		width := 0
		for _, s := range node.subs {
			if len(s.name) > width {
				width = len(s.name)
			}
		}
		for _, s := range node.subs {
			fmt.Fprintf(w, "  %-*s  %s\n", width, s.name, s.summary)
		}
		fmt.Fprintf(w, "\nRun 'promise %s <subcommand> --help' for details.\n", full)
		return
	}
	fmt.Fprintf(w, "Usage: promise %s\n", full)
	if node.summary != "" {
		fmt.Fprintf(w, "\n%s\n", node.summary)
	}
}

// routeHelp implements `promise help <path...>`: resolve the path through the
// command tree and print that node's help to stdout (exit 0). With no path it
// prints the overview. An unknown path is a usage error → stderr, exit 1.
func routeHelp(path []string) {
	if len(path) == 0 {
		printHelp()
		return
	}
	node, matched, ok := findNode(path)
	if !ok || len(matched) < len(path) {
		fmt.Fprintf(os.Stderr, "unknown help topic: %s\n", strings.Join(path, " "))
		fmt.Fprintln(os.Stderr, "Run 'promise help' for the command index.")
		os.Exit(1)
	}
	printNodeHelp(os.Stdout, node, matched)
}

// isHelpFlag reports whether arg is a help flag. normalizeArgs already maps the
// double-dash forms to single-dash, so only `-h`/`-help` reach here.
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "-help"
}

// helpHint writes the shared short pointer line used by usage errors.
func helpHint(w io.Writer) {
	fmt.Fprintln(w, "Run 'promise help' or 'promise <command> --help'.")
}

// handleHelp centralizes help interception so every command and subcommand
// responds to `-h`/`-help`/`--help` and `promise help <path...>` uniformly —
// all to stdout, exit 0. It returns true when it handled the invocation.
//
// args is the already-normalized argument list (os.Args[1:]). An unknown
// command carrying a help flag is left for normal dispatch to error out.
func handleHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "help" {
		routeHelp(args[1:])
		return true
	}
	hasFlag := false
	for _, a := range args {
		if isHelpFlag(a) {
			hasFlag = true
			break
		}
	}
	if !hasFlag {
		return false
	}
	// Build the longest leading run of non-flag tokens as the node path.
	var path []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		path = append(path, a)
	}
	node, matched, ok := findNode(path)
	if !ok {
		return false
	}
	printNodeHelp(os.Stdout, node, matched)
	return true
}
