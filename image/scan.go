package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	appi18n "github.com/ejfkdev/udf/i18n"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/types"
)

type Selection struct {
	ImageIndex int
	RepoTag    string
	// Platform selects by "os/arch[/variant]" — the spelling docker and OCI
	// use — among the images the archive holds. Combining it with a tag
	// narrows that tag's images; combining it with an index is refused.
	Platform string
}

func ScanImageMetadata(imageTarPath string, sel Selection) (*types.ImageMetadata, error) {
	key := cacheKeyFor(imageTarPath, "imgmeta", fmt.Sprintf("%d\x00%s\x00%s", sel.ImageIndex, sel.RepoTag, sel.Platform))
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

// errPlatformNeedsPath is internal: resolveSelection has no archive path and
// so cannot look up per-image platforms; callers that have one resolve a
// platform selection first (see resolvePlatformSelection).
var errPlatformNeedsPath = errors.New("platform selection needs the archive path")

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
		meta, err := scanImageMetadataIndexed(imageTarPath, r, sel)
		if err == nil || !errors.Is(err, errIndexRead) {
			return meta, err
		}
	}
	return scanImageMetadataSequential(imageTarPath, sel)
}

// scanImageMetadataIndexed reads manifest.json and the selected config from an
// index-backed reader.
func scanImageMetadataIndexed(imageTarPath string, r indexedOuter, sel Selection) (*types.ImageMetadata, error) {
	manifestBytes, err := readIndexedEntry(r, "manifest.json")
	if err != nil {
		return nil, err
	}
	if manifestBytes == nil {
		return nil, fmt.Errorf("manifest.json not found")
	}
	manifest, imageIndex, err := parseManifest(manifestBytes, sel, imageTarPath)
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
	meta, err := buildImageMetadata(manifest, imageIndex, item, configBytes)
	if err != nil {
		return nil, err
	}
	if size, ok := storedSizeOf(imageTarPath, item.Layers); ok {
		meta.StoredSize = size
	}
	return meta, nil
}

// readIndexedEntry returns the bytes of one tar member from an index. A nil
// slice with a nil error means the index has no such member; a non-nil error
// means the member exists but could not be read, and the caller should fall
// back to reading the archive.
func readIndexedEntry(r indexedOuter, name string) ([]byte, error) {
	if e, ok := r.Lookup(name); ok {
		data, err := readOuterRange(r, e.OutOff, e.Size)
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
	manifest, imageIndex, err := parseManifest(data, sel, imageTarPath)
	if err != nil {
		return nil, err
	}
	item := manifest[imageIndex]

	configBytes, err := readNamedEntry(imageTarPath, item.Config)
	if err != nil {
		return nil, err
	}
	meta, err := buildImageMetadata(manifest, imageIndex, item, configBytes)
	if err != nil {
		return nil, err
	}
	if size, ok := storedSizeOf(imageTarPath, item.Layers); ok {
		meta.StoredSize = size
	}
	return meta, nil
}

// parseManifest decodes manifest.json and resolves the requested image. A
// --platform selection is resolved first, because it needs the archive path (to
// read the configs) rather than the manifest alone.
func parseManifest(data []byte, sel Selection, imageTarPath string) ([]types.ManifestItem, int, error) {
	// A Docker schema 1 manifest lists fsLayers instead of layers with digests,
	// and its layers carry no diff ids to verify: say so plainly rather than
	// listing an image with no layers.
	if bytes.Contains(data, []byte("\"fsLayers\"")) && !bytes.Contains(data, []byte("\"Layers\"")) {
		return nil, 0, fmt.Errorf("this is a Docker schema 1 image, which is not supported: re-save it with a current docker (docker save --format oci) or podman")
	}
	var manifest []types.ManifestItem
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, 0, fmt.Errorf("parse manifest.json: %w", err)
	}
	if sel.Platform != "" {
		imageIndex, err := resolvePlatformSelection(imageTarPath, manifest, sel)
		if err != nil {
			return nil, 0, err
		}
		return manifest, imageIndex, nil
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
		Index:           imageIndex,
		Total:           len(manifest),
		RepoTags:        append([]string(nil), item.RepoTags...),
		ConfigPath:      item.Config,
		LayerOrder:      append([]string(nil), item.Layers...),
		LayerMediaTypes: append([]string(nil), item.LayerMediaTypes...),
		ManifestDigest:  item.ManifestDigest,
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

	if sel.Platform != "" {
		return 0, errPlatformNeedsPath
	}

	if sel.RepoTag != "" {
		return matchImageTag(manifest, sel.RepoTag)
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

// resolvePlatformSelection maps a --platform value to an image index, using the
// per-image platform the configs record. A tag may accompany it (narrowing the
// tag's images); an index may not.
func resolvePlatformSelection(imageTarPath string, manifest []types.ManifestItem, sel Selection) (int, error) {
	if sel.ImageIndex >= 0 {
		return 0, appi18n.NewError("err_platform_with_index", map[string]any{"Platform": sel.Platform}, nil)
	}
	summaries, err := ScanImageSummaries(imageTarPath)
	if err != nil {
		return 0, err
	}
	idx, err := matchPlatform(summaries, sel.Platform)
	if err != nil {
		return 0, err
	}
	if sel.RepoTag != "" {
		// The tag must name the image the platform picked.
		matched := false
		if idx < len(manifest) {
			for _, form := range []func(string) []string{
				func(tag string) []string { return []string{tag} },
				func(tag string) []string { return pathSuffixes(tag) },
				func(tag string) []string { return pathSuffixes(repoNamePath(tag)) },
				func(tag string) []string { return []string{repoTagPart(tag)} },
			} {
				if matchesAny(manifest[idx].RepoTags, sel.RepoTag, form) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return 0, appi18n.NewError("err_platform_tag_mismatch", map[string]any{
				"Platform":  sel.Platform,
				"Tag":       sel.RepoTag,
				"Available": formatPlatformChoices(summaries),
			}, nil)
		}
	}
	return idx, nil
}

// matchPlatform resolves a "os/arch[/variant]" selection among the archive's
// images. A selection without a variant matches any variant of that
// os/architecture; several matches are refused with the candidates listed, the
// same way an ambiguous tag is.
func matchPlatform(summaries []ImageSummary, want string) (int, error) {
	spec, err := parsePlatform(want)
	if err != nil {
		return 0, err
	}
	var matches []int
	for _, s := range summaries {
		if !strings.EqualFold(s.OS, spec.os) || !strings.EqualFold(s.Architecture, spec.arch) {
			continue
		}
		if spec.variant != "" && !strings.EqualFold(s.Variant, spec.variant) {
			continue
		}
		matches = append(matches, s.Index)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return 0, appi18n.NewError("err_platform_not_found", map[string]any{
			"Platform":  want,
			"Available": formatPlatformChoices(summaries),
		}, nil)
	default:
		return 0, appi18n.NewError("err_platform_ambiguous", map[string]any{
			"Platform":  want,
			"Available": formatPlatformChoices(summaries),
		}, nil)
	}
}

// platformSpec is a parsed --platform value.
type platformSpec struct {
	os, arch, variant string
}

func parsePlatform(want string) (platformSpec, error) {
	fields := strings.Split(strings.TrimSpace(want), "/")
	if len(fields) < 2 || len(fields) > 3 || fields[0] == "" || fields[1] == "" {
		return platformSpec{}, appi18n.NewError("err_platform_invalid", map[string]any{"Platform": want}, nil)
	}
	spec := platformSpec{os: fields[0], arch: fields[1]}
	if len(fields) == 3 {
		spec.variant = fields[2]
	}
	return spec, nil
}

// formatPlatformChoices renders the platform of every image, for error
// messages and listings.
func formatPlatformChoices(summaries []ImageSummary) string {
	out := make([]string, 0, len(summaries))
	for _, s := range summaries {
		p := s.OS
		if p == "" {
			p = "?"
		}
		if s.Architecture != "" {
			p += "/" + s.Architecture
		} else {
			p += "/?"
		}
		if s.Variant != "" {
			p += "/" + s.Variant
		}
		out = append(out, fmt.Sprintf("[%d]=%s", s.Index, p))
	}
	return strings.Join(out, ", ")
}

// matchImageTag resolves the image names a user may type. A full repo tag
// always matches; beyond that the tail of a repo tag ("name:tag"), a
// repository name ("name") and a bare tag ("1.2") are accepted as long as they
// are unambiguous — the answer is never a guess, a clash lists the candidates.
func matchImageTag(manifest []types.ManifestItem, name string) (int, error) {
	want := strings.TrimSpace(name)
	// Each form is looser than the one before; the first that matches decides.
	// A form may name several shapes of the same repo tag (a tag, and every
	// path suffix of it), so "group/app:1.2" and "app:1.2" both work.
	for _, form := range []func(string) []string{
		func(tag string) []string { return []string{tag} },                   // as written
		func(tag string) []string { return pathSuffixes(tag) },               // group/app:1.2, app:1.2
		func(tag string) []string { return pathSuffixes(repoNamePath(tag)) }, // group/app, app
		func(tag string) []string { return []string{repoTagPart(tag)} },      // 1.2
	} {
		var matches []int
		for i, item := range manifest {
			if !matchesAny(item.RepoTags, want, form) {
				continue
			}
			if len(matches) == 0 || matches[len(matches)-1] != i {
				matches = append(matches, i)
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			return matches[0], nil
		default:
			return 0, appi18n.NewError("err_repo_tag_ambiguous", map[string]any{
				"Tag":        want,
				"Candidates": formatMatchingChoices(manifest, matches),
			}, nil)
		}
	}
	return 0, appi18n.NewError("err_repo_tag_not_found", map[string]any{
		"Tag":       want,
		"Available": formatManifestChoices(manifest),
	}, nil)
}

// pathSuffixes lists a repo tag or repository path together with every suffix
// that starts at a path separator, so a user can name the tail of a registry
// path: "reg.example/group/app:1.2" yields itself, "group/app:1.2" and
// "app:1.2".
func pathSuffixes(name string) []string {
	out := []string{name}
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			out = append(out, name[i+1:])
		}
	}
	return out
}

// repoNamePath returns a repo tag without its tag part, keeping any registry
// and group path: "reg.example/group/app:1.2" -> "reg.example/group/app".
func repoNamePath(repoTag string) string {
	if i := strings.LastIndex(repoTag, ":"); i > strings.LastIndex(repoTag, "/") {
		return repoTag[:i]
	}
	return repoTag
}

// matchesAny reports whether any of a repo tag's shapes equals want.
func matchesAny(repoTags []string, want string, form func(string) []string) bool {
	for _, repoTag := range repoTags {
		for _, shape := range form(repoTag) {
			if shape == want && shape != "" {
				return true
			}
		}
	}
	return false
}

// repoTagTail returns a repo tag without its registry and path:
// "reg.example/group/app:1.2" -> "app:1.2".
func repoTagTail(repoTag string) string {
	if i := strings.LastIndex(repoTag, "/"); i >= 0 {
		return repoTag[i+1:]
	}
	return repoTag
}

// repoTagPart returns the tag part of a repo tag, or "" when it has none.
func repoTagPart(repoTag string) string {
	tail := repoTagTail(repoTag)
	i := strings.LastIndex(tail, ":")
	if i < 0 {
		return ""
	}
	return tail[i+1:]
}

// formatMatchingChoices lists the images a loose name matched, so an ambiguous
// one can be resolved by copying a full repo tag.
func formatMatchingChoices(manifest []types.ManifestItem, indexes []int) string {
	choices := make([]string, 0, len(indexes))
	for _, i := range indexes {
		if i < len(manifest) {
			choices = append(choices, manifestChoice(manifest, i))
		}
	}
	return strings.Join(choices, "; ")
}

func formatManifestChoices(manifest []types.ManifestItem) string {
	choices := make([]string, 0, len(manifest))
	for i := range manifest {
		choices = append(choices, manifestChoice(manifest, i))
	}
	return strings.Join(choices, "; ")
}

// manifestChoice renders one image as it is listed in a selection error.
func manifestChoice(manifest []types.ManifestItem, i int) string {
	item := manifest[i]
	label := strings.Join(item.RepoTags, ",")
	if label == "" {
		label = "<untagged>"
	}
	return fmt.Sprintf("[%d]=%s", i, label)
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

// ImageSummary describes one image of a multi-image archive: enough to tell the
// images apart and to pick one with --tag/-t (or --image/-i), without
// reading every image's config.
type ImageSummary struct {
	// Index is the image's position in manifest.json (what -i takes).
	Index int `json:"index"`
	// Image carries the image's repo tags, comma separated, or its config
	// path when it has none (what -t takes).
	Image string `json:"image"`
	// OS, Architecture and Variant mirror the config's "os", "architecture"
	// and "variant"; they are separate fields because a config may name only
	// some of them.
	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Variant      string `json:"variant,omitempty"`
	// Created is the config's "created" timestamp, as written.
	Created string `json:"created,omitempty"`
	// Layers counts the image's layers; Size is their stored size in the
	// archive (empty when the archive's members were not indexed).
	Layers int    `json:"layers"`
	Size   string `json:"size,omitempty"`
}

// ScanImageSummaries lists every image of an archive. The result is cached:
// it needs only manifest.json, which the index makes cheap to read.
func ScanImageSummaries(imageTarPath string) ([]ImageSummary, error) {
	key := cacheKeyFor(imageTarPath, "imgs")
	var cached []ImageSummary
	if loadCachedJSON(key, &cached) {
		return cached, nil
	}
	summaries, err := scanImageSummaries(imageTarPath)
	if err != nil {
		return nil, err
	}
	storeCachedJSON(key, summaries)
	return summaries, nil
}

func scanImageSummaries(imageTarPath string) ([]ImageSummary, error) {
	manifestBytes, err := readManifestBytes(imageTarPath)
	if err != nil {
		return nil, err
	}
	var manifest []types.ManifestItem
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}

	// Layer sizes come from the tar directory when a cached index has one:
	// the size of a layer member is the space it takes in the archive.
	var directory map[string]int64
	if r, ok := loadImageIndex(imageTarPath); ok {
		directory = make(map[string]int64, len(r.Entries()))
		for _, e := range r.Entries() {
			directory[e.Name] = e.Size
		}
		_ = r.Close()
	}

	out := make([]ImageSummary, 0, len(manifest))
	for i, item := range manifest {
		s := ImageSummary{
			Index:   i,
			Layers:  len(item.Layers),
			Created: "",
		}
		switch {
		case len(item.RepoTags) > 0:
			s.Image = strings.Join(item.RepoTags, ", ")
		default:
			s.Image = "<untagged> " + item.Config
		}
		if cfg, err := readImageConfig(imageTarPath, item.Config); err == nil {
			s.OS = cfg.OS
			s.Architecture = cfg.Architecture
			s.Variant = cfg.Variant
			s.Created = cfg.Created
		}
		if directory != nil {
			var total int64
			known := true
			for _, layer := range item.Layers {
				size, ok := directory[layer]
				if !ok {
					known = false
					break
				}
				total += size
			}
			if known {
				s.Size = HumanBytes(total)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// readManifestBytes reads manifest.json through the index when one is
// available, and sequentially otherwise.
func readManifestBytes(imageTarPath string) ([]byte, error) {
	if r, ok := ensureImageIndex(imageTarPath); ok {
		defer r.Close()
		data, err := readIndexedEntry(r, "manifest.json")
		if err == nil && data != nil {
			return data, nil
		}
	}
	data, err := readNamedEntry(imageTarPath, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("manifest.json not found")
	}
	return data, nil
}

// readImageConfig returns one image's parsed config, through the index when
// possible.
func readImageConfig(imageTarPath, name string) (*types.ImageConfig, error) {
	var data []byte
	if r, ok := loadImageIndex(imageTarPath); ok {
		raw, err := readIndexedEntry(r, name)
		_ = r.Close()
		if err == nil {
			data = raw
		}
	}
	if data == nil {
		raw, err := readNamedEntry(imageTarPath, name)
		if err != nil {
			return nil, err
		}
		data = raw
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty config %s", name)
	}
	var cfg types.ImageConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", name, err)
	}
	return &cfg, nil
}

// storedSizeOf sums the archive space the image's layers occupy, as recorded
// by the index directory; it reports false when the directory is unavailable or
// misses a layer.
func storedSizeOf(imageTarPath string, layers []string) (int64, bool) {
	r, ok := loadImageIndex(imageTarPath)
	if !ok {
		return 0, false
	}
	defer r.Close()
	var total int64
	for _, layer := range layers {
		e, ok := r.Lookup(layer)
		if !ok {
			return 0, false
		}
		total += e.Size
	}
	return total, true
}

// HumanBytes renders a byte count the way the listings do.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
