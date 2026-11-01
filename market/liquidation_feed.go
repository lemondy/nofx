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
	"nofx/netmeter"
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
// Coverage starts at the first successful connection and excludes detected
// disconnections. The first 24h and any gaps are incomplete observations;
// an empty event window is absent, not proof of zero liquidations.
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
	WindowHours             float64 // actual online hours within the last 24h; zero before first connection
	SpanHours               float64 // hours since first successful connection, capped at 24
	GapCount                int     // detected online-to-offline transitions within the last 24h
	Complete                bool    // full 24h span and online coverage, with no disconnections in that window
	Sample                  string  // largest single liquidation in 24h ("LONG 1.2M @ BTCUSDT")
}

const (
	liqFeedURL      = "wss://fstream.binance.com/ws/!forceOrder@arr"
	liqWindow       = 24 * time.Hour
	liqMaxEvents    = 200_000 // hard memory bound; prune drops oldest first
	liqReconnectMin = 5 * time.Second
	// Every connection after the first is preceded by at least this backoff.
	// This bound retains every 24h interval (including both boundaries) without
	// merging across gaps, which would lose exact rolling-window coverage.
	liqMaxCoverageIntervals = int(liqWindow/liqReconnectMin) + 2
)

type liqOnlineInterval struct {
	start, end time.Time
}

// Closed intervals form a bounded chronological ring. Their end timestamps
// also record disconnections. onlineSince is separate so an ongoing outage
// cannot accrue online time, even after all closed intervals expire.
type liqCoverageState struct {
	intervals   [liqMaxCoverageIntervals]liqOnlineInterval
	head, count int
	onlineSince time.Time
}

var (
	liqMu       sync.Mutex
	liqEvents   []liqEvent
	liqStarted  time.Time // first successful connection, not feed/process startup
	liqRunning  bool
	liqBySymbol = map[string][]liqEvent{}
	liqCoverage liqCoverageState
	liqNow      = time.Now
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
	liqMu.Unlock()
	go liqFeedLoop()
}

func liqFeedLoop() {
	backoff := liqReconnectMin
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
	// dialer ignores the environment, so wire it explicitly. NetDialContext
	// feeds the settings-page traffic monitor (gorilla dials the proxy itself
	// through it, so tunneled TLS bytes are captured at wire level).
	dialer := &websocket.Dialer{Proxy: proxyFromEnv, NetDialContext: netmeter.NetDialContext, HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.Dial(liqFeedURL, nil)
	if err != nil {
		return err
	}
	liqSetConnected(true)
	defer conn.Close()
	defer liqSetConnected(false)
	logger.Infof("📡 liquidation feed connected (!forceOrder@arr)")
	// Refresh the deadline for both data and control traffic. A one-shot
	// deadline disconnected every healthy stream after exactly ten minutes.
	const idleTimeout = 10 * time.Minute
	refreshDeadline := func() error { return conn.SetReadDeadline(liqNow().Add(idleTimeout)) }
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
				if err := conn.WriteControl(websocket.PingMessage, nil, liqNow().Add(5*time.Second)); err != nil {
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
			ts := liqNow()
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

// liqSetConnected records successful connections and their detected ends.
// Failed dial attempts never open an interval or add another disconnection.
func liqSetConnected(connected bool) {
	liqMu.Lock()
	defer liqMu.Unlock()
	now := liqNow()
	liqPruneCoverageLocked(now)
	if connected {
		if !liqCoverage.onlineSince.IsZero() {
			return
		}
		if liqStarted.IsZero() {
			liqStarted = now
		}
		liqCoverage.onlineSince = now
		return
	}
	if liqCoverage.onlineSince.IsZero() {
		return
	}
	i := (liqCoverage.head + liqCoverage.count) % liqMaxCoverageIntervals
	liqCoverage.intervals[i] = liqOnlineInterval{start: liqCoverage.onlineSince, end: now}
	if liqCoverage.count < liqMaxCoverageIntervals {
		liqCoverage.count++
	} else {
		// The minimum reconnect backoff makes this unreachable for retained
		// 24h intervals in the feed, but keep the storage bounded regardless.
		liqCoverage.head = (liqCoverage.head + 1) % liqMaxCoverageIntervals
	}
	liqCoverage.onlineSince = time.Time{}
}

// Caller holds liqMu. Keep an end exactly at cutoff for GapCount even though
// that interval contributes no online duration at the boundary.
func liqPruneCoverageLocked(now time.Time) {
	cutoff := now.Add(-liqWindow)
	for liqCoverage.count > 0 {
		oldest := &liqCoverage.intervals[liqCoverage.head]
		if !oldest.end.Before(cutoff) {
			if oldest.start.Before(cutoff) {
				oldest.start = cutoff
			}
			break
		}
		*oldest = liqOnlineInterval{}
		liqCoverage.head = (liqCoverage.head + 1) % liqMaxCoverageIntervals
		liqCoverage.count--
	}
}

// Caller holds liqMu. Coverage is based on observed socket connectivity;
// failures before their detection and exchange-side omissions are unknown.
func liqCoverageWindowLocked(now time.Time) LiquidationWindow {
	liqPruneCoverageLocked(now)
	var w LiquidationWindow
	if liqStarted.IsZero() || now.Before(liqStarted) {
		return w
	}
	span := now.Sub(liqStarted)
	if span > liqWindow {
		span = liqWindow
	}
	w.SpanHours = span.Hours()
	cutoff := now.Add(-liqWindow)
	var online time.Duration
	addOnline := func(start, end time.Time) {
		if start.Before(cutoff) {
			start = cutoff
		}
		if end.After(now) {
			end = now
		}
		if end.After(start) {
			online += end.Sub(start)
		}
	}
	for n := 0; n < liqCoverage.count; n++ {
		interval := liqCoverage.intervals[(liqCoverage.head+n)%liqMaxCoverageIntervals]
		addOnline(interval.start, interval.end)
		if !interval.end.Before(cutoff) && !interval.end.After(now) {
			w.GapCount++
		}
	}
	if !liqCoverage.onlineSince.IsZero() {
		addOnline(liqCoverage.onlineSince, now)
	}
	w.WindowHours = online.Hours()
	w.Complete = span == liqWindow && online == liqWindow && w.GapCount == 0
	return w
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
	cutoff := liqNow().Add(-liqWindow)
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
// start or empty window): the prompt renders absent, never zero. Coverage
// metadata is returned even when ok=false and excludes detected outages.
func LiquidationStats(symbol string) (LiquidationWindow, bool) {
	StartLiquidationFeed()
	liqMu.Lock()
	defer liqMu.Unlock()
	now := liqNow()
	c1h, c24h := now.Add(-time.Hour), now.Add(-liqWindow)
	w := liqCoverageWindowLocked(now)
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
