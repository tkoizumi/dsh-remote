#!/usr/bin/env bash
#
# Install dsh-remote as a supervised systemd user service: the fix for the
# 2026-10-08 outage, where the proxy died and nothing restarted it.
#
# Order matters. The currently running proxy (started in the foreground) holds
# port 3081 and its DeepSeek Harness holds port 3080. If the unit is installed
# and enabled before those are released, the service starts, fails to bind, and
# Restart=on-failure loops on the same conflict.
#
# Safe to re-run: it reconciles rather than assuming a clean starting point.
#
#   bash setup-supervision.sh --lan
#   bash setup-supervision.sh              # Tailscale-serve only
set -uo pipefail

BIN="${DSH_REMOTE_BIN:-dsh-remote}"
UNIT="dsh-remote.service"
UNIT_PATH="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/${UNIT}"
PROXY_PORT=3081
DSH_PORT=3080
LAN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --lan) LAN=1; shift ;;
    --proxy-port) PROXY_PORT="$2"; shift 2 ;;
    --dsh-port) DSH_PORT="$2"; shift 2 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

step() { printf '\n== %s ==\n' "$1"; }
ok()   { printf '  [ok]   %s\n' "$1"; }
bad()  { printf '  [FAIL] %s\n' "$1"; }
warn() { printf '  [warn] %s\n' "$1"; }
info() { printf '         %s\n' "$1"; }

healthy() {
  curl -fsS --max-time 3 "http://127.0.0.1:${PROXY_PORT}/__dsh_remote/health" >/dev/null 2>&1
}

wait_healthy() {
  local deadline=$((SECONDS + ${1:-90}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    healthy && return 0
    sleep 2
  done
  return 1
}

# ---------------------------------------------------------------------------
step "prerequisites"
if ! command -v "$BIN" >/dev/null 2>&1; then
  bad "$BIN not found in PATH"
  exit 1
fi
VERSION="$("$BIN" version 2>/dev/null)"
ok "$BIN $VERSION"

if ! systemctl --user show-environment >/dev/null 2>&1; then
  bad "cannot reach the systemd user bus"
  info "run this as the user that will own the service, in a normal login session"
  exit 1
fi
ok "systemd user bus reachable"

# The installed service must be a build that can reclaim a leftover DeepSeek
# Harness; 0.1.8 and earlier cannot, and would loop on the port conflict.
case "$VERSION" in
  *0.1.10*|*0.1.11*|*0.1.12*) ok "version can reclaim an orphaned DeepSeek Harness" ;;
  *) warn "this build predates orphan reclamation; expect a port-conflict loop"
     info "upgrade first: brew update && brew upgrade --cask dsh-remote" ;;
esac

# ---------------------------------------------------------------------------
step "release the ports held by the foreground instance"
if healthy; then
  info "a proxy is currently serving on $PROXY_PORT; stopping it cleanly"
  "$BIN" stop --proxy-port "$PROXY_PORT" || warn "stop reported a problem"
  sleep 2
else
  info "nothing healthy on $PROXY_PORT"
  # Clear any leftover run state so `start` is not blocked by a dead record.
  "$BIN" stop --proxy-port "$PROXY_PORT" >/dev/null 2>&1 || true
fi

blocked=0
for p in "$DSH_PORT" "$PROXY_PORT"; do
  if ss -ltn 2>/dev/null | grep -q ":${p}\b"; then
    bad "port $p is still in use"
    info "identify the holder: ss -ltnp | grep :$p"
    blocked=1
  fi
done
if [ "$blocked" -eq 0 ]; then
  ok "ports $DSH_PORT and $PROXY_PORT are free"
else
  info "resolve the holder, then re-run this script"
  exit 1
fi

# ---------------------------------------------------------------------------
step "install the unit"
# --start=false is deliberate. `install` would otherwise enable and start the
# unit itself, and this script then restarts it. Starting twice races: two
# dsh-remote processes each launch a DeepSeek Harness, one loses the port, and
# the loser's child is left orphaned. Install with the unit stopped, then start
# it exactly once below.
INSTALL_ARGS=(install --force --start=false)
[ "$LAN" -eq 1 ] && INSTALL_ARGS+=(--lan)
if "$BIN" "${INSTALL_ARGS[@]}"; then
  ok "${INSTALL_ARGS[*]}"
else
  bad "install failed"
  exit 1
fi

if [ -f "$UNIT_PATH" ]; then
  ok "unit written to $UNIT_PATH"
else
  bad "no unit at $UNIT_PATH"
  exit 1
fi

# ---------------------------------------------------------------------------
step "verify the unit will actually recover"
RESTART="$(systemctl --user show "$UNIT" --property=Restart --value 2>/dev/null)"
case "$RESTART" in
  always|on-failure|on-abnormal|on-abort) ok "Restart=$RESTART" ;;
  *) bad "Restart=${RESTART:-<unset>} will not recover an unexpected exit"
     info "the installed unit should use Restart=on-failure; reinstall it" ;;
esac

if systemctl --user is-active --quiet "$UNIT"; then
  ok "$UNIT is active"
else
  warn "$UNIT is not active yet"
fi

# ---------------------------------------------------------------------------
step "survive logout and reboot"
if loginctl enable-linger "$USER" 2>/dev/null; then
  ok "enabled lingering"
else
  bad "could not enable lingering without elevation"
  info "run: sudo loginctl enable-linger $USER"
  info "without it the service does not start after a reboot with nobody logged in"
fi
LINGER="$(loginctl show-user "$USER" --property=Linger --value 2>/dev/null || echo unknown)"
info "Linger=$LINGER"

# ---------------------------------------------------------------------------
step "bring the service up"
if systemctl --user restart "$UNIT"; then
  ok "restarted $UNIT"
else
  bad "could not restart $UNIT"
  info "systemctl --user status $UNIT --no-pager"
  exit 1
fi

if wait_healthy 90; then
  ok "proxy healthy on $PROXY_PORT"
else
  bad "proxy did not become healthy within 90s"
  info "journalctl --user -u $UNIT -n 80 --no-pager"
  info "$BIN doctor --dsh-port $DSH_PORT --proxy-port $PROXY_PORT"
  exit 1
fi

# ---------------------------------------------------------------------------
step "end-to-end verdict"
"$BIN" doctor --dsh-port "$DSH_PORT" --proxy-port "$PROXY_PORT" || true

step "done"
cat <<EOF
The proxy is now supervised. What changed:

  - Restart=${RESTART}: an unexpected exit is recovered automatically.
  - Lingering: the service starts at boot without an interactive login.
  - Every run appends to ~/.local/state/dsh-remote/dsh-remote.log, so the next
    failure can be explained afterwards.

Prove the recovery path rather than trusting it:

  scripts/verify-supervision.sh --no-reboot

That SIGKILLs the proxy's own pid and confirms systemd brings it back.
EOF
