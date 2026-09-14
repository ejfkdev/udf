package archive

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ejfkdev/udf/fsview"
	rpmutils "github.com/sassoftware/go-rpmutils"
)

// rpmMagic is the RPM lead signature at offset 0.
const rpmMagic = "\xed\xab\xee\xdb"

// rpmKind maps an RPM file mode (a Unix stat mode with S_IFMT type bits) to the
// tree kind used by the listing/reader.
func rpmKind(mode int) fsview.Kind {
	switch mode & 0o170000 {
	case 0o040000:
		return fsview.KindDir
	case 0o120000:
		return fsview.KindSymlink
	case 0o020000:
		return fsview.KindCharDev
	case 0o060000:
		return fsview.KindBlockDev
	case 0o010000:
		return fsview.KindFifo
	default:
		return fsview.KindFile
	}
}

// rpmNameKey normalizes an RPM file name so header names ("/usr/bin/x") and
// cpio payload names ("./usr/bin/x") compare equal.
func rpmNameKey(s string) string {
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	return s
}

type rpmArchive struct {
	path string
}

func (a *rpmArchive) List() ([]Entry, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rpm, err := rpmutils.ReadRpm(f)
	if err != nil {
		return nil, err
	}
	files, err := rpm.Header.GetFiles()
	if err != nil {
		return nil, err
	}

	out := make([]Entry, 0, len(files))
	for _, fi := range files {
		kind := rpmKind(fi.Mode())
		out = append(out, Entry{
			Name:     fi.Name(),
			Size:     fi.Size(),
			Kind:     kind,
			Mode:     defaultMode(kind, int64(fi.Mode())&0o7777),
			ModTime:  time.Unix(int64(fi.Mtime()), 0),
			Linkname: fi.Linkname(),
		})
	}
	return out, nil
}

func (a *rpmArchive) Open(name string) (io.ReadCloser, int64, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, 0, err
	}
	rpm, err := rpmutils.ReadRpm(f)
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	pr, err := rpm.PayloadReaderExtended()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}

	target := rpmNameKey(name)
	for {
		fi, err := pr.Next()
		if err == io.EOF {
			_ = f.Close()
			return nil, 0, fmt.Errorf("entry %s not found in archive", name)
		}
		if err != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("read payload: %w", err)
		}
		if rpmNameKey(fi.Name()) != target {
			continue
		}
		return &rpmEntryReadCloser{pr: pr, f: f}, fi.Size(), nil
	}
}

type rpmEntryReadCloser struct {
	pr rpmutils.PayloadReader
	f  *os.File
}

func (r *rpmEntryReadCloser) Read(p []byte) (int, error) { return r.pr.Read(p) }
func (r *rpmEntryReadCloser) Close() error               { return r.f.Close() }
