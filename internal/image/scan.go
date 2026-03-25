package image

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	appi18n "udf/internal/i18n"
	"udf/internal/types"
)

type Selection struct {
	ImageIndex int
	RepoTag    string
}

func ScanImageMetadata(imageTarPath string, sel Selection) (*types.ImageMetadata, error) {
	archive, err := openArchive(imageTarPath)
	if err != nil {
		return nil, err
	}

	var manifest []types.ManifestItem
	var manifestLoaded bool
	data, err := readEntry(archive, "manifest.json")
	if err == nil {
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("parse manifest.json: %w", err)
		}
		manifestLoaded = true
	}

	if !manifestLoaded {
		return nil, fmt.Errorf("manifest.json not found")
	}

	imageIndex, err := resolveSelection(manifest, sel)
	if err != nil {
		return nil, err
	}

	item := manifest[imageIndex]
	meta := &types.ImageMetadata{
		Index:      imageIndex,
		Total:      len(manifest),
		RepoTags:   append([]string(nil), item.RepoTags...),
		ConfigPath: item.Config,
		LayerOrder: append([]string(nil), item.Layers...),
	}

	configBytes, err := readNamedEntry(imageTarPath, item.Config)
	if err != nil {
		return nil, err
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
	archive, err := openArchive(imageTarPath)
	if err != nil {
		return nil, err
	}
	return readEntry(archive, targetName)
}

func readEntry(archive imageArchive, targetName string) ([]byte, error) {
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
