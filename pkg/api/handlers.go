package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/sirupsen/logrus"
)

// Manager interface decoupling the HTTP layer from SandboxManager.
type Manager interface {
	Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResponse, error)
	Branch(ctx context.Context, parentID string, newID string) (*BranchResponse, error)
	HypervisorName() string
	KvmAvailable() bool
	AvailableTaps() int
	ActiveCount() int
	ListSandboxes() []SandboxInfo
}

// Server encapsulates the HTTP API router and handlers.
type Server struct {
	mgr    Manager
	logger *logrus.Logger
}

// NewServer creates a new API Server.
func NewServer(mgr Manager, logger *logrus.Logger) *Server {
	return &Server{
		mgr:    mgr,
		logger: logger,
	}
}

// Routes constructs the Chi router with middlewares.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.HandleHealthz)
	r.Post("/v1/execute", s.HandleExecute)
	r.Get("/v1/sandboxes", s.HandleListSandboxes)
	r.Post("/v1/sandboxes/{id}/branch", s.HandleBranchSandbox)
	r.Post("/sandboxes/{id}/branch", s.HandleBranchSandbox)

	return r
}

// HandleHealthz returns health status, hypervisor readiness, and node capacity.
func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:          "healthy",
		Version:         "0.1.0",
		Hypervisor:      s.mgr.HypervisorName(),
		KvmAvailable:    s.mgr.KvmAvailable(),
		ActiveSandboxes: s.mgr.ActiveCount(),
		AvailableTaps:   s.mgr.AvailableTaps(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleExecute handles synchronous code execution inside a microVM sandbox.
func (s *Server) HandleExecute(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request payload"}`, http.StatusBadRequest)
		return
	}

	if req.Code == "" {
		http.Error(w, `{"error":"code payload cannot be empty"}`, http.StatusBadRequest)
		return
	}

	resp, err := s.mgr.Execute(r.Context(), req)
	if err != nil {
		s.logger.Errorf("Execute handler error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleBranchSandbox snapshots and forks an existing sandbox into a parallel branch.
func (s *Server) HandleBranchSandbox(w http.ResponseWriter, r *http.Request) {
	parentID := chi.URLParam(r, "id")
	if parentID == "" {
		http.Error(w, `{"error":"parent sandbox id is required"}`, http.StatusBadRequest)
		return
	}

	var req BranchRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = io.Copy(io.Discard, r.Body)
	}

	resp, err := s.mgr.Branch(r.Context(), parentID, req.NewSandboxID)
	if err != nil {
		s.logger.Errorf("Branch handler error for %s: %v", parentID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleListSandboxes returns active sandbox instances.
func (s *Server) HandleListSandboxes(w http.ResponseWriter, r *http.Request) {
	sandboxes := s.mgr.ListSandboxes()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sandboxes)
}
