package archive

import (
	"compress/bzip2"
	"compress/zlib"
	"fmt"
	"io"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// openCompressed wraps r in the decompressor named by compression, returning
// the decompressed reader and a close function. An empty compression passes r
// through unchanged.
func openCompressed(r io.Reader, compression string) (io.Reader, func(), error) {
	switch compression {
	case "", "raw":
		return r, func() {}, nil
	case "gzip":
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return gr, func() { _ = gr.Close() }, nil
	case "zlib":
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return zr, func() { _ = zr.Close() }, nil
	case "bzip2":
		return bzip2.NewReader(r), func() {}, nil
	case "xz":
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return xr, func() {}, nil
	case "lzma":
		lr, err := lzma.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return lr, func() {}, nil
	case "zstd":
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return zr, func() { zr.Close() }, nil
	case "lz4":
		return lz4.NewReader(r), func() {}, nil
	}
	return nil, nil, fmt.Errorf("unsupported compression: %s", compression)
}
