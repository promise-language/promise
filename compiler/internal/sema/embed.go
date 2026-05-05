package sema

import (
	"fmt"
	"os"
	"path/filepath"
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

	return errs
}

func fmtEmbedError(fd *ast.FuncDecl, format string, args ...any) error {
	pos := fd.Pos()
	msg := fmt.Sprintf(format, args...)
	return fmt.Errorf("%s:%d:%d: %s", pos.File, pos.Line, pos.Column, msg)
}
