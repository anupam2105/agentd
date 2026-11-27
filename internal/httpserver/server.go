// Package httpserver builds and manages the agentd HTTP API.
package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anupam2105/agentd/internal/domain"
	"github.com/anupam2105/agentd/internal/engine"
	"github.com/anupam2105/agentd/internal/observability"
	"github.com/anupam2105/agentd/internal/store"
)

// MaxBodyBytes caps request bodies (LLM conversations can be large).
const MaxBodyBytes = 4 << 20

// Options configures the server.
type Options struct {
	Addr     string
	Version  string
	Logger   *slog.Logger
	Metrics  *observability.Metrics
	Engine   *engine.Engine
	APIToken string
}

// Server owns the http.Server lifecycle.
type Server struct {
	srv *http.Server
	o   Options
}

// New constructs a Server.
func New(o Options) *Server {
	s := &Server{o: o}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.Handle("GET /metrics", o.Metrics.Handler())

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("POST /v1/runs", s.createRun)
	api("GET /v1/runs", s.listRuns)
	api("GET /v1/runs/{id}", s.getRun)
	api("POST /v1/runs/{id}/finish", s.finishRun)
	api("POST /v1/runs/{id}/cancel", s.cancelRun)
	api("GET /v1/runs/{id}/steps", s.listSteps)
	api("POST /v1/runs/{id}/steps", s.executeStep)
	api("GET /v1/runs/{id}/steps/{key}", s.getStep)
	api("POST /v1/runs/{id}/steps/{key}/decision", s.decide)
	api("GET /v1/approvals", s.listApprovals)
	api("GET /v1/tools", s.listTools)

	s.srv = &http.Server{
		Addr:              o.Addr,
		Handler:           s.instrument(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// LLM and sandboxed tool steps execute synchronously within the request.
		WriteTimeout: 15 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}
	return s
}

// Start begins serving. It blocks until the server stops.
func (s *Server) Start() error { return s.srv.ListenAndServe() }

// Shutdown gracefully stops the server, draining in-flight requests until ctx is done.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Handler exposes the fully instrumented http.Handler for testing.
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// ---------------------------------------------------------------- handlers

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.o.Engine.Store().Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.o.Version, "api": "v1"})
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req engine.CreateRunRequest
	if !decode(w, r, &req) {
		return
	}
	run, created, err := s.o.Engine.CreateRun(r.Context(), &req)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, run)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	runs, err := s.o.Engine.Store().ListRuns(r.Context(), store.RunFilter{
		Status: domain.RunStatus(q.Get("status")), Agent: q.Get("agent"), Limit: limit,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": nonNil(runs)})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.o.Engine.Store().GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, notFoundOr(err, "run"))
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) finishRun(w http.ResponseWriter, r *http.Request) {
	var req engine.FinishRunRequest
	if !decode(w, r, &req) {
		return
	}
	run, err := s.o.Engine.FinishRun(r.Context(), r.PathValue("id"), &req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	run, err := s.o.Engine.CancelRun(r.Context(), r.PathValue("id"), req.Reason)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listSteps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.o.Engine.Store().GetRun(r.Context(), id); err != nil {
		writeError(w, notFoundOr(err, "run"))
		return
	}
	steps, err := s.o.Engine.Store().ListSteps(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": nonNil(steps)})
}

// stepResponse is a step plus whether it was served from the journal.
type stepResponse struct {
	*domain.Step
	Replayed bool `json:"replayed"`
}

func (s *Server) executeStep(w http.ResponseWriter, r *http.Request) {
	var req engine.StepRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.o.Engine.ExecuteStep(r.Context(), r.PathValue("id"), &req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, stepStatus(out.Step), stepResponse{Step: out.Step, Replayed: out.Replayed})
}

func (s *Server) getStep(w http.ResponseWriter, r *http.Request) {
	st, err := s.o.Engine.Store().GetStep(r.Context(), r.PathValue("id"), r.PathValue("key"))
	if err != nil {
		writeError(w, notFoundOr(err, "step"))
		return
	}
	writeJSON(w, http.StatusOK, stepResponse{Step: st})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req engine.DecisionRequest
	if !decode(w, r, &req) {
		return
	}
	st, err := s.o.Engine.Decide(r.Context(), r.PathValue("id"), r.PathValue("key"), &req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stepResponse{Step: st})
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	steps, err := s.o.Engine.Store().ListStepsByStatus(r.Context(), domain.StepAwaitingApproval, 200)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": nonNil(steps)})
}

func (s *Server) listTools(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": s.o.Engine.Tools().List()})
}

// stepStatus maps a step to an HTTP status: 202 while it still needs a
// decision or a re-post, 200 once it has an outcome.
func stepStatus(st *domain.Step) int {
	switch st.Status {
	case domain.StepAwaitingApproval, domain.StepApproved, domain.StepRunning:
		return http.StatusAccepted
	default:
		return http.StatusOK
	}
}

// ---------------------------------------------------------------- plumbing

func (s *Server) auth(next http.Handler) http.Handler {
	if s.o.APIToken == "" {
		return next
	}
	want := []byte("Bearer " + s.o.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentd"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: &engine.Error{Code: "unauthorized", Message: "missing or invalid bearer token"}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type errorBody struct {
	Error *engine.Error `json:"error"`
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("trailing data after JSON body")
	}
	if err != nil {
		status := http.StatusBadRequest
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			status = http.StatusRequestEntityTooLarge
		}
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeJSON(w, status, errorBody{Error: &engine.Error{Code: engine.ErrCodeInvalid, Message: err.Error()}})
		return false
	}
	return true
}

func notFoundOr(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return &engine.Error{Status: http.StatusNotFound, Code: engine.ErrCodeNotFound, Message: what + " not found"}
	}
	return err
}

func writeError(w http.ResponseWriter, err error) {
	e := engine.AsError(err)
	if e.Status >= 500 {
		slog.Error("internal error", "error", err)
		e = &engine.Error{Status: e.Status, Code: e.Code, Message: "internal error"}
	}
	if e.Retryable {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, e.Status, errorBody{Error: e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // a JSON API; keep "&&" in commands readable
	_ = enc.Encode(v)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// instrument wraps the mux with metrics and structured logging.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		duration := time.Since(start)
		route := routeLabel(r)
		s.o.Metrics.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.o.Metrics.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(duration.Seconds())

		level := slog.LevelInfo
		if route == "/healthz" || route == "/readyz" || route == "/metrics" {
			level = slog.LevelDebug
		}
		s.o.Logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", duration.Milliseconds(),
		)
	})
}

// routeLabel uses the matched ServeMux pattern so metric label cardinality is
// bounded by registered routes; unmatched requests are "unknown".
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unknown"
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		return path
	}
	return r.Pattern
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}
