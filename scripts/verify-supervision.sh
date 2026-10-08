#!/usr/bin/env bash
#
# Acceptance test for the incident report's P0 items: supervised restart,
# recovery after reboot, and access without an interactive session.
#
# Run this on the Ubuntu server where dsh-remote is installed as a user service.
# It is safe to re-run. It never touches Tailscale Serve configuration directly
# and never prints a DeepSeek Harness token.
#
#   scripts/verify-supervision.sh --lan          # service installed with --lan
#   scripts/verify-supervision.sh --no-reboot    # skip the reboot check
#
# Exit status is 0 only when every requested check passed.
set -u

BIN="${DSH_REMOTE_BIN:-dsh-remote}"
UNIT="dsh-remote.service"
DSH_PORT=3080
PROXY_PORT=3081
DO_REBOOT=1
ASSUME_YES=0
FAILURES=0

usage() {
  cat >&2 <<'USAGE'
Usage: scripts/verify-supervision.sh [options]

Acceptance test for supervised restart, reboot recovery, and access without an
interactive session. Run it on the Ubuntu server that hosts the user service.

  --dsh-port N     DeepSeek Harness port (default 3080)
  --proxy-port N   proxy port (default 3081)
  --no-reboot      skip the interactive reboot check
  --yes, -y        answer yes to the reboot prompt
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dsh-port) DSH_PORT="$2"; shift 2 ;;
    --proxy-port) PROXY_PORT="$2"; shift 2 ;;
    --no-reboot) DO_REBOOT=0; shift ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

pass() { printf '  [ok]   %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
warn() { printf '  [warn] %s\n' "$1"; }
info() { printf '         %s\n' "$1"; }
step() { printf '\n== %s ==\n' "$1"; }

curl_health() {
  curl -fsS --max-time 3 "http://127.0.0.1:${PROXY_PORT}/__dsh_remote/health" 2>/dev/null
}

wait_healthy() {
  local deadline=$((SECONDS + ${1:-90}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    curl_health >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

state_field() {
  # state_field <json-key>: read a field from the run state without a JSON parser.
  local file="$1" key="$2"
  sed -n "s/.*\"${key}\": *\([0-9]*\).*/\1/p" "$file" 2>/dev/null | head -1
}

state_file() {
  printf '%s/dsh-remote/state.json' "${XDG_STATE_HOME:-$HOME/.local/state}"
}

# ---------------------------------------------------------------------------
step "prerequisites"
if [ "$(uname -s)" != "Linux" ]; then
  echo "this script tests systemd and must run on Linux" >&2
  exit 2
fi
command -v "$BIN" >/dev/null 2>&1 || { echo "$BIN not found in PATH (set DSH_REMOTE_BIN)" >&2; exit 2; }
command -v systemctl >/dev/null 2>&1 || { echo "systemctl not found" >&2; exit 2; }
if ! systemctl --user show-environment >/dev/null 2>&1; then
  echo "cannot reach the systemd user bus; run this as the user that owns the service" >&2
  exit 2
fi
pass "$BIN present, systemd user bus reachable"

# ---------------------------------------------------------------------------
step "logs survive a restart"
STATE_DIR="$(dirname "$(state_file)")"
LOG_FILE="$STATE_DIR/dsh-remote.log"
if [ -f "$LOG_FILE" ]; then
  pass "persistent dsh-remote log at $LOG_FILE"
  info "last entry: $(tail -n 1 "$LOG_FILE" 2>/dev/null)"
else
  warn "no dsh-remote log yet at $LOG_FILE"
  info "it is created by the next \`$BIN start\`"
fi

# The journal is the systemd-level view (restart reasons, exit codes). It only
# survives a reboot when /var/log/journal exists, because the default
# Storage=auto keeps logs in /run otherwise.
if [ -d /var/log/journal ]; then
  pass "journald is persistent (/var/log/journal exists)"
else
  warn "journald is volatile on this host (no /var/log/journal)"
  info "systemd's restart history will be lost on reboot. To keep it:"
  info "  sudo mkdir -p /var/log/journal && sudo systemd-tmpfiles --create --prefix /var/log/journal && sudo systemctl restart systemd-journald"
  info "dsh-remote's own log at $LOG_FILE is unaffected either way"
fi

# ---------------------------------------------------------------------------
step "unit and restart policy"
UNIT_PATH="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/${UNIT}"
if [ ! -f "$UNIT_PATH" ]; then
  fail "no unit at $UNIT_PATH"
  info "run: $BIN install --lan   (or $BIN install)"
  exit 1
fi
pass "unit present at $UNIT_PATH"

RESTART=$(systemctl --user show "$UNIT" --property=Restart --value)
case "$RESTART" in
  always|on-failure|on-abnormal|on-abort)
    pass "Restart=$RESTART" ;;
  *)
    fail "Restart=${RESTART:-<unset>} will not recover every unexpected exit"
    info "reinstall with: $BIN install --force" ;;
esac

if grep -q '^KillMode=' "$UNIT_PATH"; then
  KILL_MODE=$(sed -n 's/^KillMode=//p' "$UNIT_PATH" | head -1)
else
  KILL_MODE="control-group (systemd default)"
fi
info "KillMode: $KILL_MODE"

if [ -n "${XDG_SESSION_ID:-}" ] || [ -n "${SSH_CONNECTION:-}" ]; then
  LINGER=$(loginctl show-user "${USER:-$(id -un)}" --property=Linger --value 2>/dev/null || echo unknown)
  if [ "$LINGER" = "yes" ]; then
    pass "lingering enabled (service starts after reboot without a login)"
  else
    fail "lingering is '${LINGER}'"
    info "run: sudo loginctl enable-linger $USER"
  fi
fi

# ---------------------------------------------------------------------------
step "service active and proxy reachable (baseline)"
if systemctl --user is-active --quiet "$UNIT"; then
  pass "$UNIT is active"
else
  fail "$UNIT is not active"
  info "run: systemctl --user start $UNIT   then re-run this script"
  exit 1
fi

if wait_healthy 60; then
  pass "proxy health endpoint answered on port $PROXY_PORT"
else
  fail "proxy did not become healthy on port $PROXY_PORT within 60s"
  info "inspect: journalctl --user -u $UNIT -n 80 --no-pager"
  exit 1
fi

"$BIN" doctor --dsh-port "$DSH_PORT" --proxy-port "$PROXY_PORT" || warn "doctor reported problems; review its output above"

STATE="$(state_file)"
if [ -f "$STATE" ]; then
  SERVICE_DSH_PID=$(state_field "$STATE" dshPid)
  SERVICE_PROXY_PID=$(state_field "$STATE" pid)
  pass "run state records proxy pid ${SERVICE_PROXY_PID:-?} and DSH pid ${SERVICE_DSH_PID:-?}"
else
  warn "no run state at $STATE; the service may not have reached a steady state"
  SERVICE_DSH_PID=""
fi

# ---------------------------------------------------------------------------
step "restart after an unexpected kill"
# SIGKILL to the proxy's main pid is the closest reproduction of the incident.
# systemd's default KillMode=control-group means it must clean up the child it
# launched as well; whatever survives is exactly what the report called an
# orphan, and what `dsh-remote start` now reclaims.
if [ -n "${SERVICE_PROXY_PID:-}" ]; then
  echo "         SIGKILL pid $SERVICE_PROXY_PID"
  kill -9 "$SERVICE_PROXY_PID" 2>/dev/null || warn "pid $SERVICE_PROXY_PID was already gone"

  # Record what survives, before systemd's restart has a chance to clean up.
  sleep 2
  SURVIVING_DSH=""
  if [ -n "${SERVICE_DSH_PID:-}" ] && kill -0 "$SERVICE_DSH_PID" 2>/dev/null; then
    SURVIVING_DSH="$SERVICE_DSH_PID"
    warn "DeepSeek Harness pid $SERVICE_DSH_PID survived the proxy kill (orphan)"
    info "with the unit's default KillMode=control-group, systemd should clean the"
    info "cgroup up during the restart; the next check verifies that it did"
  else
    pass "no DeepSeek Harness process was left behind by the kill"
  fi

  if wait_healthy 90; then
    NEW_PROXY_PID=$(state_field "$STATE" pid)
    pass "systemd restarted the proxy (new pid ${NEW_PROXY_PID:-?}); health is OK again"
    if [ -n "$SURVIVING_DSH" ] && kill -0 "$SURVIVING_DSH" 2>/dev/null; then
      fail "orphan pid $SURVIVING_DSH is still running after the restart"
      info "collect: systemctl --user show $UNIT --property=KillMode --value"
    elif [ -n "$SURVIVING_DSH" ]; then
      pass "the leftover DeepSeek Harness was cleaned up during the restart"
    fi
  else
    fail "the proxy did not come back within 90s after being killed"
    info "this is the persistent-outage case from the report; collect:"
    info "  systemctl --user status $UNIT --no-pager"
    info "  journalctl --user -u $UNIT -n 80 --no-pager"
    info "  $BIN status --dsh-port $DSH_PORT --proxy-port $PROXY_PORT"
  fi
else
  warn "no recorded proxy pid to kill; skipping the restart check"
fi

# ---------------------------------------------------------------------------
step "stable URL end to end"
REDIRECT=$(curl -s -i --max-time 5 "http://127.0.0.1:${PROXY_PORT}/dsh" 2>/dev/null | sed -n 's/^[Ll]ocation: .*token=.*/present/p' | head -1)
if [ "$REDIRECT" = "present" ]; then
  pass "GET /dsh issues a token-bound redirect (token value not shown)"
else
  BODY_STATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${PROXY_PORT}/dsh" 2>/dev/null)
  fail "GET /dsh did not redirect with a token (status ${BODY_STATUS:-none})"
fi

# ---------------------------------------------------------------------------
step "boot persistence"
if [ "$DO_REBOOT" -eq 0 ]; then
  warn "reboot check skipped (--no-reboot)"
else
  echo "         This will reboot the machine. This SSH session will be lost."
  echo "         After it comes back, reconnect and run this script again with"
  echo "         --no-reboot: that verifies the service came up without you"
  echo "         logging in, which is the property being tested."
  if [ "$ASSUME_YES" -eq 1 ]; then
    REPLY=y
  else
    printf '         Reboot now? [y/N] '
    read -r REPLY || REPLY=n
  fi
  case "$REPLY" in
    y|Y|yes|YES)
      echo
      echo "Rebooting. After it comes back, reconnect and run:"
      echo "  $0 --no-reboot"
      sync
      exec sudo systemctl reboot
      ;;
    *)
      warn "reboot check skipped; verify manually with: sudo systemctl reboot"
      ;;
  esac
fi

# ---------------------------------------------------------------------------
step "summary"
if [ "$FAILURES" -eq 0 ]; then
  echo "all requested checks passed."
  exit 0
fi
echo "$FAILURES check(s) failed; see the output above."
exit 1
