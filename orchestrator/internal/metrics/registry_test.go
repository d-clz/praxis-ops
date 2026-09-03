package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestRegistry_Write_SessionsByState(t *testing.T) {
	reg := NewRegistry("orchestrator", "test")
	now := time.Now()
	reg.SetSnapshot(Snapshot{
		TakenAt: now,
		Sessions: []Session{
			{AttemptID: "a", State: "running", Weight: 1, ExpiresAt: now.Add(time.Hour)},
			{AttemptID: "b", State: "running", Weight: 1, ExpiresAt: now.Add(time.Hour)},
			{AttemptID: "c", State: "exited", Weight: 1, ExpiresAt: now.Add(time.Hour)},
		},
	})

	var b strings.Builder
	reg.Write(&b)
	out := b.String()

	if !strings.Contains(out, `praxis_sessions_current{state="running",view="orchestrator"} 2`) {
		t.Errorf("expected 2 running sessions in output, got:\n%s", out)
	}
	if !strings.Contains(out, `praxis_sessions_current{state="exited",view="orchestrator"} 1`) {
		t.Errorf("expected 1 exited session in output, got:\n%s", out)
	}
	if strings.Contains(out, `praxis_scrape_error{view="orchestrator"} 1`) {
		t.Error("scrape_error should be 0 on a successful snapshot")
	}
}

func TestRegistry_Write_ScrapeErrorSurfaces(t *testing.T) {
	reg := NewRegistry("host", "test")
	reg.SetSnapshot(Snapshot{Err: errDummy{}})

	var b strings.Builder
	reg.Write(&b)
	out := b.String()

	if !strings.Contains(out, `praxis_scrape_error{view="host"} 1`) {
		t.Errorf("expected scrape_error=1 after a failed collection, got:\n%s", out)
	}
}

type errDummy struct{}

func (errDummy) Error() string { return "collection failed" }

func TestRegistry_Write_ExpiredCountsAsUnreaped(t *testing.T) {
	reg := NewRegistry("orchestrator", "test")
	now := time.Now()
	reg.SetSnapshot(Snapshot{
		TakenAt: now,
		Sessions: []Session{
			{AttemptID: "stale", State: "running", Weight: 1, ExpiresAt: now.Add(-time.Hour)},
		},
	})

	var b strings.Builder
	reg.Write(&b)
	out := b.String()

	if !strings.Contains(out, `praxis_sessions_expired_unreaped{view="orchestrator"} 1`) {
		t.Errorf("expected 1 expired-unreaped session, got:\n%s", out)
	}
}

func TestWeightInFlight(t *testing.T) {
	snap := Snapshot{
		Sessions: []Session{
			{State: "running", Weight: 4},
			{State: "created", Weight: 2},
			{State: "exited", Weight: 100}, // must not count -- already torn down
		},
	}
	if got, want := WeightInFlight(snap), 6; got != want {
		t.Errorf("WeightInFlight = %d, want %d (exited sessions must not count)", got, want)
	}
}
