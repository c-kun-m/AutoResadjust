package telemetry

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPCountsStreamingAndFailures(t *testing.T) {
	r := New()
	r.Register(Service{ID: "api", HTTP: true, Traffic: true, Status: "ready"})
	h := r.HTTP("api", http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		io.Copy(io.Discard, q.Body)
		w.WriteHeader(503)
		fmt.Fprint(w, "abc")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "def")
	}))
	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest("POST", "/", strings.NewReader("hello")))
	s := r.List(false)[0]
	if s.Requests != 1 || s.Errors != 1 || s.RX != 5 || s.TX != 6 || s.Active != 0 {
		t.Fatalf("incorrect counters: %+v", s)
	}
	p := httptest.NewRecorder()
	r.Prometheus(p, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(p.Body.String(), "request_duration_seconds_bucket") {
		t.Fatal("histogram missing")
	}
}
func TestReceiverRatesResetAndOffline(t *testing.T) {
	r := New()
	s := Service{ID: "n/rpc", Instance: "one", Status: "ready", Traffic: true, RX: 100}
	r.Ingest(s)
	r.mu.Lock()
	v := r.services[s.ID]
	v.History[0].At = time.Now().Add(-10 * time.Second)
	r.services[s.ID] = v
	r.mu.Unlock()
	s.RX = 1100
	r.Ingest(s)
	v = r.List(false)[0]
	if v.RXRate < 90 || v.RXRate > 110 {
		t.Fatalf("rate %f", v.RXRate)
	}
	s.Instance = "two"
	s.RX = 3
	r.Ingest(s)
	if v = r.List(false)[0]; v.RXRate != 0 {
		t.Fatal("counter reset produced false rate")
	}
	r.mu.Lock()
	v = r.services[s.ID]
	v.LastSeen = time.Now().Add(-time.Minute)
	r.services[s.ID] = v
	r.mu.Unlock()
	if v = r.List(false)[0]; v.Status != "offline" || v.RXRate != 0 {
		t.Fatal("stale service marked healthy")
	}
}

func TestRetiredServicesKeepTerminalState(t *testing.T) {
	r := New()
	for _, status := range []string{"stopped", "failed"} {
		r.Ingest(Service{ID: status, Status: status})
		r.mu.Lock()
		s := r.services[status]
		s.LastSeen = time.Now().Add(-time.Hour)
		r.services[status] = s
		r.mu.Unlock()
	}
	for _, s := range r.List(false) {
		if s.Status != s.ID {
			t.Fatalf("terminal state became %s", s.Status)
		}
	}
	r.Forget("stopped")
	if len(r.List(false)) != 1 {
		t.Fatal("retired worker not pruned")
	}
}
func TestRPCProxyByteAccountingAndStop(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, e := backend.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := New()
	r.Register(Service{ID: "rpc", Traffic: true})
	stop := make(chan struct{})
	defer close(stop)
	go r.ServeTCP(l, backend.Addr().String(), "rpc", stop)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("payload"))
	b := make([]byte, 7)
	if _, err = io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "payload" {
		t.Fatal(string(b))
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s := r.List(false)[0]
		if s.RX == 7 && s.TX == 7 && s.Connections == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("RPC bytes not recorded")
}
func TestContainerTrafficParser(t *testing.T) {
	data := `container_network_receive_bytes_total{container_label_com_docker_compose_service="nats",interface="eth0"} 100
container_network_receive_bytes_total{container_label_com_docker_compose_service="nats",interface="lo"} 999
container_network_transmit_bytes_total{container_label_com_docker_compose_service="nats",interface="eth0"} 200
`
	values := ParseContainerTraffic(strings.NewReader(data))
	if values["nats"] != [2]uint64{100, 200} {
		t.Fatal(values)
	}
}

func TestContainerTrafficIsScopedToComposeProject(t *testing.T) {
	data := `container_network_receive_bytes_total{container_label_com_docker_compose_project="ours",container_label_com_docker_compose_service="postgres",interface="eth0"} 100
container_network_receive_bytes_total{container_label_com_docker_compose_project="other",container_label_com_docker_compose_service="postgres",interface="eth0"} 999
`
	values := ParseContainerTraffic(strings.NewReader(data), "ours")
	if values["postgres"][0] != 100 {
		t.Fatal(values)
	}
}
