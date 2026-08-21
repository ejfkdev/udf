// Package fsview builds an in-memory merged view of an image's layered
// filesystem. Layers are overlaid in manifest order with whiteout and
// opaque-directory semantics, so the resulting tree describes the final
// rootfs without writing anything to disk.
package fsview
