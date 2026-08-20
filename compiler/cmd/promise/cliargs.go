package main

import (
	"fmt"
	"strings"
)

// flagSpec describes the flags a command accepts. `value` flags consume the
// following token as their value; `flag` (boolean) flags are set true when
// present. Keys are the flag name without the leading dash (args reaching the
// parser are already normalized, so `--release` and `-release` both arrive as
// `-release` → key "release").
type flagSpec struct {
	value map[string]*string
	flag  map[string]*bool
}

// cliResult is the outcome of a strict parse: the positional tokens seen before
// any bare `--`, plus the program argv tail after the first bare `--` (T1426;
// nil when the command doesn't forward program args or no `--` was present).
type cliResult struct {
	positionals []string
	progArgs    []string
}

// parseCLIArgs is the single strict argument parser shared by build, run,
// emit-ir, and exec (T1604). It replaces four independent loops whose
// `default:` branches silently treated any unrecognized token — including a
// mistyped flag like `-relase` — as the source positional, so a typo in
// `--release` quietly produced a debug build.
//
// Args must already be normalized (`--flag` → `-flag`, `-flag=v` → `-flag v`).
// Behavior:
//   - A bare `--`: if supportsProgArgs, stop and return the tail as progArgs;
//     otherwise it is an error (the command never launches the program).
//   - A `-…` token (not a lone `-`) is looked up in the spec; if absent it is an
//     unknown-flag error pointing at `--` when the command forwards program args.
//   - A non-flag token is a positional; a second positional is an error unless
//     allowManyPositionals (only exec, whose positionals join into source text).
func parseCLIArgs(cmd string, args []string, spec flagSpec, supportsProgArgs, allowManyPositionals bool) (cliResult, error) {
	var res cliResult
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if !supportsProgArgs {
				return res, fmt.Errorf("error: unexpected `--`\n"+
					"hint: promise %s does not forward program arguments", cmd)
			}
			// Everything after the first bare `--` is the program's argv (T1426).
			res.progArgs = args[i+1:]
			return res, nil
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			name := arg[1:]
			if dst, ok := spec.value[name]; ok {
				if i+1 >= len(args) {
					return res, fmt.Errorf("error: flag %s requires a value", arg)
				}
				*dst = args[i+1]
				i++
				continue
			}
			if dst, ok := spec.flag[name]; ok {
				*dst = true
				continue
			}
			return res, unknownFlagError(cmd, arg, supportsProgArgs)
		}
		// Positional token.
		if len(res.positionals) >= 1 && !allowManyPositionals {
			return res, fmt.Errorf("error: unexpected extra argument %q\n"+
				"hint: promise %s takes a single source file or project directory", arg, cmd)
		}
		res.positionals = append(res.positionals, arg)
	}
	return res, nil
}

// unknownFlagError builds the unknown-flag diagnostic. For commands that forward
// program arguments (run), it points the user at `--` as the escape hatch;
// otherwise it just names the flag.
func unknownFlagError(cmd, flag string, supportsProgArgs bool) error {
	if supportsProgArgs {
		return fmt.Errorf("error: unknown flag %s\n"+
			"hint: to pass arguments to the program, put them after --: promise %s <file.pr> -- %s",
			flag, cmd, flag)
	}
	return fmt.Errorf("error: unknown flag %s", flag)
}
