// Command orchestrator is the Praxis sandbox interpreter.
//
// Scope: takes a Runbook plus an attempt_id and drives a container runtime.
// It knows nothing about tickets, stars, gates, scoring or candidates. If a
// field named difficulty or fault_id ever appears in this binary, the boundary
// has leaked.
//
// State lives on the runtime as labels. There is no database and no in-memory
// registry -- a restart rebuilds the whole world from a label selector.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"praxis-orchestrator/internal/api"
	"praxis-orchestrator/internal/sandbox"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	token := os.Getenv("PRAXIS_ORCH_TOKEN")
	if token == "" {
		log.Error("PRAXIS_ORCH_TOKEN is required")
		os.Exit(1)
	}

	addr := envOr("PRAXIS_LISTEN", "127.0.0.1:8081")
	reapEvery := time.Duration(envInt("PRAXIS_REAP_INTERVAL", 30)) * time.Second
	maxConcurrent := envInt("PRAXIS_MAX_CONCURRENT", 3)
	execTimeout := time.Duration(envInt("PRAXIS_EXEC_TIMEOUT", 120)) * time.Second

	backend, err := sandbox.NewContainerBackend()
	if err != nil {
		log.Error("backend init failed; is DOCKER_HOST pointing at the rootless socket?", "err", err)
		os.Exit(1)
	}
	defer backend.Close()

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(backend, api.Config{Token: token, MaxConcurrent: maxConcurrent, ExecTimeout: execTimeout}, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go reaper(ctx, backend, reapEvery, log)

	go func() {
		log.Info("orchestrator listening", "addr", addr, "max_concurrent", maxConcurrent)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// reaper enforces TTL. This is what makes a stateless orchestrator safe: if the
// portal loses an attempt record, the container still dies on schedule, because
// expiry does not depend on anyone remembering why the sandbox exists.
func reaper(ctx context.Context, b sandbox.Backend, every time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			killed, err := b.Reap(ctx)
			if err != nil {
				log.Error("reap cycle failed", "err", err)
				continue
			}
			if len(killed) > 0 {
				log.Info("reaped expired sandboxes", "count", len(killed), "attempts", killed)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
