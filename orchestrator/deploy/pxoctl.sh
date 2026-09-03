#!/usr/bin/env bash
# pxoctl.sh -- day-2 operations for praxis-orchestrator, in two layers:
#
#   unit <cmd>   infrastructure: is the process even alive (systemd --user,
#                inside praxis-sbx's own lingering instance)
#   api  <cmd>   API: is it actually answering requests correctly
#
# Check unit first. A systemd unit reporting failed doesn't need an API probe
# to explain itself, and one reporting active(running) can still be wedged or
# answering wrong -- that's what api is for.
#
# Never self-elevates. A subcommand that needs root says so, plainly, and
# exits -- deciding to re-run under sudo is always yours, not this script's.
set -uo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
UNIT="praxis-orchestrator"
BASE="${PRAXIS_API_BASE:-http://127.0.0.1:8081}"
ENV_FILE="$(getent passwd "$SBX_USER" 2>/dev/null | cut -d: -f6)/.config/praxis/orchestrator.env"

usage() {
  cat <<EOF
usage: $0 <group> <command> [args]

unit -- infrastructure layer (needs root; runs inside praxis-sbx's own
        systemd --user instance)
  unit status            unit status (active/failed, log tail, restart count)
  unit logs [N]          last N journal lines, default 50
  unit logs -f           follow the journal live
  unit start|stop|restart
  unit enable            enable at boot and start now (first-time activation)
  unit disable           disable at boot (does not stop a running unit)

api -- API layer (mostly does NOT need root)
  api health             GET /healthz -- no token, no root
  api get <attempt_id>   GET /instances/<attempt_id> -- needs the shared
                         token (export PRAXIS_ORCH_TOKEN yourself, or run as
                         root to read it from praxis-sbx's own env file)

Check unit before api: a failed unit doesn't need an API probe to explain
itself, and a running one can still be wedged or answering wrong -- that's
what api is for.
EOF
  exit "${1:-1}"
}

# Never sudo on your own behalf. Say what's needed, in the exact command that
# would fix it, and let the caller make that call themselves.
need_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "ERROR: '$*' needs root -- re-run with: sudo $0 $*" >&2
    exit 2
  fi
}

as_sbx() {
  local sbx_uid
  sbx_uid="$(id -u "$SBX_USER")"
  runuser -u "$SBX_USER" -- env XDG_RUNTIME_DIR="/run/user/$sbx_uid" "$@"
}

token() {
  if [[ -n "${PRAXIS_ORCH_TOKEN:-}" ]]; then
    printf '%s' "$PRAXIS_ORCH_TOKEN"
    return 0
  fi
  if [[ "$(id -u)" -eq 0 && -r "$ENV_FILE" ]]; then
    grep -m1 '^PRAXIS_ORCH_TOKEN=' "$ENV_FILE" | cut -d= -f2-
    return 0
  fi
  return 1
}

group="${1:-}"
cmd="${2:-}"
[[ -z "$group" || "$group" == "-h" || "$group" == "--help" || "$group" == "help" ]] && usage 0

case "$group" in
  unit)
    [[ -z "$cmd" ]] && usage 1
    need_root "unit $cmd"
    case "$cmd" in
      status)
        as_sbx systemctl --user status "$UNIT"
        ;;
      logs)
        if [[ "${3:-}" == "-f" ]]; then
          as_sbx journalctl --user -u "$UNIT" -f
        else
          as_sbx journalctl --user -u "$UNIT" -n "${3:-50}" --no-pager
        fi
        ;;
      start|stop|restart)
        as_sbx systemctl --user "$cmd" "$UNIT"
        ;;
      enable)
        as_sbx systemctl --user enable --now "$UNIT"
        ;;
      disable)
        as_sbx systemctl --user disable "$UNIT"
        ;;
      *)
        echo "unknown unit command: $cmd" >&2
        usage 1
        ;;
    esac
    ;;
  api)
    [[ -z "$cmd" ]] && usage 1
    case "$cmd" in
      health)
        curl -sf "$BASE/healthz" && echo \
          || { echo "unreachable -- check the infrastructure layer: $0 unit status" >&2; exit 1; }
        ;;
      get)
        id="${3:-}"
        [[ -z "$id" ]] && { echo "usage: $0 api get <attempt_id>" >&2; exit 1; }
        tok="$(token)" || {
          echo "ERROR: no token available -- export PRAXIS_ORCH_TOKEN yourself, or re-run with: sudo $0 api get $id" >&2
          exit 2
        }
        curl -sf -H "X-Praxis-Token: $tok" "$BASE/instances/$id" && echo \
          || { echo "no such instance, or the request failed" >&2; exit 1; }
        ;;
      *)
        echo "unknown api command: $cmd" >&2
        usage 1
        ;;
    esac
    ;;
  *)
    echo "unknown group: $group" >&2
    usage 1
    ;;
esac
