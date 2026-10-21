package trader

import (
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/binance_bstock"
)

// Regression tests for the remaining 2026-10-20 us_stock review items
// (#4 protection ownership, #11 persisted weekly circuit).

func alertContains(fx *stockFixture, sub string) bool {
	for _, a := range fx.alert {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

// #4: a second automatic manager of the same symbol on the same account is
// refused before any order is sent.
func TestStockOpenRefusedWhenAnotherTraderManagesSymbol(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return openAAPL })
	fx.at.exchangeID = "ex1"
	if err := fx.at.store.Trader().Create(&store.Trader{ID: "t2", UserID: "u", Name: "other", ExchangeID: "ex1"}); err != nil {
		t.Fatal(err)
	}
	// A different account's trader on the same symbol must not matter.
	if err := fx.at.store.Trader().Create(&store.Trader{ID: "t3", UserID: "u", Name: "elsewhere", ExchangeID: "ex2"}); err != nil {
		t.Fatal(err)
	}
	if err := fx.at.store.StockPaper().SaveProtectionOrders([]store.StockProtectionOrder{
		{TraderID: "t3", Symbol: "AAPLBUSDT", OrderID: "31", Role: "stop_loss", Quantity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fx.at.store.StockPaper().SaveProtectionOrders([]store.StockProtectionOrder{
		{TraderID: "t2", Symbol: "AAPLBUSDT", OrderID: "21", Role: "stop_loss", Quantity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	fx.cycle(t)
	if len(fx.spot.buys) != 0 || fx.spot.limitBuys != 0 {
		t.Fatalf("an order was sent despite another manager: buys=%v limit=%d", fx.spot.buys, fx.spot.limitBuys)
	}
	if !alertContains(fx, "其他美股交易员") {
		t.Fatalf("no alert: %v", fx.alert)
	}
	if rec := fx.lastRecord(t); rec.Decisions[0].Success {
		t.Fatalf("the refused open must not be recorded as success: %+v", rec.Decisions[0])
	}
}

// A peer that is flat (all legs retired, no positions) does not block opening.
func TestStockOpenAllowedWhenPeerIsFlat(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return openAAPL })
	fx.at.exchangeID = "ex1"
	if err := fx.at.store.Trader().Create(&store.Trader{ID: "t2", UserID: "u", Name: "other", ExchangeID: "ex1"}); err != nil {
		t.Fatal(err)
	}
	sp := fx.at.store.StockPaper()
	if err := sp.SaveProtectionOrders([]store.StockProtectionOrder{{TraderID: "t2", Symbol: "AAPLBUSDT", OrderID: "21", Role: "stop_loss"}}); err != nil {
		t.Fatal(err)
	}
	if err := sp.RetireProtectionOrders("t2", "AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	fx.cycle(t)
	if len(fx.spot.buys) != 1 {
		t.Fatalf("flat peer must not block: buys=%v alerts=%v", fx.spot.buys, fx.alert)
	}
}

type ownerSpot struct {
	*fakeSpot
	tag   string
	owned func(symbol, orderID string) bool
}

func (o *ownerSpot) SetProtectionOwner(tag string, owned func(string, string) bool) {
	o.tag, o.owned = tag, owned
}

// #4: the executor is bound to this trader; an untagged (pre-upgrade) leg is
// claimed only through THIS trader's own registered order IDs.
func TestStockBindProtectionOwnerUsesOwnRegistryOnly(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return holdAAPL })
	ex := &ownerSpot{fakeSpot: fx.spot}
	fx.at.stockTrader = ex
	fx.at.stockBindProtectionOwner()
	if ex.tag != binance_bstock.OwnerTag("t1") || ex.tag == "" || ex.owned == nil {
		t.Fatalf("executor not bound: %+v", ex)
	}
	sp := fx.at.store.StockPaper()
	if err := sp.SaveProtectionOrders([]store.StockProtectionOrder{
		{TraderID: "t1", Symbol: "AAPLBUSDT", OrderID: "77", Role: "stop_loss"},
		{TraderID: "t2", Symbol: "AAPLBUSDT", OrderID: "88", Role: "stop_loss"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sym, id string
		want    bool
	}{{"AAPLBUSDT", "77", true}, {"AAPLBUSDT", "88", false}, {"AAPLBUSDT", "99", false}, {"MSFTBUSDT", "77", false}} {
		if got := ex.owned(c.sym, c.id); got != c.want {
			t.Fatalf("owned(%s,%s)=%v want %v", c.sym, c.id, got, c.want)
		}
	}
}

// #11: the weekly drawdown circuit survives a restart/reload within the ET
// week, is mode-specific, and clears in the next week.
func TestStockWeekHaltSurvivesRestartUntilNextWeek(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return holdAAPL })
	at := fx.at
	if halted, _ := at.stockWeekRisk(stockTestNow, 100); halted {
		t.Fatal("halted at week start")
	}
	if halted, dd := at.stockWeekRisk(stockTestNow, 85); !halted || dd < 14 {
		t.Fatalf("circuit not tripped: %v %v", halted, dd)
	}
	// Restart/reload: memory is gone, equity has partly recovered (5% drawdown).
	at.stock.weekKey, at.stock.weekStartEquity, at.stock.weekHalted = "", 0, false
	halted, dd := at.stockWeekRisk(stockTestNow.Add(time.Hour), 95)
	if !halted {
		t.Fatalf("circuit lifted by the restart (dd %.2f)", dd)
	}
	if dd < 4.9 || dd > 5.1 {
		t.Fatalf("baseline not restored, dd %.2f", dd)
	}
	// The other mode does not inherit the live circuit.
	paper := true
	at.config.StrategyConfig.StockConfig.PaperTrading = &paper
	at.stock.weekKey, at.stock.weekHalted = "", false
	if halted, _ := at.stockWeekRisk(stockTestNow.Add(time.Hour), 95); halted {
		t.Fatal("paper mode inherited the live circuit")
	}
	paper = false
	// A new ET week starts clear.
	at.stock.weekKey, at.stock.weekHalted = "", false
	if halted, _ := at.stockWeekRisk(stockTestNow.Add(8*24*time.Hour), 95); halted {
		t.Fatal("circuit carried into the next week")
	}
}
