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

## Next: operator dashboard with an embedded shell

Not started. Decided to write the shape down now rather than build it
without alignment, since it has real forks.

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

### Decisions to make before writing code

1. **WebSocket implementation.** Hand-roll RFC 6455 framing, or pull in a
   small, established library (`github.com/coder/websocket`,
   `gorilla/websocket`). This project has deliberately hand-rolled things
   all session to dodge dependencies (the Prometheus exposition format
   specifically to avoid `client_golang`) -- but that was printing text
   lines; WS framing is security/correctness-sensitive binary protocol work
   (masking, fragmentation, ping/pong, close handshakes). A library is very
   likely the right call here despite the pattern, not a violation of it.
   Would be the first new Go dependency added this session.
2. **Where the dashboard lives.** Served by the orchestrator itself (new
   route, `embed`-ded static assets, one binary, nothing new to deploy) vs.
   a separate small service vs. a Claude Artifact. Leaning toward the
   orchestrator serving it directly -- matches the project's "one thing to
   deploy" pattern throughout, and sidesteps CORS/reachability questions a
   separate service (or an Artifact hosted on claude.ai, reaching into a
   private box) would introduce.
3. **Token-in-a-browser.** `X-Praxis-Token` as a header works cleanly for
   `curl`/`pxoctl`; a plain page navigation can't attach a header. Needs a
   deliberate answer (token in a query param on first load then moved into
   `sessionStorage`, a login form, something else) rather than accidentally
   leaking it into browser history or a `Referer` header.

Not scoped further than this until those three are settled.
