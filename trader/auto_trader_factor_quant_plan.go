package trader

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/store"
)

// factor_quant (design 2026-11-01): the pure parts — the rebalance decision
// and the order planner. No I/O, no clocks: everything is a parameter so the
// rules are unit-testable.

// ------------------------------------------------------------ rebalance decision

// Decision codes (also used as skip-record keys).
const (
	fqCodeRebalance      = "rebalance"
	fqCodeStatus         = "status"
	fqCodeInvalid        = "invalid"
	fqCodeStale          = "stale"
	fqCodeHorizon        = "horizon"
	fqCodeTooSoon        = "too_soon"
	fqCodeUnmanaged      = "unmanaged_positions"
	fqCodeRetryExhausted = "retry_exhausted"
)

// fqDecision is the outcome of the rebalance gate.
type fqDecision struct {
	Rebalance bool
	Code      string
	Reason    string
	Horizon   int // effective horizon in days (config, or the file's when the config follows it)
}

// noisy reports whether a non-rebalance outcome deserves a decision record
// (too_soon is the normal state between rebalances and is only logged).
func (d fqDecision) noisy() bool { return !d.Rebalance && d.Code != fqCodeTooSoon }

// fqUTCDate truncates t to its UTC calendar day.
func fqUTCDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func fqDaysBetween(from, to time.Time) int {
	return int(math.Round(fqUTCDate(to).Sub(fqUTCDate(from)).Hours() / 24))
}

// fqShouldRebalance decides whether the targets t may be traded now.
//
//	lastAsOf       as_of of the last rebalance attempt ("" = never rebalanced)
//	hasPositions   the strategy currently holds positions
func fqShouldRebalance(t *quantTargets, cfg *store.FactorQuantConfig, lastAsOf string, hasPositions bool, now time.Time) fqDecision {
	if t == nil {
		return fqDecision{Code: fqCodeInvalid, Reason: "没有可用的目标文件"}
	}
	if t.Status != quantStatusOK {
		reason := fmt.Sprintf("目标状态为 %s，不调仓（保持现状、不开新仓）", t.Status)
		if r := t.reasonText(); r != "" {
			reason += "：" + r
		}
		return fqDecision{Code: fqCodeStatus, Reason: reason}
	}
	if t.asOfDate.IsZero() {
		return fqDecision{Code: fqCodeInvalid, Reason: "status=ok 但缺少 as_of"}
	}
	horizon := cfg.EffectiveHorizon()
	if horizon > 0 && t.Horizon != horizon {
		return fqDecision{Code: fqCodeHorizon, Horizon: horizon,
			Reason: fmt.Sprintf("目标 horizon=%d 与策略配置 horizon=%d 不一致，不调仓", t.Horizon, horizon)}
	}
	if horizon <= 0 {
		horizon = t.Horizon // us: the config follows the file
	}
	if horizon <= 0 {
		return fqDecision{Code: fqCodeInvalid, Reason: fmt.Sprintf("目标 horizon=%d 无效", t.Horizon)}
	}
	age := fqDaysBetween(t.asOfDate, now)
	if age < 0 {
		return fqDecision{Code: fqCodeInvalid, Horizon: horizon, Reason: fmt.Sprintf("as_of=%s 在未来（今天 UTC %s）", t.AsOf, fqUTCDate(now).Format(factorDateFmt))}
	}
	if maxStale := cfg.EffectiveMaxStaleDays(t.Horizon); age > maxStale {
		return fqDecision{Code: fqCodeStale, Horizon: horizon,
			Reason: fmt.Sprintf("目标 as_of=%s 已落后 %d 天，超过 max_stale_days=%d，不调仓（请检查因子挖掘任务）", t.AsOf, age, maxStale)}
	}
	if lastAsOf == "" {
		if hasPositions {
			return fqDecision{Code: fqCodeUnmanaged, Horizon: horizon,
				Reason: "账户已有持仓但没有调仓记录，为避免误动非本策略持仓，不调仓（需人工清仓后才会首次调仓）"}
		}
		return fqDecision{Rebalance: true, Code: fqCodeRebalance, Horizon: horizon, Reason: "首次调仓（账户空仓）"}
	}
	last, err := time.Parse(factorDateFmt, lastAsOf)
	if err != nil {
		return fqDecision{Code: fqCodeInvalid, Horizon: horizon, Reason: fmt.Sprintf("已持久化的 last_as_of=%q 无法解析", lastAsOf)}
	}
	if gap := fqDaysBetween(last, t.asOfDate); gap < horizon {
		return fqDecision{Code: fqCodeTooSoon, Horizon: horizon,
			Reason: fmt.Sprintf("距上次调仓 as_of=%s 仅 %d 天，未满 horizon=%d 天", lastAsOf, gap, horizon)}
	}
	return fqDecision{Rebalance: true, Code: fqCodeRebalance, Horizon: horizon,
		Reason: fmt.Sprintf("距上次调仓 as_of=%s 已 %d 天 ≥ horizon=%d", lastAsOf, fqDaysBetween(last, t.asOfDate), horizon)}
}

// ------------------------------------------------------------ order planner

const (
	fqSideLong  = "long"
	fqSideShort = "short"

	fqKindClose    = "close"    // flatten a symbol (absent from the weights, or the wrong side)
	fqKindReduce   = "reduce"   // shrink a same-side position
	fqKindIncrease = "increase" // grow a same-side position
	fqKindOpen     = "open"     // new position (including the re-open after a flip)
)

// fqPosition is one held position (Qty > 0 whatever the side).
type fqPosition struct {
	Symbol string
	Side   string // fqSideLong | fqSideShort
	Qty    float64
	Entry  float64
	Mark   float64
}

// fqOrder is one planned market order.
type fqOrder struct {
	Symbol   string
	Kind     string // fqKind*
	Side     string // position side being traded: fqSideLong | fqSideShort
	Qty      float64
	CloseAll bool // full close: the executor passes quantity 0 ("everything")
	Price    float64
	Notional float64
	Target   float64 // signed target notional of the symbol (0 for closes)
	Reason   string
}

// Action maps to the decision-record vocabulary.
func (o fqOrder) Action() string {
	closing := o.Kind == fqKindClose || o.Kind == fqKindReduce
	switch {
	case closing && o.Side == fqSideLong:
		return "close_long"
	case closing:
		return "close_short"
	case o.Side == fqSideLong:
		return "open_long"
	default:
		return "open_short"
	}
}

// opens reports whether the order adds exposure (open / increase).
func (o fqOrder) opens() bool { return o.Kind == fqKindOpen || o.Kind == fqKindIncrease }

func (o fqOrder) phase() int {
	switch o.Kind {
	case fqKindClose:
		return 0
	case fqKindReduce:
		return 1
	}
	return 2
}

// fqSkip is a symbol the plan deliberately leaves alone.
type fqSkip struct {
	Symbol string
	Reason string
}

// fqPlanInput is everything the planner needs.
type fqPlanInput struct {
	Positions       []fqPosition
	Equity          float64
	Prices          map[string]float64
	Weights         map[string]float64
	CapitalFraction float64
	Band            float64 // fraction of the target notional
	MinOrder        float64 // USDT
	LongOnly        bool    // us: negative weights are ignored
	Flatten         bool    // stale flatten: close every held side with empty weights
	// Format rounds a quantity to the venue's lot size (nil = identity). It
	// returns 0 when the quantity is below the venue minimum; an error marks
	// the symbol unplannable.
	Format func(symbol string, qty, price float64) (float64, error)
}

// fqPlan is the ordered result.
type fqPlan struct {
	Budget float64 // equity × capital fraction
	Orders []fqOrder
	Skips  []fqSkip
}

func fqSign(v float64) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

func fqSideOf(sign int) string {
	if sign > 0 {
		return fqSideLong
	}
	return fqSideShort
}

// fqPlanRetryOrders also reconciles both sides left by a failed flip: the
// original pass can fail to close the old side while opening the target side.
// Keep that target holding in the ordinary band calculation, and plan full
// closes for every unwanted side independently of the symbol-keyed planner.
func fqPlanRetryOrders(in fqPlanInput) fqPlan {
	var wanted, unwanted []fqPosition
	for _, p := range in.Positions {
		w := in.Weights[p.Symbol]
		if w == 0 || (in.LongOnly && w < 0) || p.Side != fqSideOf(fqSign(w)) {
			unwanted = append(unwanted, p)
		} else {
			wanted = append(wanted, p)
		}
	}
	in.Positions = wanted
	plan := fqPlanOrders(in)
	in.Positions, in.Weights, in.Flatten = unwanted, nil, true
	closes := fqPlanOrders(in)
	plan.Orders = append(closes.Orders, plan.Orders...)
	return plan
}

// fqPlanOrders turns current holdings + target weights into ordered orders:
// closes (absent symbols, opposite side) first, then reductions, then
// increases / opens, so margin is released before it is consumed.
func fqPlanOrders(in fqPlanInput) fqPlan {
	plan := fqPlan{}
	cf := in.CapitalFraction
	if cf <= 0 || cf > 1 {
		cf = 1
	}
	plan.Budget = in.Equity * cf
	// Empty targets close every position, including both sides of a hedged
	// symbol. Full closes bypass lot-size, minimum-order and rebalance bands.
	if in.Flatten && len(in.Weights) == 0 {
		for _, p := range in.Positions {
			if p.Qty <= 0 {
				continue
			}
			price := in.Prices[p.Symbol]
			if price <= 0 {
				price = p.Mark
			}
			plan.Orders = append(plan.Orders, fqOrder{Symbol: p.Symbol, Kind: fqKindClose, Side: p.Side, Qty: p.Qty,
				CloseAll: true, Price: price, Notional: p.Qty * price, Reason: "不在目标权重中，平仓"})
		}
		sort.SliceStable(plan.Orders, func(i, j int) bool {
			if plan.Orders[i].Symbol != plan.Orders[j].Symbol {
				return plan.Orders[i].Symbol < plan.Orders[j].Symbol
			}
			return plan.Orders[i].Side < plan.Orders[j].Side
		})
		return plan
	}
	format := in.Format
	if format == nil {
		format = func(_ string, qty, _ float64) (float64, error) { return qty, nil }
	}

	held := map[string]fqPosition{}
	for _, p := range in.Positions {
		if p.Qty > 0 {
			held[p.Symbol] = p
		}
	}
	symbols := map[string]bool{}
	for s := range held {
		symbols[s] = true
	}
	for s, w := range in.Weights {
		if w != 0 {
			symbols[s] = true
		}
	}
	names := make([]string, 0, len(symbols))
	for s := range symbols {
		names = append(names, s)
	}
	sort.Strings(names)

	skip := func(sym, reason string) { plan.Skips = append(plan.Skips, fqSkip{Symbol: sym, Reason: reason}) }

	for _, sym := range names {
		pos, isHeld := held[sym]
		w := in.Weights[sym]
		if in.LongOnly && w < 0 {
			skip(sym, "只做多市场，忽略负权重")
			w = 0
		}
		price := in.Prices[sym]
		if price <= 0 && isHeld {
			price = pos.Mark
		}
		sign := fqSign(w)
		target := w * plan.Budget // signed target notional

		// Positions the targets no longer want (absent, or the other side).
		if isHeld && (sign == 0 || fqSideOf(sign) != pos.Side) {
			reason := "不在目标权重中，平仓"
			if sign != 0 {
				reason = "目标方向相反，先平仓"
			}
			plan.Orders = append(plan.Orders, fqOrder{Symbol: sym, Kind: fqKindClose, Side: pos.Side, Qty: pos.Qty, CloseAll: true,
				Price: price, Notional: pos.Qty * price, Reason: reason})
			isHeld = false
		}
		if sign == 0 {
			continue
		}
		if price <= 0 {
			skip(sym, "缺少可用价格，无法下单")
			continue
		}
		side := fqSideOf(sign)
		absTarget := math.Abs(target)

		if !isHeld { // new exposure (or the re-open after a flip)
			if absTarget < in.MinOrder {
				skip(sym, fmt.Sprintf("目标名义 %.2f 低于最小下单额 %.2f", absTarget, in.MinOrder))
				continue
			}
			qty, err := format(sym, absTarget/price, price)
			if err != nil {
				skip(sym, "数量取整失败："+err.Error())
				continue
			}
			if qty <= 0 || qty*price < in.MinOrder {
				skip(sym, fmt.Sprintf("取整后数量 %.8g（名义 %.2f）低于下单下限", qty, qty*price))
				continue
			}
			plan.Orders = append(plan.Orders, fqOrder{Symbol: sym, Kind: fqKindOpen, Side: side, Qty: qty, Price: price,
				Notional: qty * price, Target: target, Reason: "按目标权重建仓"})
			continue
		}

		// Same side: move the held notional towards the target.
		cur := pos.Qty * price
		diff := absTarget - cur
		if math.Abs(diff) < in.Band*absTarget {
			skip(sym, fmt.Sprintf("目标 %.2f 与现有 %.2f 之差 %.2f 在 %.0f%% 调仓带内", absTarget, cur, math.Abs(diff), in.Band*100))
			continue
		}
		qty, err := format(sym, math.Abs(diff)/price, price)
		if err != nil {
			skip(sym, "数量取整失败："+err.Error())
			continue
		}
		if qty <= 0 || qty*price < in.MinOrder {
			skip(sym, fmt.Sprintf("调整量取整后 %.8g（名义 %.2f）低于下单下限", qty, qty*price))
			continue
		}
		if diff < 0 {
			if qty >= pos.Qty {
				plan.Orders = append(plan.Orders, fqOrder{Symbol: sym, Kind: fqKindClose, Side: side, Qty: pos.Qty, CloseAll: true,
					Price: price, Notional: pos.Qty * price, Target: target, Reason: "减仓量不小于持仓，全平"})
				continue
			}
			plan.Orders = append(plan.Orders, fqOrder{Symbol: sym, Kind: fqKindReduce, Side: side, Qty: qty, Price: price,
				Notional: qty * price, Target: target, Reason: "高于目标，减仓"})
			continue
		}
		plan.Orders = append(plan.Orders, fqOrder{Symbol: sym, Kind: fqKindIncrease, Side: side, Qty: qty, Price: price,
			Notional: qty * price, Target: target, Reason: "低于目标，加仓"})
	}

	// Stable by phase; symbols are already alphabetical within a phase.
	sort.SliceStable(plan.Orders, func(i, j int) bool { return plan.Orders[i].phase() < plan.Orders[j].phase() })
	return plan
}

// fqSummaryLine renders one order for logs and records.
func (o fqOrder) summary() string {
	return fmt.Sprintf("%s %s %s qty=%.8g @%.6g 名义=%.2f", o.Action(), o.Symbol, strings.ToUpper(o.Side), o.Qty, o.Price, o.Notional)
}
