package platform

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func networkController(t *testing.T) *Controller {
	c := groupController(t)
	for _, n := range c.Nodes.ListNodes() {
		n.Agent.NetworkProbe = true
		c.Nodes.UpsertNode(n)
	}
	_, err := c.PutNetworkGroup(NetworkGroup{ID: "measured-lan", Name: "Measured LAN", NodeIDs: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func recordLink(t *testing.T, c *Controller, a, b string, speed float64) {
	t.Helper()
	p, err := c.QueueNetworkProbe(a, b)
	if err != nil {
		t.Fatal(err)
	}
	c.ReportNetwork(a, []NetworkResult{{ProbeID: p.ID, RTTP95MS: 2, JitterMS: 0.5, UploadMbps: speed, Samples: 5, PayloadBytes: NetworkProbeBytes}})
}
func TestNetworkPolicyRequiresFreshBidirectionalMeasurements(t *testing.T) {
	c := networkController(t)
	spec := deploymentSpec()
	spec.NetworkGroup = "measured-lan"
	spec.NodeIDs = []string{"a", "b"}
	if _, err := c.PlanDeployment(spec); err == nil {
		t.Fatal("unmeasured links admitted")
	}
	recordLink(t, c, "a", "b", 1000)
	if _, err := c.PlanDeployment(spec); err == nil {
		t.Fatal("one direction treated as bidirectional")
	}
	recordLink(t, c, "b", "a", 1000)
	p, err := c.PlanDeployment(spec)
	if err != nil || len(p.Placements) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	l := c.networkLinks[linkKey("b", "a")]
	l.UploadMbps = 1
	c.networkLinks[linkKey("b", "a")] = l
	if _, err = c.PlanDeployment(spec); err == nil {
		t.Fatal("slow reverse link admitted")
	}
	l.UploadMbps = 1000
	l.ObservedAt = time.Now().Add(-6 * time.Minute)
	c.networkLinks[linkKey("b", "a")] = l
	p, err = c.PlanDeployment(spec)
	if err == nil || !strings.Contains(fmtRejections(p), "stale") {
		t.Fatalf("stale link admitted: %+v %v", p, err)
	}
}
func fmtRejections(p DeploymentPlan) string {
	out := ""
	for _, r := range p.Rejections {
		out += strings.Join(r, " ")
	}
	return out
}

func TestNetworkOwnershipExpiryEndpointChangeAndPersistence(t *testing.T) {
	c := networkController(t)
	p, err := c.QueueNetworkProbe("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.QueueNetworkProbe("c", "b"); err == nil {
		t.Fatal("concurrent receiver probe admitted")
	}
	r := NetworkResult{ProbeID: p.ID, RTTP95MS: 2, JitterMS: 0.5, UploadMbps: 1000, Samples: 5, PayloadBytes: NetworkProbeBytes}
	c.ReportNetwork("b", []NetworkResult{r})
	if len(c.ListNetworkLinks()) != 0 {
		t.Fatal("wrong agent result accepted")
	}
	jobs := c.NetworkAssignments("a")
	if len(jobs) != 1 || jobs[0].Status != "running" {
		t.Fatal(jobs)
	}
	c.ReportNetwork("a", []NetworkResult{r})
	if c.ListNetworkLinks()[0].Status != "measured" {
		t.Fatal("valid result lost")
	}
	if _, err = c.QueueNetworkProbe("a", "b"); err == nil {
		t.Fatal("measurement cooldown bypassed")
	}
	n, _ := c.Nodes.GetNode("b")
	n.Agent.URL = "http://changed:9090"
	c.Nodes.UpsertNode(n)
	if c.ListNetworkLinks()[0].Status != "stale" {
		t.Fatal("old address result reused")
	}
	p, err = c.QueueNetworkProbe("b", "a")
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return p.ExpiresAt.Add(time.Second) }
	for _, job := range c.ListNetworkProbes() {
		if job.ID == p.ID && job.Status != "failed" {
			t.Fatal("expired job remains active")
		}
	}
	c.ReportNetwork("b", []NetworkResult{{ProbeID: p.ID, UploadMbps: 1000}})
	if c.networkLinks[linkKey("b", "a")].Error == "" {
		t.Fatal("late report overwrote failure")
	}
	file := filepath.Join(t.TempDir(), "state.json")
	if err = c.Save(file); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadController(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.ListNetworkGroups()) != 1 {
		t.Fatal("group lost across restart")
	}
	for _, l := range restored.ListNetworkLinks() {
		if l.Status != "stale" {
			t.Fatal("restart retained measurement freshness")
		}
	}
}

func TestNetworkGroupImmutableWhileDeploymentActive(t *testing.T) {
	c := networkController(t)
	recordLink(t, c, "a", "b", 1000)
	recordLink(t, c, "b", "a", 1000)
	spec := deploymentSpec()
	spec.NetworkGroup = "measured-lan"
	spec.NodeIDs = []string{"a", "b"}
	d, _, err := c.CreateDeployment(spec)
	if err != nil || d.Phase != "starting" {
		t.Fatal(d, err)
	}
	g := c.ListNetworkGroups()[0]
	g.NodeIDs = []string{"c"}
	if _, err = c.PutNetworkGroup(g); err == nil {
		t.Fatal("running group policy changed")
	}
	if len(c.ListNetworkGroups()[0].NodeIDs) != 3 {
		t.Fatal("failed update mutated group")
	}
}

func TestNetworkGroupFreshnessMatchesReportedStatus(t *testing.T) {
	c := networkController(t)
	recordLink(t, c, "a", "b", 1000)
	l := c.networkLinks[linkKey("a", "b")]
	l.ObservedAt = time.Now().Add(-6 * time.Minute)
	c.networkLinks[linkKey("a", "b")] = l
	g := c.ListNetworkGroups()[0]
	g.MaxAgeSeconds = 600
	if _, err := c.PutNetworkGroup(g); err != nil {
		t.Fatal(err)
	}
	if c.ListNetworkLinks()[0].Status != "stale" || c.ListNetworkLinks(g.ID)[0].Status != "measured" {
		t.Fatal("group freshness and API status disagree")
	}
}

func TestMeasuredPlacementFindsCliqueDespiteGreedyTrap(t *testing.T) {
	c := networkController(t)
	// The large first node only connects to one other node; b,c,d form the
	// three-node clique needed to hold this model.
	n := deploymentNode("d", 8192)
	n.Agent.NetworkProbe = true
	c.Nodes.UpsertNode(n)
	g := c.ListNetworkGroups()[0]
	g.NodeIDs = append(g.NodeIDs, "d")
	c.PutNetworkGroup(g)
	for _, pair := range [][2]string{{"a", "b"}, {"b", "c"}, {"b", "d"}, {"c", "d"}} {
		recordLink(t, c, pair[0], pair[1], 1000)
		recordLink(t, c, pair[1], pair[0], 1000)
	}
	spec := deploymentSpec()
	spec.NetworkGroup = g.ID
	spec.WeightMiB = 18000
	spec.MinNodes = 3
	p, err := c.PlanDeployment(spec)
	if err != nil || len(p.Placements) != 3 {
		t.Fatal(p, err)
	}
	for _, placement := range p.Placements {
		if placement.NodeID == "a" {
			t.Fatal("incompatible node in plan")
		}
	}
}
