package kernel

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"nofx/store"
)

func TestReview20261028RemRRDedupPreservesQualifyingStructure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		long       bool
		structural []float64
		boll       float64
		costBps    float64
		want       float64
	}{
		{"long review example", true, []float64{105}, 104.99, 0, 105},
		{"short mirror", false, []float64{95}, 95.01, 0, 95},
		{"long nearer structure", true, []float64{105}, 105.01, 0, 105},
		{"short nearer structure", false, []float64{95}, 94.99, 0, 95},
		{"long threshold crossing", true, []float64{104.99, 105}, 0, 0, 105},
		{"short threshold crossing", false, []float64{95.01, 95}, 0, 0, 95},
		{"long cost threshold", true, []float64{105.34, 105.35}, 0, 10, 105.35},
		{"short cost threshold", false, []float64{94.66, 94.65}, 0, 10, 94.65},
		{"exact overlap", true, []float64{105}, 105, 0, 105},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, fullStructure := range []bool{false, true} {
				tf := &TFSignal{BOLLSourced: map[float64]bool{}}
				displayed := append([]float64{}, tc.structural...)
				if tc.boll != 0 {
					displayed = append(displayed, tc.boll)
					tf.BOLLSourced[tc.boll] = true
				}
				if tc.long {
					tf.Resistance = displayed
					if fullStructure {
						tf.StructuralResistance = tc.structural
					}
				} else {
					tf.Support = displayed
					if fullStructure {
						tf.StructuralSupport = tc.structural
					}
				}
				// Exact-overlap provenance is resolved across timeframes too.
				tfs := map[string]*TFSignal{"15m": tf}
				if tc.boll == tc.want {
					tfs["1h"] = &TFSignal{Resistance: tc.structural}
				}
				stop := 98.0
				if !tc.long {
					stop = 102
				}
				out := scanRRWindowWithCost(100, "live_price", 2, stop, tfs, tc.long, 2.5,
					15*time.Minute, 4*time.Hour, tc.costBps)
				if !out.Usable || out.FirstRRGeTarget != tc.want || out.TargetsScanned != 1 ||
					len(out.Options) != 1 || out.Options[0].Level != tc.want {
					t.Fatalf("fullStructure=%v: qualifying structure lost: %+v", fullStructure, out)
				}
			}
		})
	}
}

func TestReview20261028RemTPChoicesAreDecisionLocal(t *testing.T) {
	gs := &GateState{
		LongStopPlanPrice: 98, LongTakeProfit: 105,
		ShortStopPlanPrice: 102, ShortTakeProfit: 95,
		LongTPMenu:  []TPOption{{Level: 105}, {Level: 110}},
		ShortTPMenu: []TPOption{{Level: 95}, {Level: 90}},
	}
	before := *gs
	body := `[
		{"symbol":"BTCUSDT","action":"open_long","leverage":3,"position_size_usd":100,"tp_option":2},
		{"symbol":"BTCUSDT","action":"open_long","leverage":3,"position_size_usd":100},
		{"symbol":"BTCUSDT","action":"open_short","leverage":3,"position_size_usd":100,"tp_option":2},
		{"symbol":"BTCUSDT","action":"open_short","leverage":3,"position_size_usd":1,"tp_option":2},
		{"symbol":"BTCUSDT","action":"open_short","leverage":3,"position_size_usd":100,"tp_option":99}
	]`
	fd, err := parseFullDecisionResponse(body, 1000, 5, 3, 0.5, 0.3, 5, nil,
		map[string]*GateState{"BTCUSDT": gs}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{110, 105, 90, 0, 95} {
		if fd.Decisions[i].TakeProfit != want {
			t.Errorf("decision %d: TP=%g, want %g", i, fd.Decisions[i].TakeProfit, want)
		}
	}
	if fd.Decisions[3].Action != "wait" || fd.Decisions[4].TPOption != 0 {
		t.Fatalf("rejection/invalid-option handling changed: %+v", fd.Decisions)
	}
	if !reflect.DeepEqual(*gs, before) {
		t.Fatalf("menu choices modified the shared snapshot: %+v", *gs)
	}
}

func TestReview20261028RemPromptContract(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		for _, limit := range []bool{false, true} {
			for _, menu := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/limit=%v/menu=%v", lang, limit, menu), func(t *testing.T) {
					cfg := store.GetDefaultStrategyConfig(lang)
					cfg.RiskControl.LimitEntryEnabled = limit
					cfg.RiskControl.TPMenuEnabled = &menu
					prompt := NewStrategyEngine(&cfg).BuildSystemPrompt(1000, "")
					for _, want := range []string{
						"price/stop_loss/take_profit 为0或省略=按程序计划填充",
						"不交易就 wait/hold",
						"open_*/close_*/partial_close_* 另填 `reasoning`(一句,≤60字)",
					} {
						if !strings.Contains(prompt, want) {
							t.Errorf("prompt missing %q", want)
						}
					}
					if strings.Contains(prompt, "为0表示不可交易或被抑制") {
						t.Fatal("obsolete zero semantics remain")
					}
					start := strings.Index(prompt, "<decision>\n") + len("<decision>\n")
					end := strings.Index(prompt[start:], "</decision>") + start
					examples, err := extractDecisions(prompt[start:end])
					if err != nil || len(examples) != 3 {
						t.Fatalf("invalid prompt examples: %+v, %v", examples, err)
					}
					if n := utf8.RuneCountInString(examples[0].Reasoning); n == 0 || n > 60 {
						t.Fatalf("opening example reasoning length=%d", n)
					}
					if examples[1].Reasoning != "" || examples[2].Reasoning != "" {
						t.Fatal("wait/hold examples should stay compact")
					}
				})
			}
		}
	}
}

func TestReview20261028RemReasoningSurvivesParsing(t *testing.T) {
	for _, action := range []string{
		"open_long", "open_short", "open_long_limit", "open_short_limit",
		"close_long", "close_short", "partial_close_long", "partial_close_short",
	} {
		t.Run(action, func(t *testing.T) {
			stop, target := 98.0, 105.0
			if strings.Contains(action, "short") {
				stop, target = 102, 95
			}
			const reason = "结构信号确认，按程序方案执行。"
			body := fmt.Sprintf(`<decision>[{"symbol":"BTCUSDT","action":%q,"reasoning":%q,"leverage":3,"position_size_usd":100,"price":100,"stop_loss":%g,"take_profit":%g,"close_fraction":0.25}]</decision>`, action, reason, stop, target)
			fd, err := parseFullDecisionResponse(body, 1000, 5, 3, 0.5, 0.3, 5,
				map[string]bool{"BTCUSDT": true}, nil, nil)
			if err != nil || len(fd.Decisions) != 1 || fd.Decisions[0].Reasoning != reason {
				t.Fatalf("reasoning lost: fd=%+v err=%v", fd, err)
			}
		})
	}
}

func TestReview20261028RemZeroAndOmittedPricesUsePlans(t *testing.T) {
	for _, action := range []string{"open_long_limit", "open_short_limit"} {
		for _, prices := range []string{"", `,"price":0,"stop_loss":0,"take_profit":0`} {
			gs := &GateState{LongStopPlanPrice: 98, LongTakeProfit: 105, ShortStopPlanPrice: 102, ShortTakeProfit: 95}
			body := fmt.Sprintf(`[{"symbol":" btcusdt ","action":%q,"leverage":5,"position_size_usd":400%s}]`, action, prices)
			fd, err := parseFullDecisionResponse(body, 1000, 5, 3, 0.5, 0.3, 5, nil,
				map[string]*GateState{"BTCUSDT": gs}, map[string]*LimitAnchor{"BTCUSDT": {LimitBuy: 100, LimitSell: 100}})
			if err != nil {
				t.Fatal(err)
			}
			d := fd.Decisions[0]
			wantStop, wantTP := 98.0, 105.0
			if action == "open_short_limit" {
				wantStop, wantTP = 102, 95
			}
			if d.Symbol != "BTCUSDT" || d.Price != 100 || d.StopLoss != wantStop || d.TakeProfit != wantTP || d.Leverage != 5 {
				t.Fatalf("program plan/major-coin limits not adopted: %+v", d)
			}
		}
	}
}

func TestReview20261028RemSymbolNormalization(t *testing.T) {
	for _, tc := range []struct {
		symbol, want string
		size         float64
		leverage     int
	}{
		{" btcusdt \n", "BTCUSDT", 400, 5},
		{"\tEthUsdt ", "ETHUSDT", 400, 5},
		{" solusdt ", "SOLUSDT", 100, 3},
		{" btc ", "BTC", 100, 3},
	} {
		d := Decision{Symbol: tc.symbol, Action: "open_long", Leverage: 10,
			PositionSizeUSD: tc.size, StopLoss: 98, TakeProfit: 105}
		if err := validateDecision(&d, 1000, 5, 3, 0.5, 0.3, 5, false, nil); err != nil {
			t.Fatalf("%q: %v", tc.symbol, err)
		}
		if d.Symbol != tc.want || d.Leverage != tc.leverage {
			t.Errorf("symbol/limits: got %q/%d, want %q/%d", d.Symbol, d.Leverage, tc.want, tc.leverage)
		}
	}
}

func TestReview20261028RemRejectedTPChoiceDoesNotMutateGate(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mixed=%v", side, mixed), func(t *testing.T) {
				gs := &GateState{
					LongStopPlanPrice: 98, LongTakeProfit: 105,
					ShortStopPlanPrice: 102, ShortTakeProfit: 95,
					LongTPMenu:  []TPOption{{Level: 105}, {Level: 110}},
					ShortTPMenu: []TPOption{{Level: 95}, {Level: 90}},
				}
				before := *gs
				body := fmt.Sprintf(`[{"symbol":"BTCUSDT","action":"open_%s","leverage":3,"position_size_usd":1,"tp_option":2}`, side)
				if mixed {
					body += `,{"symbol":"ETHUSDT","action":"close_long"}`
				}
				body += "]"
				fd, err := parseFullDecisionResponse(body, 1000, 5, 3, 0.5, 0.3, 5, nil,
					map[string]*GateState{"BTCUSDT": gs}, nil)
				if mixed && (err != nil || fd.Decisions[0].Action != "wait") {
					t.Fatalf("mixed batch should replace invalid open: fd=%+v err=%v", fd, err)
				}
				if !mixed && err == nil {
					t.Fatal("undersized open should fail validation")
				}
				if !reflect.DeepEqual(*gs, before) {
					t.Fatalf("rejected tp_option polluted shared gate: before=%+v after=%+v", before, *gs)
				}
			})
		}
	}
}
