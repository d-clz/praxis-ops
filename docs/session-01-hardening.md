# Session 01 — Host hardening

Ubuntu 26.04, single box, shared with GitLab. Scope was host setup only: no
registry, no pipeline, no orchestrator.

**Outcome:** `50-verify.sh` passes 15/15. Configuration is correct and
self-consistent. Containment is **not yet proven** — no container has run.

---

## What exists now

| Layer | State |
|---|---|
| Podman 5.7.0 rootless | working as `praxis-sbx` (uid 1001), cgroups v2, systemd manager |
| User isolation | `praxis-sbx` nologin + locked; home 0700 on a 24G loop volume (`nodev,nosuid`) |
| subuid/subgid | 165536–231071, non-overlapping with `praxis` (100000–165535) |
| Runtime policy | zero default caps, `netns=none`, pids 256, overlay `nodev`, seccomp with 11 extra denied syscalls |
| Supply chain | `policy.json` defaults to reject; only `containers-storage` accepted; public registries blocked |
| Network | nftables rejects all non-loopback egress from uid 1001; other users untouched |
| Lingering | enabled, `user@1001` running |

Both directions of user isolation confirmed by hand:
`praxis` cannot read `/home/praxis-sbx`; `praxis-sbx` cannot read
`/home/praxis` or `/etc/shadow`.

---

## Problems hit

Ten issues. Seven were bugs in the scripts as originally written.

| # | Symptom | Cause | Resolution |
|---|---|---|---|
| 1 | dirs 0707, files 0664, exec bit lost | scp from Windows + server-side umask 070 | transfer as tar; `Subsystem sftp … -u 0022` |
| 2 | sysctl **lowered** `max_user_namespaces` (50238 → 28633) | default written below the kernel's own | `10` now writes only the two inotify lines |
| 3 | `/run/user/1` in output | `stat %i` is the inode, not the path | `%n` |
| 4 | subuid overlap check proved nothing | `useradd` pre-assigned 165536; script validated 200000 — which *overlaps* it | default → 500000; check now reads the effective range from the file |
| 5 | Nexus / tpb.com hardcoded in three scripts | assumed defaults from the operator's work environment, never flagged as a guess | registry logic removed entirely |
| 6 | `cannot chdir …: Permission denied` | `sudo -u` and `runuser` inherit cwd; `praxis-sbx` cannot enter `/home/praxis` | `as_sbx` wrapped in `( cd / && … )` |
| 7 | `failed to reexec: Permission denied` | a hand-written `podman-praxis` AppArmor profile conflicted with the one Ubuntu ships | `10` detects the distro profile and removes the competing one |
| 8 | "uid scoping rule not found" against a correct ruleset | brittle literal grep on `nft list` output | tolerant regex, accepts uid or username |
| 9 | 10 FAILs on a healthy host | script run without sudo; unreadable 0700 config reported as "missing" | `50` exits 2 when not root |
| 10 | orchestrator API would have hung (latent) | `40` rejected loopback replies from uid 1001 — its own listener | `oif "lo" accept` |

**Pattern worth carrying forward:** #7 and #10 were both hardening that broke the
thing it was protecting, and neither would have surfaced until much later. When
adding a control, test the legitimate path immediately, not just the blocked one.

**Ordering constraint discovered:** the loop volume must be mounted between `20`
and `30`. Mounting after `30` hides the config `30` wrote underneath the new
filesystem.

---

## Not proven

- **No container has ever run on this box.** Userns mapping to a non-zero host
  UID, socket invisibility, `/proc/kcore` masking, mount blocking, egress denial,
  and cgroup ceilings are all configuration, not evidence. `50-verify.sh` skips
  that half without an image.
- **No reboot.** The fstab loop entry and the nftables include have never been
  exercised at boot. `user.max_user_namespaces` remains at 28633 until then —
  applied and live, not pending; `sysctl --system` does not restore values whose
  lines were deleted.
- `praxis-portal` does not exist, so the socket-reachability check is a note
  rather than a result.

## Loose ends

- [ ] `chmod` pass over `~/praxis-ops` to undo the scp modes (0707 dirs)
- [ ] delete `/root/podman-praxis.bak`
- [ ] `sysctl -w user.max_user_namespaces=50238` or reboot
- [ ] reboot and re-run `50-verify.sh`

## Notes for whoever picks this up

Run `podman` as `praxis-sbx` only from a directory that user can traverse.
`cd /` first. The error is `cannot chdir`, which reads like a podman fault and
is not one.

The `DENIED` lines for `dac_read_search` and `dac_override` under the
`unprivileged_userns` AppArmor profile are normal. Podman probes for those
capabilities, is refused, and falls back. Do not chase them.
