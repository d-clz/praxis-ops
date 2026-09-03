# Security checks

Three scripts, three different questions, run at three different cadences.

| Script | Question | Run when |
|---|---|---|
| `hardening-check.sh` | Does *this running container* actually hold containment? | Inside every spawned container -- called by the other two, not usually by hand |
| `preflight-ticket.sh` | Is *this ticket image* safe to publish? | Once per ticket image, before it enters the catalog, and after any change to its `Containerfile`/`seed.sh`/entrypoint |
| `verify-shell-isolation.sh` | Does the Phase C shell-access primitive (PTY exec, exec-by-name isolation) still hold? | Once per host, after Phase A/B, and again after any change to how shell access is wired |

`bootstrap/50-verify.sh` also calls `hardening-check.sh`, but only ever
against `praxis/ops-base` -- it proves the base image, not any ticket built
on top of it. That's exactly the gap `preflight-ticket.sh` closes: a ticket's
own `Containerfile` layer (`seed.sh`, an entrypoint script, an added
capability, the systemd tier) can each reintroduce something the base image
never had. Base image containment and ticket image containment are separate
claims -- proving one is not proving the other.

## Publishing a ticket image

```bash
# 1. Build it into praxis-sbx's store (as praxis-sbx, or via whatever the
#    eventual bake pipeline does -- podman build itself is unaffected by this
#    checklist).

# 2. Run the gate, with the flags matching the ticket's own scenario.yaml
#    `runtime:` block:
sudo ./security/preflight-ticket.sh <image> \
  --network none --memory 512m --pids 256          # SJN-01 / SKN-01 shape

sudo ./security/preflight-ticket.sh <image> \
  --systemd --network none --memory 768m --pids 384 # CPT-01 shape

# 3. PASS means every hardening-check.sh property held from inside a
#    container run under those exact limits. FAIL means do not publish --
#    fix the image, not the check.
```

`preflight-ticket.sh`'s flags are not auto-read from `scenario.yaml` --
pass the values yourself. That parsing belongs to the bake pipeline (still
owed, `docs/session-02-plan.md`); wiring a second copy of that logic here
would just be two places to keep in sync, and this script is asking a
narrower question anyway: not "is this ticket correctly authored," but "does
this image, run under its declared limits, actually stay contained."

## A SKIP is not a PASS

`hardening-check.sh` reports `SKIP`, not a silent `PASS`, for a property it
couldn't test (`capsh`/`findmnt` missing, no `/proc/self/uid_map`). Do not
treat a clean run with SKIPs as equivalent to one with none -- a SKIPped
property is untested, not held. If a ticket image is missing a tool
`hardening-check.sh` needs, that belongs on `bootstrap/60-build-base.sh`'s
package list (fixing it for every image at once), not papered over per-ticket.
