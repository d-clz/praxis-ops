#!/usr/bin/env bash
# 20-spawnbox-user.sh -- the dedicated Linux user that owns the sandbox runtime.
# Run as root. Idempotent.
#
# ONE user owns both the podman socket and the orchestrator process. Splitting
# those into two users adds a group ACL on the socket without adding a boundary --
# the socket IS the credential, and whoever holds it can create containers.
#
# The boundary that matters is:
#     praxis-portal  --(127.0.0.1 + token)-->  praxis-sbx  --(socket)-->  podman
# The portal user has no filesystem path to the socket at all.
set -euo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
# 500000 is well clear of Ubuntu's auto-allocation band, which starts at
# 100000 and hands out 65536 per user. A default inside that band collides
# with whatever useradd assigns to the NEXT account created on this box.
SUBID_START="${SUBID_START:-500000}"
SUBID_COUNT="${SUBID_COUNT:-65536}"

echo "=== provisioning ${SBX_USER} ==="

# --- 1. the account ----------------------------------------------------------
# No shell, no SSH key, no password. Everything runs via systemd user units,
# which is why lingering (step 4) is mandatory rather than convenient.
if ! id "$SBX_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /usr/sbin/nologin --user-group "$SBX_USER"
  echo "  created $SBX_USER"
else
  echo "  $SBX_USER already exists"
fi
passwd -l "$SBX_USER" >/dev/null

SBX_UID="$(id -u "$SBX_USER")"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"

# --- 2. filesystem isolation -------------------------------------------------
# Ubuntu's HOME_MODE default is 0750 -- group-readable. Not good enough here:
# ~/.local/share/containers holds the fault-injected images, which ARE the answer
# key. A readable image layer means a ticket solvable by extraction rather than
# by troubleshooting.
chmod 0700 "$SBX_HOME"
chown "$SBX_USER:$SBX_USER" "$SBX_HOME"
install -d -o "$SBX_USER" -g "$SBX_USER" -m 0700 \
  "$SBX_HOME/.config" \
  "$SBX_HOME/.config/systemd/user" \
  "$SBX_HOME/.config/containers" \
  "$SBX_HOME/.config/praxis" \
  "$SBX_HOME/.local" \
  "$SBX_HOME/.local/share" \
  "$SBX_HOME/bin"
stat -c '  %n mode=%a owner=%U:%G' "$SBX_HOME"

if [[ "$(stat -c %G "$SBX_HOME")" != "$SBX_USER" ]]; then
  echo "FAIL: $SBX_HOME group is not $SBX_USER -- another user could read the image store" >&2
  exit 1
fi

# --- 3. subordinate id ranges ------------------------------------------------
# An escaped container lands on one of these UIDs, which owns nothing on the box.
# Overlap with another user's range would let an escape inherit that user's file
# ownership -- so the overlap check below is a real control, not hygiene.
add_subid() {
  local file="$1"
  if grep -q "^${SBX_USER}:" "$file"; then
    echo "  $file already assigns ${SBX_USER}: $(grep "^${SBX_USER}:" "$file" | tr '\n' ' ')"
  else
    echo "${SBX_USER}:${SUBID_START}:${SUBID_COUNT}" >> "$file"
    echo "  $file += ${SBX_USER}:${SUBID_START}:${SUBID_COUNT}"
  fi
}
add_subid /etc/subuid
add_subid /etc/subgid

# Check the range that is ACTUALLY in effect, not the one we intended to write.
# useradd auto-assigns a range when /etc/subuid exists, so the effective range is
# frequently not $SUBID_START -- validating the intended value would prove nothing.
check_overlap() {
  local file="$1"
  local eff
  eff="$(grep "^${SBX_USER}:" "$file" | head -1)"
  [[ -n "$eff" ]] || { echo "FAIL: no range for ${SBX_USER} in $file" >&2; return 1; }
  local lo="${eff#*:}"; lo="${lo%%:*}"
  local cnt="${eff##*:}"
  echo "  $file effective range: ${lo}-$((lo + cnt - 1))"
  awk -F: -v me="$SBX_USER" -v mylo="$lo" -v mycnt="$cnt" '
    $1 != me {
      lo=$2; hi=$2+$3-1; myhi=mylo+mycnt-1
      if (lo <= myhi && hi >= mylo)
        { printf "  FAIL overlap with user %s (%d-%d)\n", $1, lo, hi; bad=1 }
    }
    END { exit bad?1:0 }
  ' "$file" || return 1
  return 0
}
echo "  checking for overlapping ranges..."
check_overlap /etc/subuid || { echo "resolve the overlap before continuing" >&2; exit 1; }
check_overlap /etc/subgid || { echo "resolve the overlap before continuing" >&2; exit 1; }
echo "  no overlap"

# --- 4. lingering ------------------------------------------------------------
# Without this the user's systemd instance dies on logout, taking the podman
# socket AND the TTL reaper with it -- sandboxes would then run until reboot.
loginctl enable-linger "$SBX_USER"
echo "  lingering enabled"

# XDG_RUNTIME_DIR is created 0700 by systemd, which is what protects the podman
# socket from the portal user. Verify rather than assume.
for _ in $(seq 1 10); do [[ -d "/run/user/$SBX_UID" ]] && break; sleep 1; done
if [[ -d "/run/user/$SBX_UID" ]]; then
  stat -c '  %n mode=%a owner=%U' "/run/user/$SBX_UID"
  [[ "$(stat -c %a "/run/user/$SBX_UID")" == "700" ]] \
    || echo "  WARN: /run/user/$SBX_UID is not 0700 -- socket is exposed"
else
  echo "  WARN: /run/user/$SBX_UID not created yet; re-check after a reboot"
fi

# --- 5. capacity -------------------------------------------------------------
# Linux permissions separate ACCESS, not CAPACITY. The sandbox user cannot read a
# single GitLab file, and can still take GitLab down by filling the shared
# filesystem with image layers. Permissions do not help with this at all.
if findmnt -no TARGET "$SBX_HOME" >/dev/null 2>&1; then
  echo "  OK: $SBX_HOME is a separate mount -- capacity is bounded"
else
  cat <<CAP
  WARN: $SBX_HOME shares a filesystem with the rest of the host.
        Bound it before running real attempts. Pick one:
          (a) separate LVM volume mounted at $SBX_HOME    <- simplest hard ceiling
          (b) XFS project quota on $SBX_HOME/.local/share/containers
          (c) ext4 quota: setquota -u $SBX_USER 20G 25G 0 0 /
CAP
  df -h / | tail -1 | sed 's/^/        /'
fi

echo
echo "done -- continue with 30-podman-policy.sh"
