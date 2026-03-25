package layer

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"

	"udf/internal/fsutil"
)

func ApplyLayer(r io.Reader, outputDir string, buf []byte) ([]fsutil.DirMetadata, error) {
	layerReader, closeFn, err := openLayerReader(r)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	tr := tar.NewReader(layerReader)
	var dirs []fsutil.DirMetadata

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return dirs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read layer entry: %w", err)
		}

		handled, err := HandleWhiteout(outputDir, hdr.Name)
		if err != nil {
			return nil, err
		}
		if handled {
			continue
		}

		targetPath, err := fsutil.ResolveSafePath(outputDir, hdr.Name)
		if err != nil {
			return nil, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := fsutil.ReplaceWithDir(targetPath, hdr.FileInfo().Mode()); err != nil {
				return nil, fmt.Errorf("create directory %s: %w", hdr.Name, err)
			}
			dirs = append(dirs, fsutil.DirMetadata{
				Path:    targetPath,
				Mode:    hdr.FileInfo().Mode(),
				ModTime: hdr.ModTime,
			})
		case tar.TypeReg, tar.TypeRegA:
			if err := fsutil.ReplaceWithFile(targetPath, hdr, tr, buf); err != nil {
				return nil, fmt.Errorf("write file %s: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			if err := fsutil.ReplaceWithSymlink(targetPath, hdr); err != nil {
				return nil, fmt.Errorf("create symlink %s: %w", hdr.Name, err)
			}
		case tar.TypeLink:
			if err := fsutil.ReplaceWithHardlink(outputDir, targetPath, hdr); err != nil {
				return nil, fmt.Errorf("create hardlink %s: %w", hdr.Name, err)
			}
		default:
			return nil, fmt.Errorf("unsupported tar entry type %q for %s", hdr.Typeflag, hdr.Name)
		}
	}
}

func openLayerReader(r io.Reader) (io.Reader, func(), error) {
	br := bufio.NewReader(r)
	header, err := br.Peek(6)
	if err != nil && err != io.EOF {
		return nil, func() {}, fmt.Errorf("peek layer header: %w", err)
	}

	if len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		gr, err := gzip.NewReader(br)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open gzip layer: %w", err)
		}
		return gr, func() { _ = gr.Close() }, nil
	}

	if len(header) >= 3 && header[0] == 'B' && header[1] == 'Z' && header[2] == 'h' {
		return bzip2.NewReader(br), func() {}, nil
	}

	if len(header) >= 4 && header[0] == 0x28 && header[1] == 0xB5 && header[2] == 0x2F && header[3] == 0xFD {
		return nil, func() {}, fmt.Errorf("unsupported zstd-compressed layer")
	}

	return br, func() {}, nil
}
