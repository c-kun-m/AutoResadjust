package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
)

func TestValidateControlPlaneURL(t *testing.T) {
	for _, raw := range []string{"", "localhost:8080", "ftp://localhost:8080", "http://"} {
		if err := validateControlPlaneURL(raw); err == nil {
			t.Errorf("validateControlPlaneURL(%q) unexpectedly succeeded", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "https://control.example.test/base"} {
		if err := validateControlPlaneURL(raw); err != nil {
			t.Errorf("validateControlPlaneURL(%q): %v", raw, err)
		}
	}
}

func TestHeartbeatClientUsesBearerTokenAndTimeoutClient(t *testing.T) {
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/resources/heartbeat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("unexpected authorization header: %q", got)
		}
		var node platform.ResourceNode
		if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
			t.Errorf("decode heartbeat: %v", err)
		}
		if node.ID != "node-a" {
			t.Errorf("unexpected node: %#v", node)
		}
		called <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	c := heartbeatClient{baseURL: server.URL + "/", token: "test-token", client: &http.Client{Timeout: time.Second}}
	err := c.send(context.Background(), platform.ResourceNode{ID: "node-a", Health: platform.NodeDegraded})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("server did not receive heartbeat")
	}
}

func TestHeartbeatClientRejectsHTTPErrorWithBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token required", http.StatusUnauthorized)
	}))
	defer server.Close()
	c := heartbeatClient{baseURL: server.URL, client: &http.Client{Timeout: time.Second}}
	err := c.send(context.Background(), platform.ResourceNode{ID: "node-a"})
	if err == nil || !strings.Contains(err.Error(), "token required") {
		t.Fatalf("expected body in HTTP error, got %v", err)
	}
}
