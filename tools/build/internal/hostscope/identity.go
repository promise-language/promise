package hostscope

// The arena identity the exclusion is held by and re-entered on, copied from
// flow's identity.go on the terms the package comment states. A runner names
// its holder flow.ArenaAt(checkout); a nested verify must compute the identical
// pair from the same checkout, or it is a peer queueing behind its own parent.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HostId is the machine an arena lives on: the host's SHORT NAME — its first
// dotted segment — normalized.
//
// It MUST be unique across every host the orchestrator can see. A duplicate is
// not a cosmetic collision: arenas are addressed by (HostId, ArenaId), so two
// machines sharing an id merge into one identity and the one-to-one claim
// invariant stops holding. Uniqueness cannot be inherited from the FQDN,
// because the short form drops the domain — build01.us-east and build01.eu-west
// both normalize to build01 — so a fleet spanning domains must assign unique
// short names rather than relying on the domain to separate them.
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

// ArenaAt names the arena a checkout at root is: the machine's derived HostId
// and the ArenaId READ from the checkout's arena record. Nothing about the id is
// derived — not from the path, not from the host — because a derived id is
// copied by a cloned image and discloses what it was derived from (org
// identity.md § Ids).
//
// A missing record is ErrArenaUnknown. A record that exists and does not parse,
// or whose id is not an arena id, is refused naming the file: it is never
// replaced and never read around, because either would give the arena a second
// identity. The record's label is for display and is not read.
func ArenaAt(root string) (ArenaIdentity, error) {
	path := filepath.Join(root, filepath.FromSlash(ArenaRecordPath))
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ArenaIdentity{}, ErrArenaUnknown
	}
	if err != nil {
		return ArenaIdentity{}, fmt.Errorf("read the arena record %s: %w", path, err)
	}
	var rec struct {
		Id ArenaId `json:"id"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return ArenaIdentity{}, fmt.Errorf("the arena record %s does not parse, and is refused rather than replaced: %w", path, err)
	}
	if !rec.Id.Valid() {
		return ArenaIdentity{}, fmt.Errorf(
			"the arena record %s names %q, which is not an arena id (a- and 32 lowercase hexadecimal digits), and is refused rather than replaced",
			path, string(rec.Id))
	}
	return ArenaIdentity{Host: DeriveHostId(), Id: rec.Id}, nil
}

// NormalizeHostId reduces a machine name to its HostId: the first dotted
// segment, lowercased.
//
// The normalization is a requirement rather than a courtesy — these are
// compared by string equality across systems, so two spellings of one host are
// two hosts.
func NormalizeHostId(name string) HostId {
	name = strings.TrimSpace(name)
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return HostId(strings.ToLower(name))
}

// DeriveHostId returns this machine's HostId, normalized from its own name.
//
// A machine that cannot name itself yields the empty HostId rather than a
// guess: an invented name would be a second machine's identity waiting to
// collide.
func DeriveHostId() HostId {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return NormalizeHostId(name)
}
