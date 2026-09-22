// placetp places a PARTIAL take-profit algo order for an open position —
// the one-off repair path for positions whose split-TP leg failed to place
// (AKEUSDT/PHAUSDT 2026-09-22: the ReduceOnly param drew Binance -1106 and
// the runner mark kept the watchdog from re-placing). NOT for regular use:
// new opens place their split TP inline.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"

	"nofx/trader/binance"
	"nofx/config"
	"nofx/crypto"
	"nofx/store"

	"github.com/joho/godotenv"
)

func main() {
	userID := flag.String("user", "f6b845ba-f7b2-4b27-8501-fcf6af94f276", "owner user id")
	accountName := flag.String("account", "mac-nofx", "exchange account_name")
	symbol := flag.String("symbol", "", "symbol with the open position")
	positionSide := flag.String("side", "LONG", "position side SHORT|LONG")
	fraction := flag.Float64("fraction", 0.5, "fraction of the position the TP closes")
	tp := flag.Float64("tp", 0, "take-profit trigger price (0 = refuse — never guess)")
	dryRun := flag.Bool("dryrun", false, "compute and print, place nothing")
	flag.Parse()

	if *symbol == "" || *tp <= 0 {
		fmt.Fprintln(os.Stderr, "-symbol and a positive -tp are required")
		os.Exit(1)
	}
	_ = godotenv.Load()
	config.Init()

	cryptoService, err := crypto.NewCryptoService()
	if err != nil {
		fmt.Fprintln(os.Stderr, "crypto:", err)
		os.Exit(1)
	}
	crypto.SetGlobalCryptoService(cryptoService)

	cfg := config.Get()
	st, err := store.NewWithConfig(store.DBConfig{Type: store.DBTypeSQLite, Path: cfg.DBPath})
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		os.Exit(1)
	}
	defer st.Close()

	exs, err := st.Exchange().List(*userID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list exchanges:", err)
		os.Exit(1)
	}
	var ex *store.Exchange
	for _, e := range exs {
		if e.AccountName == *accountName && e.ExchangeType == "binance" {
			ex = e
			break
		}
	}
	if ex == nil {
		fmt.Fprintf(os.Stderr, "no binance exchange account %q for user\n", *accountName)
		os.Exit(1)
	}

	ft := binance.NewFuturesTrader(string(ex.APIKey), string(ex.SecretKey), ex.UserID)
	positions, err := ft.GetPositions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "positions:", err)
		os.Exit(1)
	}
	var posQty float64
	found := false
	for _, p := range positions {
		if sym, _ := p["symbol"].(string); sym == *symbol {
			if q, _ := p["positionAmt"].(float64); q != 0 {
				posQty = math.Abs(q)
				found = true
			}
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "no open position on", *symbol)
		os.Exit(1)
	}

	qty := posQty * *fraction
	if *dryRun {
		fmt.Printf("DRY RUN: %s %s TP %.8g on %.8g of %.8g (%.0f%%)\n", *symbol, *positionSide, *tp, qty, posQty, *fraction*100)
		return
	}
	if err := ft.SetTakeProfit(*symbol, *positionSide, qty, *tp); err != nil {
		fmt.Fprintln(os.Stderr, "place TP:", err)
		os.Exit(1)
	}
	fmt.Printf("✅ %s %s partial TP placed: %.8g @ %.8g (%.0f%% of %.8g)\n", *symbol, *positionSide, qty, *tp, *fraction*100, posQty)
}
