#!/usr/bin/env bash
# 10-install-podman.sh -- install the rootless container stack. Run as root.
# Idempotent.
#
# Ubuntu 26.04 ships podman 5.7.x in universe. No PPA, no third-party repo. The
# old Kubic/OBS repository that older guides reference was discontinued in 2023
# and must not be used.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

echo "=== installing rootless container stack ==="

apt-get update -qq
apt-get install -y --no-install-recommends \
  podman \
  uidmap \
  passt \
  fuse-overlayfs \
  crun \
  netavark \
  aardvark-dns \
  slirp4netns \
  nftables

podman --version

# --- host sysctls ------------------------------------------------------------
# Persisted, not just set for this boot.
install -d -m 0755 /etc/sysctl.d
cat > /etc/sysctl.d/80-praxis-rootless.conf <<'EOF'
# Praxis sandbox host.
#
# Deliberately minimal. Two earlier entries were removed:
#   user.max_user_namespaces   -- pinning it LOWERED the kernel default on a
#                                 box that already had far more headroom than
#                                 rootless containers need.
#   net.ipv4.ping_group_range  -- a host-wide loosening for no gain: with
#                                 network=none and zero egress, ping reaches
#                                 loopback and nothing else.

# systemd inside a container consumes inotify instances, and Ubuntu's default of
# 128 per UID is tight once several sandboxes run at once. Both limits are
# per-UID, so raising them for everyone cannot let a sandbox starve GitLab's
# file watchers -- praxis-sbx has its own budget either way.
fs.inotify.max_user_instances = 1024
fs.inotify.max_user_watches = 524288
EOF
sysctl --system >/dev/null
echo "  sysctls applied"

# --- AppArmor userns restriction ---------------------------------------------
# Ubuntu 24.04+ restricts unprivileged user namespaces. Ubuntu ALREADY ships an
# AppArmor profile for podman plus the `unprivileged_userns` transition target
# that handles this correctly.
#
# Do NOT install a competing profile for /usr/bin/podman. Two profiles attached
# to the same executable produce "conflicting profile attachments", and podman's
# rootless re-exec then fails with "failed to reexec: Permission denied" after a
# "Failed name lookup - disconnected path" denial on /proc/self/exe. An earlier
# version of this script did exactly that; the profile has been removed.
if [[ -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]] \
   && [[ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns)" != "0" ]]; then
  if aa-status 2>/dev/null | grep -qx '   podman'; then
    echo "  distro podman AppArmor profile present -- nothing to do"
  else
    echo "  WARN: userns is restricted and no podman AppArmor profile is loaded."
    echo "        Check 'apt policy podman' and 'aa-status'. Do NOT hand-write a"
    echo "        profile for /usr/bin/podman -- it will conflict with the shipped one."
  fi
fi

# Clean up the conflicting profile if a previous run of this script installed it.
if [[ -f /etc/apparmor.d/podman-praxis ]]; then
  echo "  removing obsolete podman-praxis profile (caused attachment conflict)"
  apparmor_parser -R /etc/apparmor.d/podman-praxis 2>/dev/null || true
  rm -f /etc/apparmor.d/podman-praxis
fi

# --- no registry ---------------------------------------------------------------
# This host has no container registry. Images are built directly into the sandbox
# user's local store with `podman build`, and 30-podman-policy.sh configures the
# signature policy to accept local storage only and reject every remote pull.
#
# If a registry is added later, that is the point to revisit: 30 needs a pull
# exception and 40 needs an egress exception. Until then, sealed is correct.

echo
echo "done -- continue with 20-spawnbox-user.sh"
