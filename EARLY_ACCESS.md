# Early access — thanks for taking a look

You're seeing **Promise** a few days before it's public. Please keep it to
yourself until launch (targeting **Tuesday, June 23, 2026** — the date may still
move; I'll give you a heads-up the moment it's live).

## What this is

A statically-typed, natively-compiled language designed so an AI agent can
generate **correct, maintainable** code: explicit ownership, explicit errors,
zero hidden effects, one obvious way to do things — read one file and know exactly
what it does. The compiler, standard library, and catalog are themselves **written
by AI agents** (have a look through the commit history). Open source,
Apache-2.0 OR MIT.

It's **early and not production-ready** — that's exactly why your eyes on it now
are useful.

## Quickstart (~2 min)

Install the toolchain in one command, then try it — including with your own agent.

**1. Install the toolchain (CLI).**

macOS / Linux:

```sh
curl -sSf https://promise-lang.org/install-early.sh | sh
```

Windows (PowerShell):

```powershell
powershell -ExecutionPolicy Bypass -Command "irm https://promise-lang.org/install-early.ps1 | iex"
```

Installs to `~/.promise/bin` (Windows: `%USERPROFILE%\.promise\bin`). The
installer downloads a small (~20 MB) compiler and then sets up the LLVM toolchain
it builds with — a one-time download (a couple of minutes, with a progress bar),
after which everything is cached under `~/.promise` and your builds are instant.
Prefer to read before you pipe? The script is at
<https://promise-lang.org/install-early.sh>.

The `promise` compiler must be on your PATH — the agent step below can't find it
otherwise, and (with no prior knowledge of the language) has nothing to fall back
on.

**macOS / Linux — you must add it to PATH yourself** (the installer doesn't). This
is two steps, and **both matter** — the common trip-up is doing only the first:

**1. Make it permanent.** Append the line to your shell's startup file so *every
new terminal — and any coding agent, which starts its own fresh shell* — sees it.
This matters specifically for the agent step below: a coding-agent CLI runs its
commands in shells it spawns itself, and it needs `promise` on the PATH in those
shells to do the two things the whole exercise depends on — *learn the language*
(`promise --help`, `promise guide`) and *build and run* what it writes. A
persistent PATH entry is what those spawned shells inherit; a one-off `export` in
your current terminal is **not** enough — a new window won't have it, and the
agent will fail with `promise: command not found`.

```sh
# zsh (the macOS default):
echo 'export PATH="$HOME/.promise/bin:$PATH"' >> ~/.zshrc
# bash:
echo 'export PATH="$HOME/.promise/bin:$PATH"' >> ~/.bashrc
```

**2. Load it into the shell you're in right now** (the startup file only applies to
terminals opened *after* step 1). Either open a brand-new terminal, or run:

```sh
export PATH="$HOME/.promise/bin:$PATH"
```

**Windows — the installer adds it to your PATH automatically.** Just open a new
terminal and confirm with the smoke test below.

Confirm it's found (this both smoke-tests the compiler and proves PATH is set):

```sh
promise exec 'print_line("hi")'      # Windows:  promise exec print_line(\"hi\")
```

This should print `hi` quickly — the installer already set up the toolchain, so
there's no first-compile wait.

If that prints `hi`, you're set. If it says `promise: command not found`, the PATH
step above didn't take (most likely you skipped step 1, or opened the agent in a
new terminal) — fix that before the agent step, or the agent will be stuck.

Then run a file of your own — save this as `hello.pr`:

```promise
main() {
  print_line("hello, promise");
}
```

```sh
promise run hello.pr
```

Explore the language with **`promise guide`** (the full guide ships inside the
toolchain — no repo checkout needed).

**Editing `.pr` files?** The **Promise Lang** extension gives you syntax
highlighting — install it from the
[VS Code Marketplace](https://marketplace.visualstudio.com/items?itemName=promise-lang.promise-language)
or [Open VSX](https://open-vsx.org/extension/promise-lang/promise-language)
(Cursor / Windsurf / VSCodium), or just search "Promise Lang" in your editor's
Extensions panel. Editor support only; the toolchain comes from the install above.

**2. Point your own agent at it (the real test).**

The whole premise is that an agent can pick up Promise and write clean, correct
code. So try it: paste the prompt below to your coding agent, then *read what it
produces* — that's the part that matters. (Note how it opens by telling the agent
*what Promise is* and to learn it from the toolchain — the language isn't in any
model's training data, so without that it'll guess, or assume you mean JavaScript
Promises.)

> **Promise is a brand-new statically-typed, natively-compiled programming
> language.** Its compiler is already installed on this
> machine as the `promise` command, but the language is not in your training data,
> so **first learn it: run `promise --help` and `promise guide`** to pick up the
> syntax, the standard library, and the project workflow. Then build a small
> command-line tool in Promise: it takes file paths as arguments and, processing
> the files **concurrently** (one green thread per file), prints the line count
> for each file plus a grand total; skip unreadable files gracefully instead of
> crashing. Scaffold with `promise init`, build a single self-contained binary
> with `promise build`, and run it. Write idiomatic Promise — explicit types, the
> error operators, ownership annotations — and make it read cleanly: I should be
> able to open the file and know exactly what it does.
>
> **If you hit a compiler bug, capture it — don't quietly work around it.** The
> language is new enough that you may trip over a genuine codegen or compiler
> failure. When you find a reproducible one, minimize it first: bisect to the
> smallest source that still triggers it, and find the closest variant that *does*
> compile, so the trigger is pinned down precisely (not just "my big program
> failed"). Then write it to a `BUG-<slug>.md` file in the project root with a
> one-line title, the `promise version` output, your platform, the minimal repro
> (smallest `.pr` source plus the exact `promise build` command), the verbatim
> error output, the expected behavior, a short table of what does and doesn't
> trigger it, your best guess at the cause, and the workaround you used in the
> real code. Verify the repro actually fails and the "compiles fine" controls
> actually compile before writing them down — don't go from memory. Put `Please
> file this upstream at https://github.com/promise-language/promise/issues` at the
> top of the file, and mention the file in your summary so I remember to submit
> it. Only write it if you actually hit a reproducible failure — don't invent one.

Did it compile? Does the code read the way the prompt implies? That's the thesis,
tested on your own setup.

**Recorded sessions.** The [**Promise Zoo**](https://github.com/promise-language/zoo/tree/early-access)
is a gallery of exactly this — real programs built in Promise by AI agents, each with
the exact prompt, the generated code, an honest account of how the run went, and a
faithful terminal recording you can replay.

## Already on my list (no need to report these)

A couple of things I already know are rough — flagging them so you can look past
them, and so you don't spend feedback here. Neither affects correctness:

- **Compile speed isn't optimized yet.** Compiled programs run at native speed,
  but the *compile step* in `promise exec` / `promise run` takes a few seconds,
  and there's no build caching for those two yet — every invocation recompiles
  from scratch. There's a lot of headroom here; it'll get significantly faster.
- **Memory use isn't optimized yet.** The current focus is correctness, not
  footprint. Both the compiler and the programs it generates use more memory than
  they need to — many optimizations are planned.

## What I'd love your take on

- **Did your agent build a working project — with zero prior knowledge of Promise?** This is the core question. What worked, and where did it get lost or need a nudge (learning the language from `promise --help` / `promise guide`, the build, the error/ownership model in practice)?
- **Does the code read clearly?** Can you open a file the agent wrote and know exactly what it does — would you be comfortable maintaining it?
- **The ownership (`~` / `&` / `*`) and error (`!` / `?^` / `?!` / `? e {}`) model** — natural, or awkward?
- **Anything that made you go "huh?"** — rough edges, confusing docs, install hiccups.

## How to reach me

Open an issue, reply to my message, or email **early@promise-lang.org**. Even a
one-line reaction helps.

## Please keep it under wraps for now

No public links, screenshots, or "look at this" posts until it's live. After
launch, share away — and I'd love it if you did.

Thanks for looking early.

— George
