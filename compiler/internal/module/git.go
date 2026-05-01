package module

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GlobalCacheDir returns the global module cache directory (~/.promise/cache/modules/).
// Creates it if it doesn't exist.
func GlobalCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".promise", "cache", "modules")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cannot create global cache directory: %w", err)
	}
	return dir, nil
}

// URLToCachePath maps a normalized URL to a filesystem path under the cache directory.
// e.g., "github.com/someone/parser" → "<cacheDir>/github.com/someone/parser"
func URLToCachePath(cacheDir, normalizedURL string) string {
	return filepath.Join(cacheDir, filepath.FromSlash(normalizedURL))
}

// ResolveRemoteModule ensures the repo at `url` is checked out at `commitHash`
// in the global cache. Returns the absolute path to the checkout directory.
func ResolveRemoteModule(url, commitHash string) (string, error) {
	if err := requireGit(); err != nil {
		return "", err
	}

	normalized := NormalizeURL(url)
	cacheDir, err := GlobalCacheDir()
	if err != nil {
		return "", err
	}

	modCacheDir := URLToCachePath(cacheDir, normalized)
	if err := os.MkdirAll(modCacheDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create module cache directory: %w", err)
	}

	repoDir := filepath.Join(modCacheDir, "repo.git")
	// Use first 12 chars of commit hash as checkout directory name.
	checkoutSuffix := commitHash
	if len(checkoutSuffix) > 12 {
		checkoutSuffix = checkoutSuffix[:12]
	}
	checkoutDir := filepath.Join(modCacheDir, checkoutSuffix)

	// Fast path: checkout already exists
	if info, err := os.Stat(checkoutDir); err == nil && info.IsDir() {
		toml := filepath.Join(checkoutDir, "promise.toml")
		if _, err := os.Stat(toml); err == nil {
			return checkoutDir, nil
		}
	}

	// Acquire filesystem lock to prevent concurrent clone/fetch races
	lockPath := filepath.Join(modCacheDir, ".lock")
	unlock, err := acquireLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("cannot acquire lock for %s: %w", normalized, err)
	}
	defer unlock()

	// Re-check after acquiring lock (another process may have completed)
	if info, err := os.Stat(checkoutDir); err == nil && info.IsDir() {
		toml := filepath.Join(checkoutDir, "promise.toml")
		if _, err := os.Stat(toml); err == nil {
			return checkoutDir, nil
		}
	}

	// Ensure bare repo exists
	if err := ensureBareRepo(repoDir, url); err != nil {
		return "", fmt.Errorf("cannot fetch %s: %w", url, err)
	}

	// Checkout the specific commit
	if err := ensureCheckout(repoDir, checkoutDir, commitHash); err != nil {
		return "", fmt.Errorf("cannot checkout %s at %s: %w", url, commitHash, err)
	}

	return checkoutDir, nil
}

// PinResolve resolves a human-friendly ref (tag, branch, "HEAD", or short hash prefix)
// to a full commit SHA for the given remote URL.
func PinResolve(url, ref string) (string, error) {
	if err := requireGit(); err != nil {
		return "", err
	}

	// If ref looks like a full 40-char hex SHA, just validate format
	if IsFullCommitHash(ref) {
		return ref, nil
	}

	// Empty ref or "HEAD" — resolve to default branch
	if ref == "" || ref == "HEAD" {
		out, err := runGit("", "ls-remote", url, "HEAD")
		if err != nil {
			return "", fmt.Errorf("cannot resolve HEAD from %s: %w", url, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if parts := strings.Fields(line); len(parts) >= 2 {
				return parts[0], nil
			}
		}
		return "", fmt.Errorf("cannot resolve HEAD from %s: no HEAD ref found", url)
	}

	// Use git ls-remote to resolve tags and branches by name
	out, err := runGit("", "ls-remote", url, ref)
	if err != nil {
		return "", fmt.Errorf("cannot resolve ref %q from %s: %w", ref, url, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if parts := strings.Fields(line); len(parts) >= 2 {
			return parts[0], nil
		}
	}

	return "", fmt.Errorf("ref %q not found in %s", ref, url)
}

// IsFullCommitHash returns true if s is a 40-character hex string.
func IsFullCommitHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// IsCommitHashLike returns true if s looks like a commit hash (7-40 hex chars).
func IsCommitHashLike(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ensureBareRepo clones or fetches the bare repo at repoDir.
func ensureBareRepo(repoDir, url string) error {
	if _, err := os.Stat(filepath.Join(repoDir, "HEAD")); err == nil {
		// Bare repo exists — fetch latest
		_, err := runGit(repoDir, "fetch", "--quiet", url, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
		return err
	}

	// Clone as bare repo
	_, err := runGit("", "clone", "--bare", "--quiet", url, repoDir)
	return err
}

// ensureCheckout populates checkoutDir with the working tree at commitHash.
func ensureCheckout(repoDir, checkoutDir, commitHash string) error {
	if err := os.MkdirAll(checkoutDir, 0755); err != nil {
		return err
	}

	// Use git archive to extract the tree (cleaner than checkout for a bare repo)
	// Falls back to --work-tree approach if archive fails.
	_, err := runGitPiped(repoDir, checkoutDir, commitHash)
	if err != nil {
		// Fallback: use --work-tree
		_, err = runGit("", "--work-tree="+checkoutDir, "--git-dir="+repoDir, "checkout", commitHash, "--", ".")
		if err != nil {
			// Clean up partial checkout on failure
			os.RemoveAll(checkoutDir)
			return err
		}
	}

	return nil
}

// runGitPiped runs `git archive <commit> | tar x` in checkoutDir.
func runGitPiped(repoDir, checkoutDir, commitHash string) (string, error) {
	archive := exec.Command("git", "--git-dir="+repoDir, "archive", "--format=tar", commitHash)
	untar := exec.Command("tar", "xf", "-")
	untar.Dir = checkoutDir

	pipe, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	untar.Stdin = pipe

	if err := archive.Start(); err != nil {
		return "", err
	}
	if err := untar.Start(); err != nil {
		archive.Process.Kill()
		archive.Wait() // reap the killed process
		return "", err
	}

	archiveErr := archive.Wait()
	untarErr := untar.Wait()
	if archiveErr != nil {
		return "", archiveErr
	}
	if untarErr != nil {
		return "", untarErr
	}

	return "", nil
}

// requireGit checks that git is available on PATH.
func requireGit() error {
	_, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git is required for remote modules but was not found on PATH")
	}
	return nil
}

// runGit runs a git command and returns its combined output.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// acquireLock creates an exclusive lock file. Returns an unlock function.
// Retries for up to 30 seconds if the lock is held.
func acquireLock(lockPath string) (func(), error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		// Check if lock is stale (older than 5 minutes)
		if info, statErr := os.Stat(lockPath); statErr == nil {
			if time.Since(info.ModTime()) > 5*time.Minute {
				os.Remove(lockPath)
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for lock %s (another process may be fetching this module)", lockPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// CleanGlobalCache removes the entire global module cache.
func CleanGlobalCache() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".promise", "cache", "modules")
	return os.RemoveAll(dir)
}
