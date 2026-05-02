package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"djabi.dev/go/promise_lang/internal/formatter"
)

func runFmt(args []string) {
	var writeInPlace, check, showDiff bool
	var files []string

	for _, arg := range args {
		switch arg {
		case "-w":
			writeInPlace = true
		case "--check":
			check = true
		case "--diff":
			showDiff = true
		default:
			files = append(files, arg)
		}
	}

	// No files: stdin → stdout
	if len(files) == 0 {
		src, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading stdin: %v\n", err)
			os.Exit(1)
		}
		out := formatter.Format(src)
		os.Stdout.Write(out)
		return
	}

	// Expand directories
	var expanded []string
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if info.IsDir() {
			found := collectPrFiles(f)
			expanded = append(expanded, found...)
		} else {
			expanded = append(expanded, f)
		}
	}

	anyUnformatted := false
	for _, path := range expanded {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading %s: %v\n", path, err)
			os.Exit(1)
		}

		out := formatter.Format(src)

		if bytes.Equal(out, src) {
			continue // already formatted
		}

		anyUnformatted = true

		if check {
			fmt.Println(path)
			continue
		}

		if showDiff {
			printDiff(path, string(src), string(out))
			continue
		}

		if writeInPlace {
			// Preserve original file permissions
			perm := os.FileMode(0644)
			if fi, e := os.Stat(path); e == nil {
				perm = fi.Mode().Perm()
			}
			err := os.WriteFile(path, out, perm)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
				os.Exit(1)
			}
			fmt.Println(path)
		} else {
			os.Stdout.Write(out)
		}
	}

	if check && anyUnformatted {
		os.Exit(1)
	}
}

func collectPrFiles(dir string) []string {
	var files []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".pr") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func printDiff(path, old, new string) {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")

	fmt.Printf("--- %s\n", path)
	fmt.Printf("+++ %s\n", path)

	// Simple line-by-line diff (not unified, but functional)
	maxLen := len(oldLines)
	if len(newLines) > maxLen {
		maxLen = len(newLines)
	}

	for i := 0; i < maxLen; i++ {
		var ol, nl string
		if i < len(oldLines) {
			ol = oldLines[i]
		}
		if i < len(newLines) {
			nl = newLines[i]
		}
		if ol != nl {
			if i < len(oldLines) {
				fmt.Printf("-%s\n", ol)
			}
			if i < len(newLines) {
				fmt.Printf("+%s\n", nl)
			}
		}
	}
	fmt.Println()
}
