package image

import (
	"fmt"

	"github.com/ejfkdev/udf/types"
)

// layerOpenError explains why a layer could not be read. The common cause is a
// non-distributable (foreign) layer: its content lives at the vendor and an
// archive normally does not carry it, so "not found" would send the reader
// looking for a corrupt archive instead of an intentionally absent blob.
func layerOpenError(meta *types.ImageMetadata, layerName string, err error) error {
	if meta != nil {
		for i, name := range meta.LayerOrder {
			if name != layerName {
				continue
			}
			if meta.NonDistributableLayer(i) {
				mediaType := ""
				if i < len(meta.LayerMediaTypes) {
					mediaType = meta.LayerMediaTypes[i]
				}
				return fmt.Errorf("layer %d is non-distributable (%s) and its content is not part of the archive", i, mediaType)
			}
			break
		}
	}
	return err
}
