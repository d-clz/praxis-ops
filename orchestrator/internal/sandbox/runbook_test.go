package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// This is Phase D item 2 (docs/session-02-plan.md), written down as a test
// instead of just prose: a locally-built image has no digest to pin, and
// Validate() rejects it today. Whichever way that conflict gets resolved,
// this test should change on purpose, not by accident -- if it starts
// failing without anyone touching Validate(), that's the digest-pinning
// decision finally landing, not a regression.
func TestRunbookValidate_RequiresDigestPin(t *testing.T) {
	rb := DefaultRunbook()
	rb.Image = "praxis/ops-base:latest" // tag, not a digest
	if err := rb.Validate(); !errors.Is(err, ErrInvalidRunbook) {
		t.Errorf("Validate() with a tagged, non-digest image = %v, want ErrInvalidRunbook", err)
	}

	rb.Image = "praxis/ops-base@sha256:" + strings.Repeat("a", 64)
	if err := rb.Validate(); err != nil {
		t.Errorf("Validate() with a digest-pinned image = %v, want nil", err)
	}
}

func TestRunbookValidate_Bounds(t *testing.T) {
	base := func() Runbook {
		rb := DefaultRunbook()
		rb.Image = "praxis/ops-base@sha256:" + strings.Repeat("a", 64)
		return rb
	}

	cases := []struct {
		name string
		mod  func(*Runbook)
	}{
		{"bad network", func(r *Runbook) { r.Network = "bridge" }},
		{"zero ttl", func(r *Runbook) { r.TTLSeconds = 0 }},
		{"ttl over 24h", func(r *Runbook) { r.TTLSeconds = 86401 }},
		{"zero cpus", func(r *Runbook) { r.CPUs = 0 }},
		{"cpus over 8", func(r *Runbook) { r.CPUs = 8.1 }},
		{"zero pids limit", func(r *Runbook) { r.PidsLimit = 0 }},
	}
	for _, c := range cases {
		rb := base()
		c.mod(&rb)
		if err := rb.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", c.name)
		}
	}
}

func TestRunbookDigest(t *testing.T) {
	rb := Runbook{Image: "praxis/ops-base@sha256:deadbeef"}
	if got, want := rb.Digest(), "sha256:deadbeef"; got != want {
		t.Errorf("Digest() = %q, want %q", got, want)
	}
}

func TestLoadScenario(t *testing.T) {
	yaml := `
substrate_image: praxis/ops-base@sha256:` + strings.Repeat("b", 64) + `
runtime:
  network: none
  memory: 512m
  cpus: 1.0
  pids_limit: 128
  ttl_seconds: 1800
`
	rb, err := LoadScenario([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if rb.Image == "" {
		t.Error("LoadScenario did not fall back to substrate_image for Image")
	}
	if rb.PidsLimit != 128 {
		t.Errorf("PidsLimit = %d, want 128", rb.PidsLimit)
	}
}

func TestLoadScenario_MissingDigestFailsClosed(t *testing.T) {
	yaml := `substrate_image: praxis/ops-base` // no digest at all
	if _, err := LoadScenario([]byte(yaml)); err == nil {
		t.Error("LoadScenario with no digest should fail Validate(), not load silently")
	}
}
