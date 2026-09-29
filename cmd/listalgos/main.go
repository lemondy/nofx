package main

import (
	"flag"
	"fmt"
	"os"

	"nofx/config"
	"nofx/crypto"
	"nofx/store"
	"nofx/trader/binance"

	"github.com/joho/godotenv"
)

func main() {
	userID := flag.String("user", "f6b845ba-f7b2-4b27-8501-fcf6af94f276", "owner user id")
	accountName := flag.String("account", "mac-nofx", "exchange account name")
	symbol := flag.String("symbol", "ARXUSDT", "symbol")
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
		fmt.Fprintf(os.Stderr, "no binance exchange account %q\n", *accountName)
		os.Exit(1)
	}

	ft := binance.NewFuturesTrader(string(ex.APIKey), string(ex.SecretKey), ex.UserID)
	// ListOpenAlgoOrders is an unexported-client call site; reuse the
	// package's exposed client through the exported wrapper if present,
	// otherwise query via the stop/TP cancel primitives' listing helper.
	orders := ft.DebugListOpenAlgoOrders(*symbol)
	fmt.Printf("open algo orders for %s: %d\n", *symbol, len(orders))
	for _, a := range orders {
		fmt.Printf("ALGO: %s\n", a)
	}
}
