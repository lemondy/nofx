package trader

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/kernel/stockengine"
	"nofx/market/usstock"
)

func entryAlerts(fx *stockFixture) []string {
	var out []string
	for _, a := range fx.alert {
		if strings.HasPrefix(a, "ALERT|") && strings.Contains(a, "美股入场已放弃") {
			out = append(out, a)
		}
	}
	return out
}

// A live entry abandoned by the final pre-order check must reach Telegram.
func TestStockEntryRejectionSendsTelegramAlert(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(*usstock.Quote)
	}{
		{"stale bstock quote", "超过 30 秒", func(q *usstock.Quote) { q.BStockTime = stockTestNow.Add(-31 * time.Second) }},
		{"divergence recheck", "下单前复核未通过", func(q *usstock.Quote) { q.BStockPrice = 123; q.RefPrice = 120 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newStockFixture(t, false, stockTestNow, func(int) string { return openAAPL })
			oldQuote := stockGetQuote
			fx.ai.respond = func(int) string {
				stockGetQuote = func(ctx context.Context, sym string) (*usstock.Quote, error) {
					q, e := oldQuote(ctx, sym)
					tc.mutate(q)
					return q, e
				}
				return openAAPL
			}
			fx.cycle(t)
			alerts := entryAlerts(fx)
			if len(fx.spot.buys) != 0 || len(alerts) != 1 ||
				!strings.Contains(alerts[0], "AAPLBUSDT") || !strings.Contains(alerts[0], tc.reason) {
				t.Fatalf("buys=%v alerts=%q", fx.spot.buys, alerts)
			}
		})
	}
}

func TestStockEntryAlertDedupePaperAndCancel(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return "{}" })
	fx.alert = nil
	d := stockengine.Decision{Symbol: "AAPLBUSDT", Action: stockengine.ActionOpenLong}
	stale := fmt.Errorf("BSTOCK_PRICE_STALE: execution quote age 41s")
	at := fx.at
	at.stockAlertEntryRejected(false, d, stale, stockTestNow)
	at.stockAlertEntryRejected(false, d, stale, stockTestNow.Add(10*time.Minute)) // same reason, inside window
	if n := len(entryAlerts(fx)); n != 1 {
		t.Fatalf("same symbol+reason inside the window must alert once, got %d", n)
	}
	at.stockAlertEntryRejected(false, d, fmt.Errorf("ENTRY_QUOTE_UNAVAILABLE: timeout"), stockTestNow.Add(11*time.Minute))                                    // other reason
	at.stockAlertEntryRejected(false, stockengine.Decision{Symbol: "MSFTBUSDT", Action: stockengine.ActionOpenLong}, stale, stockTestNow.Add(11*time.Minute)) // other symbol
	if n := len(entryAlerts(fx)); n != 3 {
		t.Fatalf("other reason / other symbol must each alert, got %d", n)
	}
	at.stockAlertEntryRejected(false, d, stale, stockTestNow.Add(31*time.Minute)) // window elapsed
	if n := len(entryAlerts(fx)); n != 4 {
		t.Fatalf("alert must repeat after the window, got %d", n)
	}
	before := len(entryAlerts(fx))
	at.stockAlertEntryRejected(true, d, fmt.Errorf("ENTRY_QUOTE_UNAVAILABLE: x"), stockTestNow.Add(2*time.Hour))       // paper: no real order, no alert
	at.stockAlertEntryRejected(false, d, context.Canceled, stockTestNow.Add(2*time.Hour))                              // shutdown, not a rejection
	at.stockAlertEntryRejected(false, d, fmt.Errorf("x: %w", context.DeadlineExceeded), stockTestNow.Add(2*time.Hour)) // timeout of the whole cycle
	at.stockAlertEntryRejected(false, d, nil, stockTestNow.Add(2*time.Hour))
	if len(entryAlerts(fx)) != before {
		t.Fatalf("paper / cancelled / nil must not alert: %q", entryAlerts(fx))
	}
}
