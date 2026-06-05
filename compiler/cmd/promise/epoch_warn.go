package main

import (
	"fmt"
	"os"

	"github.com/promise-language/promise/compiler/internal/module"
)

// warnEpochCommands are the project-operating subcommands for which a mismatch
// between this compiler's epoch and the project's pinned epoch is worth warning
// about. Toolchain-management verbs (install, sync, use, epochs, remove, update,
// pkg, …) deliberately run on whatever binary was invoked and are excluded.
var warnEpochCommands = map[string]bool{
	"build":   true,
	"run":     true,
	"test":    true,
	"check":   true,
	"emit-ir": true,
	"exec":    true,
	"doc":     true,
}

// warnEpochMismatch prints a warning (never exits, never re-execs) when this
// compiler is invoked directly on a project whose promise.toml pins a different
// epoch. With the shim-in-binary launcher retired (T0770) the compiler is no
// longer a trampoline — "you get what you run" — so a directly-invoked epoch
// binary that does not match the project's pin surfaces as a warning instead of
// a silent hand-off.
//
// The warning is suppressed when:
//   - PROMISE_NO_EPOCH_WARN is set (explicit opt-out), or
//   - PROMISE_EPOCH is set — the epoch was chosen explicitly (by the user, or a
//     wrapper that forces an epoch), so a project-pin mismatch is intentional
//     rather than an accident worth flagging. (The on-PATH stub does not set
//     PROMISE_EPOCH; it resolves the epoch and exec-replaces into the matching
//     compiler, which therefore never trips this mismatch in the first place.)
func warnEpochMismatch() {
	if os.Getenv("PROMISE_NO_EPOCH_WARN") != "" {
		return
	}
	if os.Getenv("PROMISE_EPOCH") != "" {
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	cfg, err := module.FindConfig(cwd)
	if err != nil || cfg == nil || cfg.Epoch == "" {
		return
	}

	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || myEpoch == "" {
		return
	}

	if cfg.Epoch == myEpoch {
		return
	}

	fmt.Fprintf(os.Stderr,
		"warning: this compiler is epoch %s, but promise.toml pins epoch %s. "+
			"Run via the 'promise' launcher, or 'promise use %s' to switch.\n",
		myEpoch, cfg.Epoch, cfg.Epoch)
}
