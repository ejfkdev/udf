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

// writeDockerSaveFixture builds a minimal docker-save archive: a config, one
// layer tar, and a manifest.json. corrupt selects what to break.
func writeDockerSaveFixture(t *testing.T, corrupt string) string {
	t.Helper()
	body := []byte("hello from a layer\n")
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	if err := tw.WriteHeader(&tar.Header{Name: "etc/hello.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	diffID := sha256.Sum256(layer.Bytes())
	diffHex := hex.EncodeToString(diffID[:])
	diffField := diffHex
	if corrupt == "layer" {
		diffField = hex.EncodeToString(bytes.Repeat([]byte{0xAB}, 32))
	}
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:` + diffField + `"]}}`)
	// The name is the digest of the config as written; breaking the config
	// means storing bytes that do not hash to it.
	configSum := sha256.Sum256(config)
	configHex := hex.EncodeToString(configSum[:])
	stored := config
	if corrupt == "config" {
		stored = append(append([]byte{}, config...), '\n')
	}

	layerName := diffHex + "/layer.tar"
	manifest := []any{map[string]any{
		"Config":   configHex + ".json",
		"RepoTags": []string{"example/app:1.0"},
		"Layers":   []string{layerName},
	}}
	manifestJSON, _ := json.Marshal(manifest)

	path := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw = tar.NewWriter(f)
	write := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", manifestJSON)
	write(configHex+".json", stored)
	write(layerName, layer.Bytes())
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyDockerSave(t *testing.T) {
	path := writeDockerSaveFixture(t, "")
	results, err := VerifyImages(path, Selection{ImageIndex: -1}, false)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results", len(results))
	}
	r := results[0]
	if r.Failed != 0 || r.Config != "ok" || r.Verified != 2 {
		t.Fatalf("clean archive should verify: %+v", r)
	}
	if r.Platform != "linux/amd64" {
		t.Fatalf("platform = %q", r.Platform)
	}
	if VerifyFailures(results) != 0 {
		t.Fatal("a clean archive reported failures")
	}

	// A layer whose digest does not match the config's diff_id is caught.
	path = writeDockerSaveFixture(t, "layer")
	results, err = VerifyImages(path, Selection{ImageIndex: -1}, false)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	r = results[0]
	if r.Failed == 0 || VerifyFailures(results) != 1 {
		t.Fatalf("a mismatched layer was not caught: %+v", r)
	}
	if len(r.Checks) != 1 || r.Checks[0].Kind != "layer" || r.Checks[0].Status != "mismatch" {
		t.Fatalf("unexpected checks: %+v", r.Checks)
	}

	// A config whose bytes do not hash to its name is caught too.
	path = writeDockerSaveFixture(t, "config")
	results, err = VerifyImages(path, Selection{ImageIndex: -1}, false)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if results[0].Config != "mismatch" || results[0].Failed == 0 {
		t.Fatalf("a mismatched config was not caught: %+v", results[0])
	}

	// --fast only checks presence, so the mismatched layer passes there.
	path = writeDockerSaveFixture(t, "layer")
	results, err = VerifyImages(path, Selection{ImageIndex: -1}, true)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Failed != 0 {
		t.Fatalf("--fast should not compare digests: %+v", results[0])
	}
}

// A gzip-wrapped layer inside an OCI-style archive verifies through the same
// path, which is also what catches a truncated blob.
func TestVerifyDetectsTruncatedLayer(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	payload := bytes.Repeat([]byte("data"), 4096)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := gz.Bytes()
	// Truncate the compressed layer: decompression must fail, and that is a
	// failed check rather than a silent pass.
	truncated := compressed[:len(compressed)-64]
	blobDigest := sha256.Sum256(truncated)

	diffID := sha256.Sum256(raw.Bytes())
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:` + hex.EncodeToString(diffID[:]) + `"]}}`)
	configSum := sha256.Sum256(config)
	blobHex := hex.EncodeToString(blobDigest[:])

	path := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw = tar.NewWriter(f)
	write := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	manifest := []any{map[string]any{
		"Config":   "blobs/sha256/" + hex.EncodeToString(configSum[:]),
		"RepoTags": []string{"example/truncated:1.0"},
		"Layers":   []string{"blobs/sha256/" + blobHex},
	}}
	manifestJSON, _ := json.Marshal(manifest)
	write("manifest.json", manifestJSON)
	write("blobs/sha256/"+hex.EncodeToString(configSum[:]), config)
	write("blobs/sha256/"+blobHex, truncated)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	results, err := VerifyImages(path, Selection{ImageIndex: -1}, false)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if results[0].Failed == 0 {
		t.Fatalf("a truncated layer verified clean: %+v", results[0])
	}
	if len(results[0].Checks) == 0 || results[0].Checks[0].Status != "unreadable" {
		t.Fatalf("expected an unreadable-layer check, got %+v", results[0].Checks)
	}
}
