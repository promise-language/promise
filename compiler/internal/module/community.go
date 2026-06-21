package module

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// DefaultCommunityCatalogURL is the well-known location of the community catalog
// repo (§9.9). It is a living git repo (not frozen in the compiler binary), so a
// module's per-epoch compatibility can be recorded WITHOUT a compiler update.
const DefaultCommunityCatalogURL = "https://github.com/promise-community/catalog"

// CommunityCatalogURL returns the community catalog repo URL, honoring the
// PROMISE_COMMUNITY_CATALOG override (mirrors / air-gapped environments, §17) and
// falling back to the well-known constant. Mirrors the PROMISE_HOME / release-URL
// override convention so a single env var redirects the whole community tier.
func CommunityCatalogURL() string {
	if u := strings.TrimSpace(os.Getenv("PROMISE_COMMUNITY_CATALOG")); u != "" {
		return u
	}
	return DefaultCommunityCatalogURL
}

// CommunityEntry is one module in the community catalog's name→URL map.
type CommunityEntry struct {
	Name        string // module name (the TOML key, e.g. "foo")
	URL         string // fetch-ready git URL
	Description string // human-readable description
}

// CommunityCatalog is the parsed name→URL map (modules.toml) of the community
// catalog repo. The per-epoch compatibility index is a separate payload
// (CompatIndex / LoadCompatIndex).
type CommunityCatalog struct {
	Modules map[string]*CommunityEntry // name → entry
}

// Lookup returns the community entry for name, or nil if not listed.
func (c *CommunityCatalog) Lookup(name string) *CommunityEntry {
	if c == nil {
		return nil
	}
	return c.Modules[name]
}

// LookupByURL returns the community entry whose URL matches url (normalized), or
// nil if no listed module points at that URL. Used by `pkg update` to re-resolve
// an existing community [require] pin through the fresh index.
func (c *CommunityCatalog) LookupByURL(url string) *CommunityEntry {
	if c == nil {
		return nil
	}
	n := NormalizeURL(url)
	for _, e := range c.Modules {
		if NormalizeURL(e.URL) == n {
			return e
		}
	}
	return nil
}

// ParseCommunityModules parses the community catalog's modules.toml (the name→URL
// map). Format mirrors catalog.toml but every entry carries a url (no commit, no
// epoch — versioning is per-epoch in the compatibility index, not here):
//
//	[modules.foo]
//	url = "https://github.com/promise-community/foo"
//	description = "..."
func ParseCommunityModules(data []byte) (*CommunityCatalog, error) {
	cc := &CommunityCatalog{Modules: make(map[string]*CommunityEntry)}

	var section string
	var current *CommunityEntry

	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("modules.toml:%d: invalid section header: %s", lineNum, line)
			}
			section = line[1 : len(line)-1]
			current = nil
			if strings.HasPrefix(section, "modules.") {
				name := section[len("modules."):]
				if name == "" {
					return nil, fmt.Errorf("modules.toml:%d: empty module name in section [%s]", lineNum, section)
				}
				current = &CommunityEntry{Name: name}
				cc.Modules[name] = current
			}
			continue
		}
		key, val, err := parseTOMLLine(line)
		if err != nil {
			return nil, fmt.Errorf("modules.toml:%d: %w", lineNum, err)
		}
		if current != nil {
			switch key {
			case "url":
				current.URL = val
			case "description":
				current.Description = val
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading modules.toml: %w", err)
	}

	for name, e := range cc.Modules {
		if e.URL == "" {
			return nil, fmt.Errorf("modules.toml: module '%s' is missing 'url'", name)
		}
	}
	return cc, nil
}

// IndexEntry is one module's verified-commit record for a single epoch.
type IndexEntry struct {
	Commit       string `json:"commit"`
	Tag          string `json:"tag,omitempty"`
	VerifiedAt   string `json:"verified_at,omitempty"`
	CompilerHash string `json:"compiler_hash,omitempty"`
}

// CompatIndex is the per-epoch compatibility index — one JSON file per epoch
// (index/<epoch>.json) in the community catalog repo. A module ABSENT from the
// index for an epoch means "no verified version for that epoch" (§9.10). Per-epoch
// files (vs one big file) keep the repo append-only across epochs with clean diffs
// and let a post-release verdict be added without rewriting prior epochs.
type CompatIndex struct {
	Epoch   string                `json:"epoch"`
	Modules map[string]IndexEntry `json:"modules"`
}

// ParseCompatIndex parses an index/<epoch>.json payload.
func ParseCompatIndex(data []byte) (*CompatIndex, error) {
	var idx CompatIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parsing compat index: %w", err)
	}
	if idx.Modules == nil {
		idx.Modules = make(map[string]IndexEntry)
	}
	return &idx, nil
}

// Verified returns the recorded entry for name and true when the index records a
// verified commit for it, or (zero, false) when the module is absent — the §9.10
// "no verified version for this epoch" signal.
func (idx *CompatIndex) Verified(name string) (IndexEntry, bool) {
	if idx == nil {
		return IndexEntry{}, false
	}
	e, ok := idx.Modules[name]
	if !ok || e.Commit == "" {
		return IndexEntry{}, false
	}
	return e, true
}

// LoadCompatIndex reads index/<epoch>.json from a fetched community catalog
// checkout. Returns (nil, nil) when no index file exists for the epoch yet — an
// epoch with no recorded verdicts is the same, for resolution, as a module absent
// from the index (§9.10).
func LoadCompatIndex(catalogDir, epoch string) (*CompatIndex, error) {
	path := filepath.Join(catalogDir, "index", epoch+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseCompatIndex(data)
}

// SaveCompatIndex writes idx to index/<epoch>.json under catalogDir, creating the
// index directory if necessary. Used by the catalog CI matrix builder
// (`promise package build-index`).
func SaveCompatIndex(catalogDir string, idx *CompatIndex) error {
	dir := filepath.Join(catalogDir, "index")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dir, idx.Epoch+".json"), data, 0644)
}

// IndexedEpochs returns the epochs that have an index/<epoch>.json file, sorted
// ascending (numeric epoch compare). Non-numeric filenames are ignored. Used to
// build the published module × epoch matrix.
func IndexedEpochs(catalogDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(catalogDir, "index"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var epochs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ep := strings.TrimSuffix(e.Name(), ".json")
		if _, _, ok := ParseEpoch(ep); !ok {
			continue
		}
		epochs = append(epochs, ep)
	}
	sortEpochsAsc(epochs)
	return epochs, nil
}

// HighestIndexedEpoch scans all per-epoch index files and returns the largest
// epoch (and its recorded tag) for which name has a verified commit, or ("","")
// when name is recorded for no epoch. Feeds the §9.10 "highest verified epoch"
// message in the community tier.
func HighestIndexedEpoch(catalogDir, name string) (epoch, tag string) {
	epochs, err := IndexedEpochs(catalogDir)
	if err != nil {
		return "", ""
	}
	for _, ep := range epochs {
		idx, err := LoadCompatIndex(catalogDir, ep)
		if err != nil || idx == nil {
			continue
		}
		if ie, ok := idx.Verified(name); ok {
			if epoch == "" || CompareEpochs(ep, epoch) > 0 {
				epoch, tag = ep, ie.Tag
			}
		}
	}
	return epoch, tag
}

// sortEpochsAsc sorts epoch strings ascending by numeric epoch compare.
func sortEpochsAsc(epochs []string) {
	for i := 1; i < len(epochs); i++ {
		for j := i; j > 0 && CompareEpochs(epochs[j-1], epochs[j]) > 0; j-- {
			epochs[j-1], epochs[j] = epochs[j], epochs[j-1]
		}
	}
}

// Tier classifies a module by its URL (§9.9 — "no separate flag"). It drives the
// verdict source: community modules trust the catalog's CI index; ad-hoc modules
// are verified locally on add.
type Tier int

const (
	TierEmbedded   Tier = iota // no URL — source lives in the compiler binary
	TierFirstParty             // github.com/promise-language/*
	TierCommunity              // github.com/promise-community/* (community catalog)
	TierAdHoc                  // any other git URL
)

func (t Tier) String() string {
	switch t {
	case TierEmbedded:
		return "embedded"
	case TierFirstParty:
		return "first-party"
	case TierCommunity:
		return "community"
	default:
		return "ad-hoc"
	}
}

// ModuleTier returns the tier for a module URL. The empty URL is the embedded
// tier; the promise-language / promise-community GitHub orgs are first-party /
// community; everything else is ad-hoc. The host+org prefix is matched
// case-insensitively (GitHub orgs are case-insensitive) after URL normalization,
// so scheme/.git/case variants all classify the same.
func ModuleTier(url string) Tier {
	if strings.TrimSpace(url) == "" {
		return TierEmbedded
	}
	n := strings.ToLower(NormalizeURL(url))
	switch {
	case strings.HasPrefix(n, "github.com/promise-language/"):
		return TierFirstParty
	case strings.HasPrefix(n, "github.com/promise-community/"):
		return TierCommunity
	default:
		return TierAdHoc
	}
}

// communityRefreshed latches a successful refresh for the lifetime of the
// process: a single command that resolves several community modules (e.g.
// `pkg update` over many deps) must refresh the living catalog ONCE, not once per
// module — re-fetching + re-checking-out the same small repo per dependency is
// redundant network + disk work. After the first refresh, subsequent refresh
// requests reuse that just-fetched checkout. No reset is needed: each CLI
// invocation is a fresh process, so the latch naturally re-arms next run.
var communityRefreshed atomic.Bool

// FetchCommunityCatalog clones or refreshes the community catalog repo into the
// global cache and returns the checkout directory (root of modules.toml +
// index/). With refresh=true the repo's default-branch HEAD is re-fetched and the
// checkout replaced (the living-repo property — used on `package add`/`update`);
// with refresh=false a present checkout is reused as-is. A refresh is performed at
// most once per process (see communityRefreshed) so a multi-dependency command
// does not re-fetch the catalog for every module.
func FetchCommunityCatalog(refresh bool) (string, error) {
	if err := requireGit(); err != nil {
		return "", err
	}
	if refresh && communityRefreshed.Load() {
		refresh = false
	}
	home, err := PromiseHome()
	if err != nil {
		return "", fmt.Errorf("cannot determine Promise home: %w", err)
	}
	baseDir := filepath.Join(home, "cache", "community-catalog")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create community catalog cache directory: %w", err)
	}
	repoDir := filepath.Join(baseDir, "repo.git")
	checkoutDir := filepath.Join(baseDir, "checkout")

	// Fast path: reuse a present checkout when no refresh is requested.
	if !refresh {
		if info, err := os.Stat(checkoutDir); err == nil && info.IsDir() {
			return checkoutDir, nil
		}
	}

	lockPath := filepath.Join(baseDir, ".lock")
	unlock, err := acquireLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("cannot acquire lock for community catalog: %w", err)
	}
	defer unlock()

	// Re-check after acquiring the lock (another process may have populated it).
	if !refresh {
		if info, err := os.Stat(checkoutDir); err == nil && info.IsDir() {
			return checkoutDir, nil
		}
	}

	url := CommunityCatalogURL()
	if err := ensureBareRepo(repoDir, url); err != nil {
		return "", fmt.Errorf("cannot fetch community catalog %s: %w", url, err)
	}
	head, err := runGit(repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("cannot resolve community catalog HEAD: %w", err)
	}
	commit := strings.TrimSpace(head)

	// Replace the stale checkout so the refreshed tree lands cleanly.
	if err := os.RemoveAll(checkoutDir); err != nil {
		return "", err
	}
	if err := ensureCheckout(repoDir, checkoutDir, commit); err != nil {
		return "", fmt.Errorf("cannot checkout community catalog: %w", err)
	}
	communityRefreshed.Store(true)
	return checkoutDir, nil
}
