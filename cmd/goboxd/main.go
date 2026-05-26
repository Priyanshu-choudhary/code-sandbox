package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Priyanshu-choudhary/code-sandbox/internal/api"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/config"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/executor"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/registry"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/worker"
)

func main() {
	// Structured JSON logs from day 0 - the hackathon judging criteria call
	// out "structured JSON logs" as a bonus, and this also makes it trivial
	// to ship logs into a tool like CloudWatch, Loki, or Datadog later.
	level := slog.LevelInfo
	if v := os.Getenv("GOBOXD_LOG_LEVEL"); strings.EqualFold(v, "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	cfg := config.Load()
	slog.Info("starting",
		"addr", cfg.HTTPAddr,
		"nsjail", cfg.NsjailPath,
		"languages_dir", cfg.LanguagesDir,
		"max_concurrency", cfg.MaxConcurrency,
		"queue_depth", cfg.QueueDepth,
		"jail_uid", cfg.JailUID,
	)

	if err := os.MkdirAll(cfg.JobRoot, 0o755); err != nil {
		slog.Error("mkdir job root", "err", err)
		os.Exit(1)
	}
	sweepStaleJobs(cfg.JobRoot, time.Hour)

	reg := registry.New()
	if err := reg.LoadDir(cfg.LanguagesDir); err != nil {
		slog.Error("load languages", "err", err)
		os.Exit(1)
	}
	slog.Info("languages registered", "languages", reg.Names(), "count", len(reg.Names()))

	exec := executor.New(cfg, reg)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(cfg, reg, exec).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown via a cancellable root context.
	rootCtx, rootCancel := context.WithCancel(context.Background())

	// Start SQS worker if configured.
	workerDone := make(chan struct{})
	if cfg.SQSQueueURL != "" {
		slog.Info("sqs worker enabled",
			"queue_url", cfg.SQSQueueURL,
			"redis_addr", cfg.RedisAddr,
			"redis_tls", cfg.RedisTLS,
			"sqs_workers", cfg.SQSWorkers,
		)
		w, err := worker.New(cfg.SQSQueueURL, cfg.RedisAddr, cfg.RedisTLS, exec, cfg.SQSWorkers)
		if err != nil {
			slog.Error("failed to create sqs worker", "err", err)
			os.Exit(1)
		}
		go func() {
			defer close(workerDone)
			w.Start(rootCtx)
		}()
	} else {
		slog.Info("sqs worker disabled (SQS_QUEUE_URL not set)")
		close(workerDone)
	}

	// Signal handler.
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutdown signal received")
		// Cancel root context first — stops the SQS worker.
		rootCancel()
		// Then shut down HTTP.
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
		// Wait for worker to drain.
		<-workerDone
		close(idle)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen", "err", err)
		rootCancel()
		os.Exit(1)
	}
	<-idle
	slog.Info("bye")
}

// sweepStaleJobs removes job dirs older than maxAge at startup.
// Guards against stale state when the previous instance crashed.
func sweepStaleJobs(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "job-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		slog.Info("startup sweep", "removed_stale_jobs", removed)
	}
}
