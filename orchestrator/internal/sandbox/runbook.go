package sandbox

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Runbook is declarative and backend-neutral. It must never carry Docker or
// Kubernetes native specs -- that is what keeps the k8s backend a new
// implementation rather than a rewrite.
//
// Every field carries both yaml and json tags, kept identical on purpose:
// this struct is unmarshaled two ways (LoadScenario's YAML, and the raw
// JSON body of POST /instances' createReq.Runbook) and a caller building
// either one should be able to use the same field names. Without an
// explicit json tag, encoding/json falls back to case-insensitive matching
// against the Go field name -- which is NOT the same as tolerating the
// underscore in "ttl_seconds" vs "TTLSeconds". A JSON body using the yaml
// convention silently left TTLSeconds and PidsLimit at their zero value
// before these tags existed, past Decode() without an error, all the way to
// Validate() reporting a confusing "ttl_seconds must be within (0,86400],
// got 0" -- confusing because it names the field that never received its
// value, not the field that was actually sent.
type Runbook struct {
	// Image MUST be digest-pinned. A tag would let the fault drift between
	// spawns and break determinism.
	Image string `yaml:"image" json:"image"`

	Systemd       bool              `yaml:"systemd" json:"systemd"`
	RootInSandbox bool              `yaml:"root_in_sandbox" json:"root_in_sandbox"`
	Network       string            `yaml:"network" json:"network"` // none | internal
	Memory        string            `yaml:"memory" json:"memory"`
	CPUs          float64           `yaml:"cpus" json:"cpus"`
	PidsLimit     int64             `yaml:"pids_limit" json:"pids_limit"`
	TTLSeconds    int               `yaml:"ttl_seconds" json:"ttl_seconds"`
	Tmpfs         map[string]string `yaml:"tmpfs" json:"tmpfs,omitempty"`
	Entrypoint    []string          `yaml:"entrypoint" json:"entrypoint,omitempty"`
	Workdir       string            `yaml:"workdir" json:"workdir"`

	// Weight is the admission unit internal/metrics sums against
	// PRAXIS_CAPACITY_WEIGHT. A flat container count under-admits a light
	// ticket and over-admits a heavy one -- CPT-01 (systemd + nginx + three
	// faults) is several times SKN-01 (files and grep). Zero/unset means "not
	// specified", not "free": container.go's spec() treats <= 0 as 1, the
	// same default ParseSession uses on the read side in internal/metrics.
	Weight int `yaml:"weight" json:"weight,omitempty"`

	// ReadOnly is false for ops sandboxes on purpose. The candidate must edit
	// configs, kill processes and write files -- that is the exercise.
	// Immutability here comes from "destroy and respawn from a pinned digest",
	// not from a frozen rootfs. Keep it true only for batch grader containers.
	ReadOnly bool `yaml:"read_only" json:"read_only"`
}

// scenarioFile is the subset of tickets/<KEY>/scenario.yaml the orchestrator
// reads. It deliberately ignores star, brief, difficulty and detect: the
// orchestrator does not know what a ticket is.
type scenarioFile struct {
	SubstrateImage string  `yaml:"substrate_image"`
	Runtime        Runbook `yaml:"runtime"`
}

func DefaultRunbook() Runbook {
	return Runbook{
		Network:       "none",
		Memory:        "512m",
		CPUs:          1.0,
		PidsLimit:     256,
		TTLSeconds:    3600,
		RootInSandbox: true,
		Workdir:       "/home/candidate",
		Tmpfs:         map[string]string{"/tmp": "size=64m"},
		Weight:        1,
	}
}

// LoadScenario parses a ticket scenario file into a Runbook. Typed unmarshal
// means a malformed runtime block fails here, with a field name, instead of
// surfacing as a runtime error at spawn.
func LoadScenario(data []byte) (Runbook, error) {
	sf := scenarioFile{Runtime: DefaultRunbook()}
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return Runbook{}, fmt.Errorf("parse scenario: %w", err)
	}
	rb := sf.Runtime
	if rb.Image == "" {
		rb.Image = sf.SubstrateImage
	}
	if err := rb.Validate(); err != nil {
		return Runbook{}, err
	}
	return rb, nil
}

func (r Runbook) Validate() error {
	if !strings.Contains(r.Image, "@sha256:") {
		return fmt.Errorf("%w: image must be digest-pinned, got %q", ErrInvalidRunbook, r.Image)
	}
	switch r.Network {
	case "none", "internal":
	default:
		return fmt.Errorf("%w: network must be none|internal, got %q", ErrInvalidRunbook, r.Network)
	}
	if r.TTLSeconds <= 0 || r.TTLSeconds > 86400 {
		return fmt.Errorf("%w: ttl_seconds must be within (0,86400], got %d", ErrInvalidRunbook, r.TTLSeconds)
	}
	if r.CPUs <= 0 || r.CPUs > 8 {
		return fmt.Errorf("%w: cpus must be within (0,8], got %v", ErrInvalidRunbook, r.CPUs)
	}
	if r.PidsLimit <= 0 {
		return fmt.Errorf("%w: pids_limit must be positive", ErrInvalidRunbook)
	}
	return nil
}

func (r Runbook) Digest() string {
	if i := strings.LastIndex(r.Image, "@"); i >= 0 {
		return r.Image[i+1:]
	}
	return r.Image
}

func (r Runbook) TTL() time.Duration {
	return time.Duration(r.TTLSeconds) * time.Second
}

// EffectiveWeight is Weight with the "unset means 1, not 0" default applied.
// The single place this logic lives: container.go's spec() (stamping
// praxis.weight) and internal/api's admission check both need it, and a
// second hand-copied "<= 0 ? 1 : Weight" would be exactly the kind of
// silent-drift bug internal/metrics/labels.go already caused once this
// session (see its own comment on why it now references sandbox's label
// constants directly instead of duplicating them).
func (r Runbook) EffectiveWeight() int {
	if r.Weight <= 0 {
		return 1
	}
	return r.Weight
}
