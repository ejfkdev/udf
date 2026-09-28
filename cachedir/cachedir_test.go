package cachedir

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDirFollowsTheSystemTempDirectory checks the location decision: the cache
// lives under the operating system's temporary directory (which each system
// reclaims on its own terms), and UDF_CACHE_DIR overrides it.
func TestDirFollowsTheSystemTempDirectory(t *testing.T) {
	t.Setenv(Env, "")
	want := filepath.Join(os.TempDir(), "ejfkdev", "udf")
	if got := Dir(); got != want {
		t.Fatalf("Dir() = %s, want %s", got, want)
	}
	if got, want := IndexDir(), filepath.Join(want, "idx"); got != want {
		t.Fatalf("IndexDir() = %s, want %s", got, want)
	}
	if got, want := ScratchDir(), filepath.Join(want, "tmp"); got != want {
		t.Fatalf("ScratchDir() = %s, want %s", got, want)
	}

	custom := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv(Env, custom)
	if got := Dir(); got != custom {
		t.Fatalf("Dir() with %s = %s, want %s", Env, got, custom)
	}
}

// TestTempLandsInScratch checks that per-run scratch goes into the cache
// directory's temporary area rather than the bare system temp directory.
func TestTempLandsInScratch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(Env, dir)

	scratch, err := Temp("run-*")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(scratch) != ScratchDir() {
		t.Fatalf("scratch directory %s is not under %s", scratch, ScratchDir())
	}
	if err := os.WriteFile(filepath.Join(scratch, "disk.raw"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	file, err := TempFile("ova-*.vmdk")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(file.Name()) != ScratchDir() {
		t.Fatalf("scratch file %s is not under %s", file.Name(), ScratchDir())
	}
	_ = file.Close()
}

// TestPruneKeepsFreshAndRemovesStale checks the self-cleaning that stands in for
// systems where the OS does not clean its temporary directory: derived values
// older than CachePruneAge and scratch from an interrupted run older than
// ScratchPruneAge go, everything recent stays.
func TestPruneKeepsFreshAndRemovesStale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(Env, dir)
	for _, sub := range []string{"", "idx", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()

	write := func(path string, age time.Duration) string {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := now.Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}
	scratch := filepath.Join(dir, "tmp", "old-run")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	oldScratchWhen := now.Add(-2 * ScratchPruneAge)
	if err := os.Chtimes(scratch, oldScratchWhen, oldScratchWhen); err != nil {
		t.Fatal(err)
	}

	stale := write(filepath.Join(dir, "stale.json"), CachePruneAge+time.Hour)
	staleIdx := write(filepath.Join(dir, "idx", "stale.idx"), CachePruneAge+time.Hour)
	fresh := write(filepath.Join(dir, "fresh.json"), CachePruneAge-time.Hour)
	freshIdx := write(filepath.Join(dir, "idx", "fresh.idx"), time.Hour)

	Prune(now)

	for _, path := range []string{stale, staleIdx} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived pruning", path)
		}
	}
	for _, path := range []string{fresh, freshIdx} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was pruned: %v", path, err)
		}
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("interrupted run's scratch %s survived pruning", scratch)
	}
}

// TestEnsureCreatesTheTree checks that Ensure prepares the directory the rest of
// the code writes into.
func TestEnsureCreatesTheTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	t.Setenv(Env, dir)

	if got := Ensure(); got != dir {
		t.Fatalf("Ensure() = %q, want %q", got, dir)
	}
	info, err := os.Stat(IndexDir())
	if err != nil || !info.IsDir() {
		t.Fatalf("index directory was not created: %v", err)
	}
}
