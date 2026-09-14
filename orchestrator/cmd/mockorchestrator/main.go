// Command mockorchestrator serves the identical HTTP surface as
// cmd/orchestrator (api.New/Routes -- the same code, not a reimplementation)
// against internal/mockbackend's in-memory, no-podman backend instead of a
// real container runtime.
//
// For a portal or dashboard developer to build against a real, running
// instance of the real API contract (internal/dashboard/static/openapi.yaml) with zero
// podman/host/root requirement, on any machine. Not a test -- a real
// binary meant to be run, e.g. `go run ./cmd/mockorchestrator`.
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
	"praxis-orchestrator/internal/dashboard"
	"praxis-orchestrator/internal/metrics"
	"praxis-orchestrator/internal/mockbackend"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Defaults intentionally differ from cmd/orchestrator's: a mock has no
	// production disk/memory to protect, so it defaults to a token and a
	// generous capacity a developer can start using immediately without
	// first reading an env-var list, while still overridable for anyone
	// who wants to reproduce a specific production configuration.
	token := envOr("PRAXIS_ORCH_TOKEN", "mock-token")
	addr := envOr("PRAXIS_LISTEN", "127.0.0.1:8081")
	reapEvery := time.Duration(envInt("PRAXIS_REAP_INTERVAL", 30)) * time.Second
	capacityWeight := envInt("PRAXIS_CAPACITY_WEIGHT", 1000)
	execTimeout := time.Duration(envInt("PRAXIS_EXEC_TIMEOUT", 120)) * time.Second

	backend := mockbackend.New()
	// "orchestrator", not "mockorchestrator": Registry.Write() gates the
	// entire capacity_weight_used/_limit block on view == "orchestrator"
	// exactly (internal/metrics/registry.go) -- found by actually running
	// this and grepping /metrics for the capacity lines, which came back
	// empty. Same class of view-string-mismatch bug this project has
	// already been bitten by once (internal/metrics/labels.go's
	// underscore/hyphen mismatch) -- verified as fixed here by re-running
	// the same real check, not just re-reading the code.
	reg := metrics.NewRegistry("orchestrator", "dev")

	apiRoutes := api.New(backend, backend, reg, api.Config{
		Token: token, CapacityWeight: capacityWeight, ExecTimeout: execTimeout,
	}, log).Routes()

	// Same dashboard, same reasoning for /ui/ over /dashboard/ as
	// cmd/orchestrator -- mounted here too so a dashboard developer can
	// point their browser at the mock directly, not just curl/websocket
	// clients.
	dashboardRoutes, err := dashboard.Handler("/ui/")
	if err != nil {
		log.Error("dashboard asset init failed", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/", apiRoutes)
	mux.Handle("/ui/", dashboardRoutes)
	// Same convenience redirect as cmd/orchestrator -- see its comment.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go reapLoop(ctx, backend, reapEvery, reg, capacityWeight, log)

	go func() {
		log.Info("mockorchestrator listening", "addr", addr, "token", token, "capacity_weight", capacityWeight)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// reapLoop mirrors cmd/orchestrator's own reaper closely enough to publish
// real, consistent /metrics and /sessions data -- deliberately simpler,
// not shared code: it's a handful of lines, cmd/orchestrator's own
// reapTick has a disk-limit branch this mock has nothing to check against
// (mockbackend.Backend.Reap is TTL-only), and two different `main`
// packages can't import one another's unexported functions anyway.
func reapLoop(ctx context.Context, b *mockbackend.Backend, every time.Duration, reg *metrics.Registry, capacityLimit int, log *slog.Logger) {
	tick := func() {
		killed, err := b.Reap(ctx)
		if err != nil {
			log.Error("reap failed", "err", err)
			return
		}
		for range killed {
			reg.IncDestroy("ttl")
		}
		snap := metrics.CollectManaged(ctx, b)
		reg.SetSnapshot(snap)
		reg.SetCapacity(metrics.WeightInFlight(snap), capacityLimit)
	}

	tick() // prime immediately, same reasoning as cmd/orchestrator's reaper
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
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
