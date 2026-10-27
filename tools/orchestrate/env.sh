# Shared settings for the orchestrate scripts. Sourced, not run.
# Values come from the environment, then from $ORCH_ENV (default: local/orchestrate.env
# next to this file, gitignored). Nothing here holds a token: tea and git use their own
# stored logins (tea login, Git Credential Manager).
ORCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ORCH_ENV="${ORCH_ENV:-$ORCH_DIR/local/orchestrate.env}"
# ORCH_* values already in the environment win over the file.
if [ -f "$ORCH_ENV" ]; then
  _orch_env_saved=$(declare -p $(compgen -v ORCH_) 2>/dev/null)
  # shellcheck disable=SC1090
  . "$ORCH_ENV"
  eval "$_orch_env_saved"; unset _orch_env_saved
fi

ORCH_RT_URL="${ORCH_RT_URL:-http://127.0.0.1:18787}"
ORCH_RT_BIN="${ORCH_RT_BIN:-roundtable}"
ORCH_TEA="${ORCH_TEA:-tea}"
ORCH_BASE="${ORCH_BASE:-main}"
ORCH_STATE="${ORCH_STATE:-$HOME/.rt-orchestrate}"
if [ -z "$ORCH_PYTHON" ]; then
  if command -v python >/dev/null 2>&1; then ORCH_PYTHON=python; else ORCH_PYTHON=python3; fi
fi

rt() { "$ORCH_RT_BIN" rt --url "$ORCH_RT_URL" "$@"; }
py() { "$ORCH_PYTHON" -X utf8 "$@"; }
die() { echo "$*" >&2; exit 2; }
# need VAR... fails with exit 2 when a required setting is empty.
need() { local v; for v in "$@"; do [ -n "${!v}" ] || die "$v is not set (environment or $ORCH_ENV)"; done; }
state_dir() { mkdir -p "$ORCH_STATE" && echo "$ORCH_STATE"; }
# winpath prints a path the way a model on this host reads it (Windows form under Git Bash).
winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else echo "$1"; fi; }
