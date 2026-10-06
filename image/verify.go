package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/layer"
	"github.com/ejfkdev/udf/types"
)

// VerifyResult is the outcome of checking one image: the digests that could be
// recomputed, and a per-layer report.
type VerifyResult struct {
	Image          string       `json:"image"`
	Index          int          `json:"index"`
	Platform       string       `json:"platform,omitempty"`
	ManifestDigest string       `json:"manifest_digest,omitempty"`
	Manifest       string       `json:"manifest_check,omitempty"` // "ok" | "mismatch" | "unverifiable"
	Config         string       `json:"config_check"`             // "ok" | "mismatch" | "unverifiable"
	LayerCount     int          `json:"layers"`
	Verified       int          `json:"verified"`
	Failed         int          `json:"failed"`
	Skipped        int          `json:"skipped"`
	Checks         []VerifyLine `json:"checks"`
}

// VerifyLine is one checked item: a layer, the config, or the manifest.
type VerifyLine struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // "manifest" | "config" | "layer"
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// VerifyImages checks the digest of every image an archive holds (or of the
// selected one): the config against the digest its name records, each layer
// against the uncompressed digest the config lists, and the manifest when the
// format records its digest. A non-distributable layer is reported as skipped
// rather than failed, because the archive is not supposed to carry it.
func VerifyImages(inputPath string, sel Selection, fast bool) ([]VerifyResult, error) {
	return verifyImages(inputPath, sel, fast)
}

// VerifyFailures counts the images that failed a check, so a caller can turn
// that into an exit status.
func VerifyFailures(results []VerifyResult) int { return verifyFailures(results) }

func verifyImages(inputPath string, sel Selection, fast bool) ([]VerifyResult, error) {
	ar, err := openArchive(inputPath)
	if err != nil {
		return nil, err
	}
	defer ar.close()

	manifest, err := readManifestItems(inputPath)
	if err != nil {
		return nil, err
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("archive holds no images")
	}

	indexes := []int{}
	switch {
	case sel.Platform != "" || sel.RepoTag != "" || sel.ImageIndex >= 0:
		idx, err := resolveForVerify(inputPath, manifest, sel)
		if err != nil {
			return nil, err
		}
		indexes = append(indexes, idx)
	default:
		for i := range manifest {
			indexes = append(indexes, i)
		}
	}

	out := make([]VerifyResult, 0, len(indexes))
	for _, idx := range indexes {
		res, err := verifyOne(inputPath, ar, manifest, idx, fast)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

func resolveForVerify(inputPath string, manifest []types.ManifestItem, sel Selection) (int, error) {
	if sel.Platform != "" {
		return resolvePlatformSelection(inputPath, manifest, sel)
	}
	return resolveSelection(manifest, sel)
}

func verifyOne(inputPath string, ar *openedArchive, manifest []types.ManifestItem, idx int, fast bool) (VerifyResult, error) {
	item := manifest[idx]
	res := VerifyResult{
		Index:          idx,
		ManifestDigest: item.ManifestDigest,
		LayerCount:     len(item.Layers),
	}
	res.Image = strings.Join(item.RepoTags, ", ")
	if res.Image == "" {
		res.Image = item.Config
	}

	// The config's digest is its own name when that name looks like a digest.
	configData, err := ar.readAll(item.Config)
	if err != nil {
		return res, fmt.Errorf("read config %s: %w", item.Config, err)
	}
	wantConfig := digestFromName(item.Config)
	if wantConfig == "" {
		res.Config = "unverifiable"
		res.Checks = append(res.Checks, VerifyLine{Name: item.Config, Kind: "config", Status: "unverifiable",
			Detail: "the archive records no digest for the config"})
	} else if got := sha256Hex(configData); got != wantConfig {
		res.Config = "mismatch"
		res.Failed++
		res.Checks = append(res.Checks, VerifyLine{Name: item.Config, Kind: "config", Status: "mismatch",
			Detail: fmt.Sprintf("sha256 is %s, the name says %s", shortDigest(got), shortDigest(wantConfig))})
	} else {
		res.Config = "ok"
		res.Verified++
	}

	var cfg types.ImageConfig
	if err := json.Unmarshal(configData, &cfg); err == nil {
		res.Platform = cfg.Platform()
	}

	// The manifest's digest, when the format records one (OCI).
	if item.ManifestDigest != "" {
		if raw, err := ar.readAll(item.ManifestDigest); err == nil {
			if got := sha256Hex(raw); got != strings.TrimPrefix(item.ManifestDigest, "sha256:") {
				res.Manifest = "mismatch"
				res.Failed++
				res.Checks = append(res.Checks, VerifyLine{Name: item.ManifestDigest, Kind: "manifest", Status: "mismatch",
					Detail: fmt.Sprintf("sha256 is %s", shortDigest(got))})
			} else {
				res.Manifest = "ok"
				res.Verified++
			}
		}
	}

	// Layers: the uncompressed digest the config lists (rootfs.diff_ids) is the
	// one thing both layouts record, so it is what each layer is checked
	// against — decompressing as it streams, which is also what catches a
	// truncated layer.
	meta := &types.ImageMetadata{LayerOrder: item.Layers, LayerMediaTypes: item.LayerMediaTypes}
	stream := newLayerStreamSource(ar.archive)
	if stream != nil {
		defer stream.Close()
	}
	for i, layerName := range item.Layers {
		line := VerifyLine{Name: layerName, Kind: "layer", Status: "ok"}
		if meta.NonDistributableLayer(i) {
			res.Skipped++
			line.Status = "skipped"
			line.Detail = "non-distributable layer, the archive does not carry it"
			res.Checks = append(res.Checks, line)
			continue
		}
		if fast {
			if !ar.has(layerName) {
				res.Failed++
				line.Status = "missing"
				line.Detail = "not present in the archive"
				res.Checks = append(res.Checks, line)
				continue
			}
			res.Verified++
			continue
		}
		var reader io.Reader
		var closeFn func()
		if stream != nil {
			reader, closeFn, err = stream.OpenLayer(layerName)
		} else {
			var rc io.ReadCloser
			rc, _, err = ar.open(layerName)
			if err == nil {
				reader, closeFn, err = layerReaderFor(rc)
			} else {
				err = layerOpenError(meta, layerName, err)
			}
		}
		if err != nil {
			res.Failed++
			line.Status = "unreadable"
			line.Detail = err.Error()
			res.Checks = append(res.Checks, line)
			continue
		}
		sum := sha256.New()
		_, copyErr := io.Copy(sum, reader)
		if closeFn != nil {
			closeFn()
		}
		got := hex.EncodeToString(sum.Sum(nil))
		want := ""
		if i < len(cfg.RootFS.DiffIDs) {
			want = strings.TrimPrefix(cfg.RootFS.DiffIDs[i], "sha256:")
		}
		switch {
		case copyErr != nil:
			res.Failed++
			line.Status = "unreadable"
			line.Detail = copyErr.Error()
		case want == "":
			res.Skipped++
			line.Status = "unverifiable"
			line.Detail = "the config lists no digest for this layer"
		case got != want:
			res.Failed++
			line.Status = "mismatch"
			line.Detail = fmt.Sprintf("sha256 is %s, the config says %s", shortDigest(got), shortDigest(want))
		default:
			res.Verified++
			continue
		}
		res.Checks = append(res.Checks, line)
	}
	return res, nil
}

// digestFromName extracts a sha256 digest from an entry name: "sha256:…",
// "blobs/sha256/<hex>" and "<hex>.json" all carry one.
func digestFromName(name string) string {
	trimmed := name
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	trimmed = strings.TrimPrefix(trimmed, "sha256:")
	trimmed = strings.TrimSuffix(trimmed, ".json")
	trimmed = strings.TrimSuffix(trimmed, ".tar")
	if len(trimmed) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return ""
	}
	return strings.ToLower(trimmed)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shortDigest(hexDigest string) string {
	if len(hexDigest) > 19 {
		return "sha256:" + hexDigest[:19] + "…"
	}
	return "sha256:" + hexDigest
}

// formatVerifySummary renders a one-line summary per image.
func FormatVerifySummary(res VerifyResult) string {
	status := "OK"
	if res.Failed > 0 {
		status = "FAILED"
	}
	return fmt.Sprintf("%s  %s  platform=%s  layers=%d verified=%d failed=%d skipped=%d",
		status, res.Image, orQuestion(res.Platform), res.LayerCount, res.Verified, res.Failed, res.Skipped)
}

func orQuestion(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// verifyFailures counts images that failed any check.
func verifyFailures(results []VerifyResult) int {
	n := 0
	for _, r := range results {
		if r.Failed > 0 {
			n++
		}
	}
	return n
}

// --- a small archive handle for verifying ---

// openedArchive memoises one archive's entry names so a fast check can ask
// whether a layer is present without opening it.
type openedArchive struct {
	archive arch.Archive
	names   map[string]bool
	order   []string
}

func openArchive(path string) (*openedArchive, error) {
	a, err := arch.Open(path)
	if err != nil {
		return nil, err
	}
	oa := &openedArchive{archive: a}
	if entries, err := a.List(); err == nil {
		oa.names = make(map[string]bool, len(entries))
		for _, e := range entries {
			oa.names[e.Name] = true
			oa.order = append(oa.order, e.Name)
		}
	}
	return oa, nil
}

func (o *openedArchive) close() error { return nil }

func (o *openedArchive) has(name string) bool {
	if o.names == nil {
		return true // listing unavailable: let the open decide
	}
	return o.names[name]
}

func (o *openedArchive) open(name string) (io.ReadCloser, int64, error) { return o.archive.Open(name) }

func (o *openedArchive) readAll(name string) ([]byte, error) {
	rc, _, err := o.archive.Open(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// readManifestItems reads manifest.json (through the index when one exists).
func readManifestItems(imageTarPath string) ([]types.ManifestItem, error) {
	data, err := readManifestBytes(imageTarPath)
	if err != nil {
		return nil, err
	}
	var manifest []types.ManifestItem
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	return manifest, nil
}

// layerReaderFor decompresses a layer the way the reader does: by magic, so a
// layer stored as a compressed blob inside an OCI layout or as a plain tar in a
// docker save both work.
func layerReaderFor(rc io.ReadCloser) (io.Reader, func(), error) {
	r, closeFn, err := layer.OpenLayerReader(rc)
	if err != nil {
		return nil, func() {}, err
	}
	return r, closeFn, nil
}
