package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

// One outgoing probe and one bounded incoming upload. No detached work survives
// agent shutdown; results remain until acknowledged by a successful sync.
type NetworkProber struct {
	nodeID, token string
	metrics       *telemetry.Registry
	mu            sync.Mutex
	running       bool
	seen          map[string]time.Time
	results       map[string]platform.NetworkResult
	uploadGate    chan struct{}
}

func NewNetworkProber(nodeID, token string, m *telemetry.Registry) *NetworkProber {
	p := &NetworkProber{nodeID: nodeID, token: token, metrics: m, seen: map[string]time.Time{}, results: map[string]platform.NetworkResult{}, uploadGate: make(chan struct{}, 1)}
	m.Register(telemetry.Service{ID: nodeID + "/network-probe", Name: "Network measurements", Kind: "network-probe", NodeID: nodeID, HTTP: true, Traffic: true, Status: "ready"})
	return p
}

// Handler is mounted behind the same authentication boundary as agent APIs.
// Downloading arbitrary URLs, unlimited payloads and redirects are unsupported.
func (p *NetworkProber) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.metrics.HTTP(p.nodeID+"/network-probe", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Node-ID", p.nodeID)
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case r.URL.Path == "/network-probe/ping" && r.Method == "GET":
			w.WriteHeader(204)
		case r.URL.Path == "/network-probe/upload" && r.Method == "POST":
			if r.ContentLength != platform.NetworkProbeBytes {
				http.Error(w, "payload must be exactly 1 MiB", 400)
				return
			}
			select {
			case p.uploadGate <- struct{}{}:
				defer func() { <-p.uploadGate }()
			default:
				http.Error(w, "probe receiver busy", 429)
				return
			}
			ctrl := http.NewResponseController(w)
			_ = ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
			defer ctrl.SetReadDeadline(time.Time{})
			r.Body = http.MaxBytesReader(w, r.Body, platform.NetworkProbeBytes)
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil || n != platform.NetworkProbeBytes {
				http.Error(w, "incomplete probe upload", 400)
				return
			}
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	})).ServeHTTP(w, r)
}

func privateProbeDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("probe host has no address")
	}
	for _, ip := range ips {
		v4 := ip.IP.To4()
		// RFC 6598 addresses are used by private overlay VPNs.
		sharedVPN := v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
		if !ip.IP.IsPrivate() && !ip.IP.IsLoopback() && !sharedVPN {
			return nil, fmt.Errorf("network probes require private or loopback addresses")
		}
	}
	var last error
	for _, ip := range ips {
		conn, e := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return conn, nil
		}
		last = e
	}
	return nil, last
}

func (p *NetworkProber) Apply(ctx context.Context, jobs []platform.NetworkProbe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, t := range p.seen {
		if time.Since(t) > 10*time.Minute {
			delete(p.seen, id)
			delete(p.results, id)
		}
	}
	if p.running || len(p.seen) >= 256 {
		return
	}
	for _, job := range jobs {
		if _, ok := p.seen[job.ID]; ok {
			continue
		}
		if job.SourceID != p.nodeID || job.TargetID == p.nodeID || !time.Now().Before(job.ExpiresAt) {
			continue
		}
		p.running = true
		p.seen[job.ID] = time.Now()
		go func(job platform.NetworkProbe) {
			result := p.measure(ctx, job)
			p.mu.Lock()
			p.results[job.ID] = result
			p.running = false
			p.mu.Unlock()
			if result.Error != "" {
				p.metrics.State(p.nodeID+"/network-probe", "degraded", result.Error)
			} else {
				p.metrics.State(p.nodeID+"/network-probe", "ready", "")
			}
		}(job)
		break
	}
}

func (p *NetworkProber) Reports() []platform.NetworkResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []platform.NetworkResult{}
	for _, r := range p.results {
		out = append(out, r)
	}
	return out
}
func (p *NetworkProber) Acknowledge(reports []platform.NetworkResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range reports {
		delete(p.results, r.ProbeID)
	}
}

func (p *NetworkProber) measure(parent context.Context, job platform.NetworkProbe) (result platform.NetworkResult) {
	result.ProbeID = job.ID
	u, err := url.Parse(job.TargetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		result.Error = "invalid registered agent URL"
		return
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	tr := &http.Transport{DialContext: privateProbeDial, DisableCompression: true, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, body []byte) (time.Duration, error) {
		req, e := http.NewRequestWithContext(ctx, method, job.TargetURL+path, bytes.NewReader(body))
		if e != nil {
			return 0, e
		}
		if p.token != "" {
			req.Header.Set("Authorization", "Bearer "+p.token)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		start := time.Now()
		resp, e := client.Do(req)
		elapsed := time.Since(start)
		status := 503
		received := 0
		if resp != nil {
			status = resp.StatusCode
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1025))
			resp.Body.Close()
			received = len(data)
			if e == nil {
				e = readErr
			}
			if e == nil && (status != 204 || received != 0 || resp.Header.Get("X-Node-ID") != job.TargetID) {
				e = fmt.Errorf("unexpected probe response or node identity (HTTP %d)", status)
			}
		}
		if e != nil && status < 400 {
			status = 502
		}
		p.metrics.Record(p.nodeID+"/network-probe", status, received, len(body), elapsed)
		return elapsed, e
	}
	rtts := make([]float64, 0, 5)
	for i := 0; i < 5; i++ {
		d, e := request("GET", "/network-probe/ping", nil)
		if e != nil {
			result.Error = "RTT probe failed: " + e.Error()
			return
		}
		rtts = append(rtts, float64(d)/float64(time.Millisecond))
	}
	for i := 1; i < len(rtts); i++ {
		result.JitterMS += math.Abs(rtts[i]-rtts[i-1]) / 4
	}
	sort.Float64s(rtts)
	result.RTTP95MS = rtts[4]
	result.Samples = 5
	// Random payload avoids compression producing an optimistic link estimate.
	data := make([]byte, platform.NetworkProbeBytes)
	if _, err = rand.Read(data); err != nil {
		result.Error = "cannot prepare probe payload"
		return
	}
	elapsed, e := request("POST", "/network-probe/upload", data)
	if e != nil {
		result.Error = "upload probe failed: " + e.Error()
		return
	}
	result.PayloadBytes = platform.NetworkProbeBytes
	result.UploadMbps = float64(platform.NetworkProbeBytes) * 8 / elapsed.Seconds() / 1e6
	return
}
