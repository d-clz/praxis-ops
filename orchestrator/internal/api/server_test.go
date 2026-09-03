package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"praxis-orchestrator/internal/sandbox"
)

// fakeBackend implements sandbox.Backend entirely in memory. No podman, no
// docker socket -- that's the point: the API layer's job (auth, status
// checks, routing, and the hijack relay) is independent of the runtime
// underneath it, and should be testable without one.
type fakeBackend struct {
	instances map[string]sandbox.Instance
	shellConn io.ReadWriteCloser
	shellErr  error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{instances: map[string]sandbox.Instance{}}
}

func (f *fakeBackend) Create(ctx context.Context, attemptID string, rb sandbox.Runbook) (sandbox.Instance, error) {
	inst := sandbox.Instance{AttemptID: attemptID, Status: sandbox.StatusRunning}
	f.instances[attemptID] = inst
	return inst, nil
}
func (f *fakeBackend) Get(ctx context.Context, attemptID string) (sandbox.Instance, error) {
	inst, ok := f.instances[attemptID]
	if !ok {
		return sandbox.Instance{}, sandbox.ErrNotFound
	}
	return inst, nil
}
func (f *fakeBackend) Destroy(ctx context.Context, attemptID string) (bool, error) {
	_, ok := f.instances[attemptID]
	delete(f.instances, attemptID)
	return ok, nil
}
func (f *fakeBackend) Reap(ctx context.Context) ([]string, error) { return nil, nil }
func (f *fakeBackend) PutFile(ctx context.Context, attemptID, path string, content []byte, mode int64) error {
	return nil
}
func (f *fakeBackend) ExecScript(ctx context.Context, attemptID string, script []byte, timeout time.Duration) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (f *fakeBackend) ExecShell(ctx context.Context, attemptID, user string) (io.ReadWriteCloser, error) {
	if f.shellErr != nil {
		return nil, f.shellErr
	}
	return f.shellConn, nil
}
func (f *fakeBackend) CountRunning(ctx context.Context) (int, error) { return len(f.instances), nil }

var _ sandbox.Backend = (*fakeBackend)(nil)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAuth_RejectsMissingOrWrongToken(t *testing.T) {
	be := newFakeBackend()
	be.instances["a"] = sandbox.Instance{AttemptID: "a", Status: sandbox.StatusRunning}
	ts := httptest.NewServer(New(be, Config{Token: "secret"}, testLogger()).Routes())
	defer ts.Close()

	for _, tok := range []string{"", "wrong"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/instances/a", nil)
		if tok != "" {
			req.Header.Set("X-Praxis-Token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", tok, resp.StatusCode)
		}
	}
}

func TestAuth_AllowsCorrectToken(t *testing.T) {
	be := newFakeBackend()
	be.instances["a"] = sandbox.Instance{AttemptID: "a", Status: sandbox.StatusRunning}
	ts := httptest.NewServer(New(be, Config{Token: "secret"}, testLogger()).Routes())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/instances/a", nil)
	req.Header.Set("X-Praxis-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestShell_ConflictWhenNotRunning(t *testing.T) {
	be := newFakeBackend() // no instance registered at all
	ts := httptest.NewServer(New(be, Config{Token: "secret"}, testLogger()).Routes())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/instances/nope/shell", nil)
	req.Header.Set("X-Praxis-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 (no exec, no hijack, should have happened for a non-running instance)", resp.StatusCode)
	}
}

// TestShell_RelaysBytesBothWays is the one test that actually exercises the
// hijack + relayBytes path from Phase C -- untestable any other way without
// a live podman daemon. net.Pipe() stands in for the container's PTY; a raw
// TCP dial into httptest's real listener stands in for the portal, because
// httptest.ResponseRecorder does not implement http.Hijacker and this
// handler requires one.
func TestShell_RelaysBytesBothWays(t *testing.T) {
	be := newFakeBackend()
	be.instances["attempt-1"] = sandbox.Instance{AttemptID: "attempt-1", Status: sandbox.StatusRunning}
	sandboxSide, testSide := net.Pipe()
	be.shellConn = sandboxSide

	ts := httptest.NewServer(New(be, Config{Token: "secret"}, testLogger()).Routes())
	defer ts.Close()

	conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := "POST /instances/attempt-1/shell HTTP/1.1\r\n" +
		"Host: praxis\r\n" +
		"X-Praxis-Token: secret\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" {
			break // end of headers -- everything after this is opaque relay traffic
		}
	}

	// portal -> sandbox
	const toSandbox = "hello sandbox"
	if _, err := conn.Write([]byte(toSandbox)); err != nil {
		t.Fatalf("write toward sandbox: %v", err)
	}
	got := make([]byte, len(toSandbox))
	if _, err := io.ReadFull(testSide, got); err != nil {
		t.Fatalf("read on sandbox side: %v", err)
	}
	if string(got) != toSandbox {
		t.Errorf("sandbox side received %q, want %q", got, toSandbox)
	}

	// sandbox -> portal
	const toPortal = "hello portal"
	if _, err := testSide.Write([]byte(toPortal)); err != nil {
		t.Fatalf("write toward portal: %v", err)
	}
	got2 := make([]byte, len(toPortal))
	if _, err := io.ReadFull(br, got2); err != nil {
		t.Fatalf("read on portal side: %v", err)
	}
	if string(got2) != toPortal {
		t.Errorf("portal side received %q, want %q", got2, toPortal)
	}
}
