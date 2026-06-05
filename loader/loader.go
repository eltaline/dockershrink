package loader

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/eltaline/dockershrink/image"
)

// inspectResponse mirrors the subset of fields returned by
// GET /images/{name}/json that we care about.
type inspectResponse struct {
	ID           string    `json:"Id"`
	RepoTags     []string  `json:"RepoTags"`
	RepoDigests  []string  `json:"RepoDigests"`
	Created      time.Time `json:"Created"`
	Architecture string    `json:"Architecture"`
	OS           string    `json:"Os"`
	Size         int64     `json:"Size"`
	Config       struct {
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		Labels       map[string]string   `json:"Labels"`
	} `json:"Config"`
	RootFS struct {
		Type   string   `json:"Type"`
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

// historyEntry mirrors GET /images/{name}/history.
type historyEntry struct {
	CreatedBy string `json:"CreatedBy"`
	Size      int64  `json:"Size"`
}

// manifestItem is an entry inside the top-level manifest.json
// that Docker writes into the `docker save` tar stream.
type manifestItem struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// Load retrieves a Docker image from the local daemon by reference (name, name:tag, or ID),
// inspects its metadata, and streams the layer contents without writing a temporary tar file.
func Load(ctx context.Context, ref string, socketPath string) (*image.Image, error) {
	c := NewClient(socketPath)

	// 1. Inspect image metadata.
	inspect, err := inspectImage(ctx, c, ref)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", ref, err)
	}

	// 2. Get build history for CreatedBy strings.
	history, err := imageHistory(ctx, c, ref)
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", ref, err)
	}

	// Build a map from diff-ID index to CreatedBy.
	createdByIndex := buildCreatedByIndex(history)

	// 3. Stream `docker save`-style tar and extract layer file lists on the fly.
	layers, err := streamLayers(ctx, c, ref, inspect.RootFS.Layers, createdByIndex)
	if err != nil {
		return nil, fmt.Errorf("stream layers %s: %w", ref, err)
	}

	img := &image.Image{
		ID:           inspect.ID,
		RepoTags:     inspect.RepoTags,
		RepoDigests:  inspect.RepoDigests,
		Created:      inspect.Created,
		Architecture: inspect.Architecture,
		OS:           inspect.OS,
		Size:         inspect.Size,
		Config: image.Config{
			Env:          inspect.Config.Env,
			Cmd:          inspect.Config.Cmd,
			Entrypoint:   inspect.Config.Entrypoint,
			ExposedPorts: inspect.Config.ExposedPorts,
			Volumes:      inspect.Config.Volumes,
			WorkingDir:   inspect.Config.WorkingDir,
			User:         inspect.Config.User,
			Labels:       inspect.Config.Labels,
		},
		RootFS: image.RootFS{
			Type:    inspect.RootFS.Type,
			DiffIDs: inspect.RootFS.Layers,
		},
		Layers: layers,
	}
	return img, nil
}

func inspectImage(ctx context.Context, c *Client, ref string) (*inspectResponse, error) {
	resp, err := c.get(ctx, "/images/"+ref+"/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ir inspectResponse
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	return &ir, nil
}

func imageHistory(ctx context.Context, c *Client, ref string) ([]historyEntry, error) {
	resp, err := c.get(ctx, "/images/"+ref+"/history")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var entries []historyEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode history: %w", err)
	}
	return entries, nil
}

// buildCreatedByIndex maps zero-based layer indices to the CreatedBy string from history.
// Docker history is returned newest-first; real (non-zero-size) entries correspond to layers.
func buildCreatedByIndex(history []historyEntry) map[int]string {
	m := make(map[int]string)
	idx := 0
	// Walk history in reverse (oldest first) so indices match layer order.
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Size > 0 {
			m[idx] = history[i].CreatedBy
			idx++
		}
	}
	return m
}

// streamLayers calls GET /images/{ref}/get which returns a tar archive
// (the same format as `docker save`). Inside the outer tar we find:
//   - manifest.json — layer ordering
//   - <hash>/layer.tar — each layer's filesystem
//
// We parse the outer tar in a single pass. For each inner layer.tar we open
// a nested tar reader and collect file entries without ever writing to disk.
func streamLayers(
	ctx context.Context,
	c *Client,
	ref string,
	diffIDs []string,
	createdByIndex map[int]string,
) ([]image.Layer, error) {
	resp, err := c.get(ctx, "/images/"+ref+"/get")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	tr := tar.NewReader(resp.Body)

	// We'll collect layers keyed by their path prefix (e.g. "<hash>/layer.tar").
	// After reading manifest.json we know the canonical order.
	type rawLayer struct {
		files []image.FileEntry
		size  int64
	}
	layerData := make(map[string]*rawLayer)
	var manifest []manifestItem

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read outer tar: %w", err)
		}

		name := path.Clean(hdr.Name)

		switch {
		case name == "manifest.json":
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				return nil, fmt.Errorf("decode manifest.json: %w", err)
			}

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
		return nil, fmt.Errorf("manifest.json not found in image export")
	}

	// Build ordered Layer slice from manifest.
	layerPaths := manifest[0].Layers
	layers := make([]image.Layer, len(layerPaths))
	for i, lp := range layerPaths {
		lp = path.Clean(lp)
		rl := layerData[lp]
		l := image.Layer{
			CreatedBy: createdByIndex[i],
		}
		if i < len(diffIDs) {
			l.DiffID = diffIDs[i]
		}
		if rl != nil {
			l.Files = rl.files
			l.Size = rl.size
		}
		layers[i] = l
	}

	return layers, nil
}
