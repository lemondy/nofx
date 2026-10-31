package trader

import (
	"math"
	"testing"
	"time"

	"nofx/fundsledger"
	"nofx/kernel"
	"nofx/store"
)

func TestReview20261029AIContextUsesTWRDrawdown(t *testing.T) {
	for _, tc := range []struct {
		name         string
		equities     []float64
		flow         float64 // at snapshot 2
		window       int
		want         float64
		peak, trough int
		basis        string
	}{
		{"withdrawal_full_history", []float64{100, 110, 60, 58}, -45, -1, (1 - (105.0/110)*(58.0/60)) * 100, 1, 3, "equity_twr"},
		{"withdrawal_rolling", []float64{100, 110, 60, 58}, -45, 30, (1 - (105.0/110)*(58.0/60)) * 100, 1, 3, "equity_twr"},
		{"deposit_does_not_hide_loss", []float64{100, 80, 180, 162}, 100, 30, 28, 0, 3, "equity_twr"},
		{"withdrawal_without_drawdown", []float64{100, 110, 65}, -45, 30, 0, 0, 0, "equity_twr"},
		{"closed_trades_without_provenance", nil, 0, 30, 5, 0, 0, "closed_trades"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			sqlDB, err := st.GormDB().DB()
			if err != nil {
				t.Fatal(err)
			}
			sqlDB.SetMaxOpenConns(1) // Keep all queries on the same in-memory SQLite database.
			at := riskTestTrader(store.RiskControlConfig{})
			at.id, at.store, at.exchange, at.exchangeID = "ai-mdd", st, "binance", "e"
			at.initialBalance, at.startTime = 100, time.Now()
			at.trader = &audit05Mock{flat: true}
			at.config.StrategyConfig.CoinSource.SourceType = "static"
			at.config.StrategyConfig.StatsWindowDays = tc.window
			at.strategyEngine = kernel.NewStrategyEngine(at.config.StrategyConfig)
			base := time.Now().UTC().Truncate(24 * time.Hour).Add(-7*24*time.Hour + 23*time.Hour + 30*time.Minute)
			for _, row := range []interface{}{
				&store.Trader{ID: at.id, ExchangeID: at.exchangeID},
				&store.Exchange{ID: at.exchangeID, ExchangeType: "binance"},
				&store.TraderPosition{TraderID: at.id, ExchangeID: at.exchangeID, Symbol: "XUSDT", Side: "LONG", Status: "CLOSED",
					EntryTime: base.UnixMilli(), ExitTime: base.Add(4 * time.Hour).UnixMilli(), RealizedPnL: -5},
			} {
				if err := st.GormDB().Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			for i, eq := range tc.equities {
				// Store converts these local timestamps to UTC; the context must format UTC dates.
				atTime := base.Add(time.Duration(i) * time.Hour).In(time.FixedZone("UTC+8", 8*60*60))
				if err := st.Equity().Save(&store.EquitySnapshot{TraderID: at.id, Timestamp: atTime, TotalEquity: eq}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.flow != 0 {
				if err := st.AccountIncome().Save(at.exchangeID, []fundsledger.Income{{TranID: "transfer", IncomeType: "TRANSFER", Asset: "USDT", Amount: tc.flow, TimeMS: base.Add(2 * time.Hour).UnixMilli()}}); err != nil {
					t.Fatal(err)
				}
			}
			stats, err := st.Position().GetRollingStats(at.id, at.initialBalance, at.config.StrategyConfig.EffectiveStatsWindowDays())
			if err != nil {
				t.Fatal(err)
			}
			if stats.TotalTrades != 1 || stats.MaxDrawdownBasis != tc.basis {
				t.Fatalf("store stats=%+v", stats)
			}
			ctx, err := at.buildTradingContext()
			if err != nil {
				t.Fatal(err)
			}
			got := ctx.TradingStats
			if got == nil || math.Abs(got.MaxDrawdownPct-tc.want) > 1e-9 || got.MaxDrawdownPct != stats.MaxDrawdownPct || got.WindowDays != stats.WindowDays {
				t.Fatalf("AI stats=%+v, want DD=%.10f (store=%+v)", got, tc.want, stats)
			}
			if tc.want == 0 || tc.basis != "equity_twr" {
				if got.MaxDDPeakAt != "" || got.MaxDDTroughAt != "" || got.MaxDDPeakEquity != 0 || got.MaxDDTroughEquity != 0 {
					t.Fatalf("unexpected provenance: %+v", got)
				}
				return
			}
			if got.MaxDDPeakEquity != tc.equities[tc.peak] || got.MaxDDTroughEquity != tc.equities[tc.trough] ||
				got.MaxDDPeakAt != base.Add(time.Duration(tc.peak)*time.Hour).Format("01-02") ||
				got.MaxDDTroughAt != base.Add(time.Duration(tc.trough)*time.Hour).Format("01-02") {
				t.Fatalf("wrong AI peak/trough provenance: %+v", got)
			}
		})
	}
}
