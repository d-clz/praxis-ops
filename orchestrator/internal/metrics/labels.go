package metrics

import (
	"strconv"
	"time"

	"praxis-orchestrator/internal/sandbox"
)

// Label keys. These used to be a second, hand-copied set of string literals
// here -- and they were wrong (underscores, where internal/sandbox actually
// stamps hyphens: praxis.attempt-id, not praxis.attempt_id). A mismatch here
// turns every managed container into a reported orphan, silently, since
// nothing fails loudly when a label key just doesn't match. Referencing
// internal/sandbox's own constants instead of copying their values makes
// that class of bug impossible to reintroduce: there is exactly one place
// these are defined now.
const (
	LabelAttemptID = sandbox.LabelAttempt
	LabelRunbook   = sandbox.LabelRunbook
	LabelExpiresAt = sandbox.LabelExpires
	LabelSpawnedAt = sandbox.LabelSpawnedAt
	LabelWeight    = sandbox.LabelWeight
	LabelDiskLimit = sandbox.LabelDiskLimit
)

// TimeFormat is the serialisation assumed for the two timestamp labels.
const TimeFormat = time.RFC3339

// Session is a container as understood through its labels. A container that
// fails to parse is not a Session — it is an orphan, and that distinction is
// the entire point of the two-view model.
type Session struct {
	ContainerID string
	AttemptID   string
	Runbook     string
	State       string // created | running | exited | ...
	SpawnedAt   time.Time
	ExpiresAt   time.Time
	Weight      int

	// DiskLimitBytes and DiskUsedBytes are only meaningful when the caller
	// requested container.ListOptions.Size -- CollectManaged does, CollectAll
	// (hostmon) does not, since hostmon enforces nothing and the size walk
	// costs real time per container. DiskUsedBytes is 0, not "confirmed
	// empty", when size wasn't requested -- callers reading this from a
	// CollectAll snapshot should not treat 0 as a real measurement.
	DiskLimitBytes int64
	DiskUsedBytes  int64
}

// OverDiskLimit reports whether a measured writable-layer size has exceeded
// this session's stamped cap. False whenever either side is unknown (0) --
// callers must have collected with Size requested for this to mean anything.
func (s Session) OverDiskLimit() bool {
	return s.DiskLimitBytes > 0 && s.DiskUsedBytes > s.DiskLimitBytes
}

// Expired reports whether the TTL has passed. The reaper's own clock is the
// only authority; there is no stored "expired" flag to disagree with.
func (s Session) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

// Remaining is negative for expired sessions.
func (s Session) Remaining(now time.Time) time.Duration {
	return s.ExpiresAt.Sub(now)
}

// OrphanKind classifies a container that could not be resolved to a Session.
type OrphanKind string

const (
	// OrphanUnmanaged: no praxis labels at all. Invisible to the label-filtered
	// list the reaper makes, so it will never be reaped. Only the host view can
	// see this class.
	OrphanUnmanaged OrphanKind = "unmanaged"

	// OrphanUnparseable: praxis labels present but malformed — bad timestamp,
	// missing attempt_id. The reaper may see it and refuse to act on it.
	OrphanUnparseable OrphanKind = "unparseable_label"

	// OrphanUnreaped: parses fine, past its TTL, still present. The backstop
	// has failed.
	OrphanUnreaped OrphanKind = "unreaped"
)

// ParseSession converts a container's label map into a Session.
//
// Returns (session, "", true) when the container is a well-formed managed
// sandbox; (zero, kind, false) otherwise. The caller decides what to do with
// the orphan kind — the orchestrator logs it, hostmon counts it.
func ParseSession(id, state string, labels map[string]string) (Session, OrphanKind, bool) {
	attempt, ok := labels[LabelAttemptID]
	if !ok || attempt == "" {
		// No praxis identity whatsoever. On a dedicated socket owned by
		// praxis-sbx, anything without this label had no business being created.
		if !hasAnyPraxisLabel(labels) {
			return Session{}, OrphanUnmanaged, false
		}
		return Session{}, OrphanUnparseable, false
	}

	s := Session{
		ContainerID: id,
		AttemptID:   attempt,
		Runbook:     labels[LabelRunbook],
		State:       state,
		Weight:      1,
	}

	exp, err := time.Parse(TimeFormat, labels[LabelExpiresAt])
	if err != nil {
		// A sandbox with no readable TTL cannot be reaped on schedule. Treat it
		// as broken rather than immortal.
		return Session{}, OrphanUnparseable, false
	}
	s.ExpiresAt = exp

	if raw, ok := labels[LabelSpawnedAt]; ok {
		if t, err := time.Parse(TimeFormat, raw); err == nil {
			s.SpawnedAt = t
		}
	}

	if raw, ok := labels[LabelWeight]; ok {
		if w, err := strconv.Atoi(raw); err == nil && w > 0 {
			s.Weight = w
		}
	}

	if raw, ok := labels[LabelDiskLimit]; ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			s.DiskLimitBytes = n
		}
	}

	// DiskUsedBytes is NOT set here -- it comes from the list call's per-item
	// SizeRw, not from a label, and only when the caller requested it (see
	// collect()). The caller fills it in after ParseSession succeeds.

	return s, "", true
}

func hasAnyPraxisLabel(labels map[string]string) bool {
	for k := range labels {
		if len(k) >= 7 && k[:7] == "praxis." {
			return true
		}
	}
	return false
}
