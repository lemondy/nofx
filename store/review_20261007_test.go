package store

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func newReview07Store(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "review07.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// 2026-10-07 review N1: open → partial close → final close must store the
// SUM of the legs, not double the earlier legs and the entry fee.
func TestReview07CloseDoesNotDoubleCountPnLOrFee(t *testing.T) {
	st := newReview07Store(t)
	pb := NewPositionBuilder(st.Position())
	now := time.Now().UnixMilli()
	steps := []struct {
		action            string
		qty, px, fee, pnl float64
	}{
		{"open_long", 100, 1.00, 0.02, 0},
		{"close_long", 60, 0.98, 0.03, -1.2},
		{"close_long", 40, 0.98, 0.02, -0.8},
	}
	for i, s := range steps {
		if err := pb.ProcessTrade("t", "ex", "binance", "XUSDT", "LONG", s.action, s.qty, s.px, s.fee, s.pnl, now+int64(i), "o"); err != nil {
			t.Fatal(err)
		}
	}
	var row TraderPosition
	if err := st.gdb.Where("trader_id = ? AND symbol = ?", "t", "XUSDT").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "CLOSED" {
		t.Fatalf("status=%s", row.Status)
	}
	if math.Abs(row.RealizedPnL-(-2.0)) > 1e-9 || math.Abs(row.Fee-0.07) > 1e-9 {
		t.Fatalf("realized_pnl=%.4f fee=%.4f, want -2.0000 / 0.0700", row.RealizedPnL, row.Fee)
	}
}

// Single-leg close: fee = entry fee + exit fee (entry fee was doubled).
func TestReview07SingleLegCloseFee(t *testing.T) {
	st := newReview07Store(t)
	pb := NewPositionBuilder(st.Position())
	now := time.Now().UnixMilli()
	_ = pb.ProcessTrade("t", "ex", "binance", "XUSDT", "SHORT", "open_short", 10, 2, 0.01, 0, now, "o1")
	_ = pb.ProcessTrade("t", "ex", "binance", "XUSDT", "SHORT", "close_short", 10, 1.9, 0.02, 1.0, now+1, "o2")
	var row TraderPosition
	st.gdb.Where("trader_id = ?", "t").First(&row)
	if math.Abs(row.RealizedPnL-1.0) > 1e-9 || math.Abs(row.Fee-0.03) > 1e-9 {
		t.Fatalf("realized_pnl=%.4f fee=%.4f, want 1.0000 / 0.0300", row.RealizedPnL, row.Fee)
	}
}

// Orphan reconcile closes with a zero delta: the row's accumulation is kept.
func TestReview07ZeroDeltaCloseKeepsAccumulation(t *testing.T) {
	st := newReview07Store(t)
	pb := NewPositionBuilder(st.Position())
	now := time.Now().UnixMilli()
	_ = pb.ProcessTrade("t", "ex", "binance", "XUSDT", "LONG", "open_long", 10, 1, 0.01, 0, now, "o1")
	_ = pb.ProcessTrade("t", "ex", "binance", "XUSDT", "LONG", "close_long", 4, 1.1, 0.01, 0.4, now+1, "o2")
	var row TraderPosition
	st.gdb.Where("trader_id = ?", "t").First(&row)
	if err := st.Position().ClosePositionFully(row.ID, 1.1, "", now+2, 0, 0, "netting_reconcile"); err != nil {
		t.Fatal(err)
	}
	st.gdb.First(&row, row.ID)
	if math.Abs(row.RealizedPnL-0.4) > 1e-9 || math.Abs(row.Fee-0.02) > 1e-9 {
		t.Fatalf("realized_pnl=%.4f fee=%.4f, want 0.4000 / 0.0200", row.RealizedPnL, row.Fee)
	}
}

// 2026-10-07 review N2: the AI marks the exchange ORDER id; the sync must
// hand the builder that same order id (not the fill id) for the row to be
// attributed to the AI book.
func TestReview07OwnershipByExchangeOrderID(t *testing.T) {
	st := newReview07Store(t)
	pb := NewPositionBuilder(st.Position())
	now := time.Now().UnixMilli()
	if err := st.AIManaged().MarkEntry("t", "YUSDT", "long", "482032646"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade("t", "ex", "binance", "YUSDT", "LONG", "open_long", 10, 1, 0, 0, now, "482032646"); err != nil {
		t.Fatal(err)
	}
	var row TraderPosition
	st.gdb.Where("trader_id = ? AND symbol = ?", "t", "YUSDT").First(&row)
	if !row.AIManaged {
		t.Fatal("AI entry keyed by exchange order id was not attributed to the AI book")
	}
}

// Repair recomputes a doubled row from fills and is idempotent.
func TestReview07RepairCloseAccumulation(t *testing.T) {
	st := newReview07Store(t)
	now := time.Now().UnixMilli()
	pos := &TraderPosition{TraderID: "t", Symbol: "XUSDT", Side: "LONG", Quantity: 100, EntryQuantity: 100,
		EntryPrice: 1, EntryTime: now, ExitTime: now + 10, Status: "CLOSED", RealizedPnL: -3.2, Fee: 0.12, Leverage: 1}
	if err := st.gdb.Create(pos).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.gdb.Create(&TradeJournalDB{TraderID: "t", PositionID: pos.ID, Symbol: "XUSDT", Side: "LONG", RealizedPnL: -3.2, Fee: 0.12}).Error; err != nil {
		t.Fatal(err)
	}
	for i, f := range []struct {
		pnl, fee float64
	}{{0, 0.02}, {-1.2, 0.03}, {-0.8, 0.02}} {
		o := &TraderOrder{TraderID: "t", ExchangeID: "ex", ExchangeOrderID: fmt.Sprint("x", i), Symbol: "XUSDT", Side: "BUY", PositionSide: "LONG", Type: "MARKET", Status: "FILLED"}
		if err := st.gdb.Create(o).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.gdb.Create(&TraderFill{TraderID: "t", ExchangeID: "ex", OrderID: o.ID, ExchangeOrderID: o.ExchangeOrderID, ExchangeTradeID: o.ExchangeOrderID,
			Symbol: "XUSDT", Side: "BUY", RealizedPnL: f.pnl, Commission: f.fee, CommissionAsset: "USDT", CreatedAt: now + int64(i)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	fixes, err := st.RepairCloseAccumulation("t", now, false)
	if err != nil || len(fixes) != 1 {
		t.Fatalf("dry-run fixes=%v err=%v", fixes, err)
	}
	var row TraderPosition
	st.gdb.First(&row, pos.ID)
	if row.RealizedPnL != -3.2 {
		t.Fatal("dry-run wrote")
	}
	if _, err := st.RepairCloseAccumulation("t", now, true); err != nil {
		t.Fatal(err)
	}
	st.gdb.First(&row, pos.ID)
	var j TradeJournalDB
	st.gdb.Where("position_id = ?", pos.ID).First(&j)
	if math.Abs(row.RealizedPnL+2.0) > 1e-9 || math.Abs(row.Fee-0.07) > 1e-9 || math.Abs(j.RealizedPnL+2.0) > 1e-9 || math.Abs(j.Fee-0.07) > 1e-9 {
		t.Fatalf("after repair pos=%.4f/%.4f journal=%.4f/%.4f", row.RealizedPnL, row.Fee, j.RealizedPnL, j.Fee)
	}
	if again, _ := st.RepairCloseAccumulation("t", now, true); len(again) != 0 {
		t.Fatalf("repair not idempotent: %v", again)
	}
}

// Re-keying a fill-keyed row by its AI order id stamps ownership.
func TestReview07ReattributeEntryOrder(t *testing.T) {
	st := newReview07Store(t)
	now := time.Now().UnixMilli()
	_ = st.AIManaged().MarkEntry("t", "YUSDT", "long", "482032646")
	_ = st.AIManaged().Unmark("t", "YUSDT", "long")
	pos := &TraderPosition{TraderID: "t", Symbol: "YUSDT", Side: "LONG", Quantity: 1, EntryPrice: 1, EntryTime: now, EntryOrderID: "46693004", Status: "CLOSED"}
	st.gdb.Create(pos)
	owned, err := st.ReattributeEntryOrder("t", pos.ID, "YUSDT", "long", "482032646", true)
	if err != nil || !owned {
		t.Fatalf("owned=%v err=%v", owned, err)
	}
	var row TraderPosition
	st.gdb.First(&row, pos.ID)
	if !row.AIManaged || row.EntryOrderID != "482032646" {
		t.Fatalf("row=%+v", row)
	}
}

// 2026-10-07 review N4: bounds + exit-ladder relations, house 0/negative
// semantics preserved.
func TestReview07ValidateBoundsAndLadder(t *testing.T) {
	ok := func(mut func(*RiskControlConfig)) error {
		c := &StrategyConfig{}
		mut(&c.RiskControl)
		return c.Validate()
	}
	for name, mut := range map[string]func(*RiskControlConfig){
		"defaults":     func(r *RiskControlConfig) {},
		"negative-off": func(r *RiskControlConfig) { r.DailyMaxLossPct, r.MaxAccountRiskPct = -1, -1 },
		"live ladder": func(r *RiskControlConfig) {
			r.BreakevenArmR, r.TPTrimAtR, r.TPFullAtR, r.PeakDrawdownArmR = 0.5, 1.2, -1, 1
		},
		"be below lock": func(r *RiskControlConfig) { r.BreakevenArmR, r.ProfitLockAtR = 0.5, 1 },
		"risk at cap":   func(r *RiskControlConfig) { r.RiskPerTradePct = 10 },
	} {
		if err := ok(mut); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	for name, mut := range map[string]func(*RiskControlConfig){
		"risk 50%":         func(r *RiskControlConfig) { r.RiskPerTradePct = 50 },
		"daily 150%":       func(r *RiskControlConfig) { r.DailyMaxLossPct = 150 },
		"giveback > 1":     func(r *RiskControlConfig) { r.PeakDrawdownGivebackR = 1.5 },
		"full below trim":  func(r *RiskControlConfig) { r.TPTrimAtR, r.TPFullAtR = 2, 1.5 },
		"be after lock":    func(r *RiskControlConfig) { r.BreakevenArmR, r.ProfitLockAtR = 1.5, 1 },
		"negative acct dd": func(r *RiskControlConfig) { r.AccountMaxDrawdownPct = -5 },
	} {
		if err := ok(mut); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
