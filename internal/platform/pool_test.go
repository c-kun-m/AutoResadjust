package platform

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPoolImmutableIdentityMembershipAndPersistence(t *testing.T) {
	c := NewController(nil)
	a := ModelArtifact{ID: "a", Name: "A", Format: "gguf", File: "a.gguf", Revision: "fixed", SHA256: strings.Repeat("a", 64), Architecture: "llama", Quantization: "q8", WeightMiB: 100, Layers: 10, ContextLimit: 1024}
	if _, err := c.RegisterArtifact(a); err != nil {
		t.Fatal(err)
	}
	other := a
	other.ID = "b"
	other.ChatTemplate = "different"
	c.RegisterArtifact(other)
	c.deployments["one"] = Deployment{Spec: DeploymentSpec{ModelRef: "a", Backend: BackendLocal}}
	c.deployments["two"] = Deployment{Spec: DeploymentSpec{ModelRef: "a", Backend: BackendLocal}}
	c.deployments["different-template"] = Deployment{Spec: DeploymentSpec{ModelRef: "b", Backend: BackendLocal}}
	c.deployments["different-backend"] = Deployment{Spec: DeploymentSpec{ModelRef: "a", Backend: BackendRPC}}
	p := ModelPool{ID: "chat", ModelRef: "a", Backend: BackendLocal, DeploymentIDs: []string{"one", "two"}}
	if _, err := c.PutModelPool(p); err != nil {
		t.Fatal(err)
	}
	p.DeploymentIDs[0] = "changed"
	saved := c.ListModelPools()[0]
	saved.DeploymentIDs[0] = "changed"
	if c.ListModelPools()[0].DeploymentIDs[0] != "one" {
		t.Fatal("pool references escaped lock")
	}
	for _, id := range []string{"different-template", "different-backend", "missing", "two"} {
		p.DeploymentIDs = []string{id, "two"}
		if _, err := c.PutModelPool(p); err == nil {
			t.Fatal("accepted incompatible or repeated member", id)
		}
	}
	p.ModelRef = "b"
	p.DeploymentIDs = []string{"different-template"}
	if _, err := c.PutModelPool(p); err == nil {
		t.Fatal("existing alias changed identity")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadController(path)
	if err != nil || len(restored.ListModelPools()) != 1 {
		t.Fatal(err)
	}
}
