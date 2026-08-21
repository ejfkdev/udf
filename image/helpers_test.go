package image

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/ejfkdev/udf/types"
)

type layerTarEntry struct {
	Header *tar.Header
	Body   []byte
}

// writeTestImage archives manifest.json, config.json and the given layer
// tars (keyed by layer entry name) into an uncompressed image tar.
func writeTestImage(t *testing.T, layers map[string][]layerTarEntry) string {
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

	item := types.ManifestItem{
		Config:   "config.json",
		RepoTags: []string{"repo/app:latest"},
	}
	var names []string
	for name := range layers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		item.Layers = append(item.Layers, name)
		writeTarFile(t, tw, name, buildLayerBytesForTest(t, layers[name]))
	}

	manifestBody, err := json.Marshal([]types.ManifestItem{item})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeTarFile(t, tw, "manifest.json", manifestBody)

	if err := tw.Close(); err != nil {
		t.Fatalf("close image tar writer: %v", err)
	}
	return imagePath
}

func buildLayerBytesForTest(t *testing.T, entries []layerTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.Header); err != nil {
			t.Fatalf("write layer header %s: %v", entry.Header.Name, err)
		}
		if len(entry.Body) > 0 {
			if _, err := tw.Write(entry.Body); err != nil {
				t.Fatalf("write layer body %s: %v", entry.Header.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close layer tar writer: %v", err)
	}
	return buf.Bytes()
}

func imgFile(name, body string) layerTarEntry {
	return layerTarEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			ModTime:  time.Unix(1600000000, 0),
			Uid:      0,
			Gid:      0,
			Uname:    "root",
			Gname:    "root",
			Typeflag: tar.TypeReg,
		},
		Body: []byte(body),
	}
}

func imgDir(name string) layerTarEntry {
	return layerTarEntry{
		Header: &tar.Header{
			Name:     name + "/",
			Mode:     0o755,
			ModTime:  time.Unix(1600000000, 0),
			Typeflag: tar.TypeDir,
		},
	}
}

func imgWhiteout(name string) layerTarEntry {
	return layerTarEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o000,
			Size:     0,
			Typeflag: tar.TypeReg,
		},
	}
}

func imgSymlink(name, target string) layerTarEntry {
	return layerTarEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o777,
			ModTime:  time.Unix(1600000000, 0),
			Linkname: target,
			Typeflag: tar.TypeSymlink,
		},
	}
}

func imgHardlink(name, target string) layerTarEntry {
	return layerTarEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o644,
			ModTime:  time.Unix(1600000000, 0),
			Linkname: target,
			Typeflag: tar.TypeLink,
		},
	}
}

func scanTestMeta(t *testing.T, imagePath string) *types.ImageMetadata {
	t.Helper()
	meta, err := ScanImageMetadata(imagePath, Selection{})
	if err != nil {
		t.Fatalf("scan image metadata: %v", err)
	}
	return meta
}
