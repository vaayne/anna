#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

find_gateway_pids() {
  ps -axo pid=,command= | awk '
    /mise run:gateway|(^|[[:space:]])\.\/bin\/anna gateway([[:space:]]|$)|(^|[[:space:]])bin\/anna gateway([[:space:]]|$)/ {
      if ($0 !~ /awk/ && $0 !~ /restart-gateway\.sh/) print $1
    }
  '
}

PIDS="$(find_gateway_pids)"

if [[ -n "$PIDS" ]]; then
  echo "Stopping existing anna gateway process(es):"
  echo "$PIDS"
  kill $PIDS || true

  for pid in $PIDS; do
    for _ in {1..20}; do
      if ! kill -0 "$pid" 2>/dev/null; then
        break
      fi
      sleep 0.5
    done

    if kill -0 "$pid" 2>/dev/null; then
      echo "Force killing PID $pid"
      kill -9 "$pid" || true
    fi
  done
else
  echo "No running anna gateway process found."
fi

echo "Starting anna gateway with mise..."
mkdir -p "$ROOT_DIR/.agents/workspace"
nohup mise run:gateway > "$ROOT_DIR/.agents/workspace/anna-gateway.log" 2>&1 &

echo "Started. Logs: $ROOT_DIR/.agents/workspace/anna-gateway.log"
echo "New PID: $!"
