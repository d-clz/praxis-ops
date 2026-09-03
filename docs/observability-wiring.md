# Wiring — appending these files to the orchestrator

File placement, relative to `orchestrator/`:

```
docs/observability.md
docs/observability-wiring.md
internal/metrics/labels.go
internal/metrics/registry.go
internal/metrics/collect.go
internal/metrics/json.go
cmd/hostmon/main.go
deploy/praxis-hostmon.service
deploy/alerts.yml
bench/staircase.sh
```

No new module dependencies. `internal/metrics` uses stdlib plus the docker
client already in `go.mod`; the exposition format is emitted by hand rather than
pulling in `client_golang`, given the module proxy has already been unreachable
on this box once.

## 1. Reaper tick

The orchestrator's snapshot must be a by-product of the reaper's own list call.
Do not add a second poller — a metrics view that queries independently can
disagree with the view the reaper acts on, and then neither number means
anything.

```go
func (r *Reaper) tick(ctx context.Context) {
    start := time.Now()

    snap := metrics.CollectManaged(ctx, r.cli)
    if snap.Err != nil {
        r.reg.SetSnapshot(snap)   // publish the failure; a stale zero is worse
        return
    }

    now := time.Now()
    for _, s := range snap.Sessions {
        if s.Expired(now) {
            if err := r.destroy(ctx, s.ContainerID); err != nil {
                r.reg.IncDestroy("error")
                continue
            }
            r.reg.IncDestroy("ttl")
        }
    }

    r.reg.SetSnapshot(snap)
    r.reg.SetCapacity(metrics.WeightInFlight(snap), r.capacityLimit)
    r.reg.ObserveReaper(time.Since(start))   // success only — staleness is the alert
}
```

`ObserveReaper` is called only on a completed tick. That is deliberate:
`PraxisReaperStalled` fires on the *absence* of progress, which catches a
deadlock that an error counter would not.

## 2. Spawn path

```go
switch {
case err == nil:
    reg.IncSpawn("ok")
case errdefs.IsConflict(err):
    reg.IncSpawn("conflict")        // idempotency hit, not a failure
case errors.Is(err, ErrCapacity):
    reg.IncSpawn("denied_capacity") // admission worked as designed
default:
    reg.IncSpawn("error")
}
```

Keeping `conflict` and `denied_capacity` distinct from `error` matters: both are
the system behaving correctly, and folding them into a failure rate hides the
retry storm that `PraxisSpawnConflictSpike` is meant to catch.

## 3. Admission on weight, not count

`WeightInFlight` sums `praxis.weight` across non-exited sessions. For this to
mean anything, the spawn path must stamp the label from the Runbook:

```go
labels[metrics.LabelWeight] = strconv.Itoa(rb.Weight)   // default 1 if unset
```

CPT-01 (systemd + nginx + three faults) is several times SKN-01 (files and
grep). Admission that counts containers will over-admit on a heavy mix and
under-admit on a light one.

## 4. Listener

Metrics belong on the existing loopback API, not a new public port. Session
counts and runbook names describe live assessments.

```go
mux.Handle("/metrics",  metrics.Handler(reg))
mux.Handle("/sessions", metrics.SessionsHandler(reg))
```

`/sessions` is where `attempt_id` lives. It is deliberately absent from every
metric label except the two opt-in `praxis_session_*` gauges — attempt IDs in a
metric namespace grow cardinality without bound over the process lifetime.

## 5. Build and install hostmon

```bash
go build -ldflags "-X main.version=$(git describe --always --dirty)" \
  -o /opt/praxis/bin/hostmon ./cmd/hostmon

install -m 0644 deploy/praxis-hostmon.service \
  /home/praxis-sbx/.config/systemd/user/
sudo -u praxis-sbx systemctl --user daemon-reload
sudo -u praxis-sbx systemctl --user enable --now praxis-hostmon
```

Verify the two views agree on an idle box — both should report zero sessions and
zero orphans:

```bash
# Same authenticated port as the rest of the API, not a separate 9101 --
# see §4 above.
curl -s -H "X-Praxis-Token: $PRAXIS_ORCH_TOKEN" 127.0.0.1:8081/metrics \
  | grep -E 'praxis_(sessions_current|orphans)'
curl -s 127.0.0.1:9102/metrics | grep -E 'praxis_(sessions_current|orphans|containers_total)'
```

Then spawn one sandbox by hand and confirm both move together. If `view=host`
counts it and `view=orchestrator` does not, the label keys in `labels.go` do not
match what `internal/sandbox` stamps — fix that before trusting any alert here.

## 6. Prove the orphan detector before trusting it

The unmanaged-container alarm is the one piece of this that cannot be validated
by normal operation, because normal operation never produces its input. Inject
one:

```bash
sudo -u praxis-sbx podman run -d --name orphan-test \
  --label unrelated=1 praxis/ops-base sleep 3600
```

Within one poll interval, `praxis_orphans{view="host",kind="unmanaged"}` should
read 1 and the orchestrator view should be unchanged. Remove it afterwards. An
alert that has never fired on a known-positive is an assumption, not a control.

## 7. Known break point

Every list call uses the docker v25 surface (`types.ContainerListOptions`,
`[]types.Container`). If `go mod tidy` resolves to v26+, these move to
`container.ListOptions` and `container.Summary` — same fields, different import
path. This is the same break already flagged for the orchestrator; fix both in
one pass.
