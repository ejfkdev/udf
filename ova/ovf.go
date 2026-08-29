package ova

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ejfkdev/udf/vmdk"
)

// OpenOVF opens a standalone OVF descriptor (.ovf) and the VMDK disk files it
// references from its own directory. Unlike an OVA (a tar), a standalone OVF
// references sibling files directly, so the disks are not copied to temporaries
// and Close leaves the user's files in place.
func OpenOVF(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hrefs, err := vmdkHrefs(f)
	if err != nil {
		return nil, err
	}

	img := &Image{}
	dir := filepath.Dir(path)
	for _, href := range hrefs {
		full := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(href, "./")))
		vf, err := os.Open(full)
		if err != nil {
			img.Close()
			return nil, fmt.Errorf("open referenced disk %s: %w", href, err)
		}
		d, err := vmdk.Open(vf)
		if err != nil {
			_ = vf.Close()
			img.Close()
			return nil, fmt.Errorf("open referenced disk %s: %w", href, err)
		}
		img.Disks = append(img.Disks, &Disk{Name: filepath.Base(full), vmdk: d, file: vf, remove: false})
	}
	if len(img.Disks) == 0 {
		img.Close()
		return nil, fmt.Errorf("no .vmdk disk referenced by OVF descriptor %s", filepath.Base(path))
	}
	return img, nil
}

// vmdkHrefs returns every href attribute (ovf:href) in the descriptor whose
// value names a .vmdk file, in sorted order.
func vmdkHrefs(r io.Reader) ([]string, error) {
	dec := xml.NewDecoder(r)
	var hrefs []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse ovf descriptor: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "href" {
				hrefs = append(hrefs, a.Value)
			}
		}
	}

	var out []string
	for _, h := range hrefs {
		if strings.HasSuffix(strings.ToLower(h), ".vmdk") {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}
