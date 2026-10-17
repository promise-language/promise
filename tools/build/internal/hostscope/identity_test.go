package hostscope

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// writeHostRecord writes the host home's identity record, as provisioning
// does, into a host home the test owns.
func writeHostRecord(t *testing.T, hostHome, content string) {
	t.Helper()
	if err := os.MkdirAll(hostHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostHome, hostRecordFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestArenaAt(t *testing.T) {
	const (
		id    = "a-6af16c501a04618de507d06ca0ea5bbd"
		host  = "h-b2bac7ba8491e4a608c478bbf2bacf55"
		guest = "g-0123456789abcdef0123456789abcdef"
	)
	hostRec := `{"kind":"host","id":"` + host + `","label":"livingroom_ubuntu"}`

	for _, tc := range []struct {
		name, hostRec string
		want          HostId
	}{
		{"host record", hostRec, host},
		// In a guest the machine is the guest: its own id, never its host's.
		{"guest record", `{"kind":"guest","id":"` + guest + `","host":{"id":"` + host + `"}}`, guest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostHome, root := t.TempDir(), t.TempDir()
			writeHostRecord(t, hostHome, tc.hostRec)
			writeArenaRecord(t, root, `{"id":"`+id+`","label":"promise_x"}`)
			got, err := ArenaAt(hostHome, root)
			if err != nil {
				t.Fatalf("ArenaAt: %v", err)
			}
			if want := (ArenaIdentity{Host: tc.want, Id: id}); got != want {
				t.Errorf("ArenaAt = %v, want %v", got, want)
			}
		})
	}

	t.Run("no arena record is an unknown arena", func(t *testing.T) {
		hostHome := t.TempDir()
		writeHostRecord(t, hostHome, hostRec)
		got, err := ArenaAt(hostHome, t.TempDir())
		if !errors.Is(err, ErrArenaUnknown) {
			t.Errorf("err = %v, want ErrArenaUnknown", err)
		}
		if got != (ArenaIdentity{}) {
			t.Errorf("ArenaAt = %v, want the empty pair", got)
		}
	})

	t.Run("no host record is an unknown host", func(t *testing.T) {
		hostHome, root := t.TempDir(), t.TempDir()
		writeArenaRecord(t, root, `{"id":"`+id+`"}`)
		got, err := ArenaAt(hostHome, root)
		if !errors.Is(err, ErrHostUnknown) {
			t.Fatalf("err = %v, want ErrHostUnknown", err)
		}
		if !strings.Contains(err.Error(), filepath.Join(hostHome, "identity.json")) {
			t.Errorf("the error must name where it looked, got: %v", err)
		}
		if got != (ArenaIdentity{}) {
			t.Errorf("ArenaAt = %v, want the empty pair", got)
		}
	})

	t.Run("no host home is an unknown host", func(t *testing.T) {
		root := t.TempDir()
		writeArenaRecord(t, root, `{"id":"`+id+`"}`)
		if got, err := ArenaAt("", root); !errors.Is(err, ErrHostUnknown) || got != (ArenaIdentity{}) {
			t.Errorf("ArenaAt(\"\", root) = %v, %v; want the empty pair and ErrHostUnknown", got, err)
		}
	})

	for _, tc := range []struct{ name, content string }{
		{"unparseable record", `{"id":`},
		{"not an arena id", `{"id":"build01"}`},
		{"a host id", `{"id":"` + host + `"}`},
		{"uppercase hex", `{"id":"a-6AF16C501A04618DE507D06CA0EA5BBD"}`},
		{"empty id", `{}`},
	} {
		t.Run("arena: "+tc.name+" is refused", func(t *testing.T) {
			hostHome, root := t.TempDir(), t.TempDir()
			writeHostRecord(t, hostHome, hostRec)
			writeArenaRecord(t, root, tc.content)
			got, err := ArenaAt(hostHome, root)
			if err == nil || errors.Is(err, ErrArenaUnknown) {
				t.Fatalf("err = %v, want a refusal naming the record", err)
			}
			if !strings.Contains(err.Error(), "arena.json") {
				t.Errorf("the refusal must name the record, got: %v", err)
			}
			if got != (ArenaIdentity{}) {
				t.Errorf("ArenaAt = %v, want the empty pair", got)
			}
		})
	}

	for _, tc := range []struct{ name, content string }{
		{"unparseable record", `{"kind":`},
		// The derivation this replaced: a host name is not a host id.
		{"a host name", `{"kind":"host","id":"livingroom-ubuntu"}`},
		{"a guest id in a host record", `{"kind":"host","id":"` + guest + `"}`},
		{"an arena id in a host record", `{"kind":"host","id":"` + id + `"}`},
		{"uppercase hex", `{"kind":"host","id":"h-B2BAC7BA8491E4A608C478BBF2BACF55"}`},
		{"an arena kind", `{"kind":"arena","id":"` + id + `"}`},
		{"no kind", `{"id":"` + host + `"}`},
		{"a guest with no host", `{"kind":"guest","id":"` + guest + `"}`},
		{"a guest whose host is not a host id", `{"kind":"guest","id":"` + guest + `","host":{"id":"` + guest + `"}}`},
	} {
		t.Run("host: "+tc.name+" is refused", func(t *testing.T) {
			hostHome, root := t.TempDir(), t.TempDir()
			writeHostRecord(t, hostHome, tc.content)
			writeArenaRecord(t, root, `{"id":"`+id+`"}`)
			got, err := ArenaAt(hostHome, root)
			if err == nil || errors.Is(err, ErrHostUnknown) {
				t.Fatalf("err = %v, want a refusal naming the record — a malformed record is not a missing one", err)
			}
			if !strings.Contains(err.Error(), "identity.json") {
				t.Errorf("the refusal must name the record, got: %v", err)
			}
			if got != (ArenaIdentity{}) {
				t.Errorf("ArenaAt = %v, want the empty pair", got)
			}
		})
	}
}

// The host home is forge's, and no variable moves it: a reader that honoured
// XDG_STATE_HOME would read a different record than the runner it is nested in
// whenever the variable is set, and name a different arena.
func TestHostHome_IsForgesHostHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	got, ok := HostHome()
	if !ok {
		t.Fatal("HostHome() reports no host home with HOME set")
	}
	want := filepath.Join(home, ".local", "state", "promise-language")
	if runtime.GOOS == "windows" {
		want = filepath.Join(home, "AppData", "Local", "promise-language", "state")
	}
	if got != want {
		t.Errorf("HostHome() = %q, want %q", got, want)
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
