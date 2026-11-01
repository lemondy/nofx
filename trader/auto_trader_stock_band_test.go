package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nofx/trader/types"
)

func TestStockEntrySellBand(t *testing.T) {
	for _, tc := range []struct {
		name, entryType string
		floor, down     float64
		reject          bool
	}{
		{"market below floor", "market", 116, .9, true},
		{"limit below floor", "limit", 116, .97, true},
		{"market uses current floor", "market", 115.2, .96, true},
		{"limit uses fill reference", "limit", 115.2, .96, false},
		{"market uses entry multiplier", "market", 108, .96, true},
		{"entry safety margin", "market", 114.5, .9, true},
		{"market in band", "market", 108, .9, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := fmt.Sprintf(`[{"symbol":"AAPLBUSDT","action":"open_long","entry_type":%q,"limit_price":119,"stop_loss":115,"take_profit":130,"confidence":80,"thesis":"trend continuation","invalidation":"close below stop","reasoning":"trend up"}]`, tc.entryType)
			fx := newStockFixture(t, false, stockTestNow, func(int) string { return decision })
			fx.spot.band = &types.SellBand{Floor: tc.floor, Ceiling: 144, DownMultiplier: tc.down, UpMultiplier: 1.2, Ref: 120}
			fx.cycle(t)
			rec := fx.lastRecord(t)
			if len(rec.Decisions) != 1 {
				t.Fatalf("decisions: %+v", rec.Decisions)
			}
			act := rec.Decisions[0]
			if tc.reject {
				if act.Success || !strings.Contains(act.Error, "STOP_BELOW_SELL_BAND") || len(fx.spot.buys) != 0 || fx.spot.limitBuys != 0 || len(fx.spot.setProt) != 0 {
					t.Fatalf("rejected entry sent orders: decision=%+v buys=%v limits=%d protection=%v", act, fx.spot.buys, fx.spot.limitBuys, fx.spot.setProt)
				}
				if alerts := entryAlerts(fx); len(alerts) != 1 || !strings.Contains(alerts[0], "止损距入场价超过交易所卖出价格带") {
					t.Fatalf("entry rejection alert: %v", alerts)
				}
			} else if !act.Success {
				t.Fatalf("in-band entry rejected: %+v", act)
			} else if tc.entryType == "limit" {
				if fx.spot.limitBuys != 1 || fx.at.stock.pendings["AAPLBUSDT"] == nil {
					t.Fatal("in-band limit entry not placed")
				}
			} else if len(fx.spot.buys) != 1 || len(fx.spot.setProt) != 1 {
				t.Fatalf("in-band entry not protected: buys=%v protection=%v", fx.spot.buys, fx.spot.setProt)
			}
		})
	}
}

// Hide optional capabilities while retaining the raw inventory read.
type spotWithoutSellBand struct{ types.SpotStockTrader }

func (s spotWithoutSellBand) RawStockBalance(sym string) (float64, error) {
	return s.SpotStockTrader.(*fakeSpot).RawStockBalance(sym)
}

func TestStockSellBandUnavailableAndPaperUnchanged(t *testing.T) {
	for _, mode := range []string{"unsupported", "read error", "paper"} {
		t.Run(mode, func(t *testing.T) {
			fx := newStockFixture(t, mode == "paper", stockTestNow, func(int) string { return openAAPL })
			fx.spot.band = &types.SellBand{Floor: 119, DownMultiplier: .99}
			if mode == "unsupported" {
				fx.at.stockTrader = spotWithoutSellBand{fx.spot}
				if _, ok, err := fx.at.stockSellBand("AAPLBUSDT"); ok || err != nil {
					t.Fatalf("optional capability: ok=%t err=%v", ok, err)
				}
			} else if mode == "read error" {
				fx.spot.bandErr = errors.New("average price unavailable")
			}
			fx.cycle(t)
			if rec := fx.lastRecord(t); len(rec.Decisions) != 1 || !rec.Decisions[0].Success {
				t.Fatalf("entry behavior changed: %+v", rec.Decisions)
			}
			if mode != "paper" && (len(fx.spot.buys) != 1 || len(fx.spot.setProt) != 1) {
				t.Fatalf("entry not protected: buys=%v protection=%v", fx.spot.buys, fx.spot.setProt)
			}
			if mode == "paper" && (len(fx.spot.buys) != 0 || len(fx.spot.setProt) != 0) {
				t.Fatal("paper entry touched exchange")
			}
		})
	}
}

func TestStockEntrySellBandUsesTighterExistingStop(t *testing.T) {
	decision := strings.Replace(openAAPL, "open_long", "add_long", 1)
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return decision })
	if _, err := fx.at.stockRecordBuy("AAPLBUSDT", 4, 119, 0, "seed1", 115, stockTestNow); err != nil {
		t.Fatal(err)
	}
	fx.spot.hold["AAPLBUSDT"] = 12
	if err := fx.at.stockPlaceProtection(context.Background(), "AAPLBUSDT", 4, 117, 130, 120); err != nil {
		t.Fatal(err)
	}
	fx.spot.band = &types.SellBand{Floor: 116, Ceiling: 144, DownMultiplier: .96}
	fx.cycle(t)
	if len(fx.spot.buys) != 1 || len(fx.spot.setProt) != 2 || fx.spot.setProt[1].Stop != 117 {
		t.Fatalf("add did not keep tighter stop: buys=%v protection=%v decision=%+v", fx.spot.buys, fx.spot.setProt, fx.lastRecord(t).Decisions)
	}
	if fx.spot.prot["AAPLBUSDT"].Quantity > fx.spot.hold["AAPLBUSDT"]-8+1e-9 {
		t.Fatal("manual shares protected")
	}
}

func stockBandAlerts(fx *stockFixture) []string {
	var out []string
	for _, a := range fx.alert {
		if strings.HasPrefix(a, "ALERT|") && strings.Contains(a, "美股止损超出交易所卖出价格带") {
			out = append(out, a)
		}
	}
	return out
}

func newStockBandHeldFixture(t *testing.T) *stockFixture {
	t.Helper()
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return holdAAPL })
	fx.spot.band = &types.SellBand{Floor: 116, Ceiling: 144, DownMultiplier: .96}
	fx.spot.hold["AAPLBUSDT"] = 12 // four program-owned, eight manual
	fx.at.stock.bandHeld = nil
	if _, err := fx.at.stockBuyFilled(context.Background(), "AAPLBUSDT", 4, 120, 0, 0, "seed1", 115, 130, nil, stockTestNow, func(string, ...interface{}) {}, nil); err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestStockBandHeldTickWaitsThenFallbackSellsOwned(t *testing.T) {
	fx := newStockBandHeldFixture(t)
	for i := 0; i < stockFirstProtectionMaxAttempts+2; i++ {
		fx.at.stockProtectionTick(context.Background())
	}
	if len(fx.spot.sells) != 0 || len(fx.spot.setProt) != 0 || fx.spot.cancelProt != 0 || !fx.at.stock.bandHeld["AAPLBUSDT"] {
		t.Fatalf("band hold mutated orders: sells=%v protection=%v cancels=%d held=%v", fx.spot.sells, fx.spot.setProt, fx.spot.cancelProt, fx.at.stock.bandHeld)
	}
	if alerts := stockBandAlerts(fx); len(alerts) != 1 || !strings.Contains(alerts[0], "程序每分钟按该止损监控") {
		t.Fatalf("band alerts: %v", alerts)
	}
	if n, err := fx.at.store.StockPaper().BumpProtectionIntent("t1", "AAPLBUSDT"); err != nil || n != 1 {
		t.Fatalf("band errors counted as failed attempts: n=%d err=%v", n, err)
	}
	fx.price, fx.spot.price = 115, 115
	fx.at.stockProtectionTick(context.Background())
	if len(fx.spot.sells) != 1 || fx.spot.sells[0].Qty != 4 || fx.spot.zeroSells != 0 || !near(fx.spot.hold["AAPLBUSDT"], 8) {
		t.Fatalf("fallback must only sell owned shares: sells=%v remaining=%v", fx.spot.sells, fx.spot.hold)
	}
	if qty, _ := ownedQty(t, fx); qty != 0 {
		t.Fatalf("program position not closed: %v", qty)
	}
}

func TestStockBandHeldRecoversAndClearsEpisode(t *testing.T) {
	fx := newStockBandHeldFixture(t)
	fx.at.stockProtectionTick(context.Background())
	fx.spot.band.Floor = 108
	fx.at.stockProtectionTick(context.Background())
	if fx.spot.prot["AAPLBUSDT"] == nil || len(fx.spot.setProt) != 1 || fx.spot.setProt[0].Qty != 4 {
		t.Fatalf("protection not restored for owned shares: %v", fx.spot.setProt)
	}
	if fx.at.stock.bandHeld["AAPLBUSDT"] || fx.at.stock.stopTicks["AAPLBUSDT"] != 0 {
		t.Fatal("successful placement did not clear held episode")
	}
	if intents, err := fx.at.store.StockPaper().ListProtectionIntents("t1"); err != nil || intents["AAPLBUSDT"] {
		t.Fatalf("protection intent not cleared: %v %v", intents, err)
	}
	// A subsequent missing-protection episode alerts again.
	fx.spot.cancelProtection("AAPLBUSDT")
	fx.spot.band.Floor = 116
	fx.at.stockProtectionTick(context.Background())
	if n := len(stockBandAlerts(fx)); n != 2 {
		t.Fatalf("new episode should alert again, got %d", n)
	}
}

func TestStockBandCheckPreservesExistingProtectionAndDropsHighTP(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return holdAAPL })
	fx.openLiveOwned(t, 4, 12)
	ctx := context.Background()
	if err := fx.at.stockPlaceProtection(ctx, "AAPLBUSDT", 4, 115, 130, 120); err != nil {
		t.Fatal(err)
	}
	old, cancels := fx.spot.prot["AAPLBUSDT"], fx.spot.cancelProt
	fx.spot.band = &types.SellBand{Floor: 116, Ceiling: 125}
	for i := 0; i < stockFirstProtectionMaxAttempts+2; i++ {
		err := fx.at.stockPlaceProtection(ctx, "AAPLBUSDT", 4, 115, 130, 120)
		if !errors.Is(err, types.ErrStopBelowSellBand) {
			t.Fatalf("missing band error: %v", err)
		}
	}
	if fx.spot.prot["AAPLBUSDT"] != old || fx.spot.cancelProt != cancels || len(fx.spot.setProt) != 1 {
		t.Fatal("band rejection cancelled or replaced live protection")
	}
	fx.spot.band.Floor = 108
	if err := fx.at.stockPlaceProtection(ctx, "AAPLBUSDT", 4, 115, 130, 120); err != nil {
		t.Fatal(err)
	}
	p := fx.spot.prot["AAPLBUSDT"]
	if p.TakeProfit != 0 || p.TakeProfitID != "" || p.Quantity != 4 || len(fx.spot.setProt) != 2 || fx.spot.setProt[1].Profit != 0 {
		t.Fatalf("high TP should become lone owned stop: %+v calls=%v", p, fx.spot.setProt)
	}
}

func TestStockExchangeBandRejectionWaitsAndEscapesAlert(t *testing.T) {
	for _, cause := range []error{
		fmt.Errorf("wrapped: %w", types.ErrStopBelowSellBand),
		errors.New("protection canceled; replacement stop failed: PERCENT_PRICE_BY_SIDE <floor>"),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			fx := newStockFixture(t, false, stockTestNow, func(int) string { return openAAPL })
			fx.spot.setProtErr = cause
			fx.cycle(t)
			for i := 0; i < stockFirstProtectionMaxAttempts+2; i++ {
				fx.at.stockProtectionTick(context.Background())
			}
			if len(fx.spot.sells) != 0 || len(stockBandAlerts(fx)) != 1 {
				t.Fatalf("exchange band error forced exit or repeated alerts: sells=%v alerts=%v", fx.spot.sells, stockBandAlerts(fx))
			}
			if strings.Contains(stockBandAlerts(fx)[0], "<floor>") {
				t.Fatal("band error not HTML escaped")
			}
		})
	}
}
