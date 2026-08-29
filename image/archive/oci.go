package archive

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
// rather than a disk image. The single-file variant (oci-archive, e.g.
// `podman save --format oci` or a flatpak bundle) is a tar handled by openOCITar.
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

// ociStore reads named entries of an OCI image, whether it is backed by a
// directory (oci image layout) or a tar (oci-archive: transport / a flatpak
// bundle). Entry names are "index.json" or "blobs/sha256/<hex>".
type ociStore struct {
	open func(name string) (io.ReadCloser, int64, error)
}

func (s ociStore) readBytes(name string) ([]byte, error) {
	rc, _, err := s.open(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (s ociStore) readJSON(name string) (map[string]any, error) {
	data, err := s.readBytes(name)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ociBlobPath maps a digest ("sha256:…") to its blob entry path.
func ociBlobPath(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

// dirOCIStore adapts an OCI image layout directory.
func dirOCIStore(dir string) ociStore {
	return ociStore{open: func(name string) (io.ReadCloser, int64, error) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, 0, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, st.Size(), nil
	}}
}

// archiveOCIStore adapts an existing Archive (a tar) so the OCI image archive
// format is read through the same code as the directory layout.
func archiveOCIStore(a Archive) ociStore {
	return ociStore{open: a.Open}
}

// ociArchive presents an OCI image (layout or archive) as an Archive:
// manifest.json is synthesized from index.json + the image manifest, and every
// other entry name (the config and layer digests) maps onto a blob.
type ociArchive struct {
	store        ociStore
	manifestJSON []byte
}

func (a *ociArchive) List() ([]Entry, error) {
	return []Entry{{Name: "manifest.json", Size: int64(len(a.manifestJSON))}}, nil
}

func (a *ociArchive) Open(name string) (io.ReadCloser, int64, error) {
	if name == "manifest.json" {
		return io.NopCloser(bytes.NewReader(a.manifestJSON)), int64(len(a.manifestJSON)), nil
	}
	return a.store.open(ociBlobPath(name))
}

// openOCIDir opens an OCI image layout directory.
func openOCIDir(dir string) (Archive, error) {
	return openOCIAgainst(dirOCIStore(dir))
}

// openOCITar opens an OCI image archive — a tar holding oci-layout, index.json
// and blobs/sha256/* — through the same pipeline as the directory layout. This
// covers `podman save --format oci`, `skopeo oci-archive:` and flatpak bundles.
func openOCITar(path, compression string) (Archive, error) {
	ta := &tarArchive{path: path, compression: compression}
	return openOCIAgainst(archiveOCIStore(ta))
}

// openOCIAgainst parses index.json and builds the synthesized manifest.json.
func openOCIAgainst(store ociStore) (Archive, error) {
	manifest, repoTag, err := readOCIManifest(store)
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
	return &ociArchive{store: store, manifestJSON: manifestJSON}, nil
}

// readOCIManifest resolves index.json to the image manifest plus its tag.
func readOCIManifest(store ociStore) (map[string]any, string, error) {
	index, err := store.readJSON("index.json")
	if err != nil {
		return nil, "", err
	}

	if manifestValueString(index, "", "mediaType") == "application/vnd.oci.image.index.v1+json" {
		manifests, _ := index["manifests"].([]any)
		if len(manifests) == 0 {
			return nil, "", fmt.Errorf("OCI index has no manifests")
		}
		first, _ := manifests[0].(map[string]any)
		digest := manifestValueString(first, "", "digest")
		repoTag := manifestAnnotationsTag(first)
		if manifest, err := store.readJSON(ociBlobPath(digest)); err == nil {
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
