// protectposition is a one-off maintenance tool (09-19 PONS watchdog alert):
// places protective SL/TP algo orders for a naked position the watchdog
// cannot repair (no exchange order AND no recorded stop). SL distance =
// 1.5×ATR(1h) from the LIVE price; TP distance = rr × SL distance (1:2 RR,
// user directive). Prices round to the symbol's tick size. closePosition
// algo orders cannot flip the position.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"

	"nofx/config"
	"nofx/crypto"
	"nofx/market"
	"nofx/store"
	"nofx/trader/binance"

	"github.com/joho/godotenv"
)

func main() {
	userID := flag.String("user", "f6b845ba-f7b2-4b27-8501-fcf6af94f276", "owner user id")
	accountName := flag.String("account", "mac-nofx", "exchange account_name")
	symbol := flag.String("symbol", "PONSUSDT", "symbol of the naked position")
	positionSide := flag.String("side", "SHORT", "position side SHORT|LONG")
	rr := flag.Float64("rr", 2.0, "reward:risk ratio (TP distance = rr × SL distance)")
	dryRun := flag.Bool("dryrun", false, "compute and print, place nothing")
	flag.Parse()

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

	// Verify the position still exists on the exchange.
	positions, err := ft.GetPositions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "positions:", err)
		os.Exit(1)
	}
	var posQty, entry float64
	found := false
	for _, p := range positions {
		if sym, _ := p["symbol"].(string); sym == *symbol {
			if q, _ := p["positionAmt"].(float64); q != 0 {
				posQty = math.Abs(q)
				entry, _ = p["entryPrice"].(float64)
				found = true
			}
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "no open position on", *symbol, "— nothing to protect")
		os.Exit(1)
	}

	// Live price + ATR(1h) — the same yardstick the noise floor uses.
	md, err := market.GetWithTimeframes(*symbol, []string{"1h"}, "1h", 99)
	if err != nil {
		fmt.Fprintln(os.Stderr, "market:", err)
		os.Exit(1)
	}
	price := md.CurrentPrice
	tf1h := md.TimeframeData["1h"]
	if tf1h == nil || tf1h.ATR14 <= 0 || price <= 0 {
		fmt.Fprintln(os.Stderr, "no ATR(1h) available")
		os.Exit(1)
	}
	atrPct := tf1h.ATR14 / price * 100 // the same normalization the signal layer uses

	slDist := 1.5 * atrPct / 100 * price
	tpDist := *rr * slDist
	var slPrice, tpPrice float64
	if *positionSide == "SHORT" {
		slPrice, tpPrice = price+slDist, price-tpDist
	} else {
		slPrice, tpPrice = price-slDist, price+tpDist
	}

	prec, err := ft.GetSymbolPricePrecision(*symbol)
	if err != nil {
		fmt.Fprintln(os.Stderr, "precision:", err)
		os.Exit(1)
	}
	scale := math.Pow10(prec)
	round := func(v float64) float64 { return math.Round(v*scale) / scale }
	slPrice, tpPrice = round(slPrice), round(tpPrice)

	fmt.Printf("position: %s %s qty %.4f entry %.6g\n", *symbol, *positionSide, posQty, entry)
	fmt.Printf("live: %.6g | ATR(1h) %.2f%% | SL dist %.2f%% → SL %.6g | TP dist %.2f%% → TP %.6g | RR 1:%.1f\n",
		price, atrPct, slDist/price*100, slPrice, tpDist/price*100, tpPrice, *rr)
	if *positionSide == "SHORT" {
		fmt.Printf("sanity: SL %.6g > live %.6g > TP %.6g | entry %.6g (SL above entry caps loss at %.2f USDT notional-basis)\n",
			slPrice, price, tpPrice, entry, (slPrice-entry)*posQty)
	}
	if *dryRun {
		fmt.Println("dry-run: nothing placed")
		return
	}

	if err := ft.SetStopLoss(*symbol, *positionSide, posQty, slPrice); err != nil {
		fmt.Fprintln(os.Stderr, "place SL:", err)
		os.Exit(1)
	}
	if err := ft.SetTakeProfit(*symbol, *positionSide, posQty, tpPrice); err != nil {
		fmt.Fprintln(os.Stderr, "place TP:", err)
		os.Exit(1)
	}

	orders, err := ft.GetOpenOrders(*symbol)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify list:", err)
		os.Exit(1)
	}
	fmt.Printf("✓ placed; open orders on %s now: %d\n", *symbol, len(orders))
	for _, o := range orders {
		fmt.Printf("  %s %s trigger=%.6g status=%s\n", o.Type, o.Side, o.StopPrice, o.Status)
	}
}
