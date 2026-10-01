package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type snapshot struct {
	Version        int                          `json:"version"`
	Nodes          map[string]ResourceNode      `json:"nodes"`
	Tasks          map[string]Task              `json:"tasks"`
	Allocations    map[string]map[string]string `json:"allocations"`
	Blocked        map[string]bool              `json:"blocked"`
	Keys           map[string]string            `json:"keys"`
	Deployments    map[string]Deployment        `json:"deployments"`
	DeploymentKeys map[string]string            `json:"deployment_keys"`
	Artifacts      map[string]ModelArtifact     `json:"artifacts,omitempty"`
	NetworkGroups  map[string]NetworkGroup      `json:"network_groups,omitempty"`
	NetworkLinks   map[string]NetworkLink       `json:"network_links,omitempty"`
}

// Save holds the controller lock through replacement, preventing an older
// concurrent checkpoint from overwriting a newer one. This is single-process
// persistence; running multiple control planes on one file is unsupported.
func (c *Controller) Save(path string) error {
	if path == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Nodes.mu.RLock()
	defer c.Nodes.mu.RUnlock()
	data, err := json.Marshal(snapshot{Version: 3, Nodes: c.Nodes.nodes, Tasks: c.Nodes.tasks, Allocations: c.Nodes.allocations, Blocked: c.Nodes.blocked, Keys: c.keys, Deployments: c.deployments, DeploymentKeys: c.deploymentKeys, Artifacts: c.artifacts, NetworkGroups: c.networkGroups, NetworkLinks: c.networkLinks})
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".platform-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadController(path string) (*Controller, error) {
	c := NewController(nil)
	if path == "" {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err = json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 && s.Version != 2 && s.Version != 3 {
		return nil, fmt.Errorf("unsupported state version %d", s.Version)
	}
	if s.Nodes != nil {
		c.Nodes.nodes = s.Nodes
	}
	if s.Tasks != nil {
		c.Nodes.tasks = s.Tasks
	}
	if s.Allocations != nil {
		c.Nodes.allocations = s.Allocations
	}
	if s.Blocked != nil {
		c.Nodes.blocked = s.Blocked
	}
	if s.Keys != nil {
		c.keys = s.Keys
	}
	if s.Deployments != nil {
		c.deployments = s.Deployments
	}
	if s.DeploymentKeys != nil {
		c.deploymentKeys = s.DeploymentKeys
	}
	if s.Artifacts != nil {
		c.artifacts = s.Artifacts
	}
	if s.NetworkGroups != nil {
		c.networkGroups = s.NetworkGroups
	}
	if s.NetworkLinks != nil {
		c.networkLinks = s.NetworkLinks
	}
	for k, l := range c.networkLinks {
		l.ObservedAt = time.Time{}
		c.networkLinks[k] = l
	}
	for id, n := range c.Nodes.nodes {
		n.LastHeartbeat = time.Time{}
		c.Nodes.nodes[id] = n
	}
	for id, d := range c.deployments {
		if d.Spec.Backend == "" {
			d.Spec.Backend = BackendRPC
		}
		for node, r := range d.Workers {
			r.ObservedAt = time.Time{}
			d.Workers[node] = r
		}
		if d.Phase == "ready" || d.Phase == "loading" {
			d.Phase = "degraded"
			d.Message = "control plane restarted; waiting for worker reconciliation"
		}
		c.deployments[id] = d
	}
	return c, nil
}
