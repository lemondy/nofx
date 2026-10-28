package trader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/store"
	"nofx/trader/binance"
	"nofx/trader/types"
	"strings"
	"sync"
	"time"
)

// openOrdersCache is the TTL-bounded all-symbol open-order snapshot shared by
// every strategy on one exchange account.
type openOrdersCache struct {
	mu     sync.Mutex
	at     time.Time
	orders []types.OpenOrder
	err    error
}

type accountExecution struct {
	mu      sync.Mutex
	traders map[*AutoTrader]bool
	open    openOrdersCache
}

// Match the Binance execution account cache (futures.go: cacheDuration).
const executionOpenOrdersTTL = 15 * time.Second

// Traders without an account key (pure unit-test fixtures) cache per trader.
// Keyed by pointer and retained, so a recycled address can never inherit a
// stale snapshot.
var unkeyedOpenOrders = struct {
	sync.Mutex
	caches map[*AutoTrader]*openOrdersCache
}{caches: map[*AutoTrader]*openOrdersCache{}}

func (at *AutoTrader) openOrdersCache() *openOrdersCache {
	if key := at.executionAccountKey(); key != "" {
		accountRegistry.Lock()
		defer accountRegistry.Unlock()
		account := accountRegistry.accounts[key]
		if account == nil {
			account = &accountExecution{traders: map[*AutoTrader]bool{}}
			accountRegistry.accounts[key] = account
		}
		return &account.open
	}
	unkeyedOpenOrders.Lock()
	defer unkeyedOpenOrders.Unlock()
	c := unkeyedOpenOrders.caches[at]
	if c == nil {
		c = &openOrdersCache{}
		unkeyedOpenOrders.caches[at] = c
	}
	return c
}

// executionOpenOrders returns the account-wide open-order snapshot used to
// reserve exposure for orders this process did not register (the user's manual
// limit orders on the same Binance account). One GetOpenOrders("") call per TTL
// per account: the Binance adapter issues it through the bnweight-wrapped
// execution client (Critical priority; all-symbol openOrders costs 40 weight
// plus the algo list). Failures are cached for the TTL too, and a previous
// successful snapshot is never reused after an error.
func (at *AutoTrader) executionOpenOrders() ([]types.OpenOrder, error) {
	return at.executionOpenOrdersFresh(false)
}

// executionOpenOrdersFresh with force bypasses the TTL: the execution refresh
// (once per decision batch / final open check) re-reads the account like it
// does balance and positions, and the admission gates that follow reuse it.
func (at *AutoTrader) executionOpenOrdersFresh(force bool) ([]types.OpenOrder, error) {
	c := at.openOrdersCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && !c.at.IsZero() && time.Since(c.at) < executionOpenOrdersTTL {
		return c.orders, c.err
	}
	orders, err := at.trader.GetOpenOrders("")
	c.orders, c.err = nil, err
	if err == nil {
		c.orders = append([]types.OpenOrder(nil), orders...)
	}
	c.at = time.Now()
	return c.orders, c.err
}

func reservationIdentity(pe *pendingEntry, ids, clients map[string]bool) {
	if pe == nil {
		return
	}
	if isUnknownEntryRef(pe.OrderID) {
		clients[pe.Symbol+"|"+strings.TrimPrefix(pe.OrderID, binance.ClientOrderRefPrefix)] = true
	} else if pe.OrderID != "" {
		ids[pe.Symbol+"|"+pe.OrderID] = true
	}
	if pe.ClientOrderID != "" {
		clients[pe.Symbol+"|"+pe.ClientOrderID] = true
	}
}

// Admission-only reservations: NEVER merge these into accountPendingEntries,
// whose callers also cancel owned entries during a breaker. Manual orders must
// remain hands-off. Full original quantity is conservative for partial external
// fills because the adapter does not expose open-order executedQty.
func (at *AutoTrader) externalEntryReservations(local map[string]*pendingEntry) map[string]*pendingEntry {
	out := map[string]*pendingEntry{}
	if at.trader == nil {
		return out
	} // no executable account (pure sizing callers)
	orders, err := at.executionOpenOrders()
	if err != nil {
		out["unknown"] = nil
		return out
	}
	ids, clients := map[string]bool{}, map[string]bool{}
	for _, pe := range local {
		reservationIdentity(pe, ids, clients)
	}
	leverage := map[string]int{}
	positionsRead := false
	for _, o := range orders {
		if o.ReduceOnly || o.ClosePosition || o.Algo || o.StopPrice > 0 || strings.ToUpper(o.Type) != "LIMIT" {
			continue
		}
		if o.Status != "" && !strings.EqualFold(o.Status, "NEW") && !strings.EqualFold(o.Status, "PARTIALLY_FILLED") {
			continue
		}
		side := ""
		switch strings.ToUpper(o.PositionSide) {
		case "LONG":
			if strings.EqualFold(o.Side, "BUY") {
				side = "long"
			}
		case "SHORT":
			if strings.EqualFold(o.Side, "SELL") {
				side = "short"
			}
		case "", "BOTH":
			// Without reduce-only, a one-way order can add/reverse exposure.
			if strings.EqualFold(o.Side, "BUY") {
				side = "long"
			} else if strings.EqualFold(o.Side, "SELL") {
				side = "short"
			}
		}
		if side == "" {
			continue
		}
		if (o.OrderID != "" && ids[o.Symbol+"|"+o.OrderID]) || (o.ClientID != "" && clients[o.Symbol+"|"+o.ClientID]) {
			continue
		}
		if o.Symbol == "" || o.Price <= 0 || o.Quantity <= 0 || math.IsNaN(o.Price*o.Quantity) || math.IsInf(o.Price*o.Quantity, 0) {
			out["unknown"] = nil
			continue
		}
		if !positionsRead {
			positionsRead = true
			// GetPositions reports the current symbol leverage for held symbols;
			// absent/unknown leverage is charged at 1x, never the new entry's leverage.
			if positions, err := at.trader.GetPositions(); err == nil {
				for _, p := range positions {
					s, _ := p["symbol"].(string)
					lev, _ := p["leverage"].(float64)
					if lev >= 1 && !math.IsNaN(lev) && !math.IsInf(lev, 0) {
						leverage[s] = int(lev)
					}
				}
			}
		}
		lev := leverage[o.Symbol]
		if lev < 1 {
			lev = 1
		}
		// No external SL plan is known. StopLoss=0 charges the existing
		// kernel.UnprotectedStopWorstCasePct of notional (no parameter change).
		out["external:"+o.Symbol+"|"+o.OrderID+"|"+o.ClientID] = &pendingEntry{Symbol: o.Symbol, Side: side, Price: o.Price, Quantity: o.Quantity, Leverage: lev, OrderID: o.OrderID, ClientOrderID: o.ClientID}
		reservationIdentity(out["external:"+o.Symbol+"|"+o.OrderID+"|"+o.ClientID], ids, clients)
	}
	return out
}

var accountRegistry = struct {
	sync.Mutex
	accounts map[string]*accountExecution
}{accounts: map[string]*accountExecution{}}

func (at *AutoTrader) executionAccountKey() string {
	key := at.exchangeID
	if key == "" {
		key = at.config.ExchangeID
	}
	if key == "" {
		return ""
	}
	return at.userID + ":" + key
}
func registerAccountTrader(at *AutoTrader) *accountExecution {
	key := at.executionAccountKey()
	if key == "" {
		return nil
	}
	accountRegistry.Lock()
	defer accountRegistry.Unlock()
	account := accountRegistry.accounts[key]
	if account == nil {
		account = &accountExecution{traders: map[*AutoTrader]bool{}}
		accountRegistry.accounts[key] = account
	}
	account.traders[at] = true
	return account
}
func unregisterAccountTrader(at *AutoTrader) {
	accountRegistry.Lock()
	defer accountRegistry.Unlock()
	if account := accountRegistry.accounts[at.executionAccountKey()]; account != nil {
		delete(account.traders, at)
	}
}
func (at *AutoTrader) executionMutex() *sync.Mutex {
	if account := registerAccountTrader(at); account != nil {
		return &account.mu
	}
	return &at.executionStateMu
}

func (at *AutoTrader) accountPeers() []*AutoTrader {
	peers := []*AutoTrader{at}
	key := at.executionAccountKey()
	if key == "" {
		return peers
	}
	accountRegistry.Lock()
	defer accountRegistry.Unlock()
	if account := accountRegistry.accounts[key]; account != nil {
		peers = nil
		for peer := range account.traders {
			peers = append(peers, peer)
		}
	}
	return peers
}

// The caller owns executionMutex; pending map membership has its own mutex.
func (at *AutoTrader) accountPendingEntries() map[string]*pendingEntry {
	peers := []*AutoTrader{at}
	key := at.executionAccountKey()
	if key != "" {
		accountRegistry.Lock()
		if account := accountRegistry.accounts[key]; account != nil {
			peers = nil
			for peer := range account.traders {
				peers = append(peers, peer)
			}
		}
		accountRegistry.Unlock()
	}
	out := map[string]*pendingEntry{}
	seen := map[string]bool{}
	for _, peer := range peers {
		peer.pendingEntriesMu.RLock()
		for key, pe := range peer.pendingEntries {
			if pe == nil {
				continue
			}
			id := pe.Symbol + "|" + pe.OrderID
			if pe.OrderID != "" && seen[id] {
				continue
			}
			seen[id] = true
			entryKey := key
			if peer != at {
				entryKey = fmt.Sprintf("%p|%s", peer, key)
			}
			copy := *pe
			out[entryKey] = &copy
		}
		peer.pendingEntriesMu.RUnlock()
		if peer.gridState != nil && peer.config.StrategyConfig != nil && peer.config.StrategyConfig.GridConfig != nil {
			peer.gridState.mu.RLock()
			levels := append([]kernel.GridLevelInfo(nil), peer.gridState.Levels...)
			peer.gridState.mu.RUnlock()
			addGridReservations(out, seen, peer.id, peer.config.StrategyConfig.GridConfig, levels)
		}
	}
	if at.store != nil && at.executionAccountKey() != "" {
		exchangeID := at.exchangeID
		if exchangeID == "" {
			exchangeID = at.config.ExchangeID
		}
		if rows, err := at.store.PendingEntry().ListForAccount(exchangeID); err == nil {
			for _, row := range rows {
				id := row.Symbol + "|" + row.OrderID
				if seen[id] {
					continue
				}
				seen[id] = true
				pe := pendingEntryFromRow(row)
				pe.ProtectedQty = row.ProtectedQty
				key := pendingEntryKey(row.Symbol, row.Side)
				if row.TraderID != at.id {
					key = "stored:" + row.TraderID + "|" + key
				}
				out[key] = &pe
			}
		} else {
			// A missing durable reservation snapshot must prevent new risk.
			out["unknown"] = nil
		}
		rows, err := at.store.PendingEntry().ListGridForAccount(at.executionAccountKey())
		if err != nil {
			out["unknown"] = nil
		} else {
			for _, row := range rows {
				var state struct {
					Config *store.GridStrategyConfig
					Levels []kernel.GridLevelInfo
				}
				if err := json.Unmarshal([]byte(row.StateJSON), &state); err != nil || state.Config == nil {
					out["unknown"] = nil
					continue
				}
				addGridReservations(out, seen, row.TraderID, state.Config, state.Levels)
			}
		}
	}

	return out
}

// Re-read account exposure after AI latency, under the shared account lock.
func (at *AutoTrader) refreshExecutionAccount(ctx *kernel.Context) error {
	if at.trader == nil {
		return nil
	}
	if cache, ok := at.trader.(interface{ InvalidateAccountCache() }); ok {
		cache.InvalidateAccountCache()
	}
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("execution balance unknown: %w", err)
	}
	equity := gridAccountEquity(balance)
	if equity <= 0 || math.IsNaN(equity) || math.IsInf(equity, 0) {
		return fmt.Errorf("execution equity unknown")
	}
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("execution positions unknown: %w", err)
	}
	previous := map[string]kernel.PositionInfo{}
	for _, p := range ctx.Positions {
		previous[p.Symbol+"|"+p.Side] = p
	}
	fresh := []kernel.PositionInfo{}
	margin := 0.0
	// P2-7: one GetOpenOrders per unique SYMBOL up front — hedge mode puts
	// LONG+SHORT rows of the same symbol in this loop and the per-row call
	// doubled the request on exactly those. A failed fetch fails the pass
	// (fail-closed) exactly as the per-row call did.
	openOrdersBySymbol := make(map[string][]types.OpenOrder, len(positions))
	// One forced all-symbol read replaces the per-symbol reads and doubles as
	// the manual-order snapshot the admission gates reuse. If it fails the
	// per-symbol path below still runs, so reduce/close handling is never
	// blocked by the extra read; the cached failure keeps new opens closed.
	var allOrders []types.OpenOrder
	allKnown := false
	if len(positions) > 0 {
		var allErr error
		allOrders, allErr = at.executionOpenOrdersFresh(true)
		allKnown = allErr == nil
	}
	for _, p := range positions {
		symbol, _ := p["symbol"].(string)
		if symbol == "" {
			continue
		}
		if _, done := openOrdersBySymbol[symbol]; done {
			continue
		}
		if allKnown {
			orders := make([]types.OpenOrder, 0, len(allOrders))
			for _, o := range allOrders {
				if o.Symbol == "" || o.Symbol == symbol {
					orders = append(orders, o)
				}
			}
			openOrdersBySymbol[symbol] = orders
			continue
		}
		orders, err := at.trader.GetOpenOrders(symbol)
		if err != nil {
			return fmt.Errorf("execution protective orders unknown: %w", err)
		}
		openOrdersBySymbol[symbol] = orders
	}
	for _, p := range positions {
		symbol, _ := p["symbol"].(string)
		side, _ := p["side"].(string)
		qty, validQty := p["positionAmt"].(float64)
		if !validQty || math.IsNaN(qty) || math.IsInf(qty, 0) {
			return fmt.Errorf("execution position quantity unknown")
		}
		entry, _ := p["entryPrice"].(float64)
		if qty == 0 {
			continue
		}
		if symbol == "" || (side != "long" && side != "short") {
			return fmt.Errorf("execution position identity unknown")
		}
		row := previous[symbol+"|"+side]
		row.Symbol = symbol
		row.Side = side
		row.Quantity = math.Abs(qty)
		row.EntryPrice = entry
		mark, _ := p["markPrice"].(float64)
		if mark > 0 {
			row.MarkPrice = mark
		}
		// Exposure uses the current exchange protection, never the prompt's old stop.
		orders := openOrdersBySymbol[symbol]
		stop := protectionPrice(orders, side, "SL")
		if !enoughProtection(orders, side, "SL", stop, row.Quantity) {
			stop = 0
		}
		row.StopLossPrice = stop
		row.TakeProfitPrice = protectionPrice(orders, side, "TP")
		if entry <= 0 || math.IsNaN(entry) || math.IsInf(entry, 0) {
			return fmt.Errorf("execution position price unknown")
		}
		leverage, _ := p["leverage"].(float64)
		if leverage <= 0 || math.IsNaN(leverage) || math.IsInf(leverage, 0) {
			return fmt.Errorf("execution leverage unknown")
		}
		row.Leverage = int(leverage)
		row.MarginUsed = math.Abs(qty) * entry / leverage
		margin += row.MarginUsed
		fresh = append(fresh, row)
	}
	ctx.Positions = fresh
	ctx.Account.TotalEquity = equity
	ctx.Account.MarginUsed = margin
	ctx.Account.PositionCount = len(fresh)
	ctx.Account.AvailableBalance = math.Max(0, equity-margin)
	if available, ok := balance["availableBalance"].(float64); ok {
		ctx.Account.AvailableBalance = available
	}
	return nil
}

// Reacquire even on panic: callers defer Unlock for the surrounding transaction.
func withoutExecutionLock[T any](mu *sync.Mutex, call func() (T, error)) (T, error) {
	mu.Unlock()
	defer mu.Lock()
	return call()
}

func addGridReservations(out map[string]*pendingEntry, seen map[string]bool, traderID string, cfg *store.GridStrategyConfig, levels []kernel.GridLevelInfo) {
	for _, level := range levels {
		if level.State != "pending" || level.OrderID == "" {
			continue
		}
		id := cfg.Symbol + "|" + level.OrderID
		if seen[id] {
			continue
		}
		seen[id] = true
		side := "long"
		stop := level.Price * (1 - cfg.StopLossPct/100)
		if level.Side == "sell" {
			side = "short"
			stop = level.Price * (1 + cfg.StopLossPct/100)
		}
		if cfg.StopLossPct <= 0 {
			stop = 0
		}
		out["grid:"+traderID+"|"+level.OrderID] = &pendingEntry{Symbol: cfg.Symbol, Side: side, Price: level.Price, StopLoss: stop, Quantity: level.OrderQuantity, ProtectedQty: level.ExecutedQuantity, Leverage: cfg.Leverage, OrderID: level.OrderID}
	}
}

func withExecutionLock[T any](mu *sync.Mutex, call func() (T, error)) (T, error) {
	mu.Lock()
	defer mu.Unlock()
	return call()
}
