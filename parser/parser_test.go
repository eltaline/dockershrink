package parser

import (
	"testing"
	"time"
)

func TestParseManifest(t *testing.T) {
	data := []byte(`{
		"schemaVersion": 2,
		"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config": {
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"digest": "sha256:aabbccdd",
			"size": 1234
		},
		"layers": [
			{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": "sha256:layer1", "size": 100},
			{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": "sha256:layer2", "size": 200}
		]
	}`)

	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", m.SchemaVersion)
	}
	if m.Config.Digest != "sha256:aabbccdd" {
		t.Errorf("Config.Digest = %q, want sha256:aabbccdd", m.Config.Digest)
	}
	if len(m.Layers) != 2 {
		t.Fatalf("len(Layers) = %d, want 2", len(m.Layers))
	}
	if m.Layers[0].Digest != "sha256:layer1" {
		t.Errorf("Layers[0].Digest = %q, want sha256:layer1", m.Layers[0].Digest)
	}
	if m.Layers[1].Size != 200 {
		t.Errorf("Layers[1].Size = %d, want 200", m.Layers[1].Size)
	}
}

func TestParseManifestInvalid(t *testing.T) {
	// No config digest.
	data := []byte(`{"schemaVersion": 2, "config": {}, "layers": []}`)
	_, err := ParseManifest(data)
	if err == nil {
		t.Fatal("expected error for manifest without config digest")
	}

	// Invalid JSON.
	_, err = ParseManifest([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseImageConfig(t *testing.T) {
	data := []byte(`{
		"architecture": "amd64",
		"os": "linux",
		"created": "2024-01-15T10:30:00.123456789Z",
		"config": {
			"Env": ["PATH=/usr/bin", "HOME=/root"],
			"Cmd": ["/bin/sh"],
			"Entrypoint": ["/docker-entrypoint.sh"],
			"WorkingDir": "/app",
			"User": "nobody",
			"Labels": {"version": "1.0"},
			"ExposedPorts": {"8080/tcp": {}},
			"Volumes": {"/data": {}}
		},
		"rootfs": {
			"type": "layers",
			"diff_ids": ["sha256:aaa", "sha256:bbb", "sha256:ccc"]
		},
		"history": [
			{"created": "2024-01-10T00:00:00Z", "created_by": "/bin/sh -c #(nop) ADD file:abc in /"},
			{"created": "2024-01-11T00:00:00Z", "created_by": "/bin/sh -c apt-get update"},
			{"created": "2024-01-12T00:00:00Z", "created_by": "/bin/sh -c #(nop) ENV PATH=/usr/bin", "empty_layer": true},
			{"created": "2024-01-13T00:00:00Z", "created_by": "/bin/sh -c npm install"},
			{"created": "2024-01-14T00:00:00Z", "created_by": "/bin/sh -c #(nop) CMD [\"/bin/sh\"]", "empty_layer": true}
		]
	}`)

	ic, err := ParseImageConfig(data)
	if err != nil {
		t.Fatalf("ParseImageConfig: %v", err)
	}

	if ic.Architecture != "amd64" {
		t.Errorf("Architecture = %q, want amd64", ic.Architecture)
	}
	if ic.OS != "linux" {
		t.Errorf("OS = %q, want linux", ic.OS)
	}
	if ic.Created.Year() != 2024 || ic.Created.Month() != time.January || ic.Created.Day() != 15 {
		t.Errorf("Created = %v, want 2024-01-15", ic.Created)
	}

	// Container config.
	if len(ic.Config.Env) != 2 {
		t.Errorf("len(Env) = %d, want 2", len(ic.Config.Env))
	}
	if ic.Config.Cmd[0] != "/bin/sh" {
		t.Errorf("Cmd[0] = %q, want /bin/sh", ic.Config.Cmd[0])
	}
	if ic.Config.Entrypoint[0] != "/docker-entrypoint.sh" {
		t.Errorf("Entrypoint[0] = %q, want /docker-entrypoint.sh", ic.Config.Entrypoint[0])
	}
	if ic.Config.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q, want /app", ic.Config.WorkingDir)
	}
	if ic.Config.User != "nobody" {
		t.Errorf("User = %q, want nobody", ic.Config.User)
	}
	if ic.Config.Labels["version"] != "1.0" {
		t.Errorf("Labels[version] = %q, want 1.0", ic.Config.Labels["version"])
	}
	if _, ok := ic.Config.ExposedPorts["8080/tcp"]; !ok {
		t.Error("ExposedPorts missing 8080/tcp")
	}
	if _, ok := ic.Config.Volumes["/data"]; !ok {
		t.Error("Volumes missing /data")
	}

	// RootFS.
	if ic.RootFS.Type != "layers" {
		t.Errorf("RootFS.Type = %q, want layers", ic.RootFS.Type)
	}
	if len(ic.RootFS.DiffIDs) != 3 {
		t.Fatalf("len(DiffIDs) = %d, want 3", len(ic.RootFS.DiffIDs))
	}

	// History.
	if len(ic.History) != 5 {
		t.Fatalf("len(History) = %d, want 5", len(ic.History))
	}
	if !ic.History[2].EmptyLayer {
		t.Error("History[2] should be EmptyLayer")
	}
	if ic.History[1].CreatedBy != "/bin/sh -c apt-get update" {
		t.Errorf("History[1].CreatedBy = %q", ic.History[1].CreatedBy)
	}
}

func TestMapLayerCommands(t *testing.T) {
	data := []byte(`{
		"architecture": "amd64",
		"os": "linux",
		"config": {},
		"rootfs": {
			"type": "layers",
			"diff_ids": ["sha256:aaa", "sha256:bbb", "sha256:ccc"]
		},
		"history": [
			{"created": "2024-01-10T00:00:00Z", "created_by": "ADD file:abc in /"},
			{"created_by": "ENV PATH=/usr/bin", "empty_layer": true},
			{"created": "2024-01-11T00:00:00Z", "created_by": "RUN apt-get update"},
			{"created_by": "LABEL version=1.0", "empty_layer": true},
			{"created": "2024-01-12T00:00:00Z", "created_by": "COPY . /app"}
		]
	}`)

	ic, err := ParseImageConfig(data)
	if err != nil {
		t.Fatalf("ParseImageConfig: %v", err)
	}

	cmds := ic.MapLayerCommands()
	if len(cmds) != 3 {
		t.Fatalf("len(MapLayerCommands) = %d, want 3", len(cmds))
	}

	// First real layer.
	if cmds[0].LayerIndex != 0 {
		t.Errorf("cmds[0].LayerIndex = %d, want 0", cmds[0].LayerIndex)
	}
	if cmds[0].DiffID != "sha256:aaa" {
		t.Errorf("cmds[0].DiffID = %q, want sha256:aaa", cmds[0].DiffID)
	}
	if cmds[0].CreatedBy != "ADD file:abc in /" {
		t.Errorf("cmds[0].CreatedBy = %q", cmds[0].CreatedBy)
	}

	// Second real layer (skipped empty ENV).
	if cmds[1].LayerIndex != 1 {
		t.Errorf("cmds[1].LayerIndex = %d, want 1", cmds[1].LayerIndex)
	}
	if cmds[1].DiffID != "sha256:bbb" {
		t.Errorf("cmds[1].DiffID = %q, want sha256:bbb", cmds[1].DiffID)
	}
	if cmds[1].CreatedBy != "RUN apt-get update" {
		t.Errorf("cmds[1].CreatedBy = %q", cmds[1].CreatedBy)
	}

	// Third real layer (skipped empty LABEL).
	if cmds[2].LayerIndex != 2 {
		t.Errorf("cmds[2].LayerIndex = %d, want 2", cmds[2].LayerIndex)
	}
	if cmds[2].DiffID != "sha256:ccc" {
		t.Errorf("cmds[2].DiffID = %q, want sha256:ccc", cmds[2].DiffID)
	}
	if cmds[2].CreatedBy != "COPY . /app" {
		t.Errorf("cmds[2].CreatedBy = %q", cmds[2].CreatedBy)
	}
}

func TestMapLayerCommandsMoreLayersThanHistory(t *testing.T) {
	data := []byte(`{
		"architecture": "amd64",
		"os": "linux",
		"config": {},
		"rootfs": {
			"type": "layers",
			"diff_ids": ["sha256:aaa", "sha256:bbb", "sha256:ccc"]
		},
		"history": [
			{"created_by": "ADD file:abc in /"}
		]
	}`)

	ic, err := ParseImageConfig(data)
	if err != nil {
		t.Fatalf("ParseImageConfig: %v", err)
	}

	cmds := ic.MapLayerCommands()
	if len(cmds) != 1 {
		t.Fatalf("len(MapLayerCommands) = %d, want 1", len(cmds))
	}
	if cmds[0].DiffID != "sha256:aaa" {
		t.Errorf("cmds[0].DiffID = %q, want sha256:aaa", cmds[0].DiffID)
	}
}

func TestParseImageConfigEmpty(t *testing.T) {
	data := []byte(`{}`)
	ic, err := ParseImageConfig(data)
	if err != nil {
		t.Fatalf("ParseImageConfig: %v", err)
	}
	if ic.Architecture != "" {
		t.Errorf("Architecture = %q, want empty", ic.Architecture)
	}
	if len(ic.History) != 0 {
		t.Errorf("len(History) = %d, want 0", len(ic.History))
	}
	cmds := ic.MapLayerCommands()
	if len(cmds) != 0 {
		t.Errorf("len(MapLayerCommands) = %d, want 0", len(cmds))
	}
}
