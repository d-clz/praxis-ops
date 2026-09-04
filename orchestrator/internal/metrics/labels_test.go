package metrics

import (
	"strconv"
	"testing"
	"time"

	"praxis-orchestrator/internal/sandbox"
)

// realLabels builds a label map the way internal/sandbox.ContainerBackend
// actually stamps one (container.go's spec()) -- not the hand-typed
// underscore keys the original labels.go assumed and got wrong. This is the
// regression test for that bug: it fails immediately if labels.go's
// constants ever stop matching internal/sandbox's again.
func realLabels(expires, spawnedAt time.Time, weight int) map[string]string {
	l := map[string]string{
		sandbox.LabelManaged: sandbox.ManagedValue,
		sandbox.LabelAttempt: "attempt-1",
		sandbox.LabelRunbook: "sha256:deadbeef",
		sandbox.LabelExpires: expires.Format(TimeFormat),
	}
	if !spawnedAt.IsZero() {
		l[sandbox.LabelSpawnedAt] = spawnedAt.Format(TimeFormat)
	}
	if weight > 0 {
		l[sandbox.LabelWeight] = strconv.Itoa(weight)
	}
	return l
}

func TestParseSession_MatchesRealLabelSchema(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	expires := now.Add(time.Hour)
	labels := realLabels(expires, now, 3)

	sess, kind, ok := ParseSession("c1", "running", labels)
	if !ok {
		t.Fatalf("ParseSession did not parse a real (hyphenated) label set -- kind=%q", kind)
	}
	if sess.AttemptID != "attempt-1" {
		t.Errorf("AttemptID = %q, want attempt-1", sess.AttemptID)
	}
	if sess.Weight != 3 {
		t.Errorf("Weight = %d, want 3", sess.Weight)
	}
	if !sess.SpawnedAt.Equal(now) {
		t.Errorf("SpawnedAt = %v, want %v", sess.SpawnedAt, now)
	}
	if !sess.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", sess.ExpiresAt, expires)
	}
}

func TestParseSession_Unmanaged(t *testing.T) {
	_, kind, ok := ParseSession("c1", "running", map[string]string{"unrelated": "1"})
	if ok {
		t.Fatal("container with no praxis labels parsed as a session")
	}
	if kind != OrphanUnmanaged {
		t.Errorf("kind = %q, want %q", kind, OrphanUnmanaged)
	}
}

func TestParseSession_UnparseableExpiry(t *testing.T) {
	labels := map[string]string{
		sandbox.LabelAttempt: "attempt-1",
		sandbox.LabelExpires: "not-a-timestamp",
	}
	_, kind, ok := ParseSession("c1", "running", labels)
	if ok {
		t.Fatal("session with an unparseable expiry should not parse")
	}
	if kind != OrphanUnparseable {
		t.Errorf("kind = %q, want %q", kind, OrphanUnparseable)
	}
}

func TestParseSession_WeightDefaultsToOne(t *testing.T) {
	now := time.Now().UTC()
	labels := map[string]string{
		sandbox.LabelAttempt: "attempt-1",
		sandbox.LabelExpires: now.Add(time.Hour).Format(TimeFormat),
	}
	sess, _, ok := ParseSession("c1", "running", labels)
	if !ok {
		t.Fatal("expected a parseable session")
	}
	if sess.Weight != 1 {
		t.Errorf("Weight with no weight label = %d, want 1", sess.Weight)
	}
}

func TestParseSession_DiskLimitFromLabel(t *testing.T) {
	now := time.Now().UTC()
	labels := map[string]string{
		sandbox.LabelAttempt:   "attempt-1",
		sandbox.LabelExpires:   now.Add(time.Hour).Format(TimeFormat),
		sandbox.LabelDiskLimit: "536870912", // 512m, as container.go stamps it
	}
	sess, _, ok := ParseSession("c1", "running", labels)
	if !ok {
		t.Fatal("expected a parseable session")
	}
	if sess.DiskLimitBytes != 536870912 {
		t.Errorf("DiskLimitBytes = %d, want 536870912", sess.DiskLimitBytes)
	}
}

// TestSession_OverDiskLimit is the reaper's actual enforcement predicate
// (cmd/orchestrator's reapTick). False on either side being 0 is
// deliberate: DiskUsedBytes is only real when the caller collected with
// Size requested (CollectManaged does; CollectAll/hostmon does not), and an
// unset DiskLimitBytes must never read as "0 bytes allowed" -- that would
// destroy every session on the very next tick.
func TestSession_OverDiskLimit(t *testing.T) {
	cases := []struct {
		name string
		sess Session
		want bool
	}{
		{"under limit", Session{DiskLimitBytes: 512 << 20, DiskUsedBytes: 100 << 20}, false},
		{"over limit", Session{DiskLimitBytes: 512 << 20, DiskUsedBytes: 600 << 20}, true},
		{"limit unset", Session{DiskUsedBytes: 600 << 20}, false},
		{"used not collected (size not requested)", Session{DiskLimitBytes: 512 << 20}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sess.OverDiskLimit(); got != c.want {
				t.Errorf("OverDiskLimit() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSession_Expired(t *testing.T) {
	now := time.Now()
	past := Session{ExpiresAt: now.Add(-time.Minute)}
	future := Session{ExpiresAt: now.Add(time.Minute)}
	if !past.Expired(now) {
		t.Error("session past its TTL should be Expired")
	}
	if future.Expired(now) {
		t.Error("session before its TTL should not be Expired")
	}
}
