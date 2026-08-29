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
)

func ociDigestHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// createOCIArchiveTar builds a single-file OCI image archive (the oci-archive
// transport produced by `podman save --format oci` / `skopeo oci-archive:`):
// a tar holding oci-layout, an OCI image index, and the config/manifest/layer
// blobs under blobs/sha256/.
func createOCIArchiveTar(t *testing.T) string {
	t.Helper()

	layer := buildOCICompatLayer(t, map[string]string{
		"etc/passwd":   "root:x:0:0\n",
		"usr/bin/tool": "#!/bin/sh\n",
	})
	config := []byte(`{"architecture":"amd64","config":{"WorkingDir":"/app","Entrypoint":["/bin/sh"]}}`)

	manifestJSON, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + ociDigestHex(config),
			"size":      len(config),
		},
		"layers": []any{
			map[string]any{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    "sha256:" + ociDigestHex(layer),
				"size":      len(layer),
			},
		},
	})

	indexJSON, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{
			map[string]any{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    "sha256:" + ociDigestHex(manifestJSON),
				"size":      len(manifestJSON),
				"annotations": map[string]any{
					"org.opencontainers.image.ref.name": "example/app:latest",
				},
			},
		},
	})

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	write("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	write("index.json", indexJSON)
	write("blobs/sha256/"+ociDigestHex(config), config)
	write("blobs/sha256/"+ociDigestHex(manifestJSON), manifestJSON)
	write("blobs/sha256/"+ociDigestHex(layer), layer)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOCIArchiveScanAndExtract lists and extracts from a single-file OCI image
// archive through the image pipeline (ClassifyInput → ScanImageMetadata →
// BuildFileSystem → ExtractPath).
func TestOCIArchiveScanAndExtract(t *testing.T) {
	p := createOCIArchiveTar(t)

	if got := ClassifyInput(p); got != "image" {
		t.Fatalf("ClassifyInput = %q, want image", got)
	}

	meta, err := ScanImageMetadata(p, Selection{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if meta.Config == nil || meta.Config.Config.WorkingDir != "/app" {
		t.Fatalf("config = %+v", meta.Config)
	}
	if len(meta.LayerOrder) != 1 {
		t.Fatalf("layers = %d, want 1", len(meta.LayerOrder))
	}

	tree, err := BuildFileSystem(p, meta)
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
	if _, err := ExtractPath(p, meta, "/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract passwd: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected passwd %q err=%v", data, err)
	}
}
