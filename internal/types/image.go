package types

type ManifestItem struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type ImageConfig struct {
	Architecture string `json:"architecture"`
	Config       struct {
		User         string         `json:"User"`
		Env          []string       `json:"Env"`
		Entrypoint   []string       `json:"Entrypoint"`
		Cmd          []string       `json:"Cmd"`
		WorkingDir   string         `json:"WorkingDir"`
		ExposedPorts map[string]any `json:"ExposedPorts"`
	} `json:"config"`
}

type ImageMetadata struct {
	Index      int
	Total      int
	RepoTags   []string
	ConfigPath string
	LayerOrder []string
	Config     *ImageConfig
	ConfigRaw  any
}
