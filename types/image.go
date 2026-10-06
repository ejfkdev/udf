package types

type ManifestItem struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`

	// LayerMediaTypes and ManifestDigest are recorded by formats that carry
	// them (OCI layouts and archives); a classic docker save records neither,
	// so both are empty there and the reader falls back to the blob names.
	LayerMediaTypes []string `json:"LayerMediaTypes,omitempty"`
	ManifestDigest  string   `json:"ManifestDigest,omitempty"`
}

type ImageConfig struct {
	Architecture  string `json:"architecture"`
	Variant       string `json:"variant"`
	OS            string `json:"os"`
	Created       string `json:"created"`
	DockerVersion string `json:"docker_version"`
	Config        struct {
		User         string         `json:"User"`
		Env          []string       `json:"Env"`
		Entrypoint   []string       `json:"Entrypoint"`
		Cmd          []string       `json:"Cmd"`
		WorkingDir   string         `json:"WorkingDir"`
		ExposedPorts map[string]any `json:"ExposedPorts"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// Platform renders the image's platform the way docker and OCI name it:
// os/architecture[/variant].
func (c *ImageConfig) Platform() string {
	if c == nil || c.OS == "" || c.Architecture == "" {
		return ""
	}
	p := c.OS + "/" + c.Architecture
	if c.Variant != "" {
		p += "/" + c.Variant
	}
	return p
}

type ImageMetadata struct {
	Index      int
	Total      int
	RepoTags   []string
	ConfigPath string
	LayerOrder []string
	Config     *ImageConfig
	ConfigRaw  any

	// StoredSize is how much space the image's layers take in the archive,
	// when an index knows their members; zero when it does not.
	StoredSize int64

	// LayerMediaTypes is the media type of each layer as the manifest records
	// it (OCI layouts and archives); entries are empty for a classic docker
	// save, which stores layer paths without media types.
	LayerMediaTypes []string

	// ManifestDigest is "sha256:…" of the raw image manifest when the format
	// records one (OCI); empty for a classic docker save.
	ManifestDigest string
}

// NonDistributableLayer reports whether layer i is a non-distributable
// (foreign) layer: one whose blobs live at the vendor and are normally not
// shipped inside an archive.
func (m *ImageMetadata) NonDistributableLayer(i int) bool {
	if i < 0 || i >= len(m.LayerMediaTypes) {
		return false
	}
	return IsNonDistributable(m.LayerMediaTypes[i])
}

// Media types a container image manifest can name for a layer or a manifest.
// Kept here (rather than taken from a registry toolkit) because the reader only
// needs to recognise them, and the list is short.
const (
	MediaTypeOCIManifest       = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex          = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIConfig         = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayer          = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCILayerZstd      = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeOCIUncompressed   = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeOCIRestricted     = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"
	MediaTypeOCIUncompRestr    = "application/vnd.oci.image.layer.nondistributable.v1.tar"
	MediaTypeOCIRestrictedZstd = "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd"

	MediaTypeDockerManifest      = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList  = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerSchema1       = "application/vnd.docker.distribution.manifest.v1+json"
	MediaTypeDockerSchema1Signed = "application/vnd.docker.distribution.manifest.v1+prettyjws"
	MediaTypeDockerConfig        = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerLayer         = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaTypeDockerUncompressed  = "application/vnd.docker.image.rootfs.diff.tar"
	MediaTypeDockerForeignLayer  = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
)

// IsNonDistributable reports whether a layer media type is one whose content is
// fetched from the vendor rather than stored with the image.
func IsNonDistributable(mediaType string) bool {
	switch mediaType {
	case MediaTypeOCIRestricted, MediaTypeOCIUncompRestr, MediaTypeOCIRestrictedZstd, MediaTypeDockerForeignLayer:
		return true
	}
	return false
}

// IsIndexMediaType reports whether a manifest media type names an index (a
// manifest list) rather than an image.
func IsIndexMediaType(mediaType string) bool {
	switch mediaType {
	case MediaTypeOCIIndex, MediaTypeDockerManifestList:
		return true
	}
	return false
}

// LayerCompression names the compression a layer media type implies: "gzip",
// "zstd" or "" when the layer is an uncompressed tar.
func LayerCompression(mediaType string) string {
	switch mediaType {
	case MediaTypeOCILayer, MediaTypeOCIRestricted, MediaTypeDockerLayer, MediaTypeDockerForeignLayer:
		return "gzip"
	case MediaTypeOCILayerZstd, MediaTypeOCIRestrictedZstd:
		return "zstd"
	}
	return ""
}
