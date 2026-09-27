package image

import (
	"os"
	"testing"
	"time"

	"github.com/ejfkdev/udf/fsview"
)

// TestCapturedLayersMatchIndexTree compares the two ways to a merged tree on a
// real archive: the layers captured while the index was built, and the layers
// read back through that index. The trees must be identical, entry for entry.
//
// Point UDF_BIGGZ at a large docker-save .tar.gz to run it; without it the
// test skips.
func TestCapturedLayersMatchIndexTree(t *testing.T) {
	path := os.Getenv("UDF_BIGGZ")
	if path == "" {
		t.Skip("set UDF_BIGGZ to a large docker-save .tar.gz")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("no sample")
	}
	t.Setenv("UDF_CACHE_DIR", t.TempDir())

	tag := os.Getenv("UDF_REPO_TAG")
	meta, err := ScanImageMetadata(path, Selection{RepoTag: tag, ImageIndex: -1})
	if err != nil {
		t.Fatalf("scan metadata: %v", err)
	}
	if tag == "" && meta.Total > 1 {
		meta, err = ScanImageMetadata(path, Selection{ImageIndex: 0})
		if err != nil {
			t.Fatalf("scan metadata: %v", err)
		}
	}

	captured, ok := capturedLayerDirectories(path, meta.LayerOrder)
	if !ok {
		t.Fatalf("no layers were captured during the index build")
	}
	t0 := time.Now()
	fromCapture, err := fsview.BuildParsed(meta.LayerOrder, captured)
	if err != nil {
		t.Fatalf("build from capture: %v", err)
	}
	captureTime := time.Since(t0)

	r, ok := ensureImageIndex(path)
	if !ok {
		t.Fatal("index unusable")
	}
	defer r.Close()
	t0 = time.Now()
	layers, ok := parseLayersFromIndex(r, meta.LayerOrder)
	if !ok {
		t.Fatal("layers through the index failed")
	}
	indexTime := time.Since(t0)
	fromIndex, err := fsview.BuildParsed(meta.LayerOrder, layers)
	if err != nil {
		t.Fatalf("build from index: %v", err)
	}

	diff := diffTrees(fromCapture, fromIndex)
	if diff != "" {
		t.Fatalf("captured tree differs from the index tree: %s", diff)
	}
	t.Logf("identical trees: layers=%d nodes(captured)=%d merge captured=%v index=%v",
		len(meta.LayerOrder), countNodes(fromCapture), captureTime, indexTime)
}

func countNodes(n *fsview.Node) int {
	total := 1
	for _, c := range n.Children {
		total += countNodes(c)
	}
	return total
}

// diffTrees reports the first structural difference between two trees.
func diffTrees(a, b *fsview.Node) string {
	if a == nil || b == nil {
		if a != b {
			return "one tree is nil"
		}
		return ""
	}
	if a.Name != b.Name || a.Kind != b.Kind || a.Size != b.Size || a.Mode != b.Mode ||
		a.Linkname != b.Linkname || a.UID != b.UID || a.GID != b.GID ||
		a.Uname != b.Uname || a.Gname != b.Gname || a.Devmajor != b.Devmajor || a.Devminor != b.Devminor {
		return "node fields differ: " + a.Path
	}
	if len(a.Children) != len(b.Children) {
		return "child count differs at " + a.Path
	}
	for i := range a.Children {
		if d := diffTrees(a.Children[i], b.Children[i]); d != "" {
			return d
		}
	}
	return ""
}
