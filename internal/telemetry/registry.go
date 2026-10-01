// Package telemetry measures traffic at service boundaries. Byte counters are
// application payload bytes, not NIC bytes (TCP/IP/TLS overhead is excluded).
package telemetry

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Point struct {
	At       time.Time `json:"at"`
	Requests uint64    `json:"requests"`
	Errors   uint64    `json:"errors"`
	RX       uint64    `json:"rx_bytes"`
	TX       uint64    `json:"tx_bytes"`
	RPS      float64   `json:"rps"`
	RXRate   float64   `json:"rx_bytes_per_sec"`
	TXRate   float64   `json:"tx_bytes_per_sec"`
}

type Service struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Kind         string             `json:"kind"`
	NodeID       string             `json:"node_id,omitempty"`
	DeploymentID string             `json:"deployment_id,omitempty"`
	Address      string             `json:"address,omitempty"`
	Status       string             `json:"status"`
	Message      string             `json:"message,omitempty"`
	Instance     string             `json:"instance"`
	StartedAt    time.Time          `json:"started_at"`
	LastSeen     time.Time          `json:"last_seen"`
	HTTP         bool               `json:"http_metrics"`
	Traffic      bool               `json:"traffic_metrics"`
	Requests     uint64             `json:"requests"`
	Errors       uint64             `json:"errors"`
	RX           uint64             `json:"rx_bytes"`
	TX           uint64             `json:"tx_bytes"`
	Active       int64              `json:"active"`
	Connections  uint64             `json:"connections"`
	LatencySum   float64            `json:"latency_seconds_sum"`
	Buckets      [7]uint64          `json:"latency_buckets"`
	Restarts     uint64             `json:"restarts"`
	Engine       map[string]float64 `json:"engine,omitempty"`
	RPS          float64            `json:"rps"`
	ErrorRate    float64            `json:"error_ratio"`
	RXRate       float64            `json:"rx_bytes_per_sec"`
	TXRate       float64            `json:"tx_bytes_per_sec"`
	AverageMS    float64            `json:"average_latency_ms"`
	History      []Point            `json:"history,omitempty"`
}

var bounds = []float64{.01, .05, .1, .5, 1, 5, 30}

func (r *Registry) Record(id string, status int, rx, tx int, elapsed time.Duration) {
	r.Update(id, func(s *Service) {
		s.Requests++
		if status >= 400 {
			s.Errors++
		}
		s.RX += uint64(rx)
		s.TX += uint64(tx)
		s.LatencySum += elapsed.Seconds()
		for i, b := range bounds {
			if elapsed.Seconds() <= b {
				s.Buckets[i]++
			}
		}
	})
}

type Registry struct {
	mu       sync.Mutex
	services map[string]Service
	remote   map[string]bool
	stale    time.Duration
}

func New() *Registry {
	return &Registry{services: map[string]Service{}, remote: map[string]bool{}, stale: 35 * time.Second}
}

// Expect creates a remote service placeholder without refreshing its heartbeat.
func (r *Registry) Expect(s Service) {
	r.Register(s)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remote[s.ID] = true
}

func (r *Registry) Register(s Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.services[s.ID]; ok {
		return
	}
	now := time.Now().UTC()
	s.StartedAt, s.LastSeen = now, now
	s.Instance = fmt.Sprintf("%s-%d", s.ID, now.UnixNano())
	if s.Status == "" {
		s.Status = "starting"
	}
	r.services[s.ID] = s
}

func (r *Registry) Update(id string, fn func(*Service)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.services[id]
	if !ok {
		return
	}
	fn(&s)
	s.LastSeen = time.Now().UTC()
	r.services[id] = s
}

func (r *Registry) State(id, status, message string) {
	r.Update(id, func(s *Service) { s.Status, s.Message = status, message })
}

// Forget removes a retired local worker after its final state was synchronized.
// The control plane and Prometheus retain that worker's historical observation.
func (r *Registry) Forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.services, id)
	delete(r.remote, id)
}

// Ingest accepts only caller-scoped identities; the HTTP layer verifies NodeID.
// Rates use receiver time and instance-aware deltas, never client timestamps.
func (r *Registry) Ingest(s Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, exists := r.services[s.ID]
	if exists && !r.remote[s.ID] {
		return
	}
	s.LastSeen = time.Now().UTC()
	s.History = old.History
	if exists && old.Instance != s.Instance {
		s.History = nil
	}
	r.services[s.ID] = sample(s, s.LastSeen)
	r.remote[s.ID] = true
}

func delta(a, b uint64) float64 {
	if a < b {
		return 0
	}
	return float64(a - b)
}

func sample(s Service, now time.Time) Service {
	p := Point{At: now, Requests: s.Requests, Errors: s.Errors, RX: s.RX, TX: s.TX}
	if n := len(s.History); n > 0 {
		prev := s.History[n-1]
		dt := now.Sub(prev.At).Seconds()
		if dt < 1 {
			return s
		}
		p.RPS = delta(p.Requests, prev.Requests) / dt
		p.RXRate = delta(p.RX, prev.RX) / dt
		p.TXRate = delta(p.TX, prev.TX) / dt
		s.RPS, s.RXRate, s.TXRate = p.RPS, p.RXRate, p.TXRate
		s.ErrorRate = 0
		if d := delta(p.Requests, prev.Requests); d > 0 {
			s.ErrorRate = delta(p.Errors, prev.Errors) / d
		}
	}
	s.History = append(s.History, p)
	if len(s.History) > 120 {
		s.History = append([]Point(nil), s.History[len(s.History)-120:]...)
	}
	if s.Requests > 0 {
		s.AverageMS = s.LatencySum * 1000 / float64(s.Requests)
	}
	return s
}

func (r *Registry) List(history bool) []Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	result := make([]Service, 0, len(r.services))
	for id, s := range r.services {
		if !r.remote[id] {
			s.LastSeen = now
			s = sample(s, now)
			r.services[id] = s
		}
		if r.remote[id] && now.Sub(s.LastSeen) > r.stale && s.Status != "stopped" && s.Status != "failed" {
			s.Status = "offline"
			s.Message = "telemetry heartbeat expired; last observation retained"
			s.RPS, s.RXRate, s.TXRate, s.ErrorRate = 0, 0, 0, 0
		}
		if history {
			s.History = append([]Point(nil), s.History...)
		} else {
			s.History = nil
		}
		if s.Engine != nil {
			m := map[string]float64{}
			for k, v := range s.Engine {
				m[k] = v
			}
			s.Engine = m
		}
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

type countedBody struct {
	io.ReadCloser
	add func(int)
}

func (b countedBody) Read(p []byte) (int, error) { n, e := b.ReadCloser.Read(p); b.add(n); return n, e }

type writer struct {
	http.ResponseWriter
	code int
	add  func(int)
}

func (w *writer) WriteHeader(code int) {
	if w.code != 0 {
		return
	}
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *writer) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(200)
	}
	n, e := w.ResponseWriter.Write(p)
	w.add(n)
	return n, e
}
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *writer) Flush() {
	if w.code == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (r *Registry) HTTP(id string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		start := time.Now()
		r.Update(id, func(s *Service) { s.Active++ })
		cw := &writer{ResponseWriter: w, add: func(n int) { r.Update(id, func(s *Service) { s.TX += uint64(n) }) }}
		if q.Body != nil {
			q.Body = countedBody{q.Body, func(n int) { r.Update(id, func(s *Service) { s.RX += uint64(n) }) }}
		}
		defer func() {
			p := recover()
			elapsed := time.Since(start).Seconds()
			r.Update(id, func(s *Service) {
				s.Active--
				s.Requests++
				s.LatencySum += elapsed
				if cw.code >= 400 || p != nil {
					s.Errors++
				}
				for i, b := range bounds {
					if elapsed <= b {
						s.Buckets[i]++
					}
				}
			})
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(cw, q)
	})
}

// ServeTCP is a transparent byte-counting proxy. A connection is not an LLM
// request: RPC has no HTTP request/latency semantics. Closing stop aborts active
// streams as well as accepts, so a stopped worker cannot keep serving a model.
func (r *Registry) ServeTCP(listener net.Listener, target, id string, stop <-chan struct{}) {
	go func() { <-stop; _ = listener.Close() }()
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer client.Close()
			up, err := net.DialTimeout("tcp", target, 3*time.Second)
			if err != nil {
				r.Update(id, func(s *Service) { s.Errors++ })
				return
			}
			defer up.Close()
			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-stop:
					client.Close()
					up.Close()
				case <-done:
				}
			}()
			r.Update(id, func(s *Service) { s.Connections++; s.Active++ })
			defer r.Update(id, func(s *Service) { s.Active-- })
			copyOne := func(dst, src net.Conn, rx bool) {
				buf := make([]byte, 64*1024)
				for {
					n, e := src.Read(buf)
					if n > 0 {
						written, we := dst.Write(buf[:n])
						r.Update(id, func(s *Service) {
							if rx {
								s.RX += uint64(written)
							} else {
								s.TX += uint64(written)
							}
						})
						if we != nil || written != n {
							break
						}
					}
					if e != nil {
						break
					}
				}
				if tcp, ok := dst.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
			}
			ch := make(chan struct{})
			go func() { copyOne(up, client, true); close(ch) }()
			copyOne(client, up, false)
			<-ch
		}()
	}
}

func (r *Registry) Prometheus(w http.ResponseWriter, q *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, s := range r.List(false) {
		labels := fmt.Sprintf("service=%s,kind=%s,node=%s,deployment=%s,instance=%s", strconv.Quote(s.ID), strconv.Quote(s.Kind), strconv.Quote(s.NodeID), strconv.Quote(s.DeploymentID), strconv.Quote(s.Instance))
		g := func(name string, v any) { fmt.Fprintf(w, "platform_service_%s{%s} %v\n", name, labels, v) }
		up := 0
		if s.Status == "ready" {
			up = 1
		}
		g("up", up)
		g("last_seen_seconds", s.LastSeen.Unix())
		g("active", s.Active)
		g("restarts_total", s.Restarts)
		if s.Traffic {
			g("receive_bytes_total", s.RX)
			g("transmit_bytes_total", s.TX)
			g("connections_total", s.Connections)
		}
		if s.HTTP {
			g("requests_total", s.Requests)
			g("errors_total", s.Errors)
			g("request_duration_seconds_sum", s.LatencySum)
			g("request_duration_seconds_count", s.Requests)
			for i, b := range bounds {
				fmt.Fprintf(w, "platform_service_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(b, 'g', -1, 64), s.Buckets[i])
			}
			fmt.Fprintf(w, "platform_service_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, s.Requests)
		}
		if s.Kind == "rpc-worker" {
			g("connection_errors_total", s.Errors)
		}
		for k, v := range s.Engine {
			fmt.Fprintf(w, "platform_engine_metric{%s,metric=%s} %g\n", labels, strconv.Quote(k), v)
		}
	}
}
