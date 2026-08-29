package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	errs "github.com/ejfkdev/xyz-go/errors"
	"github.com/ejfkdev/xyz-go/langx"

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

func TestIsSupportedArchive(t *testing.T) {
	write := func(pref []byte) string {
		p := filepath.Join(t.TempDir(), "input.bin")
		if err := os.WriteFile(p, pref, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, p := range map[string]string{
		"gzip":  write([]byte{0x1f, 0x8b}),
		"zip":   write([]byte("PK\x03\x04")),
		"7z":    write([]byte("7z\xbc\xaf\x27\x1c")),
		"rar":   write([]byte("Rar!\x1a\x07\x00")),
		"cpio":  write([]byte("070701")),
		"qcow2": write([]byte("QFI\xfb")),
		"vmdk":  write([]byte("KDMV")),
		"wim":   write([]byte("MSWIM\x00\x00\x00")),
	} {
		if !isSupportedArchive(p) {
			t.Fatalf("expected content %q to be a supported input", name)
		}
	}
	if isSupportedArchive(write([]byte("this is plain text\n"))) {
		t.Fatalf("plain text should not be a supported input")
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

func TestCatEntryStreamsToStdout(t *testing.T) {
	imagePath := writeTestImage(t)

	capture := filepath.Join(t.TempDir(), "cat.out")
	f, err := os.Create(capture)
	if err != nil {
		t.Fatalf("create stdout capture: %v", err)
	}

	old := os.Stdout
	os.Stdout = f
	resp, herr := catEntry(context.Background(), &CatArgs{
		Archive: imagePath, Source: "/etc/passwd", BufferSize: 1 << 16,
	})
	os.Stdout = old
	if err := f.Close(); err != nil {
		t.Fatalf("close stdout capture: %v", err)
	}

	if herr != nil {
		t.Fatalf("cat: %v", herr)
	}
	if resp != nil {
		t.Fatalf("cat must return a nil result so the CLI adds no framing, got %v", resp)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read stdout capture: %v", err)
	}
	// /etc/passwd in the fixture has no trailing newline; cat must reproduce
	// the exact bytes without appending one (unlike the CLI's string Render).
	if want := []byte("root:x:0:0"); !bytes.Equal(data, want) {
		t.Fatalf("stdout bytes mismatch: got %q want %q", data, want)
	}
}

func TestCatEntryNotFoundKind(t *testing.T) {
	imagePath := writeTestImage(t)

	_, err := catEntry(context.Background(), &CatArgs{
		Archive: imagePath, Source: "/nosuch", BufferSize: 1 << 16,
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if got := errs.Classify(err); got != errs.KindNotFound {
		t.Fatalf("unexpected error kind: %v", got)
	}
}

func TestHexDumpFormat(t *testing.T) {
	got := hexDump([]byte{0x4d, 0x5a, 0x00, 0x41, 0xff}, 0)
	want := "00000000:" + " 4d 5a 00 41 ff" + strings.Repeat("   ", 11) + "  " + "MZ.A."
	if got != want {
		t.Fatalf("unexpected hex dump:\ngot  %q\nwant %q", got, want)
	}
}

func TestHexDumpMultipleLinesAndOffset(t *testing.T) {
	data := make([]byte, 20)
	for i := range data {
		data[i] = byte('A' + i)
	}
	got := hexDump(data, 0x40)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 dump lines, got %d:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "00000040:") {
		t.Fatalf("first line should start at the base offset, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "00000050:") {
		t.Fatalf("second line should continue at +16, got %q", lines[1])
	}
}

func TestXxdEntryHexDumps(t *testing.T) {
	imagePath := writeTestImage(t)

	out, err := xxdEntry(context.Background(), &XxdArgs{
		Archive: imagePath, Source: "/etc/passwd", Bytes: 256, Offset: 0,
	})
	if err != nil {
		t.Fatalf("xxd: %v", err)
	}
	if !strings.HasPrefix(out, "00000000:") {
		t.Fatalf("dump should start with offset 00000000:, got %q", out)
	}
	if !strings.Contains(out, "72 6f 6f 74") { // hex of "root"
		t.Fatalf("dump should contain hex of 'root', got:\n%s", out)
	}
	if !strings.Contains(out, "root:x:0:0") { // ASCII gutter
		t.Fatalf("dump should contain the ASCII gutter, got:\n%s", out)
	}
}

func TestXxdEntryRespectsBytesAndOffset(t *testing.T) {
	imagePath := writeTestImage(t)

	out, err := xxdEntry(context.Background(), &XxdArgs{
		Archive: imagePath, Source: "/etc/passwd", Bytes: 4, Offset: 4,
	})
	if err != nil {
		t.Fatalf("xxd: %v", err)
	}
	// passwd is "root:x:0:0"; offset 4 + 4 bytes = ":x:0".
	if !strings.HasPrefix(out, "00000004:") {
		t.Fatalf("dump should start at offset 00000004, got %q", out)
	}
	if !strings.Contains(out, ":x:0") {
		t.Fatalf("dump should contain ':x:0' from offset 4, got:\n%s", out)
	}
}

func TestXxdEntryNotFoundKind(t *testing.T) {
	imagePath := writeTestImage(t)

	_, err := xxdEntry(context.Background(), &XxdArgs{
		Archive: imagePath, Source: "/nosuch", Bytes: 256,
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if got := errs.Classify(err); got != errs.KindNotFound {
		t.Fatalf("unexpected error kind: %v", got)
	}
}

func TestXxdEntryInvalidArgs(t *testing.T) {
	imagePath := writeTestImage(t)

	for _, args := range []XxdArgs{
		{Archive: imagePath, Source: "/etc/passwd", Bytes: 0},
		{Archive: imagePath, Source: "/etc/passwd", Bytes: 256, Offset: -1},
	} {
		if _, err := xxdEntry(context.Background(), &args); errs.Classify(err) != errs.KindInvalidInput {
			t.Fatalf("expected invalid-input error for %+v, got %v", args, err)
		}
	}
}

func TestXxdEntryOffsetBeyondEOFIsEmpty(t *testing.T) {
	imagePath := writeTestImage(t)

	out, err := xxdEntry(context.Background(), &XxdArgs{
		Archive: imagePath, Source: "/etc/passwd", Bytes: 256, Offset: 1 << 20,
	})
	if err != nil {
		t.Fatalf("xxd beyond EOF should not error: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty dump beyond EOF, got %q", out)
	}
}

func TestCatEntryInvalidBuffer(t *testing.T) {
	imagePath := writeTestImage(t)

	_, err := catEntry(context.Background(), &CatArgs{
		Archive: imagePath, Source: "/etc/passwd", BufferSize: 0,
	})
	if got := errs.Classify(err); got != errs.KindInvalidInput {
		t.Fatalf("unexpected error kind: %v", got)
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

func TestHelpBlocksCarryMetaInfo(t *testing.T) {
	zh := helpTextFor(langx.ZhCn)
	before := helpBeforeBlock(langx.ZhCn)
	for _, want := range []string{
		zh.summary,
		"版本: " + version,
		helpRepoURL,
		"示例:",
		"udf ls ./image.tar /etc",
		"udf cp ./image.tar /etc/passwd ./passwd",
		"udf cat ./image.tar /etc/passwd",
		"udf xxd ./image.tar /etc/passwd",
		"udf serve --addr 127.0.0.1:8080",
		"udf mcp stdio",
	} {
		if !strings.Contains(before, want) {
			t.Errorf("zh help Before block missing %q:\n%s", want, before)
		}
	}
	for _, want := range []string{"-h, --help", "-v, --version", "--json", "completion", "--bearer", "--versions", "--xyz.lang"} {
		if !strings.Contains(zh.options, want) {
			t.Errorf("zh help After block missing %q:\n%s", want, zh.options)
		}
	}
	for _, want := range []string{"默认命令", "flag 请放在路径之后"} {
		if !strings.Contains(zh.extractAfter, want) {
			t.Errorf("zh extract help After block missing %q:\n%s", want, zh.extractAfter)
		}
	}

	en := helpTextFor(langx.En)
	before = helpBeforeBlock(langx.En)
	for _, want := range []string{
		en.summary,
		"Version: " + version,
		"Repository: " + helpRepoURL,
		"Examples:",
		"show image metadata",
	} {
		if !strings.Contains(before, want) {
			t.Errorf("en help Before block missing %q:\n%s", want, before)
		}
	}
	for _, want := range []string{"Built-in options:", "interface language", "--xyz.lang"} {
		if !strings.Contains(en.options, want) {
			t.Errorf("en help After block missing %q:\n%s", want, en.options)
		}
	}
	for _, want := range []string{"extract is the default command", "put flags after the archive path"} {
		if !strings.Contains(en.extractAfter, want) {
			t.Errorf("en extract help After block missing %q:\n%s", want, en.extractAfter)
		}
	}

	// The footer now also lists the supported input formats.
	if after := helpAfterBlock(langx.ZhCn); !strings.Contains(after, "支持的输入") || !strings.Contains(after, "qcow2") || !strings.Contains(after, "内置选项") {
		t.Errorf("zh helpAfter block missing formats/options:\n%s", after)
	}
	if after := helpAfterBlock(langx.En); !strings.Contains(after, "Supported inputs") || !strings.Contains(after, "qcow2") || !strings.Contains(after, "Built-in options") {
		t.Errorf("en helpAfter block missing formats/options:\n%s", after)
	}
	for _, want := range []string{"qed", "wim", "erofs", "LVM2"} {
		if !strings.Contains(helpTextFor(langx.En).formats, want) {
			t.Errorf("en formats missing %q:\n%s", want, helpTextFor(langx.En).formats)
		}
	}
}

func TestEffectiveLangDetectsEnvironment(t *testing.T) {
	t.Setenv("LANG", "zh_CN.UTF-8")
	t.Setenv("LC_ALL", "")
	if got := effectiveLang(); got != langx.ZhCn {
		t.Fatalf("effectiveLang with zh LANG = %v, want zh-CN", got)
	}

	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "C")
	if got := effectiveLang(); got != langx.En {
		t.Fatalf("effectiveLang with C locale = %v, want en", got)
	}
}
