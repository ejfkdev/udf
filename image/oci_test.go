package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOCILayoutScanAndExtract(t *testing.T) {
	dir := createOCILayout(t)

	meta, err := ScanImageMetadata(dir, Selection{})
	if err != nil {
		t.Fatalf("scan oci metadata: %v", err)
	}
	if meta.Config == nil || meta.Config.Config.WorkingDir != "/app" {
		t.Fatalf("unexpected oci config: %+v", meta.Config)
	}
	if len(meta.LayerOrder) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(meta.LayerOrder))
	}

	tree, err := BuildFileSystem(dir, meta)
	if err != nil {
		t.Fatalf("build filesystem: %v", err)
	}
	entries, err := ListEntries(tree, "/etc")
	if err != nil {
		t.Fatalf("list /etc: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["passwd"] {
		t.Fatalf("expected passwd in listing, got %v", names)
	}

	dest := filepath.Join(t.TempDir(), "passwd.out")
	if _, err := ExtractPath(dir, meta, "/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract passwd: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected passwd %q err=%v", data, err)
	}
}

// createOCILayout builds a minimal single-layer OCI image layout directory.
func createOCILayout(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "oci")
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}

	layer := buildOCICompatLayer(t, map[string]string{
		"etc/passwd":   "root:x:0:0\n",
		"usr/bin/tool": "#!/bin/sh\n",
	})
	layerDigest := writeOCIBlob(t, dir, layer)

	config := []byte(`{"architecture":"amd64","config":{"WorkingDir":"/app","Entrypoint":["/bin/sh"]}}`)
	configDigest := writeOCIBlob(t, dir, config)

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + configDigest,
			"size":      len(config),
		},
		"layers": []any{
			map[string]any{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    "sha256:" + layerDigest,
				"size":      len(layer),
			},
		},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), manifestJSON, 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}
	return dir
}

func buildOCICompatLayer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func writeOCIBlob(t *testing.T, dir string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", hexSum), data, 0o644); err != nil {
		t.Fatalf("write blob %s: %v", hexSum, err)
	}
	return hexSum
}
