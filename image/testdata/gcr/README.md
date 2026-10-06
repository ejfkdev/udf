# Fixtures from go-containerregistry

These three docker-save archives are copied from
`github.com/google/go-containerregistry` (Apache License 2.0,
`pkg/v1/mutate/testdata/`), and the tests run against them assert the same
behaviour its own tests do:

- `whiteout_image.tar` — a whiteout (`.wh.foo`) must remove the lower layer's
  file, and no whiteout entry may surface in the merged listing.
- `whiteout_dir.tar` — a whiteout naming a directory removes the directory.
- `overwritten_file.tar` — a file replaced by a later layer must carry the
  later layer's content (a symlink there, so following it must not reach the
  overwritten bytes).

The point is an outside reference for the layer merge: the fixtures were built
by another project's tooling, so the merge is checked against bytes udf did not
produce.
