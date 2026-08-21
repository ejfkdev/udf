package fsview

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Kind describes how a node is materialized on disk.
type Kind uint8

const (
	KindDir Kind = iota
	KindFile
	KindSymlink
	KindHardlink
)

const opaqueWhiteout = ".wh..wh..opq"

// Node is a single entry in the merged filesystem view of an image.
type Node struct {
	Name      string // base name inside the tree
	Path      string // slash-separated path relative to the tree root; empty for the root itself
	EntryName string // original entry name in the layer tar that defined this node
	Kind      Kind
	Size      int64
	Mode      int64 // raw tar mode, permission bits plus special bits
	ModTime   time.Time
	UID       int
	GID       int
	Uname     string
	Gname     string
	Linkname  string // symlink target, or hardlink source path inside the tree
	Layer     string // archive entry name of the layer that last defined this node
	Children  []*Node
}

// Build merges every layer in order into a single in-memory tree, applying
// whiteout and opaque-directory semantics the same way on-disk extraction does.
func Build(layerOrder []string, open func(name string) (io.Reader, func(), error)) (*Node, error) {
	root := &Node{Kind: KindDir, Mode: 0o755}

	for _, layerName := range layerOrder {
		if err := mergeLayer(root, layerName, open); err != nil {
			return nil, err
		}
	}

	return root, nil
}

func mergeLayer(root *Node, layerName string, open func(name string) (io.Reader, func(), error)) error {
	r, closeFn, err := open(layerName)
	if err != nil {
		return fmt.Errorf("open layer %s: %w", layerName, err)
	}
	defer closeFn()

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read layer %s: %w", layerName, err)
		}
		if err := mergeEntry(root, layerName, hdr); err != nil {
			return fmt.Errorf("merge layer %s: %w", layerName, err)
		}
	}
}

func mergeEntry(root *Node, layerName string, hdr *tar.Header) error {
	clean, err := CleanEntryName(hdr.Name)
	if err != nil {
		return err
	}
	if clean == "" {
		return nil
	}

	parent, err := ensureDir(root, path.Dir(clean), layerName)
	if err != nil {
		return err
	}

	base := path.Base(clean)
	if strings.HasPrefix(base, ".wh.") {
		if base == opaqueWhiteout {
			parent.Children = nil
			return nil
		}
		parent.removeChild(strings.TrimPrefix(base, ".wh."))
		return nil
	}

	kind, err := classifyType(hdr)
	if err != nil {
		return err
	}

	node := &Node{
		Name:      base,
		Path:      joinRel(parent.Path, base),
		EntryName: hdr.Name,
		Kind:      kind,
		Size:      hdr.Size,
		Mode:      hdr.Mode,
		ModTime:   hdr.ModTime,
		UID:       hdr.Uid,
		GID:       hdr.Gid,
		Uname:     hdr.Uname,
		Gname:     hdr.Gname,
		Linkname:  hdr.Linkname,
		Layer:     layerName,
	}
	parent.insert(node)
	return nil
}

func classifyType(hdr *tar.Header) (Kind, error) {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return KindDir, nil
	case tar.TypeReg, tar.TypeRegA:
		return KindFile, nil
	case tar.TypeSymlink:
		return KindSymlink, nil
	case tar.TypeLink:
		return KindHardlink, nil
	default:
		return 0, fmt.Errorf("unsupported tar entry type %q for %s", hdr.Typeflag, hdr.Name)
	}
}

// CleanEntryName turns a raw tar entry name into a clean slash-separated path
// relative to the tree root, rejecting names that escape the root.
func CleanEntryName(name string) (string, error) {
	trimmed := strings.TrimPrefix(name, "/")
	trimmed = strings.TrimPrefix(trimmed, "./")
	if trimmed == "" || trimmed == "." {
		return "", nil
	}

	clean := path.Clean(trimmed)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes root: %s", name)
	}
	return clean, nil
}

// NormalizePath converts a user-provided in-image path into a clean relative
// path: "" denotes the tree root itself.
func NormalizePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")

	clean := path.Clean(p)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes root: %s", p)
	}
	return clean, nil
}

// Resolve walks the tree following the given clean relative path and returns
// the matching node, or nil when any component does not exist. An empty or
// root path returns the tree root itself.
func (n *Node) Resolve(target string) *Node {
	clean, err := NormalizePath(target)
	if err != nil {
		return nil
	}
	if clean == "" {
		return n
	}

	cur := n
	for _, comp := range strings.Split(clean, "/") {
		var next *Node
		for _, child := range cur.Children {
			if child.Name == comp {
				next = child
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// SortedChildren returns the children in byte-wise name order, like ls.
func (n *Node) SortedChildren() []*Node {
	out := append([]*Node(nil), n.Children...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ensureDir walks the tree along rel, creating plain directories for any
// missing component. A non-directory node blocking the way is replaced by an
// implicit directory, mirroring how a later layer shadows it.
func ensureDir(root *Node, rel, layerName string) (*Node, error) {
	cur := root
	if rel == "." || rel == "" {
		return cur, nil
	}

	for _, comp := range strings.Split(rel, "/") {
		next := cur.child(comp)
		if next == nil {
			next = &Node{
				Name:  comp,
				Path:  joinRel(cur.Path, comp),
				Kind:  KindDir,
				Mode:  0o755,
				Layer: layerName,
			}
			cur.Children = append(cur.Children, next)
		} else if next.Kind != KindDir {
			*next = Node{
				Name:  comp,
				Path:  joinRel(cur.Path, comp),
				Kind:  KindDir,
				Mode:  0o755,
				Layer: layerName,
			}
		}
		cur = next
	}
	return cur, nil
}

func (n *Node) child(name string) *Node {
	for _, child := range n.Children {
		if child.Name == name {
			return child
		}
	}
	return nil
}

// insert places node under parent. A later layer's directory entry keeps the
// children accumulated from earlier layers and only refreshes metadata, while
// any other merge replaces the node (dropping the shadowed subtree).
func (p *Node) insert(node *Node) {
	for i, c := range p.Children {
		if c.Name != node.Name {
			continue
		}
		if node.Kind == KindDir && c.Kind == KindDir {
			c.Size = node.Size
			c.Mode = node.Mode
			c.ModTime = node.ModTime
			c.UID = node.UID
			c.GID = node.GID
			c.Uname = node.Uname
			c.Gname = node.Gname
			c.EntryName = node.EntryName
			c.Layer = node.Layer
			return
		}
		p.Children[i] = node
		return
	}
	p.Children = append(p.Children, node)
}

func (p *Node) removeChild(name string) {
	for i, c := range p.Children {
		if c.Name == name {
			p.Children = append(p.Children[:i], p.Children[i+1:]...)
			return
		}
	}
}

func joinRel(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
