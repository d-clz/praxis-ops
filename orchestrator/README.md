# Praxis orchestrator

Takes a Runbook plus an `attempt_id` and drives a container runtime. Knows
nothing about tickets, stars, gates, or scoring -- that boundary is load
bearing, not a style choice (see `cmd/orchestrator/main.go`'s own header).
State lives on the runtime as labels; there is no database, so a restart
rebuilds the whole world from a label selector.

## Who calls this, and what it does and doesn't own

This is a backend service for another team's backend, not a public surface.
The **portal** -- login, the ticket pool, letting a candidate pick a ticket
and try to solve it -- is a separate team's deliverable, out of scope here by
design, not an unplanned gap. Its job is to hold candidate identity and
sessions, map a candidate to the one `attempt_id` they're allowed to touch,
and translate a browser terminal into calls against this API. This
orchestrator's job is to be a correct, tested surface for that team to
integrate against -- every route is `attempt_id`-scoped and deliberately
"dumb" (`/exec` runs whatever script it's given and reports the result
without knowing it's a check script; `/shell` relays raw bytes without
knowing it's a candidate typing) precisely so the portal can own all of the
policy and this can stay a mechanism.

Concretely: **"this orchestrator is finished" means safe for the portal team
to start integrating against, not safe to expose to a real candidate.**
Those are different milestones, on different sides of the same seam --
finishing this one doesn't imply the other is close, because the portal's
own login/session/authorization work hasn't started and isn't tracked here.

`/shell` isn't only for that future integration, either. It's equally a
direct operator debugging tool today -- anyone holding the shared token can
drop into a running sandbox by hand (`pxoctl api get`, or `curl` with
`X-Praxis-Token`), no portal involved at all. That dual purpose is
deliberate, not a stopgap standing in for the real thing.

## Build

```bash
go mod tidy      # only needed after touching go.mod, or on a fresh checkout
go build ./...
go vet ./...
go test -race ./...
```

Run as `praxis`, unprivileged -- building needs network egress to fetch Go
modules, which `praxis-sbx` (uid 1001) doesn't have at all
(`bootstrap/40-network-guard.sh` blocks it by uid, not by user identity, so
building as `praxis-sbx` would fail outright, not just be slower). `make
build` wraps the `go build` line above with the project's actual flags
(`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`).

One dependency gotcha worth knowing before it costs you an hour again:
`docker/docker@v25.0.5+incompatible`'s own client code imports
`github.com/pkg/errors` (a third-party package, package name also `errors` --
not stdlib) and calls `errors.As`/`errors.Is` on it. If that resolves to
`v0.8.1` or earlier (predates those functions), the build fails with
`undefined: errors.As` pointing at a file deep in the module cache, reading
exactly like a broken Go stdlib rather than an under-resolved transitive
dependency. `go.mod` now pins `github.com/pkg/errors v0.9.1`, which is why
this doesn't happen today -- don't let a future `go mod tidy` regress it
without noticing.

## Deploy and operate

See `deploy/README.md` for the full account model (short version: both the
orchestrator daemon and every sandbox it spawns run as `praxis-sbx`, never
`praxis` -- there is no third "orchestrator" identity), one-time setup, and
routine deploy (`sudo make deploy`).

Day-2 operations go through `pxoctl` (`deploy/pxoctl.sh`, installed to
`/usr/local/bin/pxoctl` by `make deploy`):

```bash
sudo pxoctl unit status|logs|start|stop|restart   # is the process alive
pxoctl api health                                  # is it answering correctly
sudo pxoctl api get <attempt_id>
sudo pxoctl hostmon status                         # the independent second view
pxoctl hostmon health
```

Check `unit` before `api` -- a failed unit doesn't need an API probe to
explain itself, and a running one can still be wedged or answering wrong.
`hostmon` (`internal/metrics`, `cmd/hostmon`, deployed separately via
`make deploy-hostmon`) is checked independently of both, on purpose --
staying in its own failure domain is the entire reason it exists. Full
detail in `deploy/README.md`.

## Layout

```
cmd/orchestrator/     main() -- wiring only, not unit tested on purpose
cmd/hostmon/           the independent second view: reads the podman socket
                        directly, never calls the orchestrator, never imports
                        internal/sandbox
cmd/mockorchestrator/  the real API surface against an in-memory backend --
                        no podman/host/root requirement, for a portal or
                        dashboard developer to build against
internal/sandbox/      Runbook, the Backend interface, the podman-backed
                        implementation (Create/Destroy/Reap/ExecScript/
                        ExecShell)
internal/mockbackend/  in-memory sandbox.Backend + metrics.Lister for
                        cmd/mockorchestrator -- not a test fixture, a real
                        runnable stand-in
internal/api/          HTTP surface: auth, routing, the exec/shell handlers
internal/metrics/      shared by both binaries -- label parsing, the
                        Prometheus-format registry, orphan classification
deploy/                .service units, README, pxoctl, this module's
                        Makefile targets that touch praxis-sbx
```

`internal/sandbox.Backend` is the seam the whole design leans on: `internal/
api` is tested entirely against a fake implementation (`internal/api/
server_test.go`), no podman socket required. That's what made the Phase C
shell-relay code (`ExecShell`, the `/shell` hijack handler) testable at all
before a real container was ever involved -- a real `net.Pipe()` standing in
for the container's PTY, a real TCP connection into `httptest.NewServer`
standing in for the portal, and the actual hijack/relay code in between.
`internal/mockbackend` leans on the exact same seam for a different purpose
-- a real, runnable binary another team can point a client at, not a
same-package test fixture.

## API contract and a mock for integration

`openapi.yaml` documents the real, already-shipped HTTP surface --
written as the contract other code gets built against, not retrofitted
after the fact. If it and the code ever disagree, the code is right and
the spec has drifted.

For a portal or dashboard developer who wants to build against that
contract without a live podman host:

```bash
go run ./cmd/mockorchestrator
# or: make build-mock && ./mockorchestrator
```

Defaults to `PRAXIS_ORCH_TOKEN=mock-token`, `127.0.0.1:8081`, and a
generous `PRAXIS_CAPACITY_WEIGHT=1000` so it's usable immediately --
override any of them the same way `cmd/orchestrator` reads its own env
vars. It serves the *identical* `api.New()`/`Routes()` production runs,
against `internal/mockbackend` instead of a real container runtime: real
create/get/destroy/exec, a real interactive shell (a raw line-echo PTY
stand-in, not a full terminal emulation) reachable through both `/shell`
and the real WebSocket `/shell/ws`, and real capacity/session accounting
(`/metrics`, `/sessions`) derived through the same `metrics.CollectManaged`
code path production uses, fed synthetic-but-correctly-labeled container
listings instead of a real docker socket's response.
