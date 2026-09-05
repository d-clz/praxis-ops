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

## Known host constraint: podman userns/idmap ceiling (~60-64 containers)

Discovered 2026-09-04/05, running the real SJN-01 staircase (`docs/capacity-benchmark.md`'s
own SJN-01 entry below). This is not one of `bench/staircase.sh`'s four
documented stop conditions (PSI, storage, OOM, neighbour) — it's a fifth,
previously-unknown failure mode that turned out to be the actual binding
constraint on this host, arriving well before any of the four monitored
conditions got close to tripping.

### Symptom

Every SJN-01 staircase run stopped at the exact same point — held 60
concurrent containers cleanly, failed on the 65th spawn attempt — across
three separate attempts, none of which moved the number even slightly:

1. Original run: `/etc/subuid`/`/etc/subgid` at the host's auto-assigned
   `165536:65536` (65,536 total delegated subordinate UIDs/GIDs for
   `praxis-sbx`). Failed at weight 65.
2. Widened `/etc/subuid`/`/etc/subgid` to `165536:1048576` (16x larger) and
   redeployed. **Failed at weight 65 again, identical error text.**
3. Ran `podman system migrate` (podman's own suggested remedy, printed in
   the error message itself) on top of the widened range. **Failed at
   weight 65 again, still identical error text.**

The orchestrator's log for the failing spawn (`journalctl --user -u
praxis-orchestrator`):

```
create failed err="create sbx-bench-...-064: Error response from daemon:
container create: creating container storage: creating an ID-mapped copy
of layer \"...\": creating copy of template layer \"...\" with ID \"...\":
potentially insufficient UIDs or GIDs available in user namespace
(requested 65537:65537 for /home/praxis-sbx/.local/share/containers/
storage/overlay/...): Check /etc/subuid and /etc/subgid if configured
locally and run \"podman system migrate\": chown ...: invalid argument"
```

The requested value (`65537:65537` — exactly `65536 + 1`) never changed
across any of the three attempts, despite the real subuid pool size
changing by 16x. That alone rules out subuid pool size as the actual
constraint, whatever the error message's own suggested remedy implies.

### What was ruled out, with real measurements

- **Not host resource pressure.** At the last held weight (60), real memory
  usage was ~1.6GB out of 14GB (~11%), storage was 52% free (well above the
  20% floor), PSI was `0.00`/`0.00`, and zero OOM kills — all four of
  `bench/staircase.sh`'s real stop conditions were nowhere close.
- **Not loop devices.** `losetup -a` showed exactly 1 active loop device at
  failure time, against a `max_loop` module parameter of 8 — nowhere near
  exhausted.
- **Not the kernel's global user-namespace limit.**
  `/proc/sys/user/max_user_namespaces` reported `50238` — vastly more than
  64.
- **Not the subuid/subgid pool size**, confirmed empirically as above
  (widening it 16x changed nothing).

### What it actually is

The error originates from podman/`containers-storage`'s "ID-mapped copy of
layer" mechanism — an optimization that uses the kernel's ID-mapped mounts
feature (`mount_setattr(MOUNT_ATTR_IDMAP)`) to let multiple `--userns=auto`
containers share the same underlying base image layer without a full
copy-on-write duplication per container. This is a distinct code path from
plain container spawning, and it's the thing that's actually failing here
— not container creation in general.

This is a real, if murky, class of podman/containers-storage behavior, not
specific to this host or this project's configuration. From public
reports of the same error class:

- [containers/podman discussion #20139](https://github.com/containers/podman/discussions/20139) —
  the closest match found: a user hit the identical error class (there, on
  `podman import`, triggered by a single file inside the image owned by a
  GID outside the available subordinate range). The discussion was never
  conclusively resolved upstream — the reporter's own summary after two
  weeks: *"I suspect that it either should have been filed as an issue, or
  nobody has any ideas."* A later commenter (months afterward) confirmed
  hitting the same thing with no fix either. The only workaround mentioned
  (`--storage-opt ignore_chown_errors=true`) applies to `podman import`
  specifically, not `container create`'s ID-mapped-copy path this project
  actually hit, and even its own reporter was unsure what it silently
  breaks.
- [containers/podman issue #12715](https://github.com/containers/podman/issues/12715) and
  [Red Hat Solution 7005221](https://access.redhat.com/solutions/7005221) —
  same error family on image pull; consistent with this being a known,
  recurring rough edge in how podman's rootless userns/idmap machinery
  behaves, not a one-off.

Given upstream itself hasn't nailed down a fix for the same error class,
further chasing this from the application/config side (this project has no
access to podman/containers-storage internals) has poor expected payoff.

### What this means for capacity planning

**On this host, with the currently-installed podman version, ~60 concurrent
`--userns=auto` containers is the real, binding ceiling — for any ticket,
not just SJN-01.** It's a property of container creation generally (the
ID-mapped layer copy path engages for every `--userns=auto` spawn sharing a
base layer), not of SJN-01's specific resource profile. CPT-01's own
staircase run may hit this same wall, independent of whatever memory/PSI
behavior it shows on its own.

This ceiling could plausibly move with a podman/containers-storage version
upgrade, or by disabling the ID-mapped-copy sharing optimization if a
config toggle for it exists and its performance/correctness trade-off is
understood — neither investigated here, since it's a genuinely separate
piece of work from ticket capacity planning and the upstream trail runs
cold. Worth a dedicated follow-up if headroom above ~60 concurrent real
assessments is ever actually needed.

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
