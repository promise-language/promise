package blobstore

import (
	"fmt"
	"os"
	"runtime"
)

// integrityTelemetryEnv is the explicit, disclosed opt-in for integrity-mismatch
// telemetry. Unset (the default) means nothing is sent — no hidden effects.
const integrityTelemetryEnv = "PROMISE_INTEGRITY_TELEMETRY"

// reportIntegrityMismatch is a DESIGN CANDIDATE — see distribution.md §4.4 and
// release-automation.md §7. The integrity-only signal (dependency, source,
// expected/actual hash, epoch, platform) would let a broken release be detected
// centrally within minutes. It is gated behind an explicit opt-in env var and,
// when unset (default), does nothing. Disclosure/opt-in UX and the reporting
// endpoint are TBD before shipping; this stub deliberately sends nothing.
func reportIntegrityMismatch(name, sourceURL, expected, got, epoch string) {
	if os.Getenv(integrityTelemetryEnv) != "1" {
		return // default: no hidden effects
	}
	// DESIGN CANDIDATE: when an endpoint and disclosure UX are decided, emit the
	// integrity-only signal here. Until then, opting in only logs locally.
	fmt.Fprintf(os.Stderr,
		"[integrity-telemetry candidate] dep=%s source=%s expected=%s actual=%s epoch=%s platform=%s-%s\n",
		name, sourceURL, expected, got, epoch, runtime.GOOS, runtime.GOARCH)
}
