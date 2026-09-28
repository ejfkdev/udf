// Package cachedir owns udf's derived-data directory: a per-user directory
// inside the operating system's temporary directory, <temp>/ejfkdev/udf.
//
// Everything in it is derived from an input file and can be deleted at any
// time — a missing entry is rebuilt by the next command that needs it (an
// image archive index costs one pass, a listing or a metadata file is
// milliseconds). Keeping it in the temporary directory means the operating
// system reclaims it on its own terms: macOS purges entries unused for three
// days, Linux clears /tmp at boot or after ten days. Where the OS does nothing
// — Windows leaves %TEMP% alone — udf prunes entries itself, on the first
// write of a run, so the directory cannot grow without bound anywhere.
//
// Layout:
//
//	<dir>/*.json      small derived values: input class, image metadata,
//	                  archive listings, disk metadata, image summaries
//	<dir>/idx/*.idx   random-access indexes for large archives
//	<dir>/tmp/        per-run scratch (disks decompressed from an OVA/VMA/VM
//	                  export); removed by the run that made it, and pruned when
//	                  a run is interrupted
//
// The location can be overridden with UDF_CACHE_DIR, which is also how tests
// keep their cache out of the real one.
package cachedir

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// Env is the environment variable that overrides the directory.
	Env = "UDF_CACHE_DIR"

	// CachePruneAge is how long a derived value (a listing, a metadata file, an
	// index) survives without being rewritten. It is deliberately longer than
	// the shortest OS purge (three days on macOS) so that pruning only matters
	// where the OS does not clean up at all.
	CachePruneAge = 7 * 24 * time.Hour

	// ScratchPruneAge is how long a scratch directory from an interrupted run
	// survives. Runs remove their own scratch, so anything this old was left
	// behind by a crash or a kill.
	ScratchPruneAge = 24 * time.Hour

	idxDirName     = "idx"
	scratchDirName = "tmp"
)

// Dir returns the directory holding udf's derived data. It does not have to
// exist yet.
func Dir() string {
	if dir := os.Getenv(Env); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "ejfkdev", "udf")
}

// IndexDir returns the directory holding random-access indexes.
func IndexDir() string { return filepath.Join(Dir(), idxDirName) }

// ScratchDir returns the directory holding per-run scratch files.
func ScratchDir() string { return filepath.Join(Dir(), scratchDirName) }

// pruneOnce keeps pruning to the first call of a process: the sweep is hygiene,
// not correctness, and one pass per run is enough.
var pruneOnce sync.Once

// Ensure creates the directory (and its index subdirectory) and, once per
// process, prunes entries earlier runs left behind. It returns the directory
// path, or "" when it cannot be created. The path is read from the environment
// on every call, so a caller that overrides the location gets that location.
func Ensure() string {
	dir := Dir()
	if err := os.MkdirAll(filepath.Join(dir, idxDirName), 0o755); err != nil {
		return ""
	}
	pruneOnce.Do(func() { Prune(time.Now()) })
	return dir
}

// Temp creates a scratch directory for this run, under the temporary area of
// the cache directory.
func Temp(pattern string) (string, error) {
	if err := os.MkdirAll(ScratchDir(), 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(ScratchDir(), pattern)
}

// TempFile creates a scratch file for this run, under the temporary area of
// the cache directory. The pattern may carry a suffix, as in "disk-*.raw".
func TempFile(pattern string) (*os.File, error) {
	dir := ScratchDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, pattern)
}

// Prune removes derived values and scratch left behind by earlier runs. It is
// best effort: anything it cannot read or remove is ignored, since every entry
// is expendable.
func Prune(now time.Time) {
	dir := Dir()
	pruneFiles(dir, now, CachePruneAge)
	pruneFiles(filepath.Join(dir, idxDirName), now, CachePruneAge)

	scratch := filepath.Join(dir, scratchDirName)
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < ScratchPruneAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(scratch, e.Name()))
	}
}

// pruneFiles removes the regular files of dir that were last written longer
// than age ago. Entries that are not regular files (a scratch directory in the
// wrong place, for instance) are left alone.
func pruneFiles(dir string, now time.Time, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode().IsRegular() == false {
			continue
		}
		if now.Sub(info.ModTime()) < age {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
