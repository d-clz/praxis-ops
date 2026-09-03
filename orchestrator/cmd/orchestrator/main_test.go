package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types"

	"praxis-orchestrator/internal/metrics"
	"praxis-orchestrator/internal/sandbox"
)

func TestEnvOr(t *testing.T) {
	if got := envOr("PRAXIS_TEST_UNSET_STR", "default"); got != "default" {
		t.Errorf("envOr unset = %q, want %q", got, "default")
	}
	t.Setenv("PRAXIS_TEST_STR", "set")
	if got := envOr("PRAXIS_TEST_STR", "default"); got != "set" {
		t.Errorf("envOr set = %q, want %q", got, "set")
	}
}

func TestEnvInt(t *testing.T) {
	if got := envInt("PRAXIS_TEST_UNSET_INT", 42); got != 42 {
		t.Errorf("envInt unset = %d, want 42", got)
	}
	t.Setenv("PRAXIS_TEST_INT", "7")
	if got := envInt("PRAXIS_TEST_INT", 42); got != 7 {
		t.Errorf("envInt set = %d, want 7", got)
	}
	// A malformed value fails closed to the caller's default, not to zero and
	// not by crashing -- an operator typo in PRAXIS_MAX_CONCURRENT or
	// PRAXIS_EXEC_TIMEOUT should not silently zero out a concurrency limit or
	// a timeout.
	t.Setenv("PRAXIS_TEST_INT_BAD", "not-a-number")
	if got := envInt("PRAXIS_TEST_INT_BAD", 42); got != 42 {
		t.Errorf("envInt malformed = %d, want fallback to default 42", got)
	}
}

// fakeReapBackend implements sandbox.Backend. Destroy is the only method the
// rewritten reaper calls -- it no longer calls Reap() at all (see main.go's
// comment on reaper()); the rest exist solely to satisfy the interface.
type fakeReapBackend struct {
	destroyCalls int32
	destroyed    []string
	mu           sync.Mutex
}

func (f *fakeReapBackend) Create(context.Context, string, sandbox.Runbook) (sandbox.Instance, error) {
	return sandbox.Instance{}, nil
}
func (f *fakeReapBackend) Get(context.Context, string) (sandbox.Instance, error) {
	return sandbox.Instance{}, sandbox.ErrNotFound
}
func (f *fakeReapBackend) Destroy(_ context.Context, attemptID string) (bool, error) {
	atomic.AddInt32(&f.destroyCalls, 1)
	f.mu.Lock()
	f.destroyed = append(f.destroyed, attemptID)
	f.mu.Unlock()
	return true, nil
}
func (f *fakeReapBackend) Reap(context.Context) ([]string, error)                       { return nil, nil }
func (f *fakeReapBackend) PutFile(context.Context, string, string, []byte, int64) error { return nil }
func (f *fakeReapBackend) ExecScript(context.Context, string, []byte, time.Duration) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (f *fakeReapBackend) ExecShell(context.Context, string, string) (io.ReadWriteCloser, error) {
	return nil, nil
}
func (f *fakeReapBackend) CountRunning(context.Context) (int, error) { return 0, nil }

var _ sandbox.Backend = (*fakeReapBackend)(nil)

// fakeLister stands in for the docker client reapTick lists through
// (metrics.CollectManaged). One container, already expired, with the real
// hyphenated labels internal/sandbox actually stamps -- this is what makes
// the test prove reapTick's own expiry decision, not just that it ticks.
type fakeLister struct {
	listCalls int32
}

func (f *fakeLister) ContainerList(context.Context, types.ContainerListOptions) ([]types.Container, error) {
	atomic.AddInt32(&f.listCalls, 1)
	expired := time.Now().Add(-time.Minute).Format(metrics.TimeFormat)
	return []types.Container{
		{
			ID:    "c1",
			State: "running",
			Labels: map[string]string{
				sandbox.LabelAttempt: "attempt-x",
				sandbox.LabelExpires: expired,
			},
		},
	}, nil
}

var _ metrics.Lister = (*fakeLister)(nil)

// TestReaper_TicksAndStopsOnCancel is the property that actually matters:
// this is the backstop the whole design leans on if a portal loses an
// attempt record (main.go's own comment on reaper()). It has to keep firing
// on schedule, actually destroy what it finds expired, and stop when told to
// -- a reaper that outlives its context leaks a goroutine per restart.
func TestReaper_TicksAndStopsOnCancel(t *testing.T) {
	be := &fakeReapBackend{}
	lister := &fakeLister{}
	reg := metrics.NewRegistry("orchestrator", "test")
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		reaper(ctx, be, lister, 10*time.Millisecond, reg, 2, log)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if calls := atomic.LoadInt32(&lister.listCalls); calls < 2 {
		t.Errorf("list calls after 50ms at a 10ms interval = %d, want >= 2", calls)
	}
	if calls := atomic.LoadInt32(&be.destroyCalls); calls == 0 {
		t.Error("the expired session was never destroyed")
	}
	be.mu.Lock()
	destroyed := append([]string(nil), be.destroyed...)
	be.mu.Unlock()
	if len(destroyed) == 0 || destroyed[0] != "attempt-x" {
		t.Errorf("destroyed = %v, want [attempt-x, ...]", destroyed)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("reaper did not return after context cancellation -- goroutine leak")
	}
}
