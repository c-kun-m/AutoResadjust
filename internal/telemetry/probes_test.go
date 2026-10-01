package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDedicatedContainerTrafficExporter(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(map[bool]string{true: "measured", false: "unknown"}[available], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := map[string]any{"ready": true, "instance": "boot-1"}
				if available {
					data["traffic_source"] = "container-network"
					data["receive_bytes_total"] = 123
					data["transmit_bytes_total"] = 456
				}
				json.NewEncoder(w).Encode(data)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "targets.json")
			data, _ := json.Marshal(ProbeConfig{Targets: []ProbeTarget{{ID: "bootstrap", Name: "Private bootstrap", URL: server.URL, TrafficURL: server.URL}}})
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			r := New()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := r.StartProbes(ctx, path); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				s := r.List(false)[0]
				if s.Status == "ready" {
					if s.Traffic != available || available && (s.RX != 123 || s.TX != 456 || s.Instance != "infra/bootstrap/boot-1") {
						t.Fatal(s)
					}
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Fatal("exporter not observed")
		})
	}
}
