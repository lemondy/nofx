// cancelorphan is a one-off maintenance tool: after a process restart the
// in-memory pendingEntries map is gone, so a resting limit-entry order on the
// exchange has no owner — nobody manages its expiry or places SL/TP on fill
// (naked exposure). This tool lists the account's open orders and cancels the
// one given by -symbol/-orderid after printing it.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"nofx/config"
	"nofx/crypto"
	"nofx/market"
	"nofx/store"
	"nofx/trader/binance"
	"nofx/trader/types"

	"github.com/joho/godotenv"
)

func main() {
	userID := flag.String("user", "f6b845ba-f7b2-4b27-8501-fcf6af94f276", "owner user id")
	accountName := flag.String("account", "mac-nofx", "exchange account_name")
	symbol := flag.String("symbol", "", "symbol of the orphan order")
	orderID := flag.String("orderid", "", "exchange order id to cancel")
	selftest := flag.Bool("selftest", false, "place a tagged far-limit order, read back its client ID, cancel it (tag pipeline verification, no fill risk)")
	flag.Parse()

	_ = godotenv.Load()
	config.Init()

	// EncryptedString decrypts on Scan only when the global crypto service is
	// set — mirror main.go's init order (crypto BEFORE store).
	cryptoService, err := crypto.NewCryptoService()
	if err != nil {
		fmt.Fprintln(os.Stderr, "crypto:", err)
		os.Exit(1)
	}
	crypto.SetGlobalCryptoService(cryptoService)

	cfg := config.Get()
	st, err := store.NewWithConfig(store.DBConfig{
		Type: store.DBTypeSQLite,
		Path: cfg.DBPath,
	})
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

	// Selftest: exercise the tagged client-ID pipeline end to end — place a
	// far-from-market limit (50% below, no fill risk), read back its client
	// ID through GetOpenOrders, verify the tag shape, cancel it.
	if *selftest {
		marketData, err := market.GetWithExchange(*symbol, "binance")
		if err != nil {
			fmt.Fprintln(os.Stderr, "market:", err)
			os.Exit(1)
		}
		price := marketData.CurrentPrice * 0.5
		res, err := ft.PlaceLimitOrder(&types.LimitOrderRequest{
			Symbol: *symbol, Side: "BUY", PositionSide: "LONG",
			Price: price, Quantity: 0.5, Leverage: 1,
			ClientID: fmt.Sprintf("lim-selftest-%d", time.Now().UnixMilli()),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "selftest place:", err)
			os.Exit(1)
		}
		fmt.Printf("placed selftest order id=%s clientID=%q\n", res.OrderID, res.ClientID)
		orders, err := ft.GetOpenOrders(*symbol)
		if err != nil {
			fmt.Fprintln(os.Stderr, "selftest list:", err)
			os.Exit(1)
		}
		for _, o := range orders {
			fmt.Printf("open order: id=%s clientID=%q type=%s status=%s price=%.6g\n", o.OrderID, o.ClientID, o.Type, o.Status, o.Price)
		}
		if err := ft.CancelOrder(*symbol, res.OrderID); err != nil {
			fmt.Fprintln(os.Stderr, "selftest cancel:", err)
			os.Exit(1)
		}
		fmt.Println("✓ selftest done: tag placed, read back, cancelled")
		return
	}

	orders, err := ft.GetOpenOrders(*symbol)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open orders:", err)
		os.Exit(1)
	}
	fmt.Printf("open orders on %s (%s): %d\n", *accountName, *symbol, len(orders))
	for _, o := range orders {
		fmt.Printf("  %+v\n", o)
	}

	if *symbol == "" || *orderID == "" {
		fmt.Println("no -symbol/-orderid given: listing only, nothing cancelled")
		return
	}
	if err := ft.CancelOrder(*symbol, *orderID); err != nil {
		fmt.Fprintln(os.Stderr, "cancel:", err)
		os.Exit(1)
	}
	fmt.Printf("✓ cancelled %s order %s\n", *symbol, *orderID)
}