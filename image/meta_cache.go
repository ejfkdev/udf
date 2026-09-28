package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/djherbis/times"

	"github.com/ejfkdev/udf/cachedir"
)

// cacheKeyFor derives a stable, small cache key from the source file's
// identity: its path, size and timestamps — modification time, plus change and
// creation time where the platform reports them. Nothing reads the file
// contents, so deriving a key costs one stat, and any edit that changes the
// size or a timestamp lands on a different key.
func cacheKeyFor(path string, args ...string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, cacheFormat)
	_, _ = io.WriteString(h, path)
	if st, err := os.Stat(path); err == nil {
		_, _ = fmt.Fprintf(h, "\x00%d\x00%d", st.Size(), st.ModTime().UnixNano())
	}
	if ts, err := times.Stat(path); err == nil {
		if ts.HasChangeTime() {
			_, _ = fmt.Fprintf(h, "\x00%d", ts.ChangeTime().UnixNano())
		}
		if ts.HasBirthTime() {
			_, _ = fmt.Fprintf(h, "\x00%d", ts.BirthTime().UnixNano())
		}
	}
	for _, a := range args {
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, a)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cacheFormat invalidates every cached entry when the shape of a cached value
// changes: a stale entry would otherwise be read back without its new fields.
const cacheFormat = "udf-cache-2"

// cacheDir returns the directory holding small derived data (file listings,
// image metadata, archive indexes). It is the per-user directory under the
// system temporary directory, so the operating system reclaims it on its own
// schedule; see the cachedir package. UDF_CACHE_DIR overrides it.
func cacheDir() string { return cachedir.Dir() }

func loadCachedJSON(key string, v any) bool {
	dir := cacheDir()
	if dir == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

func storeCachedJSON(key string, v any) {
	dir := cachedir.Ensure()
	if dir == "" {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	written, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil && cerr == nil && written == len(data) {
		_ = os.Rename(tmp.Name(), filepath.Join(dir, key+".json"))
	} else {
		_ = os.Remove(tmp.Name())
	}
}
