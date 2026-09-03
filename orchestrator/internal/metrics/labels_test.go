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
