package trader

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"nofx/logger"
	"nofx/store"
	"nofx/trader/paper"
)

// factor_quant (design 2026-11-01): execution. A broker hides the venue; both
// implementations are PAPER-only — the crypto one drives the paper futures
// simulator, the US one books the bStock paper ledger. Neither can reach a real
// order endpoint.

// fqAccount is the account state a rebalance plans against.
type fqAccount struct {
	Equity     float64
	Balance    float64 // wallet / cash
	Unrealized float64
	Exposure   float64 // Σ |notional|
	Positions  []fqPosition
}

// fqResult is the outcome of one planned order.
type fqResult struct {
	Order      fqOrder
	OK         bool
	Err        string
	FillQty    float64
	FillPrice  float64
	OrderID    string
	Fee        float64 // trading fee paid (USDT), paper-ledger fills
	StopPrice  float64 // stop level checked after the order (0 = none)
	StopExited bool    // protection closed an already breached position at market
	StopErr    string
	Note       string
}

// failed reports whether the order or its protection did not complete.
func (r fqResult) failed() bool { return !r.OK || r.StopErr != "" }

// fqBroker is the venue seam.
type fqBroker interface {
	Account() (*fqAccount, error)
	Price(symbol string) (float64, error)
	// FormatQty rounds to the venue lot; 0 means below the venue minimum.
	FormatQty(symbol string, qty, price float64) (float64, error)
	Place(o fqOrder) fqResult
}

// ------------------------------------------------------------ crypto: paper futures

// fqFuturesTrader is the slice of types.Trader the crypto broker needs, so
// tests can substitute a fake.
type fqFuturesTrader interface {
	GetBalance() (map[string]interface{}, error)
	GetPositions() ([]map[string]interface{}, error)
	OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error)
	OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error)
	CloseLong(symbol string, quantity float64) (map[string]interface{}, error)
	CloseShort(symbol string, quantity float64) (map[string]interface{}, error)
	SetLeverage(symbol string, leverage int) error
	GetMarketPrice(symbol string) (float64, error)
	SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error
	CancelStopLossOrdersForSide(symbol, positionSide string) error
	FormatQuantity(symbol string, quantity float64) (string, error)
}

type fqFuturesBroker struct {
	t            fqFuturesTrader
	leverage     int
	longStopPct  float64 // 0 = no stop
	shortStopPct float64 // 0 = no stop
	levSet       map[string]bool
}

func newFQFuturesBroker(t fqFuturesTrader, leverage int, longStopPct, shortStopPct float64) *fqFuturesBroker {
	return &fqFuturesBroker{t: t, leverage: leverage, longStopPct: longStopPct, shortStopPct: shortStopPct, levSet: map[string]bool{}}
}

func fqFloat(m map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v, err := SafeFloat64(m, k); err == nil {
			return v
		}
	}
	return 0
}

func (b *fqFuturesBroker) Account() (*fqAccount, error) {
	bal, err := b.t.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("读取模拟盘余额失败：%w", err)
	}
	rows, err := b.t.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("读取模拟盘持仓失败：%w", err)
	}
	acct := &fqAccount{
		Balance:    fqFloat(bal, "totalWalletBalance", "wallet_balance"),
		Unrealized: fqFloat(bal, "totalUnrealizedProfit"),
		Equity:     gridAccountEquity(bal),
	}
	if acct.Equity <= 0 {
		acct.Equity = acct.Balance + acct.Unrealized
	}
	for _, row := range rows {
		sym, _ := row["symbol"].(string)
		amt := fqFloat(row, "positionAmt")
		qty := math.Abs(amt)
		if sym == "" || qty <= 0 {
			continue
		}
		side, _ := row["side"].(string)
		switch strings.ToLower(side) {
		case fqSideLong, fqSideShort:
			side = strings.ToLower(side)
		default:
			side = fqSideLong
			if amt < 0 {
				side = fqSideShort
			}
		}
		mark := fqFloat(row, "markPrice")
		acct.Positions = append(acct.Positions, fqPosition{Symbol: sym, Side: side, Qty: qty, Entry: fqFloat(row, "entryPrice"), Mark: mark})
		acct.Exposure += qty * mark
	}
	sort.Slice(acct.Positions, func(i, j int) bool {
		if acct.Positions[i].Symbol != acct.Positions[j].Symbol {
			return acct.Positions[i].Symbol < acct.Positions[j].Symbol
		}
		return acct.Positions[i].Side < acct.Positions[j].Side
	})
	return acct, nil
}

func (b *fqFuturesBroker) Price(symbol string) (float64, error) { return b.t.GetMarketPrice(symbol) }

func (b *fqFuturesBroker) FormatQty(symbol string, qty, _ float64) (float64, error) {
	text, err := b.t.FormatQuantity(symbol, qty)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析取整后的数量 %q", text)
	}
	return v, nil
}

func (b *fqFuturesBroker) Place(o fqOrder) fqResult {
	res := fqResult{Order: o}
	qty := o.Qty
	if o.CloseAll {
		qty = 0 // "close everything"; avoids a lot-size dust remainder
	}
	var raw map[string]interface{}
	var err error
	switch o.Action() {
	case "open_long", "open_short":
		if !b.levSet[o.Symbol] {
			if err = b.t.SetLeverage(o.Symbol, b.leverage); err != nil {
				res.Err = fmt.Sprintf("设置杠杆 %dx 失败：%v", b.leverage, err)
				return res
			}
			b.levSet[o.Symbol] = true
		}
		if o.Side == fqSideLong {
			raw, err = b.t.OpenLong(o.Symbol, qty, b.leverage)
		} else {
			raw, err = b.t.OpenShort(o.Symbol, qty, b.leverage)
		}
	case "close_long":
		raw, err = b.t.CloseLong(o.Symbol, qty)
	case "close_short":
		raw, err = b.t.CloseShort(o.Symbol, qty)
	default:
		res.Err = "未知动作 " + o.Action()
		return res
	}
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.OK = true
	res.FillQty = fqFloat(raw, "executedQty")
	res.FillPrice = fqFloat(raw, "avgPrice")
	if id, ok := raw["orderId"]; ok && id != nil {
		res.OrderID = fmt.Sprint(id)
	}
	// Paper opens/increases cancel protection; reductions retain it. Refresh
	// both sides from the remaining full quantity and average entry after any
	// fill. A full close already cancels that side's protection in paper.execute.
	stop := b.placeStop(o.Symbol, o.Side)
	res.StopPrice, res.StopErr, res.StopExited = stop.Price, stop.Err, stop.Exited
	res.Note = stop.Note
	return res
}

type fqStopResult struct {
	Symbol string
	Side   string
	Price  float64
	Exited bool
	Note   string
	Err    string
}

func fqStopSideLabel(side string) string {
	if side == fqSideLong {
		return "多"
	}
	return "空"
}

func (b *fqFuturesBroker) placeStop(sym, side string) fqStopResult {
	res := fqStopResult{Symbol: sym, Side: side}
	label := "止损（" + fqStopSideLabel(side) + "）"
	pct := b.shortStopPct
	if side == fqSideLong {
		pct = b.longStopPct
	}
	if pct == 0 {
		// Config changes can disable a stop that was already resting.
		if err := b.t.CancelStopLossOrdersForSide(sym, strings.ToUpper(side)); err != nil {
			res.Err = fmt.Sprintf("%s取消失败：%v", label, err)
		}
		return res
	}
	rows, err := b.t.GetPositions()
	if err != nil {
		res.Err = fmt.Sprintf("读取持仓以放置%s失败：%v", label, err)
		return res
	}
	for _, row := range rows {
		if s, _ := row["symbol"].(string); s != sym {
			continue
		}
		ps, _ := row["side"].(string)
		if strings.ToLower(ps) != side {
			continue
		}
		entry := fqFloat(row, "entryPrice")
		qty := math.Abs(fqFloat(row, "positionAmt"))
		if qty <= 0 {
			continue
		}
		if entry <= 0 || math.IsNaN(entry) || math.IsInf(entry, 0) {
			res.Err = fmt.Sprintf("%s持仓均价无效：%v", label, entry)
			return res
		}
		stop := entry * (1 + pct)
		if side == fqSideLong {
			stop = entry * (1 - pct)
		}
		mark := fqFloat(row, "markPrice")
		if mark <= 0 || math.IsNaN(mark) || math.IsInf(mark, 0) {
			mark, err = b.t.GetMarketPrice(sym)
			if err != nil || mark <= 0 || math.IsNaN(mark) || math.IsInf(mark, 0) {
				res.Err = fmt.Sprintf("%s读取标记价格失败：%v（价格 %v）", label, err, mark)
				return res
			}
		}
		res.Price = stop
		if (side == fqSideLong && mark <= stop) || (side == fqSideShort && mark >= stop) {
			if side == fqSideLong {
				_, err = b.t.CloseLong(sym, 0)
			} else {
				_, err = b.t.CloseShort(sym, 0)
			}
			if err != nil {
				res.Err = fmt.Sprintf("%s止损价已越过，市价平仓失败：%v", label, err)
			} else {
				res.Exited = true
				res.Note = "止损价已越过，已市价平仓"
			}
			return res
		}
		if err := b.t.SetStopLoss(sym, strings.ToUpper(side), qty, stop); err != nil {
			res.Err = fmt.Sprintf("%s %.8g 放置失败：%v", label, stop, err)
		}
		return res
	}
	// A filled close or a simulator-triggered exit can leave no position.
	return res
}

// ensureStops migrates and repairs both sides, including positions left inside
// the rebalance band. Paper SetStopLoss replaces only that side's STOP_MARKET.
func (b *fqFuturesBroker) ensureStops() []fqStopResult {
	acct, err := b.Account()
	if err != nil {
		return []fqStopResult{{Err: err.Error()}}
	}
	var out []fqStopResult
	for _, p := range acct.Positions {
		res := b.placeStop(p.Symbol, p.Side)
		if res.Price > 0 || res.Err != "" || res.Exited {
			out = append(out, res)
		}
	}
	return out
}

// ------------------------------------------------------------ us: bStock paper ledger

// fqStockBroker books the bStock paper ledger (store.StockPaper). It has no
// reference to any live spot executor: at.stockTrader is never touched.
type fqStockBroker struct {
	at *AutoTrader
	// Costs applied to every fill (basis points): slippage moves the fill price
	// against the trade, the fee is a fraction of the notional taken from cash.
	feeBps, shortFeeBps, slipBps float64
}

func (b *fqStockBroker) now() time.Time {
	if b.at.fqNow != nil {
		return b.at.fqNow()
	}
	return time.Now()
}

func (b *fqStockBroker) Account() (*fqAccount, error) {
	at := b.at
	if at.store == nil {
		return nil, fmt.Errorf("没有存储，无法读取美股模拟账本")
	}
	_, err := at.store.StockPaper().EnsureAccount(at.id, at.initialBalance)
	if err != nil {
		return nil, fmt.Errorf("读取美股模拟账本失败：%w", err)
	}
	open, err := at.store.StockPaper().ListOpen(at.id)
	if err != nil {
		return nil, fmt.Errorf("读取美股模拟持仓失败：%w", err)
	}
	out := &fqAccount{}
	marketValue := 0.0
	for _, p := range open {
		// Refresh the mark so equity snapshots between rebalances move with the
		// market; a failed quote falls back to the last cached / average price.
		side := fqSideLong
		if p.EffectiveSide() == store.StockPaperShort {
			side = fqSideShort
		}
		mark, _ := b.PriceForSide(p.Symbol, side)
		if mark <= 0 {
			if side == fqSideShort {
				mark = at.stock.perpPrice(p.Symbol)
			} else {
				mark = at.stock.price(p.Symbol)
			}
		}
		if mark <= 0 {
			mark = p.AvgPrice
		}
		b.applyFunding(p, mark, false)
		out.Positions = append(out.Positions, fqPosition{Symbol: p.Symbol, Side: side, Qty: p.Quantity, Entry: p.AvgPrice, Mark: mark})
		out.Exposure += p.Quantity * mark
		pnl := (mark - p.AvgPrice) * p.Quantity
		if side == fqSideShort {
			pnl = -pnl
		}
		out.Unrealized += pnl
		marketValue += p.MarketValue(mark)
	}
	// Funding changes cash, so read it after settling every open short.
	acct, err := at.store.StockPaper().GetAccount(at.id)
	if err != nil || acct == nil {
		return nil, fmt.Errorf("读取美股模拟现金失败：%v", err)
	}
	out.Balance, out.Equity = acct.Cash, acct.Cash+marketValue
	return out, nil
}

// applyFunding serializes settlement across the loop and API readers. Failed
// attempts also consume the 30-minute allowance, preventing retry storms.
// Reductions bypass the allowance and always settle before changing quantity.
func (b *fqStockBroker) applyFunding(p *store.StockPaperPosition, mark float64, force bool) {
	if p.EffectiveSide() != store.StockPaperShort {
		return
	}
	at, now := b.at, b.now()
	at.fq.fundingMu.Lock()
	defer at.fq.fundingMu.Unlock()
	if at.fq.fundingAt == nil {
		at.fq.fundingAt = map[int64]time.Time{}
	}
	if last, ok := at.fq.fundingAt[p.ID]; !force && ok && now.Sub(last) < 30*time.Minute {
		return
	}
	at.fq.fundingAt[p.ID] = now
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := at.stockApplyFunding(ctx, p, mark, now); err != nil {
		logger.Warnf("⚠️ [%s] factor_quant 空头资金费结算失败 %s：%v", at.name, p.Symbol, err)
	}
}

// PriceForSide keeps short sizing and marks on the perpetual quote. Spot
// prices retain their existing path; no executor is involved in either read.
func (b *fqStockBroker) PriceForSide(symbol, side string) (float64, error) {
	if side != fqSideShort {
		return b.Price(symbol)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, ok := stockLookupSymbol(ctx, symbol)
	if !ok || info.PerpSymbol == "" {
		return 0, fmt.Errorf("NOT_SHORTABLE: %s", symbol)
	}
	q, err := stockGetPerpQuote(ctx, info.PerpSymbol)
	if err != nil {
		return 0, err
	}
	if q == nil || q.Price <= 0 || math.IsNaN(q.Price) || math.IsInf(q.Price, 0) {
		return 0, fmt.Errorf("%s 没有有效的永续价格", symbol)
	}
	b.at.stock.setPerpPrice(symbol, q.Price)
	return q.Price, nil
}

func (b *fqStockBroker) Price(symbol string) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	q, err := stockGetQuote(ctx, symbol)
	if err != nil {
		return 0, err
	}
	if q == nil || q.BStockPrice <= 0 {
		return 0, fmt.Errorf("%s 没有有效的 bStock 价格", symbol)
	}
	b.at.stock.setPrice(symbol, q.BStockPrice)
	return q.BStockPrice, nil
}

func (b *fqStockBroker) FormatQty(symbol string, qty, price float64) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, ok := stockLookupSymbol(ctx, symbol)
	if !ok {
		return 0, fmt.Errorf("%s 不是已上市的 bStock 交易对", symbol)
	}
	// The registry currently exposes only bStock lot/minimum filters, no perp
	// step/minimum. Use those for both sides until perp filters are available;
	// FormatQty has no side parameter, so no target-side state is needed.
	q := floorStep(qty, info.StepSize)
	if q <= 0 || (info.MinQty > 0 && q < info.MinQty) {
		return 0, nil
	}
	if price > 0 && info.MinNotional > 0 && q*price < info.MinNotional {
		return 0, nil
	}
	return q, nil
}

func (b *fqStockBroker) Place(o fqOrder) fqResult {
	res := fqResult{Order: o}
	at := b.at
	if at.store == nil {
		res.Err = "没有存储"
		return res
	}
	if o.Side != fqSideLong {
		fq := at.config.StrategyConfig.FactorQuantConfig
		if o.Side == fqSideShort && fq.IsUS() && fq.EffectiveUSShortMode() == "paper" {
			return b.placeShort(o)
		}
		res.Err = "美股模拟账本只做多"
		return res
	}
	if o.Price <= 0 {
		res.Err = "缺少价格"
		return res
	}
	now := b.now()
	led := at.store.StockPaper()
	note := fmt.Sprintf("模拟账本成交（滑点 %.4g bp，手续费 %.4g bp）", b.slipBps, b.feeBps)
	if o.opens() {
		fill := o.Price * (1 + b.slipBps/10000) // buy: adverse = higher
		acct, err := led.GetAccount(at.id)
		if err != nil || acct == nil {
			res.Err = fmt.Sprintf("读取模拟现金失败：%v", err)
			return res
		}
		cost := o.Qty * fill
		fee := cost * b.feeBps / 10000
		if cost+fee > acct.Cash+1e-9 {
			res.Err = fmt.Sprintf("模拟现金不足：需要 %.2f（含手续费 %.2f），可用 %.2f", cost+fee, fee, acct.Cash)
			return res
		}
		if _, fee, err = led.OpenOrAddWithFee(at.id, o.Symbol, o.Qty, fill, 0, 0, b.feeBps, now); err != nil {
			res.Err = err.Error()
			return res
		}
		res.OK, res.FillQty, res.FillPrice, res.Fee, res.Note = true, o.Qty, fill, fee, note
		return res
	}
	qty := o.Qty
	if o.CloseAll {
		qty = 0
	}
	fill := o.Price * (1 - b.slipBps/10000) // sell: adverse = lower
	_, _, fee, err := led.ReduceWithFee(at.id, o.Symbol, qty, fill, b.feeBps, now, "factor_quant_rebalance")
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.OK, res.FillQty, res.FillPrice, res.Fee, res.Note = true, o.Qty, fill, fee, note
	return res
}

func (b *fqStockBroker) placeShort(o fqOrder) fqResult {
	res := fqResult{Order: o}
	// Always fetch a fresh perpetual quote, including when the planner has a
	// cached price. This path writes only store.StockPaper, never an executor.
	price, err := b.PriceForSide(o.Symbol, fqSideShort)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	at, now := b.at, b.now()
	led := at.store.StockPaper()
	fill, fee := price*(1-b.slipBps/10000), 0.0
	if o.opens() {
		_, fee, err = led.OpenOrAddSideWithFee(at.id, o.Symbol, store.StockPaperShort, o.Qty, fill, 0, 0, b.shortFeeBps, now)
	} else {
		rows, readErr := led.ListOpen(at.id)
		if readErr != nil {
			res.Err = readErr.Error()
			return res
		}
		for _, p := range rows {
			if p.Symbol == o.Symbol && p.EffectiveSide() == store.StockPaperShort {
				b.applyFunding(p, price, true)
				break
			}
		}
		qty := o.Qty
		if o.CloseAll {
			qty = 0
		}
		fill = price * (1 + b.slipBps/10000)
		_, _, fee, err = led.ReduceSideWithFee(at.id, o.Symbol, store.StockPaperShort, qty, fill, b.shortFeeBps, now, "factor_quant_rebalance")
	}
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.OK, res.FillQty, res.FillPrice, res.Fee = true, o.Qty, fill, fee
	res.Note = fmt.Sprintf("模拟永续账本成交（滑点 %.4g bp，手续费 %.4g bp）", b.slipBps, b.shortFeeBps)
	return res
}

// ------------------------------------------------------------ orchestration

// fqRebalanceReport is everything one rebalance attempt produced.
type fqRebalanceReport struct {
	Targets *quantTargets
	Before  *fqAccount
	After   *fqAccount
	Plan    fqPlan
	Results []fqResult
	Stops   []fqStopResult // protection checks, independent of planned market orders
	// PriceErrs: symbols whose price could not be read (planned around).
	PriceErrs map[string]string
	Warnings  []string // execution anomalies retained in tick notes and records
	// Err: the attempt aborted before any order (account or plan inputs unavailable).
	Err error
}

func (r *fqRebalanceReport) withWarnings(message string) string {
	if len(r.Warnings) > 0 {
		return message + "；" + strings.Join(r.Warnings, "；")
	}
	return message
}

func (r *fqRebalanceReport) failedCount() int {
	n := 0
	for _, res := range r.Results {
		if res.failed() {
			n++
		}
	}
	for _, stop := range r.Stops {
		if stop.Err != "" {
			n++
		}
	}
	return n
}

// fqRunRebalance plans against the broker's account and executes every order
// in sequence; one failing order never stops the rest.
func fqRunRebalance(b fqBroker, t *quantTargets, bandCfg fqPlanConfig) *fqRebalanceReport {
	rep := &fqRebalanceReport{Targets: t, PriceErrs: map[string]string{}}
	before, err := b.Account()
	if err != nil {
		rep.Err = err
		return rep
	}
	if (before.Equity <= 0 && !bandCfg.Flatten) || math.IsNaN(before.Equity) || math.IsInf(before.Equity, 0) {
		rep.Err = fmt.Errorf("账户权益 %.4f 无效，不调仓", before.Equity)
		return rep
	}
	rep.Before = before

	symbols := map[string]bool{}
	for s, w := range t.Weights {
		if w != 0 {
			symbols[s] = true
		}
	}
	for _, p := range before.Positions {
		symbols[p.Symbol] = true
	}
	names := make([]string, 0, len(symbols))
	for s := range symbols {
		names = append(names, s)
	}
	sort.Strings(names)
	prices := map[string]float64{}
	priceSides := map[string]string{}
	sided, hasSidePrices := b.(interface {
		PriceForSide(string, string) (float64, error)
	})
	for _, s := range names {
		var p float64
		var perr error
		if hasSidePrices {
			side := fqSideOf(fqSign(t.Weights[s]))
			if t.Weights[s] == 0 {
				for _, pos := range before.Positions {
					if pos.Symbol == s {
						side = pos.Side
						break
					}
				}
			}
			p, perr = sided.PriceForSide(s, side)
			priceSides[s] = side
		} else {
			p, perr = b.Price(s)
		}
		if perr != nil || p <= 0 || math.IsNaN(p) || math.IsInf(p, 0) {
			if perr == nil {
				perr = fmt.Errorf("价格 %v 无效", p)
			}
			rep.PriceErrs[s] = perr.Error()
			continue
		}
		prices[s] = p
	}

	planEquity := before.Equity
	if bandCfg.CostRate > 0 {
		planEquity /= 1 + bandCfg.CostRate
	}
	planOrders := fqPlanOrders
	twoSided := fqTwoSidedSymbols(before.Positions)
	if len(twoSided) > 0 {
		warning := fmt.Sprintf("检测到双边持仓 %s，按两侧核对目标", strings.Join(twoSided, ", "))
		rep.Warnings = append(rep.Warnings, warning)
		logger.Warnf("⚠️ factor_quant %s", warning)
	}
	if bandCfg.Retry || len(twoSided) > 0 {
		planOrders = fqPlanRetryOrders
	}
	rep.Plan = planOrders(fqPlanInput{
		Positions:       before.Positions,
		Equity:          planEquity,
		Prices:          prices,
		Weights:         t.Weights,
		CapitalFraction: bandCfg.CapitalFraction,
		Band:            bandCfg.Band,
		MinOrder:        bandCfg.MinOrder,
		LongOnly:        bandCfg.LongOnly,
		Flatten:         bandCfg.Flatten,
		Format:          b.FormatQty,
	})
	for _, o := range rep.Plan.Orders {
		// A direction flip was sized using the target side's venue. Refresh the
		// old side's close price so a long sale still uses the bStock quote.
		if side := priceSides[o.Symbol]; side != "" && side != o.Side {
			price, err := sided.PriceForSide(o.Symbol, o.Side)
			if err != nil {
				rep.Results = append(rep.Results, fqResult{Order: o, Err: err.Error()})
				continue
			}
			o.Price, o.Notional = price, o.Qty*price
		}
		res := b.Place(o)
		if res.failed() {
			logger.Warnf("⚠️ factor_quant %s 失败：%s %s", o.summary(), res.Err, res.StopErr)
		}
		rep.Results = append(rep.Results, res)
	}
	if after, aerr := b.Account(); aerr == nil {
		rep.After = after
	}
	return rep
}

// fqPlanConfig carries the resolved knobs of a rebalance.
type fqPlanConfig struct {
	CapitalFraction float64
	Band            float64
	MinOrder        float64
	LongOnly        bool
	Flatten         bool // risk reduction only: close all sides even with nonpositive equity
	Retry           bool // reconcile both sides left by a partially failed direction flip
	// CostRate (fraction) is the round-trip-ish cost buffer of fee-charging
	// venues: the planner sizes against equity/(1+CostRate) so a fully invested
	// target is still affordable once slippage and fees are paid from cash.
	CostRate float64
}

// compile-time guard: the crypto broker only ever wraps the paper simulator
// when built by newFactorQuantBroker (see auto_trader_factor_quant.go).
var _ fqFuturesTrader = (*paper.PaperFuturesTrader)(nil)
