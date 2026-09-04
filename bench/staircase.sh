#!/usr/bin/env bash
#
# staircase.sh — measure the sandbox capacity of this box.
#
# Replaces the guessed PRAXIS_CAPACITY_WEIGHT with two measured numbers,
# which are different limits:
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
# Run as praxis-sbx. Requires curl and awk (no jq, no yq -- scenario.yaml's
# runtime: block is simple enough that a targeted awk/sed extraction covers
# it without a new dependency).
#
#   IMAGE=praxis/ops-base@sha256:<id> ./bench/staircase.sh
#   RUNBOOK=SKN-01 IMAGE=praxis/ops-base@sha256:<id> STEP_WEIGHT=1 MAX_WEIGHT=8 ./bench/staircase.sh
#
# The orchestrator has no concept of a named runbook -- POST /instances takes
# a fully inlined Runbook object (internal/api/server.go's createReq), and
# main.go's own header is explicit that the orchestrator "knows nothing about
# tickets". So the ticket-awareness lives here, in this script, not as a
# spawn-by-name convenience added to the orchestrator: RUNBOOK names a
# directory under tickets/, this script reads that ticket's own
# scenario.yaml runtime: block and builds the Runbook JSON itself.
#
# IMAGE is required, not read from scenario.yaml's substrate_image: that
# field is REPLACE_AT_BAKE in all three shipped tickets (docs/session-02-plan.md,
# "Still owed" -- the bake pipeline that fills it in doesn't exist yet).
# Resolve a real digest-pinned reference the same way Phase D did for
# praxis/ops-base (podman inspect --format '{{.Id}}', see
# internal/sandbox/container.go's localImageRef) and pass it explicitly.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- configuration ---------------------------------------------------------
# Paths and port match internal/api/server.go's actual routes -- override
# rather than hand-edit if they ever move.
ORCH="${ORCH:-http://127.0.0.1:8081}"
TOKEN="${PRAXIS_API_TOKEN:-}"
HOSTMON="${HOSTMON:-http://127.0.0.1:9102}"

RUNBOOK="${RUNBOOK:-CPT-01}"        # benchmark the heaviest, not the average
IMAGE="${IMAGE:-}"                  # required -- see header comment
STEP_WEIGHT="${STEP_WEIGHT:-1}"     # weight added per step
MAX_WEIGHT="${MAX_WEIGHT:-16}"      # hard ceiling; the run stops here regardless
SOAK="${SOAK:-180}"                 # seconds held per step
SAMPLE="${SAMPLE:-5}"               # seconds between samples

SLICE="${PRAXIS_SLICE_PATH:-/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/praxis.slice/praxis-sbx.slice}"
STORAGE="${PRAXIS_STORAGE_PATH:-/home/praxis-sbx/.local/share/containers}"
NEIGHBOUR_URL="${NEIGHBOUR_URL:-}"  # e.g. http://127.0.0.1/-/health ; skipped if empty

# --- stop conditions -------------------------------------------------------
PSI_FULL_MAX="${PSI_FULL_MAX:-10.0}"      # percent, avg60, slice memory.pressure
STORAGE_MIN_PCT="${STORAGE_MIN_PCT:-20}"  # percent free on the container volume
SPAWN_P95_MAX="${SPAWN_P95_MAX:-20}"      # seconds
NEIGHBOUR_MS_MAX="${NEIGHBOUR_MS_MAX:-2000}"

if [[ -z "$TOKEN" ]]; then
  echo "ERROR: PRAXIS_API_TOKEN is required -- there is no anonymous path against the real API" >&2
  echo "  export PRAXIS_API_TOKEN=\$(sudo grep PRAXIS_ORCH_TOKEN ~praxis-sbx/.config/praxis/orchestrator.env | cut -d= -f2)" >&2
  exit 1
fi

# Needs root, not specifically praxis-sbx -- corrected after actually trying
# praxis-sbx: it can read $STORAGE fine (that part of the original theory
# was right), but it cannot read ANYTHING under /home/praxis at all, which is
# where this repo lives -- tickets/<KEY>/scenario.yaml, and $OUT for the
# results, both of which this script also needs. Being praxis-sbx solves one
# boundary and immediately hits the other. Nothing here actually touches
# podman directly, though -- every podman-adjacent thing goes through the
# orchestrator's HTTP API, which needs no particular identity, just loopback
# network access -- so there is no requirement to actually assume
# praxis-sbx's identity at all. Root reads across both 0700 homes the same
# way bootstrap/50-verify.sh and 60-build-base.sh already do, which is
# simpler than runuser-ing into praxis-sbx for one file and staying as
# whoever invoked this for the rest.
#
# (Earlier version of this check required praxis-sbx specifically --
# storage_free_pct() failing silently to empty from praxis was real, but the
# fix was incomplete: it never confirmed praxis-sbx could read the OTHER
# things this script needs. It can't.)
if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: run as root (sudo $0 ...) -- reads across both praxis's and praxis-sbx's 0700 homes (repo + tickets/, and container storage), same reason every bootstrap/ script needs it" >&2
  exit 1
fi
if [[ -z "$IMAGE" ]]; then
  echo "ERROR: IMAGE is required -- see this script's header for why (no bake pipeline yet to resolve one)" >&2
  exit 1
fi

SCENARIO="$REPO_ROOT/tickets/$RUNBOOK/scenario.yaml"
[[ -r "$SCENARIO" ]] || { echo "ERROR: $SCENARIO not found -- RUNBOOK must match a tickets/<KEY> directory exactly (case-sensitive)" >&2; exit 1; }

OUT="${OUT:-$REPO_ROOT/bench/results/$(date -u +%Y%m%dT%H%M%SZ)}"
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

auth_hdr() { printf 'X-Praxis-Token: %s' "$TOKEN"; }

# --- scenario.yaml -> Runbook JSON ------------------------------------------
# Deliberately minimal, not a general YAML parser: the runtime: block in
# every shipped ticket is flat key: value pairs, no nesting, which is exactly
# what this covers and nothing more. If a ticket ever adds tmpfs/entrypoint
# (real YAML lists/maps), this needs a real parser -- don't stretch it.
runtime_block() {
  awk '/^runtime:/{f=1;next} f && /^[^[:space:]]/{f=0} f{print}' "$SCENARIO"
}

yaml_val() { # $1=key $2=default -> trimmed, unquoted value, or the default
  local v
  v="$(printf '%s\n' "$RUNTIME_BLOCK" | grep -E "^[[:space:]]*${1}:" | head -1 \
    | sed -E "s/^[[:space:]]*${1}:[[:space:]]*//; s/[[:space:]]*#.*//; s/^\"(.*)\"\$/\1/; s/[[:space:]]*\$//")"
  printf '%s' "${v:-$2}"
}

# Defaults mirror internal/sandbox/runbook.go's DefaultRunbook() -- a field
# scenario.yaml doesn't set should behave the same way here as it would
# through LoadScenario, not silently diverge.
RUNTIME_BLOCK="$(runtime_block)"
RB_NETWORK="$(yaml_val network none)"
RB_MEMORY="$(yaml_val memory 512m)"
RB_DISK_LIMIT="$(yaml_val disk_limit 512m)"
RB_CPUS="$(yaml_val cpus 1.0)"
RB_PIDS="$(yaml_val pids_limit 256)"
RB_TTL="$(yaml_val ttl_seconds 3600)"
RB_SYSTEMD="$(yaml_val systemd false)"
RB_WORKDIR="$(yaml_val workdir /home/candidate)"
# None of the three shipped tickets set this yet -- every spawn is weight 1
# until one does. Read it anyway, not hardcoded: the day a ticket's
# scenario.yaml adds a real weight, this should pick it up without anyone
# having to remember this script also needs editing.
RB_WEIGHT="$(yaml_val weight 1)"

# entrypoint is a YAML flow-sequence ("[\"/opt/praxis/entrypoint.sh\"]"), not
# a scalar -- yaml_val's quote-stripping doesn't apply and would mangle it.
# Found the hard way: container.go's spec() ALWAYS sets Cmd (Entrypoint if
# non-empty, else a hardcoded "sleep infinity" fallback), and Cmd on create
# always overrides an image's own baked CMD. Without forwarding this,
# SJN-01 spawns ran sleep infinity instead of its planted writer, silently,
# every time -- confirmed via a real spawn whose log line count never moved.
# A YAML flow-sequence of double-quoted strings already IS valid JSON, so
# this passes through unquoted and unmodified; empty means "no entrypoint
# set", which the [] fallback makes explicit rather than an absent field
# doing the same thing implicitly.
RB_ENTRYPOINT="$(printf '%s\n' "$RUNTIME_BLOCK" | grep -E '^[[:space:]]*entrypoint:' | head -1 \
  | sed -E 's/^[[:space:]]*entrypoint:[[:space:]]*//; s/[[:space:]]*#.*//; s/[[:space:]]*$//')"
RB_ENTRYPOINT="${RB_ENTRYPOINT:-[]}"

log "ticket=$RUNBOOK image=$IMAGE network=$RB_NETWORK memory=$RB_MEMORY disk_limit=$RB_DISK_LIMIT cpus=$RB_CPUS pids_limit=$RB_PIDS ttl_seconds=$RB_TTL systemd=$RB_SYSTEMD weight=$RB_WEIGHT entrypoint=$RB_ENTRYPOINT"

runbook_json() {
  printf '{"image":%s,"network":%s,"memory":%s,"disk_limit":%s,"cpus":%s,"pids_limit":%s,"ttl_seconds":%s,"systemd":%s,"workdir":%s,"weight":%s,"entrypoint":%s}' \
    "$(json_str "$IMAGE")" "$(json_str "$RB_NETWORK")" "$(json_str "$RB_MEMORY")" "$(json_str "$RB_DISK_LIMIT")" \
    "$RB_CPUS" "$RB_PIDS" "$RB_TTL" "$RB_SYSTEMD" "$(json_str "$RB_WORKDIR")" "$RB_WEIGHT" "$RB_ENTRYPOINT"
}

json_str() { printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"; }

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
    -X POST "$ORCH/instances" \
    -H "$(auth_hdr)" -H 'Content-Type: application/json' \
    -d "{\"attempt_id\":\"$id\",\"runbook\":$(runbook_json)}" || echo 000)
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
    curl -o /dev/null -sS --max-time 60 -X DELETE "$ORCH/instances/$id" -H "$(auth_hdr)" || true
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
