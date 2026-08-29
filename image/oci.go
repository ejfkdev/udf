package image

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ejfkdev/udf/types"
)

// IsOCILayout reports whether path is a directory containing an OCI image
// layout (an oci-layout file plus an index.json). An OCI layout is read through
// the same archive pipeline as docker save archives, so it is a directory
// rather than a disk image.
func IsOCILayout(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "oci-layout")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "index.json")); err != nil {
		return false
	}
	return true
}

// ociArchive presents an OCI image layout as an imageArchive: manifest.json is
// synthesized from index.json + the image manifest, and every other entry name
// (the config and layer digests) maps onto a blob under blobs/sha256/.
type ociArchive struct {
	dir          string
	manifestJSON []byte
}

func (a *ociArchive) List() ([]archiveEntry, error) {
	return []archiveEntry{{Name: "manifest.json", Size: int64(len(a.manifestJSON))}}, nil
}

func (a *ociArchive) Open(name string) (io.ReadCloser, int64, error) {
	if name == "manifest.json" {
		return io.NopCloser(bytes.NewReader(a.manifestJSON)), int64(len(a.manifestJSON)), nil
	}
	f, err := openOCIBlobReader(a.dir, name)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// openOCIArchive parses index.json and builds the synthesized manifest.json.
func openOCIArchive(dir string) (imageArchive, error) {
	manifest, repoTag, err := readOCIManifest(dir)
	if err != nil {
		return nil, err
	}
	configDigest := manifestValueString(manifest, "config", "digest")
	layerDigests := manifestValueStrings(manifest, "layers", "digest")

	item := types.ManifestItem{
		Config:   configDigest,
		RepoTags: []string{repoTag},
		Layers:   layerDigests,
	}
	manifestJSON, err := json.Marshal([]types.ManifestItem{item})
	if err != nil {
		return nil, err
	}
	return &ociArchive{dir: dir, manifestJSON: manifestJSON}, nil
}

// readOCIManifest resolves index.json to the image manifest plus its tag.
func readOCIManifest(dir string) (map[string]any, string, error) {
	index, err := readOCIFile(dir, "index.json")
	if err != nil {
		return nil, "", err
	}

	if manifestValueString(index, "mediaType", "") == "application/vnd.oci.image.index.v1+json" {
		manifests, _ := index["manifests"].([]any)
		if len(manifests) == 0 {
			return nil, "", fmt.Errorf("OCI index has no manifests")
		}
		first, _ := manifests[0].(map[string]any)
		digest := manifestValueString(first, "digest", "")
		repoTag := manifestAnnotationsTag(first)
		if manifest, err := readOCIBlobJSON(dir, digest); err == nil {
			return manifest, repoTag, nil
		}
	}

	// index.json is itself the image manifest.
	return index, manifestAnnotationsTag(index), nil
}

func manifestAnnotationsTag(m map[string]any) string {
	annotations, _ := m["annotations"].(map[string]any)
	for _, key := range []string{
		"org.opencontainers.image.ref.name",
		"io.containerd.image.name",
	} {
		if v, ok := annotations[key].(string); ok && v != "" {
			return v
		}
	}
	return "<untagged>"
}

func manifestValueString(m map[string]any, section, field string) string {
	if section == "" {
		v, _ := m[field].(string)
		return v
	}
	sub, _ := m[section].(map[string]any)
	if sub == nil {
		return ""
	}
	v, _ := sub[field].(string)
	return v
}

func manifestValueStrings(m map[string]any, section, field string) []string {
	sub, _ := m[section].([]any)
	var out []string
	for _, item := range sub {
		e, _ := item.(map[string]any)
		if e == nil {
			continue
		}
		if v, _ := e[field].(string); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func openOCIBlobReader(dir, digest string) (*os.File, error) {
	path := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	return os.Open(path)
}

func readOCIBlob(dir, digest string) ([]byte, error) {
	rc, err := openOCIBlobReader(dir, digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func readOCIBlobJSON(dir, digest string) (map[string]any, error) {
	data, err := readOCIBlob(dir, digest)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func readOCIFile(dir, name string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}
