#!/usr/bin/env bash
# Builds the binary when sources changed, then starts Maintainer Board.
# Override defaults with MB_ADDR, MB_DB, MB_RUNTIME, MB_WORKSPACE, MB_REPO,
# MB_SYNC_INTERVAL, MB_AO_URL; extra arguments are passed through to the binary.
set -euo pipefail

cd "$(dirname "$0")"

# gh, copilot and codex are looked up on PATH.
export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"

binary=./maintainer-board
if [ ! -x "$binary" ] || [ -n "$(find cmd internal web go.mod go.sum -newer "$binary" -print -quit 2>/dev/null)" ]; then
  echo "run.sh: building $binary" >&2
  go build -o "$binary" ./cmd/maintainer-board
fi

args=(
  --addr "${MB_ADDR:-127.0.0.1:3012}"
  --db "${MB_DB:-$PWD/data/board.db}"
  --runtime "${MB_RUNTIME:-native}"
  --default-repo "${MB_REPO:-volcano-sh/volcano}"
  --sync-interval "${MB_SYNC_INTERVAL:-120s}"
)
if [ -n "${MB_WORKSPACE:-}" ]; then
  args+=(--workspace "$MB_WORKSPACE")
fi
if [ "${MB_RUNTIME:-native}" = "ao" ] || [ -n "${MB_AO_URL:-}" ]; then
  args+=(--ao-url "${MB_AO_URL:-http://127.0.0.1:3001}")
fi

exec "$binary" "${args[@]}" "$@"
