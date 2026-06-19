package loader

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFormat_DockerSave(t *testing.T) {
	archive := buildImageTar(t)
	p := writeTempFile(t, "image-*.tar", archive)

	f, err := DetectFormat(p)
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if f != FormatDockerSave {
		t.Errorf("format = %v, want docker-save", f)
	}
}

func TestDetectFormat_OCILayout(t *testing.T) {
	dir := buildOCILayout(t)

	f, err := DetectFormat(dir)
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if f != FormatOCILayout {
		t.Errorf("format = %v, want oci-layout", f)
	}
}

func TestDetectFormat_PlainTar(t *testing.T) {
	p := buildPlainTar(t)

	f, err := DetectFormat(p)
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if f != FormatPlainTar {
		t.Errorf("format = %v, want plain-tar", f)
	}
}

func TestDetectFormat_DirWithoutOCILayout(t *testing.T) {
	dir := t.TempDir()
	_, err := DetectFormat(dir)
	if err == nil {
		t.Fatal("expected error for directory without oci-layout")
	}
}

func TestLoadFile_DockerSave(t *testing.T) {
	archive := buildDockerSaveTarWithConfig(t)
	p := writeTempFile(t, "image-*.tar", archive)

	img, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if img.Architecture != "amd64" {
		t.Errorf("Architecture = %q, want amd64", img.Architecture)
	}
	if img.OS != "linux" {
		t.Errorf("OS = %q, want linux", img.OS)
	}
	if img.Config.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q, want /app", img.Config.WorkingDir)
	}
	if len(img.RepoTags) != 1 || img.RepoTags[0] != "testimg:latest" {
		t.Errorf("RepoTags = %v, want [testimg:latest]", img.RepoTags)
	}
	if len(img.Layers) != 1 {
		t.Fatalf("len(Layers) = %d, want 1", len(img.Layers))
	}
	layer := img.Layers[0]
	if layer.DiffID != "sha256:diff1" {
		t.Errorf("DiffID = %q, want sha256:diff1", layer.DiffID)
	}
	if layer.CreatedBy != "/bin/sh -c go build -o /app" {
		t.Errorf("CreatedBy = %q, want build command", layer.CreatedBy)
	}
	if len(layer.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(layer.Files))
	}
	if layer.Files[0].Path != "app/main.go" {
		t.Errorf("file path = %q, want app/main.go", layer.Files[0].Path)
	}
}

func TestLoadFile_OCILayout(t *testing.T) {
	dir := buildOCILayout(t)

	img, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if img.Architecture != "amd64" {
		t.Errorf("Architecture = %q, want amd64", img.Architecture)
	}
	if img.OS != "linux" {
		t.Errorf("OS = %q, want linux", img.OS)
	}
	if img.Config.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q, want /app", img.Config.WorkingDir)
	}
	if len(img.RootFS.DiffIDs) != 1 || img.RootFS.DiffIDs[0] != "sha256:diff1" {
		t.Errorf("DiffIDs = %v, want [sha256:diff1]", img.RootFS.DiffIDs)
	}
	if len(img.Layers) != 1 {
		t.Fatalf("len(Layers) = %d, want 1", len(img.Layers))
	}
	if len(img.Layers[0].Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(img.Layers[0].Files))
	}
	if img.Layers[0].Files[0].Path != "app/main.go" {
		t.Errorf("file path = %q, want app/main.go", img.Layers[0].Files[0].Path)
	}
	if img.Layers[0].CreatedBy != "/bin/sh -c go build -o /app" {
		t.Errorf("CreatedBy = %q, want build command", img.Layers[0].CreatedBy)
	}
}

func TestLoadFile_PlainTar(t *testing.T) {
	p := buildPlainTar(t)

	img, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if len(img.Layers) != 1 {
		t.Fatalf("len(Layers) = %d, want 1", len(img.Layers))
	}
	if len(img.Layers[0].Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2", len(img.Layers[0].Files))
	}
	names := map[string]bool{}
	for _, f := range img.Layers[0].Files {
		names[f.Path] = true
	}
	if !names["hello.txt"] || !names["world.txt"] {
		t.Errorf("files = %v, want hello.txt and world.txt", names)
	}
}

func TestFormatString(t *testing.T) {
	tests := []struct {
		f    Format
		want string
	}{
		{FormatDockerSave, "docker-save"},
		{FormatOCILayout, "oci-layout"},
		{FormatPlainTar, "plain-tar"},
		{Format(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.f.String(); got != tt.want {
			t.Errorf("Format(%d).String() = %q, want %q", tt.f, got, tt.want)
		}
	}
}

// --- helpers ---

func writeTempFile(t *testing.T, pattern string, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), pattern)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

// buildDockerSaveTarWithConfig creates a docker-save tar that includes a config JSON.
func buildDockerSaveTarWithConfig(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Inner layer.tar.
	var layerBuf bytes.Buffer
	ltw := tar.NewWriter(&layerBuf)
	content := []byte("hello world")
	ltw.WriteHeader(&tar.Header{Name: "app/main.go", Size: int64(len(content)), Mode: 0644})
	ltw.Write(content)
	ltw.Close()
	layerBytes := layerBuf.Bytes()

	tw.WriteHeader(&tar.Header{Name: "abc123/layer.tar", Size: int64(len(layerBytes)), Mode: 0644})
	tw.Write(layerBytes)

	// Config JSON.
	cfg := ociImageConfig{
		Architecture: "amd64",
		OS:           "linux",
	}
	cfg.Config.WorkingDir = "/app"
	cfg.Config.Env = []string{"PATH=/usr/bin"}
	cfg.RootFS.Type = "layers"
	cfg.RootFS.DiffIDs = []string{"sha256:diff1"}
	cfg.History = []struct {
		CreatedBy  string `json:"created_by"`
		EmptyLayer bool   `json:"empty_layer"`
	}{
		{CreatedBy: "/bin/sh -c go build -o /app", EmptyLayer: false},
		{CreatedBy: "/bin/sh -c #(nop) CMD [\"/app\"]", EmptyLayer: true},
	}
	cfgData, _ := json.Marshal(cfg)
	tw.WriteHeader(&tar.Header{Name: "abc123.json", Size: int64(len(cfgData)), Mode: 0644})
	tw.Write(cfgData)

	// Manifest.
	manifest := []manifestItem{{
		Config:   "abc123.json",
		RepoTags: []string{"testimg:latest"},
		Layers:   []string{"abc123/layer.tar"},
	}}
	mdata, _ := json.Marshal(manifest)
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Size: int64(len(mdata)), Mode: 0644})
	tw.Write(mdata)

	tw.Close()
	return buf.Bytes()
}

// buildOCILayout creates a minimal OCI image layout directory.
func buildOCILayout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// oci-layout
	os.WriteFile(filepath.Join(dir, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`), 0644)

	// Create layer tar blob.
	var layerBuf bytes.Buffer
	ltw := tar.NewWriter(&layerBuf)
	content := []byte("hello world")
	ltw.WriteHeader(&tar.Header{Name: "app/main.go", Size: int64(len(content)), Mode: 0644})
	ltw.Write(content)
	ltw.Close()
	layerBytes := layerBuf.Bytes()
	layerDigest := "sha256:layeraaaa"

	// Config blob.
	cfg := ociImageConfig{
		Architecture: "amd64",
		OS:           "linux",
	}
	cfg.Config.WorkingDir = "/app"
	cfg.Config.Env = []string{"PATH=/usr/bin"}
	cfg.RootFS.Type = "layers"
	cfg.RootFS.DiffIDs = []string{"sha256:diff1"}
	cfg.History = []struct {
		CreatedBy  string `json:"created_by"`
		EmptyLayer bool   `json:"empty_layer"`
	}{
		{CreatedBy: "/bin/sh -c go build -o /app", EmptyLayer: false},
	}
	cfgData, _ := json.Marshal(cfg)
	configDigest := "sha256:configbbbb"

	// Manifest blob.
	mf := ociManifest{
		SchemaVersion: 2,
		Config: ociDescriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      int64(len(cfgData)),
		},
		Layers: []ociDescriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Digest:    layerDigest,
			Size:      int64(len(layerBytes)),
		}},
	}
	mfData, _ := json.Marshal(mf)
	manifestDigest := "sha256:manifestcccc"

	// index.json
	idx := ociIndex{
		SchemaVersion: 2,
		Manifests: []ociDescriptor{{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    manifestDigest,
			Size:      int64(len(mfData)),
		}},
	}
	idxData, _ := json.Marshal(idx)
	os.WriteFile(filepath.Join(dir, "index.json"), idxData, 0644)

	// Write blobs.
	blobsDir := filepath.Join(dir, "blobs", "sha256")
	os.MkdirAll(blobsDir, 0755)
	os.WriteFile(filepath.Join(blobsDir, "manifestcccc"), mfData, 0644)
	os.WriteFile(filepath.Join(blobsDir, "configbbbb"), cfgData, 0644)
	os.WriteFile(filepath.Join(blobsDir, "layeraaaa"), layerBytes, 0644)

	return dir
}

// buildPlainTar creates a plain tar file with two entries.
func buildPlainTar(t *testing.T) string {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, name := range []string{"hello.txt", "world.txt"} {
		content := []byte("content of " + name)
		tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0644})
		tw.Write(content)
	}
	tw.Close()

	return writeTempFile(t, "plain-*.tar", buf.Bytes())
}
