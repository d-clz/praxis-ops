# Praxis — Ops Substrate Class (POC)

Extends the Ticket & Test Factory guide for **live-machine troubleshooting** tickets
(SadServers shape). Code-substrate rules in §2–§4 of the Authoring Guide do **not**
apply; this document is normative for `substrate_class: ops`.

Status: **POC / pending ADR 0007.** Per the Phase 2 Hard Gate, no ops Ticket Bundle
merges to the main catalog until that ADR is accepted. Everything here is proposal.

---

## 1. What changes vs code-substrate tickets

| Concern | Code substrate | Ops substrate |
|---|---|---|
| Unit of work | source tree + fault injection | live container + seeded broken state |
| Graded object | code diff | **machine state at submit time** |
| Oracle | hidden test suite | `check.sh` (server-side, never shipped) |
| Correct fix | 3 structural code variants | 3 remediation **paths** (different tooling) |
| Public test | substrate suite (Gate B) | **none** — replaced by invariants |
| Anti-cheat | sabotage markers + hidden table | **invariant assertions** |

### 1.1 New delivery mode

The `delivery` enum gains a third value:

```
browser_workspace | git_haystack | live_sandbox
```

`live_sandbox` = candidate gets a shell into a running container. No repo, no IDE.
Do not alias (`shell`, `terminal`, `box`).

### 1.2 Substrate pin

There is no `substrate_commit`. Ops tickets pin an **image digest**:

```yaml
substrate_image: registry.local/praxis/ops-base@sha256:...
```

Built by the bake pipeline. Never a tag. `git clone` at spawn is forbidden —
it breaks determinism and requires egress the sandbox does not have.

---

## 2. Gate remap

| Gate | Code substrate | Ops substrate |
|---|---|---|
| **A** | Hidden FAIL on injected code | `check.sh` exits **non-zero** on a freshly spawned box |
| **B** | Public PASS with Target | *(dropped)* — replaced by **Gate S** |
| **S** | — | **Sanity**: box reaches ready, unrelated services healthy, symptom observable |
| **C** | Hidden PASS on fix-a | `check.sh` exits **0** after `fix-a.sh` |
| **D** | fix-a/b/c all PASS | all three remediation scripts pass |
| **E** | Sabotage FAIL | each sabotage script leaves `check.sh` **non-zero** |
| **I** | — | **Invariant**: each sabotage fails on `invariants`, not only on `solved` |

Gate I is the one that matters. If a sabotage fails only because the `solved`
assertion is wrong, the check is testing the answer, not the method — and a
destructive shortcut will eventually pass it.

---

## 3. Check contract

`.grading/<KEY>/check.sh` runs **inside** the sandbox via the orchestrator, as root,
after the candidate submits. It is never present in the image the candidate sees;
the orchestrator copies it in at grade time.

It MUST emit a single JSON object on stdout and nothing else:

```json
{
  "schema": "praxis.check/v1",
  "ticket_key": "SKN-01",
  "solved": [
    {"id": "answer_correct", "ok": true,  "detail": "..."}
  ],
  "invariants": [
    {"id": "log_intact",     "ok": true,  "detail": "..."}
  ]
}
```

Exit code MUST be `0` only if every `solved` and every `invariant` entry is `ok`.
Any other outcome exits `1`. Exit `2` is reserved for check-harness failure
(missing tool, malformed state) — the orchestrator treats `2` as *inconclusive*,
never as a candidate failure.

### 3.1 Writing assertions

- **`solved`** — the observable the ticket brief promised in `## Definition of Done`.
  One assertion per numbered DoD item. No more.
- **`invariants`** — everything the candidate must NOT have destroyed to get there.
  Minimum set for every ops ticket:
  1. evidence preserved (the log/file/data the ticket is about still exists, with
     its pre-existing content intact — pin by sha256 of a stable prefix)
  2. no collateral service kill (anything running at spawn that is unrelated to the
     fault is still running)
  3. no check tampering (`/opt/praxis` untouched, host clock sane)

Invariants are authored **against the sabotage list**. Write the sabotage first,
then the invariant that catches it. A sabotage with no matching invariant means
the ticket is gameable.

### 3.2 Timing-sensitive checks

Growth/leak tickets need a settle window. Sample twice with a fixed sleep, never a
single reading. Keep the window in the check, not in the orchestrator, so the
grade is reproducible from the artifact alone.

---

## 4. Bundle shape

```
tickets/<KEY>/
  scenario.yaml        # metadata, runtime envelope, brief   (candidate-safe)
  Containerfile        # derives from substrate_image
  seed.sh              # injects the broken state at build time
  fixes.yaml           # remediation paths + sabotage         (server-only)
.grading/<KEY>/
  check.sh             # the oracle                           (server-only)
  fix-a.sh fix-b.sh fix-c.sh
  sabotage-a.sh ...
```

`seed.sh` runs at **build** time, not spawn time. Bake the broken state into the
image so every spawn is byte-identical and the fault cannot drift.

Anything under `.grading/` is answer-key material. It never enters the image the
candidate is handed, and the candidate never receives registry pull credentials.

---

## 5. Star bands (ops)

| ★ | Shape | Targets | Root needed | systemd | Example |
|---|---|---|---|---|---|
| 1 | single command, guided | 1 | no | no | find a string in one file |
| 2 | data extraction at scale | 1 | no | no | SKN-01 |
| 3 | discovery — find the actor | 1–2 | yes | no | SJN-01 |
| 4 | layered service repair | 3+ | yes | **yes** | CPT-01 |
| 5 | multi-service / cascading | multiple | yes | yes | *(deferred)* |

Vagueness rules from Authoring Guide §2 carry over unchanged — ★4 states symptom +
repro + expected, and must not name the file or unit to fix.

---

## 6. Runtime envelope

Every ops ticket declares what it needs. The orchestrator refuses to spawn a ticket
whose envelope exceeds the box budget.

```yaml
runtime:
  systemd: false          # true forces --systemd=always + cgroup v2 delegation
  root_in_sandbox: true   # root inside userns; never host root
  network: none           # none | internal ; egress is never permitted
  memory: 512m
  cpus: "1.0"
  pids_limit: 256
  ttl_seconds: 3600
  tmpfs: ["/tmp:size=64m"]
```

`network: none` is the default and correct for every ticket in this POC. A ticket
that needs a second container gets `internal` and a per-attempt bridge — never the
default bridge, which can reach GitLab on the host.

---

## 7. Authoring loop

0. Confirm `substrate_image` digest is pinned and pullable offline.
1. Write the brief and `## Definition of Done` first. One DoD item = one `solved`
   assertion. If you cannot write the assertion, the DoD item is not observable —
   rewrite it.
2. Write `seed.sh`. Verify the symptom by hand in a spawned container.
3. Write the sabotage scripts **before** the fixes. These are the shortcuts you
   expect: delete the evidence, mask the unit, bypass the service, freeze the file.
4. Write `check.sh` so every sabotage trips an invariant.
5. Write three remediation paths using different tooling. If all three use the same
   command, the ticket tests recall, not troubleshooting.
6. Run the gate runner. Gates A, S, C, D, E, I must all pass.
7. Capture incident artifacts (Authoring Guide §3.0.3 rules apply — captured, never
   hand-authored).
