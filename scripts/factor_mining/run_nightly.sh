#!/bin/sh
set -u
FM_SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
FM_REPO_DIR=$(CDPATH= cd -- "$FM_SCRIPT_DIR/../.." && pwd)
AR_PYTHON=${AR_PYTHON:-/Users/zhangyun/workspace/nofx/.venv/bin/python}
FM_ROOT=${FACTOR_MINING_DIR:-/Users/zhangyun/workspace/nofx/data/factor_mining}
mkdir -p "$FM_ROOT"
FM_LOG=$FM_ROOT/nightly.log
if [ -f "$FM_LOG" ] && [ "$(wc -c < "$FM_LOG")" -gt 2097152 ]; then
    : > "$FM_LOG"
fi
cd "$FM_REPO_DIR" || exit 1
PYTHONPATH="$FM_REPO_DIR${PYTHONPATH:+:$PYTHONPATH}"
PYTHONDONTWRITEBYTECODE=1
export PYTHONPATH PYTHONDONTWRITEBYTECODE
# Data pre-flight: the Binance proxy occasionally drops for minutes. Before each market's
# mining run, retry a fetch-only pass (incremental, so cheap) up to FM_FETCH_RETRIES rounds,
# FM_FETCH_RETRY_SLEEP seconds apart. The extra waiting (sleeps plus later rounds) is capped
# at FM_FETCH_BUDGET seconds in total across both markets (default 2400 = 40 min), so the
# job cannot slip into the next day. When it never succeeds we log `fetch preflight FAILED`
# and carry on exactly as before (stale data is then used and recorded as a fetch error).
fm_uint() { case $1 in ''|*[!0-9]*) echo "$2" ;; *) echo "$1" ;; esac; }
FM_FETCH_RETRIES=$(fm_uint "${FM_FETCH_RETRIES:-4}" 4)
FM_FETCH_RETRY_SLEEP=$(fm_uint "${FM_FETCH_RETRY_SLEEP:-600}" 600)
FM_FETCH_BUDGET=$(fm_uint "${FM_FETCH_BUDGET:-2400}" 2400)
FM_FETCH_DEADLINE=$(( $(date +%s) + FM_FETCH_BUDGET ))
fetch_preflight() {
    fp_market=$1
    fp_round=1
    [ "$FM_FETCH_RETRIES" -gt 0 ] || return 0
    while :; do
        echo "fetch preflight $fp_market round $fp_round/$FM_FETCH_RETRIES $(date '+%H:%M:%S')"
        "$AR_PYTHON" -m scripts.factor_mining.run_mining --market "$fp_market" --fetch-only
        fp_rc=$?
        if [ "$fp_rc" -eq 0 ]; then
            echo "fetch preflight OK: $fp_market"
            return 0
        fi
        if [ "$fp_rc" -eq 2 ]; then
            echo "fetch preflight FAILED: $fp_market (another factor-mining run holds the lock)"
            return 1
        fi
        if [ "$fp_round" -ge "$FM_FETCH_RETRIES" ]; then
            echo "fetch preflight FAILED: $fp_market after $fp_round rounds (exit $fp_rc); continuing with existing data"
            return 1
        fi
        if [ $(( $(date +%s) + FM_FETCH_RETRY_SLEEP )) -ge "$FM_FETCH_DEADLINE" ]; then
            echo "fetch preflight FAILED: $fp_market after $fp_round rounds (exit $fp_rc, retry budget ${FM_FETCH_BUDGET}s exhausted); continuing with existing data"
            return 1
        fi
        echo "fetch preflight $fp_market exit $fp_rc, retrying in ${FM_FETCH_RETRY_SLEEP}s"
        sleep "$FM_FETCH_RETRY_SLEEP"
        fp_round=$(( fp_round + 1 ))
    done
}
for FM_MARKET in crypto us; do
    (
        echo "=== $FM_MARKET $(date '+%Y-%m-%d %H:%M:%S') ==="
        fetch_preflight "$FM_MARKET" || true
        "$AR_PYTHON" -m scripts.factor_mining.run_mining --market "$FM_MARKET" \
            --mutations "${FM_MUTATIONS:-1500}" --llm "${FM_LLM:-60}" || \
            echo "$FM_MARKET exited non-zero"
    ) >> "$FM_LOG" 2>&1
done
(
    echo "=== promotion report $(date '+%Y-%m-%d %H:%M:%S') ==="
    NOFX_DB="${NOFX_DB:-/Users/zhangyun/workspace/nofx/data/data.db}" \
    FACTOR_MINING_DIR="$FM_ROOT" \
    PROMOTION_DIR="${PROMOTION_DIR:-/Users/zhangyun/workspace/nofx/data/promotion}" \
    "$AR_PYTHON" -m scripts.promotion.report || \
        echo "promotion report exited non-zero"
) >> "$FM_LOG" 2>&1
