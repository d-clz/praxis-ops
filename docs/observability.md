# Observability — monitoring, orphan reconciliation, capacity benchmark

Scope: what the orchestrator exposes, what the independent host monitor exposes,
how the two are compared, and how `PRAXIS_CAPACITY_WEIGHT` (Stage 4 -- replaced
the flat `PRAXIS_MAX_CONCURRENT`) stops being a guess.

---

## Assumptions -- resolved against the real codebase

These were inferred, not read from the codebase, when this doc was written.
All five have since been checked against what actually exists and either
confirmed or fixed; kept here as a record, not an open question list.

| Assumption | Resolution |
|---|---|
| Label keys `praxis.attempt_id`, `praxis.runbook`, `praxis.expires_at`, `praxis.spawned_at`, `praxis.weight` | **Wrong** -- real keys are hyphenated (`praxis.attempt-id`, `praxis.runbook-digest`, `praxis.expires-at`). `internal/metrics/labels.go` now references `internal/sandbox`'s own constants directly instead of a second hand-copied set, so this can't drift again. |
| `expires_at` / `spawned_at` serialised as RFC3339 | Confirmed correct. |
| Docker client is v25 (`types.ContainerListOptions`, `types.Container`) | Confirmed -- `go.mod` pins `v25.0.5+incompatible`. Breaks on v26+, unchanged risk, see note at bottom. |
| Sandbox cgroup slice path `/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/praxis.slice/praxis-sbx.slice` | Now backed by a real unit: `deploy/praxis-sbx.slice`, referenced via `internal/sandbox.SandboxSlice` and set as every spawned container's `CgroupParent`. Previously just an env default with nothing creating the path. |
| Spawn API is `POST {base}/v1/sandboxes` with `{"attempt_id","runbook"}` | **Wrong** -- real route is `POST {base}/instances` (port 8081, not 9100), auth is `X-Praxis-Token` not `Authorization: Bearer`, and `runbook` must be a fully inlined `Runbook` object, not a name (the orchestrator has no concept of a named runbook -- see `main.go`'s "knows nothing about tickets"). `bench/staircase.sh` now builds that object itself from the target ticket's own `scenario.yaml`. |

---

## 1. Two-view orphan model

Orphan detection is worthless if it is computed by the thing that creates the
orphans. So it is measured twice, by two processes that share no runtime state
and fail independently.

```
  view=orchestrator                     view=host
  ─────────────────                     ─────────
  praxis-orchestrator.service           praxis-hostmon.service
  lists WITH label filter               lists ALL containers, no filter
  what the reaper can actually see      ground truth from the socket
  :8081/metrics (authed)                :9102/metrics (no auth)
```

Set algebra, evaluated in Prometheus rather than in either process:

| Set | Meaning | Severity |
|---|---|---|
| `H \ O` — on host, not visible to orchestrator | **Unmanaged.** Missing or unparseable labels. The reaper will never touch it. Leaks until the box dies. | page |
| `O` with `expires_at < now` | **Unreaped.** Reaper sees it and isn't killing it. The backstop has failed. | page |
| `P \ H` — portal has a live attempt, no container | **Phantom.** OOM-killed or crashed; candidate is staring at a dead terminal. | ticket |
| slice procs > sum of container pids | **Escaped/leftover.** conmon or exec leftovers not attributable to any container. | page |

The orchestrator cannot report `H \ O` about itself — by construction it cannot
see those containers. That asymmetry is the whole reason for the second view.

The host monitor must never import `internal/sandbox` or call the orchestrator.
It reads the socket directly. If the orchestrator is deadlocked, hostmon is
still the thing that tells you.

---

## 2. Metric reference

Both exporters emit a `praxis_build_info` and a `praxis_scrape_error` so a dead
collector is distinguishable from a genuine zero.

### Orchestrator — `:8081/metrics` (same authenticated listener as the rest of the API, not a separate port -- `docs/observability-wiring.md` §4 is explicit that metrics belong on the existing loopback API, and `internal/api/server.go`'s `Routes()` follows that: `/metrics`/`/sessions` sit in the same mux as `/instances`, behind the same `X-Praxis-Token`)

```
praxis_sessions_current{view="orchestrator",state="created|running|exited"}
praxis_sessions_expiring_within{view="orchestrator",window="60s|300s|900s"}
praxis_sessions_expired_unreaped{view="orchestrator"}
praxis_oldest_expired_age_seconds{view="orchestrator"}
praxis_capacity_weight_used{view="orchestrator"}
praxis_capacity_weight_limit{view="orchestrator"}
praxis_spawn_total{result="ok|conflict|denied_capacity|error"}
praxis_destroy_total{reason="ttl|explicit|error"}
praxis_reaper_last_success_timestamp_seconds
praxis_reaper_duration_seconds
praxis_scrape_error{view="orchestrator"}
```

### Host monitor — `:9102/metrics`

```
praxis_sessions_current{view="host",state="..."}
praxis_containers_total{view="host"}              # every container on the socket
praxis_orphans{view="host",kind="unmanaged|unreaped|unparseable_label"}
praxis_oldest_expired_age_seconds{view="host"}
praxis_slice_procs{view="host"}
praxis_slice_memory_bytes{view="host"}
praxis_slice_memory_pressure_ratio{view="host",kind="some|full"}
praxis_storage_free_bytes{view="host"}            # the 24G loop volume
praxis_session_memory_bytes{...}                  # optional, PRAXIS_HOSTMON_STATS=1
praxis_session_pids{...}                          # optional
praxis_scrape_error{view="host"}
```

**Cardinality.** No metric carries `attempt_id` except the two opt-in
`praxis_session_*` gauges, which are off by default and bounded by the
number of concurrent sessions `PRAXIS_CAPACITY_WEIGHT` admits anyway. Per-session
detail for humans lives on `/sessions` (JSON), not in the metric namespace.

**Cost.** Both exporters cache. The orchestrator's snapshot is written by the
reaper tick — one list call serves both reaping and metrics, so scraping adds
zero socket traffic. Hostmon has its own poll interval (default 15s) and serves
the last snapshot regardless of scrape rate.

---

## 3. Alerts

In `deploy/alerts.yml`. The three that matter:

- **`PraxisUnmanagedContainers`** — `praxis_orphans{kind="unmanaged"} > 0` for
  5m. Something created a container outside the orchestrator's accounting.
- **`PraxisReaperStalled`** — `time() - praxis_reaper_last_success_timestamp_seconds
  > 3 × interval`. The TTL backstop is the load-bearing element of the design.
- **`PraxisViewDivergence`** — host count exceeds orchestrator count for 5m.
  Catches label corruption before it becomes a leak.

Leak-rate identity, asserted continuously:

```
increase(praxis_spawn_total{result="ok"}[1h])
  - increase(praxis_destroy_total[1h])
  - delta(praxis_sessions_current[1h])   ==  0
```

Non-zero means containers are appearing or vanishing outside the orchestrator's
knowledge. On a box shared with GitLab that is the failure that quietly eats
everything.

---

## 4. Capacity benchmark

`bench/staircase.sh`. Replaces the guessed `PRAXIS_CAPACITY_WEIGHT=2` with a measured number.

Method: spawn one weight-unit at a time, hold for `SOAK` seconds, sample, step
up. Abort on the first stop condition. Report the last step that held.

Stop conditions, checked every 5s:

| Condition | Default | Why |
|---|---|---|
| slice memory PSI `full avg60` | > 10% | real pressure, not utilisation |
| any OOM kill on the box | any | `memory.events` / dmesg |
| loop volume free | < 20% | writable layers; silent killer |
| spawn p95 latency | > 20s | admission, not steady state |
| GitLab health probe | fails | the neighbour's SLO is a stop condition |

Two numbers come out, and they are different limits:

- **Steady-state weight** — how much can be held at once.
- **Spawn rate** — how fast weight can be added. Image unpack on a loop device
  is bursty; 6 held is not 6 simultaneous.

Size on measured `memory.current` p95 during an actual solve, **not** on the
768m limit. The limit is an OOM ceiling; planning against it will
under-provision by a wide margin.

### Weighted admission

A flat count is the wrong unit — CPT-01 (systemd + nginx + three faults) is
several times SKN-01 (files and grep). Put `weight` in the Runbook, stamp it
onto `praxis.weight`, and have admission compare summed weight against
`PRAXIS_CAPACITY_WEIGHT`. Benchmark against the worst-case mix, not the average.

### Blast radius

Independent of the benchmark: every sandbox belongs to `praxis-sbx.slice` with
its own `MemoryMax`. Without it, overcommit hands the choice to the kernel OOM
killer, which scores GitLab at 6G as a far better victim than a 768m sandbox.

### Rate, from concurrency

```
concurrent = arrivals_per_min × mean_session_minutes
```

Hold 6 with a 25-minute mean solve → ~14 candidates/hour. Plan at ~70% of it;
arrivals are not smooth and the rejection is visible to a candidate mid-assessment.

---

## Note on the docker client version

Every list call here uses the v25 surface (`types.ContainerListOptions`,
`[]types.Container`). If `go mod tidy` resolves to v26+, these move to
`container.ListOptions` and `container.Summary` — same fields, different import.
That is the same break flagged for the orchestrator itself; fix both together.
