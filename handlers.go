package main

import (
	"context"
	"errors"
	"fmt"

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
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip); no files are uploaded" required:"true" cli:"positional"`
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
}

func infoImage(_ context.Context, in *InfoArgs) (*InfoResult, error) {
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
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip); no files are uploaded" required:"true" cli:"positional"`
	Path       string `json:"path" desc:"path inside the merged image filesystem; / or empty lists the image root" default:"/" cli:"positional"`
	RepoTag    string `json:"repo_tag" desc:"select the image by RepoTag from manifest.json, e.g. repo/app:latest" cli:"shorthand=t"`
	ImageIndex int    `json:"image_index" desc:"select the image by its index in the manifest.json array" default:"-1" cli:"shorthand=i"`
}

func listImage(_ context.Context, in *LsArgs) ([]image.FileEntry, error) {
	sel, err := selectionFor(in.RepoTag, in.ImageIndex)
	if err != nil {
		return nil, err
	}

	meta, err := image.ScanImageMetadata(in.Archive, sel)
	if err != nil {
		return nil, toXyzErr(err)
	}
	tree, err := image.BuildFileSystem(in.Archive, meta)
	if err != nil {
		return nil, toXyzErr(err)
	}
	entries, err := image.ListEntries(tree, in.Path)
	if err != nil {
		return nil, toXyzErr(err)
	}
	return entries, nil
}

// ---- cp ----

type CpArgs struct {
	Archive    string `json:"archive" desc:"local path to the image archive on this machine (tar/tar.gz/tgz/zip); no files are uploaded" required:"true" cli:"positional"`
	Source     string `json:"source" desc:"file or directory path inside the merged image filesystem" required:"true" cli:"positional"`
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

// ---- extract ----

type ExtractArgs struct {
	Archive    string `json:"archive" desc:"local path to an image archive on this machine; may also be a glob pattern or a directory (top level scanned)" required:"true" cli:"positional"`
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

	paths, err := resolveInputs([]string{in.Archive})
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
