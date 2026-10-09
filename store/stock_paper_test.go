package store

import (
	"math"
	"testing"
	"time"
)

func TestStockPaperLedgerRoundTrip(t *testing.T) {
	st := newTestStore(t)
	sp := st.StockPaper()
	now := time.Now()
	acct, err := sp.EnsureAccount("tr", 0)
	if err != nil || acct.Cash != DefaultStockPaperCash {
		t.Fatalf("default account %+v %v", acct, err)
	}
	if again, _ := sp.EnsureAccount("tr", 777); again.Cash != DefaultStockPaperCash {
		t.Fatal("EnsureAccount must not reset an existing ledger")
	}
	if _, err := sp.OpenOrAdd("tr", "AAPLBUSDT", 10, 100, 95, 120, now); err != nil {
		t.Fatal(err)
	}
	pos, err := sp.OpenOrAdd("tr", "AAPLBUSDT", 10, 110, 100, 0, now)
	if err != nil || math.Abs(pos.AvgPrice-105) > 1e-9 || pos.Quantity != 20 || pos.InitialStop != 95 || pos.Stop != 100 || pos.TakeProfit != 0 {
		t.Fatalf("merged position %+v %v", pos, err)
	}
	pnl, closed, err := sp.Reduce("tr", "AAPLBUSDT", 5, 115, now, "ai_reduce")
	if err != nil || closed || math.Abs(pnl-50) > 1e-9 {
		t.Fatalf("partial reduce pnl=%v closed=%v err=%v", pnl, closed, err)
	}
	pnl, closed, err = sp.Reduce("tr", "AAPLBUSDT", 0, 125, now, "ai_close")
	if err != nil || !closed || math.Abs(pnl-15*20) > 1e-9 {
		t.Fatalf("full close pnl=%v closed=%v err=%v", pnl, closed, err)
	}
	rows, _ := sp.ListClosed("tr", 10)
	if len(rows) != 1 || rows[0].Status != StockPaperClosed || math.Abs(rows[0].RealizedPnL-350) > 1e-9 || math.Abs(rows[0].ExitPrice-(5*115+15*125)/20.0) > 1e-9 {
		t.Fatalf("closed row %+v", rows)
	}
	if open, _ := sp.ListOpen("tr"); len(open) != 0 {
		t.Fatal("no open rows expected")
	}
	acct, _ = sp.GetAccount("tr")
	if math.Abs(acct.Cash-(10000+350)) > 1e-9 {
		t.Fatalf("cash %v", acct.Cash)
	}
}

func TestApplyExternalReductionKeepsCostBase(t *testing.T) {
	st := newTestStore(t)
	row := &TraderPosition{TraderID: "tr", Symbol: "AAPLBUSDT", Side: "LONG", Quantity: 10, EntryQuantity: 10, EntryPrice: 100, EntryTime: 1, AIManaged: true, Status: "OPEN"}
	if err := st.Position().CreateOpenPosition(row); err != nil {
		t.Fatal(err)
	}
	if err := st.Position().ApplyExternalReduction(row.ID, 6, 101, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.Position().StockOwnedRows("tr", "AAPLBUSDT")
	if len(rows) != 1 || rows[0].Quantity != 6 || rows[0].EntryQuantity != 6 || rows[0].RealizedPnL != 0 {
		t.Fatalf("after shrink %+v", rows)
	}
	if err := st.Position().ApplyExternalReduction(row.ID, 0, 101, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rows, _ = st.Position().StockOwnedRows("tr", "AAPLBUSDT"); len(rows) != 0 {
		t.Fatal("row should be closed")
	}
	closed, _ := st.Position().GetClosedPositions("tr", 5)
	if len(closed) != 1 || closed[0].CloseReason != "external" || closed[0].RealizedPnL != 0 {
		t.Fatalf("closed %+v", closed)
	}
}
