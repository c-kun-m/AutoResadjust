package platform

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const PetalsProfile = "petals-22afba6-torch2.8-cu128-bnb0.48.1-v1"

type PetalsEndpoint struct {
	PeerID       string   `json:"peer_id"`
	Address      string   `json:"address"`
	InitialPeers []string `json:"initial_peers"`
}

var peerPattern = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,128}$`)

func PrivatePetalsIP(value string) bool {
	ip := net.ParseIP(value)
	if ip == nil {
		return false
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "100.64.0.0/10", "fc00::/7", "::1/128"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func ValidatePetalsEndpoint(p *PetalsEndpoint) error {
	if p == nil || !peerPattern.MatchString(p.PeerID) || len(p.InitialPeers) < 1 || len(p.InitialPeers) > 8 {
		return fmt.Errorf("persistent peer ID and 1..8 private bootstrap peers required")
	}
	host, port, err := net.SplitHostPort(p.Address)
	value, parseErr := strconv.Atoi(port)
	if err != nil || parseErr != nil || !PrivatePetalsIP(host) || value < 1 || value > 65535 {
		return fmt.Errorf("Petals endpoint requires a private literal IP and TCP port")
	}
	seen := map[string]bool{}
	for _, peer := range p.InitialPeers {
		parts := strings.Split(peer, "/")
		if len(parts) != 7 || parts[0] != "" || (parts[1] != "ip4" && parts[1] != "ip6") || parts[3] != "tcp" || parts[5] != "p2p" || !PrivatePetalsIP(parts[2]) || !peerPattern.MatchString(parts[6]) {
			return fmt.Errorf("invalid private bootstrap multiaddress")
		}
		v, e := strconv.Atoi(parts[4])
		if e != nil || v < 1 || v > 65535 || (net.ParseIP(parts[2]).To4() != nil) != (parts[1] == "ip4") || seen[peer] {
			return fmt.Errorf("invalid or duplicate bootstrap address")
		}
		seen[peer] = true
	}
	return nil
}

func (c *Controller) planPetalsDeployment(s DeploymentSpec) (DeploymentPlan, error) {
	p := DeploymentPlan{Placements: []Placement{}, Rejections: map[string][]string{}, Warning: "Fixed contiguous Petals blocks on a private swarm. GPU/RAM budgets are estimates; all files, worker health and full-model warmup must pass. Requests are never replayed after a worker failure."}
	if _, ok := c.networkGroups[s.NetworkGroup]; !ok {
		return p, fmt.Errorf("petals requires a registered measured network group")
	}
	nodes := c.Nodes.ListNodes()
	owners, peers, addresses := map[string]int{}, map[string]int{}, map[string]int{}
	for _, n := range nodes {
		for _, g := range n.GPUs {
			owners[g.ID]++
		}
		if n.Agent != nil && n.Agent.Petals != nil {
			peers[n.Agent.Petals.PeerID]++
			addresses[n.Agent.Petals.Address]++
		}
	}
	versions := map[string]string{}
	candidates := []Placement{}
	ram := s.LoadRAMMiB + s.ReserveMiB + s.KVCacheMiB
	for _, n := range nodes {
		if len(s.NodeIDs) > 0 && !contains(s.NodeIDs, n.ID) {
			continue
		}
		reasons := []string{}
		if !n.SchedulingEnabled || n.Health != NodeReady || n.LastHeartbeat.IsZero() || c.now().Sub(n.LastHeartbeat) > 35*time.Second {
			reasons = append(reasons, "node disabled or heartbeat unavailable")
		}
		if n.DeploymentID != "" || c.nodeBusyLocked(n.ID, n) {
			reasons = append(reasons, "node already reserved")
		}
		if !validAgentURL(n.Agent) || !supportsBackend(n.Agent, BackendPetals) || n.Agent.EngineVersion != PetalsProfile || ValidatePetalsEndpoint(n.Agent.Petals) != nil {
			reasons = append(reasons, "compatible private Petals executor unavailable")
		} else {
			u, _ := url.Parse(n.Agent.URL)
			h, _, _ := net.SplitHostPort(n.Agent.Petals.Address)
			if u.Hostname() != h || !c.networkMember(s.NetworkGroup, n) {
				reasons = append(reasons, "private Agent and Petals address/group mismatch")
			}
			if peers[n.Agent.Petals.PeerID] > 1 || addresses[n.Agent.Petals.Address] > 1 {
				reasons = append(reasons, "duplicated peer identity or endpoint")
			}
		}
		if n.Host == nil || n.Host.MemoryAvailableMiB-n.ReservedRAMMiB-s.HostReserveMiB < ram {
			reasons = append(reasons, "insufficient host memory for sealed loading budget plus reserves")
		}
		var gpu GPU
		for _, g := range n.GPUs {
			if strings.EqualFold(g.Vendor, "NVIDIA") && g.Available() && g.FreeMemoryMiB > gpu.FreeMemoryMiB {
				gpu = g
			}
		}
		capacity := (gpu.FreeMemoryMiB - s.ReserveMiB - s.KVCacheMiB) / s.BlockMiB
		kvCapacity := s.KVCacheMiB * (1 << 20) / (s.KVBytesPerTokenPerLayer * int64(s.ContextSize))
		if capacity > kvCapacity {
			capacity = kvCapacity
		}
		if capacity > int64(s.Layers) {
			capacity = int64(s.Layers)
		}
		if gpu.ID == "" || owners[gpu.ID] > 1 || capacity < 1 {
			reasons = append(reasons, "missing/duplicated GPU or insufficient per-block GPU/KV budget")
		}
		if len(reasons) > 0 {
			p.Rejections[n.ID] = reasons
			continue
		}
		endpoint := *n.Agent.Petals
		endpoint.InitialPeers = append([]string(nil), endpoint.InitialPeers...)
		sort.Strings(endpoint.InitialPeers)
		versions[n.ID] = n.Agent.EngineVersion + "|" + strings.Join(endpoint.InitialPeers, "|")
		candidates = append(candidates, Placement{NodeID: n.ID, GPUID: gpu.ID, AgentURL: n.Agent.URL, Petals: &endpoint, UsableMiB: capacity * s.BlockMiB, RAMMiB: ram})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if s.CoordinatorID != "" {
			if candidates[i].NodeID == s.CoordinatorID {
				return true
			}
			if candidates[j].NodeID == s.CoordinatorID {
				return false
			}
		}
		if candidates[i].UsableMiB != candidates[j].UsableMiB {
			return candidates[i].UsableMiB > candidates[j].UsableMiB
		}
		return candidates[i].NodeID < candidates[j].NodeID
	})
	capacitySpec := s
	capacitySpec.WeightMiB = int64(s.Layers) * s.BlockMiB
	selected, err := c.planMeasuredRPC(capacitySpec, p, candidates, versions)
	if err != nil {
		return selected, err
	}
	if len(selected.Placements) > s.Layers {
		return p, fmt.Errorf("more nodes than model layers")
	}
	remaining, offset := s.Layers, 0
	for i := range selected.Placements {
		x := &selected.Placements[i]
		blocks := int(x.UsableMiB / s.BlockMiB)
		if limit := remaining - (len(selected.Placements) - i - 1); blocks > limit {
			blocks = limit
		}
		x.StartBlock, x.EndBlock = offset, offset+blocks
		x.WeightShareMiB = int64(blocks) * s.BlockMiB
		x.Fraction = float64(blocks) / float64(s.Layers)
		offset += blocks
		remaining -= blocks
	}
	return selected, nil
}
