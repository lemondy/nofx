package binance_bstock

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"github.com/shopspring/decimal"
)

// The executor deliberately uses only spot exchangeInfo. The strategy layer
// owns equity-universe classification; here a candidate is {base ending B}USDT.
func (t *BStockTrader) symbols() (map[string]binance.Symbol, error) {
	t.rulesMu.Lock()
	defer t.rulesMu.Unlock()
	if t.rules != nil && t.now().Sub(t.rulesTime) < 10*time.Minute {
		return t.rules, nil
	}
	info, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return nil, wrap("exchangeInfo", err)
	}
	rules := make(map[string]binance.Symbol)
	for _, s := range info.Symbols {
		if s.Status == "TRADING" && s.QuoteAsset == "USDT" && strings.HasSuffix(s.BaseAsset, "B") && s.Symbol == s.BaseAsset+"USDT" {
			rules[s.Symbol] = s
		}
	}
	t.rules, t.rulesTime = rules, t.now()
	return rules, nil
}

func (t *BStockTrader) rule(symbol string) (binance.Symbol, error) {
	rules, err := t.symbols()
	if err != nil {
		return binance.Symbol{}, err
	}
	s, ok := rules[symbol]
	if !ok || !s.IsSpotTradingAllowed {
		return s, fmt.Errorf("symbol not tradable on spot bStock: %s", symbol)
	}
	return s, nil
}

func filter(s binance.Symbol, kind string) map[string]interface{} {
	for _, f := range s.Filters {
		if f["filterType"] == kind {
			return f
		}
	}
	return nil
}
func field(f map[string]interface{}, key string) float64 { return number(fmt.Sprint(f[key])) }
func flag(f map[string]interface{}, key string) bool     { b, _ := f[key].(bool); return b }
func finite(f float64) bool                              { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Decimal arithmetic avoids binary division rounding an exact tick down twice.
func round(f float64, increment float64, up bool) float64 {
	if increment <= 0 {
		return f
	}
	d := decimal.NewFromFloat(f).Div(decimal.NewFromFloat(increment))
	if up {
		d = d.Ceil()
	} else {
		d = d.Floor()
	}
	v, _ := d.Mul(decimal.NewFromFloat(increment)).Float64()
	return v
}

func quantity(s binance.Symbol, qty float64, market bool) (float64, error) {
	if !finite(qty) || qty <= 0 {
		return 0, fmt.Errorf("LOT_SIZE: quantity must be positive and finite")
	}
	f := filter(s, "LOT_SIZE")
	if field(f, "stepSize") <= 0 {
		return 0, fmt.Errorf("LOT_SIZE: missing stepSize for %s", s.Symbol)
	}
	qty = round(qty, field(f, "stepSize"), false)
	if market {
		qty = round(qty, field(filter(s, "MARKET_LOT_SIZE"), "stepSize"), false)
	}
	for _, kind := range []string{"LOT_SIZE", "MARKET_LOT_SIZE"} {
		if kind == "MARKET_LOT_SIZE" && !market {
			continue
		}
		f = filter(s, kind)
		step := field(f, "stepSize")
		if qty <= 0 || qty < field(f, "minQty") || (field(f, "maxQty") > 0 && qty > field(f, "maxQty")) || (step > 0 && !decimal.NewFromFloat(qty).Mod(decimal.NewFromFloat(step)).IsZero()) {
			return 0, fmt.Errorf("%s: quantity %s outside filter", kind, value(qty))
		}
	}
	return qty, nil
}

func price(s binance.Symbol, p float64) (float64, error) {
	f := filter(s, "PRICE_FILTER")
	if !finite(p) || p <= 0 || field(f, "tickSize") <= 0 {
		return 0, fmt.Errorf("PRICE_FILTER: invalid price or missing tickSize")
	}
	p = round(p, field(f, "tickSize"), false)
	if p <= 0 || p < field(f, "minPrice") || (field(f, "maxPrice") > 0 && p > field(f, "maxPrice")) {
		return 0, fmt.Errorf("PRICE_FILTER: price outside filter")
	}
	return p, nil
}

func notional(s binance.Symbol, n float64, market bool) error {
	if !finite(n) || n <= 0 {
		return fmt.Errorf("NOTIONAL: amount must be positive and finite")
	}
	for _, kind := range []string{"MIN_NOTIONAL", "NOTIONAL"} {
		f := filter(s, kind)
		minApplies := !market || flag(f, "applyMinToMarket") || (kind == "MIN_NOTIONAL" && flag(f, "applyToMarket"))
		if minApplies && n+1e-10 < field(f, "minNotional") {
			return fmt.Errorf("%s: %s below minimum %s", kind, value(n), value(field(f, "minNotional")))
		}
		if (!market || flag(f, "applyMaxToMarket")) && field(f, "maxNotional") > 0 && n > field(f, "maxNotional")+1e-10 {
			return fmt.Errorf("NOTIONAL: %s above maximum %s", value(n), value(field(f, "maxNotional")))
		}
	}
	return nil
}

func (t *BStockTrader) FormatQuantity(symbol string, qty float64) (string, error) {
	s, err := t.rule(symbol)
	if err != nil {
		return "", err
	}
	if !finite(qty) || qty < 0 {
		return "", fmt.Errorf("LOT_SIZE: invalid quantity")
	}
	step := field(filter(s, "LOT_SIZE"), "stepSize")
	if step <= 0 {
		return "", fmt.Errorf("LOT_SIZE: missing stepSize for %s", symbol)
	}
	return value(round(qty, step, false)), nil
}

// Preflight uses the average-price endpoint when a nonzero averaging window
// is configured. The exchange test remains authoritative for moving filters.
func (t *BStockTrader) Preflight(symbol string) error {
	s, err := t.rule(symbol)
	if err != nil {
		return err
	}
	p, err := t.GetMarketPrice(symbol)
	if err != nil {
		return err
	}
	pf := filter(s, "PRICE_FILTER")
	low, high := field(pf, "minPrice"), field(pf, "maxPrice")
	if high <= 0 {
		high = math.Inf(1)
	}
	hasPercent := false
	for _, kind := range []string{"PERCENT_PRICE", "PERCENT_PRICE_BY_SIDE"} {
		f := filter(s, kind)
		if f == nil {
			continue
		}
		ref := p
		if field(f, "avgPriceMins") > 0 {
			avg, e := t.client.NewAveragePriceService().Symbol(symbol).Do(context.Background())
			if e != nil {
				return wrap("Preflight average price", e)
			}
			ref = number(avg.Price)
		}
		lo, hi := field(f, "multiplierDown"), field(f, "multiplierUp")
		if kind == "PERCENT_PRICE_BY_SIDE" {
			lo, hi = field(f, "bidMultiplierDown"), field(f, "bidMultiplierUp")
		}
		if !finite(ref) || ref <= 0 || !finite(lo) || !finite(hi) || lo <= 0 || hi <= lo {
			return fmt.Errorf("Preflight filter failure: invalid %s", kind)
		}
		low, high = math.Max(low, ref*lo), math.Min(high, ref*hi)
		hasPercent = true
	}
	// Intersect both percent filters and PRICE_FILTER, including tick rounding.
	// Midpoint leaves room for price movement during the test request.
	low = round(low, field(pf, "tickSize"), true)
	if finite(high) {
		high = round(high, field(pf, "tickSize"), false)
	}
	if low > high {
		return fmt.Errorf("Preflight filter failure: no tick price satisfies percent and price filters")
	}
	if hasPercent {
		p = (low + high) / 2
	} else {
		p = math.Max(low, math.Min(p, high))
	}
	p, err = price(s, p)
	if err != nil {
		return fmt.Errorf("Preflight filter failure: %w", err)
	}
	lot := filter(s, "LOT_SIZE")
	minimum := math.Max(field(filter(s, "NOTIONAL"), "minNotional"), field(filter(s, "MIN_NOTIONAL"), "minNotional"))
	q := round(math.Max(field(lot, "minQty"), minimum/p), field(lot, "stepSize"), true)
	q, err = quantity(s, q, false)
	if err != nil {
		return fmt.Errorf("Preflight filter failure: %w", err)
	}
	if err = notional(s, q*p, false); err != nil {
		return fmt.Errorf("Preflight filter failure: %w", err)
	}
	err = t.client.NewCreateOrderService().Symbol(symbol).Side(binance.SideTypeBuy).Type(binance.OrderTypeLimit).TimeInForce(binance.TimeInForceTypeGTC).Price(value(p)).Quantity(value(q)).NewClientOrderID(clientID()).Test(context.Background())
	return wrap("Preflight", err)
}
