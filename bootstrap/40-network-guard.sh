#!/usr/bin/env bash
# 40-network-guard.sh -- host firewall for sandbox traffic. Run as root.
# Idempotent.
#
# THE KEY POINT: rootless podman uses pasta/slirp4netns, which proxies container
# traffic through a USERSPACE process owned by the sandbox user. There is no
# bridge interface and no container subnet visible to the host, so subnet-based
# rules match nothing and give false confidence. The correct lever is
# `meta skuid` -- filter by the UID that owns the proxy process.
#
# Policy on this host: praxis-sbx gets NO external egress at all. There is no
# registry to reach, so nothing legitimate needs to leave the box. One rule
# covers both halves of the threat model:
#   - a candidate cannot diff their work against an upstream repo
#   - a sandbox cannot reach GitLab, the portal, or anything else on this box
set -euo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER")"

echo "=== network guard for uid ${SBX_UID} (${SBX_USER}) ==="

install -d -m 0755 /etc/nftables.d
cat > /etc/nftables.d/praxis-sandbox.nft <<EOF
#!/usr/sbin/nft -f
# Praxis sandbox egress policy. Managed by 40-network-guard.sh -- do not hand-edit.

table inet praxis
delete table inet praxis

table inet praxis {
    chain output {
        type filter hook output priority filter; policy accept;

        # Only traffic owned by the sandbox user is in scope. Everything else on
        # this box -- GitLab, your shell, admin SSH -- is untouched.
        meta skuid != ${SBX_UID} accept

        # Loopback must stay open. The orchestrator runs as ${SBX_USER} and
        # listens on 127.0.0.1 for the portal; its REPLY packets are attributed
        # to this UID and would otherwise hit the reject below -- breaking the
        # API in a way that looks like a hang rather than a firewall denial.
        oif "lo" accept

        # Everything else from this user is denied. reject rather than drop, so
        # failures surface immediately instead of hanging for a timeout.
        log prefix "praxis-egress-denied " level warn
        reject with icmpx type admin-prohibited
    }
}
EOF

nft -c -f /etc/nftables.d/praxis-sandbox.nft && echo "  ruleset syntax OK"
nft -f /etc/nftables.d/praxis-sandbox.nft && echo "  ruleset loaded"

if [[ -f /etc/nftables.conf ]] && ! grep -q 'praxis-sandbox.nft' /etc/nftables.conf; then
  echo 'include "/etc/nftables.d/praxis-sandbox.nft"' >> /etc/nftables.conf
  echo "  added include to /etc/nftables.conf"
fi
systemctl enable nftables >/dev/null 2>&1 || true

cat <<'NOTE'

  Verify:         nft list table inet praxis
  Watch denials:  journalctl -kf | grep praxis-egress-denied

  Adding a registry later means an exception HERE and a pull rule in
  30-podman-policy.sh. Both layers deny by default; changing only one gives a
  confusing half-working state.
NOTE
echo "done -- continue with 50-verify.sh"
