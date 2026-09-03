package api

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"praxis-orchestrator/internal/sandbox"
)

type Config struct {
	Token         string
	MaxConcurrent int
	ExecTimeout   time.Duration
}

type Server struct {
	backend sandbox.Backend
	cfg     Config
	log     *slog.Logger
}

func New(b sandbox.Backend, cfg Config, log *slog.Logger) *Server {
	return &Server{backend: b, cfg: cfg, log: log}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("POST /instances", s.auth(http.HandlerFunc(s.create)))
	mux.Handle("GET /instances/{attemptID}", s.auth(http.HandlerFunc(s.get)))
	mux.Handle("DELETE /instances/{attemptID}", s.auth(http.HandlerFunc(s.destroy)))
	mux.Handle("POST /instances/{attemptID}/exec", s.auth(http.HandlerFunc(s.exec)))
	mux.Handle("POST /instances/{attemptID}/shell", s.auth(http.HandlerFunc(s.shell)))
	mux.Handle("POST /reap", s.auth(http.HandlerFunc(s.reap)))
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Praxis-Token")
		if s.cfg.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "bad token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type createReq struct {
	AttemptID string          `json:"attempt_id"`
	Runbook   sandbox.Runbook `json:"runbook"`
}

type execReq struct {
	ScriptB64 string `json:"script_b64"`
	TimeoutS  int    `json:"timeout_seconds"`
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	n, err := s.backend.CountRunning(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": n})
}

// create treats attempt_id as the idempotency key. A repeat call returns the
// first instance rather than spawning a second one -- that is what makes a
// portal timeout or retry safe.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}

	if _, err := s.backend.Get(r.Context(), req.AttemptID); errors.Is(err, sandbox.ErrNotFound) {
		// Only a genuinely new instance consumes a slot. On one box the budget
		// guard is load bearing, not defensive: a leaked instance is a
		// meaningful fraction of total capacity.
		n, cErr := s.backend.CountRunning(r.Context())
		if cErr != nil {
			writeErr(w, http.StatusServiceUnavailable, cErr.Error())
			return
		}
		if n >= s.cfg.MaxConcurrent {
			writeErr(w, http.StatusTooManyRequests, "at capacity")
			return
		}
	}

	inst, err := s.backend.Create(r.Context(), req.AttemptID, req.Runbook)
	if err != nil {
		switch {
		case errors.Is(err, sandbox.ErrInvalidRunbook), errors.Is(err, sandbox.ErrInvalidAttemptID):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			s.log.Error("create failed", "attempt_id", req.AttemptID, "err", err)
			writeErr(w, http.StatusInternalServerError, "spawn failed")
		}
		return
	}
	writeJSON(w, http.StatusAccepted, inst)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	inst, err := s.backend.Get(r.Context(), r.PathValue("attemptID"))
	if errors.Is(err, sandbox.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such instance")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attemptID")
	ok, err := s.backend.Destroy(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempt_id": id, "destroyed": ok})
}

// exec runs an arbitrary script as root and reports exit code plus output.
// Deliberately dumb: the orchestrator does not know this is a check script,
// does not parse its JSON, and does not decide pass or fail. The portal does
// all three. That is the boundary holding.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attemptID")
	inst, err := s.backend.Get(r.Context(), id)
	if err != nil || inst.Status != sandbox.StatusRunning {
		writeErr(w, http.StatusConflict, "instance not running")
		return
	}
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	script, err := base64.StdEncoding.DecodeString(req.ScriptB64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "script_b64 not valid base64")
		return
	}
	timeout := s.cfg.ExecTimeout
	if req.TimeoutS > 0 {
		timeout = time.Duration(req.TimeoutS) * time.Second
	}
	res, err := s.backend.ExecScript(r.Context(), id, script, timeout)
	if err != nil {
		s.log.Error("exec failed", "attempt_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "exec failed")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// shell attaches an interactive PTY session for a candidate-equivalent user
// and relays raw bytes between the portal's connection and the container's
// terminal until either side closes. Deliberately dumb, same principle as
// exec: no framing, no terminal emulation, no parsing of what crosses it.
// That is what keeps this a relay and not a place for a vulnerability to
// hide between an untrusted process and the portal (docs/session-02-plan.md
// Phase C, Option 1 -- podman exec proxied, never a listener in the sandbox).
//
// Authorization here is the same shared X-Praxis-Token every other endpoint
// uses -- it proves the caller IS the portal, not which candidate it's
// acting for. Mapping a candidate's own session to only their own attempt_id
// is the portal's job; this endpoint trusts whatever attempt_id it's given,
// exactly like exec and destroy already do. There is no portal yet to prove
// that half end-to-end -- see security/verify-shell-isolation.sh for what
// can be proven without one: exec-by-name is exact and exclusive, so a
// caller that only ever learns one attempt_id can only ever reach one
// container.
func (s *Server) shell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attemptID")
	inst, err := s.backend.Get(r.Context(), id)
	if err != nil || inst.Status != sandbox.StatusRunning {
		writeErr(w, http.StatusConflict, "instance not running")
		return
	}

	user := r.URL.Query().Get("user")
	if user == "" {
		user = "candidate"
	}

	stream, err := s.backend.ExecShell(r.Context(), id, user)
	if err != nil {
		s.log.Error("shell exec failed", "attempt_id", id, "user", user, "err", err)
		writeErr(w, http.StatusInternalServerError, "exec failed")
		return
	}
	defer stream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "hijack unsupported")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		s.log.Error("hijack failed", "attempt_id", id, "err", err)
		return
	}
	defer conn.Close()

	// Last point this connection speaks HTTP. Every byte after this line is
	// opaque terminal traffic between the portal and the sandbox.
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: praxis-shell\r\nConnection: Upgrade\r\n\r\n")
	_ = buf.Flush()

	relayBytes(&hijackedPortal{rw: buf, conn: conn}, stream)
}

// hijackedPortal reads through the hijacked connection's own buffered
// reader -- it may already hold bytes the HTTP server read ahead of the
// handoff, and reading conn directly instead would drop them -- and flushes
// every write immediately, since an interactive terminal cannot wait for a
// buffer to fill before the candidate sees their own keystrokes echoed back.
type hijackedPortal struct {
	rw   *bufio.ReadWriter
	conn net.Conn
}

func (h *hijackedPortal) Read(p []byte) (int, error) { return h.rw.Read(p) }
func (h *hijackedPortal) Write(p []byte) (int, error) {
	n, err := h.rw.Write(p)
	if err != nil {
		return n, err
	}
	return n, h.rw.Flush()
}
func (h *hijackedPortal) Close() error { return h.conn.Close() }

// relayBytes splices two duplex streams until either side's copy returns --
// EOF, a closed connection, or an error. The whole shell proxy is this loop.
// Named "term", not "sandbox": this file already imports a package by that
// name, and shadowing it here would be legal but confusing.
func relayBytes(portal, term io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(portal, term); done <- struct{}{} }()
	go func() { _, _ = io.Copy(term, portal); done <- struct{}{} }()
	<-done
}

func (s *Server) reap(w http.ResponseWriter, r *http.Request) {
	killed, err := s.backend.Reap(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reaped": killed})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
