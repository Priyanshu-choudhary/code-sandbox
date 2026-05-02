package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Priyanshu-choudhary/code-sandbox/internal/config"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/executor"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/registry"
)

type Server struct {
	Cfg  config.Config
	Reg  *registry.Registry
	Exec *executor.Executor
}

func New(cfg config.Config, reg *registry.Registry, exec *executor.Executor) *Server {
	return &Server{Cfg: cfg, Reg: reg, Exec: exec}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(s.Cfg.RequestTimeout))
	r.Use(jsonAccessLog)

	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Get("/info", s.info)
	r.Get("/metrics", s.metrics)
	r.Post("/run", s.run)
	return r
}

// jsonAccessLog emits one slog line per HTTP request. Healthz / metrics are
// not skipped on purpose - it's useful to see ALB pokes during incident
// investigation. Drop them later via a level filter if the volume hurts.
func jsonAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		slog.Info("http",
			"req_id", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes_out", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Exec.Stats())
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	diag := map[string]any{}
	healthy := true

	if _, err := os.Stat(s.Cfg.NsjailPath); err != nil {
		diag["nsjail"] = "missing: " + err.Error()
		healthy = false
	} else {
		diag["nsjail"] = "ok"
	}

	// For each language, verify the binary the user can't supply exists.
	// For compiled languages the run command is the build artifact (created
	// at /run time); we check the compiler instead.
	langDiag := map[string]string{}
	for _, name := range s.Reg.Names() {
		lang, _ := s.Reg.Get(name)
		probe := lang.Run.Command
		if lang.Build != nil {
			probe = lang.Build.Command
		}
		if _, err := os.Stat(probe); err != nil {
			langDiag[name] = "missing: " + probe
			healthy = false
		} else {
			langDiag[name] = "ok"
		}
	}
	diag["languages"] = langDiag

	status := http.StatusOK
	if !healthy {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, diag)
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         "0.1.0-mvp",
		"go":              runtime.Version(),
		"nsjail_path":     s.Cfg.NsjailPath,
		"languages":       s.Reg.Names(),
		"max_concurrency": s.Cfg.MaxConcurrency,
		"limits": map[string]any{
			"max_source_bytes": s.Cfg.MaxSourceBytes,
			"max_output_bytes": s.Cfg.MaxOutputBytes,
			"max_test_cases":   s.Cfg.MaxTestCases,
		},
	})
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MaxSourceBytes*4)
	defer r.Body.Close()

	var req executor.Request
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Language == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "language required")
		return
	}
	if req.Source == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "source required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()

	resp, err := s.Exec.Run(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, executor.ErrUnknownLanguage):
			writeErr(w, http.StatusBadRequest, "unknown_language", err.Error())
		case errors.Is(err, executor.ErrSourceTooLarge):
			writeErr(w, http.StatusBadRequest, "source_too_large", err.Error())
		case errors.Is(err, executor.ErrTooManyTests):
			writeErr(w, http.StatusBadRequest, "too_many_tests", err.Error())
		case errors.Is(err, executor.ErrDisallowedFlag):
			writeErr(w, http.StatusBadRequest, "disallowed_flag", err.Error())
		case errors.Is(err, executor.ErrOverrideTooBig):
			writeErr(w, http.StatusBadRequest, "override_too_big", err.Error())
		case errors.Is(err, executor.ErrOverloaded):
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "overloaded", err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]string{"error": kind, "message": msg})
}
