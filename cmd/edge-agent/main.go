package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/resource-adjust/compute-platform/internal/platform"
)

type heartbeatClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func (c heartbeatClient) send(ctx context.Context, node platform.ResourceNode) error {
	body, err := json.Marshal(node)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+"/api/v1/resources/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(message) > 0 {
			return fmt.Errorf("control plane heartbeat returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
		}
		return fmt.Errorf("control plane heartbeat returned %s", resp.Status)
	}
	return nil
}

func validateControlPlaneURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("control-plane must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("control-plane URL scheme %q is unsupported", u.Scheme)
	}
	return nil
}

func main() { runAgent() }
