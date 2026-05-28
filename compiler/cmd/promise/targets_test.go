package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
)

func TestTargetsTextOutput(t *testing.T) {
	var buf bytes.Buffer
	writeTargets(&buf, supportedTargets(), false)
	out := buf.String()

	for _, want := range []string{
		"Supported compile targets",
		"wasm32-wasi",
		"wasm32-web",
		"(native)",
		"Use:  promise build -target",
		codegen.HostTargetTriple(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("targets output missing %q\noutput:\n%s", want, out)
		}
	}

	// Exactly one (native) marker — the host row.
	if got := strings.Count(out, "(native)"); got != 1 {
		t.Errorf("expected exactly one (native) marker, got %d", got)
	}
}

func TestTargetsJSON(t *testing.T) {
	var buf bytes.Buffer
	writeTargets(&buf, supportedTargets(), true)

	var got struct {
		Host    string       `json:"host"`
		Targets []targetSpec `json:"targets"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\noutput:\n%s", err, buf.String())
	}

	if got.Host != codegen.HostTargetTriple() {
		t.Errorf("host = %q, want %q", got.Host, codegen.HostTargetTriple())
	}
	if len(got.Targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(got.Targets))
	}

	natives := 0
	triples := map[string]bool{}
	for _, s := range got.Targets {
		triples[s.Triple] = true
		if s.Native {
			natives++
			if s.Triple != got.Host {
				t.Errorf("native target triple %q != host %q", s.Triple, got.Host)
			}
		}
	}
	if natives != 1 {
		t.Errorf("expected exactly one native target, got %d", natives)
	}
	if !triples["wasm32-wasi"] {
		t.Error("missing wasm32-wasi target")
	}
	if !triples["wasm32-web"] {
		t.Error("missing wasm32-web target")
	}
}

func TestSupportedTargetsRegistry(t *testing.T) {
	specs := supportedTargets()
	if len(specs) != 3 {
		t.Fatalf("got %d specs, want 3", len(specs))
	}
	if !specs[0].Native {
		t.Error("first spec should be the native host target")
	}
	if specs[0].Triple != codegen.HostTargetTriple() {
		t.Errorf("native triple = %q, want %q", specs[0].Triple, codegen.HostTargetTriple())
	}
	if specs[1].Triple != "wasm32-wasi" || specs[1].Native {
		t.Errorf("second spec = %+v, want wasm32-wasi non-native", specs[1])
	}
	if specs[2].Triple != "wasm32-web" || specs[2].Native {
		t.Errorf("third spec = %+v, want wasm32-web non-native", specs[2])
	}
}

func TestHostShortName(t *testing.T) {
	cases := []struct {
		triple string
		want   string
	}{
		{"x86_64-unknown-linux-musl", "linux-x86_64"},
		{"x86_64-unknown-linux-gnu", "linux-x86_64"},
		{"aarch64-unknown-linux-musl", "linux-arm64"},
		{"aarch64-unknown-linux-gnu", "linux-arm64"},
		{"x86_64-apple-macosx10.15.0", "darwin-x86_64"},
		{"arm64-apple-macosx14.0.0", "darwin-arm64"},
		{"aarch64-apple-macosx14.0.0", "darwin-arm64"},
		{"x86_64-pc-windows-msvc", "windows-x86_64"},
		{"aarch64-pc-windows-msvc", "windows-arm64"},
		// Known OS but unknown arch → bare OS name.
		{"riscv64-unknown-linux-gnu", "linux"},
		{"riscv64-apple-macosx14.0.0", "darwin"},
		{"riscv64-pc-windows-msvc", "windows"},
		// Pass-through for unknown triples.
		{"wasm32-wasi", "wasm32-wasi"},
		{"some-random-triple", "some-random-triple"},
	}
	for _, c := range cases {
		if got := hostShortName(c.triple); got != c.want {
			t.Errorf("hostShortName(%q) = %q, want %q", c.triple, got, c.want)
		}
	}
}

func TestRunTargetsDefault(t *testing.T) {
	out := captureStdout(t, func() { runTargets(nil) })
	if !strings.Contains(out, "Supported compile targets") {
		t.Errorf("default runTargets missing header: %s", out)
	}
	if !strings.Contains(out, "(native)") {
		t.Errorf("default runTargets missing (native) marker: %s", out)
	}
}

func TestRunTargetsJSON(t *testing.T) {
	out := captureStdout(t, func() { runTargets([]string{"-json"}) })
	var got struct {
		Host    string       `json:"host"`
		Targets []targetSpec `json:"targets"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("runTargets -json produced invalid JSON: %v\noutput:\n%s", err, out)
	}
	if got.Host != codegen.HostTargetTriple() {
		t.Errorf("host = %q, want %q", got.Host, codegen.HostTargetTriple())
	}
	if len(got.Targets) != 3 {
		t.Errorf("got %d targets, want 3", len(got.Targets))
	}
}

func TestRunTargetsHelp(t *testing.T) {
	for _, flag := range []string{"-h", "-help"} {
		out := captureStdout(t, func() { runTargets([]string{flag}) })
		if !strings.Contains(out, "usage: promise targets") {
			t.Errorf("runTargets %s missing usage line: %s", flag, out)
		}
	}
}
