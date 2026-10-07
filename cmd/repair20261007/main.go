// repair20261007 fixes the persisted damage of the two data defects found by
// the 2026-10-07 review (docs/architecture/FULLSTACK_REVIEW_2026-10-07.md):
//
//	N1  closed rows with doubled PnL/fee → recomputed from trader_fills
//	N2  rows keyed by the fill id instead of the order id → re-keyed by the
//	    real Binance order id (read-only userTrades lookup) and stamped
//	    ai_managed when that order is a recorded AI entry
//
// Dry-run by default. -apply first writes a VACUUM INTO backup next to the
// database, then applies. Idempotent: re-running reports nothing to do.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"nofx/config"
	"nofx/crypto"
	"nofx/store"
	"nofx/trader/binance"

	"github.com/joho/godotenv"
)

func main() {
	userID := flag.String("user", "f6b845ba-f7b2-4b27-8501-fcf6af94f276", "owner user id")
	traderFilter := flag.String("trader", "", "only this trader id (default: all traders of the user)")
	since := flag.String("since", "2026-10-03T00:00:00+08:00", "repair rows exited (N1) / opened (N2) at or after this RFC3339 time")
	apply := flag.Bool("apply", false, "write changes (default: dry-run report only)")
	skipN2 := flag.Bool("skip-ownership", false, "skip the N2 exchange lookup")
	flag.Parse()

	sinceT, err := time.Parse(time.RFC3339, *since)
	if err != nil {
		fail("since: %v", err)
	}
	sinceMs := sinceT.UnixMilli()

	_ = godotenv.Load()
	config.Init()
	cs, err := crypto.NewCryptoService()
	if err != nil {
		fail("crypto: %v", err)
	}
	crypto.SetGlobalCryptoService(cs)
	cfg := config.Get()
	st, err := store.NewWithConfig(store.DBConfig{Type: store.DBTypeSQLite, Path: cfg.DBPath})
	if err != nil {
		fail("store: %v", err)
	}
	defer st.Close()

	if *apply {
		backup := fmt.Sprintf("%s.bak-%s-repair20261007", cfg.DBPath, time.Now().Format("20060102-150405"))
		if err := st.BackupTo(backup); err != nil {
			fail("backup: %v", err)
		}
		fmt.Println("backup written:", backup)
	} else {
		fmt.Println("DRY RUN — no changes are written (pass -apply)")
	}

	traders, err := st.Trader().List(*userID)
	if err != nil {
		fail("list traders: %v", err)
	}
	for _, tr := range traders {
		if *traderFilter != "" && tr.ID != *traderFilter {
			continue
		}
		fmt.Printf("\n== trader %s (%s)\n", tr.Name, tr.ID)

		fixes, err := st.RepairCloseAccumulation(tr.ID, sinceMs, *apply)
		if err != nil {
			fail("N1 repair: %v", err)
		}
		fmt.Printf("N1 PnL/fee rows to fix: %d\n", len(fixes))
		for _, f := range fixes {
			fmt.Printf("  #%d %-14s %-5s pnl %9.4f → %9.4f  fee %7.4f → %7.4f  (%d fills, journal rows %d)\n",
				f.PositionID, f.Symbol, f.Side, f.StoredPnL, f.FillsPnL, f.StoredFee, f.FillsFee, f.FillCount, f.JournalRows)
		}

		if *skipN2 {
			continue
		}
		ex, err := st.Exchange().GetByID(*userID, tr.ExchangeID)
		if err != nil || ex == nil {
			fmt.Printf("N2 skipped: exchange %s not found (%v)\n", tr.ExchangeID, err)
			continue
		}
		if ex.ExchangeType != "binance" {
			fmt.Printf("N2 skipped: exchange type %s (only binance lookup implemented)\n", ex.ExchangeType)
			continue
		}
		ft := binance.NewFuturesTrader(string(ex.APIKey), string(ex.SecretKey), ex.UserID)
		rows, err := st.PositionsKeyedByFillSince(tr.ID, sinceMs)
		if err != nil {
			fail("N2 list: %v", err)
		}
		rekeyed, owned := 0, 0
		for _, p := range rows {
			tradeID, err := strconv.ParseInt(p.EntryOrderID, 10, 64)
			if err != nil {
				continue
			}
			trades, err := ft.GetTradesForSymbolFromID(p.Symbol, tradeID, 1)
			if err != nil || len(trades) == 0 || trades[0].TradeID != p.EntryOrderID {
				// Already keyed by an order id (fromId lands on another
				// trade) or lookup failed — leave the row alone.
				continue
			}
			orderID := trades[0].OrderID
			if orderID == "" || orderID == p.EntryOrderID {
				continue
			}
			isAI, err := st.ReattributeEntryOrder(tr.ID, p.ID, p.Symbol, p.Side, orderID, *apply)
			if err != nil {
				fail("N2 row %d: %v", p.ID, err)
			}
			rekeyed++
			mark := ""
			if isAI && !p.AIManaged {
				owned++
				mark = "  → ai_managed=true"
			}
			fmt.Printf("  #%d %-14s %-5s entry_order_id %s → %s%s\n", p.ID, p.Symbol, p.Side, p.EntryOrderID, orderID, mark)
			time.Sleep(100 * time.Millisecond) // stay well under API weight limits
		}
		fmt.Printf("N2 rows re-keyed: %d, newly AI-attributed: %d\n", rekeyed, owned)
	}
}

func fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
