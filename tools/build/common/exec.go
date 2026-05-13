package common

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Run executes a command with stdout/stderr connected to the terminal.
// Returns an error if the command fails.
func Run(name string, args ...string) error {
	return RunIn("", name, args...)
}

// RunIn executes a command in the given directory.
func RunIn(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// RunOutput executes a command and returns its stdout as a string.
// Stderr is connected to the terminal.
func RunOutput(name string, args ...string) (string, error) {
	return RunOutputIn("", name, args...)
}

// RunOutputIn executes a command in the given directory and returns stdout.
func RunOutputIn(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RunTee executes a command in the given directory, streaming stdout to
// os.Stdout in real-time while also capturing and returning it as a string.
// Stderr remains connected to os.Stderr.
func RunTee(dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(buf.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// RunTeeStderr executes a command in the given directory, streaming stdout to
// os.Stderr in real-time while also capturing and returning it as a string.
// Keeps os.Stdout clean for structured output (e.g. JSON). Stderr remains
// connected to os.Stderr.
func RunTeeStderr(dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = io.MultiWriter(os.Stderr, &buf)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(buf.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// RunSilent executes a command discarding stdout/stderr. Returns error on failure.
func RunSilent(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// RunOutputCombined executes a command capturing both stdout and stderr.
// Use for commands like "java -version" that write to stderr.
func RunOutputCombined(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RunOutputQuiet executes a command capturing stdout and discarding stderr.
// Use for probing commands where stderr noise is expected.
func RunOutputQuiet(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Which finds an executable in PATH, returning its full path or empty string.
func Which(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

// Exists returns true if the given path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
