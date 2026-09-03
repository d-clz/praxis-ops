#!/usr/bin/env bash
# fix-c: fuser, then resolve the exe and kill by path.
set -euo pipefail
for p in $(fuser /var/log/app/service.log 2>/dev/null); do
  exe=$(readlink /proc/"$p"/exe 2>/dev/null || true)
  args=$(tr '\0' ' ' < /proc/"$p"/cmdline 2>/dev/null || true)
  case "$args" in
    *cache-warmer*) kill -9 "$p" ;;
  esac
done
sleep 1
