package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	arch "github.com/ejfkdev/udf/image/archive"
)

// The Android emulator ships the real thing: a GPT system.img whose second
// partition is an LP super holding five dynamic partitions, vendor.img and
// encryptionkey.img as single-partition GPT disks, and an LZ4-legacy ramdisk.
// These tests run against a local SDK/AVD installation when told where it is:
//
//	UDF_ANDROID_SYSTEM_IMAGE=/path/to/system-images/android-…/arm64-v8a
//	UDF_ANDROID_AVD=/path/to/avd/Name.avd
//
// They are the only checks for GPT detection, super/LP partitions and the
// ext4 checksum variant Android's mkfs writes, so they are worth the opt-in.

func TestAndroidSystemImage(t *testing.T) {
	dir := os.Getenv("UDF_ANDROID_SYSTEM_IMAGE")
	if dir == "" {
		t.Skip("set UDF_ANDROID_SYSTEM_IMAGE to an SDK system-images directory")
	}
	systemImg := filepath.Join(dir, "system.img")
	if _, err := os.Stat(systemImg); err != nil {
		t.Skipf("no system.img in %s", dir)
	}

	if !IsDiskImage(systemImg) {
		t.Fatal("a GPT system.img was not recognized as a disk image")
	}
	meta, err := ScanDiskMetadata(systemImg)
	if err != nil {
		t.Fatalf("ScanDiskMetadata: %v", err)
	}
	if len(meta.Disks) != 1 {
		t.Fatalf("got %d disks, want 1", len(meta.Disks))
	}

	// The dynamic partitions of the super partition, by name and filesystem.
	want := map[string]string{
		"system":      "ext4",
		"system_ext":  "ext4",
		"product":     "ext4",
		"vendor":      "ext4",
		"system_dlkm": "erofs",
	}
	got := map[string]string{}
	for _, v := range meta.Disks[0].Volumes {
		if i := strings.LastIndexByte(v.Name, '/'); i >= 0 {
			got[v.Name[i+1:]] = v.FSType
			if v.Kind != "super" {
				t.Errorf("volume %s kind = %q, want super", v.Name, v.Kind)
			}
		}
	}
	for name, fs := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("dynamic partition %s missing; volumes: %v", name, meta.Disks[0].Volumes)
		}
		if got[name] != fs {
			t.Errorf("partition %s filesystem = %q, want %q", name, got[name], fs)
		}
	}

	// Reading through the super mapping: the system partition's own build.prop
	// must have the properties a system image carries.
	var vol string
	for _, v := range meta.Disks[0].Volumes {
		if strings.HasSuffix(v.Name, "/system") && v.FSType == "ext4" {
			vol = v.Name
		}
	}
	if vol == "" {
		t.Fatal("no system volume")
	}
	entries, err := ListDisk(systemImg, vol)
	if err != nil {
		t.Fatalf("ListDisk(%s): %v", vol, err)
	}
	if len(entries) < 10 {
		t.Fatalf("system partition lists %d entries", len(entries))
	}
	// Android 10+ uses the system-as-root layout: the partition root holds the
	// device's top-level directories, with the build.prop under system/.
	dest := filepath.Join(t.TempDir(), "build.prop")
	if _, err := ExtractDiskPath(systemImg, vol+"/system/build.prop", dest, 1<<20); err != nil {
		t.Fatalf("extract %s/system/build.prop: %v", vol, err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ro.build.") {
		t.Fatalf("build.prop does not look like one: %q", firstBytes(data, 80))
	}
	t.Logf("%s/system/build.prop: %d bytes", vol, len(data))
}

func TestAndroidAvdImages(t *testing.T) {
	dir := os.Getenv("UDF_ANDROID_AVD")
	if dir == "" {
		t.Skip("set UDF_ANDROID_AVD to an .avd directory")
	}
	cases := []struct {
		file    string
		fs      string
		wantAny bool
	}{
		{"cache.img", "ext4", false},
		{"sdcard.img", "fat32", false},
		{"userdata-qemu.img", "ext4", false},
		{"encryptionkey.img", "ext4", false},
	}
	for _, tc := range cases {
		path := filepath.Join(dir, tc.file)
		if _, err := os.Stat(path); err != nil {
			t.Logf("%s: not present", tc.file)
			continue
		}
		meta, err := ScanDiskMetadata(path)
		if err != nil {
			t.Errorf("%s: %v", tc.file, err)
			continue
		}
		found := false
		for _, d := range meta.Disks {
			for _, v := range d.Volumes {
				if v.FSType == tc.fs {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s: no %s volume in %v", tc.file, tc.fs, meta.Disks)
		}
	}
}

// TestAndroidRamdiskImage checks the LZ4-legacy ramdisk path end to end
// (ramdisk.img, and the AVD's initrd, which carries an appended bootconfig).
func TestAndroidRamdiskImage(t *testing.T) {
	var candidates []string
	if dir := os.Getenv("UDF_ANDROID_SYSTEM_IMAGE"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "ramdisk.img"))
	}
	if dir := os.Getenv("UDF_ANDROID_AVD"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "initrd"))
	}
	tested := false
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		tested = true
		archive, err := arch.Open(path)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		entries, err := archive.List()
		if err != nil {
			t.Fatalf("%s: list: %v", filepath.Base(path), err)
		}
		if len(entries) < 20 {
			t.Fatalf("%s: only %d entries", filepath.Base(path), len(entries))
		}
		var init arch.Entry
		for _, e := range entries {
			if e.Name == "init" {
				init = e
			}
		}
		if init.Name == "" {
			t.Fatalf("%s: no init in the ramdisk: %v", filepath.Base(path), entries[:5])
		}
		rc, size, err := archive.Open("init")
		if err != nil {
			t.Fatalf("%s: open init: %v", filepath.Base(path), err)
		}
		head := make([]byte, 4)
		if _, err := rc.Read(head); err != nil {
			t.Fatalf("%s: read init: %v", filepath.Base(path), err)
		}
		rc.Close()
		if string(head) != "\x7fELF" {
			t.Fatalf("%s: init does not start with an ELF header: %x", filepath.Base(path), head)
		}
		if size != init.Size {
			t.Fatalf("%s: init size %d != listed %d", filepath.Base(path), size, init.Size)
		}
		t.Logf("%s: %d entries, init %d bytes", filepath.Base(path), len(entries), size)
	}
	if !tested {
		t.Skip("no ramdisk found next to the configured Android images")
	}
}

func firstBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
