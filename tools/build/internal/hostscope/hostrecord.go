package hostscope

// The host home and the reader of its identity record, copied from forge's
// primitives/logging on the terms the package comment states. HostIdAt reads
// the host half of a runner's arena through this, so a reader that disagreed
// with forge's about where the record is or which records are machines would
// name a different arena than the runner it is nested in.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// stateDir is the organization's directory inside the account's state
	// location: `.local/state/promise-language/` on every platform but
	// Windows, and `promise-language\state\` in the local application-data
	// folder there. It is the host's name, which its settings sit under in the
	// configuration location too (org/cli-guide#configuration).
	stateDir = "promise-language"
	// hostRecordFile is the host home's identity record.
	hostRecordFile = "identity.json"
)

// HostHome is the machine's host home, and whether the platform reports one.
//
// No variable relocates it, XDG_STATE_HOME included: it is the same place for
// every program on the machine, which is what lets a program that writes there
// and a program that reads there find each other without being told. The home
// directory and the local application-data folder are the ones the platform's
// own call reports, and an account the platform reports no home for has no host
// home — a program running under it treats every record as missing.
func HostHome() (string, bool) {
	if runtime.GOOS == "windows" {
		// Go reports the local application-data folder through UserCacheDir on
		// Windows; UserConfigDir is the roaming one, which is not this.
		local, err := os.UserCacheDir()
		if err != nil {
			return "", false
		}
		return filepath.Join(local, stateDir, "state"), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".local", "state", stateDir), true
}

// The kind letter an id carries, so an id read out of context says what it
// names (identity.md, Ids).
const (
	kindHost  = 'h'
	kindGuest = 'g'
)

// The two kinds a host home's record may declare.
const (
	hostRecord  = "host"
	guestRecord = "guest"
)

// ref is a thing's id and the label beside it for display. The label is never
// compared: "Every record that names something carries the id, and the label
// only beside it for display."
type ref struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// record is the host home's identity record. In a guest it names the guest, and
// the host the guest runs on.
type record struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Host  *ref   `json:"host,omitempty"`
}

// readHostRecord reads the host home's identity.json, and reports whether the
// machine has an identity to report.
//
// A file that is missing, unreadable, unparseable, or whose id is not one of
// this kind is unknown — never repaired, never replaced. An id of the wrong
// kind is "a malformed value that is refused, not a lookup that quietly finds
// nothing", and a caller told the machine is unknown writes no host field at
// all, which is what "absent means unknown" means on a line.
func readHostRecord(hostHome string) (record, bool) {
	var rec record
	if !readJSON(filepath.Join(hostHome, hostRecordFile), &rec) {
		return record{}, false
	}
	switch rec.Kind {
	case hostRecord:
		if !validID(rec.ID, kindHost) {
			return record{}, false
		}
	case guestRecord:
		// A guest's record names the host it runs on. Without that the guest
		// cannot be placed, so the record says nothing this reader can use.
		if !validID(rec.ID, kindGuest) || rec.Host == nil || !validID(rec.Host.ID, kindHost) {
			return record{}, false
		}
	default:
		return record{}, false
	}
	return rec, true
}

// validID reports whether an id is well formed and names the kind expected of
// it. Nothing is derived from an id and nothing is parsed back out of one, so
// this is the whole of what can be asked of one.
func validID(id string, kind byte) bool {
	if len(id) != 34 || id[0] != kind || id[1] != '-' {
		return false
	}
	for i := 2; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readJSON reads one record. Every way of failing is the same fact to a reader
// — there is no identity here to report — so none of them is distinguished, and
// none of them writes anything.
func readJSON(path string, into any) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(body, into) == nil
}
