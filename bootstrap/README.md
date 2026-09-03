# Sandbox host hardening -- Ubuntu 26.04

Scope: **host setup only**. No registry, no pipeline, no orchestrator. The goal
is a box where an untrusted container can be run safely.

Run in order, as root. Every script is idempotent.

```bash
sudo ./00-preflight.sh          # read-only; STOP if it exits non-zero
sudo ./10-install-podman.sh     # podman + rootless deps + sysctls + apparmor
sudo ./20-spawnbox-user.sh      # praxis-sbx: subuid, linger, 0700 home
sudo ./30-podman-policy.sh      # zero caps, local-store-only, seccomp
sudo ./40-network-guard.sh      # nftables: zero egress for the sandbox UID
sudo ./50-verify.sh             # the gate between "configured" and "trusted"
sudo ./60-build-base.sh         # praxis/ops-base: the image everything else derives from
sudo PRAXIS_VERIFY_IMAGE=praxis/ops-base ./50-verify.sh   # re-run once an image exists
```

Only variable worth setting: `SBX_USER` (default `praxis-sbx`), and
`SUBID_START` / `SUBID_COUNT` if 200000/65536 collides with something.

## No registry

Images are built directly into the sandbox user's local store with
`podman build`. `policy.json` accepts `containers-storage` and rejects
everything else; `registries.conf` has an empty unqualified-search list and
blocks the public registries explicitly. Combined with zero egress in `40`,
this host cannot pull an image from anywhere.

That is the correct posture while there is no registry: sealed. Adding one later
means an exception in **both** `30` (pull policy) and `40` (egress). Changing
only one gives a half-working state that is annoying to debug.

## No `FROM`, either

`60-build-base.sh` produces `praxis/ops-base`, the image every ticket
`Containerfile` derives from. There is nothing to pull it `FROM`, so it isn't
built with `podman build` at all: `debootstrap` (as root, which `40` never
restricts) builds a rootfs directly, and the result is piped straight into
`podman import` as `praxis-sbx` -- `tar -C rootfs -c . | as_sbx podman import -`,
no intermediate file on disk. That's deliberate, not just tidy: an
intermediate tarball under `/root` cannot work, because `/root` is
`0700 root:root` and `praxis-sbx` can't traverse into it no matter what mode
the tarball itself carries -- the same class of reverse-isolation trap `50`
already checks for under `/home/praxis`.

The release is pinned explicitly (`noble` by default, `PRAXIS_BASE_RELEASE` to
override) rather than read from the host's own `/etc/os-release` -- inheriting
the build host's codename would make the base image depend on which machine
built it, which defeats the point of a pinned, reproducible base. The mirror
defaults to whatever the host itself already uses (`PRAXIS_BASE_MIRROR` to
override), and `debootstrap --cache-dir` persists outside the build directory
so a rebuild (`PRAXIS_REBUILD=1`) costs local disk, not another full fetch.

`libcap2-bin` is in the package list on purpose, not as a general-purpose
tool: `security/hardening-check.sh`'s capability checks shell out to `capsh`,
which is `Priority: optional` and so absent from `--variant=minbase` unless
listed explicitly. Without it those checks silently PASS because the command
is missing, not because the capability is actually gone -- a false pass, and
a worse failure mode than no check at all. `findmnt` needs nothing added:
it ships inside `util-linux`, `Priority: required`, present regardless of
`--include`. After `chroot useradd`/`mkdir`, the script confirms
`/opt/praxis` and the `candidate` user actually landed in the rootfs rather
than trusting `set -e` alone to have caught a failure inside the chroot.

## Proving containment (`50`, with an image)

Run `50-verify.sh` a second time with `PRAXIS_VERIFY_IMAGE` set once
`60-build-base.sh` has produced an image. That run adds everything a
config-only pass can't show:

- the full container-side block: containers spawn with `--userns auto`, and
  the host uid a container process actually runs as is read from the host
  side (`podman inspect` -> `ps -o uid=`) rather than trusted from inside --
  `/proc/self/uid_map` alone can't distinguish a correctly subordinate-mapped
  container from one mapped onto `praxis-sbx` itself under nested rootless
  userns
- `/proc/kcore` is checked by byte count, not exit code -- podman masks it via
  a bind-mount over `/dev/null`, so `cat` always exits 0 whether it's masked
  or not
- `security/hardening-check.sh` run from **inside** the same container,
  piped over `podman exec -i`'s stdin (`bash -s < hardening-check.sh`) rather
  than staged in with `podman cp`. cp's copier resolves ownership through the
  `--userns auto` mapping table to chown the copied file into the container,
  and failed for real doing that ("container ID 65534 cannot be mapped to a
  host ID"). Piping into `bash -s` never creates a file in the container, so
  there's nothing to map and nothing left behind. It also sidesteps the
  `/home/praxis` readability question entirely -- root (this script) reads
  the file directly; that permission maze was only ever a `praxis-sbx`
  problem. This deliberately overlaps the outside-asserted checks above it --
  a control asserted from outside and the same control observed from inside
  are different evidence.
- an active load-spawner test, run **last**, after hardening-check.sh: it
  requests 300 backgrounded processes against the 64 pids ceiling and reads
  `pids.current` while they're live. `pids.max` being set proves a ceiling is
  *configured*; this proves it *engages*. Deliberately not a literal
  `:(){ :|:& };:` fork bomb -- that's bash function syntax, the container's
  `/bin/sh` is dash, and it errors out before spawning anything, which reads
  as a passing ceiling for the wrong reason. A POSIX `for` loop is bounded,
  portable, and reports a real number instead of a maybe. It runs last
  because it's genuinely destructive to the container's own usability: once
  the pids cgroup is full, the container can't fork *anything* else,
  including the helper process a further `podman exec` would need -- which is
  why the ordering matters and why `pids.current` is read from the host side
  (`/proc/<container-pid>/cgroup`) rather than via another exec. `podman rm -f`
  doesn't need to fork into the container either, so a saturated one is still
  safe to tear down. The host's own process table never moves either way.

Two things stay manual, and always will:

- reboot, then re-run the same command -- proves the loop mount and nftables
  include survive a boot, not just this session
- `cat /proc/sys/user/max_user_namespaces` -- confirm it settled back to the
  kernel default post-reboot

## Hardening checklist

What each layer asserts. Each assumes the one above it has already failed.

**1. Account and filesystem** (`20`)
- [ ] `praxis-sbx` exists, nologin shell, password locked
- [ ] home is `0700` -- the image store holds fault-injected images, i.e. answer keys
- [ ] `/etc/subuid` + `/etc/subgid` range assigned, **verified non-overlapping**
- [ ] lingering enabled -- otherwise the socket and TTL reaper die on logout
- [ ] `/run/user/<uid>` is `0700` -- this is what protects the podman socket
- [ ] capacity bounded (separate mount or quota) -- permissions limit access, not disk

**2. Runtime policy** (`30`)
- [ ] `default_capabilities = []` -- containers get nothing unless explicitly added
- [ ] `netns = "none"` by default
- [ ] `no_hosts`, empty dns_searches -- no host name resolution leaks in
- [ ] `pids_limit` and `log_size_max` ceilings apply even if the caller forgets
- [ ] overlay `nodev` -- no usable device nodes in a layer
- [ ] seccomp denies kexec, module load, bpf, perf_event_open, userfaultfd, process_vm_*

**3. Supply chain** (`30`)
- [ ] `policy.json` defaults to reject
- [ ] only `containers-storage` accepted
- [ ] docker.io / quay.io / ghcr.io / registry.k8s.io blocked
- [ ] `unqualified-search-registries = []` -- a bare image name fails loudly

**4. Network** (`40`)
- [ ] nftables filters on `meta skuid`, not on a subnet (rootless has no bridge)
- [ ] all non-loopback egress from the sandbox UID rejected and logged
- [ ] loopback open -- the orchestrator's own API replies are attributed to this UID
- [ ] other users on the box entirely unaffected (`policy accept` + skuid exemption)

**5. Proof** (`50`)
- [ ] rootless confirmed, cgroups v2
- [ ] container root maps to a non-zero host UID
- [ ] no docker/podman socket visible from inside a container
- [ ] `/proc/kcore` masked, no block devices, mount blocked
- [ ] no external DNS, no external egress
- [ ] memory and pids ceilings actually enforced

## Two facts that shape all of this

**Rootless networking has no bridge.** pasta proxies container traffic through a
userspace process owned by `praxis-sbx`, so there is no container subnet for the
host to filter. Rules written against `10.89.x.x` match nothing.

**Docker's image store is invisible to Podman.** They share no daemon, no store,
no socket. `docker build` output cannot be run by podman. Build as the sandbox
user with `podman build`.

## Re-run `50-verify.sh` after

- any change under `~praxis-sbx/.config/containers/`
- any nftables change
- a podman upgrade
- granting a new capability to any ticket tier
