package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"

	"praxis-orchestrator/internal/sandbox"
)

type fakeLister struct {
	containers []types.Container
}

func (f fakeLister) ContainerList(ctx context.Context, opts types.ContainerListOptions) ([]types.Container, error) {
	// Mirrors what the real docker/podman client does: a "label" filter
	// (CollectManaged's `f.Add("label", LabelAttemptID)`, no "=value", means
	// "has this label key at all") only returns containers carrying it. Good
	// enough to exercise CollectManaged vs CollectAll without a real socket.
	wanted := opts.Filters.Get("label")
	if len(wanted) == 0 {
		return f.containers, nil
	}
	var out []types.Container
	for _, c := range f.containers {
		if _, ok := c.Labels[wanted[0]]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestCollectAll_SeesEverythingIncludingUnmanaged(t *testing.T) {
	now := time.Now().UTC()
	l := fakeLister{containers: []types.Container{
		{ID: "managed", State: "running", Labels: map[string]string{
			sandbox.LabelAttempt: "a1",
			sandbox.LabelExpires: now.Add(time.Hour).Format(TimeFormat),
		}},
		{ID: "rogue", State: "running", Labels: map[string]string{"other-tool": "1"}},
	}}

	snap := CollectAll(context.Background(), l)

	if snap.Total != 2 {
		t.Errorf("Total = %d, want 2 (CollectAll must see the unmanaged one too)", snap.Total)
	}
	if len(snap.Sessions) != 1 {
		t.Errorf("Sessions = %d, want 1", len(snap.Sessions))
	}
	if snap.Orphans[OrphanUnmanaged] != 1 {
		t.Errorf("OrphanUnmanaged = %d, want 1 -- this is the class only the host view can ever see", snap.Orphans[OrphanUnmanaged])
	}
}

func TestCollectManaged_NeverSeesUnmanaged(t *testing.T) {
	l := fakeLister{containers: []types.Container{
		{ID: "rogue", State: "running", Labels: map[string]string{"other-tool": "1"}},
	}}

	snap := CollectManaged(context.Background(), l)

	if len(snap.Sessions) != 0 || snap.Orphans[OrphanUnmanaged] != 0 {
		t.Errorf("CollectManaged saw an unmanaged container at all: sessions=%d orphans=%d -- "+
			"by construction it must be blind to these, that blind spot is why hostmon exists",
			len(snap.Sessions), snap.Orphans[OrphanUnmanaged])
	}
}
