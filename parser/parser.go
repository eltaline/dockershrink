// Package parser provides types and functions for parsing OCI/Docker
// image manifests and image configuration blobs.
package parser

import (
	"encoding/json"
	"fmt"
	"time"
)

// Manifest represents a parsed OCI or Docker v2 image manifest.
type Manifest struct {
	SchemaVersion int
	MediaType     string
	Config        Descriptor
	Layers        []Descriptor
}

// Descriptor identifies a content-addressable blob.
type Descriptor struct {
	MediaType string
	Digest    string
	Size      int64
}

// ImageConfig holds the parsed image configuration including
// container runtime settings and build history.
type ImageConfig struct {
	Architecture string
	OS           string
	Created      time.Time
	Config       ContainerConfig
	RootFS       RootFS
	History      []HistoryEntry
}

// ContainerConfig holds the runtime configuration of the container.
type ContainerConfig struct {
	Env          []string
	Cmd          []string
	Entrypoint   []string
	ExposedPorts map[string]struct{}
	Volumes      map[string]struct{}
	WorkingDir   string
	User         string
	Labels       map[string]string
}

// RootFS describes the layer chain of the image.
type RootFS struct {
	Type    string
	DiffIDs []string
}

// HistoryEntry records a single step in the image build history.
type HistoryEntry struct {
	Created    time.Time
	CreatedBy  string
	Comment    string
	EmptyLayer bool
}

// LayerCommand associates a layer (by index) with its build command.
type LayerCommand struct {
	LayerIndex int
	DiffID     string
	CreatedBy  string
	Created    time.Time
}

// rawManifest is the JSON wire format for OCI/Docker manifests.
type rawManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType,omitempty"`
	Config        rawDescriptor   `json:"config"`
	Layers        []rawDescriptor `json:"layers"`
}

type rawDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// rawImageConfig is the JSON wire format for OCI/Docker image configs.
type rawImageConfig struct {
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
	History []rawHistoryEntry `json:"history"`
}

type rawHistoryEntry struct {
	Created    string `json:"created"`
	CreatedBy  string `json:"created_by"`
	Comment    string `json:"comment"`
	EmptyLayer bool   `json:"empty_layer"`
}

// ParseManifest decodes an OCI or Docker v2 image manifest from JSON.
func ParseManifest(data []byte) (*Manifest, error) {
	var rm rawManifest
	if err := json.Unmarshal(data, &rm); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if rm.Config.Digest == "" {
		return nil, fmt.Errorf("manifest has no config descriptor")
	}

	m := &Manifest{
		SchemaVersion: rm.SchemaVersion,
		MediaType:     rm.MediaType,
		Config: Descriptor{
			MediaType: rm.Config.MediaType,
			Digest:    rm.Config.Digest,
			Size:      rm.Config.Size,
		},
		Layers: make([]Descriptor, len(rm.Layers)),
	}
	for i, rl := range rm.Layers {
		m.Layers[i] = Descriptor{
			MediaType: rl.MediaType,
			Digest:    rl.Digest,
			Size:      rl.Size,
		}
	}
	return m, nil
}

// ParseImageConfig decodes an OCI or Docker image configuration blob from JSON.
func ParseImageConfig(data []byte) (*ImageConfig, error) {
	var rc rawImageConfig
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("decode image config: %w", err)
	}

	ic := &ImageConfig{
		Architecture: rc.Architecture,
		OS:           rc.OS,
		Config: ContainerConfig{
			Env:          rc.Config.Env,
			Cmd:          rc.Config.Cmd,
			Entrypoint:   rc.Config.Entrypoint,
			ExposedPorts: rc.Config.ExposedPorts,
			Volumes:      rc.Config.Volumes,
			WorkingDir:   rc.Config.WorkingDir,
			User:         rc.Config.User,
			Labels:       rc.Config.Labels,
		},
		RootFS: RootFS{
			Type:    rc.RootFS.Type,
			DiffIDs: rc.RootFS.DiffIDs,
		},
		History: make([]HistoryEntry, len(rc.History)),
	}

	if rc.Created != "" {
		if t, err := time.Parse(time.RFC3339Nano, rc.Created); err == nil {
			ic.Created = t
		}
	}

	for i, rh := range rc.History {
		he := HistoryEntry{
			CreatedBy:  rh.CreatedBy,
			Comment:    rh.Comment,
			EmptyLayer: rh.EmptyLayer,
		}
		if rh.Created != "" {
			if t, err := time.Parse(time.RFC3339Nano, rh.Created); err == nil {
				he.Created = t
			}
		}
		ic.History[i] = he
	}

	return ic, nil
}

// MapLayerCommands correlates build history entries with actual filesystem
// layers. Empty-layer history entries (e.g. ENV, LABEL, CMD) do not produce
// a layer and are skipped. The returned slice has one entry per real layer,
// in the same order as RootFS.DiffIDs.
func (ic *ImageConfig) MapLayerCommands() []LayerCommand {
	var cmds []LayerCommand
	layerIdx := 0

	for _, h := range ic.History {
		if h.EmptyLayer {
			continue
		}
		lc := LayerCommand{
			LayerIndex: layerIdx,
			CreatedBy:  h.CreatedBy,
			Created:    h.Created,
		}
		if layerIdx < len(ic.RootFS.DiffIDs) {
			lc.DiffID = ic.RootFS.DiffIDs[layerIdx]
		}
		cmds = append(cmds, lc)
		layerIdx++
	}
	return cmds
}
