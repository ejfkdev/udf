package archive

import (
	"os"
	"testing"
)

// TestPEResourceWalkRealExecutable checks the PE resource walker against a real
// Windows executable: set UDF_PE=path\to\app.exe (any PE with resources will
// do; udf itself does not need it to be py2exe).
func TestPEResourceWalkRealExecutable(t *testing.T) {
	path := os.Getenv("UDF_PE")
	if path == "" {
		t.Skip("set UDF_PE to a Windows executable to run this test")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	resources, err := peResourceList(f)
	if err != nil {
		t.Fatalf("peResourceList: %v", err)
	}
	if len(resources) == 0 {
		t.Fatal("no resources found in a real executable")
	}
	types := map[string]int{}
	for _, r := range resources {
		types[peTypeName(r)]++
		if r.size <= 0 {
			t.Errorf("%s has size %d", resourceEntryName(r), r.size)
		}
		if r.fileOff < 0 || r.fileOff+r.size > info.Size() {
			t.Errorf("%s range %d+%d outside a %d byte file", resourceEntryName(r), r.fileOff, r.size, info.Size())
		}
	}
	t.Logf("%d resources: %v", len(resources), types)
	if dlls, err := peImportedDLLs(f); err == nil {
		t.Logf("imports: %v", dlls)
	}
}
