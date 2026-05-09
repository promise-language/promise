package sema

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"djabi.dev/go/promise_lang/internal/ast"
)

// ResolveEmbeds resolves embed paths relative to sourceDir, reads the files,
// and populates the Data field of each EmbedInfo. Returns errors for any
// files that cannot be read or fail validation.
func ResolveEmbeds(info *Info, sourceDir string) []error {
	if len(info.Embeds) == 0 {
		return nil
	}

	var errs []error
	for fd, embed := range info.Embeds {
		switch embed.Kind {
		case EmbedDir:
			if err := resolveEmbedDir(embed, sourceDir); err != nil {
				errs = append(errs, fmtEmbedError(fd, "%v", err))
			}
		default:
			absPath := filepath.Join(sourceDir, embed.Path)

			data, err := os.ReadFile(absPath)
			if err != nil {
				errs = append(errs, fmtEmbedError(fd, "cannot read embedded file %q: %v", embed.Path, err))
				continue
			}

			// For string embeds, validate UTF-8
			if embed.Kind == EmbedString && !utf8.Valid(data) {
				errs = append(errs, fmtEmbedError(fd, "embedded file %q is not valid UTF-8 (use u8[] for binary data)", embed.Path))
				continue
			}

			embed.Data = data
		}
	}

	return errs
}

// resolveEmbedDir walks a directory tree and populates EmbedInfo with
// concatenated file data and per-entry metadata (T0031).
func resolveEmbedDir(embed *EmbedInfo, sourceDir string) error {
	// Strip "..." suffix to get the directory path
	dirPath := strings.TrimSuffix(embed.Path, "...")
	dirPath = strings.TrimRight(dirPath, "/")
	if dirPath == "" {
		dirPath = "."
	}

	absDir := filepath.Join(sourceDir, dirPath)

	// Validate it's a directory
	fi, err := os.Stat(absDir)
	if err != nil {
		return fmt.Errorf("cannot access embedded directory %q: %v", dirPath, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("embed path %q is not a directory", dirPath)
	}

	// Walk directory tree, collecting entries
	type fileEntry struct {
		relPath string // relative to the embedded root
		name    string // base name
		size    int64
		isDir   bool
		data    []byte // nil for directories
	}

	var entries []fileEntry
	err = filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip the root directory itself
		if path == absDir {
			return nil
		}

		// Skip hidden files/directories (starting with '.')
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(absDir, path)
		if err != nil {
			return err
		}
		// Normalize to forward slashes for cross-platform consistency
		relPath = filepath.ToSlash(relPath)

		if d.IsDir() {
			entries = append(entries, fileEntry{
				relPath: relPath,
				name:    name,
				isDir:   true,
			})
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("cannot read %q: %v", relPath, err)
		}

		entries = append(entries, fileEntry{
			relPath: relPath,
			name:    name,
			size:    info.Size(),
			data:    data,
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("error walking embedded directory %q: %v", dirPath, err)
	}

	// Sort entries by path for deterministic output
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	// Concatenate file data and build DirEntries
	var blob []byte
	dirEntries := make([]EmbedDirEntry, len(entries))
	for i, e := range entries {
		offset := int64(len(blob))
		if e.data != nil {
			blob = append(blob, e.data...)
		}
		dirEntries[i] = EmbedDirEntry{
			Path:   e.relPath,
			Name:   e.name,
			Size:   e.size,
			IsDir:  e.isDir,
			Offset: offset,
		}
	}

	embed.Data = blob
	embed.DirEntries = dirEntries
	return nil
}

func fmtEmbedError(fd *ast.FuncDecl, format string, args ...any) error {
	pos := fd.Pos()
	msg := fmt.Sprintf(format, args...)
	return fmt.Errorf("%s:%d:%d: %s", pos.File, pos.Line, pos.Column, msg)
}
