#!/bin/zsh
# OpenBB sidecar launcher (user directive 2026-09-25): the OpenBB Platform
# REST API as an OPTIONAL data-enrichment service for nofx — kline tier-3
# fallback + per-symbol news. Idempotent; logs to /tmp/openbb-api.log.
set -e
cd "$(dirname "$0")/.."
PORT="${OPENBB_PORT:-6900}"
if lsof -nP -iTCP:$PORT -sTCP:LISTEN >/dev/null 2>&1; then
  echo "openbb sidecar already listening on :$PORT"
  exit 0
fi
if [ ! -x .venv-openbb/bin/python ]; then
  echo "venv missing — run: brew install uv && uv venv --python 3.12 .venv-openbb && uv pip install --python .venv-openbb/bin/python openbb"
  exit 1
fi
nohup .venv-openbb/bin/python -m uvicorn openbb_core.api.rest_api:app \
  --host 127.0.0.1 --port $PORT --workers 1 >> /tmp/openbb-api.log 2>&1 &
echo "openbb sidecar starting on :$PORT (pid $!)"
