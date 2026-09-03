#!/usr/bin/env bash
# 60-build-base.sh -- build praxis/ops-base from a debootstrapped rootfs. Run
# as root. Idempotent.
#
# There is nothing to `podman build --FROM`. With registries.conf blocking
# every public registry and 40-network-guard.sh denying all egress for
# praxis-sbx, the base rootfs has to come from outside podman entirely.
# debootstrap (running as root, which the egress ban never touches) plus
# `podman import` is the only path that keeps "this host pulls from nowhere"
# true -- see the three options weighed in docs/session-02-plan.md Phase A.
#
# Deliberately minimal package set: every package is attack surface, and every
# package is also a tool the candidate might need for the exercise. No
# curl/wget/git/compiler/package-manager at runtime -- with zero egress in the
# sandbox those only help an exfiltration attempt look plausible.
set -euo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER")"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"
IMAGE_NAME="praxis/ops-base"
BUILD_DIR="${PRAXIS_BUILD_DIR:-/root/praxis-build}"
ROOTFS="$BUILD_DIR/rootfs"
CACHE_DIR="${PRAXIS_DEB_CACHE:-/var/cache/praxis-debootstrap}"

PASS=0; FAIL=0
ok()  { printf '  OK    %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  FAIL  %s\n' "$*"; FAIL=$((FAIL+1)); }

as_sbx() {
  # Subshell cd to / first: runuser inherits the caller's working directory, and
  # praxis-sbx cannot traverse into /home/praxis. Same trap as 50-verify.sh.
  ( cd / && runuser -u "$SBX_USER" -- env \
      XDG_RUNTIME_DIR="/run/user/$SBX_UID" \
      HOME="$SBX_HOME" \
      "$@" )
}

if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: run as root (sudo $0)" >&2
  exit 2
fi

echo "=== building ${IMAGE_NAME} ==="

if as_sbx podman image exists "$IMAGE_NAME" 2>/dev/null && [[ "${PRAXIS_REBUILD:-0}" != "1" ]]; then
  echo "  ${IMAGE_NAME} already present -- skipping (set PRAXIS_REBUILD=1 to force a rebuild)"
  as_sbx podman images "$IMAGE_NAME"
  exit 0
fi

# --- debootstrap --------------------------------------------------------------
if ! command -v debootstrap >/dev/null 2>&1; then
  echo "  installing debootstrap"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends debootstrap
fi

# Pin the release explicitly. Inheriting VERSION_CODENAME from the build host
# makes the base image depend on which machine built it, which contradicts the
# digest-pinning determinism the whole ticket model rests on. Change this
# deliberately, in one place, when you want a different base.
RELEASE="${PRAXIS_BASE_RELEASE:-noble}"

# Default to the mirror the host already uses. The generic archive.ubuntu.com
# can stall for minutes on the first InRelease fetch from some regions -- which
# looks exactly like a hang and invites a Ctrl-C.
detect_mirror() {
  local m=""
  if [[ -f /etc/apt/sources.list.d/ubuntu.sources ]]; then
    m="$(awk '/^URIs:/{print $2; exit}' /etc/apt/sources.list.d/ubuntu.sources)"
  fi
  if [[ -z "$m" && -f /etc/apt/sources.list ]]; then
    m="$(awk '/^deb .*ubuntu/{print $2; exit}' /etc/apt/sources.list)"
  fi
  echo "${m:-http://archive.ubuntu.com/ubuntu}"
}
MIRROR="${PRAXIS_BASE_MIRROR:-$(detect_mirror)}"

ARCH="$(dpkg --print-architecture)"

# mawk, not gawk: matches what a stock Ubuntu install actually resolves
# /usr/bin/awk to. vim-tiny, not vim: same reasoning as the rest of this list.
#
# libcap2-bin (capsh) is here deliberately, not incidentally: it's
# Priority: optional, so --variant=minbase would otherwise skip it, and
# security/hardening-check.sh's caps.no_sys_module / caps.no_sys_ptrace checks
# call capsh directly. Without it those checks silently PASS because the
# command is missing, not because the capability is actually absent -- a
# false pass is worse than no check. findmnt needs nothing added here: it
# ships inside util-linux, which is Priority: required and always present
# regardless of --include.
PKGS="coreutils,findutils,grep,sed,mawk,less,vim-tiny,procps,psmisc,lsof,strace,python3-minimal,libcap2-bin"

rm -rf "$BUILD_DIR"
mkdir -p "$ROOTFS"

echo "-- debootstrap ${RELEASE} (${ARCH}) from ${MIRROR} --"
echo "   this takes 2-5 minutes and is quiet for long stretches; do not interrupt"
# --cache-dir persists across rebuilds. It lives outside BUILD_DIR on purpose:
# BUILD_DIR is wiped at the start and end of every run, so a cache inside it
# would be destroyed each time. First build fills it; later builds are local
# disk instead of network.
mkdir -p "$CACHE_DIR"
debootstrap --arch="$ARCH" --variant=minbase --include="$PKGS" \
  --cache-dir="$CACHE_DIR" \
  "$RELEASE" "$ROOTFS" "$MIRROR"

# --- candidate user and check-script drop-in ----------------------------------
# useradd's home-dir copy touches things that expect /proc to exist; bind-mount
# it for these two commands only, and make sure it comes back off even on error.
mount --bind /proc "$ROOTFS/proc"
trap 'umount "$ROOTFS/proc" 2>/dev/null || true' EXIT

chroot "$ROOTFS" useradd -m -u 1000 -s /bin/bash candidate
chroot "$ROOTFS" mkdir -p /opt/praxis

umount "$ROOTFS/proc"
trap - EXIT

# set -e catches a nonzero exit from either chroot command above, but that
# only fires if the chroot'd command itself reports failure -- confirm the
# actual filesystem state directly rather than trusting that alone. This is
# the check that would have caught /opt/praxis silently not existing before
# it turned into a "podman cp: no such directory" three steps and one script
# later.
[[ -d "$ROOTFS/opt/praxis" ]] || { echo "ERROR: /opt/praxis missing from rootfs after chroot mkdir" >&2; exit 1; }
grep -q '^candidate:' "$ROOTFS/etc/passwd" || { echo "ERROR: candidate user missing from rootfs after chroot useradd" >&2; exit 1; }

# Strip the network fetch history and caches: keeps the image small and leaves
# nothing in the layer describing how or from where it was built.
rm -rf "$ROOTFS"/var/lib/apt/lists/* "$ROOTFS"/var/cache/apt/archives/*.deb
rm -f "$ROOTFS"/root/.bash_history

# --- import: no FROM, no registry, ever ---------------------------------------
# Pipe straight into podman import. An intermediate tarball under /root cannot
# work: /root is 0700 root:root, so praxis-sbx cannot TRAVERSE into it no matter
# what mode the tarball itself carries. Piping sidesteps the filesystem entirely.
tar -C "$ROOTFS" -c . | as_sbx podman import - "$IMAGE_NAME" >/dev/null

rm -rf "$BUILD_DIR"

# --- done-when, per docs/session-02-plan.md Phase A ---------------------------
as_sbx podman image exists "$IMAGE_NAME" \
  && ok "podman images lists ${IMAGE_NAME}" \
  || bad "${IMAGE_NAME} not in ${SBX_USER}'s store"

out="$(as_sbx podman run --rm "$IMAGE_NAME" echo ok 2>&1)"
[[ "$out" == "ok" ]] \
  && ok "podman run --rm ${IMAGE_NAME} echo ok -> ok" \
  || bad "podman run produced '$out', expected ok"

echo
echo "pass=$PASS fail=$FAIL"
exit $(( FAIL > 0 ))
