package archive

import (
	"io"
	"testing"

	"github.com/ejfkdev/udf/fsview"
)

// The fixture is a macOS package written by pkgbuild itself (the system
// tool): a xar whose Payload member is a gzip-wrapped odc cpio. Its listing
// and file bytes were verified against the source directory before being
// checked in.
const pkgFixture = "testdata/test.pkg"

func TestPkgPayloadExpansion(t *testing.T) {
	xa := &xarArchive{path: pkgFixture}
	entries, err := xa.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	// The Payload itself stays listed, and its contents appear under it.
	if e, ok := byName["Payload"]; !ok || e.Kind != fsview.KindFile {
		t.Fatalf("Payload entry: %+v", byName["Payload"])
	}
	for _, want := range []string{"Payload/Library", "Payload/Library/Test/readme.txt"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("expanded entry %s missing: %+v", want, byName)
		}
	}

	rc, size, err := xa.Open("Payload/Library/Test/readme.txt")
	if err != nil {
		t.Fatalf("open member: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello from a real pkg\n" || size != int64(len(body)) {
		t.Fatalf("member = %q (%d bytes)", body, size)
	}

	if _, _, err := xa.Open("Payload/no/such/file"); err == nil {
		t.Fatal("opening a missing payload member succeeded")
	}
}
