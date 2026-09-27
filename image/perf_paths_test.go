package image

import (
	"archive/tar"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ejfkdev/udf/fsview"
	"github.com/ejfkdev/udf/types"
)

// writeTestImageOrdered writes an image whose layer entries appear in the
// archive in `physical` order while manifest.json lists them in
// `manifestOrder` — the situation that forces the single-pass merge (random
// access would need one decompression per layer).
func writeTestImageOrdered(t *testing.T, physical, manifestOrder []string, layers map[string][]layerTarEntry) string {
	t.Helper()

	imagePath := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(imagePath)
	if err != nil {
		t.Fatalf("create image tar: %v", err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	cfg, err := json.Marshal(types.ImageConfig{Architecture: "amd64"})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeTarFile(t, tw, "config.json", cfg)

	for _, name := range physical {
		writeTarFile(t, tw, name, buildLayerBytesForTest(t, layers[name]))
	}

	manifestBody, err := json.Marshal([]types.ManifestItem{{
		Config:   "config.json",
		RepoTags: []string{"repo/app:latest"},
		Layers:   manifestOrder,
	}})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeTarFile(t, tw, "manifest.json", manifestBody)

	if err := tw.Close(); err != nil {
		t.Fatalf("close image tar writer: %v", err)
	}
	return imagePath
}

// orderedLayers is a small image whose physical layer order (base last) is
// the reverse of the manifest order, with a hardlink in a middle layer whose
// source file is defined by a later physical layer.
func orderedLayers() (physical, manifestOrder []string, layers map[string][]layerTarEntry) {
	layers = map[string][]layerTarEntry{
		"base.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "base:x:0:0"),
			imgFile("usr/bin/tool", "base tool"),
			imgDir("usr"),
			imgDir("usr/bin"),
		},
		"mid.tar": {
			imgFile("etc/passwd", "mid:x:0:0"),
			imgHardlink("usr/bin/tool-link", "usr/bin/tool"),
		},
		"top.tar": {
			imgFile("usr/bin/tool", "top tool"),
			imgFile("etc/added.txt", "added"),
			imgFile("etc/gone.txt", "gone"),
		},
		"late.tar": {
			imgWhiteout("etc/.wh.gone.txt"),
		},
	}
	manifestOrder = []string{"base.tar", "mid.tar", "top.tar", "late.tar"}
	physical = []string{"late.tar", "top.tar", "mid.tar", "base.tar"}
	return physical, manifestOrder, layers
}

func listingOf(t *testing.T, root *fsview.Node) string {
	t.Helper()
	out, err := FormatListing(root, "/")
	if err != nil {
		t.Fatalf("format listing: %v", err)
	}
	return out
}

func TestOrderedImageMergesInManifestOrder(t *testing.T) {
	physical, manifestOrder, layers := orderedLayers()
	imagePath := writeTestImageOrdered(t, physical, manifestOrder, layers)
	meta := scanTestMeta(t, imagePath)

	root, err := BuildFileSystem(imagePath, meta)
	if err != nil {
		t.Fatalf("build filesystem: %v", err)
	}
	listing := listingOf(t, root)
	for _, want := range []string{"etc", "usr"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("listing missing %q:\n%s", want, listing)
		}
	}
	if root.Resolve("etc/gone.txt") != nil {
		t.Fatalf("whiteout from a manifest-later layer was not applied")
	}
	if root.Resolve("etc/added.txt") == nil {
		t.Fatalf("file added by the top layer is missing")
	}

	// The topmost layer must win for a path defined by several layers.
	passwd := root.Resolve("etc/passwd")
	if passwd == nil {
		t.Fatal("etc/passwd missing")
	}
	if passwd.Layer != "mid.tar" {
		t.Fatalf("etc/passwd defined by %s, want mid.tar", passwd.Layer)
	}
}

func TestOrderedImageCopyResolvesDeferredHardlink(t *testing.T) {
	physical, manifestOrder, layers := orderedLayers()
	imagePath := writeTestImageOrdered(t, physical, manifestOrder, layers)
	meta := scanTestMeta(t, imagePath)

	dest := t.TempDir()
	if _, err := ExtractPath(imagePath, meta, "/usr/bin", dest, 32*1024); err != nil {
		t.Fatalf("extract /usr/bin: %v", err)
	}

	// cp semantics: a directory source copied into an existing directory
	// lands under dest/<basename>.
	tool := filepath.Join(dest, "bin", "tool")
	link := filepath.Join(dest, "bin", "tool-link")

	toolInfo, err := os.Stat(tool)
	if err != nil {
		t.Fatalf("stat tool: %v", err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatalf("stat tool-link: %v", err)
	}
	if !os.SameFile(toolInfo, linkInfo) {
		t.Fatalf("tool-link is not a hardlink of tool")
	}

	data, err := os.ReadFile(tool)
	if err != nil {
		t.Fatalf("read tool: %v", err)
	}
	if string(data) != "top tool" {
		t.Fatalf("tool content = %q, want the top layer's version", data)
	}
}

func TestOrderedImageExtractMerged(t *testing.T) {
	physical, manifestOrder, layers := orderedLayers()
	imagePath := writeTestImageOrdered(t, physical, manifestOrder, layers)
	meta := scanTestMeta(t, imagePath)

	dest := filepath.Join(t.TempDir(), "rootfs")
	if err := ApplyImage(imagePath, meta, dest, 32*1024, nil); err != nil {
		t.Fatalf("apply image: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "etc", "passwd"))
	if err != nil {
		t.Fatalf("read etc/passwd: %v", err)
	}
	if string(data) != "mid:x:0:0" {
		t.Fatalf("etc/passwd = %q, want the mid layer's version", data)
	}
	if _, err := os.Stat(filepath.Join(dest, "etc", "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("whiteouted path exists after extract (err=%v)", err)
	}
	if _, err := os.ReadFile(filepath.Join(dest, "etc", "added.txt")); err != nil {
		t.Fatalf("added.txt missing: %v", err)
	}
}
