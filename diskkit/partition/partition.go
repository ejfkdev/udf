// Package partition provides ability to work with individual partitions.
// All useful implementations are subpackages of this package, e.g. github.com/ejfkdev/udf/diskkit/gpt
package partition

import (
	"fmt"

	"github.com/ejfkdev/udf/diskkit/backend"
	"github.com/ejfkdev/udf/diskkit/partition/gpt"
	"github.com/ejfkdev/udf/diskkit/partition/mbr"
)

// Read read a partition table from a disk
func Read(f backend.File, logicalBlocksize, physicalBlocksize int) (Table, error) {
	// just try each type
	gptTable, err := gpt.Read(f, logicalBlocksize, physicalBlocksize)
	if err == nil {
		return gptTable, nil
	}
	mbrTable, err := mbr.Read(f, logicalBlocksize, physicalBlocksize)
	if err == nil {
		return mbrTable, nil
	}
	// we are out
	return nil, fmt.Errorf("unknown disk partition type")
}
