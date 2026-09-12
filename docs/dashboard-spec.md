# Operator dashboard — spec

Written before any frontend code exists, same discipline
`docs/ops-ticket-spec.md` already imposes on tickets: the Definition of
Done comes first, not after. Architecture decisions (WebSocket library,
where it's served from, token handling) are resolved in
`docs/session-03-plan.md`; this document is the functional contract for
what gets built against that architecture —
`internal/dashboard/static/openapi.yaml` is the API contract, this is the
dashboard's own.

## Scope

An **operator** tool, not the portal. It authenticates with the same
shared `X-Praxis-Token` every `curl`/`pxoctl` call already uses, shows
*every* managed session (not one candidate's), and lets an operator drop
into any of them by hand — troubleshooting, or previewing what a candidate
would actually experience. It sits on the operator side of the seam
`orchestrator/README.md` already draws ("safe for the portal team to
integrate against" ≠ "safe to expose to a candidate"); this dashboard does
not change that boundary or authenticate candidates.

## Acceptance criteria

Each one is checkable by a person actually doing it, not a vibe:

1. **Login.** An operator holding the shared token can reach the
   dashboard's login page unauthenticated, submit the token, and land on
   the session list. An operator without the token, or with a wrong one,
   cannot get past login — verified against a real authenticated call
   (`GET /sessions`), not just a client-side string comparison a modified
   page could bypass.
2. **Session list reflects reality, not a stale cache.** The list shown
   matches `GET /sessions`'s own response at the time of the most recent
   poll — every currently-managed session appears, and a session destroyed
   or expired since the last poll is gone by the next one. The poll
   interval itself is visible in the UI (not a secret assumption an
   operator has to infer).
3. **Capacity is legible at a glance.** `capacity_used` vs
   `capacity_limit` (from `GET /dashboard/summary`) is shown without
   requiring the operator to do the arithmetic or read `/metrics`'
   Prometheus text by hand.
4. **Selecting a session opens a real, working interactive shell.**
   Clicking a session in the list opens a terminal panel; keystrokes typed
   in the browser actually reach the container (a real command run inside
   produces real output back in the browser) — round-tripped through the
   new `GET /instances/{attemptID}/shell/ws` endpoint, not simulated
   client-side.
5. **A session that stops existing is reflected without a page reload.**
   If a session hits its TTL, its disk cap, or is destroyed by another
   operator while its terminal panel is open, the dashboard visibly
   indicates the shell has disconnected — it does not sit silently as if
   still connected.
6. **The dashboard makes no undocumented API calls.** Every request it
   sends is one already described in the OpenAPI spec. If a new endpoint
   (like `GET /dashboard/summary`) is needed, the spec is updated first,
   not discovered by reading the frontend's fetch calls.
7. **No token ever appears in a URL, browser history entry, or
   `Referer` header.** The one exception permitted by design is the
   WebSocket handshake's `Sec-WebSocket-Protocol` value, per
   `docs/session-03-plan.md`'s resolved decision — checked by inspecting
   real network traffic during manual verification, not assumed from the
   code alone.
8. **The API contract is discoverable from the running server itself, not
   only from the repo.** `GET /ui/openapi.yaml` (raw) and
   `GET /ui/api-docs.html` (rendered, via a vendored Redoc bundle — no
   external fetch) are both unauthenticated and linked from the login
   page, so someone evaluating whether to integrate doesn't need a token
   or git access first.

## Tracked metrics / data points

Every value the dashboard surfaces, where it actually comes from, and how
stale it can be. The point of writing this down is to stop the dashboard
from silently drifting into implying something more live or more precise
than what it's actually showing.

| Shown as | Source | Meaning | Refresh cadence / staleness |
|---|---|---|---|
| Session list (id, runbook digest, state, weight, expiry countdown) | `GET /sessions` → `sessions[]` | The orchestrator's own label-filtered view (`docs/observability.md`'s "orchestrator view", not hostmon's independent one) | As stale as the reaper's last completed tick — up to `PRAXIS_REAP_INTERVAL` (default 30s), not live per request |
| Capacity used / limit | `GET /dashboard/summary` → `capacity_used` / `capacity_limit` | Same `praxis_capacity_weight_used`/`_limit` gauge `/metrics` exposes, restated as JSON | Same staleness as the session list — both come from the identical reaper snapshot |
| Session count | `GET /dashboard/summary` → `session_count` | Count of sessions in the same snapshot, not a separate live count | Same staleness as above |
| Orphan counts | `GET /sessions` → `orphan_counts` | Containers the orchestrator's own label filter couldn't classify (`unmanaged`, `unparseable_label`, `unreaped`) — **not** the same as hostmon's independent orphan view; this dashboard does not currently surface hostmon's view at all (see Non-goals) | Same staleness as the session list |
| Terminal output | `GET /instances/{attemptID}/shell/ws` | Raw PTY bytes from the container, relayed live | Real-time — this is the one genuinely live thing on the page |
| "Taken at" timestamp | `GET /dashboard/summary` → `taken_at` | When the underlying snapshot was actually collected | Shown explicitly so "the list looks empty/wrong" is distinguishable from "the list is just old" |

## Non-goals (explicitly out of scope for this feature)

- **hostmon's independent view.** `docs/observability.md`'s two-view model
  exists specifically so hostmon keeps reporting if the orchestrator
  deadlocks — folding its view into a dashboard the orchestrator itself
  serves would undermine that independence. If a unified view is ever
  wanted, it's a deliberate, separate decision, not a side effect of this
  feature.
- **Candidate-facing anything.** No login-as-candidate, no attempt_id ↔
  candidate identity mapping. That's the portal's job.
- **Manual capacity tuning from the UI** (the earlier open question this
  session started from — "do we have somewhere to visualize/adjust
  capacity configs?"). This dashboard is read-plus-shell, not a config
  editor; `PRAXIS_CAPACITY_WEIGHT` stays a deploy-time environment
  variable, changed via the existing `sudo make deploy` path.
- **Session history / audit log.** Only currently-managed sessions are
  shown; nothing about a destroyed session persists in the dashboard after
  it's gone from `/sessions`.
- **Remote/public reachability.** The dashboard rides `PRAXIS_LISTEN`'s
  existing loopback-only bind — reaching it from off-box is an SSH tunnel
  (`orchestrator/deploy/README.md`), not a server-side change. Rebinding to
  a real interface would expose every other route too, not just `/ui/`,
  and there is no inbound firewall today to fall back on
  (`bootstrap/40-network-guard.sh` is outbound-only) — a deliberate,
  separate decision if ever wanted, not something this feature does.
