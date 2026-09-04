# Capacity benchmark

Living record of `bench/staircase.sh` runs against the real host. Append a
new dated section per run rather than overwriting — the point is to see how
the number moves as tickets/images/host load change over time.

## How to read a run

`bench/staircase.sh` climbs `PRAXIS_CAPACITY_WEIGHT` one step at a time
against a single ticket, holding every previously-spawned sandbox alive
while adding more, until a real stop condition trips (PSI full avg60 >10%,
storage <20% free, an OOM kill, spawn p95 >20s, neighbour health >2000ms) or
it reaches the script's own `MAX_WEIGHT` ceiling first. Reaching
`MAX_WEIGHT` without tripping anything is **not** the same as finding the
box's real limit — it means the box held at least that many, and the run
didn't ask further. Say so explicitly whenever it happens; don't round it
up to "the limit."

---

## 2026-09-03 — SJN-01, weight 16

- **Ticket:** SJN-01 (`ops-base` image, no systemd — the only ticket
  buildable right now; CPT-01 needs the still-missing `ops-systemd` tier).
- **Result file:** `bench/results/20260903T172522Z/`
- **Stopped because:** reached `MAX_WEIGHT=16` without tripping a stop
  condition. **This is a ceiling we chose, not one the host hit.** Nothing
  in the run — PSI, storage, OOM, latency, neighbour health — got
  meaningfully close to its stop threshold at weight 16. SJN-01 alone would
  almost certainly hold more; this run just didn't go looking for where.
- **Neighbour check:** `http://127.0.0.1:3010` (the portal team's own
  service, per [[portal_team_service]] — not GitLab as originally assumed
  when this URL was picked. Still a valid contention probe regardless of
  which service answers on it.)

### What the data actually shows

**Memory/PSI: a non-event.** `psi_some` and `psi_full` read `0.00` for
every sample across all 16 steps, and `oom_kills` stayed `0` throughout.
`slice_mem_bytes` grew from ~12.3–12.5MB at weight 1 to ~27MB at weight 16
— roughly 1–2MB per idle sandbox. SJN-01 has a 512MB per-container memory
*limit*, but an idling SJN-01 container uses a tiny fraction of it. Memory
was nowhere close to being the constraint in this run.

**Storage: declining smoothly, plenty of runway left.** `storage_free_pct`
dropped from 98% (step 1) to 90% (step 16) — roughly linear, ~0.5 points
per additional concurrent sandbox (each new container's overlay writable
layer). At that rate, reaching the 20%-free stop condition would take on
the order of another ~140 sandboxes held concurrently — storage is a real
eventual constraint but wasn't remotely close to binding at weight 16.

**Spawn latency: a step change, then flat — not a climb.** The raw
per-spawn latencies (`spawns.csv`):

| step | latency (s) |
|---|---|
| 1 | 0.24 |
| 2 | 0.20 |
| 3 | 5.34 |
| 4 | 4.79 |
| 5 | 4.74 |
| 6 | 9.54 |
| 7 | 7.03 |
| 8 | 4.69 |
| 9 | 4.72 |
| 10 | 4.66 |
| 11 | 4.74 |
| 12 | 4.98 |
| 13 | 4.79 |
| 14 | 4.82 |
| 15 | 4.85 |
| 16 | 4.76 |

Steps 1–2 spawn near-instantly (0.2s, empty/cold box), then latency jumps
to a ~4.7–5.0s plateau from step 3 onward and **stays flat through step
16** — it does not keep climbing as weight increases. That points to a
roughly fixed per-spawn cost (image/overlay setup, userns mapping, cgroup
creation) that shows up once a couple of containers already exist, rather
than contention that gets worse with concurrency. The two outliers (9.54s
at step 6, 7.03s at step 7) look like transient host jitter, not a trend —
step 8 immediately drops back to the 4.7s plateau. `spawn p95` in the
report (7.03s) is the 15th of 16 sorted samples (nearest-rank, floor
method), which is why it reads *below* the single 9.54s max.

### Caveat: this measures idle capacity, not active-candidate capacity

`bench/staircase.sh` spawns and holds — it doesn't simulate a candidate
actually typing commands, running builds, or generating load inside the
shell. An idle SJN-01 sandbox costs almost nothing in memory (confirmed
above). A real assessment session will cost more than this benchmark
measured, by an amount this run doesn't quantify. Treat the weight-16
result as a **lower bound on cost**, not a prediction of real production
load.

### Recommendation

Per the script's own stated rule (~70% of steady-state weight), and given
steady-state here means "held 16 with zero measured pressure," not "found
the wall": set `PRAXIS_CAPACITY_WEIGHT=11` as the production value — a
large, evidence-based improvement over the original placeholder guess of
2, while staying conservative against the two caveats above (16 is a
floor, not the real ceiling; idle load is cheaper than active load).

Revisit this number when either becomes true:
- CPT-01 / `ops-systemd` exists and gets its own staircase run (it will
  cost more per sandbox — real systemd + nginx + generated logs, even
  idle — and is the real worst case this project needs a number for).
- Someone wants the actual SJN-01 ceiling rather than a floor: re-run with
  `MAX_WEIGHT=32` or higher and see what, if anything, actually trips.

### Not addressed by this run

CPT-01/`ops-systemd` benchmarking (blocked on the image tier not existing
yet), and the operator dashboard idea in `docs/session-03-plan.md` (parked
for a later phase, unrelated to capacity).
