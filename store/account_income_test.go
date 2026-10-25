package store

import (
	"nofx/fundsledger"
	"testing"
	"time"
)

func TestAccountIncomeDedupClassificationAndWalletScope(t *testing.T) {
	s := newTestStore(t)
	income := s.AccountIncome()
	if err := income.initTables(); err != nil {
		t.Fatal(err)
	}
	rows := []fundsledger.Income{{TranID: "one", IncomeType: "TRANSFER", Asset: "USDT", Amount: 50, TimeMS: 100}, {TranID: "one", IncomeType: "COMMISSION", Asset: "USDT", Amount: -.2, TimeMS: 100}, {TranID: "unknown", IncomeType: "NEW_BONUS", Asset: "USDC", Amount: 1, TimeMS: 101}}
	for i := 0; i < 2; i++ {
		if err := income.Save("exchange", rows); err != nil {
			t.Fatal(err)
		}
	}
	got, err := income.List("exchange", 0, 200)
	if err != nil || len(got) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].Category != "transfer" || got[1].Category != "fee" || got[2].Category != "other" || got[2].IncomeType != "NEW_BONUS" {
		t.Fatal(got)
	}
	if err := income.Save("another", rows); err != nil {
		t.Fatal(err)
	}
	latest, err := income.LatestTime("exchange")
	if err != nil || latest != 101 {
		t.Fatal(latest, err)
	}
	spot := []fundsledger.Income{{TranID: "spot", IncomeType: "DEPOSIT", Asset: "USDT", Amount: 500, TimeMS: 200}}
	if err := income.SaveForKind("exchange", fundsledger.LiveSpot, spot); err != nil {
		t.Fatal(err)
	}
	got, err = income.ListForKind("exchange", fundsledger.LiveSpot, 0, 300)
	if err != nil || len(got) != 1 || got[0].Amount != 500 {
		t.Fatal(got, err)
	}
	got, err = income.ListForKind("exchange", fundsledger.LiveFutures, 0, 300)
	if err != nil || len(got) != 3 {
		t.Fatal(got, err)
	}
	got, _ = income.List("exchange", 101, 101)
	if len(got) != 1 {
		t.Fatal(got)
	}
	latest, err = income.LatestTimeForKind("exchange", fundsledger.LiveFutures)
	if err != nil || latest != 101 {
		t.Fatal(latest, err)
	}
}

func TestFundsStockGrossPnLClosedAndPartiallySoldOpenRow(t *testing.T) {
	s := newTestStore(t)
	for _, model := range []interface{}{&Trader{}, &Exchange{}, &Strategy{}, &TraderPosition{}, &StockPaperPosition{}, &EquitySnapshot{}, &AccountIncome{}} {
		if err := s.GormDB().AutoMigrate(model); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 24, 2, 0, 0, 0, time.UTC)
	e := Exchange{ID: "same", UserID: "u", ExchangeType: "binance", Name: "Binance"}
	if err := s.GormDB().Create(&e).Error; err != nil {
		t.Fatal(err)
	}
	strategy := Strategy{ID: "stock", UserID: "u", Name: "stock", Config: `{"strategy_type":"us_stock","stock_config":{"paper_trading":false}}`}
	if err := s.GormDB().Create(&strategy).Error; err != nil {
		t.Fatal(err)
	}
	tr := Trader{ID: "stock-trader", UserID: "u", Name: "us-stock", ExchangeID: e.ID, StrategyID: strategy.ID, InitialBalance: 100}
	if err := s.GormDB().Create(&tr).Error; err != nil {
		t.Fatal(err)
	}
	rows := []TraderPosition{{TraderID: tr.ID, Symbol: "AMDBUSDT", Status: "CLOSED", RealizedPnL: -.5, Fee: .02, ExitTime: now.Add(-time.Hour).UnixMilli()}, {TraderID: tr.ID, Symbol: "LLYBUSDT", Status: "OPEN", RealizedPnL: 4.35, Fee: .15, UpdatedAt: now.Add(-2 * time.Hour).UnixMilli()}, {TraderID: "foreign", Symbol: "XBUSDT", Status: "CLOSED", RealizedPnL: 9999, Fee: 10, ExitTime: now.UnixMilli()}}
	if err := s.GormDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	// A legacy simulated row must never leak into the live stock book.
	if err := s.GormDB().Create(&StockPaperPosition{TraderID: tr.ID, Symbol: "XBUSDT", Status: "CLOSED", RealizedPnL: 999, ClosedAt: now.UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Equity().Save(&EquitySnapshot{TraderID: tr.ID, Timestamp: now.Add(-24 * time.Hour), TotalEquity: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.Equity().Save(&EquitySnapshot{TraderID: tr.ID, Timestamp: now, TotalEquity: 103.68}); err != nil {
		t.Fatal(err)
	}
	in, _, err := s.FundsAccount("u", tr.ID, now)
	if err != nil || in.Kind != fundsledger.LiveSpot || len(in.Trades) != 2 {
		t.Fatal(in, err)
	}
	out := fundsledger.Aggregate([]fundsledger.AccountInput{in}, fundsledger.Options{Now: now, Days: 90, Location: time.UTC})
	a := out.Accounts[0]
	if diff := a.TradingPnL - 3.85; diff > 1e-8 || diff < -1e-8 {
		t.Fatal(a)
	}
	if a.Fees != .17 && a.Fees != .16999999999999998 {
		t.Fatal(a)
	}
	if len(out.Flows) != 0 {
		t.Fatal(out.Flows)
	}
	if _, _, err := s.FundsAccount("another-user", tr.ID, now); err == nil {
		t.Fatal("foreign trader returned")
	}
}

func TestFundsStockPaperUsesItsOwnLedger(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	for _, row := range []interface{}{&Exchange{ID: "stock", UserID: "u", ExchangeType: "binance", Name: "Binance"}, &Strategy{ID: "paper-stock", UserID: "u", Name: "stock", Config: `{"strategy_type":"us_stock","stock_config":{}}`}, &Trader{ID: "paper-stock", UserID: "u", Name: "paper", ExchangeID: "stock", StrategyID: "paper-stock", InitialBalance: 100}, &StockPaperPosition{TraderID: "paper-stock", Symbol: "XBUSDT", Status: "CLOSED", RealizedPnL: 2, ClosedAt: now.Add(-time.Hour).UnixMilli()}, &TraderPosition{TraderID: "paper-stock", Symbol: "XBUSDT", Status: "CLOSED", RealizedPnL: 999, ExitTime: now.Add(-time.Hour).UnixMilli()}} {
		if err := s.GormDB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	in, _, err := s.FundsAccount("u", "paper-stock", now)
	if err != nil || in.Kind != fundsledger.Paper || len(in.Trades) != 1 || in.Trades[0].PnL != 2 {
		t.Fatal(in, err)
	}
}
