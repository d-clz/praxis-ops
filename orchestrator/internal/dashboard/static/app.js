// Praxis operator dashboard. Vanilla JS, no build step, no framework --
// matches this being a Go-only project (orchestrator/README.md). Every
// data point shown here is defined in docs/dashboard-spec.md's
// tracked-metrics table; keep this file and that table in sync.
"use strict";

const POLL_INTERVAL_MS = 5000;

// sessionStorage, not localStorage -- clears on tab/browser close, per
// docs/session-03-plan.md's resolved token-handling decision. Never
// written to a URL, browser history, or logged anywhere in this file.
function getToken() { return sessionStorage.getItem("praxisToken"); }
function setToken(t) { sessionStorage.setItem("praxisToken", t); }
function clearToken() { sessionStorage.removeItem("praxisToken"); }

async function authedFetch(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({}, opts.headers, { "X-Praxis-Token": getToken() });
  return fetch(path, opts);
}

// ---- login ----------------------------------------------------------------

const loginSection = document.getElementById("login");
const dashboardSection = document.getElementById("dashboard");
const loginForm = document.getElementById("login-form");
const loginError = document.getElementById("login-error");

// tryEnter verifies a candidate token against a real authenticated call
// (docs/dashboard-spec.md acceptance criterion #1 -- never a client-side
// string compare a modified page could bypass) and, on success, enters the
// dashboard. Shared by the login form AND the on-load check below: a token
// already sitting in sessionStorage from before a page reload deserves the
// exact same verification as one just typed in, not a different, weaker
// path assuming it's still valid.
async function tryEnter(candidate) {
  let resp;
  try {
    resp = await fetch("/sessions", { headers: { "X-Praxis-Token": candidate } });
  } catch (err) {
    return { ok: false, message: "Could not reach the orchestrator: " + err };
  }
  if (!resp.ok) {
    return { ok: false, message: resp.status === 401 ? "Wrong token." : "Login check failed: HTTP " + resp.status };
  }

  setToken(candidate);
  loginSection.hidden = true;
  dashboardSection.hidden = false;
  document.getElementById("poll-interval-s").textContent = String(POLL_INTERVAL_MS / 1000);
  startPolling();
  return { ok: true };
}

loginForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  loginError.hidden = true;
  const result = await tryEnter(document.getElementById("token-input").value);
  if (!result.ok) {
    loginError.textContent = result.message;
    loginError.hidden = false;
  }
});

// On load, a token already in sessionStorage (survives a reload within the
// same tab -- that's the entire point of choosing sessionStorage over a
// plain in-memory variable) gets the same real verification, so a reload
// doesn't force re-entering a still-valid token. Silently falls through to
// the login form if there's no stored token or it's no longer valid
// (expired token rotated server-side, etc.) -- no error banner on this
// path, since "please log in" is the expected first-visit state, not a
// failure.
(async () => {
  const stored = getToken();
  if (stored) {
    const result = await tryEnter(stored);
    if (!result.ok) clearToken();
  }
})();

document.getElementById("logout").addEventListener("click", () => {
  stopPolling();
  closeShell();
  clearToken();
  dashboardSection.hidden = true;
  loginSection.hidden = false;
});

// ---- polling: sessions + capacity summary ----------------------------------

let pollTimer = null;

function startPolling() {
  refresh();
  pollTimer = setInterval(refresh, POLL_INTERVAL_MS);
}
function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
}

async function refresh() {
  await Promise.all([refreshSessions(), refreshSummary()]);
}

async function refreshSummary() {
  const resp = await authedFetch("/dashboard/summary");
  if (!resp.ok) return; // a transient failure here shouldn't nuke the session list too
  const s = await resp.json();
  document.getElementById("capacity-used").textContent = s.capacity_used;
  document.getElementById("capacity-limit").textContent = s.capacity_limit;
  document.getElementById("taken-at").textContent = s.taken_at;
}

let openAttemptID = null;

async function refreshSessions() {
  const resp = await authedFetch("/sessions");
  if (!resp.ok) return;
  const data = await resp.json();

  document.getElementById("session-count").textContent = data.sessions.length;

  const orphanTotal = Object.values(data.orphan_counts || {}).reduce((a, b) => a + b, 0);
  const note = document.getElementById("orphans-note");
  note.textContent = orphanTotal > 0
    ? orphanTotal + " orphaned container(s) this view can't classify -- see hostmon's independent view, not shown here (docs/dashboard-spec.md Non-goals)."
    : "";

  const tbody = document.getElementById("sessions-tbody");
  tbody.innerHTML = "";
  let openStillPresent = false;
  for (const s of data.sessions) {
    if (s.attempt_id === openAttemptID) openStillPresent = true;
    const tr = document.createElement("tr");
    tr.className = "session-row";
    if (s.attempt_id === openAttemptID) tr.classList.add("selected");
    tr.innerHTML =
      "<td>" + escapeHTML(s.attempt_id) + "</td>" +
      "<td>" + escapeHTML(s.state) + "</td>" +
      "<td>" + escapeHTML(s.weight) + "</td>" +
      "<td>" + (s.expired ? "expired" : escapeHTML(s.remaining_seconds + "s")) + "</td>";
    tr.addEventListener("click", () => openShellPanel(s.attempt_id));
    tbody.appendChild(tr);
  }

  // Acceptance criterion #5: a session gone from /sessions closes its own
  // open terminal panel with a visible disconnect, not a silently-stale UI.
  if (openAttemptID && !openStillPresent) {
    showDisconnected("session no longer exists (expired, destroyed, or over its disk cap)");
  }
}

function escapeHTML(s) {
  const d = document.createElement("div");
  d.textContent = s;
  return d.innerHTML;
}

// ---- shell panel: real WebSocket, real xterm.js -----------------------------

const shellPanel = document.getElementById("shell-panel");
const shellDisconnected = document.getElementById("shell-disconnected");
let term = null;
let ws = null;

function openShellPanel(attemptID) {
  openAttemptID = attemptID;
  document.getElementById("shell-title").textContent = attemptID;
  shellPanel.hidden = false;
  connectShell();
}

document.getElementById("shell-reconnect").addEventListener("click", connectShell);
document.getElementById("shell-close").addEventListener("click", closeShell);

function connectShell() {
  if (!openAttemptID) return;
  shellDisconnected.hidden = true;

  if (!term) {
    // window.Terminal is xterm.js's UMD global (vendor/xterm.js).
    term = new Terminal({ cursorBlink: true });
    term.open(document.getElementById("terminal"));
  } else {
    term.reset();
  }

  const user = document.getElementById("shell-user").value || "candidate";
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const url = proto + "//" + location.host + "/instances/" + encodeURIComponent(openAttemptID) +
    "/shell/ws?user=" + encodeURIComponent(user);

  // The token travels as a WebSocket subprotocol, not a header -- a
  // browser's WebSocket constructor cannot attach X-Praxis-Token to the
  // handshake at all (docs/session-03-plan.md's resolved decision;
  // internal/api/server.go's wsShell reads it from here specifically).
  ws = new WebSocket(url, [getToken()]);
  ws.binaryType = "arraybuffer";

  ws.onopen = () => { term.focus(); };

  ws.onmessage = (ev) => {
    // Server relays MessageBinary frames only (websocket.NetConn(...,
    // websocket.MessageBinary) in server.go) -- write the raw bytes
    // straight to xterm.js rather than decoding to a JS string first, so
    // a multi-byte UTF-8 sequence split across two frames doesn't corrupt.
    term.write(new Uint8Array(ev.data));
  };

  ws.onclose = (ev) => {
    showDisconnected(ev.reason || ("connection closed (code " + ev.code + ")"));
  };
  ws.onerror = () => {
    showDisconnected("connection error");
  };

  term.onData((data) => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      // Must send binary, not text: the server's websocket.NetConn is
      // constructed with MessageBinary and closes the connection on
      // receiving a mismatched frame type (its own documented behavior).
      ws.send(new TextEncoder().encode(data));
    }
  });
}

function showDisconnected(reason) {
  shellDisconnected.hidden = false;
  shellDisconnected.textContent = "disconnected: " + reason;
}

function closeShell() {
  if (ws) { ws.close(); ws = null; }
  if (term) { term.dispose(); term = null; }
  openAttemptID = null;
  shellPanel.hidden = true;
}

// Known limitation, not silently omitted: no terminal resize propagation.
// Backend.ExecShell (internal/sandbox/container.go) never calls
// ContainerExecResize -- there is no mechanism today to tell the
// container's PTY the browser terminal's actual size, on this endpoint or
// the raw-hijack /shell either. A fixed-size terminal is what xterm.js
// defaults to without a fit addon; wiring real resize is separate,
// backend-touching work outside this feature's scope
// (docs/dashboard-spec.md's acceptance criteria don't require it).
