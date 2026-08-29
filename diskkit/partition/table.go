package partition

import (
	"github.com/ejfkdev/udf/diskkit/backend"
	"github.com/ejfkdev/udf/diskkit/partition/part"
)

// Table reference to a partitioning table on disk
type Table interface {
	Type() string
	Write(backend.WritableFile, int64) error
	GetPartitions() []part.Partition
	Repair(diskSize uint64) error
	Verify(f backend.File, diskSize uint64) error
	UUID() string
}
