package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ejfkdev/udf/gzipidx"
	appi18n "github.com/ejfkdev/udf/i18n"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/types"
)

type Selection struct {
	ImageIndex int
	RepoTag    string
}

func ScanImageMetadata(imageTarPath string, sel Selection) (*types.ImageMetadata, error) {
	key := cacheKeyFor(imageTarPath, "imgmeta", fmt.Sprintf("%d\x00%s", sel.ImageIndex, sel.RepoTag))
	var cached types.ImageMetadata
	if loadCachedJSON(key, &cached) {
		return &cached, nil
	}

	meta, err := scanImageMetadata(imageTarPath, sel)
	if err != nil {
		return nil, err
	}
	storeCachedJSON(key, meta)
	return meta, nil
}

// errIndexRead marks a failure to read a member through the index — a stale or
// damaged index, or a member the index does not know. Only this warrants
// reading the archive sequentially: any other error (a selection that needs a
// tag, a malformed manifest) would simply be produced again, at the cost of a
// whole decompression.
var errIndexRead = errors.New("index read failed")

func scanImageMetadata(imageTarPath string, sel Selection) (*types.ImageMetadata, error) {
	// On a large gzip tar the manifest usually sits at the end, so reading it
	// through the general-purpose reader decompresses the whole archive — and
	// reading the config after it starts over from the beginning. Building the
	// index reads the archive once and serves both from random access, and
	// every later command benefits too.
	if r, ok := ensureImageIndex(imageTarPath); ok {
		defer r.Close()
		meta, err := scanImageMetadataIndexed(r, sel)
		if err == nil || !errors.Is(err, errIndexRead) {
			return meta, err
		}
	}
	return scanImageMetadataSequential(imageTarPath, sel)
}

// scanImageMetadataIndexed reads manifest.json and the selected config from an
// index-backed reader.
func scanImageMetadataIndexed(r *gzipidx.Reader, sel Selection) (*types.ImageMetadata, error) {
	manifestBytes, err := readIndexedEntry(r, "manifest.json")
	if err != nil {
		return nil, err
	}
	if manifestBytes == nil {
		return nil, fmt.Errorf("manifest.json not found")
	}
	manifest, imageIndex, err := parseManifest(manifestBytes, sel)
	if err != nil {
		return nil, err
	}
	item := manifest[imageIndex]
	configBytes, err := readIndexedEntry(r, item.Config)
	if err != nil {
		return nil, err
	}
	if configBytes == nil {
		return nil, fmt.Errorf("entry %s not found in archive", item.Config)
	}
	return buildImageMetadata(manifest, imageIndex, item, configBytes)
}

// readIndexedEntry returns the bytes of one tar member from an index. A nil
// slice with a nil error means the index has no such member; a non-nil error
// means the member exists but could not be read, and the caller should fall
// back to reading the archive.
func readIndexedEntry(r *gzipidx.Reader, name string) ([]byte, error) {
	for _, e := range r.Index().Entries {
		if e.Name != name {
			continue
		}
		data, err := r.ReadRange(e.OutOff, e.Size)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", errIndexRead, name, err)
		}
		return data, nil
	}
	return nil, nil
}

func scanImageMetadataSequential(imageTarPath string, sel Selection) (*types.ImageMetadata, error) {
	archive, err := arch.Open(imageTarPath)
	if err != nil {
		return nil, err
	}

	data, err := readEntry(archive, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("manifest.json not found")
	}
	manifest, imageIndex, err := parseManifest(data, sel)
	if err != nil {
		return nil, err
	}
	item := manifest[imageIndex]

	configBytes, err := readNamedEntry(imageTarPath, item.Config)
	if err != nil {
		return nil, err
	}
	return buildImageMetadata(manifest, imageIndex, item, configBytes)
}

// parseManifest decodes manifest.json and resolves the requested image.
func parseManifest(data []byte, sel Selection) ([]types.ManifestItem, int, error) {
	var manifest []types.ManifestItem
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, 0, fmt.Errorf("parse manifest.json: %w", err)
	}
	imageIndex, err := resolveSelection(manifest, sel)
	if err != nil {
		return nil, 0, err
	}
	return manifest, imageIndex, nil
}

// buildImageMetadata assembles the metadata from the parsed manifest and the
// selected image's config bytes.
func buildImageMetadata(manifest []types.ManifestItem, imageIndex int, item types.ManifestItem, configBytes []byte) (*types.ImageMetadata, error) {
	meta := &types.ImageMetadata{
		Index:      imageIndex,
		Total:      len(manifest),
		RepoTags:   append([]string(nil), item.RepoTags...),
		ConfigPath: item.Config,
		LayerOrder: append([]string(nil), item.Layers...),
	}
	if len(configBytes) == 0 {
		return meta, nil
	}

	var cfg types.ImageConfig
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", item.Config, err)
	}
	meta.Config = &cfg

	var raw any
	if err := json.Unmarshal(configBytes, &raw); err != nil {
		return nil, fmt.Errorf("parse raw config %s: %w", item.Config, err)
	}
	meta.ConfigRaw = raw

	return meta, nil
}

func resolveSelection(manifest []types.ManifestItem, sel Selection) (int, error) {
	if len(manifest) == 0 {
		return 0, appi18n.NewError("err_manifest_empty", nil, nil)
	}

	if sel.RepoTag != "" {
		for i, item := range manifest {
			for _, repoTag := range item.RepoTags {
				if repoTag == sel.RepoTag {
					return i, nil
				}
			}
		}
		return 0, appi18n.NewError("err_repo_tag_not_found", map[string]any{
			"Tag":       sel.RepoTag,
			"Available": formatManifestChoices(manifest),
		}, nil)
	}

	if sel.ImageIndex >= 0 {
		if sel.ImageIndex >= len(manifest) {
			return 0, appi18n.NewError("err_image_index_out_of_range", map[string]any{
				"Index": sel.ImageIndex,
				"Count": len(manifest),
			}, nil)
		}
		return sel.ImageIndex, nil
	}

	if len(manifest) == 1 {
		return 0, nil
	}

	return 0, appi18n.NewError("err_multiple_images_require_selection", map[string]any{
		"Available": formatManifestChoices(manifest),
	}, nil)
}

func formatManifestChoices(manifest []types.ManifestItem) string {
	choices := make([]string, 0, len(manifest))
	for i, item := range manifest {
		label := strings.Join(item.RepoTags, ",")
		if label == "" {
			label = "<untagged>"
		}
		choices = append(choices, fmt.Sprintf("[%d]=%s", i, label))
	}
	return strings.Join(choices, "; ")
}

func readNamedEntry(imageTarPath, targetName string) ([]byte, error) {
	archive, err := arch.Open(imageTarPath)
	if err != nil {
		return nil, err
	}
	return readEntry(archive, targetName)
}

func readEntry(archive arch.Archive, targetName string) ([]byte, error) {
	rc, _, err := archive.Open(targetName)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", targetName, err)
	}
	return data, nil
}
