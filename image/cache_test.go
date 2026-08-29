package image

import (
	"reflect"
	"testing"
)

func TestScanImageMetadataCacheRoundTrip(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "root:x:0:0"),
		},
	})

	// First call populates the cache; second call should read it back.
	m1, err := ScanImageMetadata(f.ImagePath, Selection{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	m2, err := ScanImageMetadata(f.ImagePath, Selection{})
	if err != nil {
		t.Fatalf("scan cached: %v", err)
	}

	if m1.Index != m2.Index || m1.Total != m2.Total || m1.ConfigPath != m2.ConfigPath {
		t.Fatalf("metadata mismatch after cache: %+v vs %+v", m1, m2)
	}
	if !reflect.DeepEqual(m1.RepoTags, m2.RepoTags) || !reflect.DeepEqual(m1.LayerOrder, m2.LayerOrder) {
		t.Fatalf("metadata tags/layers mismatch after cache")
	}
	if !reflect.DeepEqual(m1.Config, m2.Config) {
		t.Fatalf("config mismatch after cache: %+v vs %+v", m1.Config, m2.Config)
	}
}

func TestListArchiveCache(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "root:x:0:0"),
			imgFile("readme.txt", "hi"),
		},
	})

	e1, err := ListArchive(f.ImagePath, Selection{}, "/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	e2, err := ListArchive(f.ImagePath, Selection{}, "/")
	if err != nil {
		t.Fatalf("list cached: %v", err)
	}
	if !reflect.DeepEqual(e1, e2) {
		t.Fatalf("listing mismatch after cache:\n%+v\nvs\n%+v", e1, e2)
	}

	names := map[string]bool{}
	for _, e := range e2 {
		names[e.Name] = true
	}
	if !names["etc"] || !names["readme.txt"] {
		t.Fatalf("expected etc and readme.txt in listing, got %v", names)
	}
}
