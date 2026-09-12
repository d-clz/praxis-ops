#!/usr/bin/env bash
# SJN-01 oracle. SERVER-ONLY.
set -uo pipefail

LOG=/var/log/app/service.log
WRITER=/usr/local/lib/telemetry/cache-warmer
MONITOR=/usr/local/lib/telemetry/log-monitor
SETTLE=6   # growth sample window; kept in the check so the grade is reproducible

solved=(); invariants=(); rc=0

emit() { local -n arr="$1"; arr+=("{\"id\":\"$2\",\"ok\":$3,\"detail\":\"$4\"}"); [[ "$3" == true ]] || rc=1; }
running() { pgrep -f "$1" >/dev/null 2>&1; }

# --- solved -----------------------------------------------------------------
# Two independent signals. Size-stability alone passes for `chattr +i`; process
# absence alone passes if the writer was replaced by a slower one.
if [[ -f "$LOG" ]]; then
  s1=$(stat -c %s "$LOG"); sleep "$SETTLE"; s2=$(stat -c %s "$LOG")
  if [[ "$s1" == "$s2" ]]; then
    emit solved growth_stopped true "size stable at $s2 over ${SETTLE}s"
  else
    emit solved growth_stopped false "grew $((s2 - s1)) bytes in ${SETTLE}s"
  fi
else
  emit solved growth_stopped false "log absent -- cannot measure"
fi

if running "$WRITER"; then
  emit solved writer_stopped false "writer process still running"
else
  emit solved writer_stopped true "no writer process"
fi

# --- invariants -------------------------------------------------------------
if [[ -f "$LOG" ]]; then
  emit invariants log_exists true "present"
  expected="$(cat /opt/praxis/.history-sha256 2>/dev/null || echo MISSING)"
  actual="$(head -n 5000 "$LOG" | sha256sum | awk '{print $1}')"
  if [[ "$actual" == "$expected" ]]; then
    emit invariants history_intact true "first 5000 lines match provision state"
  else
    emit invariants history_intact false "pre-existing history altered or truncated"
  fi
else
  emit invariants log_exists false "log deleted"
  emit invariants history_intact false "log deleted"
fi

# Catches the `freeze the file` shortcut even when growth_stopped somehow passes.
if lsattr "$LOG" 2>/dev/null | awk '{print $1}' | grep -q i; then
  emit invariants not_frozen false "immutable attribute set on the log"
else
  emit invariants not_frozen true "no immutable attribute"
fi

# The decoy reader is unrelated to the fault. Killing it is collateral damage,
# and it is the single most common wrong instinct on this ticket.
if running "$MONITOR"; then
  emit invariants no_collateral true "unrelated monitor still running"
else
  emit invariants no_collateral false "unrelated monitor was killed"
fi

join() { local IFS=,; echo "$*"; }
printf '{"schema":"praxis.check/v1","ticket_key":"SJN-01","solved":[%s],"invariants":[%s]}\n' \
  "$(join "${solved[@]}")" "$(join "${invariants[@]}")"
exit $rc
