package image

import (
	"os"
	"testing"
	"time"
)

// TestSelectionErrorComesFromTheIndex guards the metadata path against reading
// the archive sequentially after the index already answered. A multi-image
// archive asked for without a selection is an error either way, so the
// expensive path must not be taken — and a repeat of the failing call must be
// cheap, since the error is not cached.
//
// Point UDF_BIGGZ at a multi-image docker-save .tar.gz to run it.
func TestSelectionErrorComesFromTheIndex(t *testing.T) {
	path := os.Getenv("UDF_BIGGZ")
	if path == "" {
		t.Skip("set UDF_BIGGZ to a multi-image docker-save .tar.gz")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("no sample")
	}
	t.Setenv("UDF_CACHE_DIR", t.TempDir())

	start := time.Now()
	_, err := ScanImageMetadata(path, Selection{ImageIndex: -1})
	build := time.Since(start)
	if err == nil {
		t.Skip("sample holds a single image, nothing to select between")
	}

	start = time.Now()
	_, repeat := ScanImageMetadata(path, Selection{ImageIndex: -1})
	repeatTime := time.Since(start)
	if repeat == nil {
		t.Fatalf("second call did not fail the same way")
	}
	if repeatTime > 5*time.Second {
		t.Fatalf("repeating the failing call took %v: the archive was read again", repeatTime)
	}
	t.Logf("index build %v, repeated failing call %v", build, repeatTime)
}
