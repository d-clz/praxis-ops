#!/usr/bin/env bash
# preflight-ticket.sh -- the gate a ticket image must pass before it enters
# the catalog. Run as root.
#
#   sudo ./security/preflight-ticket.sh <local-image> [flags]
#     --systemd            add the capabilities/tmpfs a systemd tier needs
#     --memory <size>      default 512m (DefaultRunbook in runbook.go)
#     --pids <n>            default 256
#     --network <mode>     default none
#
# Flags are NOT auto-read from the ticket's own scenario.yaml -- pass the
# values from its `runtime:` block yourself. Parsing scenario.yaml belongs to
# the bake pipeline (still owed, docs/session-02-plan.md), and wiring a second
# copy of that logic here would just be two places to keep in sync. This
# script asks a narrower question than the bake pipeline will eventually
# answer: not "is this ticket correctly authored", but "does this image, run
# under its declared limits, actually stay contained".
#
# No CMD override, unlike bootstrap/50-verify.sh's use of praxis/ops-base:
# every ticket Containerfile already bakes its own (a sleep loop, an
# entrypoint script, or /sbin/init for the systemd tier). praxis/ops-base is
# the odd one out -- a bare debootstrap import with nothing baked in at all.
#
# 50-verify.sh only ever proves the base image. A ticket's own Containerfile
# layer -- seed.sh, an entrypoint script, an added capability, the systemd
# tier -- can each reintroduce something the base image never had. This is
# that check, per ticket, not once per host.
set -uo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER" 2>/dev/null || echo 0)"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HARDENING_CHECK="$REPO_ROOT/security/hardening-check.sh"

IMAGE="${1:-}"
[[ -z "$IMAGE" || "$IMAGE" == -* ]] && {
  echo "usage: sudo $0 <local-image> [--systemd] [--memory 512m] [--pids 256] [--network none]" >&2
  exit 1
}
shift

SYSTEMD=0
MEMORY="512m"
PIDS="256"
NETWORK="none"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --systemd) SYSTEMD=1; shift ;;
    --memory)  MEMORY="$2"; shift 2 ;;
    --pids)    PIDS="$2"; shift 2 ;;
    --network) NETWORK="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: run as root (sudo $0 $IMAGE ...)" >&2
  exit 2
fi

as_sbx() {
  ( cd / && runuser -u "$SBX_USER" -- env \
      XDG_RUNTIME_DIR="/run/user/$SBX_UID" \
      HOME="$SBX_HOME" \
      "$@" )
}

echo "=== preflight: $IMAGE ==="
echo "  network=$NETWORK memory=$MEMORY pids=$PIDS systemd=$SYSTEMD"

as_sbx podman image exists "$IMAGE" \
  || { echo "ERROR: $IMAGE not found in ${SBX_USER}'s store -- build it first" >&2; exit 1; }

NAME="praxis-preflight-$$"
RUN_ARGS=(--userns auto --network "$NETWORK" --memory "$MEMORY" --pids-limit "$PIDS"
          --cap-drop ALL --security-opt no-new-privileges)
if [[ "$SYSTEMD" -eq 1 ]]; then
  # Mirrors container.go's spec() for rb.Systemd -- unprivileged even here,
  # never --privileged. See internal/sandbox/container.go if these drift.
  RUN_ARGS+=(--cap-add SETUID --cap-add SETGID --cap-add CHOWN --cap-add KILL
             --cap-add DAC_OVERRIDE --cap-add SYS_ADMIN
             --tmpfs /run:size=64m --tmpfs /run/lock:size=8m)
else
  RUN_ARGS+=(--cap-add CHOWN --cap-add SETUID --cap-add SETGID
             --cap-add DAC_OVERRIDE --cap-add KILL)
fi

as_sbx podman run -d --rm --name "$NAME" "${RUN_ARGS[@]}" "$IMAGE" >/dev/null 2>&1 \
  || { echo "ERROR: could not spawn $IMAGE for preflight" >&2; exit 1; }

# systemd needs a moment to actually reach a state hardening-check.sh's
# checks are meaningful against; everything else is ready as soon as exec
# works.
[[ "$SYSTEMD" -eq 1 ]] && sleep 3

rc=1
if as_sbx podman exec -i "$NAME" bash -s < "$HARDENING_CHECK"; then
  echo
  echo "PASS -- $IMAGE clears the preflight gate"
  rc=0
else
  echo
  echo "FAIL -- do not publish $IMAGE until the FAIL lines above are resolved" >&2
fi

as_sbx podman rm -f "$NAME" >/dev/null 2>&1
exit $rc
