package trader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/store"
	"sync"
)

type accountExecution struct {
	mu      sync.Mutex
	traders map[*AutoTrader]bool
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
		orders, err := at.trader.GetOpenOrders(symbol)
		if err != nil {
			return fmt.Errorf("execution protective orders unknown: %w", err)
		}
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
