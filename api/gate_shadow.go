package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

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
		NoData    int     `json:"no_data"`
		WinRate   float64 `json:"win_rate_pct"`
		SumR      float64 `json:"sum_r"`
		AvgR      float64 `json:"avg_r"`
		RSamples  int     `json:"r_samples"`
		PlannedRR float64 `json:"avg_planned_rr"`
	}
	overall := &tally{}
	byCode := map[string]*tally{}
	overall48 := &tally{}
	byCode48 := map[string]*tally{}

	traders, _ := s.store.Trader().List(userID)
	fetched := 0
	for _, t := range traders {
		rows, err := s.store.GateShadow().ListEvaluated(t.ID, limit)
		if err != nil {
			continue
		}
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
				default:
					t.NoData++
				}
			}
			add(overall, r.Outcome, pnlR, okR)
			if outcome48 != "" {
				add(overall48, outcome48, pnlR48, okR48)
			}
			for _, code := range strings.Split(r.BlockedCodes, ",") {
				code = strings.TrimSpace(code)
				// Strip the numeric suffix (RR_MAX_0.07 → RR_MAX): the code
				// family is the threshold being calibrated, not the instance.
				for _, prefix := range []string{"RR_MAX_", "VENDOR_DIVERGENCE_", "CONSENSUS_OPPOSED_"} {
					if strings.HasPrefix(code, prefix) {
						code = strings.TrimSuffix(prefix, "_")
						break
					}
				}
				if code == "" {
					continue
				}
				if byCode[code] == nil {
					byCode[code] = &tally{}
				}
				add(byCode[code], r.Outcome, pnlR, okR)
				if outcome48 != "" {
					if byCode48[code] == nil {
						byCode48[code] = &tally{}
					}
					add(byCode48[code], outcome48, pnlR48, okR48)
				}
			}
			fetched++
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
	finalize(overall48)
	for _, t := range byCode48 {
		finalize(t)
	}

	c.JSON(http.StatusOK, gin.H{
		"note":           "shadow counterfactuals of BLOCKED gate directions; dual horizon 8h+48h (48h fills only after rows mature); same-bar TP+SL counts as sl_first (conservative)",
		"rows_evaluated": fetched,
		"overall":        overall,
		"by_code":        byCode,
		"overall_48h":    overall48,
		"by_code_48h":    byCode48,
	})
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
