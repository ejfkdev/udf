package archive

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// buildOCIArchiveTar crafts a single-file OCI image archive (the oci-archive
// transport used by `podman save --format oci` and flatpak bundles): a tar
// holding oci-layout, index.json, the config blob, the manifest blob and one
// layer blob.
func buildOCIArchiveTar(t *testing.T) (path, layerDigest string) {
	t.Helper()

	config := []byte(`{"architecture":"amd64","config":{"WorkingDir":"/app"}}`)
	layer := []byte("pretend layer tar.gz content")
	manifestBytes, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + sha256hex(config),
			"size":      len(config),
		},
		"layers": []any{
			map[string]any{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    "sha256:" + sha256hex(layer),
				"size":      len(layer),
			},
		},
	})
	indexBytes, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{
			map[string]any{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    "sha256:" + sha256hex(manifestBytes),
				"size":      len(manifestBytes),
				"annotations": map[string]any{
					"org.opencontainers.image.ref.name": "example/app:latest",
				},
			},
		},
	})
	ociLayout := []byte(`{"imageLayoutVersion":"1.0.0"}`)

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
	write("oci-layout", ociLayout)
	write("index.json", indexBytes)
	write("blobs/sha256/"+sha256hex(config), config)
	write("blobs/sha256/"+sha256hex(manifestBytes), manifestBytes)
	write("blobs/sha256/"+sha256hex(layer), layer)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, "sha256:" + sha256hex(layer)
}

func TestOCIArchiveTar(t *testing.T) {
	path, layerDigest := buildOCIArchiveTar(t)

	// Detection stays "tar"; the OCI routing happens in Open.
	if f, err := Detect(path); err != nil || f != "tar" {
		t.Fatalf("Detect = %q, %v; want tar", f, err)
	}

	ar, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := ar.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "manifest.json" {
		t.Fatalf("List = %+v", entries)
	}

	// The synthesized manifest.json carries the layer + repo tag.
	rc, _, err := ar.Open("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	mb, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Layers   []string `json:"Layers"`
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.Unmarshal(mb, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Layers) != 1 || items[0].Layers[0] != layerDigest {
		t.Fatalf("manifest = %+v", items)
	}
	if len(items[0].RepoTags) != 1 || items[0].RepoTags[0] != "example/app:latest" {
		t.Fatalf("repo tags = %v", items[0].RepoTags)
	}

	// The layer blob is readable by digest.
	rc2, _, err := ar.Open(layerDigest)
	if err != nil {
		t.Fatal(err)
	}
	lb, err := io.ReadAll(rc2)
	rc2.Close()
	if err != nil || string(lb) != "pretend layer tar.gz content" {
		t.Fatalf("layer = %q (err %v)", lb, err)
	}
}

// TestOCIArchiveTarNotMistakenForPlain ensures a plain tar stays a plain tar
// and docker-save detection is unaffected.
func TestOCINotMisdetected(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "readme.txt", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("data"))
	_ = tw.Close()
	p := filepath.Join(t.TempDir(), "plain.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	ar, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ar.Open("manifest.json"); err == nil {
		t.Fatalf("plain tar must not report a manifest.json")
	}
}
