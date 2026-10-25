package api

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"nofx/store"

	"github.com/gin-gonic/gin"
)

// shadow48hWindow is the span of one 48h counterfactual. It mirrors trader's
// gateShadowHorizon48, which is unexported there. review 2026-10-09 F
const shadow48hWindow = 48 * time.Hour

// handleGateShadowStats aggregates the gate shadow-block counterfactuals
// (09-21 user directive): for every BLOCKED would-be trade that matured,
// what would have happened? Grouped per blocking code — this is the
// calibration dataset for the numeric thresholds (min_rr, consensus 50,
// vendor 1%). Outcome R: tp_first = +plan_rr, sl_first = -1R, timeout =
// marked-to-market in R.
func (s *Server) handleGateShadowStats(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	limit := 2000
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 && l <= 5000 {
		limit = l
	}

	type tally struct {
		Total     int     `json:"total"`
		TpFirst   int     `json:"tp_first"`
		SlFirst   int     `json:"sl_first"`
		Timeout   int     `json:"timeout"`
		Unfilled  int     `json:"unfilled"`
		NoData    int     `json:"no_data"`
		WinRate   float64 `json:"win_rate_pct"`
		SumR      float64 `json:"sum_r"`
		AvgR      float64 `json:"avg_r"`
		RSamples  int     `json:"r_samples"`
		PlannedRR float64 `json:"avg_planned_rr"`
	}
	overall := &tally{}
	byCode := map[string]*tally{}
	bySole := map[string]*tally{}
	overall48 := &tally{}
	byCode48 := map[string]*tally{}
	bySole48 := map[string]*tally{}

	// review 2026-10-09 F: gather every trader's rows before tallying, so the 48h
	// overlap thinning can group them by trader+symbol+direction. ListEvaluated
	// returns newest first.
	var rows []*store.GateShadowBlock
	traders, _ := s.store.Trader().List(userID)
	for _, t := range traders {
		traderRows, err := s.store.GateShadow().ListEvaluated(t.ID, limit)
		if err != nil {
			continue
		}
		rows = append(rows, traderRows...)
	}
	keep48, droppedOverlap48 := thinShadowOverlap48h(rows)
	for _, r := range rows {
		// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r.
		// Each horizon contributes R only from its own verdict and exit;
		// the 48h R never falls back to the 8h exit.
		pnlR, okR := shadowR(r.Direction, r.Outcome, r.EntryPrice, r.StopPrice, r.ExitPrice)
		outcome48 := r.Outcome48
		pnlR48, okR48 := shadowR(r.Direction, outcome48, r.EntryPrice, r.StopPrice, r.ExitPrice48)
		add := func(t *tally, outcome string, sumR float64, rOK bool) {
			t.Total++
			t.PlannedRR += r.PlanRR
			if rOK {
				t.SumR += sumR
				t.RSamples++
			}
			switch outcome {
			case "tp_first":
				t.TpFirst++
			case "sl_first":
				t.SlFirst++
			case "timeout":
				t.Timeout++
			case "unfilled":
				t.Unfilled++
			default:
				t.NoData++
			}
		}
		add(overall, r.Outcome, pnlR, okR)
		// review 2026-10-09 F: the 48h tallies only see the rows the overlap
		// thinning kept. A thinned row still counts in the 8h tallies.
		counts48 := keep48[r]
		if counts48 {
			add(overall48, outcome48, pnlR48, okR48)
		}
		// any-blocker view: a row counts once under every family it touched.
		for _, family := range shadowCodeFamilies(r.BlockedCodes) {
			if byCode[family] == nil {
				byCode[family] = &tally{}
			}
			add(byCode[family], r.Outcome, pnlR, okR)
			if counts48 {
				if byCode48[family] == nil {
					byCode48[family] = &tally{}
				}
				add(byCode48[family], outcome48, pnlR48, okR48)
			}
		}
		// review 2026-10-09 F: sole view. A row counts under a family only when
		// that family is its one and only blocker (repeated families collapse).
		if families := shadowCodeFamilies(r.BlockedCodes); len(families) == 1 {
			family := families[0]
			if bySole[family] == nil {
				bySole[family] = &tally{}
			}
			add(bySole[family], r.Outcome, pnlR, okR)
			if counts48 {
				if bySole48[family] == nil {
					bySole48[family] = &tally{}
				}
				add(bySole48[family], outcome48, pnlR48, okR48)
			}
		}
	}
	finalize := func(t *tally) {
		decided := t.TpFirst + t.SlFirst
		if decided > 0 {
			t.WinRate = float64(t.TpFirst) / float64(decided) * 100
		}
		// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r.
		// AvgR divides by the rows that carry a usable R (r_samples), not Total.
		if t.RSamples > 0 {
			t.AvgR = t.SumR / float64(t.RSamples)
		}
		if t.Total > 0 {
			t.PlannedRR = t.PlannedRR / float64(t.Total)
		}
	}
	finalize(overall)
	for _, t := range byCode {
		finalize(t)
	}
	for _, t := range bySole {
		finalize(t)
	}
	finalize(overall48)
	for _, t := range byCode48 {
		finalize(t)
	}
	for _, t := range bySole48 {
		finalize(t)
	}

	c.JSON(http.StatusOK, gin.H{
		"note":                     "shadow counterfactuals of BLOCKED gate directions; dual horizon 8h+48h (48h fills only after rows mature; 48h tallies drop rows whose window overlaps an earlier kept row of the same trader+symbol+direction, counted in rows_dropped_overlap_48h); same-bar TP+SL counts as sl_first (conservative); by_code counts a row under every family it touched (any-blocker view) while by_code_sole counts it only when that family was its sole blocker (what loosening that gate alone would let through)",
		"rows_evaluated":           len(rows),
		"rows_dropped_overlap_48h": droppedOverlap48,
		"overall":                  overall,
		"by_code":                  byCode,
		"by_code_sole":             bySole,
		"overall_48h":              overall48,
		"by_code_48h":              byCode48,
		"by_code_sole_48h":         bySole48,
	})
}

// review 2026-10-09 F: the 48h windows of rows created ~8h apart overlap ~6x,
// so the 48h tallies keep only non-overlapping windows. thinShadowOverlap48h
// walks each symbol+direction group oldest first (across traders: traders on
// the same symbol and side share one price path) and keeps a row only
// when its CreatedAt is at least shadow48hWindow after the last kept row.
// Rows without a 48h verdict take no part. It returns the kept set and the
// number of 48h rows dropped.
func thinShadowOverlap48h(rows []*store.GateShadowBlock) (map[*store.GateShadowBlock]bool, int) {
	type key struct{ symbol, direction string }
	groups := map[key][]*store.GateShadowBlock{}
	for _, r := range rows {
		if r.Outcome48 == "" {
			continue
		}
		k := key{r.Symbol, r.Direction}
		groups[k] = append(groups[k], r)
	}
	kept := map[*store.GateShadowBlock]bool{}
	dropped := 0
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			if !group[i].CreatedAt.Equal(group[j].CreatedAt) {
				return group[i].CreatedAt.Before(group[j].CreatedAt)
			}
			return group[i].ID < group[j].ID
		})
		last := -1
		for i, r := range group {
			if last >= 0 && r.CreatedAt.Before(group[last].CreatedAt.Add(shadow48hWindow)) {
				dropped++
				continue
			}
			kept[r] = true
			last = i
		}
	}
	return kept, dropped
}

// shadowCodeFamily strips the numeric suffix from one blocked code
// (RR_MAX_0.07 → RR_MAX): the family is the threshold being calibrated, not
// the instance. An empty entry maps to "".
func shadowCodeFamily(code string) string {
	code = strings.TrimSpace(code)
	for _, prefix := range []string{"RR_MAX_", "VENDOR_DIVERGENCE_", "CONSENSUS_OPPOSED_"} {
		if strings.HasPrefix(code, prefix) {
			return strings.TrimSuffix(prefix, "_")
		}
	}
	return code
}

// shadowCodeFamilies returns the distinct code families of one row's
// BlockedCodes in first-seen order (RR_MAX_0.07,RR_MAX_0.10 → [RR_MAX]).
// review 2026-10-09 F
func shadowCodeFamilies(blockedCodes string) []string {
	var families []string
	for _, code := range strings.Split(blockedCodes, ",") {
		family := shadowCodeFamily(code)
		if family == "" || slices.Contains(families, family) {
			continue
		}
		families = append(families, family)
	}
	return families
}

// shadowR is one horizon's R multiple for a gate shadow row; ok=false when
// that horizon has no usable R. Only a real verdict (tp_first, sl_first,
// timeout) with a positive exit and a positive risk counts.
// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r with a bogus
// -EntryPrice/risk (long) or +EntryPrice/risk (short).
func shadowR(direction, outcome string, entry, stop, exit float64) (float64, bool) {
	if outcome != "tp_first" && outcome != "sl_first" && outcome != "timeout" {
		return 0, false
	}
	if exit <= 0 {
		return 0, false
	}
	risk := entry - stop
	pnl := exit - entry
	if direction == "short" {
		risk = stop - entry
		pnl = entry - exit
	}
	if risk <= 0 {
		return 0, false
	}
	return pnl / risk, true
}
