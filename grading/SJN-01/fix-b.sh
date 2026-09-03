#!/usr/bin/env bash
# fix-b: no lsof. Walk /proc and match the exe path directly.
set -euo pipefail
for p in /proc/[0-9]*; do
  exe=$(readlink "$p/exe" 2>/dev/null || true)
  cmd=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null || true)
  case "$cmd" in
    *cache-warmer*) kill -TERM "$(basename "$p")" ;;
  esac
done
sleep 1
