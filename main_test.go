package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	errs "github.com/ejfkdev/xyz-go/errors"

	"github.com/ejfkdev/udf/i18n"
	"github.com/ejfkdev/udf/types"
)

func writeTestImage(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.tar")
	f, err := os.Create(imagePath)
	if err != nil {
		t.Fatalf("create image tar: %v", err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)

	cfg, err := json.Marshal(types.ImageConfig{
		Architecture: "amd64",
		Config: struct {
			User         string         `json:"User"`
			Env          []string       `json:"Env"`
			Entrypoint   []string       `json:"Entrypoint"`
			Cmd          []string       `json:"Cmd"`
			WorkingDir   string         `json:"WorkingDir"`
			ExposedPorts map[string]any `json:"ExposedPorts"`
		}{WorkingDir: "/app"},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeTarEntry(t, tw, "config.json", cfg)

	layers := map[string][]tarEntry{
		"base.tar": {
			{Header: tarHeader("etc/", tar.TypeDir, 0o755, 0), Body: nil},
			{Header: tarHeader("etc/passwd", tar.TypeReg, 0o644, int64(len("root:x:0:0"))), Body: []byte("root:x:0:0")},
			{Header: tarHeader("etc/motd", tar.TypeReg, 0o644, int64(len("welcome"))), Body: []byte("welcome")},
		},
		"top.tar": {
			{Header: tarHeader("etc/.wh.motd", tar.TypeReg, 0, 0), Body: nil},
			{Header: tarHeader("usr/bin/tool", tar.TypeReg, 0o755, int64(len("#!/bin/sh"))), Body: []byte("#!/bin/sh")},
		},
	}
	item := types.ManifestItem{
		Config:   "config.json",
		RepoTags: []string{"test/app:latest"},
	}
	var layerNames []string
	for name := range layers {
		layerNames = append(layerNames, name)
	}
	sort.Strings(layerNames)
	for _, name := range layerNames {
		item.Layers = append(item.Layers, name)
		writeTarEntry(t, tw, name, buildLayer(t, layers[name]))
	}

	manifestBody, err := json.Marshal([]types.ManifestItem{item})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeTarEntry(t, tw, "manifest.json", manifestBody)

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return imagePath
}

type tarEntry struct {
	Header *tar.Header
	Body   []byte
}

func tarHeader(name string, typeflag byte, mode int64, size int64) *tar.Header {
	return &tar.Header{Name: name, Mode: mode, Size: size, Typeflag: typeflag,
		Uid: 0, Gid: 0, Uname: "root", Gname: "root"}
}

func buildLayer(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.Header); err != nil {
			t.Fatalf("write layer header %s: %v", entry.Header.Name, err)
		}
		if len(entry.Body) > 0 {
			if _, err := tw.Write(entry.Body); err != nil {
				t.Fatalf("write layer body %s: %v", entry.Header.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close layer writer: %v", err)
	}
	return buf.Bytes()
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write header %s: %v", name, err)
	}
	if len(body) > 0 {
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
}

func TestInfoImage(t *testing.T) {
	imagePath := writeTestImage(t)

	resp, err := infoImage(context.Background(), &InfoArgs{Archive: imagePath})
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if len(resp.Layers) != 2 || resp.Architecture != "amd64" || resp.WorkingDir != "/app" {
		t.Fatalf("unexpected info result: %+v", resp)
	}
	if len(resp.RepoTags) != 1 || resp.RepoTags[0] != "test/app:latest" {
		t.Fatalf("unexpected repo tags: %v", resp.RepoTags)
	}
}

func TestListImageAppliesWhiteout(t *testing.T) {
	imagePath := writeTestImage(t)

	entries, err := listImage(context.Background(), &LsArgs{Archive: imagePath, Path: "/etc"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["passwd"] {
		t.Fatalf("expected passwd in listing, got %v", names)
	}
	if names["motd"] {
		t.Fatalf("expected whiteout motd absent from listing, got %v", names)
	}
}

func TestListImageNotFoundKind(t *testing.T) {
	imagePath := writeTestImage(t)

	_, err := listImage(context.Background(), &LsArgs{Archive: imagePath, Path: "/nosuch"})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if got := errs.Classify(err); got != errs.KindNotFound {
		t.Fatalf("unexpected error kind: %v", got)
	}
}

func TestCopyEntryExtractsSingleFile(t *testing.T) {
	imagePath := writeTestImage(t)

	dest := filepath.Join(t.TempDir(), "passwd.out")
	resp, err := copyEntry(context.Background(), &CpArgs{
		Archive: imagePath, Source: "/etc/passwd", Dest: dest, BufferSize: 1 << 16,
	})
	if err != nil {
		t.Fatalf("cp: %v", err)
	}
	if resp.Extracted != 1 {
		t.Fatalf("unexpected extracted count: %d", resp.Extracted)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(data) != "root:x:0:0" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestExtractImages(t *testing.T) {
	imagePath := writeTestImage(t)
	out := t.TempDir()

	results, err := extractImages(context.Background(), &ExtractArgs{
		Archive: imagePath, Output: out, BufferSize: 1 << 16,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("unexpected result count: %d", len(results))
	}
	if results[0].Error != "" {
		t.Fatalf("unexpected per-archive error: %s", results[0].Error)
	}
	if _, err := os.ReadFile(filepath.Join(results[0].OutputDir, "etc", "passwd")); err != nil {
		t.Fatalf("expected extracted passwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(results[0].OutputDir, "etc", "motd")); !os.IsNotExist(err) {
		t.Fatalf("expected whiteout motd not extracted, stat err=%v", err)
	}
}

func TestSelectionConflictIsInvalidInput(t *testing.T) {
	imagePath := writeTestImage(t)

	_, err := infoImage(context.Background(), &InfoArgs{
		Archive: imagePath, RepoTag: "a:1", ImageIndex: 0,
	})
	if err == nil {
		t.Fatal("expected selection conflict error")
	}
	if got := errs.Classify(err); got != errs.KindInvalidInput {
		t.Fatalf("unexpected error kind: %v", got)
	}
}

func TestToXyzErrMapsLocalizedKeys(t *testing.T) {
	notFound := toXyzErr(i18n.NewError("err_cp_src_not_found", map[string]any{"Path": "/x"}, nil))
	if got := errs.Classify(notFound); got != errs.KindNotFound {
		t.Fatalf("unexpected kind: %v", got)
	}

	conflict := toXyzErr(i18n.NewError("err_cp_dest_conflict", map[string]any{"Path": "/x"}, nil))
	if got := errs.Classify(conflict); got != errs.KindConflict {
		t.Fatalf("unexpected kind: %v", got)
	}

	unknown := toXyzErr(i18n.NewError("err_something_else", nil, nil))
	if got := errs.Classify(unknown); got != errs.KindInvalidInput {
		t.Fatalf("unexpected kind: %v", got)
	}
}
