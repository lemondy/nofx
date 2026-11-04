package stockengine

import (
	"fmt"
	"math"
	"nofx/market/usstock"
	"strings"
)

// MinEntryConfidence is the confidence gate (0–100): open_long / add_long below
// it are rejected with LOW_CONFIDENCE. Exits, adjusts, hold and wait are not gated.
const MinEntryConfidence = 80

// RepairableCodes are the rejection codes the model can fix by resubmitting the
// entry (a bad stop/limit/take-profit/entry type, or a missing thesis). A
// rejected open/add is repaired only when ALL its codes are in this set: a
// low-confidence idea, a full book, a closed session or missing data cannot be
// repaired by rewording the decision.
var RepairableCodes = map[string]bool{
	"STOP_INVALID":     true,
	"STOP_OUT_OF_BAND": true,
	"LIMIT_INVALID":    true,
	"TP_INVALID":       true,
	"ENTRY_INVALID":    true,
	"THESIS_REQUIRED":  true,
}

// Validate preserves input order, rejects later duplicate symbols, and reserves
// accepted buy notionals for sizing subsequent buys in this cycle. It never
// mutates the context or assumes a proposed close has already released capacity.
func Validate(ctx *Context, decisions []Decision) []Verdict {
	verdicts := make([]Verdict, 0, len(decisions))
	seen := make(map[string]bool)
	opens, cycleNotional, cycleShort := 0, 0.0, 0.0
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
		var extra []string
		if IsEntryAction(d.Action) {
			if strings.TrimSpace(d.Thesis) == "" || strings.TrimSpace(d.Invalidation) == "" {
				switch {
				case code == "":
					code, note = "THESIS_REQUIRED", "thesis (why it works over weeks) and invalidation (what proves it wrong) are both required for open_long / add_long."
				case RepairableCodes[code]:
					extra = append(extra, "THESIS_REQUIRED")
					note = strings.TrimSpace(note + " Also: thesis and invalidation are both required for open_long / add_long.")
				}
			}
			if IsShortAction(d.Action) {
				note = strings.ReplaceAll(strings.ReplaceAll(note, "open_long", "open_short"), "add_long", "add_short")
			}
		}
		if code == "" && IsEntryAction(d.Action) {
			size, err := sizeOrder(ctx, d, cycleNotional, cycleShort)
			if err != nil {
				code = err.Error()
			} else {
				cycleNotional += size.Notional
				if IsShortAction(d.Action) {
					cycleShort += size.Notional
				}
				if d.Action == ActionOpenLong || d.Action == ActionOpenShort {
					opens++
				}
			}
		}
		v.Accepted, v.Note = code == "", note
		if code != "" {
			v.Codes = append([]string{code}, extra...)
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
	var p *Position
	if ctx != nil {
		p = positionFor(ctx, d.Symbol)
	}
	if !known && p == nil {
		return "UNKNOWN_SYMBOL", ""
	}
	entryAllowed := known
	if ctx != nil && ctx.EntrySymbols != nil {
		entryAllowed = ctx.EntrySymbols[d.Symbol]
	}
	if IsEntryAction(d.Action) && !entryAllowed {
		return "EXIT_ONLY_SYMBOL", "Held symbol is managed for hold/reduce/close/adjust_stop only; new exposure is not authorized."
	}
	if seen[d.Symbol] {
		return "DUPLICATE", ""
	}
	seen[d.Symbol] = true
	if !validAction(d.Action) {
		return "UNKNOWN_ACTION", ""
	}
	short := IsShortAction(d.Action)
	if short && !ctx.Config.ShortEnabled() {
		return "SHORT_DISABLED", "Shorts require short_mode=paper and paper_trading=true."
	}
	if p != nil && d.Action != ActionHold && d.Action != ActionWait && d.Action != ActionAdjustStop && short != IsShortSide(p.Side) {
		return "OPPOSITE_HELD", "Cannot trade the opposite side of a held symbol."
	}
	if (d.Action == ActionOpenLong || d.Action == ActionOpenShort) && p != nil {
		if short {
			return "ALREADY_HELD", "Use add_short for an existing position."
		}
		return "ALREADY_HELD", "Use add_long for an existing position."
	}
	if p == nil {
		switch d.Action {
		case ActionAddLong, ActionReduceLong, ActionCloseLong, ActionAddShort, ActionReduceShort, ActionCloseShort, ActionAdjustStop, ActionHold:
			return "NOT_HELD", ""
		}
	}
	s := snapshotFor(ctx, d.Symbol)
	switch d.Action {
	case ActionOpenLong, ActionAddLong, ActionOpenShort, ActionAddShort:
		if short && (s == nil || !s.Shortable) {
			return "NOT_SHORTABLE", "No TRADING EQUITY TRADIFI_PERPETUAL mapping."
		}
		if ctx.Session == usstock.SessionClosed {
			return "SESSION_CLOSED", ""
		}
		if !sessionAllowed(ctx.Config, ctx.Session) {
			return "SESSION_NOT_ALLOWED", ""
		}
		if d.Confidence < MinEntryConfidence {
			return "LOW_CONFIDENCE", fmt.Sprintf("confidence %d is below the entry gate %d.", d.Confidence, MinEntryConfidence)
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
		for tf, cutoff := range s.DataThrough {
			if (tf == usstock.TF1d || tf == preset.TrendTF || tf == preset.EntryTF) && usstock.CheckDataDate(tf, cutoff, ctx.Now) != nil {
				return "DATA_STALE", ""
			}
		}

		if s.Quote == nil || !s.Quote.RefFresh {
			return "BSTOCK_REF_STALE", ""
		}
		limit := effectiveMaxDivergencePct(ctx.Config)
		if limit >= 0 && (!finite(s.Quote.DivergencePct) || math.Abs(s.Quote.DivergencePct) > limit) {
			return "BSTOCK_DIVERGENCE", ""
		}
		if short {
			// Age is not checked here: decisions arrive minutes after the
			// snapshot, and the entry re-quote enforces the 30s bound (as the
			// bStock quote age is enforced only at execution for longs).
			if s.PerpQuote == nil || !positive(s.PerpQuote.Price) {
				return "PERP_PRICE_MISSING", "No usable perpetual quote."
			}
			copy := *s
			copy.Price = s.PerpQuote.Price
			s = &copy
		}
		if d.EntryType != "" && d.EntryType != EntryMarket && d.EntryType != EntryLimit {
			return "ENTRY_INVALID", "entry_type must be market or limit."
		}
		entry := entryPrice(s, d)
		if d.EntryType == EntryLimit && (!positive(entry) || (!short && entry > s.Price*1.01) || (short && entry < s.Price*0.99)) {
			if short {
				return "LIMIT_INVALID", fmt.Sprintf("limit_price must be >0 and ≥ market price×0.99 = %.4f.", s.Price*0.99)
			}
			return "LIMIT_INVALID", fmt.Sprintf("limit_price must be >0 and ≤ market price×1.01 = %.4f (yours %.4f).", s.Price*1.01, entry)
		}
		if !positive(d.StopLoss) || (!short && d.StopLoss >= entry) || (short && d.StopLoss <= entry) {
			if short {
				return "STOP_INVALID", fmt.Sprintf("stop_loss must exceed entry %.4f.", entry)
			}
			return "STOP_INVALID", fmt.Sprintf("stop_loss must be >0 and below the entry price %.4f (yours %.4f).", entry, d.StopLoss)
		}
		if !positive(s.ATR1d) {
			return "DATA_INSUFFICIENT", ""
		}
		multiple := math.Abs(entry-d.StopLoss) / s.ATR1d
		if multiple < preset.StopATRMin || multiple > preset.StopATRMax {
			if short {
				return "STOP_OUT_OF_BAND", fmt.Sprintf("stop must be within [%.2f, %.2f]×ATR(1d) above entry %.4f; yours is %.2f×ATR.", preset.StopATRMin, preset.StopATRMax, entry, multiple)
			}
			return "STOP_OUT_OF_BAND", fmt.Sprintf("stop must be within [%.2f, %.2f]×ATR(1d) = [%.4f, %.4f] below entry %.4f; yours %.4f is %.2f×ATR.",
				preset.StopATRMin, preset.StopATRMax, entry-preset.StopATRMax*s.ATR1d, entry-preset.StopATRMin*s.ATR1d, entry, d.StopLoss, multiple)
		}
		if !finite(d.TakeProfit) || (d.TakeProfit != 0 && ((!short && d.TakeProfit <= entry) || (short && (d.TakeProfit <= 0 || d.TakeProfit >= entry)))) {
			if short {
				return "TP_INVALID", fmt.Sprintf("take_profit must be 0 (none) or >0 and below entry %.4f.", entry)
			}
			return "TP_INVALID", fmt.Sprintf("take_profit must be 0 (none) or above the entry price %.4f (yours %.4f).", entry, d.TakeProfit)
		}
		if (d.Action == ActionOpenLong || d.Action == ActionOpenShort) && positionCount >= effectiveMaxPositions(ctx.Config) {
			return "MAX_POSITIONS", ""
		}
		if (d.Action == ActionAddLong && (s.Price <= p.InitialStop || s.Price <= p.AvgPrice)) ||
			(d.Action == ActionAddShort && (s.Price >= p.AvgPrice || s.Price >= p.InitialStop)) {
			return "ADD_WHILE_LOSING", ""
		}
	case ActionReduceLong, ActionReduceShort:
		if !finite(d.ReduceFraction) || d.ReduceFraction <= 0 || d.ReduceFraction >= 1 {
			return "REDUCE_INVALID", ""
		}
	case ActionAdjustStop:
		short = IsShortSide(p.Side)
		if short && s != nil {
			copy := *s
			copy.Price = p.Price
			if s.PerpQuote != nil {
				copy.Price = s.PerpQuote.Price
			}
			s = &copy
		}
		if s == nil || !positive(s.Price) {
			return "DATA_INSUFFICIENT", ""
		}
		if !positive(d.StopLoss) || (!short && d.StopLoss >= s.Price) || (short && d.StopLoss <= s.Price) {
			return "STOP_INVALID", ""
		}
		if p.StopPrice > 0 && ((!short && d.StopLoss < p.StopPrice) || (short && d.StopLoss > p.StopPrice)) {
			return "STOP_LOOSEN", ""
		}
	}
	return "", ""
}
