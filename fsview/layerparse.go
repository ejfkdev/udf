package fsview

import (
	"archive/tar"
	"fmt"
	"io"
)

// ParsedLayer holds one layer's tar headers in memory so the layer can be
// merged later. It exists for archives whose members cannot be re-read in
// manifest order: a single sequential pass parses every layer (a compressed
// archive is decompressed once), then the merge applies them in order.
type ParsedLayer struct {
	headers []*tar.Header
}

// HeaderCount reports how many tar entries the parsed layer holds; callers
// use it to bound how much is buffered.
func (l *ParsedLayer) HeaderCount() int { return len(l.headers) }

// ParseLayer reads all tar headers from r, discarding file data.
func ParseLayer(r io.Reader) (*ParsedLayer, error) {
	tr := tar.NewReader(r)
	var headers []*tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return &ParsedLayer{headers: headers}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read layer entry: %w", err)
		}
		headers = append(headers, hdr)
	}
}

// BuildParsed merges pre-parsed layers in the given order. Semantics match
// Build: whiteouts, opaque directories and later-layer overrides apply the
// same way.
func BuildParsed(layerOrder []string, layers map[string]*ParsedLayer) (*Node, error) {
	root := &Node{Kind: KindDir, Mode: 0o755}
	for _, layerName := range layerOrder {
		layer := layers[layerName]
		if layer == nil {
			return nil, fmt.Errorf("layer %s is missing", layerName)
		}
		for _, hdr := range layer.headers {
			if err := mergeEntry(root, layerName, hdr); err != nil {
				return nil, fmt.Errorf("merge layer %s: %w", layerName, err)
			}
		}
	}
	return root, nil
}
