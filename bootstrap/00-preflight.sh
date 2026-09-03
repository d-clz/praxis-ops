#!/usr/bin/env bash
# 00-preflight.sh -- verify the box can host rootless sandboxes at all.
# Read-only. Changes nothing. Run as root. Exit non-zero means STOP.
#
# Everything downstream assumes these hold. Finding out at spawn time that
# unprivileged userns is disabled costs a day; finding out here costs a minute.
set -uo pipefail

FAIL=0
ok()   { printf '  OK    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; FAIL=1; }
warn() { printf '  WARN  %s\n' "$*"; }

echo "=== praxis preflight ==="
echo

echo "-- release --"
. /etc/os-release
printf '  %s %s\n' "$NAME" "$VERSION_ID"
case "$VERSION_ID" in
  26.04) ok "26.04 ships podman 5.7.x from universe" ;;
  24.04) warn "24.04 ships podman 4.9.x -- workable, but verify --systemd behaviour" ;;
  22.04) bad "22.04 ships podman 3.4.x -- too old for the systemd ticket tier" ;;
  *)     warn "untested release; check 'apt-cache policy podman' before continuing" ;;
esac

echo "-- cgroups --"
cg="$(stat -fc %T /sys/fs/cgroup)"
if [[ "$cg" == "cgroup2fs" ]]; then
  ok "unified cgroup v2"
else
  bad "cgroup fs is '$cg'; rootless resource limits and systemd-in-container need cgroup2fs"
fi

# Without controller delegation, rootless podman cannot enforce memory or pids
# limits -- the budget guard silently becomes advisory.
deleg="/sys/fs/cgroup/user.slice/cgroup.controllers"
if [[ -r "$deleg" ]]; then
  for c in memory pids cpu; do
    grep -qw "$c" "$deleg" && ok "controller delegated: $c" || bad "controller NOT delegated: $c"
  done
else
  warn "cannot read $deleg -- check controller delegation manually"
fi

echo "-- user namespaces --"
maxns="$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo 0)"
if [[ "$maxns" -gt 1000 ]]; then
  ok "max_user_namespaces=$maxns"
else
  bad "max_user_namespaces=$maxns -- rootless containers cannot start"
fi
if [[ -e /proc/sys/kernel/unprivileged_userns_clone ]]; then
  [[ "$(cat /proc/sys/kernel/unprivileged_userns_clone)" == "1" ]] \
    && ok "unprivileged_userns_clone enabled" \
    || bad "unprivileged_userns_clone disabled"
fi

# AppArmor userns restriction: Ubuntu 24.04+ blocks unprivileged userns by
# default for unconfined binaries. This is the single most common reason
# rootless podman fails on a fresh Ubuntu box.
if [[ -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]]; then
  v="$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns)"
  if [[ "$v" == "0" ]]; then
    ok "apparmor userns restriction off"
  else
    warn "apparmor_restrict_unprivileged_userns=1 -- 10-install-podman.sh installs a profile for podman"
  fi
fi

echo "-- tooling --"
for pkg in uidmap newuidmap newgidmap; do
  command -v "$pkg" >/dev/null 2>&1 && ok "present: $pkg" || warn "missing: $pkg (apt install uidmap)"
done

echo "-- capacity --"
avail_kb="$(df -k --output=avail / | tail -1 | tr -d ' ')"
avail_gb=$((avail_kb / 1024 / 1024))
if [[ "$avail_gb" -ge 40 ]]; then
  ok "root filesystem has ${avail_gb}G free"
else
  warn "only ${avail_gb}G free on / -- image store plus GitLab will be tight"
fi
mem_gb=$(( $(awk '/MemTotal/{print $2}' /proc/meminfo) / 1024 / 1024 ))
echo "  note  ${mem_gb}G RAM total; GitLab will claim 4-8G, so plan 2-3 concurrent sandboxes"

echo "-- existing container stacks --"
systemctl is-active --quiet docker && warn "dockerd is running -- fine, but its image store is invisible to podman"
[[ -S /var/run/docker.sock ]] && warn "/var/run/docker.sock exists -- must never be reachable from a sandbox"

echo
if [[ $FAIL -eq 0 ]]; then
  echo "preflight PASSED -- continue with 10-install-podman.sh"
else
  echo "preflight FAILED -- resolve the FAIL lines before continuing" >&2
fi
exit $FAIL
