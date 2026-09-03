package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// No prometheus/client_golang dependency. The exposition format is a dozen
// lines of text and the module proxy has already proven unreliable on this box;
// adding a dependency to emit key-value pairs is not a trade worth making.

// Snapshot is the projection of one list call. The orchestrator's reaper writes
// it on every tick; hostmon's poller writes it on its own interval. Scrapes
// read the last one and never touch the socket.
type Snapshot struct {
	View      string // "orchestrator" | "host"
	TakenAt   time.Time
	Sessions  []Session
	Orphans   map[OrphanKind]int
	Total     int // every container seen, managed or not (host view)
	Err       error

	// Host-view extras. Zero on the orchestrator view.
	SliceProcs        int
	SliceMemoryBytes  uint64
	PressureSome      float64
	PressureFull      float64
	StorageFreeBytes  uint64
	SessionStats      map[string]SessionStat // keyed by attempt_id, opt-in
}

// SessionStat is per-session resource use. Opt-in (PRAXIS_HOSTMON_STATS=1)
// because it costs one stats call per container per poll.
type SessionStat struct {
	AttemptID   string
	Runbook     string
	MemoryBytes uint64
	Pids        uint64
}

// Registry holds monotonic counters plus the most recent Snapshot.
type Registry struct {
	mu   sync.RWMutex
	view string

	snap Snapshot

	spawn   map[string]uint64 // result -> count
	destroy map[string]uint64 // reason -> count

	reaperLastSuccess time.Time
	reaperDuration    time.Duration

	capacityUsed  int
	capacityLimit int

	buildVersion string
}

func NewRegistry(view, buildVersion string) *Registry {
	return &Registry{
		view:         view,
		spawn:        map[string]uint64{},
		destroy:      map[string]uint64{},
		buildVersion: buildVersion,
		snap:         Snapshot{View: view, Orphans: map[OrphanKind]int{}},
	}
}

func (r *Registry) SetSnapshot(s Snapshot) {
	if s.Orphans == nil {
		s.Orphans = map[OrphanKind]int{}
	}
	s.View = r.view
	r.mu.Lock()
	r.snap = s
	r.mu.Unlock()
}

// IncSpawn result is one of: ok, conflict, denied_capacity, error.
// "conflict" is errdefs.IsConflict — a spike means the portal is retrying
// against a live attempt id, not that the sandbox failed.
func (r *Registry) IncSpawn(result string) {
	r.mu.Lock()
	r.spawn[result]++
	r.mu.Unlock()
}

// IncDestroy reason is one of: ttl, explicit, error.
func (r *Registry) IncDestroy(reason string) {
	r.mu.Lock()
	r.destroy[reason]++
	r.mu.Unlock()
}

// ObserveReaper records a completed reaper tick. Call it only on success —
// the staleness of this timestamp is the alert.
func (r *Registry) ObserveReaper(d time.Duration) {
	r.mu.Lock()
	r.reaperLastSuccess = time.Now()
	r.reaperDuration = d
	r.mu.Unlock()
}

func (r *Registry) SetCapacity(used, limit int) {
	r.mu.Lock()
	r.capacityUsed, r.capacityLimit = used, limit
	r.mu.Unlock()
}

var expiryWindows = []time.Duration{60 * time.Second, 300 * time.Second, 900 * time.Second}

// Write renders the exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	s := r.snap
	v := r.view

	var b strings.Builder

	help(&b, "praxis_build_info", "gauge", "Build metadata; value is always 1.")
	line(&b, "praxis_build_info", map[string]string{"view": v, "version": r.buildVersion}, 1)

	scrapeErr := 0.0
	if s.Err != nil {
		scrapeErr = 1
	}
	help(&b, "praxis_scrape_error", "gauge", "1 when the last collection failed; a zero session count with this set is not a real zero.")
	line(&b, "praxis_scrape_error", map[string]string{"view": v}, scrapeErr)

	help(&b, "praxis_snapshot_age_seconds", "gauge", "Age of the cached snapshot. Rises when the collector stalls.")
	age := 0.0
	if !s.TakenAt.IsZero() {
		age = now.Sub(s.TakenAt).Seconds()
	}
	line(&b, "praxis_snapshot_age_seconds", map[string]string{"view": v}, age)

	// --- sessions by state ---
	byState := map[string]float64{"created": 0, "running": 0, "exited": 0}
	for _, sess := range s.Sessions {
		byState[sess.State]++
	}
	help(&b, "praxis_sessions_current", "gauge", "Managed sandboxes by container state. Growth in exited means the reaper is removing slowly, not that load is high.")
	states := make([]string, 0, len(byState))
	for st := range byState {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		line(&b, "praxis_sessions_current", map[string]string{"view": v, "state": st}, byState[st])
	}

	// --- expiry windows ---
	help(&b, "praxis_sessions_expiring_within", "gauge", "Managed sandboxes whose TTL falls inside the window. Answers what capacity frees up next.")
	for _, wdw := range expiryWindows {
		n := 0.0
		for _, sess := range s.Sessions {
			rem := sess.Remaining(now)
			if rem > 0 && rem <= wdw {
				n++
			}
		}
		line(&b, "praxis_sessions_expiring_within",
			map[string]string{"view": v, "window": fmt.Sprintf("%ds", int(wdw.Seconds()))}, n)
	}

	// --- the backstop ---
	unreaped, oldest := 0.0, 0.0
	for _, sess := range s.Sessions {
		if sess.Expired(now) {
			unreaped++
			if a := now.Sub(sess.ExpiresAt).Seconds(); a > oldest {
				oldest = a
			}
		}
	}
	help(&b, "praxis_sessions_expired_unreaped", "gauge", "Past TTL and still present. TTL reaping is the backstop for the whole design; this should be zero.")
	line(&b, "praxis_sessions_expired_unreaped", map[string]string{"view": v}, unreaped)

	help(&b, "praxis_oldest_expired_age_seconds", "gauge", "Seconds since the oldest unreaped sandbox should have died.")
	line(&b, "praxis_oldest_expired_age_seconds", map[string]string{"view": v}, oldest)

	// --- orphans ---
	help(&b, "praxis_orphans", "gauge", "Containers not resolvable to a managed session. kind=unmanaged is only ever visible from view=host.")
	for _, k := range []OrphanKind{OrphanUnmanaged, OrphanUnparseable, OrphanUnreaped} {
		n := float64(s.Orphans[k])
		if k == OrphanUnreaped {
			n = unreaped
		}
		line(&b, "praxis_orphans", map[string]string{"view": v, "kind": string(k)}, n)
	}

	if s.Total > 0 || v == "host" {
		help(&b, "praxis_containers_total", "gauge", "Every container on the socket, managed or not. Diverges from sessions_current when labels are lost.")
		line(&b, "praxis_containers_total", map[string]string{"view": v}, float64(s.Total))
	}

	// --- orchestrator-only ---
	if v == "orchestrator" {
		help(&b, "praxis_capacity_weight_used", "gauge", "Summed runbook weight in flight. Admission unit; a flat count under-counts CPT-01.")
		line(&b, "praxis_capacity_weight_used", map[string]string{"view": v}, float64(r.capacityUsed))
		help(&b, "praxis_capacity_weight_limit", "gauge", "Configured weight budget.")
		line(&b, "praxis_capacity_weight_limit", map[string]string{"view": v}, float64(r.capacityLimit))

		help(&b, "praxis_spawn_total", "counter", "Spawn attempts by outcome.")
		for _, k := range sortedKeys(r.spawn) {
			line(&b, "praxis_spawn_total", map[string]string{"result": k}, float64(r.spawn[k]))
		}
		help(&b, "praxis_destroy_total", "counter", "Destroys by reason.")
		for _, k := range sortedKeys(r.destroy) {
			line(&b, "praxis_destroy_total", map[string]string{"reason": k}, float64(r.destroy[k]))
		}

		help(&b, "praxis_reaper_last_success_timestamp_seconds", "gauge", "Unix time of the last completed reaper tick. Staleness here is the page.")
		ts := 0.0
		if !r.reaperLastSuccess.IsZero() {
			ts = float64(r.reaperLastSuccess.Unix())
		}
		line(&b, "praxis_reaper_last_success_timestamp_seconds", nil, ts)

		help(&b, "praxis_reaper_duration_seconds", "gauge", "Duration of the last reaper tick.")
		line(&b, "praxis_reaper_duration_seconds", nil, r.reaperDuration.Seconds())
	}

	// --- host-only ---
	if v == "host" {
		help(&b, "praxis_slice_procs", "gauge", "Processes in the sandbox cgroup slice. Exceeding the sum of container pids means something escaped or was left behind.")
		line(&b, "praxis_slice_procs", map[string]string{"view": v}, float64(s.SliceProcs))

		help(&b, "praxis_slice_memory_bytes", "gauge", "memory.current for the whole sandbox slice.")
		line(&b, "praxis_slice_memory_bytes", map[string]string{"view": v}, float64(s.SliceMemoryBytes))

		help(&b, "praxis_slice_memory_pressure_ratio", "gauge", "PSI avg60 for the slice. Better capacity signal than utilisation.")
		line(&b, "praxis_slice_memory_pressure_ratio", map[string]string{"view": v, "kind": "some"}, s.PressureSome)
		line(&b, "praxis_slice_memory_pressure_ratio", map[string]string{"view": v, "kind": "full"}, s.PressureFull)

		help(&b, "praxis_storage_free_bytes", "gauge", "Free bytes on the container storage volume. One runaway writable layer denies every other session.")
		line(&b, "praxis_storage_free_bytes", map[string]string{"view": v}, float64(s.StorageFreeBytes))

		if len(s.SessionStats) > 0 {
			help(&b, "praxis_session_memory_bytes", "gauge", "Per-session working set. Opt-in; bounded by the concurrency limit.")
			help(&b, "praxis_session_pids", "gauge", "Per-session pid count. Approaching the 256 cap means a fork bomb.")
			for _, id := range sortedStatKeys(s.SessionStats) {
				st := s.SessionStats[id]
				l := map[string]string{"view": v, "attempt_id": st.AttemptID, "runbook": st.Runbook}
				line(&b, "praxis_session_memory_bytes", l, float64(st.MemoryBytes))
				line(&b, "praxis_session_pids", l, float64(st.Pids))
			}
		}
	}

	io.WriteString(w, b.String())
}

func help(b *strings.Builder, name, typ, text string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ)
}

func line(b *strings.Builder, name string, labels map[string]string, val float64) {
	if len(labels) == 0 {
		fmt.Fprintf(b, "%s %g\n", name, val)
		return
	}
	keys := sortedKeysStr(labels)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, escape(labels[k])))
	}
	fmt.Fprintf(b, "%s{%s} %g\n", name, strings.Join(parts, ","), val)
}

func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysStr(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStatKeys(m map[string]SessionStat) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
