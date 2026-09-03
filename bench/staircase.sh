#!/usr/bin/env bash
#
# staircase.sh — measure the sandbox capacity of this box.
#
# Replaces the guessed PRAXIS_MAX_CONCURRENT with two measured numbers, which
# are different limits:
#
#   1. steady-state weight  — how much can be held concurrently
#   2. spawn rate           — how fast weight can be added
#
# Method: add one step of load, soak, sample, repeat. Abort on the first stop
# condition. Report the last step that held cleanly.
#
# Stop conditions are declared BEFORE the run, not chosen after looking at the
# graph. The neighbour's health is one of them: this box also runs GitLab, and a
# capacity number that degrades GitLab is not a capacity number.
#
# Run as praxis-sbx. Requires curl, jq, awk.
#
#   ./bench/staircase.sh
#   RUNBOOK=cpt-01 STEP_WEIGHT=4 MAX_WEIGHT=24 ./bench/staircase.sh
#
set -euo pipefail

# --- configuration ---------------------------------------------------------
# Paths are ASSUMED. Override rather than edit.
ORCH="${ORCH:-http://127.0.0.1:9100}"           # orchestrator API base
SPAWN_PATH="${SPAWN_PATH:-/v1/sandboxes}"       # POST {attempt_id, runbook}
DESTROY_PATH="${DESTROY_PATH:-/v1/sandboxes}"   # DELETE {base}/{attempt_id}
HOSTMON="${HOSTMON:-http://127.0.0.1:9102}"
TOKEN="${PRAXIS_API_TOKEN:-}"

RUNBOOK="${RUNBOOK:-cpt-01}"        # benchmark the heaviest, not the average
STEP_WEIGHT="${STEP_WEIGHT:-1}"     # weight added per step
MAX_WEIGHT="${MAX_WEIGHT:-16}"      # hard ceiling; the run stops here regardless
SOAK="${SOAK:-180}"                 # seconds held per step
SAMPLE="${SAMPLE:-5}"               # seconds between samples

SLICE="${PRAXIS_SLICE_PATH:-/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/praxis-sbx.slice}"
STORAGE="${PRAXIS_STORAGE_PATH:-/home/praxis-sbx/.local/share/containers}"
NEIGHBOUR_URL="${NEIGHBOUR_URL:-}"  # e.g. http://127.0.0.1/-/health ; skipped if empty

# --- stop conditions -------------------------------------------------------
PSI_FULL_MAX="${PSI_FULL_MAX:-10.0}"      # percent, avg60, slice memory.pressure
STORAGE_MIN_PCT="${STORAGE_MIN_PCT:-20}"  # percent free on the container volume
SPAWN_P95_MAX="${SPAWN_P95_MAX:-20}"      # seconds
NEIGHBOUR_MS_MAX="${NEIGHBOUR_MS_MAX:-2000}"

OUT="${OUT:-./bench/results/$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p "$OUT"
CSV="$OUT/samples.csv"
SPAWNS="$OUT/spawns.csv"
RUN_ID="bench-$(date -u +%s)"

echo "step,elapsed_s,weight,sessions,slice_mem_bytes,psi_some,psi_full,storage_free_pct,oom_kills" > "$CSV"
echo "attempt_id,step,latency_s,http_code" > "$SPAWNS"

ATTEMPTS=()
ABORT_REASON=""
LAST_GOOD=0

log() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

auth_hdr() { [[ -n "$TOKEN" ]] && printf 'Authorization: Bearer %s' "$TOKEN" || printf 'X-Bench: 1'; }

# --- readings --------------------------------------------------------------
read_int()  { [[ -r "$1" ]] && tr -d '[:space:]' < "$1" || echo 0; }

psi() { # $1 = some|full  -> avg60 as a percentage
  local f="$SLICE/memory.pressure"
  [[ -r "$f" ]] || { echo 0; return; }
  awk -v k="$1" '$1==k { for(i=2;i<=NF;i++) if ($i ~ /^avg60=/) { sub("avg60=","",$i); print $i; exit } }' "$f" \
    | awk 'NF{print; found=1} END{if(!found) print 0}'
}

oom_kills() {
  local f="$SLICE/memory.events"
  [[ -r "$f" ]] || { echo 0; return; }
  awk '$1=="oom_kill"{print $2; found=1} END{if(!found) print 0}' "$f"
}

storage_free_pct() {
  df --output=pcent "$STORAGE" 2>/dev/null | tail -1 | tr -dc '0-9' | awk '{print 100-$1}'
}

session_count() {
  curl -fsS --max-time 5 "$HOSTMON/metrics" 2>/dev/null \
    | awk '/^praxis_sessions_current\{.*view="host".*\}/ {s+=$2} END{printf "%d", s+0}'
}

neighbour_ms() {
  [[ -z "$NEIGHBOUR_URL" ]] && { echo 0; return; }
  local t
  t=$(curl -o /dev/null -sS -w '%{time_total}' --max-time 10 "$NEIGHBOUR_URL" 2>/dev/null || echo 99)
  awk -v t="$t" 'BEGIN{printf "%d", t*1000}'
}

OOM_BASE=$(oom_kills)

# --- lifecycle -------------------------------------------------------------
spawn_one() {
  local step="$1" id="${RUN_ID}-$(printf '%03d' "${#ATTEMPTS[@]}")"
  local t0 t1 code
  t0=$(date +%s.%N)
  code=$(curl -o /dev/null -sS -w '%{http_code}' --max-time 120 \
    -X POST "$ORCH$SPAWN_PATH" \
    -H "$(auth_hdr)" -H 'Content-Type: application/json' \
    -d "{\"attempt_id\":\"$id\",\"runbook\":\"$RUNBOOK\"}" || echo 000)
  t1=$(date +%s.%N)
  local lat; lat=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b-a}')
  echo "$id,$step,$lat,$code" >> "$SPAWNS"
  if [[ "$code" =~ ^2 ]]; then
    ATTEMPTS+=("$id")
    log "  spawned $id in ${lat}s"
  else
    log "  spawn FAILED http=$code after ${lat}s"
    ABORT_REASON="spawn returned http $code"
    return 1
  fi
}

spawn_p95() {
  awk -F, 'NR>1 && $4 ~ /^2/ {a[n++]=$3} END{
    if(!n){print 0; exit}
    asort_done=0
    for(i=0;i<n;i++) for(j=i+1;j<n;j++) if(a[j]<a[i]){t=a[i];a[i]=a[j];a[j]=t}
    idx=int(0.95*(n-1)); printf "%.2f", a[idx]
  }' "$SPAWNS"
}

teardown() {
  log "tearing down ${#ATTEMPTS[@]} sandbox(es)"
  for id in "${ATTEMPTS[@]:-}"; do
    [[ -z "$id" ]] && continue
    curl -o /dev/null -sS --max-time 60 -X DELETE "$ORCH$DESTROY_PATH/$id" -H "$(auth_hdr)" || true
  done
  # The reaper is the backstop, but a benchmark that relies on the backstop is
  # not testing what it thinks it is. Verify the box actually drained.
  sleep 5
  local left; left=$(session_count)
  log "sessions remaining after teardown: $left"
  [[ "$left" -gt 0 ]] && log "WARNING: $left sandbox(es) survived explicit destroy — check praxis_orphans"
  return 0
}
trap teardown EXIT

# --- sampling --------------------------------------------------------------
check_stops() {
  local pf ps free oom nb
  pf=$(psi full); ps=$(psi some)
  free=$(storage_free_pct); oom=$(oom_kills); nb=$(neighbour_ms)

  awk -v v="$pf" -v m="$PSI_FULL_MAX" 'BEGIN{exit !(v>m)}' && {
    ABORT_REASON="memory PSI full avg60 ${pf}% > ${PSI_FULL_MAX}%"; return 1; }
  [[ "$free" -lt "$STORAGE_MIN_PCT" ]] && {
    ABORT_REASON="container storage ${free}% free < ${STORAGE_MIN_PCT}%"; return 1; }
  [[ "$oom" -gt "$OOM_BASE" ]] && {
    ABORT_REASON="OOM kill in the sandbox slice ($oom > $OOM_BASE)"; return 1; }
  [[ "$nb" -gt "$NEIGHBOUR_MS_MAX" ]] && {
    ABORT_REASON="neighbour health probe ${nb}ms > ${NEIGHBOUR_MS_MAX}ms"; return 1; }
  return 0
}

soak_step() {
  local step="$1" weight="$2" start now
  start=$(date +%s)
  while :; do
    now=$(date +%s)
    local elapsed=$(( now - start ))
    [[ "$elapsed" -ge "$SOAK" ]] && break
    echo "$step,$elapsed,$weight,$(session_count),$(read_int "$SLICE/memory.current"),$(psi some),$(psi full),$(storage_free_pct),$(oom_kills)" >> "$CSV"
    check_stops || return 1
    sleep "$SAMPLE"
  done
  local p95; p95=$(spawn_p95)
  awk -v v="$p95" -v m="$SPAWN_P95_MAX" 'BEGIN{exit !(v>m)}' && {
    ABORT_REASON="spawn p95 ${p95}s > ${SPAWN_P95_MAX}s"; return 1; }
  return 0
}

# --- run -------------------------------------------------------------------
log "runbook=$RUNBOOK step=$STEP_WEIGHT max=$MAX_WEIGHT soak=${SOAK}s -> $OUT"
[[ -z "$NEIGHBOUR_URL" ]] && log "NOTE: NEIGHBOUR_URL unset — the GitLab stop condition is NOT armed"

weight=0; step=0
while [[ "$weight" -lt "$MAX_WEIGHT" ]]; do
  step=$(( step + 1 ))
  log "step $step: adding $STEP_WEIGHT weight (target $(( weight + STEP_WEIGHT )))"
  for _ in $(seq 1 "$STEP_WEIGHT"); do
    spawn_one "$step" || break 2
  done
  weight=$(( weight + STEP_WEIGHT ))
  log "step $step: soaking ${SOAK}s at weight $weight"
  soak_step "$step" "$weight" || break
  LAST_GOOD="$weight"
  log "step $step: held at weight $weight"
done

# --- report ----------------------------------------------------------------
{
  echo "# Staircase result — $(date -u +%FT%TZ)"
  echo
  echo "runbook:            $RUNBOOK"
  echo "steady-state weight: $LAST_GOOD"
  echo "spawn p95:           $(spawn_p95)s"
  echo "stopped because:     ${ABORT_REASON:-reached MAX_WEIGHT=$MAX_WEIGHT without tripping a stop condition}"
  echo
  echo "Set PRAXIS_CAPACITY_WEIGHT to ~70% of the steady-state weight."
  echo "Arrivals are not smooth, and a rejection lands on a candidate mid-assessment."
  echo
  echo "Rate implied by this number:  concurrent = arrivals_per_min * mean_session_minutes"
  echo "samples: $CSV"
  echo "spawns:  $SPAWNS"
} | tee "$OUT/report.txt"

if [[ -n "$ABORT_REASON" && "$LAST_GOOD" -eq 0 ]]; then
  log "no step held — the box cannot run a single $RUNBOOK sandbox under these conditions"
  exit 1
fi
