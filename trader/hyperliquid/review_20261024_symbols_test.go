package hyperliquid

import (
	"context"
	hl "github.com/sonirico/go-hyperliquid"
	"nofx/trader/types"
	"testing"
)

func TestReviewC1FrontendOrdersPreserveKPrefix(t *testing.T) {
	got, err := normalizeFrontendOrders("KPEPEUSDT", []hl.FrontendOpenOrder{
		{Coin: "kPEPE", Oid: 1, Sz: 2}, {Coin: "PEPE", Oid: 2}, {Coin: "1000PEPE", Oid: 3},
	})
	if err != nil || len(got) != 1 || got[0].OrderID != "1" {
		t.Fatalf("kPEPE orders lost/misidentified: %v, %v", got, err)
	}
}

func TestReviewC1SDKAssetIDsValidatedWithoutTrading(t *testing.T) {
	meta := &hl.Meta{Universe: []hl.AssetInfo{{Name: "BTC"}, {Name: "DEAD", IsDelisted: true}, {Name: "kPEPE"}}}
	// Supplying both metadata objects avoids constructor network requests.
	exchange := hl.NewExchange(context.Background(), nil, "https://unused.invalid", meta, "", "", &hl.SpotMeta{})
	tr := &HyperliquidTrader{exchange: exchange, meta: meta}
	for _, tc := range []struct {
		symbol, raw string
		id          int
	}{{"BTCUSDT", "BTC", 0}, {"KPEPEUSDT", "kPEPE", 2}, {"DEADUSDT", "DEAD", 1}} {
		coin, err := tr.resolveCoin(tc.symbol)
		if err != nil || coin != tc.raw || exchange.Info().NameToAsset(coin) != tc.id {
			t.Fatalf("%s resolved %q: %v", tc.symbol, coin, err)
		}
	}
	if _, err := tr.resolveCoin("PEPEUSDT"); err == nil {
		t.Fatal("unknown alias reached SDK asset zero")
	}
}

func TestReviewC1TraderResolvesMetadataIncludingDelisted(t *testing.T) {
	tr := &HyperliquidTrader{meta: &hl.Meta{Universe: []hl.AssetInfo{
		{Name: "BTC"}, {Name: "kPEPE", SzDecimals: 0, IsDelisted: true},
	}}}
	if coin, err := tr.resolveCoin(" KPEPEUSDT "); err != nil || coin != "kPEPE" {
		t.Fatalf("coin=%q err=%v", coin, err)
	}
	if quantity, err := tr.FormatQuantity("KPEPEUSDT", 123); err != nil || quantity != "123" {
		t.Fatalf("quantity=%q err=%v", quantity, err)
	}
	// No exchange is installed: reaching any SDK request would panic. Every
	// adapter entry point must reject unlisted aliases locally first.
	for _, symbol := range []string{"PEPEUSDT", "1000PEPEUSDT", "UNKNOWN"} {
		for name, call := range map[string]func() error{
			"price":        func() error { _, e := tr.GetMarketPrice(symbol); return e },
			"quantity":     func() error { _, e := tr.FormatQuantity(symbol, 1); return e },
			"open long":    func() error { _, e := tr.OpenLong(symbol, 1, 1); return e },
			"open short":   func() error { _, e := tr.OpenShort(symbol, 1, 1); return e },
			"close long":   func() error { _, e := tr.CloseLong(symbol, 1); return e },
			"close short":  func() error { _, e := tr.CloseShort(symbol, 1); return e },
			"leverage":     func() error { return tr.SetLeverage(symbol, 1) },
			"stop":         func() error { return tr.SetStopLoss(symbol, "LONG", 1, 1) },
			"take profit":  func() error { return tr.SetTakeProfit(symbol, "LONG", 1, 1) },
			"cancel all":   func() error { return tr.CancelAllOrders(symbol) },
			"cancel stops": func() error { return tr.CancelStopOrders(symbol) },
			"cancel":       func() error { return tr.CancelOrder(symbol, "1") },
			"limit":        func() error { _, e := tr.PlaceLimitOrder(&types.LimitOrderRequest{Symbol: symbol}); return e },
			"status":       func() error { _, e := tr.GetOrderStatus(symbol, "1"); return e },
			"orders":       func() error { _, e := tr.GetOpenOrders(symbol); return e },
			"book":         func() error { _, _, e := tr.GetOrderBook(symbol, 5); return e },
		} {
			if err := call(); err == nil {
				t.Fatalf("%s accepted %s", name, symbol)
			}
		}
	}
}
