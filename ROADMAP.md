# Roadmap

Top-level tracker for praxis-ops: what's done, what's in flight, what's next.
Detailed rationale lives in `docs/`; this file is the map, not the territory
— update it when a phase closes or a milestone's status changes, don't
duplicate the docs' content into it.

## Done

- **Phase A — Base image.** `praxis/ops-base`, built locally into
  `praxis-sbx`'s store (no registry, no egress). `docs/session-02-plan.md`.
- **Phase B — Containment verification.** `security/hardening-check.sh`,
  `security/verify-shell-isolation.sh` — both pass clean against the real
  host. Bidirectional filesystem isolation between `praxis` and
  `praxis-sbx` confirmed empirically.
- **Phase C — Shell access.** `podman exec` proxied through the
  orchestrator's `/shell` (raw HTTP hijack, not RFC 6455 — see below), no
  sshd. `security/preflight-ticket.sh`.
- **Phase D (upgraded) — Orchestrator hardening + observability, all 6
  stages:**
  1. Digest-pinned images (`Runbook.Validate` / `localImageRef`).
  2. Live spawn/get/destroy through the HTTP API, reaper survives a
     restart and rebuilds from container labels alone.
  3. `UsernsMode: auto` — closed a real root-maps-to-`praxis-sbx` bug,
     confirmed live before and after.
  4. Weighted admission (`Runbook.Weight` / `PRAXIS_CAPACITY_WEIGHT`),
     replacing a flat concurrency cap.
  5. `hostmon` — independent, unauthenticated, no-shared-state metrics
     view; `praxis-sbx.slice` as a persistent cgroup parent; `pxoctl` for
     day-to-day ops.
  6. `bench/staircase.sh` real capacity benchmark, rewritten from scratch
     against the real API.
  - **Closed out 2026-09-03** with a real benchmark run against SJN-01 —
    `docs/capacity-benchmark.md`. `PRAXIS_CAPACITY_WEIGHT` moved from a
    guessed `2` to a measured `11`.
  - Everything in this phase was verified against the real host at every
    step, not just against what compiled — see `docs/session-03-plan.md`
    "Real bugs this pass found" for the pattern worth remembering.

Repo pushed to `github.com/d-clz/praxis-ops`; `upgraded-phase-d` merging
into `main` closes this phase.

## In flight / keep an eye on

- **Low priority, not yet actioned:** `teardown()`'s 5s sleep can race
  hostmon's 15s poll interval, producing a spurious "survived explicit
  destroy" warning with nothing actually left in `podman ps`. Seen twice,
  both false positives. Revisit if it shows up a third time *with*
  something real still running.
- **`PRAXIS_CAPACITY_WEIGHT=11`** is evidence-based but conservative: the
  SJN-01 staircase hit its own `MAX_WEIGHT=16` ceiling without tripping a
  real stop condition, so the true SJN-01 limit is unmeasured and higher.
  Also measures idle-hold cost only, not an active candidate's real load.
  Re-benchmark before treating 11 as more than a reasonable starting
  point — see `docs/capacity-benchmark.md` for what would change it.
- **No destroy reason survives past the container itself.** Confirmed by
  reading `internal/api/server.go`'s `get()`: it returns a flat `404 {"error":
  "no such instance"}` whether the attempt_id never existed, expired on TTL,
  just tripped the new disk cap (`praxis.disk-limit-bytes`), or was explicitly
  destroyed. The only place a reason exists at all is a log line and
  `praxis_destroy_total{reason=...}` — a global counter, not a per-attempt
  record. This is consistent with the orchestrator's deliberate statelessness
  ("if the portal loses an attempt record, the container still dies on
  schedule" — `cmd/orchestrator/main.go`'s reaper comment) but it means a
  future portal cannot tell a candidate *why* their session is gone: TTL
  expiry, disk abuse, and a plain typo'd attempt_id are indistinguishable.
  Sketch for later, not built: a short-lived, explicitly-expiring in-memory
  tombstone map (`attempt_id -> {reason, destroyed_at}`, a few minutes'
  window) that `get()` checks before falling through to "no such instance,"
  returning `410 Gone` with the reason instead when a tombstone is still
  live. Deliberately NOT a persistent event log — that would be new state
  the orchestrator has to carry, the opposite of the statelessness the
  reaper's whole design leans on; a bounded, in-memory, best-effort map
  degrades safely back to today's behavior on restart rather than becoming
  another thing that can disagree with reality.

## Next milestones

1. **`ops-systemd` base image tier.** Blocks CPT-01 entirely — it's the
   only ticket that needs systemd-as-PID-1 in the container. Needed before
   CPT-01 can be built, checked, *or* benchmarked (the real worst-case
   weight number depends on this).
2. **Bake pipeline.** All three tickets still have `substrate_image:
   REPLACE_AT_BAKE` — nothing produces a real pinned digest yet.
   `docs/ops-ticket-spec.md`.
3. **Scoring envelope.** Not started. Grading/check-script execution
   against a submitted attempt.
4. **The portal.** Separate team's deliverable; this repo exposes the
   `X-Praxis-Token`-gated HTTP API for it to integrate against
   (`orchestrator/README.md`) but the portal itself isn't this repo's work.
5. **Operator dashboard + browser shell — new branch, new feature,
   not started.** Scoped in `docs/session-03-plan.md` ("Next: operator
   dashboard with an embedded shell"). Central blocker already identified:
   `/shell` is a raw HTTP hijack, not a real WebSocket — a browser can't
   speak to it as-is. Three open decisions before writing code: WS library
   choice, where the dashboard is served from, and how a browser holds
   `X-Praxis-Token` without leaking it. Branch off `main` once this merges;
   do not build on `upgraded-phase-d`.
