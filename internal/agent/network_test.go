package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

func TestNetworkRealHTTPPayloadAndDuplicateDelivery(t *testing.T) {
	serverProbe := NewNetworkProber("b", "", telemetry.New())
	s := httptest.NewServer(serverProbe)
	defer s.Close()
	p := NewNetworkProber("a", "", telemetry.New())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := platform.NetworkProbe{ID: "probe-1", SourceID: "a", TargetID: "b", TargetURL: s.URL, ExpiresAt: time.Now().Add(time.Minute)}
	p.Apply(ctx, []platform.NetworkProbe{job})
	deadline := time.Now().Add(3 * time.Second)
	for len(p.Reports()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r := p.Reports()
	if len(r) != 1 || r[0].Error != "" || r[0].PayloadBytes != platform.NetworkProbeBytes || r[0].Samples != 5 || r[0].UploadMbps <= 0 {
		t.Fatal(r)
	}
	p.Acknowledge(r)
	p.Apply(ctx, []platform.NetworkProbe{job})
	if len(p.Reports()) != 0 {
		t.Fatal("acknowledged job replayed")
	}
	services := serverProbe.metrics.List(false)
	if len(services) != 1 || services[0].RX < platform.NetworkProbeBytes {
		t.Fatalf("payload missing from traffic: %+v", services)
	}
}

func TestNetworkRejectsRedirectsIdentityAndPublicTargets(t *testing.T) {
	p := NewNetworkProber("a", "", telemetry.New())
	for _, mode := range []string{"redirect", "identity"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, "http://127.0.0.1:1/", 302)
				} else {
					w.Header().Set("X-Node-ID", "wrong")
					w.WriteHeader(204)
				}
			}))
			defer s.Close()
			r := p.measure(context.Background(), platform.NetworkProbe{ID: mode, TargetID: "b", TargetURL: s.URL})
			if r.Error == "" {
				t.Fatal("unsafe probe succeeded")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := privateProbeDial(ctx, "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("metadata endpoint accepted")
	}
	if _, err := privateProbeDial(ctx, "tcp", "8.8.8.8:80"); err == nil {
		t.Fatal("public target accepted")
	}
	r := p.measure(ctx, platform.NetworkProbe{TargetURL: "file:///etc/passwd"})
	if r.Error == "" {
		t.Fatal("non HTTP URL accepted")
	}
}

func TestNetworkBoundedPayloadAndCancellation(t *testing.T) {
	p := NewNetworkProber("b", "", telemetry.New())
	for _, size := range []int{1, platform.NetworkProbeBytes + 1} {
		req := httptest.NewRequest("POST", "/network-probe/upload", bytes.NewReader(make([]byte, size)))
		out := httptest.NewRecorder()
		p.ServeHTTP(out, req)
		if out.Code != 400 {
			t.Fatal(out.Code)
		}
	}
	req := httptest.NewRequest("POST", "/network-probe/upload", strings.NewReader("short"))
	req.ContentLength = platform.NetworkProbeBytes
	out := httptest.NewRecorder()
	p.ServeHTTP(out, req)
	if out.Code != 400 {
		t.Fatal("partial body accepted")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := p.measure(ctx, platform.NetworkProbe{TargetURL: s.URL})
	if r.Error == "" {
		t.Fatal("cancellation ignored")
	}
}
