package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Lister is the slice of the docker/podman client this package needs. Keeping
// it an interface means the reaper can pass its own client and tests need no
// socket.
//
// NOTE: v25 client surface. On v26+ this becomes
//   ContainerList(ctx, container.ListOptions) ([]container.Summary, error)
type Lister interface {
	ContainerList(ctx context.Context, opts types.ContainerListOptions) ([]types.Container, error)
}

var _ Lister = (*client.Client)(nil)

// CollectManaged builds the orchestrator view: the label-filtered list, which
// is exactly what the reaper can act on. By construction it cannot see
// unmanaged containers — that blind spot is why cmd/hostmon exists.
//
// Call this from inside the reaper tick and hand the result to
// Registry.SetSnapshot, so one list call serves both reaping and metrics and a
// scrape costs nothing on the socket.
func CollectManaged(ctx context.Context, l Lister) Snapshot {
	f := filters.NewArgs()
	f.Add("label", LabelAttemptID)

	return collect(ctx, l, f, true)
}

// CollectAll builds the host view: every container on the socket, no filter.
// This is ground truth. On a socket owned solely by praxis-sbx, anything here
// without praxis labels was created outside the orchestrator and will never be
// reaped.
func CollectAll(ctx context.Context, l Lister) Snapshot {
	return collect(ctx, l, filters.NewArgs(), false)
}

func collect(ctx context.Context, l Lister, f filters.Args, managedOnly bool) Snapshot {
	snap := Snapshot{
		TakenAt: time.Now(),
		Orphans: map[OrphanKind]int{},
	}

	containers, err := l.ContainerList(ctx, types.ContainerListOptions{
		All:     true, // exited-but-present is a state we report on, not a state we ignore
		Filters: f,
	})
	if err != nil {
		snap.Err = err
		return snap
	}

	snap.Total = len(containers)

	for _, c := range containers {
		sess, kind, ok := ParseSession(c.ID, c.State, c.Labels)
		if !ok {
			// In the managed view an unparseable container still counts as an
			// orphan — the filter matched the label but the value was garbage.
			snap.Orphans[kind]++
			continue
		}
		snap.Sessions = append(snap.Sessions, sess)
	}

	return snap
}

// WeightInFlight sums runbook weights for admission control. Sessions past
// their TTL still hold real memory, so they are counted until actually removed.
func WeightInFlight(s Snapshot) int {
	total := 0
	for _, sess := range s.Sessions {
		if sess.State == "exited" {
			continue
		}
		total += sess.Weight
	}
	return total
}

// Handler serves the exposition format. Mount on the loopback listener only —
// session counts and runbook names are not public.
func Handler(r *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// SessionsHandler serves per-session detail as JSON. This is where attempt_id
// belongs — putting it in a metric label would make cardinality unbounded over
// the lifetime of the process.
func SessionsHandler(r *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.RLock()
		snap := r.snap
		r.mu.RUnlock()

		now := time.Now()
		type row struct {
			AttemptID   string `json:"attempt_id"`
			ContainerID string `json:"container_id"`
			Runbook     string `json:"runbook"`
			State       string `json:"state"`
			Weight      int    `json:"weight"`
			SpawnedAt   string `json:"spawned_at,omitempty"`
			ExpiresAt   string `json:"expires_at"`
			RemainingS  int64  `json:"remaining_seconds"`
			Expired     bool   `json:"expired"`
		}
		out := make([]row, 0, len(snap.Sessions))
		for _, s := range snap.Sessions {
			r := row{
				AttemptID:   s.AttemptID,
				ContainerID: s.ContainerID,
				Runbook:     s.Runbook,
				State:       s.State,
				Weight:      s.Weight,
				ExpiresAt:   s.ExpiresAt.Format(TimeFormat),
				RemainingS:  int64(s.Remaining(now).Seconds()),
				Expired:     s.Expired(now),
			}
			if !s.SpawnedAt.IsZero() {
				r.SpawnedAt = s.SpawnedAt.Format(TimeFormat)
			}
			out = append(out, r)
		}
		writeJSON(w, map[string]any{
			"view":         snap.View,
			"taken_at":     snap.TakenAt.Format(TimeFormat),
			"total_seen":   snap.Total,
			"sessions":     out,
			"orphan_counts": snap.Orphans,
		})
	})
}
