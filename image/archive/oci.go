package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
// Every image the index (and any manifest list inside it) names becomes one
// entry, so an OCI layout with several platforms or tags lists the way a
// docker-save archive does, and --tag/--index/--platform select among them.
func openOCIAgainst(store ociStore) (Archive, error) {
	index, err := store.readJSON("index.json")
	if err != nil {
		return nil, err
	}

	// A single-image layout has index.json itself as the manifest; its digest
	// is over those bytes, not over a re-marshalled copy.
	topDigest := ""
	if !types.IsIndexMediaType(manifestValueString(index, "", "mediaType")) {
		if raw, err := store.readBytes("index.json"); err == nil {
			sum := sha256.Sum256(raw)
			topDigest = "sha256:" + hex.EncodeToString(sum[:])
		}
	}
	items, err := ociManifestItems(store, index, manifestAnnotationsTag(index), topDigest, 0, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("OCI index names no image manifests")
	}
	manifestJSON, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return &ociArchive{store: store, manifestJSON: manifestJSON}, nil
}

// ociManifestItems walks an index or a manifest list, collecting one item per
// image manifest. Nested indexes (a manifest list referenced from index.json)
// are followed, with a depth bound and a seen set so a malformed layout cannot
// loop.
func ociManifestItems(store ociStore, node map[string]any, tag, digest string, depth int, seen map[string]bool) ([]types.ManifestItem, error) {
	if depth > 4 {
		return nil, fmt.Errorf("OCI index nesting is too deep")
	}
	mediaType := manifestValueString(node, "", "mediaType")

	// An index or manifest list: walk its manifests.
	if mediaType == types.MediaTypeOCIIndex || mediaType == types.MediaTypeDockerManifestList {
		manifests, _ := node["manifests"].([]any)
		var out []types.ManifestItem
		for _, entry := range manifests {
			desc, _ := entry.(map[string]any)
			if desc == nil {
				continue
			}
			digest := manifestValueString(desc, "", "digest")
			if digest == "" || seen[digest] {
				continue
			}
			seen[digest] = true
			child, err := store.readJSON(ociBlobPath(digest))
			if err != nil {
				continue
			}
			childTag := manifestAnnotationsTag(desc)
			if childTag == "<untagged>" {
				childTag = tag
			}
			items, err := ociManifestItems(store, child, childTag, digest, depth+1, seen)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		return out, nil
	}

	// An image manifest: one item, with the layer media types and the digest of
	// the manifest itself (sha256 over its raw bytes).
	configDigest := manifestValueString(node, "config", "digest")
	layers, _ := node["layers"].([]any)
	layerDigests := make([]string, 0, len(layers))
	layerTypes := make([]string, 0, len(layers))
	for _, entry := range layers {
		layer, _ := entry.(map[string]any)
		if layer == nil {
			continue
		}
		layerDigests = append(layerDigests, manifestValueString(layer, "", "digest"))
		layerTypes = append(layerTypes, manifestValueString(layer, "", "mediaType"))
	}
	if configDigest == "" && len(layerDigests) == 0 {
		// Not an image manifest at all (a bare descriptor, say): nothing to add.
		return nil, nil
	}
	return []types.ManifestItem{{
		Config:          configDigest,
		RepoTags:        []string{tag},
		Layers:          layerDigests,
		LayerMediaTypes: layerTypes,
		ManifestDigest:  digest,
	}}, nil
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
