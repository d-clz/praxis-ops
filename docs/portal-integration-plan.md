# Plan — portal integration window

Written 2026-09-14/15, against `operator-dashboard` @ a5d91ed and
`docs/capacity-benchmark.md`'s 2026-09-15 revision. Synthesizes two review
passes (`review-operator-dashboard.md`, an external review of this branch;
`plan-portal-integration.md`, a follow-up plan) with corrections found by
direct testing against the real host — see the corrections section before
acting on anything below that cites a specific number.

**Ordering principle, unchanged from both source reviews:** the portal team
is integrating against the real orchestrator now, not hypothetically. Work
is ranked by *who pays when it goes wrong*, not by difficulty or interest.
Internal debt costs you time. Externalised debt costs cross-team debugging,
paid at someone else's convenience.

## Corrections to the plan this supersedes

`plan-portal-integration.md` (the source for most of this) computed its
budget against a "~64 or ~1024, unresolved" pool size. That gap is now
closed, but not the way its own T1 table expected:

- `/etc/subuid`/`/etc/subgid` confirmed **`165536:1048576`** on the live
  host, 2026-09-15 — the 16x-widened pool from the original capacity
  benchmark survived the 2026-09-11/12 `system reset` (the reset wipes
  podman's own storage/database, not this host-level file). **Real budget
  is ~1024 slices per reset, not 64.**
- That resolution **breaks**, not confirms, the idea that this slice-leak
  mechanism explains the original ~60-64 concurrency ceiling
  (`capacity-benchmark.md`'s "Known host constraint"). The original
  widened-pool test ran against this exact 1,048,576-wide pool and still
  failed at spawn 65 — nowhere near the ~1024 spawns the slice mechanism
  would need to exhaust it. **The two are separate findings; the original
  ceiling's cause remains unresolved.** Full writeup:
  `docs/capacity-benchmark.md`'s 2026-09-14/15 section and the retraction
  banner on "Known host constraint" above it.
- T9's five documentation critiques (forward-pointer banner, softening the
  overclaimed "fully explains," separating the `65537` figure from the
  `1024`-slice figure, fixing the headline number, flagging the
  dedupe/orphan inconsistency) are **done** — applied directly to
  `docs/capacity-benchmark.md` and `ROADMAP.md`.
- T6 (shell attach/detach audit log) and the P2 `escapeHTML` fix on
  `weight`/`remaining_seconds` are **done** —
  `orchestrator/internal/api/server.go`, `orchestrator/internal/dashboard/static/app.js`.
- The "Also worth noting" claim that Stage-4 behavioral verification wasn't
  re-run for either ticket post-rebuild is **half right**: SJN-01's was
  (`docs/dashboard-verification-report.md`, 2026-09-12, real planted
  processes confirmed via a live shell session against the post-rebuild
  digest). **Only CPT-01's nginx-seeded-disabled check is still open.**

---

## P0 — before the portal team generates real load

### T1. Slice-budget gauge in `hostmon`

Still the single highest-value hour of work available — converts an
unexplained spawn cliff into a number both teams can read. Budget constant
updates to match the confirmed pool size:

```
praxis_userns_slices_used   = (max_owner_uid - 165536) / 1024 + 1
praxis_userns_slices_budget = 1024        # confirmed via /etc/subuid, 2026-09-15
```

Source:

```bash
find ~/.local/share/containers/storage/overlay -maxdepth 1 -printf '%U\n' \
  | sort -n | tail -1
```

- Surface on the operator dashboard alongside weight.
- Warn at 768 (75%), alarm at 896 (87.5%) — scaled proportionally from the
  original plan's 48/56-of-64 thresholds.
- **Known limitation, document it at the metric:** reads the highest
  *surviving* owner UID, not the allocator's own pointer. Any cleanup that
  deletes the topmost directory makes the gauge read low while the
  allocator stays high. Safe today because nothing deletes them yet;
  re-validate before trusting it after any `Destroy()` reclamation work
  lands.
- **Does not, on its own, explain the original ~60-64 ceiling** (see
  corrections above) — that has a different, still-unknown cause. This
  gauge tracks the separate ~1024-slice budget confirmed this week, and
  incidentally can help settle the still-open sequential-vs-alive dedupe
  question from normal traffic, at zero additional probing cost.

### T2. Hazardous-operations warning where the portal team will see it

Unchanged from the source plan. `podman save`, `system check`, and
`system migrate` are unsafe on this host — `system check` flagged all four
real images as damaged, and `--repair` would have deleted them. Currently
buried ~150 lines into `capacity-benchmark.md`; nobody outside this
project will read that far.

- Repo-root README section, and whatever integration notes the portal team
  gets.
- State the safe diagnostics explicitly: `df`, `du`, `podman ps -a`,
  `podman inspect <known-id>`.
- State the only validated recovery: `bootstrap/90-storage-reset-rebuild.sh`.

### T3. Agree the reset protocol with the portal team

Unchanged. `90-storage-reset-rebuild.sh` rebuilds from a fresh debootstrap,
so image digests change — already happened once, 2026-09-11/12.

- Reset is coordinated, never unilateral.
- Portal reads digests from each ticket's `scenario.yaml`, never hardcoded.
- Agree who calls it and on what signal (T1's alarm threshold).

### T4. Bind decision: loopback or network interface

Unchanged. The only item with a deadline on someone else's calendar — free
to decide now, a retrofit once they've integrated.

- **Loopback + portal on this box:** nothing else changes. `ws://` stays
  fine, the shared token stays contained, every dashboard finding deferred
  in P2 stays deferred.
- **Network interface:** the token crosses the network on every request,
  the shell handshake carries it in cleartext, TLS stops being optional,
  and per-attempt token scoping comes back onto the list.

Whichever is chosen: make the orchestrator refuse to start on a
non-loopback `PRAXIS_LISTEN` without TLS.

### T5. Destroy reasons + tombstones

Unchanged. Ranked here because it changes the *volume* of inbound CRs, not
the severity of any one incident. Today a vanished session is a bare 404 —
TTL expiry, disk-cap kill, explicit delete, and a typo'd `attempt_id` are
indistinguishable, so every occurrence arrives as "your 404, ours or
yours?" across a team boundary.

- Bounded, in-memory, TTL-expiring; degrades to current behavior on
  restart (shape already sketched in `ROADMAP.md`).
- Every `Destroy()` path records a reason: `ttl_expired`, `disk_cap`,
  `explicit`, `spawn_failed`.
- `GET /instances/{id}` returns the reason while the tombstone lives.

Build **before** the error-handling milestone (T7) — error handling built
against a bare 404 encodes "not found → give up" into the portal client,
and every call site then needs revisiting by a team that isn't yours.

---

## P1 — during integration

### T6. Centralized logging, with the shell audit trail folded in — DONE

Shell attach/detach now logged at Info level (`transport`: `hijack` or
`ws`), `attempt_id`, `user` — `orchestrator/internal/api/server.go`. Not
yet folded into a larger centralized-logging system; this is the audit
trail half only.

### T7. UX polish and error handling

Now has four distinguishable failure reasons to actually handle, once T5
lands.

### T8. Close the CPT-01 leak-size inference

The arithmetic is already in `docs/capacity-benchmark.md`: 18,426MB
reclaimed, SJN-01's ~60 copies at 4KB contributing nothing, leaves ~88
copies averaging ~209MB — likely CPT-01. Still inferred, not measured. One
`du -sh` on a single CPT-01 leaked copy after one real spawn confirms or
kills it. Costs one slice (of ~1024 now available, not ~64) — spend it
deliberately, not incidentally.

### T9. Document fixes in `capacity-benchmark.md` — DONE

All five points from the source plan applied 2026-09-15: forward-pointer
banner on "Known host constraint," the "fully explains" overclaim
retracted rather than softened (turned out to be fully wrong, not just
overstated), the `65537` vs `1024`-slice figures explicitly separated, the
headline number fixed to ~1024, and the dedupe/orphan inconsistency
flagged as unresolved.

---

## P2 — postponed, deliberately

All of these cost *you* time when they go wrong, not the portal team.

| Item | Why it waits |
|---|---|
| `Destroy()` leak reclamation | Slice-reuse-after-delete unconfirmed; risk of building reclamation for an allocator that doesn't reuse freed slices. T1's gauge tells you when this becomes urgent. |
| Root cause of the original ~60-64 ceiling | Genuinely unresolved, not just deprioritized — needs someone to trace `65537:65537` through podman/containers-storage's own code, not further pattern-matching from this project's side. |
| Per-attempt token scoping | Operator-only surface, both parties trusted. Returns immediately if T4 goes non-loopback, or if this UI ever faces candidates. |
| `userns_size` Runbook field + size=256 probe | Costs slices to run; T1 is more informative for free. |
| Ticket weights | Weighted admission built and unused; all three tickets spawn at weight 1 despite comparative data existing. |
| ~~`escapeHTML` on `weight` / `remaining_seconds`~~ | **Done**, 2026-09-14. |
| redoc off the shell origin | Judgment call for a dev tool, not a finding. |
| `staircase.sh` `IMAGE`/`RUNBOOK` decoupling | Caused one invalid close-out, documented not fixed. Cost lands on you. |

---

## Triaging inbound CRs

Unchanged from the source plan — worth holding from day one.

**Contract CRs** — response shapes, status codes, missing fields, endpoint
semantics. Take these seriously and early. Cheap now, expensive once the
portal has built around a shape.

**Host CRs** — "spawn intermittently fails," "the container vanished,"
"this got slow." Check the T1 gauge before changing anything. Most of this
class will be slice budget or the leak, and patching the API to paper over
it makes the real problem harder to see.

**The one to pre-empt:** spawn failure near exhaustion surfaces as a
generic error and gets filed as an orchestrator bug. With T1 in place and
the budget communicated, it becomes "we're at 900 of ~1024, time for a
coordinated reset" instead of a week of mutual debugging.

---

## Also worth noting

`docs/dashboard-spec.md` should state **operator-only** explicitly. Not
because the current design is wrong for its audience — because the shell
panel is the obvious thing to reuse when candidate access is built, and at
that moment every P2 dashboard item returns at once.

**CPT-01's Stage-4 behavioral verification was not re-run** after the
09-11/12 rebuild — does its nginx actually seed disabled. SJN-01's was
already confirmed (`docs/dashboard-verification-report.md`, 2026-09-12).
The CPT-01 image is currently structurally and containment verified only;
do the behavioral check before the portal team's integration treats it as
correct.
