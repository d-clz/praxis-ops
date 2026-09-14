# Session 03 — plan

## Where session 02 / upgraded Phase D left off

Original Phase D (`docs/session-02-plan.md`) is fully done — all 6 items,
each verified against the real host, not just against what compiled: digest
pinning resolved (`internal/sandbox.localImageRef`), live spawn/get/destroy
through the HTTP API, the reaper surviving a restart and rebuilding from
labels alone, `/shell` proven against a real container for the first time.
The observability scaffolding that landed alongside it (`internal/metrics`,
`cmd/hostmon`, weighted admission, the `praxis-sbx.slice` cgroup, `pxoctl`,
`bench/staircase.sh`) is wired for real and verified live, not just built.

Real bugs this pass found that are worth remembering, not just fixing:
`UsernsMode` unset meant every orchestrator-spawned container mapped root to
`praxis-sbx` itself (security-significant, confirmed live before the fix);
`internal/metrics/labels.go` assumed underscore label keys that were
actually hyphenated (every managed container silently misclassified as an
orphan); `Runbook` had no `json` tags, so a JSON body using the yaml-style
snake_case names silently left `TTLSeconds`/`PidsLimit` at zero;
`praxis-sbx.slice`'s real cgroup path had an extra `praxis.slice` segment
nothing predicted (systemd's own hyphen-implies-parent naming convention).
Pattern worth carrying forward, same one `docs/session-01-hardening.md`
already named: things that compile clean and pass their own unit tests can
still be wrong in ways only running them for real reveals.

Still genuinely owed, unchanged from `docs/session-02-plan.md`: the bake
pipeline (all three tickets' `substrate_image` is still `REPLACE_AT_BAKE`),
the `ops-systemd` base image tier (CPT-01 cannot be built or benchmarked
without it), the scoring envelope, and the portal itself.

---

## Operator dashboard with an embedded shell — DONE (branch `operator-dashboard`)

Built and verified for real against a running `cmd/mockorchestrator` in an
actual browser — login, session list, capacity, and a real xterm.js
terminal round-tripping keystrokes through a real WebSocket, not just
built to spec. Full functional contract: `docs/dashboard-spec.md`. Also
folded into this pass, "for integration ready": `internal/dashboard/static/openapi.yaml`
(the real API contract, written first) and `cmd/mockorchestrator` +
`internal/mockbackend` (a real, runnable, no-podman stand-in for a portal/
dashboard developer to build against).

**One deliberate scope narrowing from this doc's original sketch:**
hostmon's independent view is NOT surfaced in this dashboard — see
`docs/dashboard-spec.md`'s Non-goals, which explains why folding it in
would undermine the two-view model's whole point (hostmon staying
independent so it keeps reporting if the orchestrator deadlocks). If a
unified view is ever wanted, that's a separate, deliberate decision later,
not something this pass did quietly.

**A real, unrelated bug found while testing this, not related to the
dashboard itself:** the first live test of `/shell` this whole project had
used a plain `curl -N -X POST` with no `--data` flag, which sent an empty
request body and could never have forwarded live keystrokes in the first
place -- the "typing does nothing" it produced was a test-tool artifact,
not a real defect in `ExecShell`/the relay. Confirmed by building the new
real-WebSocket sibling endpoint and dialing it with an actual bidirectional
client (`internal/api/server_test.go`'s `TestWsShell_RelaysBytesBothWays`,
and later a real browser) — both worked cleanly on the first try.

**Scope, settled:** this is an *operator* tool, not the portal. It doesn't
authenticate candidates or map a login to one `attempt_id` -- it's a web
front end over the exact same `X-Praxis-Token`-gated API `pxoctl`/`curl`
already use, for whoever operates this box to see every session and drop
into any of them by hand -- troubleshooting, or previewing what a candidate
would actually experience. That's a different thing from candidate-facing
access and doesn't erode the boundary `orchestrator/README.md` already
states ("this orchestrator is finished" means safe for the portal team to
integrate against, not safe to expose to a candidate) -- this dashboard sits
on the operator side of that same seam, not the candidate side.

**What it needs to show:** live session list and states (`/sessions` on
both the orchestrator and hostmon), capacity (`praxis_capacity_weight_used`
/ `_limit`), the two-view orphan reconciliation
(`docs/observability.md` §1) made visually obvious instead of requiring a
`grep` across two curl calls, and a shell into any selected session.

### The one fact that changes the design

`/shell` as built is **not a real WebSocket** -- it's a raw HTTP hijack
(`101 Switching Protocols`, custom `Upgrade: praxis-shell` header, opaque
bytes after that, no frame encoding). That works for `curl -N` and for a
future portal backend talking server-to-server, but a browser's native
`WebSocket` API cannot speak to it -- it strictly requires the RFC 6455
handshake and frame format. A browser-based terminal needs real WebSocket
support on the server side; this is not optional polish.

### Decisions to make before writing code — RESOLVED

1. **WebSocket implementation → `github.com/coder/websocket`.** The
   library call, as anticipated. Confirmed as the right *technique*, not
   just a dependency choice: it's literally how AWS CloudShell and GCP
   Cloud Shell work (real PTY, raw bytes relayed over a real WebSocket,
   xterm.js rendering the stream in-browser). `/shell`'s raw hijack was
   left completely unchanged, still the right choice for `curl -N`/a
   future portal backend -- the new `GET .../shell/ws` sits alongside it,
   not in place of it.
2. **Where the dashboard lives → served by the orchestrator itself**, as
   anticipated. `internal/dashboard`, `go:embed`'d, mounted at `/ui/` on
   the same `*http.Server` -- one binary, nothing new to deploy.
3. **Token-in-a-browser → `sessionStorage` + a login gate, `Sec-WebSocket-
   Protocol` for the handshake specifically.** Never a query param. Full
   reasoning and the one real bug this surfaced (a stored token was never
   read back on page load, defeating the entire point of choosing
   `sessionStorage`): `docs/dashboard-spec.md` and the `operator-dashboard`
   branch's own commit history.
