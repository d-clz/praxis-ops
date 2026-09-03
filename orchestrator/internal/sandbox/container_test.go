package sandbox

import "testing"

func TestParseMemory(t *testing.T) {
	cases := map[string]int64{
		"":           0,
		"512m":       512 << 20,
		"1g":         1 << 30,
		"256k":       256 << 10,
		"64M":        64 << 20, // case-insensitive
		"not-a-size": 0,        // fails closed to 0, not a huge/undefined value
	}
	for in, want := range cases {
		if got := parseMemory(in); got != want {
			t.Errorf("parseMemory(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestNetworkMode(t *testing.T) {
	rb := Runbook{Network: "none"}
	if got := networkMode(rb, "attempt-1"); got != "none" {
		t.Errorf("networkMode(none) = %q, want %q", got, "none")
	}

	// Anything other than "none" gets a per-attempt bridge, never the
	// default one -- the default bridge can reach GitLab on the host
	// (container.go's own comment on this). Two different attempts must not
	// collide on the same network name either.
	rb.Network = "internal"
	a := networkMode(rb, "attempt-a")
	b := networkMode(rb, "attempt-b")
	if a == "bridge" || a == "default" {
		t.Errorf("networkMode(internal) = %q, must not be the default bridge", a)
	}
	if a == b {
		t.Errorf("two different attempts got the same network mode (%q) -- not per-attempt", a)
	}
}
