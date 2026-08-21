// Package layer applies individual image layer tars to a target directory.
// It detects compressed layer streams, handles whiteout markers and opaque
// directories, and supports regular files, symlinks and hardlinks.
package layer
