// Command hostmon is the second, independent view of sandbox state.
//
// It shares no runtime state with the orchestrator, imports nothing from
// internal/sandbox, and never calls the orchestrator's API. It reads the podman
// socket directly and lists every container without a label filter.
//
// That independence is the point. The orchestrator lists with a label filter,
// so a container whose labels are missing or corrupt is invisible to it — and a
// process cannot report an orphan it cannot see. hostmon can, and it keeps
// reporting when the orchestrator is deadlocked.
//
// Runs as praxis-sbx (uid 1001) under the same user systemd instance that owns
// the socket. Binds loopback only.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/client"

	"github.com/tpb/praxis-orchestrator/internal/metrics"
)

var version = "dev"

type config struct {
	listen       string
	dockerHost   string
	interval     time.Duration
	slicePath    string
	storagePath  string
	collectStats bool
}

func loadConfig() config {
	c := config{
		listen:      env("PRAXIS_HOSTMON_LISTEN", "127.0.0.1:9102"),
		dockerHost:  env("DOCKER_HOST", "unix:///run/user/1001/podman/podman.sock"),
		slicePath:   env("PRAXIS_SLICE_PATH", "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/praxis-sbx.slice"),
		storagePath: env("PRAXIS_STORAGE_PATH", "/home/praxis-sbx/.local/share/containers"),
		interval:    15 * time.Second,
	}
	if v := os.Getenv("PRAXIS_HOSTMON_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.interval = d
		}
	}
	c.collectStats = os.Getenv("PRAXIS_HOSTMON_STATS") == "1"
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := loadConfig()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cli, err := client.NewClientWithOpts(
		client.WithHost(cfg.dockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Fatalf("hostmon: docker client: %v", err)
	}
	defer cli.Close()

	reg := metrics.NewRegistry("host", version)

	// Prime once so the first scrape is not an empty snapshot indistinguishable
	// from a genuinely idle box.
	reg.SetSnapshot(poll(cli, cfg))

	go func() {
		t := time.NewTicker(cfg.interval)
		defer t.Stop()
		for range t.C {
			reg.SetSnapshot(poll(cli, cfg))
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler(reg))
	mux.Handle("/sessions", metrics.SessionsHandler(reg))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("hostmon %s listening on %s, socket %s, interval %s", version, cfg.listen, cfg.dockerHost, cfg.interval)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("hostmon: listen: %v", err)
	}
}

func poll(cli *client.Client, cfg config) metrics.Snapshot {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// No label filter. Ground truth.
	snap := metrics.CollectAll(ctx, cli)

	snap.SliceProcs = readIntFile(filepath.Join(cfg.slicePath, "pids.current"))
	snap.SliceMemoryBytes = uint64(readIntFile(filepath.Join(cfg.slicePath, "memory.current")))

	some, full := readPressure(filepath.Join(cfg.slicePath, "memory.pressure"))
	snap.PressureSome, snap.PressureFull = some, full

	snap.StorageFreeBytes = freeBytes(cfg.storagePath)

	if cfg.collectStats {
		snap.SessionStats = collectStats(ctx, cli, snap)
	}

	return snap
}

// collectStats costs one stats call per running container per poll. Off by
// default; enable when benchmarking or investigating a specific session.
func collectStats(ctx context.Context, cli *client.Client, snap metrics.Snapshot) map[string]metrics.SessionStat {
	out := map[string]metrics.SessionStat{}
	for _, s := range snap.Sessions {
		if s.State != "running" {
			continue
		}
		resp, err := cli.ContainerStatsOneShot(ctx, s.ContainerID)
		if err != nil {
			continue
		}
		var v struct {
			MemoryStats struct {
				Usage uint64 `json:"usage"`
			} `json:"memory_stats"`
			PidsStats struct {
				Current uint64 `json:"current"`
			} `json:"pids_stats"`
		}
		err = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if err != nil {
			continue
		}
		out[s.AttemptID] = metrics.SessionStat{
			AttemptID:   s.AttemptID,
			Runbook:     s.Runbook,
			MemoryBytes: v.MemoryStats.Usage,
			Pids:        v.PidsStats.Current,
		}
	}
	return out
}

func readIntFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}

// readPressure parses cgroup v2 PSI. Returns avg60 for some and full as
// fractions, not percentages.
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
func readPressure(path string) (some, full float64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		var val float64
		for _, kv := range fields[1:] {
			if strings.HasPrefix(kv, "avg60=") {
				val, _ = strconv.ParseFloat(strings.TrimPrefix(kv, "avg60="), 64)
			}
		}
		switch fields[0] {
		case "some":
			some = val / 100
		case "full":
			full = val / 100
		}
	}
	return some, full
}

// freeBytes reports space available to an unprivileged writer on the volume
// holding path — the 24G loop device, in the deployed layout.
func freeBytes(path string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}
