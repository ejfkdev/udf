package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	errs "github.com/ejfkdev/xyz-go/errors"

	appi18n "github.com/ejfkdev/udf/i18n"
	"github.com/ejfkdev/udf/image"
)

// selectionFor turns the selection flags into one: --tag (spelled -t or
// --repo-tag for compatibility) names an image — a full repo tag, name:tag, a
// repository name or a bare tag, resolved by the image side — and --index
// (spelled -i or --image-index) takes its number. Exactly one of the two may
// be given, since an archive's images are picked by name or by position.
func selectionFor(repoTag string, imageIndex int, tag string, index int, platform string) (image.Selection, error) {
	sel := image.Selection{ImageIndex: -1}
	if name := strings.TrimSpace(repoTag); name != "" {
		sel.RepoTag = name
	}
	if name := strings.TrimSpace(tag); name != "" {
		sel.RepoTag = name
	}
	if index >= 0 {
		sel.ImageIndex = index
	} else if imageIndex >= 0 {
		sel.ImageIndex = imageIndex
	}
	if p := strings.TrimSpace(platform); p != "" {
		sel.Platform = p
	}
	if sel.RepoTag != "" && sel.ImageIndex >= 0 {
		return image.Selection{}, errs.New(errs.KindInvalidInput, "use only one of --tag (a name) or --index (a number)")
	}
	if sel.Platform != "" && sel.ImageIndex >= 0 {
		return image.Selection{}, errs.New(errs.KindInvalidInput, "use only one of --platform (os/arch[/variant]) or --index (a number)")
	}
	return sel, nil
}

// selectionOf builds the selection from whichever of the six selection fields
// an Args struct carries; every command uses the same three options.
func selectionOf(repoTag string, imageIndex int, tag string, index int, platform string) (image.Selection, error) {
	return selectionFor(repoTag, imageIndex, tag, index, platform)
}

// toXyzErr translates library errors into the xyz error taxonomy so that the
// CLI exit code, the HTTP status code and the MCP error code stay aligned.
func toXyzErr(err error) error {
	if err == nil {
		return nil
	}

	var le *appi18n.LocalizedError
	if errors.As(err, &le) {
		kind := errs.KindInvalidInput
		switch le.Key {
		case "err_ls_path_not_found", "err_cp_src_not_found":
			kind = errs.KindNotFound
		case "err_cp_dest_conflict":
			kind = errs.KindConflict
		}
		return errs.New(kind, le.Error())
	}

	return errs.New(errs.KindInternal, err.Error())
}

// ---- info ----

type InfoArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip/qcow2/vmdk/vhd/vhdx/ova/vma); no files are uploaded" required:"true" cli:"positional"`
	RepoTag    string `json:"repo-tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (same as --tag)" cli:"shorthand=t"`
	ImageIndex int    `json:"image-index" desc:"select the image by its index in the manifest.json array, e.g. 3 (same as --index)" default:"-1" cli:"shorthand=i"`
	Tag        string `json:"tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (the preferred spelling of -t/--repo-tag)"`
	Index      int    `json:"index" desc:"select the image by its index in the manifest.json array, e.g. 3 (the preferred spelling of -i/--image-index)" default:"-1"`
	Platform   string `json:"platform" desc:"select the image by platform, e.g. linux/arm64 or linux/arm/v7 (matches the config's os/architecture/variant)"`
}

// ImageInfoResult is the detail of one selected image.
type ImageInfoResult struct {
	Index         int      `json:"index"`
	TotalImages   int      `json:"total_images"`
	RepoTags      []string `json:"repo_tags"`
	ConfigPath    string   `json:"config_path"`
	OS            string   `json:"os,omitempty"`
	Architecture  string   `json:"architecture,omitempty"`
	Variant       string   `json:"variant,omitempty"`
	Platform      string   `json:"platform,omitempty"`
	Created       string   `json:"created,omitempty"`
	DockerVersion string   `json:"docker_version,omitempty"`
	WorkingDir    string   `json:"working_dir,omitempty"`
	Entrypoint    []string `json:"entrypoint,omitempty"`
	Cmd           []string `json:"cmd,omitempty"`
	LayerCount    int      `json:"layer_count"`
	Size          string   `json:"size,omitempty"`
	Layers        []string `json:"layers"`

	// ManifestDigest is "sha256:…" of the raw image manifest when the format
	// records one (OCI layouts and archives).
	ManifestDigest string `json:"manifest_digest,omitempty"`
	// LayerMediaTypes lists the distinct layer media types the manifest names
	// (OCI); a classic docker save records none.
	LayerMediaTypes []string `json:"layer_media_types,omitempty"`
	// NonDistributableLayers counts the layers whose content lives at the
	// vendor and is therefore not part of the archive.
	NonDistributableLayers int `json:"non_distributable_layers,omitempty"`
}

// InfoResult answers for the inputs that are not a single image: disk images
// and plain archives. Keeping it separate from the image detail means neither
// form carries an empty section.
type InfoResult struct {
	// Disks carries disk-image metadata (qcow2/vmdk/ova), present only for
	// disk inputs.
	Disks []image.DiskInfo `json:"disks,omitempty"`

	// Plain carries plain-archive metadata (asar/rpm/tar/zip/...), present only
	// for non-docker, non-disk archive inputs.
	Plain *image.PlainInfo `json:"plain,omitempty"`
}

// infoImage answers with a single image's detail, or — for an archive holding
// several images that the caller did not select from — with one summary per
// image, which the CLI renders as a table and --json as an array.
func infoImage(_ context.Context, in *InfoArgs) (any, error) {
	switch image.ClassifyInput(in.Archive) {
	case "disk":
		meta, err := image.ScanDiskMetadata(in.Archive)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return &InfoResult{Disks: meta.Disks}, nil
	case "archive":
		info, err := image.PlainArchiveInfo(in.Archive)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return &InfoResult{Plain: info}, nil
	}

	sel, err := selectionOf(in.RepoTag, in.ImageIndex, in.Tag, in.Index, in.Platform)
	if err != nil {
		return nil, err
	}

	// No image selected: an archive holding several images is answered with a
	// listing of them, since which one the caller wants is exactly what is
	// missing — the detail of any one image is one -t/-i away.
	if sel.RepoTag == "" && sel.ImageIndex < 0 && sel.Platform == "" {
		summaries, err := image.ScanImageSummaries(in.Archive)
		if err == nil && len(summaries) > 1 {
			return summaries, nil
		}
	}

	meta, err := image.ScanImageMetadata(in.Archive, sel)
	if err != nil {
		return nil, toXyzErr(err)
	}

	resp := &ImageInfoResult{
		Index:          meta.Index,
		TotalImages:    meta.Total,
		RepoTags:       meta.RepoTags,
		ConfigPath:     meta.ConfigPath,
		LayerCount:     len(meta.LayerOrder),
		Layers:         meta.LayerOrder,
		ManifestDigest: meta.ManifestDigest,
	}
	if meta.StoredSize > 0 {
		resp.Size = image.HumanBytes(meta.StoredSize)
	}
	if meta.Config != nil {
		resp.OS = meta.Config.OS
		resp.Architecture = meta.Config.Architecture
		resp.Variant = meta.Config.Variant
		resp.Platform = meta.Config.Platform()
		resp.Created = meta.Config.Created
		resp.DockerVersion = meta.Config.DockerVersion
		resp.WorkingDir = meta.Config.Config.WorkingDir
		resp.Entrypoint = meta.Config.Config.Entrypoint
		resp.Cmd = meta.Config.Config.Cmd
	}
	for i := range meta.LayerOrder {
		if meta.NonDistributableLayer(i) {
			resp.NonDistributableLayers++
		}
	}
	resp.LayerMediaTypes = distinctSorted(meta.LayerMediaTypes)
	return resp, nil
}

// distinctSorted returns the non-empty values of in, deduplicated and sorted,
// so an image with twenty gzip layers names its media type once.
func distinctSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ---- ls ----

type LsArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip/qcow2/vmdk/vhd/vhdx/ova/vma); no files are uploaded" required:"true" cli:"positional"`
	Path       string `json:"path" desc:"path inside the image; / lists disks/volumes, e.g. /vg1/root or /vg1/root/etc" default:"/" cli:"positional"`
	RepoTag    string `json:"repo-tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (same as --tag)" cli:"shorthand=t"`
	ImageIndex int    `json:"image-index" desc:"select the image by its index in the manifest.json array, e.g. 3 (same as --index)" default:"-1" cli:"shorthand=i"`
	Tag        string `json:"tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (the preferred spelling of -t/--repo-tag)"`
	Index      int    `json:"index" desc:"select the image by its index in the manifest.json array, e.g. 3 (the preferred spelling of -i/--image-index)" default:"-1"`
	Platform   string `json:"platform" desc:"select the image by platform, e.g. linux/arm64 or linux/arm/v7 (matches the config's os/architecture/variant)"`
}

func listImage(_ context.Context, in *LsArgs) ([]image.FileEntry, error) {
	switch image.ClassifyInput(in.Archive) {
	case "disk":
		entries, err := image.ListDisk(in.Archive, in.Path)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return entries, nil
	case "archive":
		entries, err := image.ListPlainArchive(in.Archive, in.Path)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return entries, nil
	}

	sel, err := selectionOf(in.RepoTag, in.ImageIndex, in.Tag, in.Index, in.Platform)
	if err != nil {
		return nil, err
	}

	entries, err := image.ListArchive(in.Archive, sel, in.Path)
	if err != nil {
		return nil, toXyzErr(err)
	}
	return entries, nil
}

// ---- cp ----

type CpArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip/qcow2/vmdk/vhd/vhdx/ova/vma); no files are uploaded" required:"true" cli:"positional"`
	Source     string `json:"source" desc:"path inside the image; for a disk image use /volume/path, e.g. /vg1/root/etc/passwd" required:"true" cli:"positional"`
	Dest       string `json:"dest" desc:"destination path on this machine" required:"true" cli:"positional"`
	BufferSize int    `json:"buffer-size" desc:"copy buffer size in bytes" default:"1048576" cli:"shorthand=b"`
	RepoTag    string `json:"repo-tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (same as --tag)" cli:"shorthand=t"`
	ImageIndex int    `json:"image-index" desc:"select the image by its index in the manifest.json array, e.g. 3 (same as --index)" default:"-1" cli:"shorthand=i"`
	Tag        string `json:"tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (the preferred spelling of -t/--repo-tag)"`
	Index      int    `json:"index" desc:"select the image by its index in the manifest.json array, e.g. 3 (the preferred spelling of -i/--image-index)" default:"-1"`
	Platform   string `json:"platform" desc:"select the image by platform, e.g. linux/arm64 or linux/arm/v7 (matches the config's os/architecture/variant)"`
}

type CpResult struct {
	Source    string `json:"source"`
	Dest      string `json:"dest"`
	Extracted int    `json:"extracted"`
}

func copyEntry(_ context.Context, in *CpArgs) (*CpResult, error) {
	if in.BufferSize <= 0 {
		return nil, errs.New(errs.KindInvalidInput, fmt.Sprintf("invalid buffer size: %d", in.BufferSize))
	}
	switch image.ClassifyInput(in.Archive) {
	case "disk":
		count, err := image.ExtractDiskPath(in.Archive, in.Source, in.Dest, in.BufferSize)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return &CpResult{Source: in.Source, Dest: in.Dest, Extracted: count}, nil
	case "archive":
		count, err := image.ExtractPlainPath(in.Archive, in.Source, in.Dest, in.BufferSize)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return &CpResult{Source: in.Source, Dest: in.Dest, Extracted: count}, nil
	}
	sel, err := selectionOf(in.RepoTag, in.ImageIndex, in.Tag, in.Index, in.Platform)
	if err != nil {
		return nil, err
	}

	meta, err := image.ScanImageMetadata(in.Archive, sel)
	if err != nil {
		return nil, toXyzErr(err)
	}
	count, err := image.ExtractPath(in.Archive, meta, in.Source, in.Dest, in.BufferSize)
	if err != nil {
		return nil, toXyzErr(err)
	}
	return &CpResult{Source: in.Source, Dest: in.Dest, Extracted: count}, nil
}

// ---- cat ----

type CatArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip/7z/rar/qcow2/vmdk/vhd/vhdx/ova/vma); no files are uploaded" required:"true" cli:"positional"`
	Source     string `json:"source" desc:"path inside the image; for a disk image use /volume/path, e.g. /vg1/root/etc/passwd" required:"true" cli:"positional"`
	BufferSize int    `json:"buffer-size" desc:"copy buffer size in bytes" default:"1048576" cli:"shorthand=b"`
	RepoTag    string `json:"repo-tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (same as --tag)" cli:"shorthand=t"`
	ImageIndex int    `json:"image-index" desc:"select the image by its index in the manifest.json array, e.g. 3 (same as --index)" default:"-1" cli:"shorthand=i"`
	Tag        string `json:"tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (the preferred spelling of -t/--repo-tag)"`
	Index      int    `json:"index" desc:"select the image by its index in the manifest.json array, e.g. 3 (the preferred spelling of -i/--image-index)" default:"-1"`
	Platform   string `json:"platform" desc:"select the image by platform, e.g. linux/arm64 or linux/arm/v7 (matches the config's os/architecture/variant)"`
}

// catEntry streams the raw bytes of one file straight to stdout so the result
// can be piped into another program. It returns a nil value on purpose: the
// xyz CLI frontend frames non-nil results (a trailing newline for strings, a
// key/value table for structs) which would corrupt binary output.
func catEntry(_ context.Context, in *CatArgs) (any, error) {
	if in.BufferSize <= 0 {
		return nil, errs.New(errs.KindInvalidInput, fmt.Sprintf("invalid buffer size: %d", in.BufferSize))
	}

	rc, err := openImageFileReader(in.Archive, in.Source, in.RepoTag, in.ImageIndex, in.Tag, in.Index, in.Platform)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	if _, err := io.CopyBuffer(os.Stdout, rc, make([]byte, in.BufferSize)); err != nil {
		return nil, errs.New(errs.KindInternal, err.Error())
	}
	return nil, nil
}

// openImageFileReader resolves one in-image path and returns a reader over its
// bytes, routing disk images and archives the same way cp and cat do.
func openImageFileReader(archive, source, repoTag string, imageIndex int, tag string, index int, platform string) (io.ReadCloser, error) {
	switch image.ClassifyInput(archive) {
	case "disk":
		rc, _, err := image.ReadDiskFile(archive, source)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return rc, nil
	case "archive":
		rc, _, err := image.ReadPlainArchiveFile(archive, source)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return rc, nil
	}

	sel, err := selectionOf(repoTag, imageIndex, tag, index, platform)
	if err != nil {
		return nil, err
	}
	meta, err := image.ScanImageMetadata(archive, sel)
	if err != nil {
		return nil, toXyzErr(err)
	}
	rc, _, err := image.ReadArchiveFile(archive, meta, source)
	if err != nil {
		return nil, toXyzErr(err)
	}
	return rc, nil
}

// ---- xxd ----

type XxdArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip/7z/rar/qcow2/vmdk/vhd/vhdx/ova/vma); no files are uploaded" required:"true" cli:"positional"`
	Source     string `json:"source" desc:"path inside the image; for a disk image use /volume/path, e.g. /vg1/root/etc/passwd" required:"true" cli:"positional"`
	Bytes      int    `json:"bytes" desc:"number of bytes to dump" default:"256" cli:"shorthand=n"`
	Offset     int    `json:"offset" desc:"skip this many bytes from the start of the file before dumping" default:"0" cli:"shorthand=s"`
	RepoTag    string `json:"repo-tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (same as --tag)" cli:"shorthand=t"`
	ImageIndex int    `json:"image-index" desc:"select the image by its index in the manifest.json array, e.g. 3 (same as --index)" default:"-1" cli:"shorthand=i"`
	Tag        string `json:"tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (the preferred spelling of -t/--repo-tag)"`
	Index      int    `json:"index" desc:"select the image by its index in the manifest.json array, e.g. 3 (the preferred spelling of -i/--image-index)" default:"-1"`
	Platform   string `json:"platform" desc:"select the image by platform, e.g. linux/arm64 or linux/arm/v7 (matches the config's os/architecture/variant)"`
}

// xxdEntry returns a bounded hex+ASCII dump of one in-image file, mirroring the
// OS `xxd -l N` / `hexdump -C` behaviour, so a caller can inspect a binary
// file's header without dumping the whole thing. Unlike cat it returns text, so
// it flows through the normal result path (CLI/--json/HTTP/MCP).
func xxdEntry(_ context.Context, in *XxdArgs) (string, error) {
	if in.Bytes <= 0 {
		return "", errs.New(errs.KindInvalidInput, fmt.Sprintf("invalid byte count: %d", in.Bytes))
	}
	if in.Offset < 0 {
		return "", errs.New(errs.KindInvalidInput, fmt.Sprintf("invalid offset: %d", in.Offset))
	}

	rc, err := openImageFileReader(in.Archive, in.Source, in.RepoTag, in.ImageIndex, in.Tag, in.Index, in.Platform)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	if in.Offset > 0 {
		if _, err := io.CopyN(io.Discard, rc, int64(in.Offset)); err != nil && err != io.EOF {
			return "", errs.New(errs.KindInternal, err.Error())
		}
	}
	data, err := io.ReadAll(io.LimitReader(rc, int64(in.Bytes)))
	if err != nil {
		return "", errs.New(errs.KindInternal, err.Error())
	}
	return hexDump(data, int64(in.Offset)), nil
}

// ---- extract ----

type ExtractArgs struct {
	Archive    string `json:"archive" desc:"local path to an image archive or a disk image (qcow2/vmdk/vhd/vhdx/ova/vma) on this machine; may also be a glob pattern or a directory (top level scanned)" required:"true" cli:"positional"`
	Output     string `json:"output" desc:"output parent directory (default: beside each input archive)" cli:"shorthand=o"`
	Force      bool   `json:"force" desc:"force writing into an existing non-empty target directory" cli:"shorthand=f"`
	BufferSize int    `json:"buffer-size" desc:"copy buffer size in bytes" default:"1048576" cli:"shorthand=b"`
	RepoTag    string `json:"repo-tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (same as --tag)" cli:"shorthand=t"`
	ImageIndex int    `json:"image-index" desc:"select the image by its index in the manifest.json array, e.g. 3 (same as --index)" default:"-1" cli:"shorthand=i"`
	Tag        string `json:"tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (the preferred spelling of -t/--repo-tag)"`
	Index      int    `json:"index" desc:"select the image by its index in the manifest.json array, e.g. 3 (the preferred spelling of -i/--image-index)" default:"-1"`
	Platform   string `json:"platform" desc:"select the image by platform, e.g. linux/arm64 or linux/arm/v7 (matches the config's os/architecture/variant)"`
}

type ExtractResult struct {
	Archive   string `json:"archive"`
	OutputDir string `json:"output_dir"`
	Layers    int    `json:"layers"`
	Error     string `json:"error,omitempty"`
}

func extractImages(ctx context.Context, in *ExtractArgs) ([]ExtractResult, error) {
	if in.BufferSize <= 0 {
		return nil, errs.New(errs.KindInvalidInput, fmt.Sprintf("invalid buffer size: %d", in.BufferSize))
	}
	sel, err := selectionOf(in.RepoTag, in.ImageIndex, in.Tag, in.Index, in.Platform)
	if err != nil {
		return nil, err
	}

	paths, err := resolveDiskOrArchiveInputs(in.Archive)
	if err != nil {
		return nil, errs.New(errs.KindInvalidInput, err.Error())
	}
	if len(paths) == 0 {
		return nil, errs.New(errs.KindInvalidInput, "no supported archive files found")
	}

	if len(paths) == 1 {
		r, err := extractOne(paths[0], in.Output, in.Force, in.BufferSize, sel)
		if err != nil {
			return []ExtractResult{{Archive: paths[0], Error: err.Error()}}, err
		}
		return []ExtractResult{r}, nil
	}

	// Archives are independent inputs, so a batch is extracted with one worker
	// per core: each one builds its own index and writes its own output
	// directory, and the serial decompression inside a worker keeps the core
	// busy. Results stay in input order.
	//
	// Two archives that share a file name resolve to the same output
	// directory, so a batch containing such a pair stays sequential: writing
	// the same tree from two workers would race.
	parallel := true
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		base := filepath.Base(p)
		if seen[base] {
			parallel = false
			break
		}
		seen[base] = true
	}
	results := make([]ExtractResult, len(paths))
	errsByIndex := make([]error, len(paths))
	succeeded := make([]bool, len(paths))
	workers := extractWorkers(len(paths))
	if !parallel {
		workers = 1
	}
	var wg sync.WaitGroup
	var next int64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1)) - 1
				if i >= len(paths) || ctx.Err() != nil {
					return
				}
				r, err := extractOne(paths[i], in.Output, in.Force, in.BufferSize, sel)
				if err != nil {
					errsByIndex[i] = err
					r = ExtractResult{Archive: paths[i], Error: err.Error()}
				} else {
					succeeded[i] = true
				}
				results[i] = r
			}
		}()
	}
	wg.Wait()

	var firstErr error
	var ok int
	for i := range paths {
		if succeeded[i] {
			ok++
		} else if firstErr == nil {
			firstErr = errsByIndex[i]
		}
	}

	if ok == 0 && firstErr != nil {
		return results, firstErr
	}
	return results, nil
}

// extractWorkers bounds how many archives are extracted at once: one per
// core, but never more than eight, since each worker also uses several cores
// for its own layer reads.
func extractWorkers(paths int) int {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if v := os.Getenv("UDF_EXTRACT_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			workers = n
		}
	}
	if workers > paths {
		workers = paths
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

func extractOne(p, outputDir string, force bool, bufferSize int, sel image.Selection) (ExtractResult, error) {
	switch image.ClassifyInput(p) {
	case "disk":
		return extractDiskOne(p, outputDir, force, bufferSize)
	case "archive":
		return extractPlainOne(p, outputDir, force, bufferSize)
	}

	meta, err := image.ScanImageMetadata(p, sel)
	if err != nil {
		return ExtractResult{}, toXyzErr(err)
	}

	target := resolveOutputDir(p, outputDir, meta)
	if err := image.PrepareOutputDir(target, force); err != nil {
		return ExtractResult{}, errs.New(errs.KindConflict, err.Error())
	}
	if err := image.WriteConfigYAML(target, meta); err != nil {
		return ExtractResult{}, toXyzErr(err)
	}
	if err := image.ApplyImage(p, meta, target, bufferSize, nil); err != nil {
		return ExtractResult{}, toXyzErr(err)
	}

	return ExtractResult{Archive: p, OutputDir: target, Layers: len(meta.LayerOrder)}, nil
}

func extractDiskOne(p, outputDir string, force bool, bufferSize int) (ExtractResult, error) {
	target := resolveOutputDir(p, outputDir, nil)
	if err := image.PrepareOutputDir(target, force); err != nil {
		return ExtractResult{}, errs.New(errs.KindConflict, err.Error())
	}
	if _, err := image.ExtractDiskVolumes(p, target, bufferSize); err != nil {
		return ExtractResult{}, toXyzErr(err)
	}
	return ExtractResult{Archive: p, OutputDir: target}, nil
}

func extractPlainOne(p, outputDir string, force bool, bufferSize int) (ExtractResult, error) {
	target := resolveOutputDir(p, outputDir, nil)
	if err := image.PrepareOutputDir(target, force); err != nil {
		return ExtractResult{}, errs.New(errs.KindConflict, err.Error())
	}
	if _, err := image.ExtractPlainArchive(p, target, bufferSize); err != nil {
		return ExtractResult{}, toXyzErr(err)
	}
	return ExtractResult{Archive: p, OutputDir: target, Layers: 1}, nil
}

// VerifyArgs selects what to check: the whole archive, or one image by tag,
// index or platform.
type VerifyArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip) or an OCI layout directory; no files are uploaded" required:"true" cli:"positional"`
	Fast       bool   `json:"fast" desc:"only check that every layer is present, without recomputing digests" cli:"shorthand=f"`
	RepoTag    string `json:"repo-tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (same as --tag)" cli:"shorthand=t"`
	ImageIndex int    `json:"image-index" desc:"select the image by its index in the manifest.json array, e.g. 3 (same as --index)" default:"-1" cli:"shorthand=i"`
	Tag        string `json:"tag" desc:"select the image by tag: a full RepoTag, name:tag, a repository name or a bare tag, when unambiguous (the preferred spelling of -t/--repo-tag)"`
	Index      int    `json:"index" desc:"select the image by its index in the manifest.json array, e.g. 3 (the preferred spelling of -i/--image-index)" default:"-1"`
	Platform   string `json:"platform" desc:"select the image by platform, e.g. linux/arm64 or linux/arm/v7 (matches the config's os/architecture/variant)"`
}

// verifyArchive checks digests and reports the result; images that fail any
// check are named in a summary error so the exit code reflects them.
func verifyArchive(_ context.Context, in *VerifyArgs) ([]image.VerifyResult, error) {
	if image.ClassifyInput(in.Archive) != "image" {
		return nil, errs.New(errs.KindInvalidInput,
			"verify checks docker-save archives, OCI layouts and OCI image archives; this input is neither")
	}
	sel, err := selectionOf(in.RepoTag, in.ImageIndex, in.Tag, in.Index, in.Platform)
	if err != nil {
		return nil, err
	}
	results, err := image.VerifyImages(in.Archive, sel, in.Fast)
	if err != nil {
		return nil, toXyzErr(err)
	}
	if failed := image.VerifyFailures(results); failed > 0 {
		return results, errs.New(errs.KindInternal,
			fmt.Sprintf("%d of %d image(s) failed verification; see the checks column", failed, len(results)))
	}
	return results, nil
}
