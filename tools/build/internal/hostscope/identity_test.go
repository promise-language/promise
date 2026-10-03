package hostscope

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeArenaRecord(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(ArenaRecordPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestArenaAt(t *testing.T) {
	const id = "a-6af16c501a04618de507d06ca0ea5bbd"

	t.Run("valid record", func(t *testing.T) {
		root := t.TempDir()
		writeArenaRecord(t, root, `{"id":"`+id+`","label":"promise_x"}`)
		got, err := ArenaAt(root)
		if err != nil {
			t.Fatalf("ArenaAt: %v", err)
		}
		if want := (ArenaIdentity{Host: DeriveHostId(), Id: id}); got != want {
			t.Errorf("ArenaAt = %v, want %v", got, want)
		}
	})

	t.Run("no record is an unknown arena", func(t *testing.T) {
		if _, err := ArenaAt(t.TempDir()); !errors.Is(err, ErrArenaUnknown) {
			t.Errorf("err = %v, want ErrArenaUnknown", err)
		}
	})

	for _, tc := range []struct{ name, content string }{
		{"unparseable record", `{"id":`},
		{"not an arena id", `{"id":"build01"}`},
		{"uppercase hex", `{"id":"a-6AF16C501A04618DE507D06CA0EA5BBD"}`},
		{"empty id", `{}`},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			root := t.TempDir()
			writeArenaRecord(t, root, tc.content)
			_, err := ArenaAt(root)
			if err == nil || errors.Is(err, ErrArenaUnknown) {
				t.Fatalf("err = %v, want a refusal naming the record", err)
			}
			if !strings.Contains(err.Error(), "arena.json") {
				t.Errorf("the refusal must name the record, got: %v", err)
			}
		})
	}
}

func TestNormalizeHostId(t *testing.T) {
	for in, want := range map[string]HostId{
		"Host.Example.COM ": "host",
		"build01":           "build01",
		"  MAC-mini.local":  "mac-mini",
		"":                  "",
	} {
		if got := NormalizeHostId(in); got != want {
			t.Errorf("NormalizeHostId(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArenaIdentity_EmptyIsEitherHalfMissing(t *testing.T) {
	for _, a := range []ArenaIdentity{{}, {Host: "h"}, {Id: "a-00000000000000000000000000000001"}} {
		if !a.Empty() {
			t.Errorf("%v.Empty() = false, want true", a)
		}
	}
	if (ArenaIdentity{Host: "h", Id: "a-00000000000000000000000000000001"}).Empty() {
		t.Error("a full pair reads as empty")
	}
}
