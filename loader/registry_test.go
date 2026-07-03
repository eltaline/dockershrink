package loader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		input string
		want  imageRef
	}{
		{
			input: "nginx",
			want:  imageRef{Registry: "registry-1.docker.io", Repo: "library/nginx", Tag: "latest"},
		},
		{
			input: "nginx:1.25",
			want:  imageRef{Registry: "registry-1.docker.io", Repo: "library/nginx", Tag: "1.25"},
		},
		{
			input: "myuser/myapp:v2",
			want:  imageRef{Registry: "registry-1.docker.io", Repo: "myuser/myapp", Tag: "v2"},
		},
		{
			input: "ghcr.io/owner/repo:latest",
			want:  imageRef{Registry: "ghcr.io", Repo: "owner/repo", Tag: "latest"},
		},
		{
			input: "registry.example.com/repo@sha256:abcdef",
			want:  imageRef{Registry: "registry.example.com", Repo: "repo", Tag: "", Digest: "sha256:abcdef"},
		},
		{
			input: "localhost:5000/myimg:dev",
			want:  imageRef{Registry: "localhost:5000", Repo: "myimg", Tag: "dev"},
		},
		{
			input: "nginx@sha256:abc123",
			want:  imageRef{Registry: "registry-1.docker.io", Repo: "library/nginx", Tag: "", Digest: "sha256:abc123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseImageRef(tt.input)
			if got != tt.want {
				t.Errorf("parseImageRef(%q) =\n  %+v\nwant\n  %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseBearerChallenge(t *testing.T) {
	header := `Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:lib/nginx:pull"`
	realm, params := parseBearerChallenge(header)

	if realm != "https://auth.example.com/token" {
		t.Errorf("realm = %q", realm)
	}
	if params["service"] != "registry.example.com" {
		t.Errorf("service = %q", params["service"])
	}
	if params["scope"] != "repository:lib/nginx:pull" {
		t.Errorf("scope = %q", params["scope"])
	}
}

func TestSelectPlatform(t *testing.T) {
	descs := []indexDescriptor{
		{
			Digest:    "sha256:arm64manifest",
			MediaType: mediaTypeDockerManifestV2,
			Platform:  &descriptorPlatform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		},
		{
			Digest:    "sha256:amd64manifest",
			MediaType: mediaTypeDockerManifestV2,
			Platform:  &descriptorPlatform{OS: "linux", Architecture: "amd64"},
		},
	}

	t.Run("exact match", func(t *testing.T) {
		d, err := selectPlatform(descs, Platform{OS: "linux", Architecture: "amd64"})
		if err != nil {
			t.Fatal(err)
		}
		if d.Digest != "sha256:amd64manifest" {
			t.Errorf("digest = %q, want sha256:amd64manifest", d.Digest)
		}
	})

	t.Run("variant match", func(t *testing.T) {
		d, err := selectPlatform(descs, Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
		if err != nil {
			t.Fatal(err)
		}
		if d.Digest != "sha256:arm64manifest" {
			t.Errorf("digest = %q, want sha256:arm64manifest", d.Digest)
		}
	})

	t.Run("no variant specified falls back", func(t *testing.T) {
		d, err := selectPlatform(descs, Platform{OS: "linux", Architecture: "arm64"})
		if err != nil {
			t.Fatal(err)
		}
		if d.Digest != "sha256:arm64manifest" {
			t.Errorf("digest = %q, want sha256:arm64manifest", d.Digest)
		}
	})

	t.Run("no match", func(t *testing.T) {
		_, err := selectPlatform(descs, Platform{OS: "windows", Architecture: "amd64"})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestIsIndex(t *testing.T) {
	t.Run("docker manifest list", func(t *testing.T) {
		mf := &registryManifest{Manifests: []indexDescriptor{{}}}
		if !isIndex(mediaTypeDockerManifestList, mf) {
			t.Error("expected true for docker manifest list")
		}
	})

	t.Run("oci index", func(t *testing.T) {
		mf := &registryManifest{Manifests: []indexDescriptor{{}}}
		if !isIndex(mediaTypeOCIIndex, mf) {
			t.Error("expected true for OCI index")
		}
	})

	t.Run("single manifest", func(t *testing.T) {
		mf := &registryManifest{Config: ociDescriptor{Digest: "sha256:abc"}}
		if isIndex(mediaTypeDockerManifestV2, mf) {
			t.Error("expected false for single manifest")
		}
	})

	t.Run("fallback detection", func(t *testing.T) {
		mf := &registryManifest{Manifests: []indexDescriptor{{}}}
		if !isIndex("", mf) {
			t.Error("expected true for fallback index detection")
		}
	})
}

// buildGzippedLayerTar creates a gzipped tar with a single file.
func buildGzippedLayerTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("hello registry")
	tw.WriteHeader(&tar.Header{Name: "app/server.go", Size: int64(len(content)), Mode: 0644})
	tw.Write(content)

	tw.Close()
	gw.Close()
	return buf.Bytes()
}

// fakeRegistryServer creates an httptest server that implements the
// Docker Registry HTTP API v2 enough for LoadRegistry to work.
func fakeRegistryServer(t *testing.T, multiArch bool) *httptest.Server {
	t.Helper()

	// Build layer blob.
	layerBlob := buildGzippedLayerTar(t)
	layerDigest := "sha256:layerdigest1234"

	// Build config blob.
	cfg := ociImageConfig{
		Architecture: "amd64",
		OS:           "linux",
		Created:      "2024-01-01T00:00:00Z",
	}
	cfg.Config.WorkingDir = "/app"
	cfg.Config.Env = []string{"PATH=/usr/bin"}
	cfg.RootFS.Type = "layers"
	cfg.RootFS.DiffIDs = []string{"sha256:diffaaa"}
	cfg.History = []struct {
		CreatedBy  string `json:"created_by"`
		EmptyLayer bool   `json:"empty_layer"`
	}{
		{CreatedBy: "COPY . /app", EmptyLayer: false},
	}
	cfgData, _ := json.Marshal(cfg)
	configDigest := "sha256:configdigest5678"

	// Build the single-platform manifest.
	singleManifest := registryManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeDockerManifestV2,
		Config: ociDescriptor{
			MediaType: "application/vnd.docker.container.image.v1+json",
			Digest:    configDigest,
			Size:      int64(len(cfgData)),
		},
		Layers: []ociDescriptor{{
			MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip",
			Digest:    layerDigest,
			Size:      int64(len(layerBlob)),
		}},
	}
	singleManifestData, _ := json.Marshal(singleManifest)
	singleManifestDigest := "sha256:singlemanifest"

	// Build multi-arch index.
	indexManifest := registryManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeDockerManifestList,
		Manifests: []indexDescriptor{
			{
				MediaType: mediaTypeDockerManifestV2,
				Digest:    singleManifestDigest,
				Size:      int64(len(singleManifestData)),
				Platform:  &descriptorPlatform{OS: "linux", Architecture: "amd64"},
			},
			{
				MediaType: mediaTypeDockerManifestV2,
				Digest:    "sha256:arm64manifest",
				Size:      int64(len(singleManifestData)),
				Platform:  &descriptorPlatform{OS: "linux", Architecture: "arm64", Variant: "v8"},
			},
		},
	}
	indexData, _ := json.Marshal(indexManifest)

	mux := http.NewServeMux()

	// v2 check — no auth required for testing.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	// Manifests endpoint.
	mux.HandleFunc("/v2/library/testimg/manifests/", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Path[len("/v2/library/testimg/manifests/"):]
		switch {
		case ref == "latest" && multiArch:
			w.Header().Set("Content-Type", mediaTypeDockerManifestList)
			w.Write(indexData)
		case ref == "latest" && !multiArch:
			w.Header().Set("Content-Type", mediaTypeDockerManifestV2)
			w.Write(singleManifestData)
		case ref == singleManifestDigest:
			w.Header().Set("Content-Type", mediaTypeDockerManifestV2)
			w.Write(singleManifestData)
		default:
			http.NotFound(w, r)
		}
	})

	// Blobs endpoint.
	mux.HandleFunc("/v2/library/testimg/blobs/", func(w http.ResponseWriter, r *http.Request) {
		digest := r.URL.Path[len("/v2/library/testimg/blobs/"):]
		switch digest {
		case configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(cfgData)
		case layerDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(layerBlob)
		default:
			http.NotFound(w, r)
		}
	})

	return httptest.NewServer(mux)
}

func TestLoadRegistry_SingleManifest(t *testing.T) {
	srv := fakeRegistryServer(t, false)
	defer srv.Close()

	// Override registry URL — strip "http://" and use server address as registry.
	addr := srv.Listener.Addr().String()

	rc := &registryClient{
		httpClient: srv.Client(),
		registry:   addr,
		repo:       "library/testimg",
	}

	// Override to use http instead of https by using a custom transport.
	rc.httpClient = &http.Client{
		Transport: &httpTransportOverride{addr: addr, wrapped: http.DefaultTransport},
	}

	ctx := context.Background()

	// Fetch manifest.
	mf, mediaType, err := rc.fetchManifest(ctx, "latest")
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if isIndex(mediaType, mf) {
		t.Fatal("expected single manifest, got index")
	}
	if mf.Config.Digest != "sha256:configdigest5678" {
		t.Errorf("config digest = %q", mf.Config.Digest)
	}

	// Fetch config.
	cfgData, err := rc.fetchBlob(ctx, mf.Config.Digest)
	if err != nil {
		t.Fatalf("fetchBlob config: %v", err)
	}
	var cfg ociImageConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Architecture != "amd64" {
		t.Errorf("Architecture = %q", cfg.Architecture)
	}

	// Fetch and extract layer.
	files, size, err := rc.fetchAndExtractLayer(ctx, mf.Layers[0].Digest)
	if err != nil {
		t.Fatalf("fetchAndExtractLayer: %v", err)
	}
	if len(files) != 1 || files[0].Path != "app/server.go" {
		t.Errorf("files = %+v", files)
	}
	if size != 14 {
		t.Errorf("size = %d, want 14", size)
	}
}

func TestLoadRegistry_MultiArch(t *testing.T) {
	srv := fakeRegistryServer(t, true)
	defer srv.Close()

	addr := srv.Listener.Addr().String()

	rc := &registryClient{
		httpClient: &http.Client{
			Transport: &httpTransportOverride{addr: addr, wrapped: http.DefaultTransport},
		},
		registry: addr,
		repo:     "library/testimg",
	}

	ctx := context.Background()

	// Fetch index.
	mf, mediaType, err := rc.fetchManifest(ctx, "latest")
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if !isIndex(mediaType, mf) {
		t.Fatal("expected index, got single manifest")
	}

	// Select amd64 platform.
	desc, err := selectPlatform(mf.Manifests, Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("selectPlatform: %v", err)
	}
	if desc.Digest != "sha256:singlemanifest" {
		t.Errorf("selected digest = %q", desc.Digest)
	}

	// Fetch the resolved manifest.
	resolved, resolvedType, err := rc.fetchManifest(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("fetchManifest resolved: %v", err)
	}
	if isIndex(resolvedType, resolved) {
		t.Fatal("resolved manifest should not be an index")
	}
	if len(resolved.Layers) != 1 {
		t.Errorf("layers count = %d, want 1", len(resolved.Layers))
	}
}

func TestLoadRegistry_Integration(t *testing.T) {
	srv := fakeRegistryServer(t, true)
	defer srv.Close()

	addr := srv.Listener.Addr().String()

	// Temporarily override defaultRegistry — we use the raw registryClient
	// approach instead, since LoadRegistry hardcodes https.
	rc := &registryClient{
		httpClient: &http.Client{
			Transport: &httpTransportOverride{addr: addr, wrapped: http.DefaultTransport},
		},
		registry: addr,
		repo:     "library/testimg",
	}

	ctx := context.Background()
	p := Platform{OS: "linux", Architecture: "amd64"}

	// Replicate LoadRegistry logic manually with our test client.
	mf, mediaType, err := rc.fetchManifest(ctx, "latest")
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}

	if isIndex(mediaType, mf) {
		desc, err := selectPlatform(mf.Manifests, p)
		if err != nil {
			t.Fatalf("selectPlatform: %v", err)
		}
		mf, _, err = rc.fetchManifest(ctx, desc.Digest)
		if err != nil {
			t.Fatalf("fetchManifest platform: %v", err)
		}
	}

	cfgData, err := rc.fetchBlob(ctx, mf.Config.Digest)
	if err != nil {
		t.Fatalf("fetchBlob: %v", err)
	}
	var cfg ociImageConfig
	json.Unmarshal(cfgData, &cfg)

	if cfg.Architecture != "amd64" {
		t.Errorf("Architecture = %q", cfg.Architecture)
	}
	if cfg.Config.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q", cfg.Config.WorkingDir)
	}
	if len(cfg.RootFS.DiffIDs) != 1 {
		t.Fatalf("DiffIDs count = %d", len(cfg.RootFS.DiffIDs))
	}

	files, _, err := rc.fetchAndExtractLayer(ctx, mf.Layers[0].Digest)
	if err != nil {
		t.Fatalf("fetchAndExtractLayer: %v", err)
	}
	if len(files) != 1 || files[0].Path != "app/server.go" {
		t.Errorf("files = %+v", files)
	}
}

// httpTransportOverride rewrites https requests to the fake server's http address.
type httpTransportOverride struct {
	addr    string
	wrapped http.RoundTripper
}

func (t *httpTransportOverride) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = "http"
	req2.URL.Host = t.addr
	req2.Host = t.addr
	resp, err := t.wrapped.RoundTrip(req2)
	if err != nil {
		return nil, fmt.Errorf("test transport: %w", err)
	}
	return resp, nil
}
