// Package mockbackend is an in-memory, no-podman implementation of
// sandbox.Backend -- for a portal/dashboard developer to build against the
// real HTTP surface (api.New/Routes, identical to production) with zero
// podman/host/root requirement, on any machine, via cmd/mockorchestrator.
//
// Not a test fixture: internal/api/server_test.go's own fakeBackend already
// covers that narrower need (minimal, in-memory, used only inside that
// package's own tests) and stays untouched. This is a standalone, richer,
// runnable stand-in -- it also implements metrics.Lister by synthesizing
// labeled container listings from its own state, so the admission/weight
// logic in internal/api's create() and the reap/capacity logic
// cmd/mockorchestrator runs are the SAME real code production runs, not a
// separately-invented mock accounting system.
package mockbackend

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/docker/docker/api/types"

	"praxis-orchestrator/internal/sandbox"
)

type entry struct {
	inst      sandbox.Instance
	weight    int
	diskLimit int64
	spawnedAt time.Time
}

// Backend implements both sandbox.Backend (the HTTP API's dependency) and
// metrics.Lister (the admission-check and reap-loop dependency) on the same
// type -- structurally distinct interfaces, one in-memory map underneath.
type Backend struct {
	mu      sync.Mutex
	entries map[string]entry
}

func New() *Backend {
	return &Backend{entries: map[string]entry{}}
}

func (b *Backend) Create(ctx context.Context, attemptID string, rb sandbox.Runbook) (sandbox.Instance, error) {
	if err := sandbox.ValidateAttemptID(attemptID); err != nil {
		return sandbox.Instance{}, err
	}
	if err := rb.Validate(); err != nil {
		return sandbox.Instance{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// Idempotent on attempt_id, matching container.go's own Create
	// semantics -- a repeat call returns the first instance rather than
	// spawning a second one.
	if e, ok := b.entries[attemptID]; ok {
		return e.inst, nil
	}

	name, err := sandbox.SandboxName(attemptID)
	if err != nil {
		return sandbox.Instance{}, err
	}
	now := time.Now()
	inst := sandbox.Instance{
		AttemptID:     attemptID,
		Name:          name,
		Status:        sandbox.StatusRunning,
		ExpiresAt:     now.Add(rb.TTL()),
		RunbookDigest: rb.Digest(),
		ContainerID:   "mock-" + attemptID,
	}
	b.entries[attemptID] = entry{
		inst:      inst,
		weight:    rb.EffectiveWeight(),
		diskLimit: rb.EffectiveDiskLimitBytes(),
		spawnedAt: now,
	}
	return inst, nil
}

func (b *Backend) Get(ctx context.Context, attemptID string) (sandbox.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[attemptID]
	if !ok {
		return sandbox.Instance{}, sandbox.ErrNotFound
	}
	return e.inst, nil
}

func (b *Backend) Destroy(ctx context.Context, attemptID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.entries[attemptID]
	delete(b.entries, attemptID)
	return ok, nil
}

// Reap expires by TTL only -- the mock has no writable-layer concept to
// model a disk cap against, unlike the real reaper's second condition
// (cmd/orchestrator/main.go's reapTick).
func (b *Backend) Reap(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	var killed []string
	for id, e := range b.entries {
		if now.After(e.inst.ExpiresAt) {
			delete(b.entries, id)
			killed = append(killed, id)
		}
	}
	return killed, nil
}

// PutFile is a no-op: there is no real filesystem behind the mock, and
// nothing in this feature's integration surface depends on file contents
// actually landing anywhere.
func (b *Backend) PutFile(ctx context.Context, attemptID, path string, content []byte, mode int64) error {
	return nil
}

func (b *Backend) ExecScript(ctx context.Context, attemptID string, script []byte, timeout time.Duration) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("mock exec: received %d bytes of script, not actually run\n", len(script)),
	}, nil
}

// ExecShell hands back a real, live, in-memory PTY-shaped pipe -- not a
// canned static response -- so a WS/relay round-trip through the mock
// visibly does something (typed input produces real output), which is the
// entire point of a mock meant to prove out integration wiring rather than
// just returning fixtures. Deliberately not a PTY emulation: raw per-byte
// echo, not line discipline or ANSI handling -- good enough to prove a
// round trip, not a terminal.
func (b *Backend) ExecShell(ctx context.Context, attemptID, user string) (io.ReadWriteCloser, error) {
	server, client := net.Pipe()
	go runFakeShell(server, user)
	return client, nil
}

func (b *Backend) CountRunning(ctx context.Context) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.entries {
		if e.inst.Status == sandbox.StatusRunning {
			n++
		}
	}
	return n, nil
}

// ContainerList implements metrics.Lister by synthesizing labeled
// container listings from the same in-memory state Create()/Destroy()
// already maintain -- not a separate mock accounting system. This is what
// lets internal/api's admission check (metrics.CollectManaged +
// WeightInFlight) and cmd/mockorchestrator's reap/capacity-publishing loop
// run the exact same real code production does, just fed synthetic
// listings instead of a real docker socket's response.
//
// opts.Filters is ignored: CollectManaged's own label filter
// (label=praxis.managed-by=praxis-orchestrator) would just re-select
// everything already listed here, since every entry already carries that
// label -- there is no unmanaged/unlabeled container for the mock to ever
// produce.
func (b *Backend) ContainerList(ctx context.Context, opts types.ContainerListOptions) ([]types.Container, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]types.Container, 0, len(b.entries))
	for id, e := range b.entries {
		c := types.Container{
			ID:    e.inst.ContainerID,
			State: string(e.inst.Status),
			Labels: map[string]string{
				sandbox.LabelManaged:   sandbox.ManagedValue,
				sandbox.LabelAttempt:   id,
				sandbox.LabelRunbook:   e.inst.RunbookDigest,
				sandbox.LabelExpires:   e.inst.ExpiresAt.Format(time.RFC3339),
				sandbox.LabelSpawnedAt: e.spawnedAt.Format(time.RFC3339),
				sandbox.LabelWeight:    fmt.Sprintf("%d", e.weight),
				sandbox.LabelDiskLimit: fmt.Sprintf("%d", e.diskLimit),
			},
		}
		out = append(out, c)
	}
	return out, nil
}

var _ sandbox.Backend = (*Backend)(nil)

func runFakeShell(conn net.Conn, user string) {
	defer conn.Close()
	fmt.Fprintf(conn, "mock-shell (%s)$ ", user)
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
