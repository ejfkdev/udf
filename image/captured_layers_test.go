package image

import (
	"archive/tar"
	"testing"

	"github.com/ejfkdev/udf/fsview"
	arch "github.com/ejfkdev/udf/image/archive"
)

// TestBuildFileSystemUsesCapturedLayers checks the wiring between an index
// build and the tree build: layers captured during the scan are what the merged
// tree is built from, and a capture that is missing a layer falls back to
// reading the archive.
func TestBuildFileSystemUsesCapturedLayers(t *testing.T) {
	fixture := buildTestImage(t, map[string][]layerTarEntry{
		"base.tar": {imgFile("etc/passwd", "root:x:0:0")},
		"top.tar":  {imgFile("etc/motd", "hello")},
	})
	meta, err := ScanImageMetadata(fixture.ImagePath, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.LayerOrder) != 2 {
		t.Fatalf("fixture has %d layers", len(meta.LayerOrder))
	}

	// A capture that claims both layers but holds other content: if that
	// content shows up in the tree, the capture is what the tree was built
	// from.
	captured := map[string]*fsview.ParsedLayer{
		meta.LayerOrder[0]: fsview.NewParsedLayer([]*tar.Header{{
			Name: "captured/only.txt", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg,
		}}),
		meta.LayerOrder[1]: fsview.NewParsedLayer([]*tar.Header{{
			Name: "captured/second.txt", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg,
		}}),
	}
	key := cacheKeyFor(fixture.ImagePath, "gzipidx")
	capturedLayers.Store(key, captured)
	defer capturedLayers.Delete(key)

	archive, err := arch.Open(fixture.ImagePath)
	if err != nil {
		t.Fatal(err)
	}
	tree, ok := buildFileSystemBulk(archive, fixture.ImagePath, meta)
	if !ok {
		t.Fatal("bulk tree build failed")
	}
	if tree.Resolve("captured/only.txt") == nil {
		t.Fatalf("tree was not built from the captured layers")
	}

	// A capture missing one layer is unusable as a whole: the tree then comes
	// from the archive, so the captured content must not appear.
	partial := map[string]*fsview.ParsedLayer{meta.LayerOrder[0]: captured[meta.LayerOrder[0]]}
	capturedLayers.Store(key, partial)
	tree, ok = buildFileSystemBulk(archive, fixture.ImagePath, meta)
	if !ok {
		t.Fatal("bulk tree build failed with a partial capture")
	}
	if tree.Resolve("captured/only.txt") != nil {
		t.Fatalf("a partial capture was used")
	}
	if tree.Resolve("etc/passwd") == nil {
		t.Fatalf("fallback tree is missing etc/passwd")
	}
}
