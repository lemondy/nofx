package breakout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"

	"nofx/logger"
)

// 2026-10-09 trend-continuation short source (shadow, user option A).
const (
	trendShortPruneAfter  = 60 * 24 * time.Hour
	trendShortMaxAttempts = 8
	trendShortLabelBudget = 400 // Per slow tick, bounded catch-up after restarts.
)

var trendShadowMu sync.Mutex
var trendShadowRunMu sync.Mutex
var trendShadowPath = "data/trendshort_shadow.jsonl"

// SetTrendShortShadowPath changes only the research journal location.
func SetTrendShortShadowPath(path string) {
	trendShadowMu.Lock()
	defer trendShadowMu.Unlock()
	trendShadowPath = path
}

type TrendShortLabel struct {
	Done          bool    `json:"done"`
	Unpriceable   bool    `json:"unpriceable,omitempty"`
	NetPct        float64 `json:"net_pct"`
	FundingPct    float64 `json:"funding_pct"`
	CostPct       float64 `json:"cost_pct"`
	LabelAt       int64   `json:"label_at,omitempty"`
	Attempts      int     `json:"attempts,omitempty"`
	NextRetryAt   int64   `json:"next_retry_at,omitempty"`
	MissingReason string  `json:"missing_reason,omitempty"`
	Path          string  `json:"path,omitempty"`
	R             float64 `json:"r,omitempty"`     // Gross exit R: +2 TP, -1 stop, close R on timeout.
	MFER          float64 `json:"mfe_r,omitempty"` // Full 24h excursions, including after a barrier hit.
	MAER          float64 `json:"mae_r,omitempty"`
}

type TrendShortLabels struct {
	H4  TrendShortLabel `json:"h4"`
	H24 TrendShortLabel `json:"h24"`
}

type TrendShortShadowRow struct {
	Kind string `json:"kind"` // pick or control
	TS   int64  `json:"ts"`   // UTC observation time (minute resolution); entry is the last closed 1h close.
	Rank int    `json:"rank,omitempty"`
	TrendShortPick
	Labels        TrendShortLabels            `json:"labels"`
	Prices        map[string]float64          `json:"prices,omitempty"`
	ControlLabels map[string]TrendShortLabels `json:"control_labels,omitempty"`
	Universe      int                         `json:"universe,omitempty"`
	Failures      int                         `json:"scan_failures,omitempty"`
}

func readTrendShadow() ([]TrendShortShadowRow, error) {
	b, err := os.ReadFile(trendShadowPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []TrendShortShadowRow
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r TrendShortShadowRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("trend shadow journal: %w", err)
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}
func writeTrendShadow(rows []TrendShortShadowRow) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return atomicWriteJSON(trendShadowPath, b.Bytes())
}

// ReadTrendShortShadow is read-only; the API never triggers network or labelling.
func ReadTrendShortShadow() ([]TrendShortShadowRow, error) {
	trendShadowMu.Lock()
	defer trendShadowMu.Unlock()
	return readTrendShadow()
}

func trendShadowSampled(rows []TrendShortShadowRow, ts int64) bool {
	for _, r := range rows {
		if r.TS/time.Hour.Milliseconds() == ts/time.Hour.Milliseconds() {
			return true
		}
	}
	return false
}
func appendTrendShadow(scan trendShortScan, ts int64) error {
	trendShadowMu.Lock()
	defer trendShadowMu.Unlock()
	rows, err := readTrendShadow()
	if err != nil {
		return err
	}
	if trendShadowSampled(rows, ts) {
		return nil
	}
	for i, p := range scan.picks {
		rows = append(rows, TrendShortShadowRow{Kind: "pick", TS: ts, Rank: i + 1, TrendShortPick: p})
	}
	rows = append(rows, TrendShortShadowRow{Kind: "control", TS: ts, Prices: scan.prices, ControlLabels: map[string]TrendShortLabels{}, Universe: scan.universe, Failures: scan.failures})
	// Atomic batch append: picks and the empty-cohort dedupe marker survive together.
	return writeTrendShadow(rows)
}

// RunTrendShortShadow runs only on the scheduler's slow goroutine.
func RunTrendShortShadow(now time.Time) {
	if !trendShadowRunMu.TryLock() {
		return
	}
	defer trendShadowRunMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("trend short shadow panic (recovered): %v", r)
		}
	}()
	anchor := now.UTC().Truncate(time.Minute)
	rows, err := ReadTrendShortShadow()
	if err != nil {
		logger.Errorf("trend short shadow read: %v", err)
		return
	}
	if !trendShadowSampled(rows, anchor.UnixMilli()) {
		scan, err := scanTrendShorts(anchor)
		if err != nil {
			logger.Warnf("trend short shadow scan: %v", err)
		} else if err = appendTrendShadow(scan, anchor.UnixMilli()); err != nil {
			logger.Errorf("trend short shadow append: %v", err)
		}
	}
	if err := labelTrendShadow(now); err != nil {
		logger.Errorf("trend short shadow labels: %v", err)
	}
}

type trendLabelKey struct {
	ts     int64
	symbol string
	hours  int
}
type trendLabelJob struct {
	key         trendLabelKey
	price, stop float64
	previous    TrendShortLabel
}

func trendHorizon(l *TrendShortLabels, h int) *TrendShortLabel {
	if h == 4 {
		return &l.H4
	}
	return &l.H24
}

// Three phases: read due work under lock, HTTP without lock, fresh-read merge.
func labelTrendShadow(now time.Time) error {
	rows, err := ReadTrendShortShadow()
	if err != nil {
		return err
	}
	jobsByKey := map[trendLabelKey]trendLabelJob{}
	add := func(ts int64, sym string, price, stop float64, labels TrendShortLabels) {
		for _, h := range []int{4, 24} {
			l := *trendHorizon(&labels, h)
			if l.Done || l.NextRetryAt > now.UnixMilli() || now.UnixMilli() < ts+int64(h)*time.Hour.Milliseconds() {
				continue
			}
			key := trendLabelKey{ts, sym, h}
			_, ok := jobsByKey[key]
			if !ok || stop > 0 {
				jobsByKey[key] = trendLabelJob{key, price, stop, l}
			}
		}
	}
	for _, r := range rows {
		if r.TS < now.Add(-trendShortPruneAfter).UnixMilli() {
			continue
		}
		if r.Kind == "pick" {
			add(r.TS, r.Symbol, r.Price, r.StopPrice, r.Labels)
		}
		if r.Kind == "control" {
			for sym, p := range r.Prices {
				add(r.TS, sym, p, 0, r.ControlLabels[sym])
			}
		}
	}
	var jobs []trendLabelJob
	for _, j := range jobsByKey {
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool {
		a, b := jobs[i].key, jobs[j].key
		if a.ts != b.ts {
			return a.ts < b.ts
		}
		if a.symbol != b.symbol {
			return a.symbol < b.symbol
		}
		return a.hours < b.hours
	})
	if len(jobs) > trendShortLabelBudget {
		jobs = jobs[:trendShortLabelBudget]
	}
	results := map[trendLabelKey]TrendShortLabel{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	queue := make(chan trendLabelJob)
	for i := 0; i < trendShortWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				l := j.previous
				price, at, funding, e := historicalShortHorizon(shortSample{TS: j.key.ts, Symbol: j.key.symbol, Price: j.price}, time.Duration(j.key.hours)*time.Hour)
				if e == nil && (j.price <= 0 || !trendFinite(j.price)) {
					e = fmt.Errorf("invalid entry")
				}
				if e == nil {
					l.NetPct = (j.price-price)/j.price*100 + funding - btCostRoundTrip
					l.FundingPct = funding
					l.CostPct = btCostRoundTrip
					l.LabelAt = at
					if j.key.hours == 24 && j.stop > 0 {
						l.Path, l.R, l.MFER, l.MAER, e = historicalTrendPath(j.key.ts, j.key.symbol, j.price, j.stop)
					}
				}
				if e != nil {
					l.Attempts++
					l.MissingReason = e.Error()
					l.NextRetryAt = now.Add(time.Hour * time.Duration(1<<min(l.Attempts, 4))).UnixMilli()
					if l.Attempts >= trendShortMaxAttempts {
						l.Done = true
						l.Unpriceable = true
					}
				} else {
					l.Done = true
					l.Unpriceable = false
					l.NextRetryAt = 0
					l.MissingReason = ""
				}
				mu.Lock()
				results[j.key] = l
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		queue <- j
	}
	close(queue)
	wg.Wait()
	trendShadowMu.Lock()
	defer trendShadowMu.Unlock()
	rows, err = readTrendShadow()
	if err != nil {
		return err
	}
	var kept []TrendShortShadowRow
	for _, r := range rows {
		if r.TS < now.Add(-trendShortPruneAfter).UnixMilli() {
			continue
		}
		for _, h := range []int{4, 24} {
			if r.Kind == "pick" {
				if l, ok := results[trendLabelKey{r.TS, r.Symbol, h}]; ok {
					*trendHorizon(&r.Labels, h) = l
				}
			}
			if r.Kind == "control" {
				for sym := range r.Prices {
					if l, ok := results[trendLabelKey{r.TS, sym, h}]; ok {
						if r.ControlLabels == nil {
							r.ControlLabels = map[string]TrendShortLabels{}
						}
						ls := r.ControlLabels[sym]
						*trendHorizon(&ls, h) = l
						r.ControlLabels[sym] = ls
					}
				}
			}
		}
		kept = append(kept, r)
	}
	if len(results) == 0 && len(kept) == len(rows) {
		return nil
	}
	return writeTrendShadow(kept)
}

func historicalTrendPath(ts int64, symbol string, entry, stop float64) (string, float64, float64, float64, error) {
	end := ts + 24*time.Hour.Milliseconds()
	if ts%time.Minute.Milliseconds() != 0 {
		return "", 0, 0, 0, fmt.Errorf("path timestamp must align to a minute")
	}
	quarter := (15 * time.Minute).Milliseconds()
	first := ((ts + quarter - 1) / quarter) * quarter
	last := (end / quarter) * quarter
	var bars []Kline
	// 15m body plus 1m boundary fragments: no pre-observation highs/lows.
	for _, segment := range []struct {
		start, end int64
		interval   string
		step       int64
	}{
		{ts, first, "1m", time.Minute.Milliseconds()},
		{first, last, "15m", quarter},
		{last, end, "1m", time.Minute.Milliseconds()},
	} {
		if segment.start == segment.end {
			continue
		}
		u := fmt.Sprintf("%s/fapi/v1/klines?symbol=%s&interval=%s&startTime=%d&endTime=%d&limit=100", fapiBase(), url.QueryEscape(symbol), segment.interval, segment.start, segment.end-1)
		var raw [][]interface{}
		if err := fetchJSON(u, &raw); err != nil {
			return "", 0, 0, 0, err
		}
		if int64(len(raw)) != (segment.end-segment.start)/segment.step {
			return "", 0, 0, 0, fmt.Errorf("incomplete 24h path")
		}
		for i, r := range raw {
			open := segment.start + int64(i)*segment.step
			if len(r) < 7 || int64(toF(r[0])) != open || int64(toF(r[6])) != open+segment.step-1 {
				return "", 0, 0, 0, fmt.Errorf("misaligned path")
			}
			b := Kline{High: toF(r[2]), Low: toF(r[3]), Close: toF(r[4])}
			if !trendFinite(b.High) || !trendFinite(b.Low) || !trendFinite(b.Close) || b.Low <= 0 || b.Close < b.Low || b.Close > b.High {
				return "", 0, 0, 0, fmt.Errorf("invalid path price")
			}
			bars = append(bars, b)
		}
	}
	return trendPath(bars, entry, stop)
}

func trendPath(bars []Kline, entry, stop float64) (outcome string, r, mfe, mae float64, err error) {
	risk := stop - entry
	if risk <= 0 || !trendFinite(risk) || len(bars) == 0 {
		return "", 0, 0, 0, fmt.Errorf("invalid path risk/bars")
	}
	outcome = "timeout"
	r = (entry - bars[len(bars)-1].Close) / risk
	hit := false
	for _, b := range bars {
		mfe = max(mfe, (entry-b.Low)/risk)
		mae = max(mae, (b.High-entry)/risk)
		if hit {
			continue
		}
		if b.High >= stop {
			outcome = "sl_first"
			r = -1
			hit = true
		} else if b.Low <= entry-2*risk {
			outcome = "tp_first"
			r = 2
			hit = true
		}
	}
	return
}
