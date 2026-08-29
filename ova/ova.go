package ova

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ejfkdev/udf/vmdk"
)

// Disk is one virtual disk inside an OVA or referenced by an OVF descriptor.
type Disk struct {
	Name string // vmdk basename
	vmdk *vmdk.Disk
	file *os.File // temp file (OVA) or the sibling .vmdk (standalone OVF)
	// remove reports whether file is a private temporary that should be deleted
	// on Close (true for OVA-extracted disks, false for standalone OVF disks).
	remove bool
}

// Image is an opened OVA archive.
type Image struct {
	Disks []*Disk
}

// namedDisk is an extracted vmdk awaiting vmdk.Open.
type namedDisk struct {
	name string
	file *os.File
}

// OpenFile opens the .ova archive at path, extracting each contained .vmdk.
func OpenFile(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var disks []*namedDisk

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			closeTempDisks(disks)
			return nil, fmt.Errorf("read ova tar: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if !strings.HasSuffix(strings.ToLower(name), ".vmdk") {
			continue
		}

		tmp, err := os.CreateTemp("", "udf-ova-*.vmdk")
		if err != nil {
			closeTempDisks(disks)
			return nil, err
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			closeTempDisks(disks)
			return nil, fmt.Errorf("extract %s: %w", name, err)
		}
		disks = append(disks, &namedDisk{name: filepath.Base(name), file: tmp})
	}

	sort.SliceStable(disks, func(i, j int) bool { return disks[i].name < disks[j].name })

	img := &Image{}
	for _, nd := range disks {
		d, err := vmdk.Open(nd.file)
		if err != nil {
			closeTempDisks(disks)
			img.Close()
			return nil, fmt.Errorf("open %s: %w", nd.name, err)
		}
		img.Disks = append(img.Disks, &Disk{Name: nd.name, vmdk: d, file: nd.file, remove: true})
	}
	return img, nil
}

// ReadAt delegates to the underlying VMDK disk.
func (d *Disk) ReadAt(p []byte, off int64) (int, error) { return d.vmdk.ReadAt(p, off) }

// Size returns the virtual disk size in bytes.
func (d *Disk) Size() int64 { return d.vmdk.Size() }

// Close releases the file backing this disk, deleting it when it is a private
// temporary (OVA).
func (d *Disk) Close() error {
	var err error
	if d.file != nil {
		if cerr := d.file.Close(); cerr != nil {
			err = cerr
		}
		if d.remove {
			if rerr := os.Remove(d.file.Name()); rerr != nil && !os.IsNotExist(rerr) && err == nil {
				err = rerr
			}
		}
		d.file = nil
	}
	return err
}

// Close releases every disk.
func (img *Image) Close() error {
	var err error
	for _, d := range img.Disks {
		if cerr := d.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

func closeTempDisks(disks []*namedDisk) {
	for _, d := range disks {
		_ = d.file.Close()
		_ = os.Remove(d.file.Name())
	}
}
