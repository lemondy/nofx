package breakout

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// TunableParams holds the engine parameters the auto-tuner may adjust. All
// values have hard bounds; the tuner moves them in small steps and persists
// to disk so tuning survives restarts.
type TunableParams struct {
	// Price dimension sigmoid (ATR-normalized breakout strength)
	PriceATRCenter float64 `json:"price_atr_center"` // default 1.0
	PriceATRWidth  float64 `json:"price_atr_width"`  // default 0.5

	// Volume dimension sigmoid center (same-slot volume multiple)
	VolCenter float64 `json:"vol_center"` // default 2.0

	// Timeframe resonance
	ResonanceGate  float64 `json:"resonance_gate"`  // default 60
	ResonanceBonus float64 `json:"resonance_bonus"` // default 5

	// Quality factors
	AlphaWeak   float64 `json:"alpha_weak"`   // default 0.6
	BetaCrowded float64 `json:"beta_crowded"` // default 0.85

	// Grade thresholds
	StrongThreshold float64 `json:"strong_threshold"` // default 80
	MediumThreshold float64 `json:"medium_threshold"` // default 60

	UpdatedAt  time.Time `json:"updated_at"`
	BacktestAt time.Time `json:"backtest_at"`
	Samples    int       `json:"samples"` // signals in the last backtest

	// Short-scan composite weights (online-tuned; keys in shortWeightKeys).
	ShortWeights map[string]float64 `json:"short_weights,omitempty"`

	// ShortTunerEnabled gates the ONLINE weight update (shorttuner.go step 2).
	// nil/false = disabled (default since 2026-09-22): the update multiplied
	// exp(η·corr) over the SAME cumulative cohort every 30 minutes, so any
	// stable-correlation component railed to the clamp bounds — live evidence:
	// overbought/parabolic pinned at 0.2984, divergence/rejection/structure/
	// volume_fade at 0.0298 (structure designed at 0.15). Signal SAMPLING and
	// outcome evaluation stay on; only the weight write is gated. Set true to
	// re-enable after the update rule is fixed (incremental window +
	// significance test).
	ShortTunerEnabled *bool `json:"short_tuner_enabled,omitempty"`
}

func defaultParams() TunableParams {
	return TunableParams{
		PriceATRCenter:  1.0,
		PriceATRWidth:   0.5,
		VolCenter:       2.0,
		ResonanceGate:   60,
		ResonanceBonus:  5,
		AlphaWeak:       0.6,
		BetaCrowded:     0.85,
		StrongThreshold: 80,
		MediumThreshold: 60,
		UpdatedAt:       time.Now(),
	}
}

// Hard bounds — the tuner can never step outside these.
var paramBounds = map[string][2]float64{
	"price_atr_center": {0.6, 1.6},
	"price_atr_width":  {0.3, 0.8},
	"vol_center":       {1.2, 3.0},
	"resonance_gate":   {50, 70},
	"resonance_bonus":  {3, 8},
	"alpha_weak":       {0.45, 0.8},
	"beta_crowded":     {0.7, 0.95},
	"strong_threshold": {72, 90},
	"medium_threshold": {50, 70},
}

func clampParam(key string, v float64) float64 {
	if b, ok := paramBounds[key]; ok {
		if v < b[0] {
			return b[0]
		}
		if v > b[1] {
			return b[1]
		}
	}
	return v
}

var (
	paramsMu      sync.RWMutex
	currentParams = defaultParams()
	paramsLoaded  bool
	paramsPath    = "data/breakout_params.json"
)

// SetParamsPath overrides the persistence path (tests).
func SetParamsPath(p string) {
	paramsMu.Lock()
	paramsPath = p
	paramsLoaded = false
	paramsMu.Unlock()
}

// GetParams returns a copy of the current tunable parameters (loading from
// disk on first access).
func GetParams() TunableParams {
	paramsMu.Lock()
	defer paramsMu.Unlock()
	loadParamsLocked()
	return currentParams
}

// ApplyParams merges new values (clamped) and persists.
func ApplyParams(p TunableParams) {
	paramsMu.Lock()
	defer paramsMu.Unlock()
	loadParamsLocked()
	p.UpdatedAt = time.Now()
	currentParams = clampParams(p)
	saveParamsLocked()
}

func clampParams(p TunableParams) TunableParams {
	p.PriceATRCenter = clampParam("price_atr_center", p.PriceATRCenter)
	p.PriceATRWidth = clampParam("price_atr_width", p.PriceATRWidth)
	p.VolCenter = clampParam("vol_center", p.VolCenter)
	p.ResonanceGate = clampParam("resonance_gate", p.ResonanceGate)
	p.ResonanceBonus = clampParam("resonance_bonus", p.ResonanceBonus)
	p.AlphaWeak = clampParam("alpha_weak", p.AlphaWeak)
	p.BetaCrowded = clampParam("beta_crowded", p.BetaCrowded)
	p.StrongThreshold = clampParam("strong_threshold", p.StrongThreshold)
	p.MediumThreshold = clampParam("medium_threshold", p.MediumThreshold)
	if p.MediumThreshold >= p.StrongThreshold-10 {
		p.MediumThreshold = p.StrongThreshold - 10
	}
	return p
}

func loadParamsLocked() {
	if paramsLoaded {
		return
	}
	paramsLoaded = true
	data, err := os.ReadFile(paramsPath)
	if err != nil {
		return
	}
	var p TunableParams
	if err := json.Unmarshal(data, &p); err != nil {
		return
	}
	currentParams = clampParams(p)
}

func saveParamsLocked() {
	data, err := json.MarshalIndent(currentParams, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll("data", 0o755)
	_ = os.WriteFile(paramsPath, data, 0o644)
}
