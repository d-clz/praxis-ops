#!/usr/bin/env bash
# verify-shell-isolation.sh -- prove the Phase C primitive before the
# orchestrator can run it (docs/session-02-plan.md Phase C, Option 1). Run as
# root; drops into SBX_USER for every podman call, same as 50-verify.sh.
#
# What this proves:
#   1. a PTY `podman exec` genuinely delivers an interactive shell, as an
#      unprivileged candidate-equivalent user, not just a batch command
#   2. exec-by-name is exact and exclusive -- an attempt_id can only ever
#      reach the one container SandboxName() derives from it, never another
#      attempt's, and a name that names nothing fails closed
#
# What this does NOT prove: that a candidate can only ever learn their own
# attempt_id. That mapping lives in the portal, which does not exist yet
# (docs/session-01-hardening.md). This script is the half of Phase C's "Done
# when" that's provable without one -- the orchestrator's own /shell endpoint
# trusts whatever attempt_id it's handed, exactly like /exec and DELETE
# already do; nothing new to prove there once this half holds.
set -uo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER" 2>/dev/null || echo 0)"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"
IMAGE="${PRAXIS_VERIFY_IMAGE:-praxis/ops-base}"

PASS=0; FAIL=0
ok()  { printf '  OK    %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  FAIL  %s\n' "$*"; FAIL=$((FAIL+1)); }

as_sbx() {
  ( cd / && runuser -u "$SBX_USER" -- env \
      XDG_RUNTIME_DIR="/run/user/$SBX_UID" \
      HOME="$SBX_HOME" \
      "$@" )
}

if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: run as root (sudo $0)" >&2
  exit 2
fi

echo "=== phase C: shell access primitive ==="

# Same NamePrefix + attempt_id scheme orchestrator/internal/sandbox/backend.go
# uses (SandboxName), so this exercises the real naming contract, not a stand-in.
NAME_A="sbx-praxis-verify-a-$$"
NAME_B="sbx-praxis-verify-b-$$"
cleanup() {
  as_sbx podman rm -f "$NAME_A" "$NAME_B" >/dev/null 2>&1
}
trap cleanup EXIT

as_sbx podman run -d --rm --name "$NAME_A" --userns auto \
  --network none --memory 256m --pids-limit 64 \
  --cap-drop ALL --security-opt no-new-privileges \
  "$IMAGE" sleep 300 >/dev/null 2>&1 \
  && ok "spawned attempt A ($NAME_A)" || bad "could not spawn attempt A"
as_sbx podman run -d --rm --name "$NAME_B" --userns auto \
  --network none --memory 256m --pids-limit 64 \
  --cap-drop ALL --security-opt no-new-privileges \
  "$IMAGE" sleep 300 >/dev/null 2>&1 \
  && ok "spawned attempt B ($NAME_B)" || bad "could not spawn attempt B"

# --- 1. PTY exec actually delivers an interactive shell, as candidate -------
# -t allocates the pty; without it `tty` would report "not a tty" even on a
# correctly configured exec, so this only proves anything with -t present.
tty_out="$(as_sbx podman exec -t "$NAME_A" sh -c 'tty' 2>&1 | tr -d '\r\n')"
case "$tty_out" in
  /dev/pts/*) ok "PTY exec allocates a real terminal ($tty_out)" ;;
  *)          bad "PTY exec did not allocate a terminal (got '$tty_out')" ;;
esac

who="$(as_sbx podman exec -t -u candidate "$NAME_A" sh -c 'whoami' 2>&1 | tr -d '\r\n')"
[[ "$who" == "candidate" ]] \
  && ok "exec -u candidate lands as candidate, not root" \
  || bad "exec -u candidate produced '$who'"

# --- 2. exec-by-name is exact and exclusive ---------------------------------
# Write a marker only attempt A's own filesystem should ever see.
as_sbx podman exec "$NAME_A" sh -c 'echo attempt-a-secret > /tmp/marker' >/dev/null 2>&1

marker_a="$(as_sbx podman exec "$NAME_A" cat /tmp/marker 2>/dev/null | tr -d '\r\n')"
[[ "$marker_a" == "attempt-a-secret" ]] \
  && ok "attempt A can read its own marker" \
  || bad "attempt A cannot read back its own marker (exec itself is broken)"

# The actual isolation claim: naming attempt B never reaches attempt A's
# filesystem, and vice versa -- there is no shared mount, no fallback name
# resolution, nothing to leak across.
if as_sbx podman exec "$NAME_B" sh -c 'test -f /tmp/marker' >/dev/null 2>&1; then
  bad "attempt B can see attempt A's marker -- containers are not isolated"
else
  ok "attempt B cannot see attempt A's marker -- exec-by-name does not cross containers"
fi

# A name that names nothing must fail closed, not fall back to something.
if as_sbx podman exec "sbx-praxis-verify-nonexistent-$$" sh -c 'true' >/dev/null 2>&1; then
  bad "exec against a nonexistent attempt name succeeded"
else
  ok "exec against a nonexistent attempt name fails closed"
fi

echo
echo "pass=$PASS fail=$FAIL"
if [[ $FAIL -eq 0 ]]; then
  echo "shell-access primitive holds -- safe for the orchestrator to proxy podman exec"
else
  echo "DO NOT wire shell access to the orchestrator until the FAIL lines are resolved" >&2
fi
exit $(( FAIL > 0 ))
