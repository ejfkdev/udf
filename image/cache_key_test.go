package image

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCacheKeyTracksFileIdentity checks that a cache key is derived from the
// path, size and timestamps: it stays stable while the file does, and changes
// when the file is rewritten — including when the rewrite keeps the size but
// updates the modification time.
func TestCacheKeyTracksFileIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sample.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	k1 := cacheKeyFor(p, "meta")
	if k2 := cacheKeyFor(p, "meta"); k1 != k2 {
		t.Fatalf("key changed without touching the file: %s vs %s", k1, k2)
	}
	if other := cacheKeyFor(p, "other"); other == k1 {
		t.Fatalf("extra argument did not change the key")
	}
	if other := cacheKeyFor(filepath.Join(dir, "different.bin"), "meta"); other == k1 {
		t.Fatalf("different path did not change the key")
	}

	// Same size, new content and a newer mtime: a different key.
	if err := os.WriteFile(p, []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if k3 := cacheKeyFor(p, "meta"); k3 == k1 {
		t.Fatalf("key did not change after rewriting the file")
	}

	// A deleted file still yields a key (path only), so callers can cache
	// negative results too.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if k4 := cacheKeyFor(p, "meta"); k4 == "" {
		t.Fatalf("no key for a missing file")
	}
}

// TestCacheDirOverride checks the cache location is the per-user directory
// under the system temp dir, and that UDF_CACHE_DIR overrides it.
func TestCacheDirOverride(t *testing.T) {
	t.Setenv("UDF_CACHE_DIR", "")
	want := filepath.Join(os.TempDir(), "ejfkdev", "udf")
	if got := cacheDir(); got != want {
		t.Fatalf("cacheDir() = %s, want %s", got, want)
	}
	custom := filepath.Join(t.TempDir(), "cache")
	t.Setenv("UDF_CACHE_DIR", custom)
	if got := cacheDir(); got != custom {
		t.Fatalf("cacheDir() with override = %s, want %s", got, custom)
	}

	// Round trip through the store/load helpers.
	storeCachedJSON("unit-test-key", map[string]int{"n": 1})
	var got map[string]int
	if !loadCachedJSON("unit-test-key", &got) || got["n"] != 1 {
		t.Fatalf("cache round trip failed: %v", got)
	}
}
