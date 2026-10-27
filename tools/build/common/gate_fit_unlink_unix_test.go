//go:build unix

package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The probe's answer for each thing a directory removal can say. The real
// os.Remove runs on the test host's own temp filesystem, which drops a name at
// unlink; the other answers are what a filesystem that keeps one (ntfs-3g, NFS)
// or a removal that simply fails would give, and are injected because no test
// can mount such a filesystem.
func TestHidesUnlinkedFiles(t *testing.T) {
	// keeping answers as a filesystem that keeps the unlinked name would, after
	// checking that the probe really unlinked its file before asking — on the
	// host's filesystem the directory is already empty by then.
	keeping := func(t *testing.T, answer error) func(string) error {
		return func(dir string) error {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("reading the probe directory: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("the directory was removed before its file was unlinked: %v", entries)
			}
			return answer
		}
	}
	for _, tc := range []struct {
		name      string
		removeDir func(t *testing.T) func(string) error
		want      bool
		wantErr   error
	}{
		{
			name:      "native filesystem drops the name",
			removeDir: func(*testing.T) func(string) error { return os.Remove },
		},
		{
			name: "directory not empty",
			removeDir: func(t *testing.T) func(string) error {
				return keeping(t, &os.PathError{Op: "remove", Err: syscall.ENOTEMPTY})
			},
			want: true,
		},
		{
			name:      "directory exists", // POSIX's other spelling of not empty
			removeDir: func(t *testing.T) func(string) error { return keeping(t, syscall.EEXIST) },
			want:      true,
		},
		{
			name: "removal fails for another reason",
			removeDir: func(*testing.T) func(string) error {
				return func(string) error { return syscall.EACCES }
			},
			wantErr: syscall.EACCES,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := filepath.Join(t.TempDir(), ".home", "tmp") // absent: the probe creates it
			var dir string
			remove := tc.removeDir(t)
			got, err := hidesUnlinkedFiles(scratch, func(d string) error { dir = d; return remove(d) })
			if dir == "" || filepath.Dir(dir) != scratch {
				t.Fatalf("removeDir was given %q, want a directory directly under %s", dir, scratch)
			}
			switch {
			case tc.wantErr != nil:
				// An answer the probe cannot read is a failure to measure, never a 0.
				if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), dir) {
					t.Errorf("err = %v, want one wrapping %v and naming %s", err, tc.wantErr, dir)
				}
			case err != nil:
				t.Fatalf("hidesUnlinkedFiles: %v", err)
			case got != tc.want:
				t.Errorf("hidesUnlinkedFiles = %v, want %v", got, tc.want)
			}
			// Whatever the answer, the probe leaves nothing behind on a
			// filesystem that lets it clean up.
			entries, err := os.ReadDir(scratch)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("the probe left %v in %s", entries, scratch)
			}
		})
	}
}

// The probe's answer is deterministic only because its file is still open when
// the directory is removed: closed first, the filesystem's release races the
// removal, and a filesystem that keeps names reads as one that does not. No
// removal answer shows that on a native filesystem, so the test asks the kernel
// which files the process holds — and that the probe hands its descriptor back
// on every path.
func TestHidesUnlinkedFiles_HoldsItsFileOpenAcrossTheRemoval(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd to see open files with")
	}
	for _, tc := range []struct {
		name   string
		remove func(string) error
	}{
		{"native filesystem drops the name", os.Remove},
		{"directory not empty", func(string) error { return syscall.ENOTEMPTY }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dir string
			var held []string
			_, err := hidesUnlinkedFiles(t.TempDir(), func(d string) error {
				resolved, err := filepath.EvalSymlinks(d)
				if err != nil {
					t.Fatal(err)
				}
				dir, held = resolved, filesOpenUnder(t, resolved)
				return tc.remove(d)
			})
			if err != nil {
				t.Fatalf("hidesUnlinkedFiles: %v", err)
			}
			if len(held) != 1 {
				t.Errorf("at the removal the probe held %v open under %s, want its one unlinked file", held, dir)
			}
			if left := filesOpenUnder(t, dir); len(left) != 0 {
				t.Errorf("the probe left %v open", left)
			}
		})
	}
}

// filesOpenUnder lists the files this process holds open below dir, as
// /proc/self/fd names them.
func filesOpenUnder(t *testing.T, dir string) []string {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("listing open files: %v", err)
	}
	var files []string
	for _, fd := range fds {
		// A descriptor closed since the listing has no target, and is not held.
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && strings.HasPrefix(target, dir+string(filepath.Separator)) {
			files = append(files, target)
		}
	}
	return files
}

// A scratch directory that cannot be made is a failure to measure: fit then has
// no answer, which the runner reports, rather than a 0 nobody measured. It is
// asked of the whole gate because the judge reads only what an envelope
// carries — an envelope printed without the metric would pass its cap unread.
func TestFitGate_UnusableScratchIsNoMeasurement(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".home"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := runContractGate(root, "fit", &stdout)
	if err == nil || !strings.Contains(err.Error(), "unlink semantics") {
		t.Errorf("fit --envelope = %v, want a failure naming the unlink probe", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("a run that could not probe printed an envelope: %s", stdout.String())
	}
}

// A filesystem that keeps the name reaches fit as a 1, probed in the
// worktree's own scratch dir: the filesystem every suite's temp root is on,
// which the machine's temp directory need not be.
func TestUnlinkMetrics_ReportsAKeptNameAsOne(t *testing.T) {
	root := t.TempDir()
	var probed string
	metrics, err := unlinkMetricsWith(root, func(dir string) error {
		probed = dir
		return syscall.ENOTEMPTY
	})
	if err != nil {
		t.Fatalf("unlinkMetrics: %v", err)
	}
	if want := filepath.Join(root, ".home", "tmp"); filepath.Dir(probed) != want {
		t.Errorf("probed %q, want a directory directly under %s", probed, want)
	}
	if len(metrics) != 1 || metrics[0].Name != "worktree_hides_unlinked_files" || metrics[0].Type != MetricInt || metrics[0].Int != 1 {
		t.Errorf("unlinkMetrics = %+v, want worktree_hides_unlinked_files = 1", metrics)
	}
}

// fit carries the measurement on Unix, as a unitless count. It is 0 here
// because the host's temp filesystem is a native one — and that is the case
// that must never read 1, since a false 1 reports every machine unfit.
func TestFitGate_ReportsUnlinkSemantics(t *testing.T) {
	var stdout bytes.Buffer
	if err := runContractGate(t.TempDir(), "fit", &stdout); err != nil {
		t.Fatalf("fit --envelope: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("envelope does not parse: %v", err)
	}
	var found []Metric
	for _, m := range env.Metrics {
		if m.Name == "worktree_hides_unlinked_files" {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one worktree_hides_unlinked_files, got %+v", env.Metrics)
	}
	if m := found[0]; m.Type != MetricInt || m.Unit != "" || m.Int != 0 {
		t.Errorf("worktree_hides_unlinked_files = %+v, want a unitless int 0", m)
	}
}
