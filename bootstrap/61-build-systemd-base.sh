#!/usr/bin/env bash
# 61-build-systemd-base.sh -- build praxis/ops-systemd from a debootstrapped
# rootfs. Run as root. Idempotent.
#
# Same debootstrap+import path as 60-build-base.sh, same reason: no `podman
# build --FROM` possible with registries.conf blocking every public registry
# and 40-network-guard.sh denying all egress for praxis-sbx. This is the base
# CPT-01 needs -- systemd as PID 1, container.go's `rb.Systemd` branch
# (CapAdd SETUID/SETGID/CHOWN/KILL/DAC_OVERRIDE/SYS_ADMIN, private cgroupns,
# /run + /run/lock tmpfs, SIGRTMIN+3) exists and is written, but has never
# run against a real container -- this script only produces the rootfs, it
# does not verify systemd actually comes up as PID 1. That verification is a
# real spawn through the orchestrator (docs/session-0X plan, Stage 2.5/4),
# not something a chroot can prove.
#
# Package list beyond ops-base's own is intentionally short and expect
# iteration: this is the first systemd-under-rootless-podman image this
# project has built. Not adding dbus -- systemctl talks to PID 1 over
# systemd's own private socket (/run/systemd/private), not the system
# message bus. Add it only if a real spawn proves systemctl actually needs
# it; do not add it on a guess.
set -euo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER")"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"
IMAGE_NAME="praxis/ops-systemd"
BUILD_DIR="${PRAXIS_BUILD_DIR:-/root/praxis-build-systemd}"
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

# Same release pin as ops-base, same reason: the image must not depend on
# which machine built it.
RELEASE="${PRAXIS_BASE_RELEASE:-noble}"

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

# ops-base's package list plus what CPT-01 actually needs:
#   systemd, systemd-sysv -- PID 1. systemd-sysv provides the /sbin/init
#     symlink and is what --variant=minbase otherwise has no reason to pull
#     in (minbase's own seed doesn't assume an init system at all).
#   nginx-light            -- CPT-01's seed.sh only needs a static document
#     root behind one sites-available vhost; the light variant covers that
#     without the extra modules the full nginx package carries.
PKGS="coreutils,findutils,grep,sed,mawk,less,vim-tiny,procps,psmisc,lsof,strace,python3-minimal,libcap2-bin,systemd,systemd-sysv,nginx-light"

rm -rf "$BUILD_DIR"
mkdir -p "$ROOTFS"

echo "-- debootstrap ${RELEASE} (${ARCH}) from ${MIRROR} --"
echo "   this takes 2-5 minutes and is quiet for long stretches; do not interrupt"
mkdir -p "$CACHE_DIR"

# Same IPv6-timeout guard as 60-build-base.sh -- see that script's own
# comment for why this is scoped to wget's config and not host networking.
WGETRC_BAK=""
if [[ -f /etc/wgetrc ]]; then
  WGETRC_BAK="$(mktemp)"
  cp /etc/wgetrc "$WGETRC_BAK"
fi
restore_wgetrc() {
  if [[ -n "$WGETRC_BAK" ]]; then
    mv "$WGETRC_BAK" /etc/wgetrc
  else
    rm -f /etc/wgetrc
  fi
}
trap restore_wgetrc EXIT
{ [[ -f /etc/wgetrc ]] && cat /etc/wgetrc; echo "inet4-only = on"; } > /etc/wgetrc.praxis-tmp
mv /etc/wgetrc.praxis-tmp /etc/wgetrc

debootstrap --arch="$ARCH" --variant=minbase --include="$PKGS" \
  --cache-dir="$CACHE_DIR" \
  "$RELEASE" "$ROOTFS" "$MIRROR"

restore_wgetrc
trap - EXIT

# --- candidate user and check-script drop-in ----------------------------------
# Same /proc bind-mount trap as 60-build-base.sh -- useradd's home-dir copy
# touches things that expect /proc to exist.
mount --bind /proc "$ROOTFS/proc"
trap 'umount "$ROOTFS/proc" 2>/dev/null || true' EXIT

chroot "$ROOTFS" useradd -m -u 1000 -s /bin/bash candidate
chroot "$ROOTFS" mkdir -p /opt/praxis

umount "$ROOTFS/proc"
trap - EXIT

[[ -d "$ROOTFS/opt/praxis" ]] || { echo "ERROR: /opt/praxis missing from rootfs after chroot mkdir" >&2; exit 1; }
grep -q '^candidate:' "$ROOTFS/etc/passwd" || { echo "ERROR: candidate user missing from rootfs after chroot useradd" >&2; exit 1; }

# Strip fetch history and caches, same as ops-base.
rm -rf "$ROOTFS"/var/lib/apt/lists/* "$ROOTFS"/var/cache/apt/archives/*.deb
rm -f "$ROOTFS"/root/.bash_history

# --- import: no FROM, no registry, ever ---------------------------------------
tar -C "$ROOTFS" -c . | as_sbx podman import - "$IMAGE_NAME" >/dev/null

rm -rf "$BUILD_DIR"

# --- done-when ------------------------------------------------------------
# These checks are structural only: files and binaries present in the image.
# They CANNOT confirm systemd actually runs as PID 1 -- a chroot has no PID
# namespace of its own to test that in. The real test is a live spawn
# through the orchestrator (see this script's header comment).
as_sbx podman image exists "$IMAGE_NAME" \
  && ok "podman images lists ${IMAGE_NAME}" \
  || bad "${IMAGE_NAME} not in ${SBX_USER}'s store"

out="$(as_sbx podman run --rm "$IMAGE_NAME" echo ok 2>&1)"
[[ "$out" == "ok" ]] \
  && ok "podman run --rm ${IMAGE_NAME} echo ok -> ok" \
  || bad "podman run produced '$out', expected ok"

init_link="$(as_sbx podman run --rm "$IMAGE_NAME" readlink -f /sbin/init 2>&1)"
[[ "$init_link" == *systemd* ]] \
  && ok "/sbin/init -> $init_link" \
  || bad "/sbin/init does not resolve to systemd (got '$init_link') -- systemd-sysv did not win the /sbin/init symlink"

nginx_check="$(as_sbx podman run --rm "$IMAGE_NAME" sh -c 'command -v nginx' 2>&1)"
[[ -n "$nginx_check" ]] \
  && ok "nginx present at $nginx_check" \
  || bad "nginx binary not found in ${IMAGE_NAME}"

echo
echo "pass=$PASS fail=$FAIL"
echo
echo "Structural checks only. Before building any ticket on this image, run"
echo "the real containment + live-spawn verification:"
echo "  sudo PRAXIS_VERIFY_IMAGE=${IMAGE_NAME} ./bootstrap/50-verify.sh"
echo "  sudo ./security/hardening-check.sh"
echo "  sudo ./security/verify-shell-isolation.sh"
exit $(( FAIL > 0 ))
