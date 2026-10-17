package hostscope

// The arena identity the exclusion is held by and re-entered on, copied from
// flow's identity.go on the terms the package comment states. A runner names
// its holder flow.ArenaAt(hostHome, checkout); a nested verify must compute the
// identical pair from the same two records, or it is a peer queueing behind its
// own parent.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HostId is the machine an arena lives on: the id the host home's identity
// record carries, as provisioning drew it (org identity.md § Where records are
// kept). It is READ and NEVER DERIVED — see HostIdAt.
//
// On a host it is `h-` and 32 lowercase hexadecimal digits. IN A GUEST IT IS
// THE GUEST'S OWN `g-` ID, never its parent host's: the machine an arena lives
// on is the one whose processes are visible from it, and from inside a guest
// that is the guest. A holder's process is confirmed alive there and nowhere
// else, and the host-scope exclusion's lock is in the guest's own filesystem,
// so two guests under one HostId would be one name for two machines neither of
// which can see the other.
//
// Random, so it is unique without anyone assigning it. The derivation it
// replaces — the host name's first dotted segment — collided across domains
// (build01.us-east and build01.eu-west were one host) and is one the corpus
// refuses by name.
type HostId string

// ArenaId is an arena's id as provisioning drew it: `a-` and 32 lowercase
// hexadecimal digits, 128 random bits (org identity.md § Ids). It is READ from
// the checkout's .workspace/arena.json and NEVER DERIVED — see ArenaAt.
//
// Random, so it is unique across the fleet by itself and discloses nothing: a
// fresh clone is a new arena even in the same directory, and a moved checkout
// keeps its arena. The derivation it replaces — the absolute worktree path — is
// the one the corpus refuses by name.
type ArenaId string

// arenaIdHexLen is the number of hexadecimal digits after the `a-` kind letter.
const arenaIdHexLen = 32

// Valid reports whether the id has the corpus's arena form: `a-` and exactly
// 32 lowercase hexadecimal digits. Anything else is a malformed record, never an
// arena — the kind letter is there so a host id in an arena field is refused
// rather than quietly compared.
func (a ArenaId) Valid() bool {
	hex, ok := strings.CutPrefix(string(a), "a-")
	if !ok || len(hex) != arenaIdHexLen {
		return false
	}
	for _, c := range hex {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// ArenaIdentity is the arena a lease binds to: the host it is on and the id its
// record carries.
type ArenaIdentity struct {
	Host HostId  `json:"host"`
	Id   ArenaId `json:"id"`
}

// Empty reports whether the arena names nothing. Both halves are the identity,
// so a pair missing either one identifies no arena.
func (a ArenaIdentity) Empty() bool { return a.Host == "" || a.Id == "" }

// ArenaRecordPath is where a checkout's arena record lives, relative to the
// checkout: the corpus's own location (org identity.md § Where records are
// kept), written by provisioning and read by everything.
const ArenaRecordPath = ".workspace/arena.json"

// ErrArenaUnknown is a checkout with no arena record: the arena is unknown, and
// nothing that needs its identity can proceed. The recovery is provisioning's,
// because only provisioning creates an arena's id — a program that finds no
// record reports the arena as unknown and never creates one for it.
var ErrArenaUnknown = errors.New("no " + ArenaRecordPath + ": this checkout has no arena identity; run workspace setup")

// ErrHostUnknown is a machine whose host home holds no identity record, or an
// account the platform reports no home for: the host is unknown, and nothing
// that needs the arena's identity can proceed. The recovery is provisioning's,
// for the reason ErrArenaUnknown's is — only provisioning creates a host's id,
// and a person confirms its label as it does.
var ErrHostUnknown = errors.New("no " + hostRecordFile + " in the host home: this machine has no host identity; run workspace setup")

// ArenaAt names the arena a checkout at root is: the HostId READ from the host
// home's record (HostIdAt) and the ArenaId READ from the checkout's arena record
// (ArenaIdAt). Nothing about either is derived — not from the path, not from the
// host name — because a derived id is copied by a cloned image and discloses
// what it was derived from (org identity.md § Ids).
//
// Either record missing is that half unknown — ErrArenaUnknown, ErrHostUnknown —
// and either one malformed is refused naming its file. A pair missing a half
// names no arena, so every failure returns the empty pair.
//
// It is here rather than inside an orchestrator because two parties need the
// answer and must not disagree about it. An orchestrator reads its own arena
// to stamp a lease; a caller holding an ItemInfo asks whether the arena named
// on it is THIS one. A second reader would be a second answer.
//
// Both locations are taken as they stand. root is DeriveArenaRoot, or whatever
// the orchestrator was configured with; nothing here reads the process working
// directory, for the reason DeriveArenaRoot gives at length. hostHome is the
// platform's (forge's logging.HostHome), "" where the account has none; it is a
// parameter so a test can hand it a record without touching the real one.
func ArenaAt(hostHome, root string) (ArenaIdentity, error) {
	id, err := ArenaIdAt(root)
	if err != nil {
		return ArenaIdentity{}, err
	}
	host, err := HostIdAt(hostHome)
	if err != nil {
		return ArenaIdentity{}, err
	}
	return ArenaIdentity{Host: host, Id: id}, nil
}

// ArenaIdAt reads the ArenaId the checkout at root carries in its arena record,
// and nothing else — the half of ArenaAt a party that needs only the id reads,
// so that a machine with no host record does not cost it an arena it has.
//
// A missing record is ErrArenaUnknown. A record that exists and does not parse,
// or whose id is not an arena id, is refused naming the file: it is never
// replaced and never read around, because either would give the arena a second
// identity. The record's label is for display and is not read.
func ArenaIdAt(root string) (ArenaId, error) {
	path := filepath.Join(root, filepath.FromSlash(ArenaRecordPath))
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrArenaUnknown
	}
	if err != nil {
		return "", fmt.Errorf("read the arena record %s: %w", path, err)
	}
	var rec struct {
		Id ArenaId `json:"id"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return "", fmt.Errorf("the arena record %s does not parse, and is refused rather than replaced: %w", path, err)
	}
	if !rec.Id.Valid() {
		return "", fmt.Errorf(
			"the arena record %s names %q, which is not an arena id (a- and 32 lowercase hexadecimal digits), and is refused rather than replaced",
			path, string(rec.Id))
	}
	return rec.Id, nil
}

// HostIdAt reads the HostId from the identity record in hostHome: the host's
// `h-` id, or in a guest the guest's own `g-` id (see HostId).
//
// THE RECORD IS READ BY FORGE'S READER (logging.ReadHostRecord), the one every
// log line on the machine is placed by, so this and the logs cannot disagree
// about which records are machines. What is added here is only what that
// reader folds together: a record that is ABSENT is ErrHostUnknown, and one
// that is THERE and not a host's or a guest's is refused naming the file.
// Reporting the second as unknown would invite creating a second identity.
//
// An empty hostHome is an account the platform reports no home for, which has
// no host home and treats every record as missing (org identity.md § Where
// records are kept).
//
// It reads and nothing else. It never creates, repairs or replaces the record —
// only provisioning writes a host's, with a person confirming its label, and
// only the governor that creates a guest writes the guest's — and it
// never falls back to the host name or anything else at hand. The label is for
// display and is not read.
func HostIdAt(hostHome string) (HostId, error) {
	if hostHome == "" {
		return "", ErrHostUnknown
	}
	if rec, ok := readHostRecord(hostHome); ok {
		return HostId(rec.ID), nil
	}
	path := filepath.Join(hostHome, hostRecordFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w (looked for %s)", ErrHostUnknown, path)
	}
	return "", fmt.Errorf(
		"the host record %s is not a readable host or guest record (kind host with an h- id, or kind guest with a g- id "+
			"and its host's h- id, each 32 lowercase hexadecimal digits), and is refused rather than replaced", path)
}
