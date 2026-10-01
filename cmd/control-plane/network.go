package main

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/resource-adjust/compute-platform/internal/platform"
)

func (s *apiServer) networkRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/api/v1/network-groups" && r.Method == "GET":
		writeJSON(w, 200, s.controller.ListNetworkGroups())
	case path == "/api/v1/network-groups" && (r.Method == "POST" || r.Method == "PUT"):
		var g platform.NetworkGroup
		if err := decodeJSON(r, &g); err != nil {
			writeError(w, 400, err)
			return true
		}
		g, err := s.controller.PutNetworkGroup(g)
		if err != nil {
			writeError(w, 409, err)
		} else {
			writeJSON(w, 200, g)
		}
	case path == "/api/v1/network-links" && r.Method == "GET":
		writeJSON(w, 200, s.controller.ListNetworkLinks(r.URL.Query().Get("group_id")))
	case path == "/api/v1/network-probes" && r.Method == "GET":
		writeJSON(w, 200, s.controller.ListNetworkProbes())
	case path == "/api/v1/network-probes" && r.Method == "POST":
		var req struct {
			SourceID string `json:"source_id"`
			TargetID string `json:"target_id"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, 400, err)
			return true
		}
		p, err := s.controller.QueueNetworkProbe(req.SourceID, req.TargetID)
		if err != nil {
			writeError(w, 409, err)
		} else {
			writeJSON(w, 202, p)
		}
	default:
		return false
	}
	return true
}

func (s *apiServer) networkMetrics(w http.ResponseWriter) {
	for _, l := range s.controller.ListNetworkLinks() {
		labels := "source=" + strconv.Quote(l.SourceID) + ",target=" + strconv.Quote(l.TargetID)
		valid := 0
		if l.Status == "measured" {
			valid = 1
		}
		fmt.Fprintf(w, "platform_network_link_valid{%s} %d\n", labels, valid)
		if !l.ObservedAt.IsZero() {
			fmt.Fprintf(w, "platform_network_link_observed_timestamp_seconds{%s} %d\n", labels, l.ObservedAt.Unix())
		}
		if valid == 1 {
			fmt.Fprintf(w, "platform_network_rtt_seconds{%s} %g\nplatform_network_jitter_seconds{%s} %g\nplatform_network_upload_bits_per_second{%s} %g\n", labels, l.RTTP95MS/1000, labels, l.JitterMS/1000, labels, l.UploadMbps*1e6)
		}
	}
}
