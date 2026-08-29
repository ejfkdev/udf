package image

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	libvhdi "github.com/aoiflux/libvhdi"
	diskxfs "github.com/carbon-os/diskimg/xfs"
	"github.com/ejfkdev/udf/diskkit"
	"github.com/ejfkdev/udf/diskkit/backend"
	"github.com/ejfkdev/udf/diskkit/filesystem"
	"github.com/ejfkdev/udf/diskkit/filesystem/ext4"
	"github.com/ejfkdev/udf/diskkit/filesystem/fat12"
	"github.com/ejfkdev/udf/diskkit/filesystem/fat16"
	"github.com/ejfkdev/udf/diskkit/filesystem/fat32"
	"github.com/ejfkdev/udf/diskkit/filesystem/iso9660"
	"github.com/ejfkdev/udf/diskkit/filesystem/squashfs"
	qcow2reader "github.com/lima-vm/go-qcow2reader"
	qcow2fmt "github.com/lima-vm/go-qcow2reader/image/qcow2"
	sif "github.com/sylabs/sif/v2/pkg/sif"

	"github.com/ejfkdev/udf/erofs"
	"github.com/ejfkdev/udf/fsutil"
	"github.com/ejfkdev/udf/fsview"
	appi18n "github.com/ejfkdev/udf/i18n"
	"github.com/ejfkdev/udf/lvm2"
	"github.com/ejfkdev/udf/ova"
	"github.com/ejfkdev/udf/parallels"
	"github.com/ejfkdev/udf/qcow"
	"github.com/ejfkdev/udf/qed"
	"github.com/ejfkdev/udf/udffs"
	"github.com/ejfkdev/udf/vdi"
	"github.com/ejfkdev/udf/vma"
	"github.com/ejfkdev/udf/vmdk"
)

// IsDiskImage reports whether path is a virtual disk image (qcow2/vmdk/vhd/
// vhdx/vdi/parallels/qed/qcow1/vma/sif/ova/ovf/ffu/wim/esd/swm) or a raw
// filesystem image (ext4/xfs/squashfs/ISO9660/UDF/exFAT/EROFS/FAT). Detection
// is content based (file magic), not by filename extension. Disk images are a
// different input class from the tar/zip archives handled elsewhere.
func IsDiskImage(path string) bool {
	if c, err := detectDiskContainer(path); err == nil && c != "" {
		return true
	}
	if a, err := detectArchive(path); err == nil && a != "" {
		return false
	}
	return detectRawFilesystem(path)
}

// DiskVolume is one extractable region on a disk image: an MBR/GPT partition
// or an LVM logical volume.
type DiskVolume struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // "partition" | "lvm" | "disk"
	Start  int64  `json:"start"`
	Size   int64  `json:"size"`
	FSType string `json:"fs_type,omitempty"` // "ext4" | "xfs"
}

// String renders a compact "name(fs)" form so CLI listings stay legible.
func (v DiskVolume) String() string {
	if v.FSType == "" {
		return v.Name
	}
	return v.Name + "(" + v.FSType + ")"
}

// DiskInfo describes one disk inside an opened image: a qcow2/vmdk file, or a
// single .vmdk extracted from an OVA.
type DiskInfo struct {
	Name        string       `json:"name"`
	Format      string       `json:"format"`
	VirtualSize int64        `json:"virtual_size"`
	Filesystem  string       `json:"filesystem,omitempty"`
	Volume      string       `json:"volume,omitempty"`
	Volumes     []DiskVolume `json:"volumes,omitempty"`
}

// DiskMetadata summarizes a disk image: one or more disks, each with its
// partitions and LVM logical volumes.
type DiskMetadata struct {
	Disks []DiskInfo `json:"disks"`
}

// diskBackend adapts a read-only disk image (qcow2 via go-qcow2reader, or a
// VMDK/OVA disk) to the backend.Storage interface used by go-diskfs, and the
// io.ReaderAt used by the LVM and XFS readers. Only random-access reads are
// supported.
type diskBackend struct {
	ra     io.ReaderAt
	parts  []io.ReaderAt // non-nil for a split WIM; parts[0] == ra
	size   int64
	path   string
	pos    int64
	format string
	name   string
}

// openDisks opens path and returns every disk it contains — one for a qcow2 or
// standalone vmdk file, one per .vmdk for an OVA — plus a single close
// function that releases every underlying resource.
func openDisks(path string) ([]*diskBackend, func() error, error) {
	format, err := detectDiskContainer(path)
	if err != nil {
		return nil, nil, err
	}
	switch format {
	case "ova":
		img, err := ova.OpenFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("open ova %s: %w", path, err)
		}
		if len(img.Disks) == 0 {
			_ = img.Close()
			return nil, nil, fmt.Errorf("no vmdk disk found in ova %s", path)
		}
		out := make([]*diskBackend, 0, len(img.Disks))
		for _, d := range img.Disks {
			out = append(out, &diskBackend{
				ra:     d,
				size:   d.Size(),
				path:   path,
				format: "vmdk",
				name:   d.Name,
			})
		}
		return out, img.Close, nil
	case "ovf":
		img, err := ova.OpenOVF(path)
		if err != nil {
			return nil, nil, fmt.Errorf("open ovf %s: %w", path, err)
		}
		if len(img.Disks) == 0 {
			_ = img.Close()
			return nil, nil, fmt.Errorf("no vmdk disk referenced by ovf %s", path)
		}
		out := make([]*diskBackend, 0, len(img.Disks))
		for _, d := range img.Disks {
			out = append(out, &diskBackend{
				ra:     d,
				size:   d.Size(),
				path:   path,
				format: "vmdk",
				name:   d.Name,
			})
		}
		return out, img.Close, nil
	case "vhd", "vhdx":
		d, err := libvhdi.OpenFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("open vhd %s: %w", path, err)
		}
		return []*diskBackend{{
			ra:     d,
			size:   int64(d.Size()),
			path:   path,
			format: format,
			name:   filepath.Base(path),
		}}, d.Close, nil
	case "vma":
		img, err := vma.OpenFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("open vma %s: %w", path, err)
		}
		if len(img.Disks) == 0 {
			_ = img.Close()
			return nil, nil, fmt.Errorf("no disk found in vma %s", path)
		}
		out := make([]*diskBackend, 0, len(img.Disks))
		for _, d := range img.Disks {
			out = append(out, &diskBackend{
				ra:     d,
				size:   d.Size(),
				path:   path,
				format: "vma",
				name:   d.Name,
			})
		}
		return out, img.Close, nil
	case "vmexport":
		return openVMExport(path)
	case "sif":
		return openSIF(path)
	case "vmdk":
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		d, err := vmdk.Open(f)
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("open vmdk %s: %w", path, err)
		}
		return []*diskBackend{{
			ra:     d,
			size:   d.Size(),
			path:   path,
			format: "vmdk",
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	case "vdi":
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		d, err := vdi.Open(f)
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("open vdi %s: %w", path, err)
		}
		return []*diskBackend{{
			ra:     d,
			size:   d.Size(),
			path:   path,
			format: "vdi",
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	case "parallels":
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		d, err := parallels.Open(f)
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("open parallels disk %s: %w", path, err)
		}
		return []*diskBackend{{
			ra:     d,
			size:   d.Size(),
			path:   path,
			format: "parallels",
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	case "qed":
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		d, err := qed.Open(f)
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("open qed disk %s: %w", path, err)
		}
		return []*diskBackend{{
			ra:     d,
			size:   d.Size(),
			path:   path,
			format: "qed",
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	case "qcow1":
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		d, err := qcow.Open(f)
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("open qcow disk %s: %w", path, err)
		}
		return []*diskBackend{{
			ra:     d,
			size:   d.Size(),
			path:   path,
			format: "qcow1",
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	case "wim", "esd":
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		if !isWIMAt(f) {
			_ = f.Close()
			return nil, nil, fmt.Errorf("%s is not a WIM image", filepath.Base(path))
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		return []*diskBackend{{
			ra:     f,
			size:   st.Size(),
			path:   path,
			format: format,
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	case "swm":
		parts, closeAll, err := openSWMParts(path)
		if err != nil {
			return nil, nil, err
		}
		if !isWIMAt(parts[0]) {
			_ = closeAll()
			return nil, nil, fmt.Errorf("%s is not a WIM image", filepath.Base(path))
		}
		var total int64
		for _, p := range parts {
			if of, ok := p.(*os.File); ok {
				if fi, err := of.Stat(); err == nil {
					total += fi.Size()
				}
			}
		}
		return []*diskBackend{{
			ra:     parts[0],
			parts:  parts,
			size:   total,
			path:   path,
			format: "swm",
			name:   filepath.Base(path),
		}}, closeAll, nil
	case "ffu":
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		if !isFFU(f) {
			_ = f.Close()
			return nil, nil, fmt.Errorf("%s is not a Full Flash Update image", filepath.Base(path))
		}
		off, err := ffuStorageOffset(f, st.Size())
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("open ffu %s: %w", path, err)
		}
		ra := io.NewSectionReader(f, off, st.Size()-off)
		return []*diskBackend{{
			ra:     ra,
			size:   st.Size() - off,
			path:   path,
			format: "ffu",
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	case "qcow2":
		return openQcow2Backend(path)
	default:
		// A raw disk or a bare filesystem image: bytes are the device itself.
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		return []*diskBackend{{
			ra:     f,
			size:   st.Size(),
			path:   path,
			format: "raw",
			name:   filepath.Base(path),
		}}, func() error { return f.Close() }, nil
	}
}

// openSIF opens a Singularity container image and exposes its filesystem
// partition (the container root filesystem) as a raw filesystem image, which
// the regular squashfs/ext4 detection then picks up.
func openSIF(path string) ([]*diskBackend, func() error, error) {
	fimg, err := sif.LoadContainerFromPath(path, sif.OptLoadWithFlag(os.O_RDONLY))
	if err != nil {
		return nil, nil, fmt.Errorf("open sif %s: %w", path, err)
	}
	fail := func(err error) ([]*diskBackend, func() error, error) {
		_ = fimg.UnloadContainer()
		return nil, nil, err
	}

	descs, err := fimg.GetDescriptors(sif.WithDataType(sif.DataPartition))
	if err != nil || len(descs) == 0 {
		return fail(fmt.Errorf("no filesystem partition found in sif %s", path))
	}

	// Prefer the OS (system/primary-system) partition; skip encrypted and
	// overlay object archives we cannot extract.
	var pick sif.Descriptor
	bestRank := -1
	for _, d := range descs {
		fsType, pt, _, perr := d.PartitionMetadata()
		if perr != nil {
			continue
		}
		if fsType == sif.FsEncryptedSquashfs || fsType == sif.FsImmuObj {
			continue
		}
		rank := 0
		if pt == sif.PartSystem || pt == sif.PartPrimSys {
			rank = 1
		}
		if rank > bestRank {
			bestRank = rank
			pick = d
		}
	}
	if bestRank < 0 {
		return fail(fmt.Errorf("no readable filesystem partition found in sif %s", path))
	}

	ra, ok := pick.GetReader().(io.ReaderAt)
	if !ok {
		return fail(fmt.Errorf("sif partition reader is not random-access"))
	}

	return []*diskBackend{{
		ra:     ra,
		size:   pick.Size(),
		path:   path,
		format: "sif",
		name:   filepath.Base(path),
	}}, fimg.UnloadContainer, nil
}

func openQcow2Backend(path string) ([]*diskBackend, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}

	img, err := qcow2reader.Open(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open disk image %s: %w", path, err)
	}
	if err := img.Readable(); err != nil {
		_ = img.Close()
		return nil, nil, fmt.Errorf("disk image %s is not readable: %w", path, err)
	}
	if img.Type() != qcow2fmt.Type {
		_ = img.Close()
		return nil, nil, fmt.Errorf("%s is not a qcow2 image (detected %q)", filepath.Base(path), img.Type())
	}
	size := img.Size()
	if size <= 0 {
		_ = img.Close()
		return nil, nil, fmt.Errorf("disk image %s has an unknown virtual size", path)
	}

	return []*diskBackend{{
		ra:     img,
		size:   size,
		path:   path,
		format: "qcow2",
		name:   filepath.Base(path),
	}}, img.Close, nil
}

func (b *diskBackend) ReadAt(p []byte, off int64) (int, error) {
	n, err := b.ra.ReadAt(p, off)
	// Some readers report io.EOF whenever a read reaches the end of the
	// virtual disk, even when it filled the buffer entirely. Normalize that
	// case so ReadAt matches the standard io.ReaderAt contract.
	if err == io.EOF && n == len(p) {
		return n, nil
	}
	return n, err
}

func (b *diskBackend) Read(p []byte) (int, error) {
	if b.pos >= b.size {
		return 0, io.EOF
	}
	n, err := b.ra.ReadAt(p, b.pos)
	b.pos += int64(n)
	if err == io.EOF && n > 0 {
		return n, nil
	}
	return n, err
}

func (b *diskBackend) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.pos + offset
	case io.SeekEnd:
		abs = b.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	b.pos = abs
	return abs, nil
}

func (b *diskBackend) Stat() (os.FileInfo, error) {
	return diskFileInfo{name: filepath.Base(b.path), size: b.size}, nil
}

func (b *diskBackend) Close() error {
	// The owning diskImage closes the underlying readers once; a backend's own
	// Close is a no-op so a caller never double-closes a shared file.
	return nil
}

func (b *diskBackend) Sys() (*os.File, error) {
	return nil, errors.New("qcow2 image is not a block device")
}

func (b *diskBackend) Writable() (backend.WritableFile, error) {
	return nil, errors.New("qcow2 image is opened read-only")
}

func (b *diskBackend) Path() string {
	return b.path
}

type diskFileInfo struct {
	name string
	size int64
}

func (i diskFileInfo) Name() string       { return i.name }
func (i diskFileInfo) Size() int64        { return i.size }
func (i diskFileInfo) Mode() os.FileMode  { return 0 }
func (i diskFileInfo) ModTime() time.Time { return time.Time{} }
func (i diskFileInfo) IsDir() bool        { return false }
func (i diskFileInfo) Sys() any           { return nil }

// diskEntry describes one entry of a volume's filesystem in walk order
// (directories before their children).
type diskEntry struct {
	Rel        string // slash path relative to the fs root; "" is the root itself
	FSPath     string // path accepted by the filesystem reader (".", "etc", ...)
	Kind       fsview.Kind
	Mode       os.FileMode
	Size       int64
	ModTime    time.Time
	LinkTarget string
}

// diskVolumeReader is the read-only filesystem surface shared by the ext4 and
// xfs drivers. Paths use the io/fs convention: "." is the root and nested
// paths are slash-relative without a leading slash.
type diskVolumeReader interface {
	Open(name string) (fs.File, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Stat(name string) (fs.FileInfo, error) // lstat semantics: does not follow symlinks
	Readlink(name string) (string, error)
	Close() error
}

type ext4Reader struct{ fs *ext4.FileSystem }

func (r *ext4Reader) Open(name string) (fs.File, error)          { return r.fs.Open(name) }
func (r *ext4Reader) ReadDir(name string) ([]fs.DirEntry, error) { return r.fs.ReadDir(name) }
func (r *ext4Reader) Stat(name string) (fs.FileInfo, error)      { return r.fs.Stat(name) }
func (r *ext4Reader) Readlink(name string) (string, error)       { return r.fs.ReadLink(name) }
func (r *ext4Reader) Close() error                               { return r.fs.Close() }

type xfsReader struct{ v *diskxfs.Volume }

func (r *xfsReader) Open(name string) (fs.File, error)          { return r.v.Open(name) }
func (r *xfsReader) ReadDir(name string) ([]fs.DirEntry, error) { return r.v.ReadDir(name) }
func (r *xfsReader) Stat(name string) (fs.FileInfo, error)      { return r.v.Lstat(name) }
func (r *xfsReader) Readlink(name string) (string, error)       { return r.v.Readlink(name) }
func (r *xfsReader) Close() error                               { return r.v.Unmount() }

// region describes one candidate volume to probe for a filesystem.
type region struct {
	name  string
	kind  string
	start int64
	size  int64
	probe bool
}

// discoverVolumes enumerates one disk's partitions and LVM logical volumes,
// probing each for a supported filesystem. It returns the volumes and the
// disk's logical block size (needed later to open a filesystem reader).
func discoverVolumes(be *diskBackend) ([]DiskVolume, int64, error) {
	// WIM (including .esd/.swm, which are WIM with different compression or
	// split-parts) is a file-system archive, not a block device: it has no
	// partition table, so short-circuit the block-device path and expose a single
	// "wim" filesystem volume holding the whole image.
	if be.format == "wim" || be.format == "esd" || be.format == "swm" {
		return []DiskVolume{{Name: "wim", Kind: "disk", Start: 0, Size: be.size, FSType: "wim"}}, 0, nil
	}

	d, err := diskfs.OpenBackend(be, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, 0, fmt.Errorf("open disk %s: %w", be.name, err)
	}
	blocksize := d.LogicalBlocksize

	// First collect every candidate region (partitions, logical volumes, or the
	// whole disk) with its metadata; LVM parsing stays serial, then filesystem
	// detection runs in parallel.
	var regions []region

	extStarts := mbrExtendedStarts(be)

	if table, terr := d.GetPartitionTable(); terr == nil {
		logicalIndex := 4 // MBR logical partitions start at 5
		for _, p := range table.GetPartitions() {
			if p.GetSize() <= 0 {
				continue
			}
			start, size := p.GetStart(), p.GetSize()

			// An MBR extended partition is only a container for logical
			// partitions and has no filesystem of its own; expand it instead.
			if extStarts[start] {
				for _, lp := range walkEBR(be, start) {
					logicalIndex++
					regions = append(regions, region{name: fmt.Sprintf("p%d", logicalIndex), kind: "partition", start: lp.start, size: lp.size, probe: true})
				}
				continue
			}

			regions = append(regions, region{name: fmt.Sprintf("p%d", p.GetIndex()), kind: "partition", start: start, size: size, probe: true})

			vg, lerr := lvm2.Open(be, start)
			if lerr != nil {
				continue
			}
			for _, lv := range vg.Volumes {
				r := region{name: vg.Name + "/" + lv.Name, kind: "lvm", size: lv.Size}
				if len(lv.Extents) == 1 {
					r.start = start + lv.Extents[0].Start
					r.probe = true
				}
				regions = append(regions, r)
			}
		}
	} else {
		// No partition table: the filesystem (if any) covers the whole disk.
		regions = append(regions, region{name: "disk", kind: "disk", start: 0, size: d.Size, probe: true})
	}

	fstypes := probeFilesystemsParallel(be, regions, blocksize)

	vols := make([]DiskVolume, len(regions))
	for i, r := range regions {
		vols[i] = DiskVolume{Name: r.name, Kind: r.kind, Start: r.start, Size: r.size, FSType: fstypes[i]}
	}
	return vols, blocksize, nil
}

// mbrExtendedStarts returns the absolute byte offsets of MBR extended
// partitions (type 0x05/0x0f/0x85), keyed for O(1) lookup. A nil map means the
// disk has no MBR signature or no extended partition.
func mbrExtendedStarts(be io.ReaderAt) map[int64]bool {
	var b [512]byte
	if _, err := be.ReadAt(b[:], 0); err != nil {
		return nil
	}
	if b[510] != 0x55 || b[511] != 0xaa {
		return nil
	}
	out := map[int64]bool{}
	for i := 0; i < 4; i++ {
		e := b[446+i*16 : 446+i*16+16]
		switch e[4] {
		case 0x05, 0x0f, 0x85:
			out[int64(binary.LittleEndian.Uint32(e[8:12]))*512] = true
		}
	}
	return out
}

// ebrPartition is one logical partition found inside an MBR extended partition.
type ebrPartition struct{ start, size int64 }

// walkEBR walks the EBR chain of an MBR extended partition starting at
// extStart (absolute byte offset of the first EBR) and returns the logical
// partitions it contains. In each EBR, the first table entry is the logical
// partition (its start is relative to the EBR), and the second entry points to
// the next EBR (relative to the extended partition's start).
func walkEBR(be io.ReaderAt, extStart int64) []ebrPartition {
	extSector := extStart / 512
	var out []ebrPartition
	ebrSector := extSector
	for guard := 0; guard < 1024; guard++ {
		var b [512]byte
		if _, err := be.ReadAt(b[:], ebrSector*512); err != nil {
			break
		}
		if b[510] != 0x55 || b[511] != 0xaa {
			break
		}
		e0 := b[446:462] // logical partition entry
		e1 := b[462:478] // next EBR pointer entry

		if t := e0[4]; t != 0 {
			start := ebrSector + int64(binary.LittleEndian.Uint32(e0[8:12]))
			size := int64(binary.LittleEndian.Uint32(e0[12:16]))
			if size > 0 {
				out = append(out, ebrPartition{start: start * 512, size: size * 512})
			}
		}
		next := binary.LittleEndian.Uint32(e1[8:12])
		if next == 0 {
			break
		}
		ebrSector = extSector + int64(next)
	}
	return out
}

// region descriptors are cheap; detectFilesystem does the I/O.
func probeFilesystemsParallel(be *diskBackend, regions []region, blocksize int64) []string {
	out := make([]string, len(regions))
	if len(regions) == 0 {
		return out
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > len(regions) {
		workers = len(regions)
	}
	if workers > 16 {
		workers = 16
	}

	idx := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				if !regions[i].probe {
					continue
				}
				out[i] = detectFilesystem(be, regions[i].start, regions[i].size, blocksize)
			}
		}()
	}
	for i := range regions {
		idx <- i
	}
	close(idx)
	wg.Wait()
	return out
}

// diskImage is an opened disk image: one or more disks (a qcow2/vmdk file, or
// the .vmdk members of an OVA), each with its own set of discovered volumes.
type diskImage struct {
	disks      []*diskBackend
	blockSizes []int64
	vols       [][]DiskVolume
	closeFn    func() error
}

func openDiskImage(path string) (*diskImage, error) {
	disks, closeFn, err := openDisks(path)
	if err != nil {
		return nil, err
	}

	img := &diskImage{
		disks:      disks,
		blockSizes: make([]int64, len(disks)),
		vols:       make([][]DiskVolume, len(disks)),
		closeFn:    closeFn,
	}
	for i, be := range disks {
		vols, blocksize, err := discoverVolumes(be)
		if err != nil {
			_ = closeFn()
			return nil, err
		}
		img.vols[i] = vols
		img.blockSizes[i] = blocksize
	}
	return img, nil
}

func (img *diskImage) Close() error {
	if img.closeFn != nil {
		return img.closeFn()
	}
	return nil
}

// metadata builds the info-oriented view of the image.
func (img *diskImage) metadata() *DiskMetadata {
	meta := &DiskMetadata{Disks: make([]DiskInfo, 0, len(img.disks))}
	for i, be := range img.disks {
		di := DiskInfo{Name: be.name, Format: be.format, VirtualSize: be.size, Volumes: img.vols[i]}
		if withFS := fsVolumes(img.vols[i]); len(withFS) == 1 {
			di.Volume = withFS[0].Name
			di.Filesystem = withFS[0].FSType
		}
		meta.Disks = append(meta.Disks, di)
	}
	return meta
}

// detectFilesystem reports a volume's filesystem type by reading only its
// boot-block/superblock magic, so enumerating all of a disk's regions stays
// cheap. The filesystem is opened lazily in openVolumeReaderOn only when a
// caller lists or reads a path inside it.
func detectFilesystem(be *diskBackend, start, size, blocksize int64) string {
	var b [512]byte
	_, _ = be.ReadAt(b[:], start)

	switch {
	case hasPrefix(b[:], "hsqs") || hasPrefix(b[:], "sqsh"):
		return "squashfs"
	case hasPrefix(b[:], "XFSB"):
		return "xfs"
	case len(b) >= 11 && hasPrefix(b[3:], "EXFAT   "):
		return "exfat"
	}

	// ext2/3/4 superblock magic sits at offset 1080.
	var e [2]byte
	if _, err := be.ReadAt(e[:], start+1080); err == nil && binary.LittleEndian.Uint16(e[:]) == 0xEF53 {
		return "ext4"
	}

	// ISO9660 primary volume descriptor magic sits at offset 32769.
	var c [5]byte
	if _, err := be.ReadAt(c[:], start+32769); err == nil && string(c[:]) == "CD001" {
		return "iso9660"
	}

	if erofs.Detect(be, start) {
		return "erofs"
	}
	if udffs.Detect(be, start) {
		return "udf"
	}

	// FAT: boot jump plus a "FAT" label in either of the two BPB layouts
	// (FAT12/16 at offset 54, FAT32 at offset 82).
	if len(b) >= 84 && (b[0] == 0xEB || b[0] == 0xE9) && (hasPrefix(b[54:], "FAT") || hasPrefix(b[82:], "FAT")) {
		return detectFATType(be, start)
	}
	return ""
}

// detectFATType distinguishes FAT12/16/32 from the BIOS parameter block at base.
func detectFATType(be *diskBackend, base int64) string {
	var bpb [44]byte
	if _, err := be.ReadAt(bpb[:], base); err != nil {
		return "fat32"
	}
	bytesPerSector := int(binary.LittleEndian.Uint16(bpb[11:13]))
	if bytesPerSector == 0 {
		bytesPerSector = 512
	}
	sectorsPerCluster := int(bpb[13])
	reservedSec := int(binary.LittleEndian.Uint16(bpb[14:16]))
	numFATs := int(bpb[16])
	rootEntCnt := int(binary.LittleEndian.Uint16(bpb[17:19]))
	totSec16 := int(binary.LittleEndian.Uint16(bpb[19:21]))
	fatSz16 := int(binary.LittleEndian.Uint16(bpb[22:24]))
	totSec32 := int(binary.LittleEndian.Uint32(bpb[32:36]))
	fatSz32 := int(binary.LittleEndian.Uint32(bpb[36:40]))

	rootDirSec := (rootEntCnt*32 + bytesPerSector - 1) / bytesPerSector
	fatSz := fatSz16
	if fatSz == 0 {
		fatSz = fatSz32
	}
	totSec := totSec16
	if totSec == 0 {
		totSec = totSec32
	}
	dataSec := totSec - reservedSec - numFATs*fatSz - rootDirSec
	if sectorsPerCluster == 0 {
		return "fat32"
	}
	count := dataSec / sectorsPerCluster
	switch {
	case count < 4085:
		return "fat12"
	case count < 65525:
		return "fat16"
	default:
		return "fat32"
	}
}

// volumeIndex returns the index of a volume matching name (exact, or the bare
// base name of a logical volume), or -1.
func volumeIndex(vols []DiskVolume, name string) int {
	target := strings.ToLower(strings.TrimSpace(name))
	for i := range vols {
		if strings.ToLower(vols[i].Name) == target {
			return i
		}
	}
	for i := range vols {
		if strings.ToLower(pathpkg.Base(vols[i].Name)) == target {
			return i
		}
	}
	return -1
}

// virtualTarget is the result of resolving a virtual path: which layer it lands
// on, and (for a filesystem layer) which disk, volume and in-volume path.
type virtualTarget struct {
	level   string // "disks" | "volumes" | "fs"
	diskIdx int
	vol     *DiskVolume
	rel     string // in-volume path; "" is the volume root
}

// resolveVirtual maps a virtual path onto the image. The first path segment may
// name a disk; the next may name a volume; the remainder is a path inside that
// volume. A single disk or single filesystem volume is selected implicitly when
// the segment does not match, keeping simple images one-liner friendly; several
// candidates require an explicit prefix rather than a silent guess.
func (img *diskImage) resolveVirtual(vp string) (*virtualTarget, error) {
	rel, err := fsview.NormalizePath(vp)
	if err != nil {
		return nil, err
	}
	var segs []string
	if rel != "" {
		segs = strings.Split(rel, "/")
	}

	diskIdx := 0
	consumedDisk := false
	if len(segs) > 0 {
		if idx := img.diskIndex(segs[0]); idx >= 0 {
			diskIdx = idx
			segs = segs[1:]
			consumedDisk = true
		} else if len(img.disks) > 1 {
			return nil, fmt.Errorf("this image has %d disks; start the path with one of: %s", len(img.disks), img.diskChoices())
		}
	}

	if len(segs) == 0 {
		if consumedDisk || len(img.disks) == 1 {
			return &virtualTarget{level: "volumes", diskIdx: diskIdx}, nil
		}
		return &virtualTarget{level: "disks"}, nil
	}

	fsVols := fsVolumes(img.vols[diskIdx])
	if idx, consumed := matchVolumePrefix(fsVols, segs); idx >= 0 {
		return &virtualTarget{level: "fs", diskIdx: diskIdx, vol: &fsVols[idx], rel: strings.Join(segs[consumed:], "/")}, nil
	}
	if len(fsVols) == 1 {
		return &virtualTarget{level: "fs", diskIdx: diskIdx, vol: &fsVols[0], rel: strings.Join(segs, "/")}, nil
	}
	return nil, fmt.Errorf("this disk has %d filesystem volumes; start the path with one of: %s", len(fsVols), formatVolumeChoices(fsVols))
}

// matchVolumePrefix matches the longest prefix of segs (joined with "/") that
// names a volume — volumes like "vg1/root" span two segments. It returns the
// volume's index and how many segments were consumed.
func matchVolumePrefix(vols []DiskVolume, segs []string) (int, int) {
	for k := len(segs); k >= 1; k-- {
		if idx := volumeIndex(vols, strings.Join(segs[:k], "/")); idx >= 0 {
			return idx, k
		}
	}
	return -1, 0
}

func (img *diskImage) diskIndex(name string) int {
	target := strings.ToLower(strings.TrimSpace(name))
	for i, be := range img.disks {
		if strings.ToLower(be.name) == target {
			return i
		}
		if strings.ToLower(strings.TrimSuffix(be.name, filepath.Ext(be.name))) == target {
			return i
		}
	}
	return -1
}

func (img *diskImage) diskChoices() string {
	names := make([]string, len(img.disks))
	for i, be := range img.disks {
		names[i] = be.name
	}
	return strings.Join(names, ", ")
}

func fsVolumes(vols []DiskVolume) []DiskVolume {
	out := make([]DiskVolume, 0, len(vols))
	for _, v := range vols {
		if v.FSType != "" {
			out = append(out, v)
		}
	}
	return out
}

func formatVolumeChoices(vols []DiskVolume) string {
	choices := make([]string, 0, len(vols))
	for _, v := range vols {
		choices = append(choices, v.String())
	}
	return strings.Join(choices, ", ")
}

// ScanDiskMetadata inspects a disk image without extracting anything.
func ScanDiskMetadata(path string) (*DiskMetadata, error) {
	key := cacheKeyFor(path, "disks")
	var cached DiskMetadata
	if loadCachedJSON(key, &cached) {
		return &cached, nil
	}

	img, err := openDiskImage(path)
	if err != nil {
		return nil, err
	}
	defer img.Close()
	meta := img.metadata()
	storeCachedJSON(key, meta)
	return meta, nil
}

// ListDisk lists entries at virtual path vp: the disks when vp is empty on a
// multi-disk image, the volumes for a single disk, or a directory/file inside
// a volume.
func ListDisk(path, vp string) ([]FileEntry, error) {
	key := cacheKeyFor(path, "ls", vp)
	var cached []FileEntry
	if loadCachedJSON(key, &cached) {
		return cached, nil
	}

	img, err := openDiskImage(path)
	if err != nil {
		return nil, err
	}
	defer img.Close()
	entries, err := img.list(vp)
	if err != nil {
		return nil, err
	}
	storeCachedJSON(key, entries)
	return entries, nil
}

// ExtractDiskPath copies the file or directory at virtual path vp out to dest.
func ExtractDiskPath(path, vp, destPath string, bufferSize int) (int, error) {
	img, err := openDiskImage(path)
	if err != nil {
		return 0, err
	}
	defer img.Close()
	return img.extract(vp, destPath, bufferSize)
}

// ExtractDiskVolumes extracts every filesystem volume in the image. With
// several volumes each lands in its own subdirectory under outputDir; a single
// volume goes straight into outputDir. It returns the extracted volume names.
func ExtractDiskVolumes(path, outputDir string, bufferSize int) ([]string, error) {
	img, err := openDiskImage(path)
	if err != nil {
		return nil, err
	}
	defer img.Close()

	total := 0
	for di := range img.disks {
		total += len(fsVolumes(img.vols[di]))
	}

	var names []string
	for di := range img.disks {
		for i := range img.vols[di] {
			vol := &img.vols[di][i]
			if vol.FSType == "" {
				continue
			}
			target := outputDir
			if total > 1 {
				target = filepath.Join(outputDir, sanitizeVolumeName(vol.Name))
			}
			reader, closeFn, err := img.openVolumeReader(di, vol)
			if err != nil {
				return names, err
			}
			err = extractVolumeRoot(reader, target, bufferSize)
			closeFn()
			if err != nil {
				return names, err
			}
			names = append(names, vol.Name)
		}
	}
	return names, nil
}

// openVolumeReader opens the filesystem reader for one discovered volume.
func (img *diskImage) openVolumeReader(diskIdx int, vol *DiskVolume) (diskVolumeReader, func() error, error) {
	if vol.FSType == "" {
		return nil, nil, fmt.Errorf("volume %s has no supported filesystem", vol.Name)
	}
	reader, err := openVolumeReaderOn(img.disks[diskIdx], *vol, img.blockSizes[diskIdx])
	if err != nil {
		return nil, nil, err
	}
	return reader, func() error { return reader.Close() }, nil
}

func (img *diskImage) list(vp string) ([]FileEntry, error) {
	t, err := img.resolveVirtual(vp)
	if err != nil {
		return nil, err
	}
	switch t.level {
	case "disks":
		return diskFileEntries(img), nil
	case "volumes":
		return volumeFileEntries(img.vols[t.diskIdx]), nil
	default:
		reader, closeFn, err := img.openVolumeReader(t.diskIdx, t.vol)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return listVolume(reader, t.rel, vp)
	}
}

func (img *diskImage) extract(vp, destPath string, bufferSize int) (int, error) {
	t, err := img.resolveVirtual(vp)
	if err != nil {
		return 0, err
	}
	if t.level != "fs" {
		return 0, fmt.Errorf("path %q does not point into a filesystem; list the image first to see its disks and volumes", vp)
	}
	reader, closeFn, err := img.openVolumeReader(t.diskIdx, t.vol)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	return extractFromVolume(reader, t.rel, destPath, bufferSize, vp)
}

// listVolume lists the directory (or single file) at rel inside one volume.
func listVolume(reader diskVolumeReader, rel, displayPath string) ([]FileEntry, error) {
	e, ok, err := statDiskEntry(reader, rel)
	if err != nil || !ok {
		return nil, appi18n.NewError("err_ls_path_not_found", map[string]any{"Path": displayPath}, nil)
	}
	if e.Kind != fsview.KindDir {
		return []FileEntry{fileEntryFromDisk(e)}, nil
	}
	children, err := listDirChildEntries(reader, rel)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(children))
	for _, c := range children {
		out = append(out, fileEntryFromDisk(c))
	}
	return out, nil
}

// extractVolumeRoot extracts a whole volume's filesystem into target.
func extractVolumeRoot(reader diskVolumeReader, target string, bufferSize int) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	entries, err := collectSubtree(reader, "")
	if err != nil {
		return err
	}
	return extractDiskEntries(reader, entries, target, bufferSize)
}

// extractFromVolume copies a single file or directory from rel inside a volume
// to destPath, mirroring ExtractPath for archive images. It resolves only what
// it needs, so a single-file copy does not walk the whole filesystem.
func extractFromVolume(reader diskVolumeReader, srcRel, destPath string, bufferSize int, displayPath string) (int, error) {
	srcEntry, ok, err := statDiskEntry(reader, srcRel)
	if err != nil || !ok {
		return 0, appi18n.NewError("err_cp_src_not_found", map[string]any{"Path": displayPath}, nil)
	}

	buf := make([]byte, bufferSize)
	var dirs []fsutil.DirMetadata
	var count int
	writeOne := func(e diskEntry, target string) error {
		if err := writeDiskEntry(reader, e, target, buf, &dirs); err != nil {
			return err
		}
		count++
		return nil
	}

	switch {
	case srcRel == "":
		// Whole volume: contents land directly in destPath.
		if err := os.MkdirAll(destPath, 0o755); err != nil {
			return 0, err
		}
		entries, err := collectSubtree(reader, "")
		if err != nil {
			return count, err
		}
		for _, e := range entries {
			if e.Rel == "" {
				continue
			}
			target, err := fsutil.ResolveSafePath(destPath, e.Rel)
			if err != nil {
				return count, err
			}
			if err := writeOne(e, target); err != nil {
				return count, err
			}
		}
		return count, fsutil.ApplyDirMetadata(dirs)

	case srcEntry.Kind == fsview.KindDir:
		info, err := os.Stat(destPath)
		var destRoot string
		switch {
		case err == nil && info.IsDir():
			destRoot = filepath.Join(destPath, pathpkg.Base(srcRel))
		case err == nil:
			return 0, appi18n.NewError("err_cp_dest_conflict", map[string]any{"Path": destPath}, nil)
		case os.IsNotExist(err):
			destRoot = destPath
		default:
			return 0, err
		}

		entries, err := collectSubtree(reader, srcRel)
		if err != nil {
			return count, err
		}
		if err := os.MkdirAll(destRoot, 0o755); err != nil {
			return 0, err
		}
		for _, e := range entries {
			r := strings.TrimPrefix(e.Rel, srcRel)
			r = strings.TrimPrefix(r, "/")
			target, err := fsutil.ResolveSafePath(destRoot, r)
			if err != nil {
				return count, err
			}
			if err := writeOne(e, target); err != nil {
				return count, err
			}
		}
		return count, fsutil.ApplyDirMetadata(dirs)

	default:
		// Single file or symlink.
		info, err := os.Stat(destPath)
		var target string
		switch {
		case err == nil && info.IsDir():
			target = filepath.Join(destPath, pathpkg.Base(srcRel))
		case err == nil:
			target = destPath
		case os.IsNotExist(err):
			target = destPath
		default:
			return 0, err
		}
		if err := writeOne(srcEntry, target); err != nil {
			return count, err
		}
		return count, fsutil.ApplyDirMetadata(dirs)
	}
}

// diskFileEntries renders the top-level disk list as FileEntry rows.
func diskFileEntries(img *diskImage) []FileEntry {
	out := make([]FileEntry, 0, len(img.disks))
	for _, be := range img.disks {
		out = append(out, FileEntry{Name: be.name, Type: "disk", Size: be.size, FSType: be.format})
	}
	return out
}

// volumeFileEntries renders one disk's volumes as FileEntry rows.
func volumeFileEntries(vols []DiskVolume) []FileEntry {
	out := make([]FileEntry, 0, len(vols))
	for _, v := range vols {
		out = append(out, FileEntry{Name: v.Name, Type: v.Kind, Size: v.Size, FSType: v.FSType})
	}
	return out
}

// sanitizeVolumeName turns a volume name into a safe directory basename.
func sanitizeVolumeName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(name)
}

// openVolumeReaderOn opens the filesystem reader for a volume, keeping the
// backend name distinct from the method wrapper.
func openVolumeReaderOn(be *diskBackend, vol DiskVolume, blocksize int64) (diskVolumeReader, error) {
	switch vol.FSType {
	case "ext4":
		efs, err := ext4.Read(be, vol.Size, vol.Start, blocksize)
		if err != nil {
			return nil, fmt.Errorf("open ext4 volume %s: %w", vol.Name, err)
		}
		return &ext4Reader{fs: efs}, nil
	case "xfs":
		xv, err := diskxfs.Open(be, vol.Start, vol.Size)
		if err != nil {
			return nil, fmt.Errorf("open xfs volume %s: %w", vol.Name, err)
		}
		return &xfsReader{v: xv}, nil
	case "squashfs":
		sfs, err := squashfs.Read(be, vol.Size, vol.Start, blocksize)
		if err != nil {
			return nil, fmt.Errorf("open squashfs volume %s: %w", vol.Name, err)
		}
		return &gofsReader{fs: sfs, readlink: func(name string) (string, error) {
			info, err := sfs.Stat(name)
			if err != nil {
				return "", err
			}
			if st, ok := info.Sys().(*squashfs.StatT); ok && st.LinkTarget != "" {
				return st.LinkTarget, nil
			}
			return "", fmt.Errorf("%s is not a symlink", name)
		}}, nil
	case "iso9660":
		ifs, err := iso9660.Read(be, vol.Size, vol.Start, 0)
		if err != nil {
			return nil, fmt.Errorf("open iso9660 volume %s: %w", vol.Name, err)
		}
		return &gofsReader{fs: ifs, readlink: func(name string) (string, error) {
			info, err := ifs.Stat(name)
			if err != nil {
				return "", err
			}
			if st, ok := info.Sys().(*iso9660.StatT); ok && st.LinkTarget != "" {
				return st.LinkTarget, nil
			}
			return "", fmt.Errorf("%s is not a symlink", name)
		}}, nil
	case "udf":
		f, err := udffs.Open(be, vol.Start)
		if err != nil {
			return nil, fmt.Errorf("open udf volume %s: %w", vol.Name, err)
		}
		return f, nil
	case "erofs":
		f, err := erofs.Open(be, vol.Start)
		if err != nil {
			return nil, fmt.Errorf("open erofs volume %s: %w", vol.Name, err)
		}
		return f, nil
	case "exfat":
		rs := io.NewSectionReader(be, vol.Start, vol.Size)
		f, err := openExFATFS(rs)
		if err != nil {
			return nil, fmt.Errorf("open exfat volume %s: %w", vol.Name, err)
		}
		return f, nil
	case "wim":
		f, err := openWIMFS(be)
		if err != nil {
			return nil, fmt.Errorf("open wim image %s: %w", vol.Name, err)
		}
		return f, nil
	case "fat12", "fat16", "fat32":
		fs, err := openFAT(be, vol, blocksize)
		if err != nil {
			return nil, err
		}
		return &gofsReader{fs: fs}, nil
	default:
		return nil, fmt.Errorf("volume %s has unsupported filesystem", vol.Name)
	}
}

func openFAT(be *diskBackend, vol DiskVolume, blocksize int64) (filesystem.FileSystem, error) {
	switch vol.FSType {
	case "fat12":
		return fat12.Read(be, vol.Size, vol.Start, blocksize)
	case "fat16":
		return fat16.Read(be, vol.Size, vol.Start, blocksize)
	default:
		return fat32.Read(be, vol.Size, vol.Start, blocksize)
	}
}

// gofsReader adapts go-diskfs filesystems whose only shared surface is the
// filesystem.FileSystem interface (FAT, ISO9660, SquashFS). readlink is
// optional: filesystems without symlinks leave it nil.
type gofsReader struct {
	fs       filesystem.FileSystem
	readlink func(string) (string, error)
}

func (r *gofsReader) Open(name string) (fs.File, error)          { return r.fs.Open(name) }
func (r *gofsReader) ReadDir(name string) ([]fs.DirEntry, error) { return r.fs.ReadDir(name) }
func (r *gofsReader) Stat(name string) (fs.FileInfo, error)      { return r.fs.Stat(name) }
func (r *gofsReader) Readlink(name string) (string, error) {
	if r.readlink == nil {
		return "", fmt.Errorf("%s is not a symlink", name)
	}
	return r.readlink(name)
}
func (r *gofsReader) Close() error { return r.fs.Close() }

func fsPathFor(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

func joinRelPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// statDiskEntry resolves one path with lstat semantics and returns its
// diskEntry. The second return is false for special files (devices, FIFOs,
// sockets) which cannot be recreated during extraction.
func statDiskEntry(reader diskVolumeReader, rel string) (diskEntry, bool, error) {
	if rel == "" {
		return diskEntry{Rel: "", FSPath: ".", Kind: fsview.KindDir, Mode: 0o755}, true, nil
	}
	info, err := reader.Stat(fsPathFor(rel))
	if err != nil {
		return diskEntry{}, false, err
	}
	e := diskEntry{Rel: rel, FSPath: fsPathFor(rel), Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}
	switch {
	case info.Mode().IsDir():
		e.Kind = fsview.KindDir
	case info.Mode()&os.ModeSymlink != 0:
		e.Kind = fsview.KindSymlink
		e.LinkTarget, err = reader.Readlink(fsPathFor(rel))
		if err != nil {
			return diskEntry{}, false, err
		}
	case info.Mode().IsRegular():
		e.Kind = fsview.KindFile
	default:
		return diskEntry{}, false, nil
	}
	return e, true, nil
}

// listDirChildEntries lists the immediate children of a directory without
// recursing into subdirectories.
func listDirChildEntries(reader diskVolumeReader, rel string) ([]diskEntry, error) {
	dirents, err := reader.ReadDir(fsPathFor(rel))
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", rel, err)
	}
	out := make([]diskEntry, 0, len(dirents))
	for _, de := range dirents {
		child := joinRelPath(rel, de.Name())
		e, ok, err := statDiskEntry(reader, child)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// collectSubtree walks rel (a directory) and every descendant in pre-order,
// returning all entries including the directory itself. An empty rel walks the
// whole volume.
func collectSubtree(reader diskVolumeReader, rel string) ([]diskEntry, error) {
	root, ok, err := statDiskEntry(reader, rel)
	if err != nil {
		return nil, err
	}
	if !ok || root.Kind != fsview.KindDir {
		return nil, nil
	}

	entries := []diskEntry{root}
	var walk func(dirRel string) error
	walk = func(dirRel string) error {
		children, err := listDirChildEntries(reader, dirRel)
		if err != nil {
			return err
		}
		for _, child := range children {
			entries = append(entries, child)
			if child.Kind == fsview.KindDir {
				if err := walk(child.Rel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(rel); err != nil {
		return nil, err
	}
	return entries, nil
}

// fileEntryFromDisk renders a diskEntry as the structured FileEntry used by
// the ls command, reusing the shared formatting of the fsview layer.
func fileEntryFromDisk(e diskEntry) FileEntry {
	node := &fsview.Node{
		Name:     pathpkg.Base(e.Rel),
		Kind:     e.Kind,
		Mode:     rawMode(e.Mode),
		Size:     e.Size,
		ModTime:  e.ModTime,
		Linkname: e.LinkTarget,
	}
	return newFileEntry(node)
}

func extractDiskEntries(reader diskVolumeReader, entries []diskEntry, outputDir string, bufferSize int) error {
	// Directories are created first (so parents exist), then files and
	// symlinks are extracted concurrently: they are independent leaves.
	var dirs []fsutil.DirMetadata
	var leaves []diskEntry
	for _, e := range entries {
		if e.Rel == "" {
			continue
		}
		target, err := fsutil.ResolveSafePath(outputDir, e.Rel)
		if err != nil {
			return err
		}
		if e.Kind == fsview.KindDir {
			buf := make([]byte, bufferSize)
			if err := writeDiskEntry(reader, e, target, buf, &dirs); err != nil {
				return err
			}
		} else {
			leaves = append(leaves, e)
		}
	}

	if err := extractLeavesParallel(reader, leaves, outputDir, bufferSize); err != nil {
		return err
	}
	return fsutil.ApplyDirMetadata(dirs)
}

// extractLeavesParallel extracts regular files and symlinks concurrently. The
// container readers (qcow2 qlists, VMDK, ext4, …) are read-only and safe to
// read from multiple goroutines; each worker uses its own copy buffer.
func extractLeavesParallel(reader diskVolumeReader, entries []diskEntry, outputDir string, bufferSize int) error {
	if len(entries) == 0 {
		return nil
	}
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > len(entries) {
		workers = len(entries)
	}
	if workers > 8 {
		workers = 8
	}

	type job struct {
		e      diskEntry
		target string
	}
	jobs := make(chan job, len(entries))
	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, bufferSize)
			var dirs []fsutil.DirMetadata
			for j := range jobs {
				if err := writeDiskEntry(reader, j.e, j.target, buf, &dirs); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}

feedLoop:
	for _, e := range entries {
		target, err := fsutil.ResolveSafePath(outputDir, e.Rel)
		if err != nil {
			close(jobs)
			wg.Wait()
			return err
		}
		select {
		case jobs <- job{e: e, target: target}:
		default:
			// A worker already failed; stop feeding.
			close(jobs)
			break feedLoop
		}
	}
	close(jobs)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func buildDiskTree(entries []diskEntry) *fsview.Node {
	root := &fsview.Node{Kind: fsview.KindDir, Mode: 0o755}
	for _, e := range entries {
		if e.Rel == "" {
			root.Mode = rawMode(e.Mode)
			root.ModTime = e.ModTime
			continue
		}

		parent := ensureDiskDir(root, pathpkg.Dir(e.Rel))
		node := &fsview.Node{
			Name:     pathpkg.Base(e.Rel),
			Path:     e.Rel,
			Kind:     e.Kind,
			Size:     e.Size,
			Mode:     rawMode(e.Mode),
			ModTime:  e.ModTime,
			Linkname: e.LinkTarget,
		}
		parent.Children = append(parent.Children, node)
	}
	return root
}

func ensureDiskDir(root *fsview.Node, rel string) *fsview.Node {
	cur := root
	if rel == "." || rel == "" {
		return cur
	}
	for _, comp := range strings.Split(rel, "/") {
		var next *fsview.Node
		for _, child := range cur.Children {
			if child.Name == comp {
				next = child
				break
			}
		}
		if next == nil {
			next = &fsview.Node{Name: comp, Path: joinDiskRel(cur.Path, comp), Kind: fsview.KindDir, Mode: 0o755}
			cur.Children = append(cur.Children, next)
		} else if next.Kind != fsview.KindDir {
			*next = fsview.Node{Name: comp, Path: joinDiskRel(cur.Path, comp), Kind: fsview.KindDir, Mode: 0o755}
		}
		cur = next
	}
	return cur
}

func joinDiskRel(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// rawMode converts an os.FileMode into the raw tar-style mode bits used by the
// in-memory filesystem view (permission bits plus setuid/setgid/sticky).
func rawMode(mode os.FileMode) int64 {
	m := int64(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		m |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		m |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		m |= 0o1000
	}
	return m
}

func writeDiskEntry(reader diskVolumeReader, e diskEntry, target string, buf []byte, dirs *[]fsutil.DirMetadata) error {
	switch e.Kind {
	case fsview.KindDir:
		if err := fsutil.ReplaceWithDir(target, e.Mode); err != nil {
			return fmt.Errorf("create directory %s: %w", e.Rel, err)
		}
		*dirs = append(*dirs, fsutil.DirMetadata{Path: target, Mode: e.Mode, ModTime: e.ModTime})
	case fsview.KindFile:
		if err := writeDiskFile(reader, e.FSPath, target, e.Mode, e.ModTime, buf); err != nil {
			return fmt.Errorf("write file %s: %w", e.Rel, err)
		}
	case fsview.KindSymlink:
		if err := createDiskSymlink(target, e.LinkTarget); err != nil {
			return fmt.Errorf("create symlink %s: %w", e.Rel, err)
		}
	}
	return nil
}

func writeDiskFile(reader diskVolumeReader, srcPath, target string, mode os.FileMode, modTime time.Time, buf []byte) error {
	r, err := reader.Open(srcPath)
	if err != nil {
		return err
	}
	defer r.Close()

	if err := fsutil.EnsureParentDir(target); err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(f, r, buf); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(target, mode.Perm()); err != nil {
		return err
	}
	return os.Chtimes(target, modTime, modTime)
}

func createDiskSymlink(target, linkTarget string) error {
	if err := fsutil.EnsureParentDir(target); err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(linkTarget, target)
}
