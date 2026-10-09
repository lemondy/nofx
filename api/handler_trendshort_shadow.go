package api

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"time"

	"nofx/market/breakout"
	"nofx/store"

	"github.com/gin-gonic/gin"
)

// 2026-10-09 trend-continuation short source (shadow, user option A).
type trendReturnStats struct {
	N       int      `json:"n"`
	Mean    *float64 `json:"mean_net_pct"`
	Median  *float64 `json:"median_net_pct"`
	WinRate *float64 `json:"win_rate"`
}
type trendPathStats struct {
	N       int      `json:"n"`
	TPFirst int      `json:"tp_first"`
	SLFirst int      `json:"sl_first"`
	Timeout int      `json:"timeout"`
	MeanR   *float64 `json:"mean_r"`
}
type trendEdgeStats struct {
	N         int      `json:"n"` // One paired mean difference per clock hour.
	Mean      *float64 `json:"mean_pct"`
	T         *float64 `json:"t"`
	TInfinite bool     `json:"t_infinite,omitempty"`
}
type trendCohortStats struct {
	H4   trendReturnStats `json:"h4"`
	H24  trendReturnStats `json:"h24"`
	Path *trendPathStats  `json:"path,omitempty"`
}
type trendShadowStats struct {
	ShadowOnly      bool                      `json:"shadow_only"`
	Counts          map[string]int            `json:"counts"`
	Picks           trendCohortStats          `json:"picks"`
	Controls        trendCohortStats          `json:"controls"` // Equal-weight universe mean, one value per complete hour.
	Edge            map[string]trendEdgeStats `json:"picks_minus_control"`
	PerSymbol       map[string]int            `json:"per_symbol"`
	OverlapLivePool *float64                  `json:"overlap_live_pool"`
	OverlapN        int                       `json:"overlap_n"`
	OverlapMatched  int                       `json:"overlap_matched"`
}

func trendStats(xs []float64) trendReturnStats {
	s := trendReturnStats{N: len(xs)}
	if len(xs) == 0 {
		return s
	}
	ys := append([]float64(nil), xs...)
	sort.Float64s(ys)
	sum, wins := 0.0, 0.0
	for _, x := range ys {
		sum += x
		if x > 0 {
			wins++
		}
	}
	mean, median, win := sum/float64(len(ys)), ys[len(ys)/2], wins/float64(len(ys))
	if len(ys)%2 == 0 {
		median = (ys[len(ys)/2-1] + median) / 2
	}
	s.Mean, s.Median, s.WinRate = &mean, &median, &win
	return s
}
func trendEdge(xs []float64) trendEdgeStats {
	s := trendEdgeStats{N: len(xs), Mean: trendStats(xs).Mean}
	if len(xs) < 2 {
		return s
	}
	ss := 0.0
	for _, x := range xs {
		ss += (x - *s.Mean) * (x - *s.Mean)
	}
	if ss == 0 {
		if *s.Mean != 0 {
			s.TInfinite = true
		} else {
			z := 0.0
			s.T = &z
		}
		return s
	}
	t := *s.Mean / math.Sqrt(ss/float64(len(xs)-1)/float64(len(xs)))
	s.T = &t
	return s
}
func trendLabelAt(ls breakout.TrendShortLabels, h int) breakout.TrendShortLabel {
	if h == 4 {
		return ls.H4
	}
	return ls.H24
}
func trendLabelValid(l breakout.TrendShortLabel) bool {
	return l.Done && !l.Unpriceable && !math.IsNaN(l.NetPct) && !math.IsInf(l.NetPct, 0)
}

func aggregateTrendShadow(rows []breakout.TrendShortShadowRow) trendShadowStats {
	s := trendShadowStats{ShadowOnly: true, Counts: map[string]int{}, PerSymbol: map[string]int{}, Edge: map[string]trendEdgeStats{}}
	for _, k := range []string{"picks", "sampled_hours", "scan_failures", "control_symbols", "unpriceable_4h", "unpriceable_24h", "labelled_4h", "labelled_24h", "pending_4h", "pending_24h", "control_labelled_4h", "control_labelled_24h", "control_unpriceable_4h", "control_unpriceable_24h", "control_pending_4h", "control_pending_24h"} {
		s.Counts[k] = 0
	}
	countLabel := func(prefix string, ls breakout.TrendShortLabels) {
		for _, h := range []int{4, 24} {
			l := trendLabelAt(ls, h)
			suffix := "4h"
			if h == 24 {
				suffix = "24h"
			}
			if l.Unpriceable {
				s.Counts[prefix+"unpriceable_"+suffix]++
			} else if trendLabelValid(l) {
				s.Counts[prefix+"labelled_"+suffix]++
			} else {
				s.Counts[prefix+"pending_"+suffix]++
			}
		}
	}
	path := &trendPathStats{}
	rsum := 0.0
	for _, r := range rows {
		if r.Kind == "pick" {
			s.Counts["picks"]++
			s.PerSymbol[r.Symbol]++
			countLabel("", r.Labels)
			l := r.Labels.H24
			if trendLabelValid(l) && (l.Path == "tp_first" || l.Path == "sl_first" || l.Path == "timeout") {
				path.N++
				rsum += l.R
				switch l.Path {
				case "tp_first":
					path.TPFirst++
				case "sl_first":
					path.SLFirst++
				case "timeout":
					path.Timeout++
				}
			}
		} else if r.Kind == "control" {
			s.Counts["sampled_hours"]++
			s.Counts["scan_failures"] += r.Failures
			s.Counts["control_symbols"] += len(r.Prices)
			for sym := range r.Prices {
				countLabel("control_", r.ControlLabels[sym])
			}
		}
	}
	if path.N > 0 {
		m := rsum / float64(path.N)
		path.MeanR = &m
	}
	s.Picks.Path = path
	for _, h := range []int{4, 24} {
		pickHours := map[int64][]float64{}
		controlHours := map[int64]float64{}
		var picks, controls []float64
		for _, r := range rows {
			hour := r.TS / time.Hour.Milliseconds()
			if r.Kind == "pick" {
				l := trendLabelAt(r.Labels, h)
				if trendLabelValid(l) {
					picks = append(picks, l.NetPct)
					pickHours[hour] = append(pickHours[hour], l.NetPct)
				}
			}
			if r.Kind == "control" {
				complete := true
				var vals []float64
				symbols := make([]string, 0, len(r.Prices))
				for sym := range r.Prices {
					symbols = append(symbols, sym)
				}
				sort.Strings(symbols)
				for _, sym := range symbols {
					l := trendLabelAt(r.ControlLabels[sym], h)
					if !l.Done {
						complete = false
					}
					if trendLabelValid(l) {
						vals = append(vals, l.NetPct)
					}
				}
				if complete && len(vals) > 0 {
					m := *trendStats(vals).Mean
					controls = append(controls, m)
					controlHours[hour] = m
				}
			}
		}
		var differences []float64
		hours := make([]int64, 0, len(pickHours))
		for hour := range pickHours {
			hours = append(hours, hour)
		}
		sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
		for _, hour := range hours {
			if control, ok := controlHours[hour]; ok {
				differences = append(differences, *trendStats(pickHours[hour]).Mean-control)
			}
		}
		if h == 4 {
			s.Picks.H4 = trendStats(picks)
			s.Controls.H4 = trendStats(controls)
			s.Edge["h4"] = trendEdge(differences)
		} else {
			s.Picks.H24 = trendStats(picks)
			s.Controls.H24 = trendStats(controls)
			s.Edge["h24"] = trendEdge(differences)
		}
	}
	return s
}

func trendOverlap(rows []breakout.TrendShortShadowRow, decisions []store.CandidatePoolRecord) (matched, n int) {
	times := map[string][]int64{}
	for _, d := range decisions {
		var symbols []string
		if json.Unmarshal([]byte(d.CandidateCoins), &symbols) != nil {
			continue
		}
		for _, sym := range symbols {
			times[sym] = append(times[sym], d.Timestamp.UnixMilli())
		}
	}
	for sym := range times {
		sort.Slice(times[sym], func(i, j int) bool { return times[sym][i] < times[sym][j] })
	}
	window := (10 * time.Minute).Milliseconds()
	for _, r := range rows {
		if r.Kind != "pick" {
			continue
		}
		n++
		ts := times[r.Symbol]
		i := sort.Search(len(ts), func(i int) bool { return ts[i] >= r.TS-window })
		if i < len(ts) && ts[i] <= r.TS+window {
			matched++
		}
	}
	return
}

func (s *Server) handleTrendShortShadow(c *gin.Context) {
	rows, err := breakout.ReadTrendShortShadow()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot read trend short shadow journal"})
		return
	}
	stats := aggregateTrendShadow(rows)
	var lo, hi int64
	for _, r := range rows {
		if r.Kind != "pick" {
			continue
		}
		if lo == 0 || r.TS < lo {
			lo = r.TS
		}
		if r.TS > hi {
			hi = r.TS
		}
	}
	if lo > 0 {
		if s.store == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decision store unavailable"})
			return
		}
		decisions, err := s.store.Decision().GetCandidatePoolsBetween(time.UnixMilli(lo).Add(-10*time.Minute), time.UnixMilli(hi).Add(10*time.Minute))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot read live candidate history"})
			return
		}
		stats.OverlapMatched, stats.OverlapN = trendOverlap(rows, decisions)
		if stats.OverlapN > 0 {
			fraction := float64(stats.OverlapMatched) / float64(stats.OverlapN)
			stats.OverlapLivePool = &fraction
		}
	}
	c.JSON(http.StatusOK, stats)
}
