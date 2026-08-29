package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	errs "github.com/ejfkdev/xyz-go/errors"

	appi18n "github.com/ejfkdev/udf/i18n"
	"github.com/ejfkdev/udf/image"
)

// selectionFor validates and builds the image selection shared by every
// command: archives may contain several images, and -t/-i pick one the same
// way across all three interfaces.
func selectionFor(repoTag string, imageIndex int) (image.Selection, error) {
	if repoTag != "" && imageIndex >= 0 {
		return image.Selection{}, errs.New(errs.KindInvalidInput, "use only one of --repo-tag/--image-index")
	}
	return image.Selection{ImageIndex: imageIndex, RepoTag: repoTag}, nil
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
	RepoTag    string `json:"repo_tag" desc:"select the image by RepoTag from manifest.json, e.g. repo/app:latest" cli:"shorthand=t"`
	ImageIndex int    `json:"image_index" desc:"select the image by its index in the manifest.json array" default:"-1" cli:"shorthand=i"`
}

type InfoResult struct {
	Index        int      `json:"index"`
	TotalImages  int      `json:"total_images"`
	RepoTags     []string `json:"repo_tags"`
	ConfigPath   string   `json:"config_path"`
	Architecture string   `json:"architecture,omitempty"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	Entrypoint   []string `json:"entrypoint,omitempty"`
	Cmd          []string `json:"cmd,omitempty"`
	Layers       []string `json:"layers"`

	// Disks carries disk-image metadata (qcow2/vmdk/ova), present only for
	// disk inputs.
	Disks []image.DiskInfo `json:"disks,omitempty"`
}

func infoImage(_ context.Context, in *InfoArgs) (*InfoResult, error) {
	if image.IsDiskImage(in.Archive) {
		meta, err := image.ScanDiskMetadata(in.Archive)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return &InfoResult{Disks: meta.Disks}, nil
	}

	sel, err := selectionFor(in.RepoTag, in.ImageIndex)
	if err != nil {
		return nil, err
	}

	meta, err := image.ScanImageMetadata(in.Archive, sel)
	if err != nil {
		return nil, toXyzErr(err)
	}

	resp := &InfoResult{
		Index:       meta.Index,
		TotalImages: meta.Total,
		RepoTags:    meta.RepoTags,
		ConfigPath:  meta.ConfigPath,
		Layers:      meta.LayerOrder,
	}
	if meta.Config != nil {
		resp.Architecture = meta.Config.Architecture
		resp.WorkingDir = meta.Config.Config.WorkingDir
		resp.Entrypoint = meta.Config.Config.Entrypoint
		resp.Cmd = meta.Config.Config.Cmd
	}
	return resp, nil
}

// ---- ls ----

type LsArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip/qcow2/vmdk/vhd/vhdx/ova/vma); no files are uploaded" required:"true" cli:"positional"`
	Path       string `json:"path" desc:"path inside the image; / lists disks/volumes, e.g. /vg1/root or /vg1/root/etc" default:"/" cli:"positional"`
	RepoTag    string `json:"repo_tag" desc:"select the image by RepoTag from manifest.json, e.g. repo/app:latest" cli:"shorthand=t"`
	ImageIndex int    `json:"image_index" desc:"select the image by its index in the manifest.json array" default:"-1" cli:"shorthand=i"`
}

func listImage(_ context.Context, in *LsArgs) ([]image.FileEntry, error) {
	if image.IsDiskImage(in.Archive) {
		entries, err := image.ListDisk(in.Archive, in.Path)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return entries, nil
	}

	sel, err := selectionFor(in.RepoTag, in.ImageIndex)
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
	BufferSize int    `json:"buffer_size" desc:"file copy buffer size in bytes" default:"1048576"`
	RepoTag    string `json:"repo_tag" desc:"select the image by RepoTag from manifest.json, e.g. repo/app:latest" cli:"shorthand=t"`
	ImageIndex int    `json:"image_index" desc:"select the image by its index in the manifest.json array" default:"-1" cli:"shorthand=i"`
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
	if image.IsDiskImage(in.Archive) {
		count, err := image.ExtractDiskPath(in.Archive, in.Source, in.Dest, in.BufferSize)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return &CpResult{Source: in.Source, Dest: in.Dest, Extracted: count}, nil
	}
	sel, err := selectionFor(in.RepoTag, in.ImageIndex)
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
	BufferSize int    `json:"buffer_size" desc:"copy buffer size in bytes" default:"1048576"`
	RepoTag    string `json:"repo_tag" desc:"select the image by RepoTag from manifest.json, e.g. repo/app:latest" cli:"shorthand=t"`
	ImageIndex int    `json:"image_index" desc:"select the image by its index in the manifest.json array" default:"-1" cli:"shorthand=i"`
}

// catEntry streams the raw bytes of one file straight to stdout so the result
// can be piped into another program. It returns a nil value on purpose: the
// xyz CLI frontend frames non-nil results (a trailing newline for strings, a
// key/value table for structs) which would corrupt binary output.
func catEntry(_ context.Context, in *CatArgs) (any, error) {
	if in.BufferSize <= 0 {
		return nil, errs.New(errs.KindInvalidInput, fmt.Sprintf("invalid buffer size: %d", in.BufferSize))
	}

	rc, err := openImageFileReader(in.Archive, in.Source, in.RepoTag, in.ImageIndex)
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
func openImageFileReader(archive, source, repoTag string, imageIndex int) (io.ReadCloser, error) {
	if image.IsDiskImage(archive) {
		rc, _, err := image.ReadDiskFile(archive, source)
		if err != nil {
			return nil, toXyzErr(err)
		}
		return rc, nil
	}

	sel, err := selectionFor(repoTag, imageIndex)
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
	RepoTag    string `json:"repo_tag" desc:"select the image by RepoTag from manifest.json, e.g. repo/app:latest" cli:"shorthand=t"`
	ImageIndex int    `json:"image_index" desc:"select the image by its index in the manifest.json array" default:"-1" cli:"shorthand=i"`
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

	rc, err := openImageFileReader(in.Archive, in.Source, in.RepoTag, in.ImageIndex)
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
	BufferSize int    `json:"buffer_size" desc:"file copy buffer size in bytes" default:"1048576" cli:"shorthand=b"`
	RepoTag    string `json:"repo_tag" desc:"select the image by RepoTag from manifest.json, e.g. repo/app:latest" cli:"shorthand=t"`
	ImageIndex int    `json:"image_index" desc:"select the image by its index in the manifest.json array" default:"-1" cli:"shorthand=i"`
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
	sel, err := selectionFor(in.RepoTag, in.ImageIndex)
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

	results := make([]ExtractResult, 0, len(paths))
	var succeeded int
	var firstErr error

	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		r, err := extractOne(p, in.Output, in.Force, in.BufferSize, sel)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			r = ExtractResult{Archive: p, Error: err.Error()}
		} else {
			succeeded++
		}
		results = append(results, r)
	}

	if succeeded == 0 && firstErr != nil {
		return results, firstErr
	}
	return results, nil
}

func extractOne(p, outputDir string, force bool, bufferSize int, sel image.Selection) (ExtractResult, error) {
	if image.IsDiskImage(p) {
		return extractDiskOne(p, outputDir, force, bufferSize)
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
