package trader

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"nofx/store"
)

// Daily market-regime pre-filter & asymmetric portfolio sizing for factor_quant.
//
// In a clear uptrend (bull: benchmark close > EMA20 > EMA50) the portfolio tilts
// long (up to 20 longs, at most 10 shorts, gross allocated proportionally ~2:1).
// In a clear downtrend (bear: benchmark close < EMA20 < EMA50) it tilts short
// (up to 20 shorts, at most 10 longs, gross allocated proportionally ~2:1).
// In chop / neutral / insufficient benchmark history it preserves the symmetric
// target weights unchanged.

const (
	fqRegimeBull    = "bull"
	fqRegimeBear    = "bear"
	fqRegimeChop    = "chop"
	fqRegimeUnknown = "unknown"

	fqRegimeMinBars       = 60
	fqRegimeFavoredMax    = 20
	fqRegimeCounterMax    = 10
	fqRegimeVolWindow     = 30
	fqRegimeMaxNameWeight = 0.1
	fqRegimeMinListing    = 90
	fqRegimeRet7Quantile  = 0.9
)

type fqRegimeInfo struct {
	State       string  `json:"state"`       // bull | bear | chop | unknown
	Benchmark   string  `json:"benchmark"`   // BTCUSDT | SPY
	AsOf        string  `json:"as_of"`       // benchmark last bar date used
	Close       float64 `json:"close"`       // benchmark close
	EMA20       float64 `json:"ema20"`       // benchmark daily EMA20
	EMA50       float64 `json:"ema50"`       // benchmark daily EMA50
	LongCount   int     `json:"long_count"`  // active target longs
	ShortCount  int     `json:"short_count"` // active target shorts
	LongGross   float64 `json:"long_gross"`  // sum of positive target weights
	ShortGross  float64 `json:"short_gross"` // sum of |negative| target weights
	TiltEnabled bool    `json:"tilt_enabled"`
	Applied     bool    `json:"applied"`
	Reason      string  `json:"reason,omitempty"`
}

func fqRegimeLabelZH(state string) string {
	switch state {
	case fqRegimeBull:
		return "牛市"
	case fqRegimeBear:
		return "熊市"
	case fqRegimeChop:
		return "震荡"
	default:
		return "未知"
	}
}

func (info fqRegimeInfo) toStatus() *FactorQuantRegimeStatus {
	return &FactorQuantRegimeStatus{
		State:       info.State,
		Benchmark:   info.Benchmark,
		AsOf:        info.AsOf,
		Close:       info.Close,
		EMA20:       info.EMA20,
		EMA50:       info.EMA50,
		LongCount:   info.LongCount,
		ShortCount:  info.ShortCount,
		LongGross:   info.LongGross,
		ShortGross:  info.ShortGross,
		TiltEnabled: info.TiltEnabled,
		Applied:     info.Applied,
		Reason:      info.Reason,
	}
}

func (info fqRegimeInfo) summaryText() string {
	label := fqRegimeLabelZH(info.State)
	if info.Close > 0 && info.EMA20 > 0 && info.EMA50 > 0 {
		return fmt.Sprintf("市场趋势=%s（%s %.2f / EMA20 %.2f / EMA50 %.2f @ %s），多 %d 只(+%.1f%%) / 空 %d 只(-%.1f%%)",
			label, info.Benchmark, info.Close, info.EMA20, info.EMA50, info.AsOf,
			info.LongCount, info.LongGross*100, info.ShortCount, info.ShortGross*100)
	}
	if info.Reason != "" {
		return fmt.Sprintf("市场趋势=%s（%s），多 %d 只(+%.1f%%) / 空 %d 只(-%.1f%%)：%s",
			label, info.Benchmark, info.LongCount, info.LongGross*100, info.ShortCount, info.ShortGross*100, info.Reason)
	}
	return fmt.Sprintf("市场趋势=%s（%s），多 %d 只(+%.1f%%) / 空 %d 只(-%.1f%%)",
		label, info.Benchmark, info.LongCount, info.LongGross*100, info.ShortCount, info.ShortGross*100)
}

type fqDailyBar struct {
	Date  string
	Close float64
}

func fqEMA(xs []float64, period int) []float64 {
	if len(xs) == 0 || period <= 0 {
		return nil
	}
	out := make([]float64, len(xs))
	k := 2.0 / (float64(period) + 1.0)
	out[0] = xs[0]
	for i := 1; i < len(xs); i++ {
		out[i] = xs[i]*k + out[i-1]*(1.0-k)
	}
	return out
}

func fqDetectDailyRegime(benchmark string, bars []fqDailyBar) fqRegimeInfo {
	info := fqRegimeInfo{
		State:     fqRegimeUnknown,
		Benchmark: benchmark,
	}
	if len(bars) < fqRegimeMinBars {
		info.Reason = fmt.Sprintf("%s 日线不足 %d 根（当前 %d 根）", benchmark, fqRegimeMinBars, len(bars))
		return info
	}
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	e20 := fqEMA(closes, 20)
	e50 := fqEMA(closes, 50)
	lastIdx := len(bars) - 1
	last := closes[lastIdx]
	v20 := e20[lastIdx]
	v50 := e50[lastIdx]
	info.AsOf = bars[lastIdx].Date
	info.Close = last
	info.EMA20 = v20
	info.EMA50 = v50
	switch {
	case v20 > v50 && last > v20:
		info.State = fqRegimeBull
	case v20 < v50 && last < v20:
		info.State = fqRegimeBear
	default:
		info.State = fqRegimeChop
	}
	return info
}

func fqLoadDailyBars(path, maxDate string) ([]fqDailyBar, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	bars := make([]fqDailyBar, 0, len(rows))
	for _, row := range rows {
		if len(row) < 6 {
			continue
		}
		var dateStr string
		if err := json.Unmarshal(row[0], &dateStr); err != nil || dateStr == "" {
			continue
		}
		if maxDate != "" && dateStr > maxDate {
			continue
		}
		o, okO := fqParseJSONFloat(row[1])
		h, okH := fqParseJSONFloat(row[2])
		l, okL := fqParseJSONFloat(row[3])
		c, okC := fqParseJSONFloat(row[4])
		v, okV := fqParseJSONFloat(row[5])
		if !okO || !okH || !okL || !okC || !okV {
			continue
		}
		if o <= 0 || h <= 0 || l <= 0 || c <= 0 || v < 0 {
			continue
		}
		hiBound := math.Max(o, math.Max(l, c)) - 1e-8
		loBound := math.Min(o, math.Min(h, c)) + 1e-8
		if h < hiBound || l > loBound {
			continue
		}
		bars = append(bars, fqDailyBar{Date: dateStr, Close: c})
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, nil
}

func fqParseJSONFloat(raw json.RawMessage) (float64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func fqResolveDailyAndPerpPaths(targetsPath string, fq *store.FactorQuantConfig) (dailyDir, perpMapPath string) {
	market := store.FactorQuantMarketCrypto
	hasOverride := false
	if fq != nil {
		if fq.Market != "" {
			market = fq.Market
		}
		hasOverride = strings.TrimSpace(fq.TargetsPath) != ""
	}
	if targetsPath != "" {
		sibling := filepath.Join(filepath.Dir(targetsPath), "daily")
		if st, err := os.Stat(sibling); err == nil && st.IsDir() {
			dailyDir = sibling
		}
	}
	if dailyDir == "" && !hasOverride {
		candidate := filepath.Join(factorMiningRoot(), market, "daily")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			dailyDir = candidate
		}
	}
	if market == store.FactorQuantMarketUS {
		if dailyDir != "" {
			for _, rel := range []string{
				filepath.Join("..", "..", "cache", "usstock_symbols.json"),
				filepath.Join("..", "cache", "usstock_symbols.json"),
			} {
				cand := filepath.Join(filepath.Dir(dailyDir), rel)
				if _, err := os.Stat(cand); err == nil {
					perpMapPath = cand
					break
				}
			}
		}
		if perpMapPath == "" && !hasOverride {
			cand := filepath.Join(factorMiningRoot(), "..", "cache", "usstock_symbols.json")
			if _, err := os.Stat(cand); err == nil {
				perpMapPath = cand
			} else if _, err := os.Stat(filepath.Join("data", "cache", "usstock_symbols.json")); err == nil {
				perpMapPath = filepath.Join("data", "cache", "usstock_symbols.json")
			}
		}
	}
	return dailyDir, perpMapPath
}

func fqLoadPerpMap(path string) (map[string]string, bool) {
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var doc struct {
		Version int `json:"version"`
		Symbols []struct {
			Underlying string `json:"underlying"`
			PerpSymbol string `json:"perp_symbol"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Version != 2 || doc.Symbols == nil {
		return nil, false
	}
	out := make(map[string]string, len(doc.Symbols))
	for _, s := range doc.Symbols {
		if s.Underlying != "" {
			out[s.Underlying] = strings.TrimSpace(s.PerpSymbol)
		}
	}
	return out, true
}

func fqCalcVol30(bars []fqDailyBar) (float64, bool) {
	n := len(bars)
	if n < fqRegimeVolWindow+1 {
		return 0, false
	}
	tail := bars[n-(fqRegimeVolWindow+1):]
	rets := make([]float64, fqRegimeVolWindow)
	mean := 0.0
	for i := 0; i < fqRegimeVolWindow; i++ {
		if tail[i].Close <= 0 || tail[i+1].Close <= 0 {
			return 0, false
		}
		r := tail[i+1].Close/tail[i].Close - 1.0
		rets[i] = r
		mean += r
	}
	mean /= float64(fqRegimeVolWindow)
	ss := 0.0
	for _, r := range rets {
		d := r - mean
		ss += d * d
	}
	sd := math.Sqrt(ss / float64(fqRegimeVolWindow-1))
	if math.IsNaN(sd) || math.IsInf(sd, 0) || sd <= 0 {
		return 0, false
	}
	return sd, true
}

// fqCappedInvVol allocates gross across symbols proportionally to 1/vol with a
// per-name cap, matching scripts/factor_mining/quant_targets.py:_capped_invvol.
func fqCappedInvVol(vols map[string]float64, symbols []string, gross, cap float64) map[string]float64 {
	weights := make(map[string]float64, len(symbols))
	if len(symbols) == 0 || gross <= 0 {
		return weights
	}
	minVol := math.Inf(1)
	for _, s := range symbols {
		v := vols[s]
		if v <= 0 {
			return weights
		}
		if v < minVol {
			minVol = v
		}
	}
	inverse := make(map[string]float64, len(symbols))
	for _, s := range symbols {
		inverse[s] = minVol / vols[s]
	}
	active := append([]string(nil), symbols...)
	remaining := gross
	for len(active) > 0 && remaining > 1e-14 {
		invSum := 0.0
		for _, s := range active {
			invSum += inverse[s]
		}
		if invSum <= 0 {
			break
		}
		var capped, next []string
		for _, s := range active {
			w := inverse[s] / invSum * remaining
			if w >= cap {
				weights[s] = cap
				capped = append(capped, s)
			} else {
				next = append(next, s)
			}
		}
		if len(capped) == 0 {
			for _, s := range active {
				weights[s] = inverse[s] / invSum * remaining
			}
			break
		}
		remaining -= float64(len(capped)) * cap
		active = next
	}
	return weights
}

func fqQuantileLinear(vals []float64, q float64) (float64, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo], true
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac, true
}

func fqStampWeightStats(info *fqRegimeInfo, weights map[string]float64) {
	info.LongCount, info.ShortCount = 0, 0
	info.LongGross, info.ShortGross = 0, 0
	for _, w := range weights {
		if w > 0 {
			info.LongCount++
			info.LongGross += w
		} else if w < 0 {
			info.ShortCount++
			info.ShortGross += -w
		}
	}
}

func fqDailySymbolFile(sym, market string) string {
	if market == store.FactorQuantMarketUS {
		return strings.TrimSuffix(sym, "BUSDT") + ".json"
	}
	return sym + ".json"
}

func fqRet7OnDate(bars []fqDailyBar, asOf string) (float64, bool) {
	if len(bars) == 0 || asOf == "" {
		return 0, false
	}
	tAsOf, err := time.Parse(factorDateFmt, asOf)
	if err != nil {
		return 0, false
	}
	prevDate := tAsOf.AddDate(0, 0, -7).Format(factorDateFmt)
	var cur, prev float64
	hasCur, hasPrev := false, false
	for _, b := range bars {
		if b.Date == asOf {
			cur, hasCur = b.Close, true
		} else if b.Date == prevDate {
			prev, hasPrev = b.Close, true
		}
	}
	if !hasCur || !hasPrev || prev <= 0 {
		return 0, false
	}
	return cur/prev - 1.0, true
}

func fqRound6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// fqResolveRegimeTargets inspects the benchmark's daily trend and, when RegimeTilt
// is enabled and the market is in a clear bull or bear regime, builds tilted
// target weights (up to 20 favored / max 10 counter, ~2:1 gross allocation).
// Otherwise it returns the original targets unchanged alongside the observed
// regime metadata.
func fqResolveRegimeTargets(t *quantTargets, fq *store.FactorQuantConfig, targetsPath string) (*quantTargets, fqRegimeInfo) {
	market := store.FactorQuantMarketCrypto
	if fq != nil && fq.Market != "" {
		market = fq.Market
	} else if t != nil && t.Market != "" {
		market = t.Market
	}
	benchmark := "BTCUSDT"
	if market == store.FactorQuantMarketUS {
		benchmark = "SPY"
	}
	enabled := fq.EffectiveRegimeTilt()
	dailyDir, perpMapPath := fqResolveDailyAndPerpPaths(targetsPath, fq)

	var asOf string
	if t != nil {
		asOf = t.AsOf
	}
	var info fqRegimeInfo
	if dailyDir == "" {
		info = fqRegimeInfo{
			State:       fqRegimeUnknown,
			Benchmark:   benchmark,
			TiltEnabled: enabled,
			Reason:      "未找到日线目录，沿用基础目标权重",
		}
	} else {
		benchBars, err := fqLoadDailyBars(filepath.Join(dailyDir, benchmark+".json"), asOf)
		if err != nil {
			info = fqRegimeInfo{
				State:       fqRegimeUnknown,
				Benchmark:   benchmark,
				TiltEnabled: enabled,
				Reason:      fmt.Sprintf("读取 %s 日线失败（%v），沿用基础目标权重", benchmark, err),
			}
		} else {
			info = fqDetectDailyRegime(benchmark, benchBars)
			info.TiltEnabled = enabled
		}
	}

	if t != nil {
		fqStampWeightStats(&info, t.Weights)
	}
	if !enabled {
		if info.Reason == "" {
			info.Reason = "策略已关闭趋势倾斜开关，保持对称权重"
		}
		return t, info
	}
	if t == nil || t.Status != quantStatusOK {
		return t, info
	}
	if info.State != fqRegimeBull && info.State != fqRegimeBear {
		if info.State == fqRegimeChop && info.Reason == "" {
			info.Reason = "震荡行情，保持对称权重"
		}
		return t, info
	}
	if len(t.Pct) < 15 || dailyDir == "" {
		if len(t.Pct) < 15 && info.Reason == "" {
			info.Reason = fmt.Sprintf("候选分位数量不足（%d < 15），沿用基础目标权重", len(t.Pct))
		}
		return t, info
	}

	type pctPair struct {
		sym string
		pct float64
	}
	ordered := make([]pctPair, 0, len(t.Pct))
	for sym, p := range t.Pct {
		if !math.IsNaN(p) && !math.IsInf(p, 0) {
			ordered = append(ordered, pctPair{sym: sym, pct: p})
		}
	}
	if len(ordered) < 15 {
		info.Reason = fmt.Sprintf("有效分位数量不足（%d < 15），沿用基础目标权重", len(ordered))
		return t, info
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].pct != ordered[j].pct {
			return ordered[i].pct < ordered[j].pct
		}
		return ordered[i].sym < ordered[j].sym
	})

	n := len(ordered)
	counterN := n / 3
	if counterN < 1 {
		counterN = 1
	}
	if counterN > fqRegimeCounterMax {
		counterN = fqRegimeCounterMax
	}
	favoredN := n - counterN
	if favoredN > fqRegimeFavoredMax {
		favoredN = fqRegimeFavoredMax
	}

	var longN, shortN int
	if info.State == fqRegimeBull {
		longN, shortN = favoredN, counterN
	} else {
		longN, shortN = counterN, favoredN
	}
	shortCands := make([]string, 0, shortN)
	for i := 0; i < shortN; i++ {
		shortCands = append(shortCands, ordered[i].sym)
	}
	longCands := make([]string, 0, longN)
	for i := n - longN; i < n; i++ {
		longCands = append(longCands, ordered[i].sym)
	}

	barsBySym := make(map[string][]fqDailyBar, len(ordered))
	loadBars := func(sym string) []fqDailyBar {
		if b, ok := barsBySym[sym]; ok {
			return b
		}
		b, err := fqLoadDailyBars(filepath.Join(dailyDir, fqDailySymbolFile(sym, market)), asOf)
		if err != nil {
			barsBySym[sym] = nil
			return nil
		}
		barsBySym[sym] = b
		return b
	}

	ret7Threshold := math.Inf(1)
	hasRet7Threshold := false
	if market == store.FactorQuantMarketCrypto {
		ret7Vals := make([]float64, 0, len(ordered))
		for _, item := range ordered {
			if r7, ok := fqRet7OnDate(loadBars(item.sym), asOf); ok {
				ret7Vals = append(ret7Vals, r7)
			}
		}
		if thr, ok := fqQuantileLinear(ret7Vals, fqRegimeRet7Quantile); ok {
			ret7Threshold, hasRet7Threshold = thr, true
		}
	}

	var perps map[string]string
	hasPerpMap := false
	if market == store.FactorQuantMarketUS {
		perps, hasPerpMap = fqLoadPerpMap(perpMapPath)
	}

	vols := make(map[string]float64, len(longCands)+len(shortCands))
	var longs []string
	for _, sym := range longCands {
		if t.Excluded[sym] != "" {
			continue
		}
		if market == store.FactorQuantMarketUS && hasPerpMap {
			underlying := strings.TrimSuffix(sym, "BUSDT")
			if _, ok := perps[underlying]; !ok {
				continue
			}
		}
		vol, ok := fqCalcVol30(loadBars(sym))
		if !ok {
			continue
		}
		vols[sym] = vol
		longs = append(longs, sym)
	}

	var shorts []string
	for _, sym := range shortCands {
		if t.Excluded[sym] != "" {
			continue
		}
		bars := loadBars(sym)
		if market == store.FactorQuantMarketCrypto {
			if len(bars) < fqRegimeMinListing {
				continue
			}
			if hasRet7Threshold {
				if r7, ok := fqRet7OnDate(bars, asOf); ok && r7 >= ret7Threshold {
					continue
				}
			}
		} else if market == store.FactorQuantMarketUS && hasPerpMap {
			underlying := strings.TrimSuffix(sym, "BUSDT")
			if perp, ok := perps[underlying]; !ok || perp == "" {
				continue
			}
		}
		vol, ok := fqCalcVol30(bars)
		if !ok {
			continue
		}
		vols[sym] = vol
		shorts = append(shorts, sym)
	}

	if len(longs) == 0 || len(shorts) == 0 {
		info.Reason = fmt.Sprintf("趋势筛选后有效多(%d)/空(%d)不足，沿用基础目标权重", len(longs), len(shorts))
		return t, info
	}

	total := float64(len(longs) + len(shorts))
	var longGross, shortGross float64
	if info.State == fqRegimeBull {
		longGross = math.Max(float64(len(longs))/total, 0.5)
		shortGross = 1.0 - longGross
	} else {
		shortGross = math.Max(float64(len(shorts))/total, 0.5)
		longGross = 1.0 - shortGross
	}

	longW := fqCappedInvVol(vols, longs, longGross, fqRegimeMaxNameWeight)
	shortW := fqCappedInvVol(vols, shorts, shortGross, fqRegimeMaxNameWeight)
	scale := 1.0
	if t.K != nil {
		scale = *t.K
	}
	merged := make(map[string]float64, len(longW)+len(shortW))
	for s, w := range longW {
		if rw := fqRound6(w * scale); rw > 0 {
			merged[s] = rw
		}
	}
	for s, w := range shortW {
		if rw := fqRound6(-w * scale); rw < 0 {
			merged[s] = rw
		}
	}
	if len(merged) == 0 {
		info.Reason = "趋势倾斜后权重为空，沿用基础目标权重"
		return t, info
	}

	out := *t
	out.Weights = merged
	fqStampWeightStats(&info, merged)
	info.Applied = true
	info.Reason = fmt.Sprintf("%s（最多%d多/%d空）", fqRegimeLabelZH(info.State), longN, shortN)
	return &out, info
}
