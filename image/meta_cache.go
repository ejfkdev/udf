package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// cacheKeyFor derives a stable, small cache key from the source file's identity
// (path, size, mtime) plus any extra distinguishing arguments. The key changes
// when the source file changes, so cached metadata is never stale.
func cacheKeyFor(path string, args ...string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, path)
	if st, err := os.Stat(path); err == nil {
		_, _ = fmt.Fprintf(h, "\x00%d\x00%d", st.Size(), st.ModTime().UnixNano())
	}
	for _, a := range args {
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, a)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cacheDir returns the per-user cache directory for small derived metadata
// (partition layout, listings), or "" when it is unavailable.
func cacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "udf", "meta")
}

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
	dir := cacheDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
