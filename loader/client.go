package loader

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	defaultSocketPath = "/var/run/docker.sock"
	defaultAPIVersion = "v1.43"
)

// Client talks to the Docker Engine API over a Unix socket.
type Client struct {
	httpClient *http.Client
	version    string
}

// NewClient creates a Client that connects to the Docker daemon at socketPath.
// If socketPath is empty, the default /var/run/docker.sock is used.
func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = defaultSocketPath
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &Client{
		httpClient: &http.Client{Transport: transport},
		version:    defaultAPIVersion,
	}
}

// get performs a GET request against the Docker Engine API.
func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	url := fmt.Sprintf("http://docker/%s%s", c.version, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker API request %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("docker API %s: status %d: %s", path, resp.StatusCode, body)
	}
	return resp, nil
}
