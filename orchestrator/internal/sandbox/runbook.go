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
type Runbook struct {
	// Image MUST be digest-pinned. A tag would let the fault drift between
	// spawns and break determinism.
	Image string `yaml:"image"`

	Systemd       bool              `yaml:"systemd"`
	RootInSandbox bool              `yaml:"root_in_sandbox"`
	Network       string            `yaml:"network"` // none | internal
	Memory        string            `yaml:"memory"`
	CPUs          float64           `yaml:"cpus"`
	PidsLimit     int64             `yaml:"pids_limit"`
	TTLSeconds    int               `yaml:"ttl_seconds"`
	Tmpfs         map[string]string `yaml:"tmpfs"`
	Entrypoint    []string          `yaml:"entrypoint"`
	Workdir       string            `yaml:"workdir"`

	// Weight is the admission unit internal/metrics sums against
	// PRAXIS_CAPACITY_WEIGHT. A flat container count under-admits a light
	// ticket and over-admits a heavy one -- CPT-01 (systemd + nginx + three
	// faults) is several times SKN-01 (files and grep). Zero/unset means "not
	// specified", not "free": container.go's spec() treats <= 0 as 1, the
	// same default ParseSession uses on the read side in internal/metrics.
	Weight int `yaml:"weight"`

	// ReadOnly is false for ops sandboxes on purpose. The candidate must edit
	// configs, kill processes and write files -- that is the exercise.
	// Immutability here comes from "destroy and respawn from a pinned digest",
	// not from a frozen rootfs. Keep it true only for batch grader containers.
	ReadOnly bool `yaml:"read_only"`
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
