package binance_bstock

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"nofx/trader/types"
)

type fifoLot struct {
	qty, cost, fee float64 // cost includes commissions; fee is informational
	when           time.Time
}
type fifoHistory struct {
	nextID int64
	at     time.Time
	lots   []fifoLot
	fills  []*binance.TradeV3
	closed []types.ClosedPnLRecord
}

// Base commissions use this fill's stock price. BNB commissions use BNBUSDT's
// one-minute candle close at the fill time (an explicit approximation, not the
// stock price or today's BNB price). Unknown fee assets fail rather than report 0.
func (t *BStockTrader) feeUSDT(symbol, asset string, fee, p float64, when int64) (float64, error) {
	if fee == 0 {
		return 0, nil
	}
	switch asset {
	case "USDT":
		return fee, nil
	case strings.TrimSuffix(symbol, "USDT"):
		return fee * p, nil
	case "BNB":
		minute := when - when%60000
		bars, err := t.client.NewKlinesService().Symbol("BNBUSDT").Interval("1m").StartTime(minute).EndTime(minute + 59999).Limit(1).Do(context.Background())
		if err != nil {
			return 0, wrap("BNB commission conversion", err)
		}
		if len(bars) != 1 || bars[0].OpenTime != minute || number(bars[0].Close) <= 0 {
			return 0, fmt.Errorf("BNB commission conversion: missing candle at %d", minute)
		}
		return fee * number(bars[0].Close), nil
	default:
		return 0, fmt.Errorf("commission conversion unsupported for asset %s", asset)
	}
}

// consume removes FIFO units and returns their cost and already-paid entry fees.
func (h *fifoHistory) consume(q float64) (matched, cost, fee float64, when time.Time) {
	for q > 1e-12 && len(h.lots) > 0 {
		lot := &h.lots[0]
		if when.IsZero() {
			when = lot.when
		}
		n := min(q, lot.qty)
		fraction := n / lot.qty
		c, f := lot.cost*fraction, lot.fee*fraction
		matched += n
		cost += c
		fee += f
		lot.qty -= n
		lot.cost -= c
		lot.fee -= f
		q -= n
		if lot.qty <= 1e-12 {
			h.lots = h.lots[1:]
		}
	}
	return
}

func (t *BStockTrader) applyFill(symbol string, h *fifoHistory, f *binance.TradeV3) error {
	qty, p, commission := number(f.Quantity), number(f.Price), number(f.Commission)
	if qty <= 0 || p <= 0 || !finite(qty) || !finite(p) || !finite(commission) || commission < 0 {
		return fmt.Errorf("invalid myTrades fill %d", f.ID)
	}
	fee, err := t.feeUSDT(symbol, f.CommissionAsset, commission, p, f.Time)
	if err != nil {
		return err
	}
	baseFee := f.CommissionAsset == strings.TrimSuffix(symbol, "USDT")
	quote := number(f.QuoteQuantity)
	if quote <= 0 {
		quote = qty * p
	}
	when := time.UnixMilli(f.Time).UTC()
	if f.IsBuyer {
		net, cost := qty, quote
		if baseFee {
			net -= commission
		} else {
			cost += fee
		}
		if net <= 0 {
			return fmt.Errorf("base commission consumes entire buy %d", f.ID)
		}
		h.lots = append(h.lots, fifoLot{qty: net, cost: cost, fee: fee, when: when})
		return nil
	}
	matched, cost, entryFee, entryTime := h.consume(qty)
	// Base commission on a sell consumes additional inventory without revenue.
	if baseFee {
		_, extraCost, extraFee, _ := h.consume(commission)
		cost += extraCost
		entryFee += extraFee
	}
	if matched > 0 {
		fraction := matched / qty
		net := quote * fraction
		if !baseFee {
			net -= fee * fraction
		}
		h.closed = append(h.closed, types.ClosedPnLRecord{Symbol: symbol, Side: "long", EntryPrice: cost / matched, ExitPrice: p, Quantity: matched, RealizedPnL: net - cost, Fee: entryFee + fee*fraction, Leverage: 1, EntryTime: entryTime, ExitTime: when, OrderID: id(f.OrderID), ExchangeID: id(f.ID), CloseType: "unknown"})
	}
	return nil
}

// historySnapshot must be called under historyMu. Start at fromId=0, never the
// API's default latest-500 window. Publish only a fully processed refresh.
func (t *BStockTrader) historySnapshot(symbol string) (*fifoHistory, error) {
	old := t.history[symbol]
	if old != nil && !old.at.IsZero() && t.now().Sub(old.at) < cacheTTL {
		return old, nil
	}
	h := &fifoHistory{}
	if old != nil {
		*h = *old
		h.lots = append([]fifoLot(nil), old.lots...)
		h.fills = append([]*binance.TradeV3(nil), old.fills...)
		h.closed = append([]types.ClosedPnLRecord(nil), old.closed...)
	}
	for {
		page, err := t.client.NewListTradesService().Symbol(symbol).FromID(h.nextID).Limit(1000).Do(context.Background())
		if err != nil {
			return nil, wrap("myTrades", err)
		}
		sort.Slice(page, func(i, j int) bool { return page[i].ID < page[j].ID })
		before := h.nextID
		for _, f := range page {
			if f.ID < h.nextID {
				continue
			}
			if err = t.applyFill(symbol, h, f); err != nil {
				return nil, err
			}
			h.fills = append(h.fills, f)
			h.nextID = f.ID + 1
		}
		if len(page) < 1000 {
			break
		}
		if h.nextID == before {
			return nil, fmt.Errorf("myTrades pagination made no progress at fromId=%d", before)
		}
	}
	h.at = t.now()
	t.history[symbol] = h
	return h, nil
}

func (t *BStockTrader) trades(symbol string) ([]*binance.TradeV3, error) {
	t.historyMu.Lock()
	defer t.historyMu.Unlock()
	h, err := t.historySnapshot(symbol)
	if err != nil {
		return nil, err
	}
	return append([]*binance.TradeV3(nil), h.fills...), nil
}

func (t *BStockTrader) CostBasis(symbol string) (avgPrice, quantity float64, err error) {
	if _, err = t.rule(symbol); err != nil {
		return
	}
	t.historyMu.Lock()
	defer t.historyMu.Unlock()
	h, e := t.historySnapshot(symbol)
	if e != nil {
		return 0, 0, e
	}
	cost := 0.0
	for _, lot := range h.lots {
		quantity += lot.qty
		cost += lot.cost
	}
	if quantity > 0 {
		avgPrice = cost / quantity
	}
	return
}

func pnlWindow(records []types.ClosedPnLRecord, start time.Time, limit int) []types.ClosedPnLRecord {
	out := make([]types.ClosedPnLRecord, 0)
	for _, r := range records {
		if !r.ExitTime.Before(start) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExitTime.Equal(out[j].ExitTime) {
			iID, _ := strconv.ParseInt(out[i].ExchangeID, 10, 64)
			jID, _ := strconv.ParseInt(out[j].ExchangeID, 10, 64)
			if iID != jID {
				return iID > jID
			}
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].ExitTime.After(out[j].ExitTime)
	})
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (t *BStockTrader) GetClosedPnLForSymbol(symbol string, start time.Time, limit int) ([]types.ClosedPnLRecord, error) {
	if _, err := t.rule(symbol); err != nil {
		return nil, err
	}
	t.historyMu.Lock()
	defer t.historyMu.Unlock()
	h, err := t.historySnapshot(symbol)
	if err != nil {
		return nil, err
	}
	return pnlWindow(h.closed, start, limit), nil
}

// Spot has no account-wide fills endpoint. Scan all candidate pairs, including
// zero-balance symbols, so fully closed positions are not lost after restart.
func (t *BStockTrader) GetClosedPnL(start time.Time, limit int) ([]types.ClosedPnLRecord, error) {
	rules, err := t.symbols()
	if err != nil {
		return nil, err
	}
	out := make([]types.ClosedPnLRecord, 0)
	for _, symbol := range sortedSymbols(rules) {
		r, e := t.GetClosedPnLForSymbol(symbol, start, limit)
		if e != nil {
			return nil, e
		}
		out = append(out, r...)
	}
	return pnlWindow(out, start, limit), nil
}
