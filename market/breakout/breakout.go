package breakout

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Direction of the monitored event.
const (
	DirUp   = "breakout"
	DirDown = "breakdown"
)

// Dimension weights per the design doc.
const (
	wPrice    = 0.25
	wVolume   = 0.20
	wFlow     = 0.20
	wOI       = 0.15
	wFunding  = 0.10
	wMomentum = 0.10
)

// Quality factors.
const (
	alphaHealthyOI = 1.0  // price and OI both moving in trade direction (new positions)
	alphaWeakOI    = 0.6  // OI shrinking while price moves (forced close / squeeze)
	alphaNeutral   = 0.8  // OI roughly flat
	betaCrowded    = 0.85 // funding percentile ≥90 (up) / ≤10 (down) aligned with direction
	confirmPenalty = 0.5  // price fell back through the key level after the cross
)

// Crowd penalty (2026-09-06): a move the whole market is already riding
// (funding extreme, accounts skewed, OI piling in) mean-reverts violently —
// longs get long-squeezed on a dip, shorts squeezed on a bounce. The six
// dimensions alone gave funding just 0.10 weight and no "is this side already
// overcrowded" verdict; this is the direction-aware symmetric counterpart of
// shortscan's Crowding component. Max score haircut 30%.
const (
	crowdMaxPenalty = 0.30
	crowdFundW      = 0.45
	crowdOIW        = 0.25
	crowdLSW        = 0.30
)

// Extended-chase penalty (2026-09-06): an "extended" pattern (crossed the
// level earlier, never retested) is a momentum chase — historically the
// lowest-win-rate entry style. The label alone left the score untouched and
// an extended symbol could still grade strong on Price+Volume+Flow; now the
// chase is discounted at the source instead of relying on downstream vetting.
//
// SINGLE charge by design (user audit 2026-09-17): this multiplier is the
// only extended haircut. The Price dim used to ALSO take ×0.8 for the same
// 1h extended fact — dim discount feeding the 1h TF score, then this ×0.65
// on the combined result — stacking to ≈×0.62; the dim discount was removed.
// Scope notes: a 15m-only extension (1h not extended) takes no score hit —
// that is a timing concern owned by the entry-timing gates, not the scanner.
// "Extended AND lost the level" (held=false) is a different, worse fact: it
// fails confirmation and takes confirmPenalty ×0.5 at TF level on top.
const extendedPenalty = 0.65

const (
	crossWindowBars = 8  // primary breakout window (recent cross)
	holdWindowBars  = 24 // extended window: breakout-then-hold / retest continuation
	confirmBand     = 0.003
	retestBandATR   = 0.25 // how close a pullback must come to the level to count as a retest
)

// Pattern labels for a scored timeframe.
const (
	PatternBreakout   = "breakout"    // crossed within crossWindowBars, holding
	PatternRetestHold = "retest_hold" // crossed earlier, pulled back to the level and held — continuation entry
	PatternExtended   = "extended"    // crossed earlier, never retested — momentum chase
	PatternApproach   = "approach"    // no cross yet — nearing the level
)

// Level is a candidate key price level.
type Level struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

// DimScores are the six dimension sub-scores (0-100 each).
type DimScores struct {
	Price    float64 `json:"price"`
	Volume   float64 `json:"volume"`
	Flow     float64 `json:"flow"`
	OI       float64 `json:"oi"`
	Funding  float64 `json:"funding"`
	Momentum float64 `json:"momentum"`
}

// TFReport is the per-timeframe breakdown for one direction.
type TFReport struct {
	Timeframe    string    `json:"timeframe"`
	Level        float64   `json:"level"`
	LevelSource  string    `json:"level_source"`
	BreakoutPct  float64   `json:"breakout_pct"` // (price-level)/level × 100, signed toward direction
	ATRStrength  float64   `json:"atr_strength"` // (price-level)/ATR14
	Dims         DimScores `json:"dims"`
	Alpha        float64   `json:"alpha"`
	Beta         float64   `json:"beta"`
	RawScore     float64   `json:"raw_score"`
	Score        float64   `json:"score"` // raw × α × β (× confirm penalty)
	Confirmed    bool      `json:"confirmed"`
	CrossAgeBars int       `json:"cross_age_bars"` // bars since the last close on the far side; -1 = no cross
	Pattern      string    `json:"pattern"`        // breakout / retest_hold / extended / approach
	Confluence   int       `json:"confluence"`     // independent level sources clustered at the trigger level
	RoomATR      float64   `json:"room_atr"`       // distance to the next opposing level (ATR units)
	Notes        []string  `json:"notes,omitempty"`
}

// DirectionSummary aggregates both timeframes for one direction.
type DirectionSummary struct {
	Score15m  float64 `json:"score_15m"`
	Score1h   float64 `json:"score_1h"`
	Score     float64 `json:"score"`
	Resonance bool    `json:"resonance"`
	Grade     string  `json:"grade"`
}

// MarketContext carries the shared context numbers (for display and debugging).
type MarketContext struct {
	ATR15m                float64  `json:"atr_15m"`
	ATR1h                 float64  `json:"atr_1h"`
	VolMultiple15m        float64  `json:"vol_multiple_15m"`
	TakerRatio1h          float64  `json:"taker_ratio_1h"`
	DepthImbalance        *float64 `json:"depth_imbalance"`
	SpreadPct             *float64 `json:"spread_pct"`
	FundingRate           *float64 `json:"funding_rate"`
	FundingPct30d         *float64 `json:"funding_pct_30d"`
	OIChange1hPct         *float64 `json:"oi_change_1h_pct"`
	OIChange4hPct         *float64 `json:"oi_change_4h_pct"`
	SpotFuturesDivergence bool     `json:"spot_futures_divergence"`
	FuturesTakerBull      *float64 `json:"futures_taker_bull"`
	SpotTakerBull         *float64 `json:"spot_taker_bull"`
}

// Report is the full analysis result.
type Report struct {
	Symbol        string                          `json:"symbol"`
	Price         float64                         `json:"price"`
	GeneratedAt   time.Time                       `json:"generated_at"`
	Levels        []Level                         `json:"levels"`
	Context       MarketContext                   `json:"context"`
	Timeframes    map[string]map[string]*TFReport `json:"timeframes"` // timeframe -> direction
	Breakout      DirectionSummary                `json:"breakout"`
	Breakdown     DirectionSummary                `json:"breakdown"`
	Selected      string                          `json:"selected"`
	SelectedScore float64                         `json:"selected_score"`
	Grade         string                          `json:"grade"`
	Crowding      float64                         `json:"crowding,omitempty"`       // 0-100 direction-side crowding (penalty input)
	CrowdLSRatio  *float64                        `json:"crowd_ls_ratio,omitempty"` // global accounts long/short at scan time
}

// shared holds context data shared across timeframes and directions.
type shared struct {
	depth     *DepthSnapshot
	spreadPct *float64
	oi        []OIPoint
	funding   []FundingPoint
	oiChg1h   *float64
	oiChg4h   *float64
	oiValChg  *float64
	fundRate  *float64
	fundPct   *float64
	spotBull  *float64
	futBull1h *float64
	divergent bool
}

// Analyze runs the full breakout/breakdown computation for one symbol.
func Analyze(symbol string, ds DataSource) (*Report, error) {
	d1, err := ds.Klines("1d", 90)
	if err != nil {
		return nil, fmt.Errorf("daily klines: %w", err)
	}
	k15, err := ds.Klines("15m", 865) // 3 days (same-slot volume comparison needs 2 prior days)
	if err != nil {
		return nil, fmt.Errorf("15m klines: %w", err)
	}
	k1h, err := ds.Klines("1h", 169) // 7 days
	if err != nil {
		return nil, fmt.Errorf("1h klines: %w", err)
	}

	sh := &shared{}
	if d, err := ds.Depth1Pct(); err == nil {
		sh.depth = d
		if d.Mid > 0 && d.BestAsk > 0 && d.BestBid > 0 {
			sp := (d.BestAsk - d.BestBid) / d.Mid * 100
			sh.spreadPct = &sp
		}
	}
	if oi, err := ds.OIHistory("5m", 288); err == nil && len(oi) >= 13 {
		sh.oi = oi
		sh.oiChg1h = pctChangePtr(oiValueSeries(oi), 12)
		sh.oiChg4h = pctChangePtr(oiValueSeries(oi), 48)
		sh.oiValChg = sh.oiChg1h
	}
	if f, err := ds.FundingHistory(100); err == nil && len(f) > 0 {
		sh.funding = f
		rates := make([]float64, len(f))
		for i, p := range f {
			rates[i] = p.Rate
		}
		cur := f[len(f)-1].Rate
		sh.fundRate = &cur
		pct := percentile(rates, cur)
		sh.fundPct = &pct
	}
	// Spot vs futures taker divergence (2h window on 15m candles).
	if spot, err := ds.SpotKlines("15m", 8); err == nil && len(spot) >= 4 {
		sb := takerBull(spot)
		fb := takerBull(lastN(k15, 8))
		sh.spotBull = &sb
		sh.futBull1h = &fb
		sh.divergent = signDiff(sb, fb) && math.Abs(sb) > 0.03 && math.Abs(fb) > 0.03
	}

	levels := buildLevels(d1, k1h)

	price := k15[len(k15)-1].Close
	rep := &Report{
		Symbol:      symbol,
		Price:       price,
		GeneratedAt: time.Now().UTC(),
		Levels:      levels,
		Timeframes:  map[string]map[string]*TFReport{},
	}

	rep.Context = MarketContext{
		SpotFuturesDivergence: sh.divergent,
		FuturesTakerBull:      sh.futBull1h,
		SpotTakerBull:         sh.spotBull,
		DepthImbalance:        depthPtr(sh.depth),
		SpreadPct:             sh.spreadPct,
		FundingRate:           sh.fundRate,
		FundingPct30d:         sh.fundPct,
		OIChange1hPct:         sh.oiChg1h,
		OIChange4hPct:         sh.oiChg4h,
	}

	atr15 := atr(highs(k15), lows(k15), closes(k15), 14)
	atr1h := atr(highs(k1h), lows(k1h), closes(k1h), 14)
	rep.Context.ATR15m = lastOr(atr15, 1)
	rep.Context.ATR1h = lastOr(atr1h, 1)
	rep.Context.VolMultiple15m = volSlotMultiple(k15, 288)
	rep.Context.TakerRatio1h = takerRatio(lastN(k15, 4))

	rep.Timeframes["15m"] = map[string]*TFReport{
		DirUp:   computeTF("15m", DirUp, k15, levels, sh),
		DirDown: computeTF("15m", DirDown, k15, levels, sh),
	}
	rep.Timeframes["1h"] = map[string]*TFReport{
		DirUp:   computeTF("1h", DirUp, k1h, levels, sh),
		DirDown: computeTF("1h", DirDown, k1h, levels, sh),
	}

	rep.Breakout = combine(rep.Timeframes, DirUp)
	rep.Breakdown = combine(rep.Timeframes, DirDown)

	if rep.Breakdown.Score > rep.Breakout.Score {
		rep.Selected = DirDown
		rep.SelectedScore = rep.Breakdown.Score
	} else {
		rep.Selected = DirUp
		rep.SelectedScore = rep.Breakout.Score
	}

	// Crowd penalty — direction-aware "is this side already overcrowded".
	// Long side: funding percentile high + accounts skewed long + OI piling.
	// Short side mirrors (funding percentile low, skew short).
	lsPart := 0.0
	if ls, err := ds.LongShortRatio("1h", 3); err == nil && len(ls) > 0 && ls[len(ls)-1].Ratio > 0 {
		v := ls[len(ls)-1].Ratio
		rep.CrowdLSRatio = &v
		if rep.Selected == DirUp {
			lsPart = clamp100(sigmoidScore(v-1, 0.5, 0.35))
		} else {
			lsPart = clamp100(sigmoidScore(1-v, 0.5, 0.35))
		}
	}
	if sh.fundPct != nil || sh.oiChg4h != nil {
		fundPart, oiPart := 0.0, 0.0
		if sh.fundPct != nil {
			fp := *sh.fundPct
			if rep.Selected == DirDown {
				fp = 100 - fp
			}
			fundPart = clamp100(sigmoidScore(fp, 88, 9))
		}
		if sh.oiChg4h != nil {
			oiPart = clamp100(sigmoidScore(*sh.oiChg4h, 8, 6))
		}
		crowding := crowdFundW*fundPart + crowdOIW*oiPart + crowdLSW*lsPart
		rep.Crowding = round2(crowding)
		if crowding > 0 {
			rep.SelectedScore = rep.SelectedScore * (1 - crowdMaxPenalty*crowding/100)
		}
	}

	// Extended-chase penalty: the headline (1h/selected) pattern being
	// "extended" means the cross is old and never retested — pure chasing.
	if tf := rep.Timeframes["1h"][rep.Selected]; tf != nil && tf.Pattern == PatternExtended {
		rep.SelectedScore = rep.SelectedScore * extendedPenalty
	}

	rep.Grade = gradeOf(rep.SelectedScore)
	return rep, nil
}

// ScanResult is the compact per-symbol summary for scan endpoints.
type ScanResult struct {
	Symbol      string  `json:"symbol"`
	Price       float64 `json:"price"`
	Direction   string  `json:"direction"`
	Score       float64 `json:"score"`
	Grade       string  `json:"grade"`
	Resonance   bool    `json:"resonance"`
	Level       float64 `json:"level"`
	LevelSource string  `json:"level_source"`
	Pattern     string  `json:"pattern,omitempty"`
	Confluence  int     `json:"confluence,omitempty"`
	RoomATR     float64 `json:"room_atr,omitempty"`
	Percentile  float64 `json:"percentile"`           // cross-sectional rank across the scanned universe (0-100)
	Crowding    float64 `json:"crowding,omitempty"`   // direction-side crowding 0-100 (drives the score penalty)
	Regime      string  `json:"regime,omitempty"`     // btc_bull / btc_bear / chop (context for alts)
	SpreadPct   float64 `json:"spread_pct,omitempty"` // order-book spread (tradability cost)
}

// AnalyzeMany analyzes symbols concurrently and returns compact summaries.
func AnalyzeMany(symbols []string, concurrency int) []ScanResult {
	if concurrency <= 0 {
		concurrency = 4
	}
	results := make([]ScanResult, len(symbols))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, sym := range symbols {
		wg.Add(1)
		go func(i int, sym string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rep, err := Analyze(sym, NewBinanceDS(sym))
			if err != nil {
				return
			}
			tf := rep.Timeframes["1h"][rep.Selected]
			if tf == nil {
				tf = &TFReport{}
			}
			r := ScanResult{
				Symbol:      sym,
				Price:       rep.Price,
				Direction:   rep.Selected,
				Score:       round2(rep.SelectedScore),
				Grade:       rep.Grade,
				Level:       tf.Level,
				LevelSource: tf.LevelSource,
				Pattern:     tf.Pattern,
				Confluence:  tf.Confluence,
				RoomATR:     tf.RoomATR,
			}
			if rep.Context.SpreadPct != nil {
				r.SpreadPct = round2(*rep.Context.SpreadPct)
			}
			if rep.Selected == DirUp {
				r.Resonance = rep.Breakout.Resonance
			} else {
				r.Resonance = rep.Breakdown.Resonance
			}
			r.Crowding = rep.Crowding
			results[i] = r
		}(i, sym)
	}
	wg.Wait()
	// Sort by score desc, drop failed lookups.
	out := make([]ScanResult, 0, len(results))
	for _, r := range results {
		if r.Symbol != "" {
			out = append(out, r)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Score > out[i].Score {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// computeTF scores one timeframe for one direction.
func computeTF(tf string, dir string, k []Kline, levels []Level, sh *shared) *TFReport {
	n := len(k)
	c := closes(k)
	h := highs(k)
	l := lows(k)
	atrNow := lastOr(atr(h, l, c, 14), 1)
	price := c[n-1]

	rep := &TFReport{Timeframe: tf, CrossAgeBars: -1, Pattern: PatternApproach}
	params := GetParams()

	// ── Level selection: confluence-aware, cross within the hold window ──
	// confluence = independent level sources clustered at the same price
	// (structure extremes weighted highest), plus a per-source priority bonus.
	levelPriority := func(name string) float64 {
		switch {
		case strings.HasPrefix(name, "20d_") || strings.HasPrefix(name, "60d_"):
			return 2.5
		case name == "vpvr_poc":
			return 1.0
		case strings.HasPrefix(name, "vpvr_"):
			return 0.75
		case strings.HasPrefix(name, "bb_"):
			return 0.5
		}
		return 0 // fib and others
	}
	// Confluence counts INDEPENDENT source families clustered at the price —
	// vpvr_poc/vah/val are one volume-profile source, not three confirmations.
	// Only the VPVR triplet (poc/vah/val — one computation, three outputs)
	// collapses into a single source. 20d_high + 60d_high at the same price is
	// a genuine double-top confirmation and counts twice.
	levelFamily := func(name string) string {
		if strings.HasPrefix(name, "vpvr_") {
			return "vpvr"
		}
		return name
	}
	confluenceOf := func(lv Level) int {
		families := map[string]bool{}
		for _, other := range levels {
			if other.Price <= 0 || other.Name == lv.Name {
				continue
			}
			if math.Abs(other.Price-lv.Price) <= 0.5*atrNow {
				families[levelFamily(other.Name)] = true
			}
		}
		delete(families, levelFamily(lv.Name))
		return len(families)
	}

	type candidate struct {
		level    Level
		age      int
		rawCount int     // sources clustered at this price
		score    float64 // rawCount + priority bonus
	}
	var candidates []candidate
	for _, lv := range levels {
		if lv.Price <= 0 {
			continue
		}
		var age int
		var crossed bool
		if dir == DirUp {
			if price <= lv.Price {
				continue
			}
			for i := n - 1; i >= n-holdWindowBars && i >= 0; i-- {
				if c[i] < lv.Price {
					age = n - 1 - i
					crossed = true
					break
				}
			}
		} else {
			if price >= lv.Price {
				continue
			}
			for i := n - 1; i >= n-holdWindowBars && i >= 0; i-- {
				if c[i] > lv.Price {
					age = n - 1 - i
					crossed = true
					break
				}
			}
		}
		if crossed {
			candidates = append(candidates, candidate{
				level: lv, age: age,
				rawCount: confluenceOf(lv),
				score:    float64(confluenceOf(lv)) + levelPriority(lv.Name),
			})
		}
	}

	// Pick the candidate: highest weighted confluence; tie → most recent cross.
	if len(candidates) > 0 {
		best := candidates[0]
		for _, cand := range candidates[1:] {
			if cand.score > best.score+1e-9 ||
				(math.Abs(cand.score-best.score) <= 1e-9 && cand.age < best.age) {
				best = cand
			}
		}
		rep.Level = best.level.Price
		rep.LevelSource = best.level.Name
		rep.CrossAgeBars = best.age
		rep.Confluence = best.rawCount
	}

	// ── Dimension 1: price structure (25%) with pattern awareness ──
	if rep.CrossAgeBars >= 0 {
		var beyond float64
		if dir == DirUp {
			beyond = price - rep.Level
		} else {
			beyond = rep.Level - price
		}
		rep.BreakoutPct = beyond / rep.Level * 100
		rep.ATRStrength = beyond / atrNow
		rep.Dims.Price = sigmoidScore(rep.ATRStrength, params.PriceATRCenter, params.PriceATRWidth)

		held := true
		crossBar := n - 1 - rep.CrossAgeBars
		retested := false
		for j := crossBar + 1; j < n; j++ {
			if dir == DirUp {
				if c[j] < rep.Level {
					held = false
					break
				}
				if l[j] <= rep.Level+retestBandATR*atrNow {
					retested = true
				}
			} else {
				if c[j] > rep.Level {
					held = false
					break
				}
				if h[j] >= rep.Level-retestBandATR*atrNow {
					retested = true
				}
			}
		}

		switch {
		case rep.CrossAgeBars < crossWindowBars:
			rep.Pattern = PatternBreakout
			// Pullback confirmation: price must still hold the far side (±0.3% band).
			if dir == DirUp {
				rep.Confirmed = price >= rep.Level*(1-confirmBand)
			} else {
				rep.Confirmed = price <= rep.Level*(1+confirmBand)
			}
			if !rep.Confirmed {
				rep.Notes = append(rep.Notes, "回踩确认失败：价格跌回关键位下方（插针/假突破）")
			}
		case retested && held:
			rep.Pattern = PatternRetestHold
			rep.Confirmed = true
			rep.Dims.Price *= 0.95
			rep.Notes = append(rep.Notes, "突破后回踩企稳（未破位），延续形态入场")
		case held:
			rep.Pattern = PatternExtended
			rep.Confirmed = true
			// No dim-level discount here: the chase charge lives ONCE at the
			// cross-section (extendedPenalty on the 1h headline in Analyze).
			// Charging both — this ×0.8 feeding the 1h score, then ×0.65 on
			// the combined result — stacked to ≈×0.62 on one binary fact
			// (user audit 2026-09-17). The label still lands in Notes and the
			// report so the entry-quality caveat stays visible downstream.
			rep.Notes = append(rep.Notes, "突破后持续延伸且未回踩(追高形态,综合分在截面统一折价 ×0.65)")
		default:
			rep.Pattern = PatternExtended
			rep.Confirmed = false
			rep.Notes = append(rep.Notes, "突破后曾失守关键位，形态质量低")
		}

		// Confluence bonus: more agreeing sources → stronger the level.
		if rep.Confluence > 1 {
			rep.Dims.Price *= math.Min(1+0.08*float64(rep.Confluence-1), 1.16)
		}

		// Room-to-run: distance from the current price to the nearest opposing
		// level ahead. A breakout right under a ceiling is worth much less than
		// one with open space.
		room := 3.0
		nextLv := math.Inf(1)
		for _, lv := range levels {
			if lv.Price <= 0 {
				continue
			}
			if dir == DirUp && lv.Price > price && lv.Price < nextLv {
				nextLv = lv.Price
			}
			if dir == DirDown && lv.Price < price && lv.Price > nextLv*-1 && (math.IsInf(nextLv, -1) || lv.Price > nextLv) {
				nextLv = lv.Price
			}
		}
		if !math.IsInf(nextLv, 0) {
			var dist float64
			if dir == DirUp {
				dist = (nextLv - price) / atrNow
			} else {
				dist = (price - nextLv) / atrNow
			}
			room = math.Max(0, math.Min(3, dist))
		}
		rep.RoomATR = round2(room)
		if room < 1 {
			rep.Dims.Price *= 0.75
			rep.Notes = append(rep.Notes, fmt.Sprintf("上行空间不足（下一关键位仅 %.1f ATR 之外），价格维度 ×0.75", room))
		} else if room < 2 {
			rep.Dims.Price *= 0.9
		}
	} else {
		// No recent cross — measure approach distance to the nearest level ahead.
		dist := math.Inf(1)
		nearest := Level{}
		for _, lv := range levels {
			if lv.Price <= 0 {
				continue
			}
			var d float64
			if dir == DirUp {
				if lv.Price <= price {
					continue
				}
				d = (lv.Price - price) / atrNow
			} else {
				if lv.Price >= price {
					continue
				}
				d = (price - lv.Price) / atrNow
			}
			if d < dist {
				dist = d
				nearest = lv
			}
		}
		rep.Level = nearest.Price
		rep.LevelSource = nearest.Name
		rep.ATRStrength = -dist
		if !math.IsInf(dist, 1) {
			rep.BreakoutPct = -dist * atrNow / math.Max(price, 1e-9) * 100
			rep.Dims.Price = math.Max(0, 25-12*dist)
		}
		rep.Notes = append(rep.Notes, "近期未突破关键位（接近度评分）")
	}

	// ── Dimension 2: volume & orderbook (20%) ──
	slot := 288
	if tf == "1h" {
		slot = 24
	}
	volMult := volSlotMultiple(k, slot)
	ratio := takerRatio(lastN(k, 4))
	if dir == DirDown {
		ratio = 1 / math.Max(ratio, 1e-9)
	}
	sVol := 0.5*sigmoidScore(volMult, params.VolCenter, 0.8) + 0.5*sigmoidScore(ratio, 1.15, 0.15)
	if sh.depth != nil {
		imb := sh.depth.Imbalance
		if dir == DirDown {
			imb = -imb
		}
		sVol = 0.4*sigmoidScore(volMult, params.VolCenter, 0.8) + 0.4*sigmoidScore(ratio, 1.15, 0.15) + 0.2*sigmoidScore(imb, 0.05, 0.08)
	}
	// Tradability cost: a wide spread eats the edge of any trade taken here.
	if sh.spreadPct != nil {
		sp := *sh.spreadPct
		if sp >= 0.30 {
			sVol *= 0.6
			rep.Notes = append(rep.Notes, fmt.Sprintf("点差 %.2f%% 过宽，量能维度 ×0.6", sp))
		} else if sp >= 0.15 {
			sVol *= 0.85
			rep.Notes = append(rep.Notes, fmt.Sprintf("点差 %.2f%% 偏宽，量能维度 ×0.85", sp))
		}
	}
	rep.Dims.Volume = sVol

	// ── Dimension 3: fund flow proxies (20%) ──
	bull := takerBull(lastN(k, 4))
	if dir == DirDown {
		bull = -bull
	}
	sFlow := sigmoidScore(bull, 0.05, 0.04)
	if sh.oiValChg != nil {
		sFlow = 0.6*sFlow + 0.4*sigmoidScore(*sh.oiValChg, 0.4, 0.4)
	}
	if sh.divergent {
		sFlow *= 0.7
		rep.Notes = append(rep.Notes, "现货与合约资金方向背离，资金流维度降权 0.7")
	}
	rep.Dims.Flow = sFlow

	// ── Dimension 4: open interest (15%) + quality factor α ──
	rep.Alpha = alphaNeutral
	if sh.oiChg1h != nil {
		rep.Dims.OI = sigmoidScore(*sh.oiChg1h, 0.3, 0.3)
		// Price-OI matrix: both "OI increasing" rows are healthy.
		priceBars := 4
		if tf == "1h" {
			priceBars = 1
		}
		priceChg := 0.0
		if n > priceBars+1 && c[n-1-priceBars] > 0 {
			priceChg = c[n-1]/c[n-1-priceBars] - 1
		}
		moveInDir := (dir == DirUp && priceChg > 0.0005) || (dir == DirDown && priceChg < -0.0005)
		oiUp := *sh.oiChg1h > 0.001
		oiDown := *sh.oiChg1h < -0.001
		switch {
		case moveInDir && oiUp:
			rep.Alpha = alphaHealthyOI
			rep.Notes = append(rep.Notes, "量价OI矩阵：新仓进场推动，趋势健康 α=1.0")
		case moveInDir && oiDown:
			rep.Alpha = params.AlphaWeak
			rep.Notes = append(rep.Notes, fmt.Sprintf("量价OI矩阵：平仓/逼空推动（OI减少），动能存疑 α=%.2f", params.AlphaWeak))
		}
	} else {
		rep.Dims.OI = 50
		rep.Notes = append(rep.Notes, "OI数据不可用，按中性 50 分计")
	}

	// ── Dimension 5: funding rate (10%) + crowding factor β ──
	rep.Beta = 1.0
	if sh.fundPct != nil && sh.fundRate != nil {
		pct := *sh.fundPct
		if dir == DirUp {
			rep.Dims.Funding = 100 - pct // low percentile = shorts paying = fuel for upside
			if pct >= 90 {
				rep.Beta = params.BetaCrowded
				rep.Notes = append(rep.Notes, fmt.Sprintf("资金费率处于30日90分位以上（多头拥挤），β=%.2f", params.BetaCrowded))
			}
		} else {
			rep.Dims.Funding = pct
			if pct <= 10 {
				rep.Beta = params.BetaCrowded
				rep.Notes = append(rep.Notes, fmt.Sprintf("资金费率处于30日10分位以下（空头拥挤），β=%.2f", params.BetaCrowded))
			}
		}
	} else {
		rep.Dims.Funding = 50
		rep.Notes = append(rep.Notes, "资金费率数据不可用，按中性 50 分计")
	}

	// ── Dimension 6: momentum / volatility (10%) ──
	r := rsiLast(c, 14)
	histN := macdHistLast(c) / atrNow
	mult := atrNow / math.Max(meanTR(h, l, c, 200), 1e-9)
	sMom := 0.4*sigmoidScore(r, 62, 10) + 0.4*sigmoidScore(histN, 0.05, 0.1) + 0.2*sigmoidScore(mult, 1.3, 0.4)
	if dir == DirDown {
		sMom = 0.4*sigmoidScore(100-r, 62, 10) + 0.4*sigmoidScore(-histN, 0.05, 0.1) + 0.2*sigmoidScore(mult, 1.3, 0.4)
	}
	if dir == DirUp && r > 88 {
		sMom *= 0.8
		rep.Notes = append(rep.Notes, fmt.Sprintf("RSI=%.0f 超买，动量维度衰减 0.8", r))
	}
	if dir == DirDown && r < 12 {
		sMom *= 0.8
		rep.Notes = append(rep.Notes, fmt.Sprintf("RSI=%.0f 超卖，动量维度衰减 0.8", r))
	}
	rep.Dims.Momentum = sMom

	// ── Composite ──
	rep.RawScore = wPrice*rep.Dims.Price + wVolume*rep.Dims.Volume + wFlow*rep.Dims.Flow +
		wOI*rep.Dims.OI + wFunding*rep.Dims.Funding + wMomentum*rep.Dims.Momentum
	score := rep.RawScore * rep.Alpha * rep.Beta
	if rep.CrossAgeBars >= 0 && !rep.Confirmed {
		score *= confirmPenalty
	}
	rep.Score = clamp(score, 0, 100)
	return rep
}

// combine aggregates both timeframes for one direction with resonance bonus.
func combine(tfs map[string]map[string]*TFReport, dir string) DirectionSummary {
	p := GetParams()
	s := DirectionSummary{}
	if tf15 := tfs["15m"][dir]; tf15 != nil {
		s.Score15m = round2(tf15.Score)
	}
	if tf1h := tfs["1h"][dir]; tf1h != nil {
		s.Score1h = round2(tf1h.Score)
	}
	// Layered weights (audit 2026-09-12 #8 — rationale stated precisely):
	// the 1h weight FILTERS DIRECTION NOISE (long-window consensus decides
	// WHICH side to trade), the 15m weight TIMES the entry. This is a
	// direction-quality-vs-timing division of labor, NOT a "lag avoidance"
	// tradeoff — raising the 15m weight buys speed at the cost of direction
	// quality; do not tune it against a "reduce lag" goal.
	s.Score = 0.65*s.Score1h + 0.35*s.Score15m
	if s.Score15m >= p.ResonanceGate && s.Score1h >= p.ResonanceGate {
		s.Resonance = true
		s.Score = math.Min(s.Score+p.ResonanceBonus, 100)
	}
	s.Score = round2(s.Score)
	s.Grade = gradeOf(s.Score)
	return s
}

// buildLevels derives candidate key levels: N-day extremes, Bollinger bands,
// VPVR POC/VAH/VAL and Fibonacci retracements.
func buildLevels(d1, k1h []Kline) []Level {
	var levels []Level
	dh := highs(d1)
	dl := lows(d1)
	dc := closes(d1)

	h20 := maxOf(lastNf(dh, 20))
	l20 := minOf(lastNf(dl, 20))
	h60 := maxOf(lastNf(dh, 60))
	l60 := minOf(lastNf(dl, 60))

	levels = append(levels,
		Level{"20d_high", h20},
		Level{"20d_low", l20},
		Level{"60d_high", h60},
		Level{"60d_low", l60},
	)

	if len(dc) >= 20 {
		_, up, low := boll(dc, 20, 2)
		levels = append(levels, Level{"bb_upper_1d", up}, Level{"bb_lower_1d", low})
	}

	// Simplified VPVR over the last 7 days of 1h candles.
	if len(k1h) >= 24 {
		w := lastN(k1h, 168)
		tp := make([]float64, len(w))
		qv := make([]float64, len(w))
		for i, kk := range w {
			tp[i] = (kk.High + kk.Low + kk.Close) / 3
			qv[i] = kk.QuoteVolume
		}
		poc, vah, val := volumeProfile(tp, qv, 50)
		levels = append(levels, Level{"vpvr_poc", poc}, Level{"vpvr_vah", vah}, Level{"vpvr_val", val})
	}

	// Fibonacci retracements of the 60d range.
	if h60 > l60 {
		for _, f := range []float64{0.236, 0.382, 0.5, 0.618, 0.786} {
			levels = append(levels, Level{fmt.Sprintf("fib_%.3f", f), l60 + f*(h60-l60)})
		}
	}

	// Drop degenerate levels.
	out := levels[:0]
	for _, lv := range levels {
		if lv.Price > 0 {
			out = append(out, lv)
		}
	}
	return out
}

// ── small helpers ──

func closes(k []Kline) []float64 {
	out := make([]float64, len(k))
	for i, v := range k {
		out[i] = v.Close
	}
	return out
}
func highs(k []Kline) []float64 {
	out := make([]float64, len(k))
	for i, v := range k {
		out[i] = v.High
	}
	return out
}
func lows(k []Kline) []float64 {
	out := make([]float64, len(k))
	for i, v := range k {
		out[i] = v.Low
	}
	return out
}

func lastN(k []Kline, n int) []Kline {
	if len(k) <= n {
		return k
	}
	return k[len(k)-n:]
}

func lastNf(xs []float64, n int) []float64 {
	if len(xs) <= n {
		return xs
	}
	return xs[len(xs)-n:]
}

func lastOr(xs []float64, def float64) float64 {
	if len(xs) == 0 {
		return def
	}
	return xs[len(xs)-1]
}

func maxOf(xs []float64) float64 {
	m := math.Inf(-1)
	for _, v := range xs {
		if v > m {
			m = v
		}
	}
	return m
}

func minOf(xs []float64) float64 {
	m := math.Inf(1)
	for _, v := range xs {
		if v < m {
			m = v
		}
	}
	return m
}

// volSlotMultiple compares the last closed candle's volume with the average of
// the same time-of-day candles on the previous days (slot step in bars).
func volSlotMultiple(k []Kline, step int) float64 {
	n := len(k)
	if n < step+2 {
		return 1
	}
	v := k[n-2].Volume
	var slots []float64
	for d := 1; d <= 3; d++ {
		idx := n - 2 - d*step
		if idx >= 0 {
			slots = append(slots, k[idx].Volume)
		}
	}
	avg := mean(slots)
	if avg <= 0 {
		return 1
	}
	return v / avg
}

// takerRatio returns buy/sell taker volume over the candles.
func takerRatio(k []Kline) float64 {
	var buy, sell float64
	for _, c := range k {
		buy += c.TakerBuyBase
		sell += c.Volume - c.TakerBuyBase
	}
	if sell <= 0 {
		if buy > 0 {
			return 10
		}
		return 1
	}
	return buy / sell
}

// takerBull returns a -1..+1 imbalance of taker buy vs sell quote flow.
func takerBull(k []Kline) float64 {
	var buy, total float64
	for _, c := range k {
		buy += c.TakerBuyQty
		total += c.QuoteVolume
	}
	if total <= 0 {
		return 0
	}
	return (buy - (total - buy)) / total
}

func signDiff(a, b float64) bool {
	return (a > 0 && b < 0) || (a < 0 && b > 0)
}

func oiValueSeries(oi []OIPoint) []float64 {
	out := make([]float64, len(oi))
	for i, p := range oi {
		out[i] = p.Value
	}
	return out
}

// pctChangePtr returns (last/last-back - 1)*100, or nil when out of range.
func pctChangePtr(series []float64, back int) *float64 {
	n := len(series)
	if n <= back || series[n-1-back] == 0 {
		return nil
	}
	v := (series[n-1]/series[n-1-back] - 1) * 100
	return &v
}

func depthPtr(d *DepthSnapshot) *float64 {
	if d == nil {
		return nil
	}
	v := d.Imbalance
	return &v
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
