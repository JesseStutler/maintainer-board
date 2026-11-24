#!/usr/bin/env bash
# Builds the binary when sources changed, then starts Maintainer Board.
# Override defaults with MB_ADDR, MB_DB, MB_AO_URL, MB_REPO, MB_SYNC_INTERVAL;
# extra arguments are passed through to the binary.
set -euo pipefail

cd "$(dirname "$0")"

# gh, copilot and codex are looked up on PATH; the AI summarizer needs them.
export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"

binary=./maintainer-board
if [ ! -x "$binary" ] || [ -n "$(find cmd internal web go.mod go.sum -newer "$binary" -print -quit 2>/dev/null)" ]; then
  echo "run.sh: building $binary" >&2
  go build -o "$binary" ./cmd/maintainer-board
fi

exec "$binary" \
  --addr "${MB_ADDR:-127.0.0.1:3012}" \
  --db "${MB_DB:-$PWD/data/board.db}" \
  --ao-url "${MB_AO_URL:-http://127.0.0.1:3001}" \
  --default-repo "${MB_REPO:-volcano-sh/volcano}" \
  --sync-interval "${MB_SYNC_INTERVAL:-120s}" \
  "$@"
