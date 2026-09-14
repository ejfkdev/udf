package image

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ejfkdev/udf/fsview"
	appi18n "github.com/ejfkdev/udf/i18n"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/layer"
	"github.com/ejfkdev/udf/types"
)

// BuildFileSystem merges all layers of the image into an in-memory tree
// without writing anything to disk.
func BuildFileSystem(imageTarPath string, meta *types.ImageMetadata) (*fsview.Node, error) {
	archive, err := arch.Open(imageTarPath)
	if err != nil {
		return nil, err
	}

	openLayer := func(layerName string) (io.Reader, func(), error) {
		rc, _, err := archive.Open(layerName)
		if err != nil {
			return nil, func() {}, err
		}
		return layer.OpenLayerReader(rc)
	}

	return fsview.Build(meta.LayerOrder, openLayer)
}

// ListArchive lists the directory (or single file) at treePath inside an
// archive image, caching the result so repeated listings skip re-scanning the
// archive and re-building the merged filesystem tree.
func ListArchive(imageTarPath string, sel Selection, treePath string) ([]FileEntry, error) {
	key := cacheKeyFor(imageTarPath, "arls", fmt.Sprintf("%d\x00%s", sel.ImageIndex, sel.RepoTag), treePath)
	var cached []FileEntry
	if loadCachedJSON(key, &cached) {
		return cached, nil
	}

	meta, err := ScanImageMetadata(imageTarPath, sel)
	if err != nil {
		return nil, err
	}
	tree, err := BuildFileSystem(imageTarPath, meta)
	if err != nil {
		return nil, err
	}
	entries, err := ListEntries(tree, treePath)
	if err != nil {
		return nil, err
	}
	storeCachedJSON(key, entries)
	return entries, nil
}

// FormatListing renders the merged image filesystem at target the way
// `ls -al` would: one long-format line per entry, plus a total block line
// for directories. target "" or "/" lists the image root.
func FormatListing(root *fsview.Node, target string) (string, error) {
	clean, err := fsview.NormalizePath(target)
	if err != nil {
		return "", err
	}

	shown := root
	if clean != "" {
		shown = root.Resolve(clean)
	}
	if shown == nil {
		return "", appi18n.NewError("err_ls_path_not_found", map[string]any{"Path": target}, nil)
	}

	if shown.Kind != fsview.KindDir {
		return formatLongLine(shown), nil
	}

	children := shown.SortedChildren()
	var b strings.Builder
	fmt.Fprintf(&b, "total %d\n", totalBlocks(children))
	for _, child := range children {
		b.WriteString(formatLongLine(child))
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func totalBlocks(children []*fsview.Node) int64 {
	var blocks int64
	for _, child := range children {
		blocks += (child.Size + 511) / 512
	}
	return blocks
}

func formatLongLine(n *fsview.Node) string {
	size := n.Size
	if n.Kind == fsview.KindSymlink {
		size = int64(len(n.Linkname))
	}

	name := n.Name
	if n.Kind == fsview.KindSymlink {
		name = fmt.Sprintf("%s -> %s", n.Name, n.Linkname)
	}

	owner := n.Uname
	if owner == "" {
		owner = strconv.Itoa(n.UID)
	}
	group := n.Gname
	if group == "" {
		group = strconv.Itoa(n.GID)
	}

	links := 1
	if n.Kind == fsview.KindDir {
		links = len(n.Children) + 2
	}

	sizeCol := fmt.Sprintf("%8d", size)
	if fsview.IsDeviceKind(n.Kind) && n.Kind != fsview.KindFifo {
		sizeCol = fmt.Sprintf("%8s", fmt.Sprintf("%d, %d", n.Devmajor, n.Devminor))
	}

	return fmt.Sprintf("%s %3d %-8s %-8s %s %s %s",
		modeString(n), links, owner, group, sizeCol, formatModTime(n.ModTime), name)
}

func modeString(n *fsview.Node) string {
	b := []byte("?---------")
	switch n.Kind {
	case fsview.KindDir:
		b[0] = 'd'
	case fsview.KindSymlink:
		b[0] = 'l'
	case fsview.KindCharDev:
		b[0] = 'c'
	case fsview.KindBlockDev:
		b[0] = 'b'
	case fsview.KindFifo:
		b[0] = 'p'
	case fsview.KindHardlink:
		b[0] = '-'
	default:
		b[0] = '-'
	}

	perm := "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if n.Mode&(1<<(8-i)) == 0 {
			b[1+i] = '-'
		} else {
			b[1+i] = perm[i]
		}
	}

	if n.Mode&0o4000 != 0 {
		if b[3] == 'x' {
			b[3] = 's'
		} else {
			b[3] = 'S'
		}
	}
	if n.Mode&0o2000 != 0 {
		if b[6] == 'x' {
			b[6] = 's'
		} else {
			b[6] = 'S'
		}
	}
	if n.Mode&0o1000 != 0 {
		if b[9] == 'x' {
			b[9] = 't'
		} else {
			b[9] = 'T'
		}
	}

	return string(b)
}

// FileEntry is a structured description of one entry in the merged image
// filesystem — the machine-friendly counterpart of a FormatListing line.
type FileEntry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Mode    string    `json:"mode"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Target  string    `json:"target,omitempty"`
	FSType  string    `json:"fs_type,omitempty"` // filesystem of a disk/volume row
}

// ListEntries resolves target inside the merged tree and returns every entry
// to display: the sorted children when target is a directory (or the image
// root), the entry itself otherwise.
func ListEntries(root *fsview.Node, target string) ([]FileEntry, error) {
	clean, err := fsview.NormalizePath(target)
	if err != nil {
		return nil, err
	}

	shown := root
	if clean != "" {
		shown = root.Resolve(clean)
	}
	if shown == nil {
		return nil, appi18n.NewError("err_ls_path_not_found", map[string]any{"Path": target}, nil)
	}

	if shown.Kind != fsview.KindDir {
		return []FileEntry{newFileEntry(shown)}, nil
	}

	children := shown.SortedChildren()
	out := make([]FileEntry, 0, len(children))
	for _, child := range children {
		out = append(out, newFileEntry(child))
	}
	return out, nil
}

func newFileEntry(n *fsview.Node) FileEntry {
	size := n.Size
	if n.Kind == fsview.KindSymlink {
		size = int64(len(n.Linkname))
	}
	entry := FileEntry{
		Name:    n.Name,
		Type:    kindName(n.Kind),
		Mode:    modeString(n),
		Size:    size,
		ModTime: n.ModTime,
	}
	if n.Kind == fsview.KindSymlink {
		entry.Target = n.Linkname
	}
	return entry
}

func kindName(kind fsview.Kind) string {
	switch kind {
	case fsview.KindDir:
		return "dir"
	case fsview.KindFile:
		return "file"
	case fsview.KindSymlink:
		return "symlink"
	case fsview.KindHardlink:
		return "hardlink"
	case fsview.KindCharDev:
		return "chardev"
	case fsview.KindBlockDev:
		return "blockdev"
	case fsview.KindFifo:
		return "fifo"
	default:
		return "unknown"
	}
}

func formatModTime(t time.Time) string {
	if t.IsZero() {
		return "Jan  1  1970"
	}
	const halfYear = 182 * 24 * time.Hour
	now := time.Now()
	if t.Before(now.Add(-halfYear)) || t.After(now.Add(halfYear)) {
		return t.Format("Jan _2  2006")
	}
	return t.Format("Jan _2 15:04")
}
