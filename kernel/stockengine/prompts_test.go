package stockengine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nofx/market/usstock"
	"nofx/store"
)

func TestSystemPromptsBandsAndContract(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		for _, name := range []string{store.StockPresetSwing, store.StockPresetPosition} {
			cfg := &store.StockConfig{Preset: name}
			p := ResolvePreset(cfg)
			prompt := BuildSystemPrompt(cfg, p, lang)
			band := "[1.50, 3.00]"
			if name == store.StockPresetPosition {
				band = "[2.00, 4.00]"
			}
			for _, token := range []string{band, "JSON", "OUTPUT CONTRACT", "quantity", "Binance bStock", "1R", "symbol", "action", "entry_type", "limit_price", "stop_loss", "take_profit", "reduce_fraction", "confidence", "reasoning"} {
				if !strings.Contains(prompt, token) {
					t.Fatalf("%s %s prompt missing %s", lang, name, token)
				}
			}
		}
		cfg := &store.StockConfig{StopATRMin: 1.75, StopATRMax: 2.25}
		if prompt := BuildSystemPrompt(cfg, ResolvePreset(cfg), lang); !strings.Contains(prompt, "[1.75, 2.25]") {
			t.Fatal(prompt)
		}
	}
	// Example uses exactly the contract's tags; no quantity field.
	var examples []map[string]any
	if err := json.Unmarshal([]byte(decisionContract), &examples); err != nil {
		t.Fatal(err)
	}
	if len(examples[0]) != 9 {
		t.Fatal(examples)
	}
}

func TestUserPromptSourcesPositionsAndR(t *testing.T) {
	c := testContext()
	holding(c)
	c.Account.Exposure = 500
	c.Snapshots[0].Underlying = "AAPL"
	c.Snapshots[0].Sources[usstock.TF1h] = usstock.SourceBStock
	// 1d is required by every preset → warned; 15m is optional for swing → not.
	c.Snapshots[0].MissingTF = []string{usstock.TF15m, usstock.TF1d}
	c.Market = []*SymbolSnapshot{{Symbol: "SPYBUSDT", Underlying: "SPY", TrendDaily: "up"}, {Symbol: "QQQBUSDT", Underlying: "QQQ", TrendDaily: "range"}}
	for _, lang := range []string{"zh", "en"} {
		prompt := BuildUserPrompt(c, lang)
		for _, token := range []string{"2026-10-09 10:00:00 EDT", "session=regular", "equity=10000.00", "available=10000.00", "(5.00%)", "qty=5.000000", "avg=90.00", "unrealized=11.11%", "R=2.00R", "held_days=2.0", "live_stop=94.00", "TP=0.00", "Yahoo", "1h: bStock", "MissingTF", "SPYBUSDT", "QQQBUSDT", "divergence=0.00%", "fresh=true"} {
			if !strings.Contains(prompt, token) {
				t.Fatalf("%s user prompt missing %s:\n%s", lang, token, prompt)
			}
		}
		if !strings.Contains(prompt, "MissingTF WARNING: 1d (") {
			t.Fatalf("%s: only the required 1d should be warned:\n%s", lang, prompt)
		}
		if lang == "zh" && !strings.Contains(prompt, "1d: Yahoo 正股 AAPL") {
			t.Fatal(prompt)
		}
	}
	c.Now = time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC)
	if prompt := BuildUserPrompt(c, "en"); !strings.Contains(prompt, "10:00:00 EST") {
		t.Fatal(prompt)
	}
}

func TestUserPromptMissingData(t *testing.T) {
	c := testContext()
	holding(c)
	c.Positions[0].InitialStop = 0
	c.Snapshots = nil
	p := BuildUserPrompt(c, "en")
	if !strings.Contains(p, "R=n/a") || !strings.Contains(p, "DATA_INSUFFICIENT") {
		t.Fatal(p)
	}
}
