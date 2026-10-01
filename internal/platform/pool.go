package platform

import (
	"fmt"
	"sort"
)

// A pool pins an immutable artifact and backend. Membership may change, but an
// existing public model name can never silently select different weights.
type ModelPool struct {
	ID            string   `json:"id"`
	ModelRef      string   `json:"model_ref"`
	Backend       string   `json:"backend"`
	DeploymentIDs []string `json:"deployment_ids"`
}

func (c *Controller) PutModelPool(p ModelPool) (ModelPool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !validCatalogID(p.ID) || len(p.DeploymentIDs) < 1 || len(p.DeploymentIDs) > 32 {
		return p, fmt.Errorf("pool requires a valid id and 1..32 deployments")
	}
	if _, ok := c.artifacts[p.ModelRef]; !ok {
		return p, fmt.Errorf("pool requires a registered immutable model_ref")
	}
	if previous, ok := c.modelPools[p.ID]; ok {
		if previous.ModelRef != p.ModelRef || previous.Backend != p.Backend {
			return p, fmt.Errorf("pool model identity is immutable; use a new pool id")
		}
	} else if len(c.modelPools) >= 64 {
		return p, fmt.Errorf("model pool limit reached")
	}
	seen := map[string]bool{}
	for _, id := range p.DeploymentIDs {
		d, ok := c.deployments[id]
		if !ok || d.Spec.ModelRef != p.ModelRef || d.Spec.Backend != p.Backend || seen[id] {
			return p, fmt.Errorf("deployments must be unique and share the pool artifact and backend")
		}
		seen[id] = true
	}
	p.DeploymentIDs = append([]string(nil), p.DeploymentIDs...)
	sort.Strings(p.DeploymentIDs)
	c.modelPools[p.ID] = p
	p.DeploymentIDs = append([]string(nil), p.DeploymentIDs...)
	return p, nil
}

func (c *Controller) ListModelPools() []ModelPool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ModelPool, 0, len(c.modelPools))
	for _, p := range c.modelPools {
		p.DeploymentIDs = append([]string(nil), p.DeploymentIDs...)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
