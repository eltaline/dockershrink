package loader

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// fakeDockerServer spins up an HTTP server on a temp Unix socket
// that responds to the three endpoints Load needs.
func fakeDockerServer(t *testing.T, inspect inspectResponse, history []historyEntry, imageArchive []byte) (socketPath string, cleanup func()) {
	t.Helper()

	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.43/images/testimg/json":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(inspect)
		case r.URL.Path == "/v1.43/images/testimg/history":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(history)
		case r.URL.Path == "/v1.43/images/testimg/get":
			w.Header().Set("Content-Type", "application/x-tar")
			w.Write(imageArchive)
		default:
			http.NotFound(w, r)
		}
	})

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	return sock, func() {
		srv.Close()
		ln.Close()
		os.RemoveAll(dir)
	}
}

// buildImageTar creates a minimal docker-save-style tar archive in memory.
func buildImageTar(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Create inner layer.tar with one file.
	var layerBuf bytes.Buffer
	ltw := tar.NewWriter(&layerBuf)
	content := []byte("hello world")
	ltw.WriteHeader(&tar.Header{
		Name: "app/main.go",
		Size: int64(len(content)),
		Mode: 0644,
	})
	ltw.Write(content)
	ltw.Close()
	layerBytes := layerBuf.Bytes()

	// Write layer.tar into outer tar.
	tw.WriteHeader(&tar.Header{
		Name: "abc123/layer.tar",
		Size: int64(len(layerBytes)),
		Mode: 0644,
	})
	tw.Write(layerBytes)

	// Write manifest.json.
	manifest := []manifestItem{{
		Config:   "abc123.json",
		RepoTags: []string{"testimg:latest"},
		Layers:   []string{"abc123/layer.tar"},
	}}
	mdata, _ := json.Marshal(manifest)
	tw.WriteHeader(&tar.Header{
		Name: "manifest.json",
		Size: int64(len(mdata)),
		Mode: 0644,
	})
	tw.Write(mdata)

	tw.Close()
	return buf.Bytes()
}

func TestLoad(t *testing.T) {
	inspect := inspectResponse{
		ID:           "sha256:abc123",
		RepoTags:     []string{"testimg:latest"},
		Architecture: "amd64",
		OS:           "linux",
		Size:         1024,
	}
	inspect.RootFS.Type = "layers"
	inspect.RootFS.Layers = []string{"sha256:diff1"}
	inspect.Config.Env = []string{"PATH=/usr/bin"}
	inspect.Config.WorkingDir = "/app"

	history := []historyEntry{
		{CreatedBy: "/bin/sh -c #(nop) CMD [\"/app\"]", Size: 0},
		{CreatedBy: "/bin/sh -c go build -o /app", Size: 500},
	}

	archive := buildImageTar(t)
	sock, cleanup := fakeDockerServer(t, inspect, history, archive)
	defer cleanup()

	img, err := Load(context.Background(), "testimg", sock)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if img.ID != "sha256:abc123" {
		t.Errorf("ID = %q, want sha256:abc123", img.ID)
	}
	if img.Architecture != "amd64" {
		t.Errorf("Architecture = %q, want amd64", img.Architecture)
	}
	if img.Config.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q, want /app", img.Config.WorkingDir)
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
	if layer.Files[0].Size != 11 {
		t.Errorf("file size = %d, want 11", layer.Files[0].Size)
	}
}

func TestBuildCreatedByIndex(t *testing.T) {
	history := []historyEntry{
		{CreatedBy: "CMD", Size: 0},
		{CreatedBy: "COPY files", Size: 200},
		{CreatedBy: "RUN apt install", Size: 100},
		{CreatedBy: "FROM base", Size: 0},
	}
	m := buildCreatedByIndex(history)
	if m[0] != "RUN apt install" {
		t.Errorf("index 0 = %q, want RUN apt install", m[0])
	}
	if m[1] != "COPY files" {
		t.Errorf("index 1 = %q, want COPY files", m[1])
	}
	if len(m) != 2 {
		t.Errorf("len = %d, want 2", len(m))
	}
}
