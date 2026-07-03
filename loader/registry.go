package loader

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/eltaline/dockershrink/image"
)

const (
	defaultRegistry = "registry-1.docker.io"
	defaultTag      = "latest"
)

// Platform specifies the desired OS and architecture when resolving
// multi-arch manifest indexes.
type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

// DefaultPlatform returns the platform matching the current runtime.
func DefaultPlatform() Platform {
	return Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
}

// registryClient handles HTTP requests to an OCI / Docker v2 registry.
type registryClient struct {
	httpClient *http.Client
	registry   string
	repo       string
	token      string // bearer token obtained via auth challenge
}

// imageRef is a parsed image reference.
type imageRef struct {
	Registry string // e.g. "registry-1.docker.io"
	Repo     string // e.g. "library/nginx"
	Tag      string // e.g. "latest" (empty if digest is set)
	Digest   string // e.g. "sha256:abcdef..." (empty if tag is set)
}

// reference returns the tag or digest suitable for the manifests endpoint.
func (r imageRef) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// parseImageRef parses a Docker-style image reference into its components.
// Accepted forms:
//
//	nginx
//	nginx:1.25
//	library/nginx:1.25
//	ghcr.io/owner/repo:tag
//	registry.example.com/repo@sha256:abcdef...
func parseImageRef(raw string) imageRef {
	ref := imageRef{Tag: defaultTag}

	// Split off @digest if present.
	if idx := strings.LastIndex(raw, "@"); idx != -1 {
		ref.Digest = raw[idx+1:]
		ref.Tag = ""
		raw = raw[:idx]
	}

	// Split off :tag if present and no digest was specified.
	if ref.Digest == "" {
		if idx := strings.LastIndex(raw, ":"); idx != -1 {
			// Make sure the colon is not part of a port (e.g. localhost:5000/repo).
			afterColon := raw[idx+1:]
			if !strings.Contains(afterColon, "/") {
				ref.Tag = afterColon
				raw = raw[:idx]
			}
		}
	}

	// Determine registry vs repo.
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) == 1 {
		// Short name like "nginx" -> Docker Hub library image.
		ref.Registry = defaultRegistry
		ref.Repo = "library/" + parts[0]
	} else if isRegistryHost(parts[0]) {
		ref.Registry = parts[0]
		ref.Repo = parts[1]
	} else {
		// User/repo on Docker Hub, e.g. "myuser/myapp".
		ref.Registry = defaultRegistry
		ref.Repo = raw
	}

	return ref
}

// isRegistryHost returns true when the first segment of the image name
// looks like a registry hostname rather than a Docker Hub user/org name.
func isRegistryHost(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, ":") || s == "localhost"
}

// registryManifest is an OCI/Docker distribution manifest.
type registryManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Manifests     []indexDescriptor `json:"manifests,omitempty"` // present only in index/manifest list
}

// indexDescriptor extends ociDescriptor with platform metadata.
type indexDescriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  *descriptorPlatform `json:"platform,omitempty"`
}

type descriptorPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

const (
	mediaTypeDockerManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerManifestList   = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifestV1        = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCIIndex             = "application/vnd.oci.image.index.v1+json"
)

// acceptManifestTypes is the Accept header value for manifest requests.
var acceptManifestTypes = strings.Join([]string{
	mediaTypeDockerManifestV2,
	mediaTypeDockerManifestList,
	mediaTypeOCIManifestV1,
	mediaTypeOCIIndex,
}, ", ")

// LoadRegistry fetches an image from a remote container registry
// by reference (name:tag) or digest, without requiring docker pull.
// It supports multi-arch manifest indexes, selecting the platform
// that matches p (or DefaultPlatform() if p is zero-value).
func LoadRegistry(ctx context.Context, rawRef string, p Platform) (*image.Image, error) {
	if p == (Platform{}) {
		p = DefaultPlatform()
	}

	ref := parseImageRef(rawRef)

	rc := &registryClient{
		httpClient: &http.Client{Timeout: 5 * time.Minute},
		registry:   ref.Registry,
		repo:       ref.Repo,
	}

	// Authenticate (anonymous token for Docker Hub, or try unauthenticated).
	if err := rc.authenticate(ctx); err != nil {
		return nil, fmt.Errorf("registry auth %s: %w", ref.Registry, err)
	}

	// Fetch the manifest (may be an index or a single manifest).
	mf, mediaType, err := rc.fetchManifest(ctx, ref.reference())
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", rawRef, err)
	}

	// If this is a manifest index / list, resolve to the right platform.
	if isIndex(mediaType, mf) {
		desc, err := selectPlatform(mf.Manifests, p)
		if err != nil {
			return nil, fmt.Errorf("select platform for %s: %w", rawRef, err)
		}
		mf, _, err = rc.fetchManifest(ctx, desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("fetch platform manifest %s: %w", desc.Digest, err)
		}
	}

	// Fetch config blob.
	cfgData, err := rc.fetchBlob(ctx, mf.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("fetch config %s: %w", mf.Config.Digest, err)
	}
	var cfg ociImageConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
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

	// Fetch and extract each layer.
	layers := make([]image.Layer, len(mf.Layers))
	for i, ld := range mf.Layers {
		l := image.Layer{
			CreatedBy: createdByMap[i],
		}
		if i < len(cfg.RootFS.DiffIDs) {
			l.DiffID = cfg.RootFS.DiffIDs[i]
		}

		files, size, err := rc.fetchAndExtractLayer(ctx, ld.Digest)
		if err != nil {
			return nil, fmt.Errorf("fetch layer %s: %w", ld.Digest, err)
		}
		l.Files = files
		l.Size = size
		layers[i] = l
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
		Layers: layers,
	}

	if cfg.Created != "" {
		if t, err := time.Parse(time.RFC3339Nano, cfg.Created); err == nil {
			img.Created = t
		}
	}

	return img, nil
}

// authenticate obtains a bearer token via the WWW-Authenticate challenge flow.
// For registries that don't require auth, this is a no-op.
func (rc *registryClient) authenticate(ctx context.Context) error {
	// Probe the registry with a v2 check.
	url := fmt.Sprintf("https://%s/v2/", rc.registry)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := rc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("v2 check: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil // no auth needed
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	// Parse WWW-Authenticate header.
	challenge := resp.Header.Get("WWW-Authenticate")
	realm, params := parseBearerChallenge(challenge)
	if realm == "" {
		return fmt.Errorf("no bearer realm in challenge: %s", challenge)
	}

	// Request a token.
	tokenURL := realm + "?"
	if svc, ok := params["service"]; ok {
		tokenURL += "service=" + svc + "&"
	}
	tokenURL += "scope=repository:" + rc.repo + ":pull"

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return err
	}
	resp, err = rc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("token endpoint status %d: %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("decode token: %w", err)
	}
	rc.token = tokenResp.Token
	if rc.token == "" {
		rc.token = tokenResp.AccessToken
	}

	return nil
}

// parseBearerChallenge extracts realm and parameters from a
// WWW-Authenticate: Bearer ... header value.
func parseBearerChallenge(header string) (realm string, params map[string]string) {
	params = make(map[string]string)
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return "", params
	}
	header = header[len("bearer "):]

	// Simple key="value" parser.
	for header != "" {
		header = strings.TrimLeft(header, " ,")
		eq := strings.Index(header, "=")
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(header[:eq])
		header = header[eq+1:]

		var val string
		if len(header) > 0 && header[0] == '"' {
			// Quoted value.
			header = header[1:]
			end := strings.Index(header, "\"")
			if end < 0 {
				val = header
				header = ""
			} else {
				val = header[:end]
				header = header[end+1:]
			}
		} else {
			end := strings.IndexAny(header, ", ")
			if end < 0 {
				val = header
				header = ""
			} else {
				val = header[:end]
				header = header[end:]
			}
		}

		params[strings.ToLower(key)] = val
		if strings.ToLower(key) == "realm" {
			realm = val
		}
	}

	return realm, params
}

// doRegistryRequest performs an authenticated GET request to the registry.
func (rc *registryClient) doRegistryRequest(ctx context.Context, url string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if rc.token != "" {
		req.Header.Set("Authorization", "Bearer "+rc.token)
	}
	resp, err := rc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("registry %s: status %d: %s", url, resp.StatusCode, body)
	}
	return resp, nil
}

// fetchManifest retrieves a manifest by tag or digest.
func (rc *registryClient) fetchManifest(ctx context.Context, ref string) (*registryManifest, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", rc.registry, rc.repo, ref)
	resp, err := rc.doRegistryRequest(ctx, url, acceptManifestTypes)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")

	var mf registryManifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, "", fmt.Errorf("decode manifest: %w", err)
	}
	// Prefer Content-Type from response; fall back to mediaType in body.
	if contentType != "" {
		mf.MediaType = contentType
	}

	return &mf, mf.MediaType, nil
}

// fetchBlob retrieves a blob by digest and returns its raw bytes.
func (rc *registryClient) fetchBlob(ctx context.Context, digest string) ([]byte, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", rc.registry, rc.repo, digest)
	resp, err := rc.doRegistryRequest(ctx, url, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// fetchAndExtractLayer downloads a layer blob (usually gzipped tar) and
// extracts its file entries without writing to disk.
func (rc *registryClient) fetchAndExtractLayer(ctx context.Context, digest string) ([]image.FileEntry, int64, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", rc.registry, rc.repo, digest)
	resp, err := rc.doRegistryRequest(ctx, url, "")
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	// Try gzip decompression; fall back to plain tar on error.
	var reader io.Reader
	gr, gzErr := gzip.NewReader(resp.Body)
	if gzErr != nil {
		// Not gzip — treat as plain tar. But we already consumed bytes
		// from resp.Body trying gzip, so this won't work for streaming.
		// In practice, registry layers are almost always gzipped.
		return nil, 0, fmt.Errorf("decompress layer %s: %w", digest, gzErr)
	}
	defer gr.Close()
	reader = gr

	tr := tar.NewReader(reader)
	var files []image.FileEntry
	var totalSize int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read layer tar %s: %w", digest, err)
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

// isIndex returns true if the manifest is a manifest list or OCI index.
func isIndex(mediaType string, mf *registryManifest) bool {
	switch mediaType {
	case mediaTypeDockerManifestList, mediaTypeOCIIndex:
		return true
	}
	// Fallback: if manifests array is populated, treat as index.
	return len(mf.Manifests) > 0 && mf.Config.Digest == ""
}

// selectPlatform picks the manifest descriptor matching the desired platform.
func selectPlatform(descs []indexDescriptor, p Platform) (*indexDescriptor, error) {
	// First pass: exact match including variant.
	for i := range descs {
		d := &descs[i]
		if d.Platform == nil {
			continue
		}
		if d.Platform.OS == p.OS && d.Platform.Architecture == p.Architecture {
			if p.Variant == "" || d.Platform.Variant == p.Variant {
				return d, nil
			}
		}
	}
	// Second pass: match OS+arch, ignore variant.
	for i := range descs {
		d := &descs[i]
		if d.Platform == nil {
			continue
		}
		if d.Platform.OS == p.OS && d.Platform.Architecture == p.Architecture {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no manifest for platform %s/%s", p.OS, p.Architecture)
}
