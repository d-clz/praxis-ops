#!/usr/bin/env bash
# Starts the Target and the decoys, then parks PID 1.
# exec-less launches on purpose: the candidate should find these via /proc and
# open file descriptors, not by reading an init script.
set -u
cd /
setsid /usr/local/lib/telemetry/cache-warmer  >/dev/null 2>&1 &
setsid /usr/local/lib/telemetry/log-monitor   >/dev/null 2>&1 &
setsid /usr/local/lib/telemetry/audit-writer  >/dev/null 2>&1 &
exec sleep infinity
