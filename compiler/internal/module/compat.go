package module

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CompatVerdict records the outcome of verifying one (url, commit, epoch) tuple:
// whether that commit compiled and passed 100% of its “ `test “ functions under
// the given epoch. It is the local, ad-hoc analog of the community catalog's
// per-epoch compatibility index (§9.9) — never published anywhere central.
type CompatVerdict struct {
	URL          string `json:"url"`
	Commit       string `json:"commit"`
	Epoch        string `json:"epoch"`
	Compatible   bool   `json:"compatible"`
	CompilerHash string `json:"compiler_hash"`         // fingerprint of the verifying compiler
	FailReason   string `json:"fail_reason,omitempty"` // populated when !Compatible
}

// compatCacheDir returns <PromiseHome>/compat, creating it if necessary.
func compatCacheDir() (string, error) {
	home, err := PromiseHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "compat")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cannot create compat cache directory: %w", err)
	}
	return dir, nil
}

// compatKey is the cache key for a verdict: url@commit#epoch, normalized so two
// spellings of the same remote share a verdict.
func compatKey(url, commit, epoch string) string {
	return hashString(NormalizeURL(url) + "@" + commit + "#" + epoch)
}

// LookupCompat returns a previously recorded verdict for (url, commit, epoch), or
// (nil, false) when none is cached. A verdict recorded by a different compiler
// build (CompilerHash mismatch) is treated as absent: a rebuilt compiler can flip
// a verdict (a source-breaking patch within an epoch), so "verify, never assume"
// must re-run rather than trust a stale yes/no.
func LookupCompat(url, commit, epoch string) (*CompatVerdict, bool) {
	dir, err := compatCacheDir()
	if err != nil {
		return nil, false
	}
	path := filepath.Join(dir, compatKey(url, commit, epoch)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var v CompatVerdict
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, false
	}
	if v.CompilerHash != CompilerHash() {
		return nil, false
	}
	return &v, true
}

// SaveCompat writes a verdict to the local compat cache. CompilerHash is stamped
// here so the caller does not have to remember to set it.
func SaveCompat(v *CompatVerdict) error {
	dir, err := compatCacheDir()
	if err != nil {
		return err
	}
	v.CompilerHash = CompilerHash()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, compatKey(v.URL, v.Commit, v.Epoch)+".json")
	return os.WriteFile(path, data, 0644)
}
