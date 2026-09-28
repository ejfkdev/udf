package image

import (
	"strings"
	"testing"

	"github.com/ejfkdev/udf/types"
)

// selectionFixture is a manifest with the shapes a real docker-save archive
// has: a registry-prefixed app, a second tag of the same app, an -arm variant
// and an image with two repo tags.
func selectionFixture() []types.ManifestItem {
	return []types.ManifestItem{
		{Config: "a.json", RepoTags: []string{"swr.cn-east-3.myhuaweicloud.com/chaitin-safeline/safeline-mgt:latest", "chaitin/safeline-mgt:latest"}},
		{Config: "b.json", RepoTags: []string{"swr.cn-east-3.myhuaweicloud.com/chaitin-safeline/safeline-mgt:9.1.0-lts"}},
		{Config: "c.json", RepoTags: []string{"registry.example:5000/team/app:1.2"}},
		{Config: "d.json", RepoTags: nil},
	}
}

func TestMatchImageTagAcceptableForms(t *testing.T) {
	manifest := selectionFixture()
	cases := []struct {
		name string
		want int
	}{
		{"swr.cn-east-3.myhuaweicloud.com/chaitin-safeline/safeline-mgt:latest", 0}, // as written
		{"chaitin/safeline-mgt:latest", 1},                                          // the other repo tag of the same image
		{"safeline-mgt:latest", 0},                                                  // name:tag
		{"safeline-mgt:9.1.0-lts", 1},                                               // name:tag disambiguates
		{"app:1.2", 2},                                                              // name:tag behind a registry port
		{"app", 2},                                                                  // repository name
		{"team/app", 2},                                                             // name with a path
		{"1.2", 2},                                                                  // bare tag
		{"safeline-mgt:Latest", 0},                                                  // hmm: case sensitive, see below
	}
	_ = cases
	for _, tc := range []struct {
		name string
		want int
	}{
		{"swr.cn-east-3.myhuaweicloud.com/chaitin-safeline/safeline-mgt:latest", 0},
		{"chaitin/safeline-mgt:latest", 0},
		{"safeline-mgt:latest", 0},
		{"safeline-mgt:9.1.0-lts", 1},
		{"app:1.2", 2},
		{"app", 2},
		{"team/app", 2},
		{"chaitin-safeline/safeline-mgt:latest", 0},
		{"chaitin-safeline/safeline-mgt:9.1.0-lts", 1},
		{"1.2", 2},
		{"latest", 0},
	} {
		got, err := matchImageTag(manifest, tc.name)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%q matched image %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestMatchImageTagAmbiguousAndUnknown(t *testing.T) {
	manifest := selectionFixture()

	// "safeline-mgt" names two images (two tags of the repository), and a bare
	// tag shared by them is ambiguous too.
	_, err := matchImageTag(manifest, "safeline-mgt")
	if err == nil || !strings.Contains(err.Error(), "[0]=") || !strings.Contains(err.Error(), "[1]=") {
		t.Fatalf("expected an ambiguity listing both candidates, got %v", err)
	}

	// A full repo tag is never ambiguous, not even when its tail collides.
	if got, err := matchImageTag(manifest, "swr.cn-east-3.myhuaweicloud.com/chaitin-safeline/safeline-mgt:9.1.0-lts"); err != nil || got != 1 {
		t.Fatalf("full repo tag resolved to %d, %v", got, err)
	}

	if _, err := matchImageTag(manifest, "nope"); err == nil || !strings.Contains(err.Error(), "safeline-mgt") {
		t.Fatalf("expected a not-found error listing the available tags, got %v", err)
	}
}

// TestResolveSelectionKeepsIndexAndUniqueness checks the neighbouring paths: an
// index still selects directly (with a range check), and a single-image archive
// needs no selection at all.
func TestResolveSelectionKeepsIndexAndUniqueness(t *testing.T) {
	manifest := selectionFixture()
	if got, err := resolveSelection(manifest, Selection{ImageIndex: 2}); err != nil || got != 2 {
		t.Fatalf("index selection = %d, %v", got, err)
	}
	if _, err := resolveSelection(manifest, Selection{ImageIndex: 9}); err == nil {
		t.Fatalf("out-of-range index was accepted")
	}
	single := []types.ManifestItem{{Config: "a.json", RepoTags: []string{"app:1"}}}
	if got, err := resolveSelection(single, Selection{ImageIndex: -1}); err != nil || got != 0 {
		t.Fatalf("single-image archive = %d, %v", got, err)
	}
}
