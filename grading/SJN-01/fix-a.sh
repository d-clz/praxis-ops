#!/usr/bin/env bash
# fix-a: lsof, select only the write-mode holder.
set -euo pipefail
pid=$(lsof -t /var/log/app/service.log -a -d '^0,^1,^2' 2>/dev/null | while read -r p; do
  for fd in /proc/"$p"/fd/*; do
    [ -L "$fd" ] || continue
    tgt=$(readlink "$fd") || continue
    [ "$tgt" = "/var/log/app/service.log" ] || continue
    grep -q '^flags:.*[12]$' /proc/"$p"/fdinfo/"$(basename "$fd")" 2>/dev/null && echo "$p"
  done
done | head -1)
[ -n "${pid:-}" ] && kill -TERM "$pid"
sleep 1
