package stockengine

import (
	"math"
	"nofx/market/usstock"
)

// Validate preserves input order, rejects later duplicate symbols, and reserves
// accepted buy notionals for sizing subsequent buys in this cycle. It never
// mutates the context or assumes a proposed close has already released capacity.
func Validate(ctx *Context, decisions []Decision) []Verdict {
	verdicts := make([]Verdict, 0, len(decisions))
	seen := make(map[string]bool)
	opens, cycleNotional := 0, 0.0
	held := 0
	if ctx != nil {
		for _, p := range ctx.Positions {
			if p.Quantity > 0 {
				held++
			}
		}
	}
	for _, d := range decisions {
		v := Verdict{Decision: d}
		code, note := validateOne(ctx, d, seen, held+opens)
		if code == "" && (d.Action == ActionOpenLong || d.Action == ActionAddLong) {
			size, err := sizeOrder(ctx, d, cycleNotional)
			if err != nil {
				code = err.Error()
			} else {
				cycleNotional += size.Notional
				if d.Action == ActionOpenLong {
					opens++
				}
			}
		}
		v.Accepted, v.Note = code == "", note
		if code != "" {
			v.Codes = []string{code}
		}
		verdicts = append(verdicts, v)
	}
	return verdicts
}

func validateOne(ctx *Context, d Decision, seen map[string]bool, positionCount int) (string, string) {
	known := false
	if ctx != nil && ctx.Config != nil {
		for _, symbol := range ctx.Config.Symbols {
			if symbol == d.Symbol {
				known = true
				break
			}
		}
	}
	if !known {
		return "UNKNOWN_SYMBOL", ""
	}
	if seen[d.Symbol] {
		return "DUPLICATE", ""
	}
	seen[d.Symbol] = true
	if !validAction(d.Action) {
		return "UNKNOWN_ACTION", ""
	}
	p := positionFor(ctx, d.Symbol)
	if d.Action == ActionOpenLong && p != nil {
		return "ALREADY_HELD", "Use add_long for an existing position."
	}
	if p == nil {
		switch d.Action {
		case ActionAddLong, ActionReduceLong, ActionCloseLong, ActionAdjustStop, ActionHold:
			return "NOT_HELD", ""
		}
	}
	s := snapshotFor(ctx, d.Symbol)
	switch d.Action {
	case ActionOpenLong, ActionAddLong:
		if ctx.Session == usstock.SessionClosed {
			return "SESSION_CLOSED", ""
		}
		if !sessionAllowed(ctx.Config, ctx.Session) {
			return "SESSION_NOT_ALLOWED", ""
		}
		preset := contextPreset(ctx)
		if s == nil || !positive(s.Price) {
			return "DATA_INSUFFICIENT", ""
		}
		for _, tf := range s.MissingTF {
			if tf == usstock.TF1d || tf == preset.TrendTF || tf == preset.EntryTF {
				return "DATA_INSUFFICIENT", ""
			}
		}
		if s.Quote == nil || !s.Quote.RefFresh {
			return "BSTOCK_REF_STALE", ""
		}
		limit := effectiveMaxDivergencePct(ctx.Config)
		if limit >= 0 && (!finite(s.Quote.DivergencePct) || math.Abs(s.Quote.DivergencePct) > limit) {
			return "BSTOCK_DIVERGENCE", ""
		}
		if d.EntryType != "" && d.EntryType != EntryMarket && d.EntryType != EntryLimit {
			return "ENTRY_INVALID", ""
		}
		entry := entryPrice(s, d)
		if d.EntryType == EntryLimit && (!positive(entry) || entry > s.Price*1.01) {
			return "LIMIT_INVALID", ""
		}
		if !positive(d.StopLoss) || d.StopLoss >= entry {
			return "STOP_INVALID", ""
		}
		if !positive(s.ATR1d) {
			return "DATA_INSUFFICIENT", ""
		}
		multiple := (entry - d.StopLoss) / s.ATR1d
		if multiple < preset.StopATRMin || multiple > preset.StopATRMax {
			return "STOP_OUT_OF_BAND", ""
		}
		if !finite(d.TakeProfit) || (d.TakeProfit != 0 && d.TakeProfit <= entry) {
			return "TP_INVALID", ""
		}
		if d.Action == ActionOpenLong && positionCount >= effectiveMaxPositions(ctx.Config) {
			return "MAX_POSITIONS", ""
		}
		if d.Action == ActionAddLong && (s.Price <= p.InitialStop || s.Price <= p.AvgPrice) {
			return "ADD_WHILE_LOSING", ""
		}
	case ActionReduceLong:
		if !finite(d.ReduceFraction) || d.ReduceFraction <= 0 || d.ReduceFraction >= 1 {
			return "REDUCE_INVALID", ""
		}
	case ActionAdjustStop:
		if s == nil || !positive(s.Price) {
			return "DATA_INSUFFICIENT", ""
		}
		if !positive(d.StopLoss) || d.StopLoss >= s.Price {
			return "STOP_INVALID", ""
		}
		if d.StopLoss < p.StopPrice {
			return "STOP_LOOSEN", ""
		}
	}
	return "", ""
}
