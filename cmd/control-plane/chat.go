package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/resource-adjust/compute-platform/internal/inference"
	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

var inferenceLog = slog.New(slog.NewJSONHandler(os.Stdout, nil))

func (s *apiServer) chatRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/api/v1/model-pools" && r.Method == "GET":
		writeJSON(w, 200, s.controller.ListModelPools())
		return true
	case path == "/api/v1/model-pools" && (r.Method == "POST" || r.Method == "PUT"):
		var p platform.ModelPool
		if err := decodeJSON(r, &p); err != nil {
			writeError(w, 400, err)
			return true
		}
		p, err := s.controller.PutModelPool(p)
		if err != nil {
			writeError(w, 400, err)
		} else {
			writeJSON(w, 200, p)
		}
		return true
	case path == "/api/v1/monitor/inference" && r.Method == "GET":
		p := s.inference.Policy
		writeJSON(w, 200, map[string]any{"deployments": s.inference.Snapshot(), "policy": map[string]any{"concurrency": p.Concurrency, "queue_size": p.QueueSize, "queue_wait_seconds": p.QueueWait.Seconds(), "first_token_seconds": p.FirstToken.Seconds(), "idle_seconds": p.Idle.Seconds(), "total_seconds": p.Total.Seconds()}, "ttft_scope": "queue admission until first content, reasoning, refusal or tool delta; comments and role-only chunks excluded"})
		return true
	case path == "/v1/models" && r.Method == "GET":
		data := []any{}
		for _, p := range s.controller.ListModelPools() {
			data = append(data, map[string]any{"id": p.ID, "object": "model", "created": 0, "owned_by": "compute-platform"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
		return true
	case path == "/v1/chat/completions" && r.Method == "POST":
		s.chatInference(w, r, "")
		return true
	}
	return false
}

func (s *apiServer) chatInference(w http.ResponseWriter, r *http.Request, direct string) {
	started := time.Now()
	requestID := make([]byte, 16)
	if _, err := rand.Read(requestID); err != nil {
		inference.WriteError(w, 500, "internal_error", "unable to create request id")
		return
	}
	trace := hex.EncodeToString(requestID)
	w.Header().Set("X-Request-ID", trace)
	ctx, cancel := context.WithTimeout(r.Context(), s.inference.Policy.Total)
	defer cancel()
	// A read deadline bounds slow request uploads as well as upstream execution.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(started.Add(s.inference.Policy.QueueWait))
	defer rc.SetReadDeadline(time.Time{})
	var body map[string]json.RawMessage
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil || body == nil {
		inference.WriteError(w, 400, "invalid_request", "JSON object required (maximum 2 MiB)")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		inference.WriteError(w, 400, "invalid_request", "a single JSON object is required")
		return
	}
	_ = rc.SetReadDeadline(time.Time{})
	var stream bool
	if value, ok := body["stream"]; ok && json.Unmarshal(value, &stream) != nil {
		inference.WriteError(w, 400, "invalid_request", "stream must be a boolean")
		return
	}
	if value, ok := body["n"]; ok {
		var n int
		if json.Unmarshal(value, &n) != nil || n != 1 {
			inference.WriteError(w, 400, "invalid_request", "n must be 1")
			return
		}
	}
	var messages []json.RawMessage
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		inference.WriteError(w, 400, "invalid_request", "messages must be a nonempty array")
		return
	}
	model := direct
	ids := []string{}
	if direct != "" {
		if _, ok := s.controller.GetDeployment(direct); !ok {
			inference.WriteError(w, 404, "model_not_found", "deployment does not exist")
			return
		}
		ids = append(ids, direct)
	} else {
		if json.Unmarshal(body["model"], &model) != nil || model == "" {
			inference.WriteError(w, 400, "invalid_request", "model must name a registered model pool")
			return
		}
		for _, p := range s.controller.ListModelPools() {
			if p.ID == model {
				ids = p.DeploymentIDs
				break
			}
		}
		if len(ids) == 0 {
			inference.WriteError(w, 404, "model_not_found", "model pool does not exist")
			return
		}
	}
	ready := []string{}
	for _, id := range ids {
		if _, err := s.controller.InferenceTarget(id); err == nil {
			ready = append(ready, id)
		}
	}
	if len(ready) == 0 {
		inference.WriteError(w, 503, "model_unavailable", "no ready replica for this model")
		return
	}
	ticket, err := s.inference.Acquire(ctx, ready)
	if err != nil {
		code := "queue_full"
		status := 429
		if errors.Is(err, inference.ErrQueueTimeout) {
			code = "queue_timeout"
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = "total_timeout"
			status = 504
		}
		w.Header().Set("Retry-After", "1")
		inference.WriteError(w, status, code, err.Error())
		return
	}
	id := ticket.ID()
	outcome := "upstream_error"
	defer func() {
		ticket.Finish(outcome)
		inferenceLog.Info("inference", "event", "inference_complete", "service_id", "chat-router", "request_id", trace, "deployment_id", id, "model", model, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds())
	}()
	if ctx.Err() != nil {
		outcome = "canceled"
		return
	}
	// The queue may have waited across a stop or an expired worker heartbeat.
	target, err := s.controller.InferenceTarget(id)
	if err != nil {
		inference.WriteError(w, 503, "model_unavailable", err.Error())
		return
	}
	d, _ := s.controller.GetDeployment(id)
	body["model"], _ = json.Marshal(id)
	body["stream"] = json.RawMessage("true")
	body["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	data, _ := json.Marshal(body)
	up, err := http.NewRequestWithContext(ctx, "POST", target+"/inference/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		inference.WriteError(w, 502, "upstream_error", "invalid agent address")
		return
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("X-Deployment-ID", id)
	up.Header.Set("X-Assignment-Token", d.Token)
	up.Header.Set("X-Request-ID", trace)
	if s.token != "" {
		up.Header.Set("Authorization", "Bearer "+s.token)
	}
	s.expectServices()
	// Gateway traffic measures dispatched payloads; the router service separately
	// measures client payloads, including rejected requests. Do not sum layers.
	measured := r.Clone(ctx)
	measured.Body = io.NopCloser(bytes.NewReader(data))
	s.metrics.HTTP("gateway/"+id, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Mark the consumed request bytes before Forward sends its separate body.
		_, _ = io.Copy(io.Discard, measured.Body)
		outcome = inference.Forward(w, up, s.chatClient, stream, model, ticket, s.inference.Policy)
	})).ServeHTTP(w, measured)
	if outcome != "completed" && stream {
		// HTTP 200 may already be committed. Semantic stream failures are exposed
		// independently by inference metrics, without falsifying HTTP status.
		s.metrics.Update("gateway/"+id, func(m *telemetry.Service) {
			if m.Engine == nil {
				m.Engine = map[string]float64{}
			}
			m.Engine["stream_failures_total"]++
		})
	}
}

func (s *apiServer) inferenceMetrics(w io.Writer) {
	for _, m := range s.inference.Snapshot() {
		labels := "deployment=" + strconv.Quote(m.DeploymentID)
		values := map[string]any{"active": m.Active, "queued": m.Queued, "requests_total": m.Requests, "completed_total": m.Completed, "failed_total": m.Failed, "canceled_total": m.Canceled, "rejected_total": m.Rejected, "queue_timeouts_total": m.QueueTimeouts, "first_token_timeouts_total": m.FirstTimeouts, "idle_timeouts_total": m.IdleTimeouts, "total_timeouts_total": m.TotalTimeouts, "ttft_seconds_count": m.TTFTCount, "ttft_seconds_sum": m.TTFTSum}
		for name, value := range values {
			fmt.Fprintf(w, "platform_inference_%s{%s} %v\n", name, labels, value)
		}
		for i, b := range inference.TTFTBounds {
			fmt.Fprintf(w, "platform_inference_ttft_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(b, 'g', -1, 64), m.TTFTBuckets[i])
		}
		fmt.Fprintf(w, "platform_inference_ttft_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, m.TTFTCount)
	}
}
