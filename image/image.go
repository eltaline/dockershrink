package image

import "time"

// Image holds the full representation of a Docker image:
// metadata from inspect and the ordered list of layers.
type Image struct {
	ID           string
	RepoTags     []string
	RepoDigests  []string
	Created      time.Time
	Architecture string
	OS           string
	Size         int64
	Config       Config
	RootFS       RootFS
	Layers       []Layer
}

// Config mirrors the container-config portion of the image inspect response.
type Config struct {
	Env          []string
	Cmd          []string
	Entrypoint   []string
	ExposedPorts map[string]struct{}
	Volumes      map[string]struct{}
	WorkingDir   string
	User         string
	Labels       map[string]string
}

// RootFS describes the image's layer chain.
type RootFS struct {
	Type    string
	DiffIDs []string
}

// Layer represents a single layer extracted from the image export stream.
type Layer struct {
	DiffID    string
	Size      int64
	CreatedBy string
	Files     []FileEntry
}

// FileEntry is a single file inside a layer.
type FileEntry struct {
	Path string
	Size int64
	Mode uint32
	Link string
}
