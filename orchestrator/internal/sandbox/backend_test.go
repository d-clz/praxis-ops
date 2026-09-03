package sandbox

import "testing"

func TestValidateAttemptID(t *testing.T) {
	valid := []string{"a", "attempt-1", "abc123", "a-b-c", "z9-9z"}
	for _, id := range valid {
		if err := ValidateAttemptID(id); err != nil {
			t.Errorf("ValidateAttemptID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",          // empty
		"Attempt-1", // uppercase
		"attempt_1", // underscore
		"-attempt",  // leading dash
		"attempt-",  // trailing dash
		"attempt 1", // space
		"attempt/1", // path separator -- this is the one that matters:
		// SandboxName feeds straight into a container name and, eventually, a
		// filesystem path component. An unvalidated attempt_id here is a
		// traversal primitive, not just a cosmetic rejection.
	}
	for _, id := range invalid {
		if err := ValidateAttemptID(id); err == nil {
			t.Errorf("ValidateAttemptID(%q) = nil, want error", id)
		}
	}

	long := ""
	for i := 0; i < 60; i++ {
		long += "a"
	}
	if err := ValidateAttemptID(long); err == nil {
		t.Errorf("ValidateAttemptID(60 chars) = nil, want error (max is 59)")
	}
}

func TestSandboxName(t *testing.T) {
	name, err := SandboxName("attempt-1")
	if err != nil {
		t.Fatalf("SandboxName: %v", err)
	}
	if want := NamePrefix + "attempt-1"; name != want {
		t.Errorf("SandboxName(%q) = %q, want %q", "attempt-1", name, want)
	}

	if _, err := SandboxName("Not Valid"); err == nil {
		t.Error("SandboxName with an invalid attempt_id should error, not silently produce a name")
	}
}
