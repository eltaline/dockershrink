package loader

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/eltaline/dockershrink/image"
)

// Format describes the detected image source format.
type Format int

const (
	FormatDockerSave Format = iota // docker save tar archive
	FormatOCILayout               // OCI image layout directory
	FormatPlainTar                // plain tar archive (single layer)
)

func (f Format) String() string {
	switch f {
	case FormatDockerSave:
		return "docker-save"
	case FormatOCILayout:
		return "oci-layout"
	case FormatPlainTar:
		return "plain-tar"
	default:
		return "unknown"
	}
}

// ociLayout is the content of the oci-layout file.
type ociLayout struct {
	ImageLayoutVersion string `json:"imageLayoutVersion"`
}

// ociIndex is the OCI image index (index.json).
type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	Manifests     []ociDescriptor `json:"manifests"`
}

// ociDescriptor describes a blob in OCI layout.
type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// ociManifest is an OCI image manifest.
type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

// ociImageConfig mirrors the subset of OCI image config we need.
type ociImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Created      string `json:"created"`
	Config       struct {
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		Labels       map[string]string   `json:"Labels"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
	History []struct {
		CreatedBy  string `json:"created_by"`
		EmptyLayer bool   `json:"empty_layer"`
	} `json:"history"`
}

// DetectFormat determines the image format of the given path.
// It returns FormatOCILayout for directories with oci-layout,
// FormatDockerSave for tar archives containing manifest.json,
// and FormatPlainTar for other tar archives.
func DetectFormat(path string) (Format, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}

	if fi.IsDir() {
		if _, err := os.Stat(filepath.Join(path, "oci-layout")); err == nil {
			return FormatOCILayout, nil
		}
		return 0, fmt.Errorf("%s is a directory but not an OCI layout (missing oci-layout file)", path)
	}

	// Must be a file — probe the tar for manifest.json.
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read tar %s: %w", path, err)
		}
		if hdr.Name == "manifest.json" || hdr.Name == "./manifest.json" {
			return FormatDockerSave, nil
		}
	}

	return FormatPlainTar, nil
}

// LoadFile loads a Docker image from a file or directory path.
// It auto-detects the format and delegates to the appropriate parser.
func LoadFile(path string) (*image.Image, error) {
	format, err := DetectFormat(path)
	if err != nil {
		return nil, err
	}

	switch format {
	case FormatDockerSave:
		return loadDockerSave(path)
	case FormatOCILayout:
		return loadOCILayout(path)
	case FormatPlainTar:
		return loadPlainTar(path)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
}

// loadDockerSave loads an image from a docker-save tar archive.
func loadDockerSave(tarPath string) (*image.Image, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tr := tar.NewReader(f)

	type rawLayer struct {
		files []image.FileEntry
		size  int64
	}
	layerData := make(map[string]*rawLayer)
	var manifest []manifestItem
	configs := make(map[string][]byte)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}

		name := path.Clean(hdr.Name)

		switch {
		case name == "manifest.json":
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				return nil, fmt.Errorf("decode manifest.json: %w", err)
			}

		case strings.HasSuffix(name, ".json") && name != "manifest.json":
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read config %s: %w", name, err)
			}
			configs[name] = data

		case strings.HasSuffix(name, "/layer.tar"):
			rl := &rawLayer{}
			innerTR := tar.NewReader(tr)
			for {
				ih, ierr := innerTR.Next()
				if ierr == io.EOF {
					break
				}
				if ierr != nil {
					return nil, fmt.Errorf("read inner layer %s: %w", name, ierr)
				}
				rl.files = append(rl.files, image.FileEntry{
					Path: ih.Name,
					Size: ih.Size,
					Mode: uint32(ih.Mode),
					Link: ih.Linkname,
				})
				rl.size += ih.Size
			}
			layerData[name] = rl
		}
	}

	if len(manifest) == 0 {
		return nil, fmt.Errorf("manifest.json not found or empty in %s", tarPath)
	}

	mi := manifest[0]

	img := &image.Image{
		RepoTags: mi.RepoTags,
	}

	// Parse the image config JSON if present.
	configName := path.Clean(mi.Config)
	if data, ok := configs[configName]; ok {
		var cfg ociImageConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("decode config %s: %w", configName, err)
		}
		img.Architecture = cfg.Architecture
		img.OS = cfg.OS
		img.Config = image.Config{
			Env:          cfg.Config.Env,
			Cmd:          cfg.Config.Cmd,
			Entrypoint:   cfg.Config.Entrypoint,
			ExposedPorts: cfg.Config.ExposedPorts,
			Volumes:      cfg.Config.Volumes,
			WorkingDir:   cfg.Config.WorkingDir,
			User:         cfg.Config.User,
			Labels:       cfg.Config.Labels,
		}
		img.RootFS = image.RootFS{
			Type:    cfg.RootFS.Type,
			DiffIDs: cfg.RootFS.DiffIDs,
		}

		// Build CreatedBy index from config history.
		createdByIdx := 0
		createdByMap := make(map[int]string)
		for _, h := range cfg.History {
			if !h.EmptyLayer {
				createdByMap[createdByIdx] = h.CreatedBy
				createdByIdx++
			}
		}

		// Build ordered layers.
		layers := make([]image.Layer, len(mi.Layers))
		for i, lp := range mi.Layers {
			lp = path.Clean(lp)
			l := image.Layer{
				CreatedBy: createdByMap[i],
			}
			if i < len(cfg.RootFS.DiffIDs) {
				l.DiffID = cfg.RootFS.DiffIDs[i]
			}
			if rl, ok := layerData[lp]; ok {
				l.Files = rl.files
				l.Size = rl.size
			}
			layers[i] = l
		}
		img.Layers = layers
	} else {
		// No config — build layers from manifest order only.
		layers := make([]image.Layer, len(mi.Layers))
		for i, lp := range mi.Layers {
			lp = path.Clean(lp)
			if rl, ok := layerData[lp]; ok {
				layers[i] = image.Layer{
					Files: rl.files,
					Size:  rl.size,
				}
			}
		}
		img.Layers = layers
	}

	return img, nil
}

// loadOCILayout loads an image from an OCI image layout directory.
func loadOCILayout(dir string) (*image.Image, error) {
	// Validate oci-layout version.
	layoutData, err := os.ReadFile(filepath.Join(dir, "oci-layout"))
	if err != nil {
		return nil, fmt.Errorf("read oci-layout: %w", err)
	}
	var layout ociLayout
	if err := json.Unmarshal(layoutData, &layout); err != nil {
		return nil, fmt.Errorf("decode oci-layout: %w", err)
	}
	if layout.ImageLayoutVersion != "1.0.0" {
		return nil, fmt.Errorf("unsupported OCI layout version: %s", layout.ImageLayoutVersion)
	}

	// Read index.json.
	indexData, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("read index.json: %w", err)
	}
	var index ociIndex
	if err := json.Unmarshal(indexData, &index); err != nil {
		return nil, fmt.Errorf("decode index.json: %w", err)
	}
	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("index.json contains no manifests")
	}

	// Read the first manifest.
	manifestDesc := index.Manifests[0]
	manifestPath := blobPath(dir, manifestDesc.Digest)
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest blob %s: %w", manifestDesc.Digest, err)
	}
	var mf ociManifest
	if err := json.Unmarshal(manifestData, &mf); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	// Read config blob.
	configPath := blobPath(dir, mf.Config.Digest)
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config blob %s: %w", mf.Config.Digest, err)
	}
	var cfg ociImageConfig
	if err := json.Unmarshal(configData, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	img := &image.Image{
		Architecture: cfg.Architecture,
		OS:           cfg.OS,
		Config: image.Config{
			Env:          cfg.Config.Env,
			Cmd:          cfg.Config.Cmd,
			Entrypoint:   cfg.Config.Entrypoint,
			ExposedPorts: cfg.Config.ExposedPorts,
			Volumes:      cfg.Config.Volumes,
			WorkingDir:   cfg.Config.WorkingDir,
			User:         cfg.Config.User,
			Labels:       cfg.Config.Labels,
		},
		RootFS: image.RootFS{
			Type:    cfg.RootFS.Type,
			DiffIDs: cfg.RootFS.DiffIDs,
		},
	}

	// Build CreatedBy index from history.
	createdByMap := make(map[int]string)
	layerIdx := 0
	for _, h := range cfg.History {
		if !h.EmptyLayer {
			createdByMap[layerIdx] = h.CreatedBy
			layerIdx++
		}
	}

	// Extract file entries from each layer blob.
	layers := make([]image.Layer, len(mf.Layers))
	for i, ld := range mf.Layers {
		l := image.Layer{
			CreatedBy: createdByMap[i],
		}
		if i < len(cfg.RootFS.DiffIDs) {
			l.DiffID = cfg.RootFS.DiffIDs[i]
		}

		bp := blobPath(dir, ld.Digest)
		files, size, err := extractLayerFiles(bp)
		if err != nil {
			return nil, fmt.Errorf("extract layer %s: %w", ld.Digest, err)
		}
		l.Files = files
		l.Size = size
		layers[i] = l
	}
	img.Layers = layers

	return img, nil
}

// loadPlainTar loads file entries from a plain tar archive as a single layer.
func loadPlainTar(tarPath string) (*image.Image, error) {
	files, size, err := extractLayerFiles(tarPath)
	if err != nil {
		return nil, fmt.Errorf("read tar %s: %w", tarPath, err)
	}

	return &image.Image{
		Layers: []image.Layer{{
			Files: files,
			Size:  size,
		}},
	}, nil
}

// extractLayerFiles reads a tar file and returns file entries and total size.
func extractLayerFiles(tarPath string) ([]image.FileEntry, int64, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	var files []image.FileEntry
	var totalSize int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read tar entry: %w", err)
		}
		files = append(files, image.FileEntry{
			Path: hdr.Name,
			Size: hdr.Size,
			Mode: uint32(hdr.Mode),
			Link: hdr.Linkname,
		})
		totalSize += hdr.Size
	}

	return files, totalSize, nil
}

// blobPath returns the filesystem path for an OCI blob given its digest.
// Digest format: "sha256:<hex>".
func blobPath(dir string, digest string) string {
	// digest is "algorithm:hex"
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return filepath.Join(dir, "blobs", digest)
	}
	return filepath.Join(dir, "blobs", parts[0], parts[1])
}
