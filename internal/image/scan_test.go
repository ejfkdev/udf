package image

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appi18n "udf/internal/i18n"
	"udf/internal/types"
)

func TestScanImageMetadataSelectsByRepoTag(t *testing.T) {
	imagePath := writeImageTar(t, []types.ManifestItem{
		{
			Config:   "first.json",
			RepoTags: []string{"repo/app:1.0"},
			Layers:   []string{"layer-0.tar"},
		},
		{
			Config:   "second.json",
			RepoTags: []string{"repo/app:latest"},
			Layers:   []string{"layer-1.tar"},
		},
	}, map[string]types.ImageConfig{
		"second.json": {
			Architecture: "amd64",
			Config: struct {
				User         string         `json:"User"`
				Env          []string       `json:"Env"`
				Entrypoint   []string       `json:"Entrypoint"`
				Cmd          []string       `json:"Cmd"`
				WorkingDir   string         `json:"WorkingDir"`
				ExposedPorts map[string]any `json:"ExposedPorts"`
			}{
				WorkingDir: "/app",
			},
		},
	})

	meta, err := ScanImageMetadata(imagePath, Selection{RepoTag: "repo/app:latest"})
	if err != nil {
		t.Fatalf("scan image metadata: %v", err)
	}
	if meta.Index != 1 {
		t.Fatalf("unexpected image index: %d", meta.Index)
	}
	if got := strings.Join(meta.LayerOrder, ","); got != "layer-1.tar" {
		t.Fatalf("unexpected layers: %s", got)
	}
	if meta.Config == nil || meta.Config.Config.WorkingDir != "/app" {
		t.Fatalf("unexpected config: %+v", meta.Config)
	}
}

func TestScanImageMetadataRequiresSelectionForMultipleImages(t *testing.T) {
	imagePath := writeImageTar(t, []types.ManifestItem{
		{Config: "first.json", RepoTags: []string{"repo/app:1.0"}, Layers: []string{"layer-0.tar"}},
		{Config: "second.json", RepoTags: []string{"repo/app:2.0"}, Layers: []string{"layer-1.tar"}},
	}, map[string]types.ImageConfig{
		"first.json":  {},
		"second.json": {},
	})

	_, err := ScanImageMetadata(imagePath, Selection{ImageIndex: -1})
	if err == nil {
		t.Fatal("expected selection error")
	}
	var le *appi18n.LocalizedError
	if !errors.As(err, &le) || le.Key != "err_multiple_images_require_selection" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeImageTar(t *testing.T, manifest []types.ManifestItem, configs map[string]types.ImageConfig) string {
	t.Helper()

	imagePath := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(imagePath)
	if err != nil {
		t.Fatalf("create image tar: %v", err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)

	for name, cfg := range configs {
		body, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		writeTarFile(t, tw, name, body)
	}

	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeTarFile(t, tw, "manifest.json", manifestBody)

	for _, item := range manifest {
		for _, layerName := range item.Layers {
			writeTarFile(t, tw, layerName, buildEmptyLayer(t))
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close image tar writer: %v", err)
	}

	return imagePath
}

func writeTarFile(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write tar header %s: %v", name, err)
	}
	if len(body) > 0 {
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write tar body %s: %v", name, err)
		}
	}
}

func buildEmptyLayer(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.Close(); err != nil {
		t.Fatalf("close empty layer tar: %v", err)
	}
	return buf.Bytes()
}
