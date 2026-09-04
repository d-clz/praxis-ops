#!/usr/bin/env bash
# Praxis — sandbox hardening validation.
#
# Run INSIDE a freshly spawned sandbox, as the candidate user and again as root:
#
#   orchestrator exec sbx-<attempt_id> /opt/praxis/hardening-check.sh
#
# Every check asserts a CONTAINMENT property. A FAIL means the sandbox config is
# wrong, not that the box is broken. Run this against every ticket image before it
# enters the catalog, and re-run it after any runtime change (adding systemd,
# adding a capability, switching to gVisor).
#
# Exit 0 = all containment properties hold. Exit 1 = at least one FAIL.

set -uo pipefail

PASS=0; FAIL=0; SKIP=0
RESULTS=()

check() { # check <id> <expectation> <command...>
  local id="$1" expect="$2"; shift 2
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  # expect=blocked -> command MUST fail; expect=allowed -> command MUST succeed
  if [[ "$expect" == "blocked" && $rc -ne 0 ]] || [[ "$expect" == "allowed" && $rc -eq 0 ]]; then
    PASS=$((PASS+1)); RESULTS+=("PASS  $id")
  else
    FAIL=$((FAIL+1)); RESULTS+=("FAIL  $id  (rc=$rc) ${out:0:160}")
  fi
}

skip() { SKIP=$((SKIP+1)); RESULTS+=("SKIP  $1  ($2)"); }

echo "=== praxis hardening validation ==="
echo "uid=$(id -u) euid=$(id -u -n) container=$(hostname)"
echo

# --- 1. identity and namespace mapping -------------------------------------
# Root inside must NOT be root outside. Under rootless podman /proc/self/uid_map
# shows a non-zero host base. If it maps to host uid 0 you are on rootful runtime
# and every other check below is worth much less.
if [[ -r /proc/self/uid_map ]]; then
  # Under nested rootless userns these numbers are relative to the OUTER
  # namespace, so a container mapped onto the podman user looks identical to
  # one mapped into a subordinate range. This can only catch the host-root
  # case; bootstrap/50-verify.sh resolves the real host uid via podman
  # inspect + ps and is the authoritative test.
  host_base=$(awk 'NR==1{print $2}' /proc/self/uid_map)
  if [[ "$host_base" == "0" ]]; then
    FAIL=$((FAIL+1)); RESULTS+=("FAIL  userns.mapped  (container uid maps to HOST UID 0 — rootful runtime)")
  else
    PASS=$((PASS+1)); RESULTS+=("PASS  userns.mapped  (host base uid $host_base)")
  fi
else
  skip "userns.mapped" "no /proc/self/uid_map"
fi

# --- 2. privilege escalation ------------------------------------------------
# capsh (libcap2-bin) is now in the base image (60-build-base.sh), but this
# check must stay honest against any OTHER image it runs against too: without
# the guard, a missing capsh makes these silently "pass" because the command
# is missing, not because the capability is absent. Report that as SKIP, not
# PASS -- an untested property is not a held one.
if command -v capsh >/dev/null 2>&1; then
  # Target the "Bounding set" line specifically, not capsh --print's whole
  # output. Found via a real run against praxis/ops-systemd: a newer
  # libcap2-bin than whatever was on the host when ops-base last passed this
  # clean prints an additional "Current IAB:" summary line that lists EVERY
  # capability, prefixed with "!" for the ones actually absent -- e.g.
  # "...,!cap_sys_module,...". A bare `grep -q cap_sys_module` matches that
  # line on substring alone regardless of the "!", so it false-FAILs a
  # container that has zero capabilities (Bounding set genuinely empty,
  # confirmed by hand against the real output). Bounding set is also the
  # actually-correct thing to check: it is what governs whether the
  # capability could ever become available at all, which is what
  # caps.no_sys_module/no_sys_ptrace are trying to answer in the first place.
  check "caps.no_sys_module" blocked sh -c "capsh --print | awk -F'=' '/^Bounding set/{print \$2}' | grep -qw cap_sys_module"
  check "caps.no_sys_ptrace" blocked sh -c "capsh --print | awk -F'=' '/^Bounding set/{print \$2}' | grep -qw cap_sys_ptrace"
else
  skip "caps.no_sys_module" "capsh not installed -- property untested"
  skip "caps.no_sys_ptrace" "capsh not installed -- property untested"
fi
check "nnp.set"              allowed sh -c 'grep -q "NoNewPrivs:.*1" /proc/self/status'

# --- 3. host filesystem exposure -------------------------------------------
check "host.no_docker_sock"  blocked test -S /var/run/docker.sock
check "host.no_podman_sock"  blocked sh -c 'ls /run/user/*/podman/podman.sock 2>/dev/null | grep -q .'
# findmnt ships inside util-linux (Priority: required), so debootstrap
# installs it regardless of --include -- this guard is belt and suspenders
# for a non-debootstrap image, not an expected miss on ops-base.
if command -v findmnt >/dev/null 2>&1; then
  check "host.no_root_mount" blocked sh -c 'findmnt -rno TARGET | grep -qx /host'
else
  skip "host.no_root_mount" "findmnt not installed -- property untested"
fi
check "host.sys_ro"          blocked sh -c 'touch /sys/kernel/praxis_probe'

# Not the generic check() helper: podman masks kcore by bind-mounting
# /dev/null over it, so `cat /proc/kcore` always exits 0 whether it's masked
# or the real device -- an exit-code check reports "readable" (FAIL) on a
# correctly masked one. Measure bytes actually returned instead.
kcore_bytes=$(head -c 64 /proc/kcore 2>/dev/null | wc -c)
if [[ "$kcore_bytes" == "0" ]]; then
  PASS=$((PASS+1)); RESULTS+=("PASS  host.proc_masked  (0 bytes)")
else
  FAIL=$((FAIL+1)); RESULTS+=("FAIL  host.proc_masked  (${kcore_bytes} bytes readable)")
fi

# --- 4. device access -------------------------------------------------------
check "dev.no_block_devices" blocked sh -c 'ls /dev/sd* /dev/nvme* /dev/vd* 2>/dev/null | grep -q .'
check "dev.no_mem"           blocked test -r /dev/mem

# --- 5. mount and kernel surface -------------------------------------------
check "kernel.no_mount"      blocked sh -c 'mkdir -p /tmp/pm && mount -t proc proc /tmp/pm'
# sysctl(8) exits 0 even when the write is refused -- it prints
# "ignoring: Read-only file system" and carries on. Exit status is useless
# here; compare the value before and after instead, and restore it in the
# FAIL case rather than leave a probe value behind on a host that turned out
# writable.
before="$(cat /proc/sys/kernel/hostname 2>/dev/null)"
sysctl -w kernel.hostname=praxis-probe >/dev/null 2>&1
after="$(cat /proc/sys/kernel/hostname 2>/dev/null)"
if [[ "$before" == "$after" ]]; then
  PASS=$((PASS+1)); RESULTS+=("PASS  kernel.no_sysctl_w  (hostname unchanged)")
else
  FAIL=$((FAIL+1)); RESULTS+=("FAIL  kernel.no_sysctl_w  (hostname changed to $after)")
  sysctl -w "kernel.hostname=$before" >/dev/null 2>&1
fi
check "kernel.no_insmod"     blocked sh -c 'command -v insmod >/dev/null && insmod /dev/null'

# --- 6. network containment -------------------------------------------------
# Zero egress is what stops a candidate diffing against upstream, and stops a
# sandbox reaching GitLab on the same host.
check "net.no_dns"           blocked sh -c 'getent hosts github.com'
check "net.no_egress_ip"     blocked sh -c 'timeout 4 sh -c ": </dev/tcp/1.1.1.1/443"'
check "net.no_host_gateway"  blocked sh -c 'gw=$(ip route 2>/dev/null | awk "/default/{print \$3}"); [ -n "$gw" ] && timeout 3 sh -c ": </dev/tcp/$gw/22"'
check "net.no_gitlab"        blocked sh -c 'timeout 4 sh -c ": </dev/tcp/host.containers.internal/80"'

# --- 7. resource ceilings ---------------------------------------------------
mem_max=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo max)
if [[ "$mem_max" == "max" ]]; then
  FAIL=$((FAIL+1)); RESULTS+=("FAIL  limits.memory  (no memory ceiling)")
else
  PASS=$((PASS+1)); RESULTS+=("PASS  limits.memory  ($mem_max bytes)")
fi
pids_max=$(cat /sys/fs/cgroup/pids.max 2>/dev/null || echo max)
if [[ "$pids_max" == "max" ]]; then
  FAIL=$((FAIL+1)); RESULTS+=("FAIL  limits.pids  (no pid ceiling — fork bomb reaches the host scheduler)")
else
  PASS=$((PASS+1)); RESULTS+=("PASS  limits.pids  ($pids_max)")
fi

# --- 8. answer-key exposure -------------------------------------------------
# The image IS the fault. If any of these leak, the ticket is solved by reading.
check "secret.no_grading_dir" blocked test -d /opt/praxis/.grading
check "secret.no_registry_creds" blocked sh -c 'ls ~/.docker/config.json /run/containers/*/auth.json 2>/dev/null | grep -q .'
check "secret.no_build_history" blocked sh -c 'test -s /root/.bash_history'

echo "--- results ---"
printf '%s\n' "${RESULTS[@]}"
echo
echo "pass=$PASS fail=$FAIL skip=$SKIP"
[[ $FAIL -eq 0 ]]
