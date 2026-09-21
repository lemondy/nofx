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
		PlannedRR float64 `json:"avg_planned_rr"`
	}
	overall := &tally{}
	byCode := map[string]*tally{}

	traders, _ := s.store.Trader().List(userID)
	fetched := 0
	for _, t := range traders {
		rows, err := s.store.GateShadow().ListEvaluated(t.ID, limit)
		if err != nil {
			continue
		}
		for _, r := range rows {
			risk := r.EntryPrice - r.StopPrice
			if r.Direction == "short" {
				risk = r.StopPrice - r.EntryPrice
			}
			pnlR := 0.0
			if risk > 0 {
				pnl := r.ExitPrice - r.EntryPrice
				if r.Direction == "short" {
					pnl = r.EntryPrice - r.ExitPrice
				}
				pnlR = pnl / risk
			}
			add := func(t *tally) {
				t.Total++
				t.SumR += pnlR
				t.PlannedRR += r.PlanRR
				switch r.Outcome {
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
			add(overall)
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
				add(byCode[code])
			}
			fetched++
		}
	}
	finalize := func(t *tally) {
		decided := t.TpFirst + t.SlFirst
		if decided > 0 {
			t.WinRate = float64(t.TpFirst) / float64(decided) * 100
		}
		if t.Total > 0 {
			t.AvgR = t.SumR / float64(t.Total)
			t.PlannedRR = t.PlannedRR / float64(t.Total)
		}
	}
	finalize(overall)
	for _, t := range byCode {
		finalize(t)
	}

	c.JSON(http.StatusOK, gin.H{
		"note":          "shadow counterfactuals of BLOCKED gate directions; 8h horizon; same-bar TP+SL counts as sl_first (conservative)",
		"rows_evaluated": fetched,
		"overall":       overall,
		"by_code":       byCode,
	})
}
