package mockbackend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"

	"praxis-orchestrator/internal/metrics"
	"praxis-orchestrator/internal/sandbox"
)

func testRunbook(ttl int, weight int) sandbox.Runbook {
	rb := sandbox.DefaultRunbook()
	rb.Image = "praxis/x@sha256:" + strings.Repeat("a", 64)
	rb.TTLSeconds = ttl
	rb.Weight = weight
	return rb
}

func TestCreate_IdempotentOnAttemptID(t *testing.T) {
	b := New()
	ctx := context.Background()
	first, err := b.Create(ctx, "a1", testRunbook(60, 1))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := b.Create(ctx, "a1", testRunbook(60, 1))
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first.ContainerID != second.ContainerID {
		t.Errorf("repeat Create produced a different instance -- want the same one returned, container.go's own idempotency semantics")
	}
}

func TestCreate_RejectsInvalidRunbook(t *testing.T) {
	b := New()
	rb := testRunbook(60, 1)
	rb.Image = "no-digest-pin" // Validate() requires "@sha256:"
	if _, err := b.Create(context.Background(), "a1", rb); err == nil {
		t.Error("Create with an unpinned image should fail Validate(), not succeed silently")
	}
}

func TestGet_NotFound(t *testing.T) {
	b := New()
	if _, err := b.Get(context.Background(), "nope"); err != sandbox.ErrNotFound {
		t.Errorf("Get on a missing attempt_id = %v, want ErrNotFound", err)
	}
}

func TestDestroy_ReportsWhetherSomethingExisted(t *testing.T) {
	b := New()
	ctx := context.Background()
	if _, err := b.Create(ctx, "a1", testRunbook(60, 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ok, err := b.Destroy(ctx, "a1")
	if err != nil || !ok {
		t.Fatalf("Destroy(existing) = %v, %v, want true, nil", ok, err)
	}
	ok, err = b.Destroy(ctx, "a1")
	if err != nil || ok {
		t.Fatalf("Destroy(already gone) = %v, %v, want false, nil", ok, err)
	}
}

func TestReap_ExpiresByTTLOnly(t *testing.T) {
	b := New()
	ctx := context.Background()
	if _, err := b.Create(ctx, "expires-fast", testRunbook(1, 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := b.Create(ctx, "lives-on", testRunbook(3600, 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	killed, err := b.Reap(ctx)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(killed) != 1 || killed[0] != "expires-fast" {
		t.Fatalf("Reap killed %v, want exactly [expires-fast]", killed)
	}
	if _, err := b.Get(ctx, "lives-on"); err != nil {
		t.Errorf("Reap should not have touched the long-TTL instance: %v", err)
	}
}

// TestContainerList_ProducesRealParseableSessions is the regression test
// for exactly the bug found running this for real (cmd/mockorchestrator
// initially passed the wrong view name to metrics.NewRegistry, which
// silently zeroed the capacity metrics -- caught only by actually curling
// /metrics, not by this test, since that bug lived in main.go's wiring,
// not here). This test instead locks in the half that DOES live in this
// package: that ContainerList's synthesized labels are real enough for
// metrics.ParseSession to accept them, matching container.go's real label
// stamping exactly -- the same class of drift internal/metrics/labels.go's
// own comment warns about (a hyphen/underscore mismatch once silently
// turned every managed container into a reported orphan).
func TestContainerList_ProducesRealParseableSessions(t *testing.T) {
	b := New()
	ctx := context.Background()
	if _, err := b.Create(ctx, "a1", testRunbook(3600, 5)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	containers, err := b.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("ContainerList returned %d entries, want 1", len(containers))
	}
	c := containers[0]

	sess, kind, ok := metrics.ParseSession(c.ID, c.State, c.Labels)
	if !ok {
		t.Fatalf("ParseSession rejected a mockbackend-synthesized container as kind=%q -- label drift from what container.go actually stamps", kind)
	}
	if sess.AttemptID != "a1" {
		t.Errorf("AttemptID = %q, want a1", sess.AttemptID)
	}
	if sess.Weight != 5 {
		t.Errorf("Weight = %d, want 5", sess.Weight)
	}

	// Also confirm CollectManaged (the actual code path both
	// cmd/orchestrator and cmd/mockorchestrator run) accepts it end to
	// end, not just the lower-level ParseSession call above.
	snap := metrics.CollectManaged(ctx, b)
	if snap.Err != nil {
		t.Fatalf("CollectManaged: %v", snap.Err)
	}
	if len(snap.Sessions) != 1 {
		t.Fatalf("CollectManaged saw %d sessions, want 1 (orphans: %v)", len(snap.Sessions), snap.Orphans)
	}
	if got := metrics.WeightInFlight(snap); got != 5 {
		t.Errorf("WeightInFlight = %d, want 5", got)
	}
}

func TestExecShell_EchoesInput(t *testing.T) {
	b := New()
	stream, err := b.ExecShell(context.Background(), "a1", "root")
	if err != nil {
		t.Fatalf("ExecShell: %v", err)
	}
	defer stream.Close()

	buf := make([]byte, 256)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "root") {
		t.Errorf("prompt = %q, want it to mention the requested user", buf[:n])
	}

	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	n, err = stream.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("echoed %q, want %q", buf[:n], "hello")
	}
}
