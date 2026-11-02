package kernel

import (
	"math"
	"nofx/market"
)

// ParseReplayDecisions uses the production per-item parser and batch conflict
// resolver without fetching market data or executing actions. Equity and held
// symbols may come from the historical prompt; unknown equity is neutral.
// Missing position maps treat holds as held (opens are unaffected).
//
// No gate/anchor snapshots are supplied: anchor/SL/TP plan snapping, TP menus,
// anti-chase, rule checks, hard-entry gates, exposure/size caps and executor risk
// filters are NOT replayed. Structural validation (positive size/leverage,
// SL/TP ordering, required limit price, valid actions) still applies. A positive
// tiny minimum is intentional: zero activates the live default size floor.
func ParseReplayDecisions(resp string, equity float64, positions ...map[string]bool) ([]Decision, error) {
	var held map[string]bool
	if len(positions) > 0 {
		held = positions[0]
	} else {
		// Infer only hold carriers when historical positions are unavailable.
		// Extraction is read-only and uses the same repair/salvage path.
		if ds, err := extractDecisions(resp); err == nil {
			held = make(map[string]bool)
			for _, d := range ds {
				if d.Action == "hold" {
					held[market.Normalize(d.Symbol)] = true
				}
			}
		}
	}
	if equity <= 0 || math.IsNaN(equity) || math.IsInf(equity, 0) {
		equity = 1
	}
	ratio := math.MaxFloat64 / 4 / equity
	fd, err := parseFullDecisionResponse(resp, equity, math.MaxInt, math.MaxInt,
		ratio, ratio, math.SmallestNonzeroFloat64, held, nil, nil)
	if err != nil {
		return nil, err
	}
	return fd.Decisions, nil
}
