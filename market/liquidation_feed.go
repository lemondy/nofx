package market

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"nofx/logger"
)

// ============================================================================
// All-market liquidation feed (user directive 2026-09-25): Binance publishes
// EVERY force-liquidation order on the public !forceOrder@arr stream — no
// key, no auth. This aggregator keeps a 24h rolling window per symbol so the
// AI prompt can carry real liquidation data (long/short notional + count,
// 1h/24h), which CoinAnk previously supplied behind a now-deprecated key.
//
// Semantics: a liquidation of a LONG arrives as a SELL force order; a SHORT
// liquidation arrives as a BUY. Notional = fill price × filled qty (USDT).
//
// Cold start: the window builds from process start — the first 24h after a
// deploy under-reports and the accessors say so (Events24h == 0 with zero
// uptime → absent, not zero).
// ============================================================================

type liqEvent struct {
	ts       time.Time
	longLiq  bool // a LONG position was liquidated
	notional float64
}

type LiquidationWindow struct {
	// LongUSD/ShortUSD: notional liquidated per side over the window.
	Long1hUSD, Short1hUSD   float64
	Long24hUSD, Short24hUSD float64
	Count1h, Count24h       int
	WindowHours             float64 // actual covered window (starts at 0 on cold start)
	Sample                  string  // largest single liquidation in 24h ("LONG 1.2M @ BTCUSDT")
}

const (
	liqFeedURL   = "wss://fstream.binance.com/ws/!forceOrder@arr"
	liqWindow    = 24 * time.Hour
	liqMaxEvents = 200_000 // hard memory bound; prune drops oldest first
)

var (
	liqMu       sync.Mutex
	liqEvents   []liqEvent
	liqStarted  time.Time
	liqRunning  bool
	liqBySymbol = map[string][]liqEvent{}
)

type liqOrder struct {
	Symbol    string `json:"s"`
	Side      string `json:"S"` // SELL = long liquidated
	AvgPrice  string `json:"ap"`
	FilledQty string `json:"z"`
	TradeTime int64  `json:"T"`
}

// Binance /ws endpoints emit raw payloads ({"o": ...}); /stream endpoints
// wrap the same payload under {"stream": ..., "data": ...}. Accept both so
// changing the endpoint cannot silently turn liquidation data into an empty
// feed again.
type liqStreamMsg struct {
	Order liqOrder `json:"o"`
	Data  *struct {
		Order liqOrder `json:"o"`
	} `json:"data,omitempty"`
}

// StartLiquidationFeed launches the all-market force-order stream with
// reconnect/backoff. Idempotent; call lazily from the stats accessor.
func StartLiquidationFeed() {
	liqMu.Lock()
	if liqRunning {
		liqMu.Unlock()
		return
	}
	liqRunning = true
	liqStarted = time.Now()
	liqMu.Unlock()
	go liqFeedLoop()
}

func liqFeedLoop() {
	backoff := 5 * time.Second
	for {
		err := liqConsumeOnce()
		if err == nil {
			err = errLiqClosed
		}
		logger.Warnf("⚠️ liquidation feed disconnected (%v) — reconnecting in %s", err, backoff)
		time.Sleep(backoff)
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

var errLiqClosed = errLiq("stream closed")

type errLiq string

func (e errLiq) Error() string { return string(e) }

func liqConsumeOnce() error {
	// This host reaches Binance through the local proxy (same as every REST
	// call — SafeHTTPClient's ProxyFromEnvironment); the default gorilla
	// dialer ignores the environment, so wire it explicitly.
	dialer := &websocket.Dialer{Proxy: proxyFromEnv, HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.Dial(liqFeedURL, nil)
	if err != nil {
		return err
	}
	logger.Infof("📡 liquidation feed connected (!forceOrder@arr)")
	defer conn.Close()
	// Refresh the deadline for both data and control traffic. A one-shot
	// deadline disconnected every healthy stream after exactly ten minutes.
	const idleTimeout = 10 * time.Minute
	refreshDeadline := func() error { return conn.SetReadDeadline(time.Now().Add(idleTimeout)) }
	if err := refreshDeadline(); err != nil {
		return err
	}
	defaultPingHandler := conn.PingHandler()
	conn.SetPingHandler(func(data string) error {
		if err := refreshDeadline(); err != nil {
			return err
		}
		return defaultPingHandler(data)
	})
	conn.SetPongHandler(func(string) error { return refreshDeadline() })
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(3 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
		}
	}()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if err := refreshDeadline(); err != nil {
			return err
		}
		orders, err := decodeLiquidationOrders(msg)
		if err != nil {
			logger.Debugf("liquidation feed ignored malformed payload: %v", err)
			continue
		}
		for _, order := range orders {
			px := parseFloatSafe(order.AvgPrice)
			qty := parseFloatSafe(order.FilledQty)
			if px <= 0 || qty <= 0 {
				continue
			}
			ts := time.Now()
			if order.TradeTime > 0 {
				ts = time.UnixMilli(order.TradeTime)
			}
			recordLiquidation(order.Symbol, liqEvent{
				ts:       ts,
				longLiq:  order.Side == "SELL",
				notional: px * qty,
			})
		}
	}
}

func decodeLiquidationOrders(msg []byte) ([]liqOrder, error) {
	decode := func(m liqStreamMsg) liqOrder {
		if m.Order.Symbol != "" {
			return m.Order
		}
		if m.Data != nil {
			return m.Data.Order
		}
		return liqOrder{}
	}

	var one liqStreamMsg
	if err := json.Unmarshal(msg, &one); err == nil {
		if order := decode(one); order.Symbol != "" {
			return []liqOrder{order}, nil
		}
	}

	// Be tolerant of providers/proxies that batch all-market events as an
	// array even though Binance currently emits one force-order event at a
	// time.
	var batch []liqStreamMsg
	if err := json.Unmarshal(msg, &batch); err != nil {
		return nil, err
	}
	orders := make([]liqOrder, 0, len(batch))
	for _, m := range batch {
		if order := decode(m); order.Symbol != "" {
			orders = append(orders, order)
		}
	}
	if len(orders) == 0 {
		return nil, fmt.Errorf("payload contains no liquidation orders")
	}
	return orders, nil
}

var proxyFromEnv = proxyFromEnvironment()

func proxyFromEnvironment() func(*http.Request) (*url.URL, error) {
	return http.ProxyFromEnvironment
}

func parseFloatSafe(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

func recordLiquidation(symbol string, ev liqEvent) {
	liqMu.Lock()
	defer liqMu.Unlock()
	cutoff := time.Now().Add(-liqWindow)
	// Global prune + per-symbol append, bounded.
	kept := liqEvents[:0]
	for _, e := range liqEvents {
		if e.ts.After(cutoff) {
			kept = append(kept, e)
		}
	}
	liqEvents = kept
	if len(liqEvents) < liqMaxEvents {
		liqEvents = append(liqEvents, ev)
	}
	key := strings.ToUpper(symbol)
	keptS := liqBySymbol[key][:0]
	for _, e := range liqBySymbol[key] {
		if e.ts.After(cutoff) {
			keptS = append(keptS, e)
		}
	}
	liqBySymbol[key] = append(keptS, ev)
}

// LiquidationStats returns the 24h rolling liquidation picture for one
// symbol and starts the feed on first call. ok=false → no data yet (cold
// start or empty window): the prompt renders absent, never zero.
func LiquidationStats(symbol string) (LiquidationWindow, bool) {
	StartLiquidationFeed()
	liqMu.Lock()
	defer liqMu.Unlock()
	now := time.Now()
	c1h, c24h := now.Add(-time.Hour), now.Add(-liqWindow)
	var w LiquidationWindow
	w.WindowHours = now.Sub(liqStarted).Hours()
	if w.WindowHours > 24 {
		w.WindowHours = 24
	}
	var top float64
	for _, e := range liqBySymbol[strings.ToUpper(symbol)] {
		if e.ts.Before(c24h) {
			continue
		}
		w.Count24h++
		if e.longLiq {
			w.Long24hUSD += e.notional
		} else {
			w.Short24hUSD += e.notional
		}
		if e.ts.After(c1h) {
			w.Count1h++
			if e.longLiq {
				w.Long1hUSD += e.notional
			} else {
				w.Short1hUSD += e.notional
			}
		}
		if e.notional > top {
			top = e.notional
			side := "SHORT"
			if e.longLiq {
				side = "LONG"
			}
			w.Sample = side + " " + humanUSD(e.notional)
		}
	}
	if w.Count24h == 0 {
		return w, false
	}
	return w, true
}

func humanUSD(v float64) string {
	switch {
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 2, 64) + "M"
	case v >= 1e3:
		return strconv.FormatFloat(v/1e3, 'f', 1, 64) + "K"
	default:
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
}
