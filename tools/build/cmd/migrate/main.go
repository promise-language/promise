// Command migrate moves this project's tracker items onto public GitHub issues.
//
// It is a ONE-TIME tool. It is deleted once the migration lands, along with the
// tracker MCP server entry and the `do` flow wiring it replaces. Nothing in the
// build depends on it and nothing else imports it.
//
// The phases are separate commands because they have different blast radii:
//
//	export   read the tracker, write a local corpus       (publishes nothing)
//	guard    screen the corpus through bin/tool-guard     (publishes nothing)
//	report   summarize what export and guard established  (publishes nothing)
//
// Everything above is reversible. The publishing phases that follow are not,
// which is why they are not in this binary yet: an issue is public the moment
// it is created, and deleting it removes it from the page and not from anyone's
// index or mail.
//
// The tracker's address is never compiled in. It is read from
// PROMISE_TRACKER_URL, or from .flow/context.json, both of which are outside
// git — a private address written into a tracked file is published by the next
// push, which is the exact class of disclosure this migration exists to avoid.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/promise-language/promise/tools/build/common"
)

var sourceHash = "dev"

const usage = `migrate — one-time tracker → GitHub issue migration

Usage:
  migrate export [-out DIR] [-live-only]
  migrate guard  [-out DIR] [-jobs N]
  migrate report [-out DIR]

Commands:
  export   Read every tracker item over the REST API and write a local corpus:
           one JSON file per item, one rendered issue body per item, and an
           index. Publishes nothing and needs no GitHub credentials.

  guard    Feed every live item's rendered body to bin/tool-guard — the guard
           the workspace supplies, not one this tool authors — and record the
           verdict. Publishes nothing. Fails closed: a guard that cannot answer
           is recorded as a refusal, never as a pass.

  report   Print what export and guard established: corpus counts, verdict
           buckets, refusal reasons, and the cross-reference graph.

Flags:
  -out DIR     corpus directory (default .home/migrate)
  -live-only   export only open + needs_answer items (default: everything,
               because the archive index needs the closed corpus too)
  -jobs N      concurrent guard invocations (default 8)
`

func main() {
	common.CheckStale(sourceHash)

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	root, err := common.FindRoot()
	if err != nil {
		fatal(err)
	}

	cmd := os.Args[1]
	args := common.NormalizeArgs(os.Args[2:])

	switch cmd {
	case "export":
		fs := flag.NewFlagSet("export", flag.ExitOnError)
		out := fs.String("out", "", "corpus directory")
		liveOnly := fs.Bool("live-only", false, "export only live items")
		mustParse(fs, args)
		err = runExport(root, corpusDir(root, *out), *liveOnly)
	case "guard":
		fs := flag.NewFlagSet("guard", flag.ExitOnError)
		out := fs.String("out", "", "corpus directory")
		jobs := fs.Int("jobs", 8, "concurrent guard invocations")
		mustParse(fs, args)
		err = runGuard(root, corpusDir(root, *out), *jobs)
	case "report":
		fs := flag.NewFlagSet("report", flag.ExitOnError)
		out := fs.String("out", "", "corpus directory")
		mustParse(fs, args)
		err = runReport(corpusDir(root, *out))
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "migrate: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fatal(err)
	}
}

func mustParse(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		fatal(err)
	}
}

// corpusDir resolves where the corpus lives. The default is under .home/, which
// is gitignored: the corpus holds unreviewed item text and captured worktree
// patches, and neither belongs in git before the triage phase has looked at it.
func corpusDir(root, override string) string {
	if override != "" {
		if filepath.IsAbs(override) {
			return override
		}
		return filepath.Join(root, override)
	}
	return filepath.Join(root, ".home", "migrate")
}

func fatal(err error) {
	var refusal guardRefusal
	if errors.As(err, &refusal) {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
	os.Exit(1)
}
