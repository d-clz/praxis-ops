package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

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

// fakeReapBackend implements sandbox.Backend with only Reap doing anything --
// reaper() never calls the rest, and the rest exist solely to satisfy the
// interface.
type fakeReapBackend struct {
	reapCalls int32
	killed    []string
}

func (f *fakeReapBackend) Create(context.Context, string, sandbox.Runbook) (sandbox.Instance, error) {
	return sandbox.Instance{}, nil
}
func (f *fakeReapBackend) Get(context.Context, string) (sandbox.Instance, error) {
	return sandbox.Instance{}, sandbox.ErrNotFound
}
func (f *fakeReapBackend) Destroy(context.Context, string) (bool, error) { return false, nil }
func (f *fakeReapBackend) Reap(context.Context) ([]string, error) {
	atomic.AddInt32(&f.reapCalls, 1)
	return f.killed, nil
}
func (f *fakeReapBackend) PutFile(context.Context, string, string, []byte, int64) error { return nil }
func (f *fakeReapBackend) ExecScript(context.Context, string, []byte, time.Duration) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (f *fakeReapBackend) ExecShell(context.Context, string, string) (io.ReadWriteCloser, error) {
	return nil, nil
}
func (f *fakeReapBackend) CountRunning(context.Context) (int, error) { return 0, nil }

var _ sandbox.Backend = (*fakeReapBackend)(nil)

// TestReaper_TicksAndStopsOnCancel is the property that actually matters:
// this is the backstop the whole design leans on if a portal loses an
// attempt record (main.go's own comment on reaper()). It has to keep firing
// on schedule, and it has to actually stop when told to -- a reaper that
// outlives its context leaks a goroutine per restart.
func TestReaper_TicksAndStopsOnCancel(t *testing.T) {
	be := &fakeReapBackend{killed: []string{"attempt-x"}}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		reaper(ctx, be, 10*time.Millisecond, log)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if calls := atomic.LoadInt32(&be.reapCalls); calls < 2 {
		t.Errorf("reap calls after 50ms at a 10ms interval = %d, want >= 2", calls)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("reaper did not return after context cancellation -- goroutine leak")
	}
}
