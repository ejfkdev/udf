package archive

import (
	"archive/zip"
	"io"
	"os"
	"strings"
	"testing"
)

// TestDecodeRealAPKResources decodes the resource table of a real APK. Point
// UDF_APK at an .apk (any modern app) to run it; without it the test skips.
//
// It checks the shape rather than exact values, because the table depends on
// the app: a package, thousands of resources, resolved type and name for the
// ones that are named, and references rendered by name rather than as hex.
func TestDecodeRealAPKResources(t *testing.T) {
	apk := os.Getenv("UDF_APK")
	if apk == "" {
		t.Skip("set UDF_APK to an .apk to decode a real resource table")
	}
	zr, err := zip.OpenReader(apk)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var data []byte
	for _, f := range zr.File {
		if f.Name == "resources.arsc" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if data == nil {
		t.Skip("no resources.arsc in that archive")
	}
	out, err := DecodeARSC(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(out, "# package 0x") {
		t.Fatalf("no package in the decoded table:\n%s", firstLines(out, 5))
	}
	lines := strings.Split(out, "\n")
	resources := 0
	named := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "0x") {
			continue
		}
		resources++
		if strings.Contains(line, "/") {
			named++
		}
	}
	if resources < 100 {
		t.Fatalf("only %d resources decoded, that is implausibly few", resources)
	}
	// Named entries dominate a real table; if none resolve, the type or key
	// string pools were not associated with the package.
	if named*2 < resources {
		t.Fatalf("%d of %d resources have no type/name: string pools are not wired up", resources-named, resources)
	}
	t.Logf("decoded %d resources (%d named) from %d bytes", resources, named, len(data))
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
