# CLI Tools

> **Tag:** `cli-guide` — remaining work to complete this document: the query named in
> `docs/index.md`.

> **Home:** [promise-language/org](https://github.com/promise-language/org) — this document is
> distributed into each managed project as `docs/org/`. A copy is never edited in place: to
> change it, file an issue against `org`.

How every command-line tool in the organization behaves at its invocation surface: how it takes
parameters, how it reports, and how it refuses. A rule stated as a blockquote is an invariant, and
the prose under it is why.

## 1. Scope

This document governs the invocation surface of every command-line tool an organization project
ships — the `bin/` tools and any binary a project's contributors or agents run by hand. It does not
own:

- **Which tools a project must have** — the tool contract owns that.
- **The gate envelope and the `--envelope` protocol** — the gate contract owns that.
- **How tools are built, provisioned, and kept fresh** — the tooling document owns that.

## 2. Every input is an explicit argument

> **A tool reads no environment variable to decide what it does.** Every parameter arrives as an
> explicit argument on the command line.

An environment variable is an argument with no audit trail: it is invisible in the invocation, it
leaks across process boundaries the caller never considered, and two invocations that look
identical behave differently. A reader of a command line must be able to know everything the tool
was told.

Two narrow uses are sanctioned, and both are outside the tool's outcome:

- **Debugging the tool itself.** A variable that turns on the tool's own diagnostics may exist. It
  must never change what the tool does — only what it reports about its own execution.
- **Guard-enforced containment markers.** A variable a parent sets so that guards deny classes of
  action in the entire subprocess tree is containment, not argument transport: it is read by the
  guard, not by the tool, and it can only narrow what is possible, never select behaviour.

## 3. Flag form

> **A flag is a full-English-word name, dash-separated when multiword, prefixed with `-` or `--`.**
> The two prefixes are the same flag: tools normalize the prefix once, then match the name exactly.

`--my-long-flag` and `-my-long-flag` are one flag. `--myLongFlag`, `--my_long_flag`, and
abbreviations are not flags at all — they are unknown input (§8).

> **A name — a flag's or a subcommand's — is lowercase ASCII letters `a`–`z` and digits `0`–`9`,
> with `-` as the only separator.** Nothing else: no uppercase, no underscores, no dots, no
> characters outside ASCII.

Case is the cheapest way to mint an accidental alias — `--force` and `--Force` are one name to a
person and two to a matcher — and anything beyond lowercase ASCII is a name that types
differently across keyboards, shells, and platforms. A closed alphabet keeps every name exactly
as greppable, quotable, and portable as the one canonical spelling requires.

> **One name per flag. No aliases, no fallbacks.** There is exactly one canonical way to pass each
> parameter, and that way is the one help text, error messages, and documentation use.

A second name for the same thing is a second thing to search for, a second thing to deny in a
guard, and a fork in every transcript. The list of sanctioned exceptions to the full-English-word
rule lives in this section, and it is empty.

> **A value flag declares a type.** String, integer, boolean is not a value type, duration, path,
> enumeration — the type is part of the flag's definition, printed by `-help`, and a value that
> does not satisfy it is a usage error naming the flag, the value, and the expected type.

A value is given as the next argument (`-timeout 30s`) or attached with `=` (`-timeout=30s`); both
normalize to the same parameter.

> **A boolean flag is a pair: `-my-flag` asserts it, `-no-my-flag` denies it.** Neither takes a
> value; `-my-flag=true` and `-my-flag=false` are rejected with a pointer to the pair.

The pair makes the negative visible and greppable. `=false` hides a decision inside a value where
a reader scanning for the flag's name will misread it.

## 4. One order

> **The command path comes first, complete and uninterrupted; every flag follows it; every
> positional argument follows the flags.**

```
tool <subcommand…> <flags…> <positional args…>
```

`tool sync -json origin` — never `tool -json sync`, and never `tool sync origin -json`. There is
no "global flag position": a flag that every command supports, `-json` included, is still written
after the command path, because it modifies the command being run, and until the path is complete
there is no such command. Tools where a flag works in one position and silently not in another
are the standing failure this rule exists to delete — two positions is two spellings of the same
invocation, and the second one is an alias.

`-help` and `-version` obey the same rule: `tool -help` is the root command's help — the full
surface, per §7 — and `tool sync -help` is `sync`'s.

> **A flag appearing after the first positional argument is an error, never a positional.** A
> parser that stops at the first non-flag and hands the rest through untouched has silently
> reinterpreted the invocation; refusing it is §8's fail-closed rule applied to position.

The operator who typed `tool sync origin -json` wanted JSON output; a tool that instead passes
`-json` to the backend as a name has done something no one asked, without a word.

> **`--` ends the flags.** Everything after it is positional, verbatim — and a positional that
> begins with `-` is accepted only after it.

Without the marker, a value like a file named `-report` is indistinguishable from a flag, and
guessing is worse than either answer. With it, the boundary between flags and arguments is
explicit exactly where it would otherwise be ambiguous.

## 5. General switches do not exist

> **No flag answers questions the tool has not asked yet.** Blanket switches — `-yes`, `-force`,
> "assume yes to everything" — are not permitted.

Every override is named for the one thing it overrides, so consenting to one risk never consents
to another. A tool that would need `-yes` is a tool that asks questions interactively; it should
instead refuse with a typed, named condition and the specific flag that overrides it.

## 6. Output: two modes, one rule

> **Output is human-readable when stdout is a terminal and JSON when it is not.** Every tool
> supports `-json` and `-human` to force the mode regardless of piping. Passing both is a usage
> error.

The mode is decided by stdout only — never stderr, never an environment variable.

> **Stdout carries the result and nothing else. Progress and narration go to stderr.**

That is what makes `tool > out.json` and `tool -json 2>/dev/null` both behave. JSON on stdout is a
stable interface: fields are added, never renamed or repurposed, and absent means unknown rather
than zero.

## 7. `-help` and `-version`

> **Every tool supports `-help`**: it prints the subcommands and, per subcommand, every flag with
> its type and a one-line description — or simply every flag, when the tool has no subcommands.
> It exits 0, and it is the only place the full flag list appears.

> **Every tool supports `-version`**: one line on stdout, either a comprehensible release version
> or the commit hash the binary was built from. It exits 0.

A binary that cannot say what it is cannot be the subject of a bug report.

## 8. Unknown input fails closed

> **An unknown flag is an error that names it** — the bad flag, the closest existing flag when one
> is close, and how to get the supported list (`-help`). Nothing is silently ignored.

The error does not dump the flag list: a wall of definitions buries the one fact the operator
needs. The pointer to `-help` is the list, one step away.

> **Invocation errors are rejected before any action.** The tool exits 2 having claimed nothing,
> written nothing, and contacted nothing.

The same rule covers unknown subcommands, missing required parameters, values failing their type,
misplaced flags (§4), and contradictory parameters. Validation is exhaustive: every problem with
the invocation is reported, not just the first.

## 9. `-json-input`: the whole invocation, from a file

> **`-json-input /path/to/args.json` supplies parameters from a JSON file whose schema maps
> exactly to the tool's flags**, plus `"args"`, an array carrying the non-flag arguments. An
> unknown key in the file is an unknown flag (§8).

The file is a transport for the same closed parameter set, not a second configuration system: no
key exists in the file that does not exist as a flag. The name is deliberately not `-file` or
`-input` — bare words a tool's own domain will want for its actual inputs; `-json-input` names
the mechanism, so it collides with nothing a tool processes.

> **A parameter set both in the file and on the command line is a usage error.** There is no
> precedence between the two, because precedence is a fallback (§3).

## 10. Subcommands

> **The command set is closed in both directions**: no command is added outside the tool's
> definition, and no command answers to a name not in the set. Each command has exactly one name.

> **Addressing is exact.** An identifier the user types resolves to exactly what it names, never to
> something that merely contains or resembles it.

## 11. Exit codes

> **`0` — did what was asked**, including when there was nothing to do. An empty result is not an
> error. **`1` — could not complete**, or stopped on a condition a human must clear. **`2` — the
> invocation itself was malformed**, and nothing was done.
