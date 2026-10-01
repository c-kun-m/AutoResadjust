package platform

import (
	"fmt"
	"math"
	"math/bits"
	"net/url"
	"sort"
	"time"
)

const NetworkProbeBytes = 1 << 20

// Membership is operator managed. Measurements never move a running deployment.
type NetworkGroup struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	NodeIDs       []string `json:"node_ids"`
	MaxRTTMS      float64  `json:"max_rtt_ms"`
	MaxJitterMS   float64  `json:"max_jitter_ms"`
	MinMbps       float64  `json:"min_mbps"`
	MaxAgeSeconds int      `json:"max_age_seconds"`
}

type NetworkProbe struct {
	ID        string    `json:"id"`
	SourceID  string    `json:"source_id"`
	TargetID  string    `json:"target_id"`
	SourceURL string    `json:"source_url"`
	TargetURL string    `json:"target_url"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type NetworkResult struct {
	ProbeID      string  `json:"probe_id"`
	RTTP95MS     float64 `json:"rtt_p95_ms"`
	JitterMS     float64 `json:"jitter_ms"`
	UploadMbps   float64 `json:"upload_mbps"`
	Samples      int     `json:"samples"`
	PayloadBytes int64   `json:"payload_bytes"`
	Error        string  `json:"error,omitempty"`
}

type NetworkLink struct {
	NetworkResult
	SourceID   string    `json:"source_id"`
	TargetID   string    `json:"target_id"`
	SourceURL  string    `json:"source_url"`
	TargetURL  string    `json:"target_url"`
	ObservedAt time.Time `json:"observed_at"`
	Status     string    `json:"status"`
}

func agentProbeURL(n ResourceNode) string {
	if n.Agent == nil || !n.Agent.NetworkProbe {
		return ""
	}
	u, err := url.Parse(n.Agent.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return n.Agent.URL
}

func finiteRange(v, min, max float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= min && v <= max
}

func (c *Controller) PutNetworkGroup(g NetworkGroup) (NetworkGroup, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !validCatalogID(g.ID) || g.Name == "" || len(g.Name) > 120 || len(g.NodeIDs) < 1 || len(g.NodeIDs) > 8 {
		return g, fmt.Errorf("group requires id, name and 1..8 nodes")
	}
	if g.MaxRTTMS == 0 {
		g.MaxRTTMS = 20
	}
	if g.MaxJitterMS == 0 {
		g.MaxJitterMS = 10
	}
	if g.MinMbps == 0 {
		g.MinMbps = 100
	}
	if g.MaxAgeSeconds == 0 {
		g.MaxAgeSeconds = 300
	}
	if !finiteRange(g.MaxRTTMS, 0.1, 5000) || !finiteRange(g.MaxJitterMS, 0.1, 5000) || !finiteRange(g.MinMbps, 0.1, 1000000) || g.MaxAgeSeconds < 30 || g.MaxAgeSeconds > 3600 {
		return g, fmt.Errorf("invalid link thresholds or freshness (30..3600 seconds)")
	}
	for _, d := range c.deployments {
		if d.Spec.NetworkGroup == g.ID && d.Phase != "stopped" && d.Phase != "failed" {
			return g, fmt.Errorf("stop deployments in this group before changing its policy")
		}
	}
	seen := map[string]bool{}
	for _, id := range g.NodeIDs {
		if _, ok := c.Nodes.GetNode(id); !ok || seen[id] {
			return g, fmt.Errorf("unknown or duplicate member %q", id)
		}
		seen[id] = true
	}
	if _, ok := c.networkGroups[g.ID]; !ok && len(c.networkGroups) >= 64 {
		return g, fmt.Errorf("network group limit reached")
	}
	g.NodeIDs = append([]string(nil), g.NodeIDs...)
	sort.Strings(g.NodeIDs)
	c.networkGroups[g.ID] = g
	g.NodeIDs = append([]string(nil), g.NodeIDs...)
	return g, nil
}

func (c *Controller) ListNetworkGroups() []NetworkGroup {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []NetworkGroup{}
	for _, g := range c.networkGroups {
		g.NodeIDs = append([]string(nil), g.NodeIDs...)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *Controller) networkMember(group string, n ResourceNode) bool {
	if g, ok := c.networkGroups[group]; ok {
		return contains(g.NodeIDs, n.ID)
	}
	return n.Agent != nil && n.Agent.NetworkGroup == group
}

func linkKey(source, target string) string { return source + "\x00" + target }

func (c *Controller) QueueNetworkProbe(source, target string) (NetworkProbe, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireNetworkProbes()
	now := c.now().UTC()
	a, aOK := c.Nodes.GetNode(source)
	b, bOK := c.Nodes.GetNode(target)
	if source == target || !aOK || !bOK || agentProbeURL(a) == "" || agentProbeURL(b) == "" || now.Sub(a.LastHeartbeat) > 35*time.Second || now.Sub(b.LastHeartbeat) > 35*time.Second {
		return NetworkProbe{}, fmt.Errorf("two distinct, fresh agents with network probe support are required")
	}
	for _, p := range c.networkProbes {
		if p.Status == "pending" || p.Status == "running" {
			// One measurement involving either host at a time avoids self-induced contention.
			if p.SourceID == source || p.TargetID == source || p.SourceID == target || p.TargetID == target {
				return p, fmt.Errorf("one of these nodes already has an active network probe")
			}
		}
		if p.SourceID == source && p.TargetID == target && now.Sub(p.CreatedAt) < 30*time.Second {
			return p, fmt.Errorf("wait 30 seconds between measurements of this directed link")
		}
	}
	if len(c.networkProbes) >= 128 {
		oldest := ""
		for id, p := range c.networkProbes {
			if p.Status != "pending" && p.Status != "running" && (oldest == "" || p.CreatedAt.Before(c.networkProbes[oldest].CreatedAt)) {
				oldest = id
			}
		}
		if oldest == "" {
			return NetworkProbe{}, fmt.Errorf("probe queue full")
		}
		delete(c.networkProbes, oldest)
	}
	p := NetworkProbe{ID: "probe-" + c.nextID(), SourceID: source, TargetID: target, SourceURL: agentProbeURL(a), TargetURL: agentProbeURL(b), Status: "pending", CreatedAt: now, ExpiresAt: now.Add(45 * time.Second)}
	c.networkProbes[p.ID] = p
	return p, nil
}

func (c *Controller) expireNetworkProbes() {
	for id, p := range c.networkProbes {
		if (p.Status == "pending" || p.Status == "running") && !c.now().Before(p.ExpiresAt) {
			p.Status = "failed"
			c.networkProbes[id] = p
			c.storeNetworkLink(p, NetworkResult{ProbeID: id, Error: "probe expired before acknowledgement"})
		}
	}
}

func (c *Controller) ListNetworkProbes() []NetworkProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireNetworkProbes()
	out := []NetworkProbe{}
	for _, p := range c.networkProbes {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Called only during authenticated, persisted agent sync. Delivery repeats until
// acknowledged; agents deduplicate IDs and never run a completed probe again.
func (c *Controller) NetworkAssignments(node string) []NetworkProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireNetworkProbes()
	out := []NetworkProbe{}
	for id, p := range c.networkProbes {
		if p.SourceID == node && (p.Status == "pending" || p.Status == "running") {
			p.Status = "running"
			c.networkProbes[id] = p
			out = append(out, p)
		}
	}
	return out
}

func (c *Controller) ReportNetwork(node string, results []NetworkResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireNetworkProbes()
	for _, r := range results {
		p, ok := c.networkProbes[r.ProbeID]
		if !ok || p.SourceID != node || (p.Status != "pending" && p.Status != "running") {
			continue
		}
		if len(r.Error) > 256 {
			r.Error = r.Error[:256]
		}
		if r.Error == "" && (r.Samples != 5 || r.PayloadBytes != NetworkProbeBytes || !finiteRange(r.RTTP95MS, 0.001, 15000) || !finiteRange(r.JitterMS, 0, 15000) || !finiteRange(r.UploadMbps, 0.001, 1000000)) {
			r = NetworkResult{ProbeID: p.ID, Error: "invalid measurement"}
		}
		p.Status = "completed"
		if r.Error != "" {
			p.Status = "failed"
		}
		c.networkProbes[p.ID] = p
		c.storeNetworkLink(p, r)
	}
}

func (c *Controller) storeNetworkLink(p NetworkProbe, r NetworkResult) {
	if len(c.networkLinks) >= 4096 {
		oldest := ""
		for k, l := range c.networkLinks {
			if oldest == "" || l.ObservedAt.Before(c.networkLinks[oldest].ObservedAt) {
				oldest = k
			}
		}
		delete(c.networkLinks, oldest)
	}
	status := "measured"
	if r.Error != "" {
		status = "failed"
	}
	c.networkLinks[linkKey(p.SourceID, p.TargetID)] = NetworkLink{NetworkResult: r, SourceID: p.SourceID, TargetID: p.TargetID, SourceURL: p.SourceURL, TargetURL: p.TargetURL, ObservedAt: c.now().UTC(), Status: status}
}

func (c *Controller) linkStatus(l NetworkLink, maxAge int) string {
	a, okA := c.Nodes.GetNode(l.SourceID)
	b, okB := c.Nodes.GetNode(l.TargetID)
	if !okA || !okB || agentProbeURL(a) != l.SourceURL || agentProbeURL(b) != l.TargetURL || c.now().Sub(a.LastHeartbeat) > 35*time.Second || c.now().Sub(b.LastHeartbeat) > 35*time.Second || l.ObservedAt.IsZero() || c.now().Sub(l.ObservedAt) > time.Duration(maxAge)*time.Second {
		return "stale"
	}
	return l.Status
}

func (c *Controller) ListNetworkLinks(groupIDs ...string) []NetworkLink {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireNetworkProbes()
	maxAge := 300
	if len(groupIDs) > 0 {
		if g, ok := c.networkGroups[groupIDs[0]]; ok {
			maxAge = g.MaxAgeSeconds
		}
	}
	out := []NetworkLink{}
	for _, l := range c.networkLinks {
		l.Status = c.linkStatus(l, maxAge)
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		return linkKey(out[i].SourceID, out[i].TargetID) < linkKey(out[j].SourceID, out[j].TargetID)
	})
	return out
}

func (c *Controller) networkPairReason(group, a, b string) string {
	g, ok := c.networkGroups[group]
	if !ok {
		return ""
	} // legacy labels remain compatible
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		l, ok := c.networkLinks[linkKey(pair[0], pair[1])]
		if !ok {
			return fmt.Sprintf("link %s → %s has not been measured", pair[0], pair[1])
		}
		if status := c.linkStatus(l, g.MaxAgeSeconds); status != "measured" {
			return fmt.Sprintf("link %s → %s is %s", pair[0], pair[1], status)
		}
		if l.RTTP95MS > g.MaxRTTMS || l.JitterMS > g.MaxJitterMS || l.UploadMbps < g.MinMbps {
			return fmt.Sprintf("link %s → %s below group policy: RTT %.2f ms, jitter %.2f ms, upload %.2f Mbps", pair[0], pair[1], l.RTTP95MS, l.JitterMS, l.UploadMbps)
		}
	}
	return ""
}

// Registered groups contain at most eight hosts. Enumerating their subsets
// avoids a greedy high-capacity, poorly connected host hiding a viable clique.
// Prefer fewer hops, then the lowest worst directed RTT among feasible groups.
func (c *Controller) planMeasuredRPC(s DeploymentSpec, plan DeploymentPlan, candidates []Placement, versions map[string]string) (DeploymentPlan, error) {
	if len(candidates) > 8 {
		return plan, fmt.Errorf("measured groups are limited to eight nodes")
	}
	var best []Placement
	bestRTT := math.Inf(1)
	var bestTotal int64
	for mask := 1; mask < 1<<len(candidates); mask++ {
		count := bits.OnesCount(uint(mask))
		if count < s.MinNodes || count > s.MaxNodes || len(best) > 0 && count > len(best) || len(s.NodeIDs) > 0 && count != len(s.NodeIDs) {
			continue
		}
		var ps []Placement
		var total int64
		for i, p := range candidates {
			if mask&(1<<i) != 0 {
				ps = append(ps, p)
				total += p.UsableMiB
			}
		}
		if total < s.WeightMiB || s.CoordinatorID != "" && ps[0].NodeID != s.CoordinatorID {
			continue
		}
		valid := true
		worstRTT := 0.0
		for i, a := range ps {
			if versions[a.NodeID] != versions[ps[0].NodeID] {
				valid = false
				break
			}
			for _, b := range ps[i+1:] {
				if reason := c.networkPairReason(s.NetworkGroup, a.NodeID, b.NodeID); reason != "" {
					plan.Rejections[b.NodeID] = []string{reason}
					valid = false
					break
				}
				for _, pair := range [][2]string{{a.NodeID, b.NodeID}, {b.NodeID, a.NodeID}} {
					worstRTT = math.Max(worstRTT, c.networkLinks[linkKey(pair[0], pair[1])].RTTP95MS)
				}
			}
			if !valid {
				break
			}
		}
		if valid && (len(best) == 0 || count < len(best) || worstRTT < bestRTT) {
			best = ps
			bestRTT = worstRTT
			bestTotal = total
		}
	}
	if len(best) == 0 {
		return plan, fmt.Errorf("no group satisfies capacity, engine version and fresh bidirectional network policy")
	}
	plan.Placements = best
	plan.TotalUsableMiB = bestTotal
	for i := range plan.Placements {
		p := &plan.Placements[i]
		p.Fraction = float64(p.UsableMiB) / float64(bestTotal)
		p.WeightShareMiB = int64(float64(s.WeightMiB) * p.Fraction)
		p.Coordinator = i == 0
		delete(plan.Rejections, p.NodeID)
	}
	return plan, nil
}
