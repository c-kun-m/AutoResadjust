package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProbeTarget struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	ComposeService string `json:"compose_service"`
}
type ProbeConfig struct {
	CadvisorURL string        `json:"cadvisor_url"`
	Targets     []ProbeTarget `json:"targets"`
}

// Infrastructure traffic is measured by cAdvisor at the container interface,
// not inferred from the bytes used by a health probe. Missing data stays unknown.
func (r *Registry) StartProbes(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg ProbeConfig
	if err = json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	for _, t := range cfg.Targets {
		u, err := url.Parse(t.URL)
		if err != nil || u.Host == "" || !containsScheme(u.Scheme) {
			return fmt.Errorf("invalid probe URL for %s", t.ID)
		}
		r.Expect(Service{ID: "infra/" + t.ID, Name: t.Name, Kind: "infrastructure", Address: t.URL, Status: "starting"})
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	poll := func() {
		var traffic map[string][2]uint64
		if cfg.CadvisorURL != "" {
			req, _ := http.NewRequestWithContext(ctx, "GET", cfg.CadvisorURL, nil)
			if req != nil {
				resp, err := client.Do(req)
				if err == nil {
					if resp.StatusCode == 200 {
						traffic = ParseContainerTraffic(io.LimitReader(resp.Body, 8<<20), os.Getenv("MONITOR_COMPOSE_PROJECT"))
					}
					resp.Body.Close()
				}
			}
		}
		var probes sync.WaitGroup
		for _, target := range cfg.Targets {
			if ctx.Err() != nil {
				break
			}
			probes.Add(1)
			go func(t ProbeTarget) {
				defer probes.Done()
				start := time.Now()
				var err error
				u, _ := url.Parse(t.URL)
				if u.Scheme == "tcp" {
					var c net.Conn
					c, err = (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", u.Host)
					if c != nil {
						c.Close()
					}
				} else {
					req, _ := http.NewRequestWithContext(ctx, "GET", t.URL, nil)
					var resp *http.Response
					resp, err = client.Do(req)
					if resp != nil {
						io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
						resp.Body.Close()
						if resp.StatusCode < 200 || resp.StatusCode >= 300 {
							err = fmt.Errorf("HTTP %d", resp.StatusCode)
						}
					}
				}
				id := "infra/" + t.ID
				var s Service
				for _, item := range r.List(false) {
					if item.ID == id {
						s = item
					}
				}
				s.Status = "ready"
				s.Message = "health probe passed; request counters require a service-specific exporter"
				if err != nil {
					s.Status = "offline"
					s.Message = "not running or unreachable: " + err.Error()
				}
				s.Engine = map[string]float64{"probe_latency_ms": float64(time.Since(start).Microseconds()) / 1000}
				s.Traffic = false
				if values, ok := traffic[t.ComposeService]; ok {
					s.Traffic = true
					s.RX, s.TX = values[0], values[1]
					s.Message += "; traffic source: cAdvisor container network"
				}
				r.Ingest(s)
			}(target)
		}
		probes.Wait()
	}
	go func() {
		poll()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
	return nil
}
func containsScheme(s string) bool { return s == "http" || s == "https" || s == "tcp" }

func ParseContainerTraffic(reader io.Reader, project ...string) map[string][2]uint64 {
	result := map[string][2]uint64{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if len(project) > 0 && project[0] != "" && !strings.Contains(line, `container_label_com_docker_compose_project=`+strconv.Quote(project[0])+`,`) && !strings.Contains(line, `container_label_com_docker_compose_project=`+strconv.Quote(project[0])+`}`) {
			continue
		}
		direction := -1
		if strings.HasPrefix(line, "container_network_receive_bytes_total{") {
			direction = 0
		} else if strings.HasPrefix(line, "container_network_transmit_bytes_total{") {
			direction = 1
		}
		if direction < 0 || strings.Contains(line, `interface="lo"`) {
			continue
		}
		const label = `container_label_com_docker_compose_service="`
		a := strings.Index(line, label)
		if a < 0 {
			continue
		}
		tail := line[a+len(label):]
		b := strings.IndexByte(tail, '"')
		if b < 0 {
			continue
		}
		name := tail[:b]
		end := strings.LastIndex(line, "}")
		if end < 0 {
			continue
		}
		fields := strings.Fields(line[end+1:])
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || v < 0 {
			continue
		}
		current := result[name]
		current[direction] += uint64(v)
		result[name] = current
	}
	return result
}
