package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// isCharDevice reports whether f is connected to a terminal (a character
// device) rather than a pipe, regular file, or /dev/null. Used to decide
// whether download feedback and prompts are appropriate — we stay silent in
// scripts, CI, and pipelines.
func isCharDevice(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// prettyLabel turns a manifest entry name into a short human label for the
// progress line: "llvm-opt" → "opt (LLVM)", "llvm-libLLVM.dylib" →
// "libLLVM.dylib (LLVM)", "musl-crt1.o" → "crt1.o (musl)". An "archive …"
// label is passed through. Keeps the URL out of the user's face (feedback
// requirement: name the component, not the full URL).
func prettyLabel(name string) string {
	switch {
	case strings.HasPrefix(name, "llvm-"):
		return strings.TrimPrefix(name, "llvm-") + " (LLVM)"
	case strings.HasPrefix(name, "musl-"):
		return strings.TrimPrefix(name, "musl-") + " (musl)"
	default:
		return name
	}
}

// ttyProgress renders a single in-place progress line per downloaded component
// to stderr (carriage-return updated, throttled). It implements
// blobstore.DownloadProgress. Only construct it when stderr is a terminal.
type ttyProgress struct {
	w       io.Writer
	label   string
	total   int64
	done    int64
	lastLen int
	lastAt  time.Time
}

func newTTYProgress(w io.Writer) *ttyProgress { return &ttyProgress{w: w} }

func (p *ttyProgress) Start(label string, total int64) {
	p.label = prettyLabel(label)
	p.total = total
	p.done = 0
	p.lastAt = time.Time{}
	p.render()
}

func (p *ttyProgress) Advance(n int64) {
	p.done += n
	// Throttle redraws to ~10/s so a fast local mirror doesn't flood the terminal.
	if time.Since(p.lastAt) >= 100*time.Millisecond {
		p.render()
		p.lastAt = time.Now()
	}
}

func (p *ttyProgress) Done() {
	p.render()
	fmt.Fprintln(p.w)
}

func (p *ttyProgress) render() {
	var line string
	if p.total > 0 {
		pct := min(int(float64(p.done)/float64(p.total)*100), 100)
		line = fmt.Sprintf("  %-22s %8s / %-8s %3d%%", p.label, mbProgress(p.done), mbProgress(p.total), pct)
	} else {
		line = fmt.Sprintf("  %-22s %8s", p.label, mbProgress(p.done))
	}
	// Pad with spaces to erase any leftover tail from a previously longer line.
	pad := ""
	if len(line) < p.lastLen {
		pad = strings.Repeat(" ", p.lastLen-len(line))
	}
	fmt.Fprintf(p.w, "\r%s%s", line, pad)
	p.lastLen = len(line)
}

// mbProgress formats a byte count with one decimal place (e.g. "8.4 MB"),
// finer-grained than formatSize so a live bar doesn't jump in whole MB steps.
func mbProgress(b int64) string {
	const mb = 1024.0 * 1024.0
	const kb = 1024.0
	switch {
	case b >= int64(mb):
		return fmt.Sprintf("%.1f MB", float64(b)/mb)
	case b >= int64(kb):
		return fmt.Sprintf("%.0f KB", float64(b)/kb)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// confirmToolchainDownload prints a one-time heads-up and asks the user to
// proceed with a first-run component download. Call only when both stderr and
// stdin are terminals. Returns true to proceed; Enter (empty) defaults to yes.
// Set PROMISE_ASSUME_YES=1 to auto-accept without prompting (still on a TTY).
func confirmToolchainDownload(what string, components int, unpackedBytes int64) bool {
	fmt.Fprintf(os.Stderr, "\nPromise needs to download its %s to compile (one-time setup).\n", what)
	fmt.Fprintf(os.Stderr, "  %d component(s), ~%s unpacked — cached under ~/.promise for future runs.\n", components, formatSize(unpackedBytes))
	if v := strings.TrimSpace(os.Getenv("PROMISE_ASSUME_YES")); v != "" && v != "0" {
		fmt.Fprintln(os.Stderr, "  (PROMISE_ASSUME_YES set — proceeding without prompt)")
		return true
	}
	fmt.Fprint(os.Stderr, "Download now? [Y/n] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "", "y", "yes":
		return true
	default:
		return false
	}
}
