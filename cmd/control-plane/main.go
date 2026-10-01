package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

type apiServer struct {
	controller *platform.Controller
	webDir     string
	token      string
	metrics    *telemetry.Registry
	stateFile  string
	initOnce   sync.Once
	writeMu    sync.Mutex
}

func main() {
	addr := flag.String("addr", envOr("CONTROL_PLANE_ADDR", "127.0.0.1:8080"), "listen address")
	webDir := flag.String("web-dir", envOr("WEB_DIR", ""), "optional directory for the operator console")
	stateFile := flag.String("state-file", envOr("STATE_FILE", "data/control-plane.json"), "durable single-process state file; empty disables persistence")
	servicesConfig := flag.String("services-config", envOr("SERVICES_CONFIG", ""), "optional infrastructure health and cAdvisor probe config")
	flag.Parse()

	controller, err := platform.LoadController(*stateFile)
	if err != nil {
		log.Fatal(err)
	}
	server := &apiServer{controller: controller, webDir: *webDir, token: strings.TrimSpace(os.Getenv("CONTROL_PLANE_TOKEN")), stateFile: *stateFile}
	server.initMetrics()
	if err := server.metrics.StartProbes(context.Background(), *servicesConfig); err != nil {
		log.Fatal(err)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			server.writeMu.Lock()
			controller.ReconcileDeployments()
			controller.ReconcilePending()
			if err := controller.Save(*stateFile); err != nil {
				log.Printf("checkpoint: %v", err)
				server.metrics.State("control-plane", "degraded", "state checkpoint failed")
			} else {
				server.metrics.State("control-plane", "ready", "")
			}
			server.writeMu.Unlock()
		}
	}()
	log.Printf("control plane listening on %s", *addr)
	if err := (&http.Server{Addr: *addr, Handler: server, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}).ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func (s *apiServer) serveAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := strings.TrimSuffix(r.URL.Path, "/")
	if (strings.HasPrefix(path, "/api/") || path == "/metrics") && s.token != "" {
		want := "Bearer " + s.token
		if r.Header.Get("Authorization") != want {
			writeError(w, http.StatusUnauthorized, errors.New("bearer token required"))
			return
		}
	}
	if s.extendedRoute(w, r, path) {
		return
	}
	switch {
	case path == "/healthz" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case path == "/readyz" && r.Method == http.MethodGet:
		for _, service := range s.metrics.List(false) {
			if service.ID == "control-plane" && service.Status != "ready" {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": service.Status, "message": service.Message})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	case path == "/api/v1/resources" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.controller.Nodes.ListNodes())
	case path == "/api/v1/resources" && r.Method == http.MethodPost:
		s.createResource(w, r)
	case path == "/api/v1/resources/heartbeat" && r.Method == http.MethodPost:
		s.heartbeat(w, r)
	case strings.HasPrefix(path, "/api/v1/resources/"):
		s.resourceRoute(w, r, strings.TrimPrefix(path, "/api/v1/resources/"))
	case path == "/api/v1/tasks" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.controller.ListTasks())
	case path == "/api/v1/tasks" && r.Method == http.MethodPost:
		s.submitTask(w, r)
	case path == "/api/v1/schedule/preview" && r.Method == http.MethodPost:
		s.preview(w, r)
	case strings.HasPrefix(path, "/api/v1/tasks/"):
		s.taskRoute(w, r, strings.TrimPrefix(path, "/api/v1/tasks/"))
	case strings.HasPrefix(path, "/api/v1/agents/") && strings.HasSuffix(path, "/tasks") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/agents/"), "/tasks")
		writeJSON(w, http.StatusOK, s.controller.TasksForNode(id))
	default:
		if s.webDir != "" && r.Method == http.MethodGet && !strings.HasPrefix(path, "/api/") {
			http.FileServer(http.Dir(s.webDir)).ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func (s *apiServer) heartbeat(w http.ResponseWriter, r *http.Request) {
	var node platform.ResourceNode
	if err := decodeJSON(r, &node); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	stored, _, err := s.controller.ApplyHeartbeat(node)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, stored)
}

func (s *apiServer) createResource(w http.ResponseWriter, r *http.Request) {
	var node platform.ResourceNode
	if err := decodeJSON(r, &node); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if node.Name == "" {
		node.Name = node.ID
	}
	created, err := s.controller.RegisterNode(node)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, platform.ErrNodeExists) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *apiServer) resourceRoute(w http.ResponseWriter, r *http.Request, suffix string) {
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := urlPathPart(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPut {
		var node platform.ResourceNode
		if err := decodeJSON(r, &node); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		updated, err := s.controller.UpdateNode(id, node)
		if err != nil {
			s.writeNodeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.controller.DeleteNode(id); err != nil {
			s.writeNodeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost {
		var node platform.ResourceNode
		var err error
		switch parts[1] {
		case "drain":
			node, err = s.controller.DrainNode(id)
		case "enable":
			node, err = s.controller.EnableNode(id)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.writeNodeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, node)
		return
	}
	http.NotFound(w, r)
}

func (s *apiServer) writeNodeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, platform.ErrNodeNotFound):
		status = http.StatusNotFound
	case errors.Is(err, platform.ErrNodeBusy):
		status = http.StatusConflict
	}
	writeError(w, status, err)
}

func urlPathPart(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "\\?#") {
		return "", fmt.Errorf("invalid resource node id")
	}
	return value, nil
}

func (s *apiServer) submitTask(w http.ResponseWriter, r *http.Request) {
	var spec platform.TaskSpec
	if err := decodeJSON(r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if spec.IdempotencyKey == "" {
		spec.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	task, decision, existing, err := s.controller.Submit(spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status := http.StatusAccepted
	if existing {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"task": task, "decision": decision, "idempotent_replay": existing})
}

func (s *apiServer) preview(w http.ResponseWriter, r *http.Request) {
	var spec platform.TaskSpec
	if err := decodeJSON(r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if spec.ID == "" {
		spec.ID = "preview"
	}
	decision, err := s.controller.Scheduler.Schedule(spec)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, decision)
		return
	}
	writeJSON(w, http.StatusOK, decision)
}

func (s *apiServer) taskRoute(w http.ResponseWriter, r *http.Request, suffix string) {
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		task, ok := s.controller.GetTask(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, task)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		phase := platform.TaskCancelled
		if existing, ok := s.controller.GetTask(id); ok && existing.Phase == platform.TaskRunning {
			phase = platform.TaskCancelling
		}
		task, err := s.controller.UpdateTaskPhase(id, phase, "cancelled by user")
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, task)
		return
	}
	if len(parts) == 2 && parts[1] == "state" && r.Method == http.MethodPost {
		var req struct {
			Phase  platform.TaskPhase `json:"phase"`
			Reason string             `json:"reason"`
		}
		if err := decodeJSON(r, &req); err != nil || req.Phase == "" {
			writeError(w, http.StatusBadRequest, errors.New("phase is required"))
			return
		}
		task, err := s.controller.UpdateTaskPhase(id, req.Phase, req.Reason)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, task)
		return
	}
	http.NotFound(w, r)
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
