package image

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// ociBlob writes a blob into a layout and returns its digest.
func ociBlob(t *testing.T, dir string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, hexSum), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + hexSum
}

// layerTar builds an uncompressed layer holding one file.
func layerTar(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeOCIImage builds an image manifest + config pair for one platform, with
// the layer compressed by the given media type, and returns the manifest bytes
// with its digest.
func writeOCIImage(t *testing.T, dir, platform, arch, variant, layerName, layerBody, layerMediaType string) ([]byte, string) {
	t.Helper()
	raw := layerTar(t, layerName, layerBody)
	diffID := sha256.Sum256(raw)

	var stored []byte
	switch layerMediaType {
	case "zstd":
		zw, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		stored = zw.EncodeAll(raw, nil)
	default:
		stored = raw
	}
	layerDigest := ociBlob(t, dir, stored)

	config := []byte(`{"architecture":"` + arch + `","os":"` + platform + `"` +
		variantField(variant) + `,"rootfs":{"type":"layers","diff_ids":["sha256:` + hex.EncodeToString(diffID[:]) + `"]}}`)
	configDigest := ociBlob(t, dir, config)

	mediaType := "application/vnd.oci.image.layer.v1.tar"
	switch layerMediaType {
	case "zstd":
		mediaType = "application/vnd.oci.image.layer.v1.tar+zstd"
	case "gzip":
		mediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
	}
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": mediaType, "digest": layerDigest, "size": len(stored)}},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := ociBlob(t, dir, manifestJSON)
	return manifestJSON, manifestDigest
}

func variantField(variant string) string {
	if variant == "" {
		return ""
	}
	return `,"variant":"` + variant + `"`
}

// writeOCILayout writes an index.json naming the given manifests, each with a
// ref-name annotation so it carries a tag.
func writeOCILayout(t *testing.T, dir string, manifests []map[string]any) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     manifests,
	}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A multi-platform OCI layout lists every image, and --platform picks one.
func TestOCIMultiPlatformLayout(t *testing.T) {
	dir := t.TempDir()
	descriptors := []map[string]any{}
	for _, p := range []struct {
		os, arch, variant, layer, body string
	}{
		{"linux", "amd64", "", "etc/amd64.txt", "amd64\n"},
		{"linux", "arm64", "v8", "etc/arm64.txt", "arm64\n"},
	} {
		_, digest := writeOCIImage(t, dir, p.os, p.arch, p.variant, p.layer, p.body, "none")
		descriptors = append(descriptors, map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    digest,
			"size":      1,
			"platform":  map[string]any{"os": p.os, "architecture": p.arch, "variant": p.variant},
			"annotations": map[string]any{
				"org.opencontainers.image.ref.name": p.arch,
			},
		})
	}
	writeOCILayout(t, dir, descriptors)

	summaries, err := ScanImageSummaries(dir)
	if err != nil {
		t.Fatalf("summaries: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("a two-platform layout listed %d images: %+v", len(summaries), summaries)
	}
	platforms := map[string]string{}
	for _, s := range summaries {
		platforms[s.OS+"/"+s.Architecture+"/"+s.Variant] = s.Image
	}
	if _, ok := platforms["linux/amd64/"]; !ok {
		t.Fatalf("amd64 image missing: %+v", platforms)
	}
	if _, ok := platforms["linux/arm64/v8"]; !ok {
		t.Fatalf("arm64 image missing: %+v", platforms)
	}

	// Selection by platform, and by tag + platform.
	for _, tc := range []struct {
		sel  Selection
		want string
	}{
		{Selection{ImageIndex: -1, Platform: "linux/amd64"}, "amd64"},
		{Selection{ImageIndex: -1, Platform: "linux/arm64"}, "arm64"},    // variant omitted
		{Selection{ImageIndex: -1, Platform: "linux/arm64/v8"}, "arm64"}, // variant given
		{Selection{ImageIndex: -1, Platform: "linux/s390x"}, ""},         // no match
	} {
		meta, err := ScanImageMetadata(dir, tc.sel)
		if tc.want == "" {
			if err == nil {
				t.Fatalf("platform %q should not have matched", tc.sel.Platform)
			}
			continue
		}
		if err != nil {
			t.Fatalf("platform %q: %v", tc.sel.Platform, err)
		}
		if meta.Config.Architecture != tc.want {
			t.Fatalf("platform %q picked %s", tc.sel.Platform, meta.Config.Architecture)
		}
		if meta.ManifestDigest == "" {
			t.Fatalf("platform %q: manifest digest not recorded", tc.sel.Platform)
		}
	}

	// Each image lists its own file, so the platform picked the right layers.
	meta, err := ScanImageMetadata(dir, Selection{ImageIndex: -1, Platform: "linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := BuildFileSystem(dir, meta)
	if err != nil {
		t.Fatalf("build filesystem: %v", err)
	}
	entries, err := ListEntries(tree, "/etc")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "arm64.txt" {
		t.Fatalf("arm64 image listed %+v", entries)
	}
}

// A zstd-compressed layer — what containerd writes with --compression zstd —
// lists and extracts like any other.
func TestOCIzstdLayer(t *testing.T) {
	dir := t.TempDir()
	_, digest := writeOCIImage(t, dir, "linux", "amd64", "", "etc/passwd", "root:x:0:0\n", "zstd")
	writeOCILayout(t, dir, []map[string]any{{
		"mediaType":   "application/vnd.oci.image.manifest.v1+json",
		"digest":      digest,
		"size":        1,
		"annotations": map[string]any{"org.opencontainers.image.ref.name": "zstd"},
	}})

	meta, err := ScanImageMetadata(dir, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.LayerMediaTypes) != 1 || meta.LayerMediaTypes[0] != "application/vnd.oci.image.layer.v1.tar+zstd" {
		t.Fatalf("layer media types: %v", meta.LayerMediaTypes)
	}
	tree, err := BuildFileSystem(dir, meta)
	if err != nil {
		t.Fatalf("build filesystem (zstd layer): %v", err)
	}
	entries, err := ListEntries(tree, "/etc")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "passwd" {
		t.Fatalf("zstd layer listed %+v", entries)
	}
	dest := filepath.Join(t.TempDir(), "passwd")
	if _, err := ExtractPath(dir, meta, "/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract from a zstd layer: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "root:x:0:0\n" {
		t.Fatalf("passwd = %q", got)
	}

	// verify recomputes the uncompressed digest for a zstd layer too.
	results, err := VerifyImages(dir, Selection{ImageIndex: -1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Failed != 0 || results[0].LayerCount != 1 {
		t.Fatalf("verify on a zstd layer: %+v", results[0])
	}
}

// A layer that the manifest calls non-distributable is reported as such, not
// as a broken archive.
func TestOCINonDistributableLayer(t *testing.T) {
	dir := t.TempDir()
	raw := layerTar(t, "etc/foreign.txt", "foreign\n")
	diffID := sha256.Sum256(raw)
	// The blob is deliberately absent, the way a foreign layer is.
	absentDigest := "sha256:" + hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32))
	config := []byte(`{"architecture":"windows","os":"windows","rootfs":{"type":"layers","diff_ids":["sha256:` + hex.EncodeToString(diffID[:]) + `"]}}`)
	configDigest := ociBlob(t, dir, config)
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
			"digest":    absentDigest,
			"size":      1,
		}},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := ociBlob(t, dir, manifestJSON)
	writeOCILayout(t, dir, []map[string]any{{
		"mediaType":   "application/vnd.oci.image.manifest.v1+json",
		"digest":      manifestDigest,
		"size":        1,
		"annotations": map[string]any{"org.opencontainers.image.ref.name": "windows"},
	}})

	meta, err := ScanImageMetadata(dir, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !meta.NonDistributableLayer(0) {
		t.Fatalf("layer media types: %v", meta.LayerMediaTypes)
	}
	if _, err := BuildFileSystem(dir, meta); err == nil {
		t.Fatal("a missing foreign layer should fail, with an explanation")
	} else if !bytes.Contains([]byte(err.Error()), []byte("non-distributable")) {
		t.Fatalf("error does not explain the foreign layer: %v", err)
	}

	// verify reports it as skipped, not as a failure.
	results, err := VerifyImages(dir, Selection{ImageIndex: -1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Skipped != 1 || results[0].Failed != 0 {
		t.Fatalf("verify on a foreign layer: %+v", results[0])
	}
}
