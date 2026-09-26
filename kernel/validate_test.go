package kernel

import (
	"testing"
)

// TestLeverageFallback tests automatic correction when leverage exceeds limit
func TestLeverageFallback(t *testing.T) {
	tests := []struct {
		name            string
		decision        Decision
		accountEquity   float64
		btcEthLeverage  int
		altcoinLeverage int
		wantLeverage    int // Expected leverage after correction
		wantError       bool
		minPositionSize float64 // strategy-config min opening notional (USDT)
	}{
		{
			name: "Altcoin leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 100,
				StopLoss:        50,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5, // Limit 5x
			minPositionSize: 12,
			wantLeverage:    5, // Should be corrected to 5
			wantError:       false,
		},
		{
			name: "BTC leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "BTCUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 1000,
				StopLoss:        90000,
				TakeProfit:      110000,
			},
			accountEquity:   100,
			btcEthLeverage:  10, // Limit 10x
			altcoinLeverage: 5,
			minPositionSize: 12,
			wantLeverage:    10, // Should be corrected to 10
			wantError:       false,
		},
		{
			name: "Leverage within limit - no correction",
			decision: Decision{
				Symbol:          "ETHUSDT",
				Action:          "open_short",
				Leverage:        5, // Not exceeded
				PositionSizeUSD: 500,
				StopLoss:        4000,
				TakeProfit:      3000,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    5, // Stays unchanged
			minPositionSize: 12,
			wantError:       false,
		},
		{
			name: "Leverage is 0 - should error",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        0, // Invalid
				PositionSizeUSD: 100,
				StopLoss:        50,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    0,
			minPositionSize: 12,
			wantError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use default position value ratios for testing (10x for BTC/ETH, 1.5x for altcoins)
			err := validateDecision(&tt.decision, tt.accountEquity, tt.btcEthLeverage, tt.altcoinLeverage, 10.0, 1.5, tt.minPositionSize, false, nil)

			// Check error status
			if (err != nil) != tt.wantError {
				t.Errorf("validateDecision() error = %v, wantError %v", err, tt.wantError)
				return
			}

			// If shouldn't error, check if leverage was correctly corrected
			if !tt.wantError && tt.decision.Leverage != tt.wantLeverage {
				t.Errorf("Leverage not corrected: got %d, want %d", tt.decision.Leverage, tt.wantLeverage)
			}
		})
	}
}

// contains checks if string contains substring (helper function)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// The binding minimum opening notional is the STRATEGY CONFIG's
// min_position_size — never a hardcoded constant (user 09-16: the validator's
// hardcoded 12 rejected an 8.66 USDT opening the strategy (min 5) had told
// the model was legal; the snapshot min_size block and the executor already
// used the config value).
func TestMinPositionSizeUsesStrategyConfig(t *testing.T) {
	decision := Decision{
		Symbol:          "QCOMUSDT",
		Action:          "open_long",
		Leverage:        3,
		PositionSizeUSD: 8.66,
		StopLoss:        180,
		TakeProfit:      200,
	}

	// Strategy min 5 → 8.66 is legal (the exact rejected-in-production case).
	if err := validateDecision(&decision, 52, 10, 5, 0.5, 1.5, 5, true, nil); err != nil {
		t.Fatalf("config min 5 must accept 8.66 USDT: %v", err)
	}
	// BTC/ETH previously carried a hardcoded 60 — the config value rules there
	// too now (the exchange's own min-notional stays the last-resort check).
	if err := validateDecision(&decision, 52, 10, 5, 0.5, 1.5, 5, true, nil); err != nil {
		t.Fatalf("BTC/ETH min must also follow the config: %v", err)
	}
	btc := decision
	btc.Symbol = "BTCUSDT"
	if err := validateDecision(&btc, 52, 10, 5, 0.5, 1.5, 5, true, nil); err != nil {
		t.Fatalf("config min 5 must accept 8.66 USDT on BTCUSDT: %v", err)
	}

	// Unset config (≤0) falls back to the executor-mirrored 12.
	if err := validateDecision(&decision, 52, 10, 5, 0.5, 1.5, 0, false, nil); err == nil {
		t.Fatal("fallback min 12 must reject 8.66 USDT")
	}
	if err := validateDecision(&decision, 52, 10, 5, 0.5, 1.5, 12, false, nil); err == nil {
		t.Fatal("explicit min 12 must reject 8.66 USDT")
	}
}

// decision_stage is fully derived (schema-redundancy audit 09-16): action +
// backend-known position state determine it — the model no longer outputs the
// field, so an action/stage contradiction is impossible by construction.
func TestDeriveDecisionStage(t *testing.T) {
	cases := []struct {
		action      string
		hasPosition bool
		want        string
	}{
		{"open_long", false, "TRIGGERED"},
		{"open_short", false, "TRIGGERED"},
		{"open_long_limit", false, "TRIGGERED"},
		{"open_short_limit", false, "TRIGGERED"},
		{"close_long", true, "EXIT"},
		{"partial_close_short", true, "EXIT"},
		{"adjust_stop_loss", true, "IN_POSITION"},
		{"hold", true, "IN_POSITION"},
		{"hold", false, "NO_SETUP"},
		{"unknown", false, "NO_SETUP"},
	}
	for _, c := range cases {
		if got := DeriveDecisionStage(c.action, c.hasPosition); got != c.want {
			t.Errorf("DeriveDecisionStage(%q, %v) = %s, want %s", c.action, c.hasPosition, got, c.want)
		}
	}
	// End-to-end: validateDecision overwrites whatever the model declared.
	d := Decision{Symbol: "TUSDT", Action: "hold", Stage: "TRIGGERED"} // contradictory on purpose
	if err := validateDecision(&d, 100, 3, 3, 1, 1, 12, true, nil); err != nil {
		t.Fatalf("hold must validate: %v", err)
	}
	if d.Stage != "IN_POSITION" {
		t.Fatalf("declared stage %q survived; want derived IN_POSITION", d.Stage)
	}
	d2 := Decision{Symbol: "TUSDT", Action: "hold", Stage: "TRIGGERED"}
	if err := validateDecision(&d2, 100, 3, 3, 1, 1, 12, false, nil); err == nil {
		t.Fatal("flat hold must be rejected; a flat symbol uses wait")
	}
}
