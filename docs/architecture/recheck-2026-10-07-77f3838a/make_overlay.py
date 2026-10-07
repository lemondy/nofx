"""Rebuild read-only Go audit overlay from the repository root (77f3838a)."""
import json
from pathlib import Path

root = Path(__file__).resolve().parents[3]
evidence = Path(__file__).resolve().parent
work = Path('/private/tmp/nofx-audit77')
work.mkdir(exist_ok=True)
replace = {}
for pkg, filename in [('trader', 'trader_test.go.txt'),
                      ('market/breakout', 'market_test.go.txt'),
                      ('kernel', 'kernel_test.go.txt')]:
    replace[str(root / pkg / 'audit77_overlay_test.go')] = str(evidence / filename)

source = root / 'kernel/engine_data_binance.go'
old = 'func BTC4hTrendCloses(limit int) []float64 { return binanceBTC4hCloses(limit) }'
new = '''var Audit77BTCClosesFn func(int) []float64
func BTC4hTrendCloses(limit int) []float64 {
 if Audit77BTCClosesFn != nil { return Audit77BTCClosesFn(limit) }
 return binanceBTC4hCloses(limit)
}'''
s = source.read_text()
assert s.count(old) == 1, 'BTC boundary changed; review before adapting overlay'
out = work / 'engine_data_binance.go'
out.write_text(s.replace(old, new))
replace[str(source)] = str(out)

source = root / 'market/data.go'
s = source.read_text()
old = 'func GetWithTimeframesForExchange(symbol string, timeframes []string, primaryTimeframe string, count int, exchange string, livePrice float64) (*Data, error) {'
new = 'var Audit77TimeframesFn func(string, []string, string, int, string, float64) (*Data, error)\n' + old + '\n if Audit77TimeframesFn != nil { return Audit77TimeframesFn(symbol,timeframes,primaryTimeframe,count,exchange,livePrice) }'
assert s.count(old) == 1, 'Strategy fetch boundary changed'
s = s.replace(old, new)
old = 'func GetWithExchangeAndPrice(symbol, exchange string, livePrice float64) (*Data, error) {'
new = 'var Audit77QuoteDataFn func(string, string, float64) (*Data, error)\n' + old + '\n if Audit77QuoteDataFn != nil { return Audit77QuoteDataFn(symbol,exchange,livePrice) }'
assert s.count(old) == 1, 'Generic quote boundary changed'
s = s.replace(old, new)
out = work / 'data.go'
out.write_text(s)
replace[str(source)] = str(out)

# Reuse earlier independent signed-short and worker recovery checks.
old_evidence = root / 'docs/architecture/review-2026-10-06/fix-verification'
replace[str(root / 'trader/audit77_previous_test.go')] = str(old_evidence / 'trader_verify_test.go.txt')
replace[str(root / 'market/breakout/audit77_previous_test.go')] = str(old_evidence / 'market_verify_test.go.txt')
(work / 'overlay.json').write_text(json.dumps({'Replace': replace}, indent=2))
print(work / 'overlay.json')
